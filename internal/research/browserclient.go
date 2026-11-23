package research

import (
	"bufio"
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	utls "github.com/refraction-networking/utls"
)

// A scraped provider is judged by the shape of the connection before it is judged by the
// request: the TLS ClientHello carries the cipher list, the extension order and the ALPN a
// client library was compiled with, and Go's net/http announces Go on every connection. A
// scraper that gets everything else right is still answered with a bot check because of it.
//
// Measured on 2026-11-03 from one machine, inside a minute: net/http and curl were both
// answered HTTP 202 with the anti-bot interstitial, while the same query over the same host
// with the ClientHello below was answered HTTP 200 with ten results. The IP was not blocked;
// one client identity was. That is what this file exists for: the identity, not the request.
//
// It is a copy of one browser's behaviour, which is why it does not follow a flag: the profile
// and the request below have to describe the same browser, or the mismatch is a fingerprint of
// its own (a Firefox ClientHello sending sec-ch-ua is not something any browser does).

// browserProfile is one browser identity: the handshake, the headers that have to agree with it,
// and the version both are derived from. name is only for error messages and tests.
type browserProfile struct {
	name string
	// major is the Chrome version this identity claims. Everything version-shaped is derived
	// from it — see chromeMajor.
	major           int
	userAgent       string
	hello           utls.ClientHelloID
	secChUa         string
	secChUaPlatform string
}

// chromeMajor is the one place the version is authored. The user agent, the sec-ch-ua hints and
// the choice of captured ClientHello are all derived from it, because the three only work as a
// set: a browser naming one version in its user agent and another in its hints is a fingerprint
// of its own, and so is an older release's hello wearing the newer name.
//
// 158 is the stable channel as of 2026-11-17. The hints are generated rather than copied because
// Chromium derives them from the major version and nothing else
// (components/embedder_support/user_agent_utils.cc, read at the 158.0.8127.37 tag). The previous
// hand-copied literal here was already wrong: it claimed
// `"Chromium";v="133", "Not(A:Brand";v="24", "Google Chrome";v="133"`, and Chrome 133 sent
// `"Not(A:Brand";v="99", "Google Chrome";v="133", "Chromium";v="133"` — all three of brand
// order, GREASE version and the pair itself were off, and nothing checked them.
const chromeMajor = 158

// chromeALPN is the only protocol this client speaks. It is offered in ALPN and in Chrome's ALPS
// extension, because a browser derives the second from the first: an `h2` ALPS list beside an
// `http/1.1`-only ALPN is a pair no browser produces, and it is what this package sent while the
// two were set separately.
const chromeALPN = "http/1.1"

// chromeHello is one ClientHello uTLS carries: the version it was captured from, and the range of
// Chrome versions it is measured to serve.
type chromeHello struct {
	version int
	hello   utls.ClientHelloID
	first   int
	last    int
}

// chromeHellos names the presets this package may present, as a table rather than
// utls.HelloChrome_Auto: Auto follows the dependency, so a `go get -u` would move this app's
// wire identity while its user agent stayed where it was. TestIdentityMajorsAgree pins the
// version this client actually sends.
//
// The 155 entry is uTLS' capture of branded Chrome 155.0.8059.40 on 2026-10-06
// (testdata/chrome155_clienthello.md): Chrome 133's hello plus GREASE, ML-DSA signature schemes
// and a 28-entry trust_anchors extension, JA4 `t13d1517h2_8daaf6152771_cb7bf5808d99` against
// 133's `t13d1516h2_8daaf6152771_d8a2da3f94cd`. Chrome 152 through 158 measured identical on
// every JA4 hash (checked 2026-11), which is what makes a 158 user agent honest over the 155
// hello rather than a lie about which release was captured. 150 and 151 changed the
// signature-algorithm tail without the extension count moving, so neither preset is exact there;
// they are left out rather than mislabelled.
var chromeHellos = []chromeHello{
	{version: 155, hello: utls.HelloChrome_155, first: 152, last: 158},
	{version: 133, hello: utls.HelloChrome_133, first: 133, last: 149},
}

