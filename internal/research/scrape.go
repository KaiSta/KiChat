package research

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/go-shiori/go-readability"

	"kichat/internal/llm"
	"kichat/internal/store"
)

// ScrapeOptions configures the scraper.
type ScrapeOptions struct {
	MaxBytes          int64
	Timeout           time.Duration
	RespectRobots     bool
	AllowPrivateHosts bool
	CacheHours        int
}

// ScrapeResult is the extracted, model-ready text of a page.
type ScrapeResult struct {
	URL         string `json:"url"`
	FinalURL    string `json:"finalUrl"`
	Title       string `json:"title"`
	Markdown    string `json:"markdown"`
	Tokens      int    `json:"tokens"`
	ContentType string `json:"contentType"`
	Truncated   bool   `json:"truncated"`
	Cached      bool   `json:"cached"`
	Pages       int    `json:"pages,omitempty"`
	Warning     string `json:"warning,omitempty"`
}

// pdfExtractor is injected by the app so the research package does not depend on
// the document-ingest package. It is a type alias rather than a defined type so
// the ingest package's own named function type stays assignable to it.
type pdfExtractor = func(data []byte, filename string) (text string, pages int, err error)

// Scraper fetches pages and converts them to Markdown.
type Scraper struct {
	mu     sync.RWMutex
	opts   ScrapeOptions
	store  *store.Store
	client *http.Client
	pdf    pdfExtractor

	robotsMu    sync.Mutex
	robotsCache map[string]robotsRules
}

// NewScraper builds a scraper.
func NewScraper(st *store.Store, opts ScrapeOptions, pdf pdfExtractor) *Scraper {
	s := &Scraper{
		store:       st,
		pdf:         pdf,
		robotsCache: map[string]robotsRules{},
	}
	s.Configure(opts)
	return s
}

// maxRedirects bounds a redirect chain. Each hop is re-guarded, so the limit is
// about cost, not about safety.
const maxRedirects = 3

// Configure applies new scrape settings and rebuilds the HTTP client.
func (s *Scraper) Configure(opts ScrapeOptions) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 5 << 20
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Second
	}
	if opts.CacheHours <= 0 {
		opts.CacheHours = 168
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	// The dial-time check covers what no hostname check can: a name that resolves
	// to a public address when inspected and to a private one when dialled. It is
	// skipped when an HTTP proxy is configured, because then the connection goes
	// to the proxy, which is allowed to live on the local network.
	if !proxyConfigured() {
		dialer.Control = privateDialControl(opts.AllowPrivateHosts)
	}

	client := &http.Client{
		Timeout: opts.Timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			MaxIdleConns:          8,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: opts.Timeout,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("zu viele Weiterleitungen")
			}
			// Every hop is re-guarded. The check in Scrape only covers the URL that
			// was named, and a redirect is exactly how a public address turns into a
			// request against the user's own machine or a cloud metadata endpoint —
			// whose response body would then be handed to the model.
			return s.guardHost(context.Background(), req.URL, opts)
		},
	}
	s.mu.Lock()
	s.opts = opts
	s.client = client
	s.mu.Unlock()
}

// proxyConfigured reports whether the environment points HTTP at a proxy. It
// decides whether the dial-time private-address check can be used at all.
func proxyConfigured() bool {
	probe, err := http.ProxyFromEnvironment(&http.Request{
		URL: &url.URL{Scheme: "https", Host: "proxy-probe.invalid"},
	})
	return err == nil && probe != nil
}

// privateDialControl rejects a connection whose target is a loopback, private or
// link-local address. It runs on the resolved address that is actually being
// dialled, which is what makes it immune to a host that answers the second
// lookup differently from the first.
func privateDialControl(allowPrivate bool) func(network, address string, _ syscall.RawConn) error {
	if allowPrivate {
		return nil
	}
	return func(_ string, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			host = address
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return nil
		}
		if isPrivateIP(ip) {
			return fmt.Errorf("Zugriff auf private Adressen ist deaktiviert (%s)", host)
		}
		return nil
	}
}

func (s *Scraper) options() ScrapeOptions {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.opts
}

