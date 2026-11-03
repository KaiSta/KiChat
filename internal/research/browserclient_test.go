package research

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// capturedRequest is one request as it appeared on the wire: the header block in the order it
// was written, and the body.
type capturedRequest struct {
	head string
	body string
}

// startRawServer accepts one connection, records the request byte for byte and answers with
// response.
//
// A capture has to be a raw listener rather than an http.Server: a server parses the headers
// into a map, and the order they arrived in — which is the part of the fingerprint this test is
// about — is gone by the time a handler can look at one.
func startRawServer(t *testing.T, response string) (string, <-chan capturedRequest) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	captured := make(chan capturedRequest, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		var head bytes.Buffer
		length := 0
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			head.WriteString(line)
			lower := strings.ToLower(line)
			if strings.HasPrefix(lower, "content-length:") {
				length, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(lower, "content-length:")))
			}
			if line == "\r\n" {
				break
			}
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			return
		}
		captured <- capturedRequest{head: head.String(), body: string(body)}
		_, _ = conn.Write([]byte(response))
	}()
	return listener.Addr().String(), captured
}

// headerIndex finds a header line in the captured block, or fails the test.
func headerIndex(t *testing.T, head, name string) int {
	t.Helper()
	for i, line := range strings.Split(head, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), strings.ToLower(name)+":") {
			return i
		}
	}
	t.Fatalf("no %s header in:\n%s", name, head)
	return -1
}