// chromeHelloFor answers the hello measured to serve major, and whether the table has one.
func chromeHelloFor(major int) (chromeHello, bool) {
	for _, capture := range chromeHellos {
		if major >= capture.first && major <= capture.last {
			return capture, true
		}
	}
	return chromeHello{}, false
}

// newestChromeHello is the hello a profile presents: the capture covering major, or the newest
// the table has once the constant has been moved past it. Falling back rather than panicking
// keeps a shipped build working; TestIdentityMajorsAgree is what fails, so the gap is found
// before a release instead of by a reader who draws a captcha.
func newestChromeHello(major int) chromeHello {
	if capture, ok := chromeHelloFor(major); ok {
		return capture
	}
	newest := chromeHellos[0]
	for _, capture := range chromeHellos {
		if capture.last > newest.last {
			newest = capture
		}
	}
	return newest
}

// chromeWindows is the identity this package presents: Chrome chromeMajor on Windows.
var chromeWindows = newChromeProfile(chromeMajor)

// newChromeProfile builds the identity for one Chrome version.
func newChromeProfile(major int) browserProfile {
	hello := newestChromeHello(major)
	return browserProfile{
		name:            fmt.Sprintf("chrome%d-windows", major),
		major:           major,
		userAgent:       chromeUserAgent(major),
		hello:           hello.hello,
		secChUa:         chromeSecChUa(major),
		secChUaPlatform: `"Windows"`,
	}
}

// chromeUserAgent is the frozen user agent Chrome sends: only the major version changes from one
// release to the next.
func chromeUserAgent(major int) string {
	return fmt.Sprintf("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36", major)
}