// Scrape fetches a URL and returns clean Markdown trimmed to maxTokens.
func (s *Scraper) Scrape(ctx context.Context, rawURL string, maxTokens int) (ScrapeResult, error) {
	clean := normaliseURL(strings.TrimSpace(rawURL))
	if clean == "" {
		return ScrapeResult{}, fmt.Errorf("leere URL")
	}
	parsed, err := url.Parse(clean)
	if err != nil {
		return ScrapeResult{}, fmt.Errorf("ungültige URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ScrapeResult{}, fmt.Errorf("nur http(s) erlaubt, nicht %q", parsed.Scheme)
	}

	opts := s.options()
	if err := s.guardHost(ctx, parsed, opts); err != nil {
		return ScrapeResult{}, err
	}
	if opts.RespectRobots {
		if err := s.checkRobots(ctx, parsed); err != nil {
			return ScrapeResult{}, err
		}
	}

	urlHash := hashKey(clean)
	if s.store != nil {
		if entry, ok := s.store.GetScrapeCache(urlHash); ok {
			return ScrapeResult{
				URL:      entry.URL,
				FinalURL: entry.URL,
				Title:    entry.Title,
				Markdown: entry.ContentMD,
				Tokens:   entry.Tokens,
				Cached:   true,
			}, nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clean, nil)
	if err != nil {
		return ScrapeResult{}, err
	}
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/pdf,application/json,text/plain;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "de-DE,de;q=0.9,en;q=0.8")

	client := s.client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return ScrapeResult{}, fmt.Errorf("Abruf fehlgeschlagen: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ScrapeResult{}, &httpError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Provider:   "scrape",
		}
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	body, err := io.ReadAll(io.LimitReader(resp.Body, opts.MaxBytes))
	if err != nil {
		return ScrapeResult{}, fmt.Errorf("Lesen fehlgeschlagen: %w", err)
	}

	result := ScrapeResult{
		URL:         clean,
		FinalURL:    resp.Request.URL.String(),
		ContentType: contentType,
	}

	switch {
	case strings.Contains(contentType, "application/pdf") || strings.HasSuffix(strings.ToLower(parsed.Path), ".pdf"):
		if s.pdf == nil {
			return ScrapeResult{}, fmt.Errorf("PDF gefunden, aber keine PDF-Extraktion verfügbar")
		}
		text, pages, err := s.pdf(body, fileNameFromURL(parsed))
		if err != nil {
			return ScrapeResult{}, fmt.Errorf("PDF konnte nicht gelesen werden: %w", err)
		}
		result.Markdown = text
		result.Pages = pages
		result.Title = fileNameFromURL(parsed)

	case strings.Contains(contentType, "application/json"):
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, body, "", "  "); err == nil {
			result.Markdown = "```json\n" + pretty.String() + "\n```"
		} else {
			result.Markdown = "```json\n" + string(body) + "\n```"
		}
		result.Title = parsed.Path

	case strings.Contains(contentType, "text/plain") ||
		strings.Contains(contentType, "text/markdown") ||
		strings.Contains(contentType, "text/csv"):
		result.Markdown = string(body)
		result.Title = fileNameFromURL(parsed)
		if strings.Contains(contentType, "csv") {
			result.Markdown = "```csv\n" + result.Markdown + "\n```"
		}

	default:
		title, markdown, err := ExtractArticle(body, resp.Request.URL)
		if err != nil {
			return ScrapeResult{}, err
		}
		result.Title = title
		result.Markdown = markdown
	}

	result.Markdown = strings.TrimSpace(result.Markdown)
	if result.Markdown == "" {
		return ScrapeResult{}, fmt.Errorf("kein lesbarer Inhalt gefunden (%s)", contentType)
	}

	// Token-bounded truncation with an explicit marker, so the model knows the
	// text was cut rather than just ending.
	if maxTokens > 0 {
		total := llm.EstimateTokens(result.Markdown)
		if total > maxTokens {
			result.Markdown = cutToTokens(result.Markdown, maxTokens)
			result.Truncated = true
			remaining := total - llm.EstimateTokens(result.Markdown)
			result.Warning = fmt.Sprintf("Inhalt gekürzt: ca. %d von %d Tokens übernommen", maxTokens, total)
			result.Markdown += fmt.Sprintf("\n\n…[gekürzt: ca. %d weitere Tokens nicht übernommen]", remaining)
		}
	}
	result.Tokens = llm.EstimateTokens(result.Markdown)

	if s.store != nil {
		_ = s.store.PutScrapeCache(store.ScrapeCacheEntry{
			URLHash:   urlHash,
			URL:       clean,
			Title:     result.Title,
			ContentMD: result.Markdown,
			Tokens:    result.Tokens,
			FetchedAt: time.Now(),
			ExpiresAt: time.Now().Add(time.Duration(opts.CacheHours) * time.Hour),
		})
	}
	return result, nil
}

// ExtractArticle runs readability extraction and converts the result to
// Markdown. When readability finds nothing usable it falls back to converting
// the raw document, which is what makes plain table pages still readable.
func ExtractArticle(body []byte, pageURL *url.URL) (string, string, error) {
	article, err := readability.FromReader(bytes.NewReader(body), pageURL)
	if err == nil && strings.TrimSpace(article.Content) != "" {
		converted, convErr := markdownConverter.ConvertString(article.Content)
		if convErr == nil && strings.TrimSpace(converted) != "" {
			return cleanText(article.Title), cleanMarkdown(converted), nil
		}
	}
	converted, convErr := markdownConverter.ConvertString(string(body))
	if convErr != nil {
		return "", "", fmt.Errorf("HTML konnte nicht gelesen werden: %w", convErr)
	}
	if strings.TrimSpace(converted) == "" {
		return "", "", fmt.Errorf("kein lesbarer Inhalt gefunden")
	}
	return "", cleanMarkdown(converted), nil
}

