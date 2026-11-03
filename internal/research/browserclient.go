package research

import (
	"bufio"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
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

// browserProfile is one browser identity: the handshake, and the headers that have to agree
// with it. name is only for error messages and tests.
type browserProfile struct {
	name            string
	userAgent       string
	hello           utls.ClientHelloID
	secChUa         string
	secChUaPlatform string
}

// chromeWindows is the identity this package presents. Every field describes Chrome 133 on
// Windows, the newest profile the uTLS version in go.mod carries; the sec-ch-ua hints repeat
// the version in the user agent because a browser that lies about one of them lies about both.
//
// The version ages and the day will come when it is old enough to look wrong on its own. When
// that happens the fix is to move to the newest profile uTLS has and to change these strings
// with it — as one edit, because half of it is worse than none.
var chromeWindows = browserProfile{
	name:            "chrome133-windows",
	userAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36",
	hello:           utls.HelloChrome_133,
	secChUa:         `"Chromium";v="133", "Not(A:Brand";v="24", "Google Chrome";v="133"`,
	secChUaPlatform: `"Windows"`,
}

// browserClient sends one form POST per call as the profile's browser would.
//
// It writes the request itself rather than handing it to http.Transport, because the header
// order is part of the fingerprint and net/http sorts header names. Only the request is
// written by hand: the response is parsed by http.ReadResponse, so chunking, lengths and
// connection handling stay the standard library's job.
type browserClient struct {
	profile browserProfile
	dialer  *net.Dialer
	timeout time.Duration
	// insecure skips certificate verification. Tests only: the fixture server uses a
	// certificate no CA signed, and a test that needed a trusted certificate would have to
	// install one in the system store.
	insecure bool
	roots    *x509.CertPool
}

func newBrowserClient(profile browserProfile) *browserClient {
	return &browserClient{
		profile: profile,
		dialer:  &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
		timeout: 25 * time.Second,
	}
}

// postForm posts form to endpoint and returns the response with its body already decoded.
//
// One request per connection: a search is one navigation, the pacing in duckduckgo.go keeps
// searches apart by seconds, and a pooled connection that sat idle for eight seconds is a
// stranger thing to see than a fresh one. Closing also means the body must be closed, which
// closes the connection.
func (c *browserClient) postForm(ctx context.Context, endpoint string, form url.Values, lang string) (*http.Response, error) {
	target, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if target.Scheme != "https" && target.Scheme != "http" {
		return nil, fmt.Errorf("research: unsupported scheme %q", target.Scheme)
	}

	var cancel context.CancelFunc
	if c.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
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
		cancel()
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
			cancel()
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
	if _, err := writer.WriteString(c.requestText(target, form, lang)); err != nil {
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
		RootCAs:            c.roots,
		InsecureSkipVerify: c.insecure,
	}
	uconn := utls.UClient(conn, config, c.profile.hello)
	if err := uconn.BuildHandshakeState(); err != nil {
		return nil, err
	}
	for _, ext := range uconn.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{"http/1.1"}
		}
	}
	if err := uconn.MarshalClientHello(); err != nil {
		return nil, err
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	return uconn, nil
}

// requestText renders the request in the order Chrome sends a form navigation: the fields the
// browser computes itself (Host, length, fetch metadata) around the ones this call supplies.
//
// The order is written out rather than looped over so that reading it next to a browser's
// network panel is a comparison, not a deduction.
func (c *browserClient) requestText(target *url.URL, form url.Values, lang string) string {
	body := form.Encode()
	origin := target.Scheme + "://" + target.Host
	if target.Scheme == "https" {
		// Searches are submitted from the site itself; the form endpoints are a different
		// host than duckduckgo.com but the same site.
		origin = "https://duckduckgo.com"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "POST %s HTTP/1.1\r\n", target.RequestURI())
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
	b.WriteString("Sec-Fetch-Site: same-site\r\n")
	b.WriteString("Sec-Fetch-Mode: navigate\r\n")
	b.WriteString("Sec-Fetch-User: ?1\r\n")
	b.WriteString("Sec-Fetch-Dest: document\r\n")
	fmt.Fprintf(&b, "Referer: %s/\r\n", origin)
	b.WriteString("Accept-Encoding: gzip, deflate, br, zstd\r\n")
	fmt.Fprintf(&b, "Accept-Language: %s\r\n", acceptLanguage(lang))
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}

// acceptLanguage renders the search language the way a browser sends it, so the header agrees
// with the query rather than with the operating system of the machine pretending to be
// Windows: a German search from a "Windows" client that asks for en-US pages is a pair that
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
func decodeResponseBody(resp *http.Response) (io.ReadCloser, error) {
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return resp.Body, nil
	case "gzip", "x-gzip":
		reader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		return reader, nil
	case "deflate":
		reader, err := zlib.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		return reader, nil
	case "br":
		return io.NopCloser(brotli.NewReader(resp.Body)), nil
	case "zstd":
		reader, err := zstd.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		return reader.IOReadCloser(), nil
	default:
		return nil, fmt.Errorf("research: unsupported content encoding %q", resp.Header.Get("Content-Encoding"))
	}
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