// Chromium builds Sec-CH-UA from the major version and nothing else. The three rules below are
// its own (components/embedder_support/user_agent_utils.cc at 158.0.8127.37), and
// TestSecChUAMatchesChromiumsRules checks the result against eight versions measured in real
// builds, so a mistake here fails a test instead of shipping as a fingerprint.
var (
	// chromeHintSeparators is the character list the fake brand's two separators come from.
	chromeHintSeparators = [...]string{" ", "(", ":", "-", ".", "/", ")", ";", "=", "?", "_"}
	// chromeGreaseVersions is the version the fake brand is given, by major % 3.
	chromeGreaseVersions = [...]string{"8", "99", "24"}
	// chromeBrandOrder says where the fake brand, Chromium and Google Chrome go, by major % 6.
	// All six orders come round every six releases on purpose: no server may rely on one.
	chromeBrandOrder = [6][3]int{
		{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {2, 0, 1}, {1, 2, 0}, {2, 1, 0},
	}
)

// chromeSecChUa renders the Sec-CH-UA header value Chrome sends for major.
func chromeSecChUa(major int) string {
	grease := "Not" + chromeHintSeparators[major%len(chromeHintSeparators)] +
		"A" + chromeHintSeparators[(major+1)%len(chromeHintSeparators)] + "Brand"
	version := fmt.Sprintf("%d", major)
	brands := [...]string{
		fmt.Sprintf("%q;v=%q", grease, chromeGreaseVersions[major%len(chromeGreaseVersions)]),
		fmt.Sprintf("%q;v=%q", "Chromium", version),
		fmt.Sprintf("%q;v=%q", "Google Chrome", version),
	}
	order := chromeBrandOrder[major%len(chromeBrandOrder)]
	out := make([]string, 0, len(brands))
	for _, index := range order {
		out = append(out, brands[index])
	}
	return strings.Join(out, ", ")
}

// searchCookies is the cookie state that belongs to the scraped identity, shared by every
// browserClient in this process.
//
// It is package-level because the jar belongs to the identity rather than to one client: the
// chain rebuilds its providers whenever a setting is saved, and a bot check the reader has just
// answered has to survive that. A browser keeps one jar for a site the same way, whoever is
// asking. Without it every request was a client that had never been to the site before, which is
// both a fingerprint of its own and the reason a solved check could not be remembered.
var searchCookies, _ = cookiejar.New(nil)

// browserClient sends one request per call as the profile's browser would: a form POST for
// DuckDuckGo, or a GET navigation for the scraper's retry after a refusal.
//
// It writes the request itself rather than handing it to http.Transport, because the header
// order is part of the fingerprint and net/http sorts header names. Only the request is
// written by hand: the response is parsed by http.ReadResponse, so chunking, lengths and
// connection handling stay the standard library's job.
type browserClient struct {
	profile browserProfile
	dialer  *net.Dialer
	timeout time.Duration
	// jar holds the identity's cookies. Nil means this client carries none, which is what a
	// fixture needs and what the identity must never be in production.
	jar http.CookieJar
	// insecure skips certificate verification. Tests only: the fixture server uses a
	// certificate no CA signed, and a test that needed a trusted certificate would have to
	// install one in the system store.
	insecure bool
}

func newBrowserClient(profile browserProfile) *browserClient {
	return &browserClient{
		profile: profile,
		dialer:  &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
		timeout: 25 * time.Second,
		jar:     searchCookies,
	}
}

// postForm posts form to endpoint and returns the response with its body already decoded.
//
// One request per connection: a search is one navigation, the pacing in duckduckgo.go keeps
// searches apart by seconds, and a pooled connection that sat idle for eight seconds is a
// stranger thing to see than a fresh one. Closing also means the body must be closed, which
// closes the connection.
func (c *browserClient) postForm(ctx context.Context, endpoint string, form url.Values, lang string) (*http.Response, error) {
	return c.postFormFromPage(ctx, endpoint, form, lang, nil)
}

// postFormFromPage posts a form as the page at from would: that page's Origin, its address as the
// Referer, and fetch metadata describing the hop from it to the endpoint. Nil keeps the shape the
// search form has always had, where the post names the site as its own origin.
//
// It matters for the bot check. Its answer is posted from the search page to duckduckgo.com, which
// a browser sends as `Origin: https://html.duckduckgo.com`, `Referer:
// https://html.duckduckgo.com/html/` and `Sec-Fetch-Site: same-site`; claiming to come from
// duckduckgo.com itself is a hop no browser makes, and an answer that looks automated is judged as
// automation rather than as an answer.
func (c *browserClient) postFormFromPage(ctx context.Context, endpoint string, form url.Values, lang string, from *url.URL) (*http.Response, error) {
	target, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	ctx, cancel := c.withTimeout(ctx)
	resp, err := c.do(ctx, target, c.requestText(target, form, lang, from))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Request = &http.Request{Method: http.MethodPost, URL: target}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// postFormFollow posts a form and, when the host answers with a redirect rather than a page,
// follows it once as the browser would. The bot check's answer is a 303 back to the results, so
// without this the reply would be an empty redirect to the caller.
func (c *browserClient) postFormFollow(ctx context.Context, endpoint string, form url.Values, lang string, from *url.URL) (*http.Response, error) {
	target, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	resp, err := c.postFormFromPage(ctx, endpoint, form, lang, from)
	if err != nil {
		return nil, err
	}
	location := resp.Header.Get("Location")
	if !isRedirect(resp.StatusCode) || location == "" {
		return resp, nil
	}
	next, err := target.Parse(location)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("research: bad redirect target: %w", err)
	}
	if next.Scheme != "https" && next.Scheme != "http" {
		return nil, fmt.Errorf("research: unsupported redirect scheme %q", next.Scheme)
	}
	return c.get(ctx, next, lang, nil)
}

// get performs one browser navigation and follows redirects. guard is called for the URL named
// and for every hop: a fallback that skipped it would be a way around the scraper's own SSRF
// checks. One request per connection, as in postForm — a redirect chain is a handful of
// navigations, not a session.
func (c *browserClient) get(ctx context.Context, target *url.URL, lang string, guard func(*url.URL) error) (*http.Response, error) {
	ctx, cancel := c.withTimeout(ctx)
	current := target
	var previous *url.URL
	for hop := 0; ; hop++ {
		if guard != nil {
			if err := guard(current); err != nil {
				cancel()
				return nil, err
			}
		}
		resp, err := c.do(ctx, current, c.navigationText(current, previous, lang))
		if err != nil {
			cancel()
			return nil, err
		}
		resp.Request = &http.Request{Method: http.MethodGet, URL: current}
		location := resp.Header.Get("Location")
		if !isRedirect(resp.StatusCode) || location == "" {
			resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
			return resp, nil
		}
		_ = resp.Body.Close()
		if hop >= maxRedirects {
			cancel()
			return nil, fmt.Errorf("research: too many redirects")
		}
		next, err := current.Parse(location)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("research: bad redirect target: %w", err)
		}
		if next.Scheme != "https" && next.Scheme != "http" {
			cancel()
			return nil, fmt.Errorf("research: unsupported redirect scheme %q", next.Scheme)
		}
		previous, current = current, next
	}
}