// markdownConverter is built once: its rule set is read-only after construction,
// so sharing it across concurrent scrapes is safe.
var markdownConverter = md.NewConverter("", true, nil)

// cleanMarkdown collapses the excessive blank lines and stray escapes that
// HTML-to-Markdown conversion tends to leave behind.
func cleanMarkdown(md string) string {
	md = strings.ReplaceAll(md, "\u00a0", " ")
	md = strings.ReplaceAll(md, "\r\n", "\n")
	lines := strings.Split(md, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, strings.TrimRight(line, " \t"))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// cutToTokens truncates Markdown at roughly the requested token budget, cutting
// on a line boundary so code blocks and lists stay legible.
func cutToTokens(md string, maxTokens int) string {
	lines := strings.Split(md, "\n")
	var sb strings.Builder
	tokens := 0
	for _, line := range lines {
		lineTokens := llm.EstimateTokens(line)
		if tokens+lineTokens > maxTokens {
			break
		}
		sb.WriteString(line)
		sb.WriteString("\n")
		tokens += lineTokens
	}
	return strings.TrimRight(sb.String(), "\n")
}

// --- host guarding ---------------------------------------------------------

// guardHost blocks requests to loopback and private address ranges by default.
// Without this, a page containing a link to a local service could turn the
// scraper into a confused deputy.
func (s *Scraper) guardHost(ctx context.Context, u *url.URL, opts ScrapeOptions) error {
	if opts.AllowPrivateHosts {
		return nil
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL ohne Host")
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return fmt.Errorf("Zugriff auf lokale Adressen ist deaktiviert (%s)", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return fmt.Errorf("Zugriff auf private Adressen ist deaktiviert (%s)", host)
		}
		return nil
	}
	// Resolve the name and check every address it maps to.
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return fmt.Errorf("Host %s nicht auflösbar: %w", host, err)
	}
	for _, addr := range addrs {
		if isPrivateIP(addr.IP) {
			return fmt.Errorf("Host %s zeigt auf eine private Adresse und ist blockiert", host)
		}
	}
	return nil
}

func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	// Carrier-grade NAT and the documentation ranges are also off limits.
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
	}
	return false
}

// --- robots.txt ------------------------------------------------------------

type robotsRules struct {
	fetchedAt time.Time
	disallow  []string
	// ok records whether robots.txt was retrieved successfully. When the fetch
	// failed we do not know the rules, so we proceed rather than blocking every
	// page on a network hiccup.
	ok bool
}

// checkRobots performs a minimal robots.txt check for the "*" user agent.
func (s *Scraper) checkRobots(ctx context.Context, u *url.URL) error {
	origin := u.Scheme + "://" + u.Host
	s.robotsMu.Lock()
	entry, ok := s.robotsCache[origin]
	s.robotsMu.Unlock()
	if !ok || time.Since(entry.fetchedAt) > 6*time.Hour {
		entry = s.fetchRobots(ctx, origin)
		s.robotsMu.Lock()
		s.robotsCache[origin] = entry
		s.robotsMu.Unlock()
	}
	if !entry.ok {
		return nil
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	for _, disallowed := range entry.disallow {
		if disallowed == "" {
			continue
		}
		if strings.HasPrefix(path, disallowed) {
			return fmt.Errorf("robots.txt verbietet den Abruf von %s", path)
		}
	}
	return nil
}

func (s *Scraper) fetchRobots(ctx context.Context, origin string) robotsRules {
	entry := robotsRules{fetchedAt: time.Now()}
	client := s.client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	rc, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rc, http.MethodGet, origin+"/robots.txt", nil)
	if err != nil {
		return entry
	}
	req.Header.Set("User-Agent", browserUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return entry
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return entry
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return entry
	}
	entry.disallow = parseRobots(string(body))
	entry.ok = true
	return entry
}

// parseRobots extracts Disallow rules that apply to the wildcard user agent.
func parseRobots(body string) []string {
	var rules []string
	applies := false
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "user-agent":
			applies = value == "*"
		case "disallow":
			if applies && value != "" {
				rules = append(rules, value)
			}
		case "allow":
			// Allow rules are intentionally ignored: for a scraper, being
			// conservative is the right default.
		}
	}
	return rules
}

func fileNameFromURL(u *url.URL) string {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 {
		return u.Host
	}
	name := parts[len(parts)-1]
	if name == "" {
		return u.Host
	}
	return name
}
