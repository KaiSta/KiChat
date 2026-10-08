package research

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// DuckDuckGo has no official web-search API. This provider scrapes the
// no-JavaScript HTML endpoints, which is inherently fragile: the selectors below
// are covered by fixture tests so layout drift is caught immediately, and the
// lite endpoint is tried as a second shape. What is not a layout change is the
// anti-bot interstitial, which is reported as what it is.
const (
	ddgHTMLEndpoint  = "https://html.duckduckgo.com/html/"
	ddgLiteEndpoint  = "https://lite.duckduckgo.com/lite/"
	browserUserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"
)

// ddgChallengeMarkers are strings only the interstitial carries: the challenge
// script, its form and its modal. They are what a block is detected by, because
// neither the status nor an empty parse can tell it apart from a real answer —
// measured on 2026-10-08, a host DuckDuckGo did not trust was answered with HTTP
// 202 (not an error status) and a page with no result nodes at all, which is
// exactly what a genuine zero-result page looks like from the outside.
var ddgChallengeMarkers = []string{"anomaly.js", "challenge-form", "anomaly-modal"}

// ddgBlockedErr is returned when the interstitial is served instead of results.
// It names the reason and the way out: "no results" would send the reader
// looking for a selector bug in a page that was never served.
var ddgBlockedErr = fmt.Errorf("%w: DuckDuckGo verlangt eine Bot-Prüfung (Captcha) statt "+
	"Suchergebnissen – im Browser lösen oder einen anderen Suchanbieter konfigurieren", ErrBlocked)