// withTimeout applies the client's per-request bound. The cancel is handed to the response
// body rather than deferred here, because the body is read after this call returns.
func (c *browserClient) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout > 0 {
		return context.WithTimeout(ctx, c.timeout)
	}
	return context.WithCancel(ctx)
}

// do writes one request and reads its response. It is the part postForm and get share; the
// request text, and therefore the method and header order, is the caller's.
func (c *browserClient) do(ctx context.Context, target *url.URL, request string) (*http.Response, error) {
	if target.Scheme != "https" && target.Scheme != "http" {
		return nil, fmt.Errorf("research: unsupported scheme %q", target.Scheme)
	}

	addr := target.Host
	if target.Port() == "" {
		if target.Scheme == "https" {
			addr = net.JoinHostPort(target.Hostname(), "443")
		} else {
			addr = net.JoinHostPort(target.Hostname(), "80")
		}
	}
	conn, err := c.dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	// A cancelled context has to interrupt the handshake and the read as well as the dial,
	// which net.Conn deadlines only cover while they are in the future.
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopped:
		}
	}()
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			close(stopped)
			_ = conn.Close()
		})
	}

	var wire io.ReadWriter = conn
	if target.Scheme == "https" {
		tlsConn, err := c.handshake(ctx, conn, target.Hostname())
		if err != nil {
			cleanup()
			return nil, err
		}
		wire = tlsConn
	}

	writer := bufio.NewWriter(wire)
	if _, err := writer.WriteString(request); err != nil {
		cleanup()
		return nil, err
	}
	if err := writer.Flush(); err != nil {
		cleanup()
		return nil, err
	}

	resp, err := http.ReadResponse(bufio.NewReader(wire), nil)
	if err != nil {
		cleanup()
		return nil, err
	}
	// The cookies a response sets are part of the identity: the bot check's answer is validated
	// against them, and a browser that forgets them has to prove itself again.
	if c.jar != nil {
		if cookies := resp.Cookies(); len(cookies) > 0 {
			c.jar.SetCookies(target, cookies)
		}
	}
	body, err := decodeResponseBody(resp)
	if err != nil {
		resp.Body.Close()
		cleanup()
		return nil, err
	}
	resp.Body = &connectionBody{ReadCloser: body, cleanup: cleanup}
	return resp, nil
}