// TestBrowserClientWritesABrowserRequest pins the request itself: the header order Chrome uses
// for a form navigation, the values that have to agree with its user agent, and no header a
// browser would not send. The order is asserted rather than only the presence, because a
// request with Go's alphabetical order is the tell this client exists to avoid.
func TestBrowserClientWritesABrowserRequest(t *testing.T) {
	addr, captured := startRawServer(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")

	client := newBrowserClient(chromeWindows)
	resp, err := client.postForm(context.Background(), "http://"+addr+"/html/",
		url.Values{"q": {"Mietrecht Berlin"}, "b": {""}, "l": {"de-de"}}, "de")
	if err != nil {
		t.Fatalf("postForm: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	request := <-captured
	lines := strings.Split(request.head, "\r\n")
	if lines[0] != "POST /html/ HTTP/1.1" {
		t.Errorf("request line = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "Host: ") {
		t.Errorf("second line = %q, want the Host header", lines[1])
	}
	if !strings.HasPrefix(lines[2], "Content-Length: ") {
		t.Errorf("third line = %q, want Content-Length, as Chrome sends it before the rest", lines[2])
	}

	// The order below is the one measured from a real navigation: pseudo-fields the browser
	// computes, then the hints, then the navigation metadata, then the accepted formats.
	order := []string{
		"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "Upgrade-Insecure-Requests",
		"Origin", "Content-Type", "User-Agent", "Accept", "Sec-Fetch-Site", "Sec-Fetch-Mode",
		"Sec-Fetch-User", "Sec-Fetch-Dest", "Referer", "Accept-Encoding", "Accept-Language",
	}
	previous := -1
	for _, name := range order {
		index := headerIndex(t, request.head, name)
		if index <= previous {
			t.Errorf("%s is out of order in:\n%s", name, request.head)
		}
		previous = index
	}

	if !strings.Contains(request.head, "User-Agent: "+chromeWindows.userAgent) {
		t.Errorf("user agent does not match the profile:\n%s", request.head)
	}
	if !strings.Contains(request.head, "sec-ch-ua: "+chromeWindows.secChUa) {
		t.Errorf("client hints do not match the profile:\n%s", request.head)
	}
	if !strings.Contains(request.head, "Accept-Language: de-DE,de;q=0.9,en;q=0.8") {
		t.Errorf("accept-language does not follow the search language:\n%s", request.head)
	}
	for _, unwanted := range []string{"Accept-Encoding: gzip\r\n", "Connection: close", "Go-http-client"} {
		if strings.Contains(request.head, unwanted) {
			t.Errorf("request carries %q:\n%s", unwanted, request.head)
		}
	}
	if want := "b=&l=de-de&q=Mietrecht+Berlin"; request.body != want {
		t.Errorf("body = %q, want %q", request.body, want)
	}
}

// TestBrowserClientHandshakesAsTheProfile pins the other half of the identity: the connection
// has to be a browser's ClientHello, and its ALPN has to agree with the HTTP/1.1 that is then
// spoken on it — offering h2 and sending HTTP/1.1 is a mismatch no browser produces.
func TestBrowserClientHandshakesAsTheProfile(t *testing.T) {
	var alpn, userAgent string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alpn = r.TLS.NegotiatedProtocol
		userAgent = r.UserAgent()
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	client := newBrowserClient(chromeWindows)
	client.insecure = true // the fixture's certificate is signed by nobody
	resp, err := client.postForm(context.Background(), server.URL,
		url.Values{"q": {"x"}}, "en")
	if err != nil {
		t.Fatalf("postForm over TLS: %v", err)
	}
	_ = resp.Body.Close()

	if alpn != "http/1.1" {
		t.Errorf("negotiated ALPN = %q, want http/1.1", alpn)
	}
	if userAgent != chromeWindows.userAgent {
		t.Errorf("user agent = %q, want the profile's", userAgent)
	}
}

// TestBrowserClientDecodesWhatItAdvertises pins the Accept-Encoding promise: the request asks
// for four encodings, so all four have to be readable. A body that arrives compressed and is
// not decoded is reported as a layout change, which is the most misleading failure there is.
func TestBrowserClientDecodesWhatItAdvertises(t *testing.T) {
	body := []byte("<html>results</html>")

	encodings := []struct {
		name   string
		encode func() []byte
	}{
		{"identity", func() []byte { return body }},
		{"gzip", func() []byte {
			var buf bytes.Buffer
			writer := gzip.NewWriter(&buf)
			_, _ = writer.Write(body)
			_ = writer.Close()
			return buf.Bytes()
		}},
		{"deflate", func() []byte {
			var buf bytes.Buffer
			writer := zlib.NewWriter(&buf)
			_, _ = writer.Write(body)
			_ = writer.Close()
			return buf.Bytes()
		}},
		{"br", func() []byte {
			var buf bytes.Buffer
			writer := brotli.NewWriter(&buf)
			_, _ = writer.Write(body)
			_ = writer.Close()
			return buf.Bytes()
		}},
		{"zstd", func() []byte {
			var buf bytes.Buffer
			writer, err := zstd.NewWriter(&buf)
			if err != nil {
				t.Fatalf("zstd writer: %v", err)
			}
			_, _ = writer.Write(body)
			_ = writer.Close()
			return buf.Bytes()
		}},
	}

	for _, encoding := range encodings {
		t.Run(encoding.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if encoding.name != "identity" {
					w.Header().Set("Content-Encoding", encoding.name)
				}
				_, _ = w.Write(encoding.encode())
			}))
			defer server.Close()

			client := newBrowserClient(chromeWindows)
			resp, err := client.postForm(context.Background(), server.URL, url.Values{"q": {"x"}}, "en")
			if err != nil {
				t.Fatalf("postForm: %v", err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != string(body) {
				t.Errorf("body = %q, want %q", got, body)
			}
		})
	}
}

// TestBrowserClientEndsARequestThatDoesNotAnswer pins the timeout: a host that accepts the
// connection and then says nothing must not hold the search until the turn ends.
func TestBrowserClientEndsARequestThatDoesNotAnswer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Accepted and held: the point of the test.
			defer conn.Close()
		}
	}()

	client := newBrowserClient(chromeWindows)
	client.timeout = 300 * time.Millisecond
	start := time.Now()
	_, err = client.postForm(context.Background(), "http://"+listener.Addr().String()+"/",
		url.Values{"q": {"x"}}, "en")
	if err == nil {
		t.Fatal("err = nil, want the request to end")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("request took %v, want it bounded by the timeout", elapsed)
	}
}