// isDDGChallenge reports whether a response body is the interstitial rather than
// a page of results.
func isDDGChallenge(body string) bool {
	for _, marker := range ddgChallengeMarkers {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

// DuckDuckGoProvider scrapes DuckDuckGo's HTML search results.
type DuckDuckGoProvider struct {
	Endpoint string
	LiteURL  string
	limiter  *rateLimiter
	client   *http.Client
}

// NewDuckDuckGoProvider builds the provider. Endpoints are overridable for tests.
func NewDuckDuckGoProvider(endpoint string) *DuckDuckGoProvider {
	if endpoint == "" {
		endpoint = ddgHTMLEndpoint
	}
	return &DuckDuckGoProvider{
		Endpoint: endpoint,
		LiteURL:  ddgLiteEndpoint,
		limiter:  newRateLimiter(0.5),
		client:   &http.Client{Timeout: 25 * time.Second},
	}
}

func (d *DuckDuckGoProvider) Name() string      { return "duckduckgo" }
func (d *DuckDuckGoProvider) Available() bool   { return true }
func (d *DuckDuckGoProvider) RequiresKey() bool { return false }

// Search tries the lightweight HTML endpoint first and falls back to the lite
// form, which has a different and more stable layout. A bot check ends the
// attempt: it is handed out per client rather than per layout, so the second
// shape cannot succeed where the first was refused.
func (d *DuckDuckGoProvider) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	if err := d.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	results, err := d.searchHTML(ctx, req)
	if err == nil && len(results) > 0 {
		return results, nil
	}
	htmlErr := err
	// Asking the lite layout now would only buy a second identical failure, and
	// the pacing it would wait out is time the turn does not have.
	if errors.Is(htmlErr, ErrBlocked) {
		return nil, htmlErr
	}

	liteResults, liteErr := d.searchLite(ctx, req)
	if liteErr == nil && len(liteResults) > 0 {
		return liteResults, nil
	}
	// Same reasoning one step later: a block explains itself better on its own
	// than nested under whichever other failure came first.
	if errors.Is(liteErr, ErrBlocked) {
		return nil, liteErr
	}
	switch {
	case htmlErr != nil && liteErr != nil:
		return nil, fmt.Errorf("duckduckgo: html: %v; lite: %v", htmlErr, liteErr)
	case liteErr != nil:
		return nil, fmt.Errorf("duckduckgo lite: %w", liteErr)
	case htmlErr != nil:
		return nil, htmlErr
	default:
		return nil, fmt.Errorf("duckduckgo: keine Ergebnisse gefunden (Layout möglicherweise geändert)")
	}
}

func (d *DuckDuckGoProvider) searchHTML(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	form := url.Values{}
	form.Set("q", req.Query)
	if req.Country != "" {
		form.Set("kl", strings.ToLower(req.Country)+"-"+defaultLang(req.SearchLang))
	}
	if req.Freshness != "" {
		if df := ddgFreshness(req.Freshness); df != "" {
			form.Set("df", df)
		}
	}

	// POST is the more reliable form for this endpoint; some networks block the
	// GET variant.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("User-Agent", browserUserAgent)
	httpReq.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, &httpError{StatusCode: resp.StatusCode, Status: resp.Status, Provider: "duckduckgo"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if isDDGChallenge(string(body)) {
		return nil, ddgBlockedErr
	}
	return parseDDGHtml(string(body), req.Count)
}

// parseDDGHtml extracts results from the html.duckduckgo.com markup.
func parseDDGHtml(body string, limit int) ([]SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	out := []SearchResult{}
	doc.Find("div.result, div.web-result").EachWithBreak(func(_ int, sel *goquery.Selection) bool {
		if hasClass(sel, "result--ad") || hasClass(sel, "result--ad--small") {
			return true
		}
		link := sel.Find("a.result__a").First()
		if link.Length() == 0 {
			return true
		}
		href, _ := link.Attr("href")
		target := unwrapDDGLink(href)
		title := cleanText(link.Text())
		if target == "" || title == "" {
			return true
		}
		snippet := cleanText(sel.Find(".result__snippet").First().Text())
		out = append(out, SearchResult{
			Title:    title,
			URL:      normaliseURL(target),
			Snippet:  snippet,
			Provider: "duckduckgo",
		})
		if limit > 0 && len(out) >= limit {
			return false
		}
		return true
	})

	out = dedupeResults(out)
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (d *DuckDuckGoProvider) searchLite(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	form := url.Values{}
	form.Set("q", req.Query)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.LiteURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("User-Agent", browserUserAgent)

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, &httpError{StatusCode: resp.StatusCode, Status: resp.Status, Provider: "duckduckgo-lite"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if isDDGChallenge(string(body)) {
		return nil, ddgBlockedErr
	}
	return parseDDGLite(string(body), req.Count)
}

// parseDDGLite extracts results from the lite endpoint's table markup.
func parseDDGLite(body string, limit int) ([]SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	out := []SearchResult{}
	doc.Find("a.result-link").EachWithBreak(func(_ int, link *goquery.Selection) bool {
		href, _ := link.Attr("href")
		target := unwrapDDGLink(href)
		title := cleanText(link.Text())
		if target == "" || title == "" {
			return true
		}
		// The snippet lives in the following table row in the lite layout, but
		// older variants nest it next to the link, so both are tried.
		snippet := cleanText(link.Closest("tr").NextAll().Find(".result-snippet").First().Text())
		if snippet == "" {
			snippet = cleanText(link.Parent().Find(".result-snippet").First().Text())
		}
		if snippet == "" {
			snippet = cleanText(link.Closest("td").NextAllFiltered("td").First().Text())
		}
		out = append(out, SearchResult{
			Title:    title,
			URL:      normaliseURL(target),
			Snippet:  snippet,
			Provider: "duckduckgo-lite",
		})
		if limit > 0 && len(out) >= limit {
			return false
		}
		return true
	})
	out = dedupeResults(out)
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// unwrapDDGLink reverses DuckDuckGo's /l/?uddg= redirect wrapper.
func unwrapDDGLink(href string) string {
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return href
	}
	if uddg := parsed.Query().Get("uddg"); uddg != "" {
		if decoded, err := url.QueryUnescape(uddg); err == nil {
			return decoded
		}
		return uddg
	}
	if parsed.Host == "" && strings.HasPrefix(href, "/") {
		return "https://duckduckgo.com" + href
	}
	return href
}

func hasClass(sel *goquery.Selection, class string) bool {
	attr, _ := sel.Attr("class")
	for _, c := range strings.Fields(attr) {
		if c == class {
			return true
		}
	}
	return false
}

func defaultLang(lang string) string {
	if lang == "" {
		return "de"
	}
	return lang
}

// ddgFreshness maps our freshness tokens onto DuckDuckGo's df parameter.
func ddgFreshness(f string) string {
	switch f {
	case "pd":
		return "d"
	case "pw":
		return "w"
	case "pm":
		return "m"
	case "py":
		return "y"
	default:
		return ""
	}
}