// handshake runs the profile's ClientHello over conn.
//
// The ALPN list is cut down to http/1.1. The preset offers h2 first, and a server that picks
// it would then be spoken to in HTTP/1.1, which fails; the alternative is to speak HTTP/2 and
// present Go's SETTINGS frame, which is the fingerprint this file exists to avoid. One
// protocol step behind a browser profile is a much smaller tell than a ClientHello that
// offers h2 and cannot speak it.
//
// The edits are applied between BuildHandshakeState and MarshalClientHello, the order uTLS
// documents for changing a preset: the handshake marshals the ClientHello built here, so a
// change made afterwards would never reach the wire.
func (c *browserClient) handshake(ctx context.Context, conn net.Conn, host string) (net.Conn, error) {
	config := &utls.Config{
		ServerName:         host,
		InsecureSkipVerify: c.insecure,
	}
	uconn := utls.UClient(conn, config, c.profile.hello)
	if err := uconn.BuildHandshakeState(); err != nil {
		return nil, err
	}
	for _, ext := range uconn.Extensions {
		switch typed := ext.(type) {
		case *utls.ALPNExtension:
			typed.AlpnProtocols = []string{chromeALPN}
		// Chrome derives its ALPS list from the protocols it offers in ALPN, so both are cut
		// to what this client can actually speak. The presets advertise h2 in both, because
		// the browser they were captured from does.
		case *utls.ApplicationSettingsExtension:
			typed.SupportedProtocols = []string{chromeALPN}
		case *utls.ApplicationSettingsExtensionNew:
			typed.SupportedProtocols = []string{chromeALPN}
		}
	}
	if err := uconn.MarshalClientHello(); err != nil {
		return nil, err
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		// The profile is named here because a failed handshake is the one error whose cause
		// is the identity rather than the request, and that is what has to be in the message.
		return nil, fmt.Errorf("%s handshake with %s: %w", c.profile.name, host, err)
	}
	return uconn, nil
}

// requestURI renders a target for the request line, with the query's unencoded bytes escaped.
//
// The escape is applied here rather than trusted to the URL because the host's own markup is not
// always a valid URI: measured on 2026-11-22, the anomaly form's action carries
// `&q=Bürostuhl hohe Rückenlehne…` verbatim, and a space on the request line is not a request at
// all — DuckDuckGo answers 400 Bad Request. A browser percent-encodes the action when it posts the
// form, and so does this.
func requestURI(target *url.URL) string {
	path := target.EscapedPath()
	if path == "" {
		path = "/"
	}
	if target.RawQuery == "" {
		return path
	}
	return path + "?" + escapeQuery(target.RawQuery)
}

// escapeQuery percent-encodes the bytes a request line cannot carry — a space, and anything
// outside ASCII — and leaves existing escapes and reserved characters as they are.
func escapeQuery(query string) string {
	var out strings.Builder
	out.Grow(len(query))
	for index := 0; index < len(query); index++ {
		switch b := query[index]; {
		case b == ' ':
			out.WriteString("%20")
		case b >= 0x80:
			fmt.Fprintf(&out, "%%%02X", b)
		default:
			out.WriteByte(b)
		}
	}
	return out.String()
}

// formFetchSite describes the hop a form submission makes. Nil from is the search form's own
// shape, where the post names the site as its origin and the endpoint is one of its subdomains.
func formFetchSite(target, from *url.URL) string {
	if from == nil || from.Host == "" {
		return "same-site"
	}
	if target.Scheme == from.Scheme && target.Host == from.Host {
		return "same-origin"
	}
	if isDuckDuckGoHost(target.Hostname()) && isDuckDuckGoHost(from.Hostname()) {
		return "same-site"
	}
	return "cross-site"
}

// requestText renders the request in the order Chrome sends a form navigation: the fields the
// browser computes itself (Host, length, fetch metadata) around the ones this call supplies.
//
// The order is written out rather than looped over so that reading it next to a browser's
// network panel is a comparison, not a deduction.
func (c *browserClient) requestText(target *url.URL, form url.Values, lang string, from *url.URL) string {
	body := form.Encode()
	origin := target.Scheme + "://" + target.Host
	referer := origin + "/"
	// A page with no host is no page: a check kept without one keeps the search form's shape
	// rather than naming an origin no host has.
	if from != nil && from.Host == "" {
		from = nil
	}
	if from != nil {
		// The page that carried the form: its origin is the one the post claims, and its own
		// address is what the request says it came from.
		origin = from.Scheme + "://" + from.Host
		referer = from.String()
	} else if host := target.Hostname(); target.Scheme == "https" && isDuckDuckGoHost(host) {
		// Searches are submitted from the site itself: html.duckduckgo.com and
		// lite.duckduckgo.com are different hosts than duckduckgo.com but the same site, and a
		// form posted from a search page names the site in Origin.
		origin = "https://duckduckgo.com"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "POST %s HTTP/1.1\r\n", requestURI(target))
	fmt.Fprintf(&b, "Host: %s\r\n", target.Host)
	fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	b.WriteString("Cache-Control: max-age=0\r\n")
	fmt.Fprintf(&b, "sec-ch-ua: %s\r\n", c.profile.secChUa)
	b.WriteString("sec-ch-ua-mobile: ?0\r\n")
	fmt.Fprintf(&b, "sec-ch-ua-platform: %s\r\n", c.profile.secChUaPlatform)
	b.WriteString("Upgrade-Insecure-Requests: 1\r\n")
	fmt.Fprintf(&b, "Origin: %s\r\n", origin)
	b.WriteString("Content-Type: application/x-www-form-urlencoded\r\n")
	fmt.Fprintf(&b, "User-Agent: %s\r\n", c.profile.userAgent)
	b.WriteString("Accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7\r\n")
	fmt.Fprintf(&b, "Sec-Fetch-Site: %s\r\n", formFetchSite(target, from))
	b.WriteString("Sec-Fetch-Mode: navigate\r\n")
	b.WriteString("Sec-Fetch-User: ?1\r\n")
	b.WriteString("Sec-Fetch-Dest: document\r\n")
	fmt.Fprintf(&b, "Referer: %s\r\n", referer)
	b.WriteString("Accept-Encoding: gzip, deflate, br, zstd\r\n")
	fmt.Fprintf(&b, "Accept-Language: %s\r\n", acceptLanguage(lang))
	c.writeCookies(&b, target)
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}

// writeCookies appends the identity's cookies for target where Chrome sends them: last, after
// Accept-Language. A request with none carries no Cookie header at all, as a fresh profile's
// would not.
func (c *browserClient) writeCookies(b *strings.Builder, target *url.URL) {
	if c.jar == nil {
		return
	}
	cookies := c.jar.Cookies(target)
	if len(cookies) == 0 {
		return
	}
	pairs := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		pairs = append(pairs, cookie.Name+"="+cookie.Value)
	}
	fmt.Fprintf(b, "Cookie: %s\r\n", strings.Join(pairs, "; "))
}

// navigationText renders a top-level GET navigation in Chrome's order. It differs from a form
// submission only where the browser does: no Origin, Content-Type or Content-Length, and the
// fetch metadata describes a navigation rather than a same-site form post.
func (c *browserClient) navigationText(target, previous *url.URL, lang string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", requestURI(target))
	fmt.Fprintf(&b, "Host: %s\r\n", target.Host)
	b.WriteString("Cache-Control: max-age=0\r\n")
	fmt.Fprintf(&b, "sec-ch-ua: %s\r\n", c.profile.secChUa)
	b.WriteString("sec-ch-ua-mobile: ?0\r\n")
	fmt.Fprintf(&b, "sec-ch-ua-platform: %s\r\n", c.profile.secChUaPlatform)
	b.WriteString("Upgrade-Insecure-Requests: 1\r\n")
	fmt.Fprintf(&b, "User-Agent: %s\r\n", c.profile.userAgent)
	b.WriteString("Accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7\r\n")
	fmt.Fprintf(&b, "Sec-Fetch-Site: %s\r\n", secFetchSite(target, previous))
	b.WriteString("Sec-Fetch-Mode: navigate\r\n")
	b.WriteString("Sec-Fetch-User: ?1\r\n")
	b.WriteString("Sec-Fetch-Dest: document\r\n")
	if previous != nil {
		fmt.Fprintf(&b, "Referer: %s\r\n", previous.String())
	}
	b.WriteString("Accept-Encoding: gzip, deflate, br, zstd\r\n")
	fmt.Fprintf(&b, "Accept-Language: %s\r\n", acceptLanguage(lang))
	c.writeCookies(&b, target)
	b.WriteString("\r\n")
	return b.String()
}

// secFetchSite describes the relationship a navigation hop has to the one before it. The first
// navigation is "none" (the user typed or pasted the address); a redirect is judged against the
// page that redirected. "same-site" is deliberately not guessed from the host alone, because it
// needs the registrable domain; "cross-site" is the conservative answer for a different one.
func secFetchSite(target, previous *url.URL) string {
	if previous == nil {
		return "none"
	}
	if target.Scheme == previous.Scheme && target.Host == previous.Host {
		return "same-origin"
	}
	return "cross-site"
}

// isRedirect reports the statuses a browser navigation follows.
func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// isDuckDuckGoHost reports whether host is one of the search endpoints' names. It is checked by
// suffix rather than equality so a subdomain DuckDuckGo adds tomorrow still counts, and it is
// checked at all so the Origin header is only ever claimed for the site it describes.
func isDuckDuckGoHost(host string) bool {
	host = strings.ToLower(host)
	return host == "duckduckgo.com" || strings.HasSuffix(host, ".duckduckgo.com")
}

// acceptLanguage renders the language the way a browser sends it, so the header agrees with
// the query or page rather than with the operating system of the machine pretending to be
// Windows: a German request from a "Windows" client that asks for en-US pages is a pair that
// does not occur.
func acceptLanguage(lang string) string {
	switch lang = strings.ToLower(strings.TrimSpace(lang)); lang {
	case "", "de":
		return "de-DE,de;q=0.9,en;q=0.8"
	case "en":
		return "en-US,en;q=0.9"
	case "zh":
		return "zh-CN,zh;q=0.9,en;q=0.8"
	default:
		return lang + ",en;q=0.9"
	}
}

// decodeResponseBody unwraps the content encodings the request advertises. Advertising br and
// zstd without being able to read them would turn a correct answer into a parse failure, and
// advertising only gzip would be a header no browser sends.
//
// The header may name several encodings — an intermediary in front of a compressed response adds
// its own — and it lists them in the order they were applied, so they are removed from the last
// one back. A response that names an encoding but carries no body is left alone: a decoder over
// an empty stream reports it as a truncated stream, which would turn an empty answer into a
// decoding failure.
func decodeResponseBody(resp *http.Response) (io.ReadCloser, error) {
	header := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if header == "" || header == "identity" || resp.ContentLength == 0 {
		return resp.Body, nil
	}
	body := io.ReadCloser(resp.Body)
	encodings := strings.Split(header, ",")
	for i := len(encodings) - 1; i >= 0; i-- {
		var err error
		switch strings.TrimSpace(encodings[i]) {
		case "", "identity":
			continue
		case "gzip", "x-gzip":
			body, err = gzip.NewReader(body)
		case "deflate":
			body, err = zlib.NewReader(body)
		case "br":
			body = io.NopCloser(brotli.NewReader(body))
		case "zstd":
			var decoder *zstd.Decoder
			if decoder, err = zstd.NewReader(body); err == nil {
				body = decoder.IOReadCloser()
			}
		default:
			return nil, fmt.Errorf("research: unsupported content encoding %q", resp.Header.Get("Content-Encoding"))
		}
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

// connectionBody ties the response body to the connection it arrived on: the connection is
// one request old, so closing the body is what ends it.
type connectionBody struct {
	io.ReadCloser
	cleanup func()
	once    sync.Once
}

func (b *connectionBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.cleanup)
	return err
}

// cancelBody releases a request's context when its response body is closed. The response has to
// outlive the call that produced it, so the cancel cannot be deferred in postForm or get.
type cancelBody struct {
	io.ReadCloser
	once   sync.Once
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.cancel)
	return err
}
