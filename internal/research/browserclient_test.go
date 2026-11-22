package research

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	utls "github.com/refraction-networking/utls"
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
	var held []net.Conn
	var mu sync.Mutex
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Accepted and then held open without an answer: that is the test.
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
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

// TestBrowserClientDecodesChainedAndEmptyBodies covers two shapes the single-encoding test
// misses and a proxy can produce: a body encoded twice (legal, and what an intermediary in
// front of a compressed response does), and a response that claims an encoding but carries no
// body at all. Both used to be reported as an undecodable response, which reads as a broken
// layout rather than as what it is.
func TestBrowserClientDecodesChainedAndEmptyBodies(t *testing.T) {
	body := []byte("<html>results</html>")

	t.Run("chained", func(t *testing.T) {
		var gzipped bytes.Buffer
		gzipWriter := gzip.NewWriter(&gzipped)
		_, _ = gzipWriter.Write(body)
		_ = gzipWriter.Close()
		var chained bytes.Buffer
		brotliWriter := brotli.NewWriter(&chained)
		_, _ = brotliWriter.Write(gzipped.Bytes())
		_ = brotliWriter.Close()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip, br")
			_, _ = w.Write(chained.Bytes())
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

	t.Run("empty", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusAccepted)
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
		if len(got) != 0 {
			t.Errorf("body = %q, want empty", got)
		}
	})
}

// measuredSecChUA is what real Chrome builds sent, read from the per-release notes at
// clearcotelabs.com/chrome-user-agent (measured there on branded Chrome 151–158). Eight versions
// cover all six brand orders, all three GREASE versions and enough of the separator list to make
// the generator's rules testable rather than self-referential.
var measuredSecChUA = map[int]string{
	151: `"Not=A?Brand";v="99", "Google Chrome";v="151", "Chromium";v="151"`,
	152: `"Chromium";v="152", "Not?A_Brand";v="24", "Google Chrome";v="152"`,
	153: `"Google Chrome";v="153", "Not_A Brand";v="8", "Chromium";v="153"`,
	154: `"Chromium";v="154", "Google Chrome";v="154", "Not A(Brand";v="99"`,
	155: `"Google Chrome";v="155", "Chromium";v="155", "Not(A:Brand";v="24"`,
	156: `"Not:A-Brand";v="8", "Chromium";v="156", "Google Chrome";v="156"`,
	157: `"Not-A.Brand";v="99", "Google Chrome";v="157", "Chromium";v="157"`,
	158: `"Chromium";v="158", "Not.A/Brand";v="24", "Google Chrome";v="158"`,
}

// TestSecChUAMatchesChromiumsRules pins the generator against those measurements, and pins what
// this build sends to the measured 158 value. The hand-copied literal this replaced had drifted
// in all three of brand order, GREASE version and pair, and nothing noticed — "it looks
// plausible" is not the standard for a header a bot check reads.
func TestSecChUAMatchesChromiumsRules(t *testing.T) {
	for major, want := range measuredSecChUA {
		if got := chromeSecChUa(major); got != want {
			t.Errorf("chromeSecChUa(%d) = %s, want %s", major, got, want)
		}
	}
	want, ok := measuredSecChUA[chromeMajor]
	if !ok {
		t.Fatalf("no measured Sec-CH-UA for Chrome %d — add the row from a real build before "+
			"moving chromeMajor", chromeMajor)
	}
	if chromeWindows.secChUa != want {
		t.Errorf("the profile sends %s, want the measured %s", chromeWindows.secChUa, want)
	}
}

// TestIdentityMajorsAgree is the point of deriving the strings from one number: the user agent,
// the hints and the hello have to describe the same browser. It fails when the constant has been
// moved past the captured hello, and it fails when the hello has been swapped for one the user
// agent does not name — uTLS' HelloChrome_Auto resolves to 155 today and would follow a
// dependency bump without this.
func TestIdentityMajorsAgree(t *testing.T) {
	const uaPrefix, uaSuffix = "Chrome/", ".0.0.0 Safari/537.36"
	agent := chromeWindows.userAgent
	index := strings.Index(agent, uaPrefix)
	if index < 0 || !strings.HasSuffix(agent, uaSuffix) {
		t.Fatalf("user agent = %q, want a Chrome major between %q and %q", agent, uaPrefix, uaSuffix)
	}
	uaMajor, err := strconv.Atoi(strings.TrimSuffix(agent[index+len(uaPrefix):], uaSuffix))
	if err != nil {
		t.Fatalf("user agent does not carry a numeric major: %v", err)
	}
	if uaMajor != chromeWindows.major || chromeWindows.major != chromeMajor {
		t.Errorf("user agent says %d, the profile says %d, the constant says %d",
			uaMajor, chromeWindows.major, chromeMajor)
	}

	// Two brands carry the real major; the third is Chromium's fake one and carries its own
	// version on purpose.
	if got := strings.Count(chromeWindows.secChUa, fmt.Sprintf("v=%q", strconv.Itoa(chromeMajor))); got != 2 {
		t.Errorf("hints name the major %d times, want 2: %s", got, chromeWindows.secChUa)
	}

	capture, ok := chromeHelloFor(chromeMajor)
	if !ok {
		t.Fatalf("no captured ClientHello covers Chrome %d — add one to chromeHellos", chromeMajor)
	}
	if chromeWindows.hello != capture.hello {
		t.Errorf("hello = %+v, want the capture taken from Chrome %d", chromeWindows.hello, capture.version)
	}
	if got := chromeWindows.hello.Version; got != "155" {
		t.Errorf("hello version = %q, want 155 pinned by name: HelloChrome_Auto moves with the dependency", got)
	}
}

// captureClientHello listens for one connection, records the first TLS record it sends and then
// closes: the ClientHello is the subject, and a server that never answers ends the client's
// handshake with an error the caller ignores.
func captureClientHello(t *testing.T) (string, <-chan []byte) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	hellos := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		header := make([]byte, 5)
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		payload := make([]byte, int(binary.BigEndian.Uint16(header[3:])))
		if _, err := io.ReadFull(conn, payload); err != nil {
			return
		}
		hellos <- append(header, payload...)
	}()
	return listener.Addr().String(), hellos
}

// helloFrom reads the capture, or fails rather than hanging on a connection that never spoke.
func helloFrom(t *testing.T, hellos <-chan []byte) []byte {
	t.Helper()
	select {
	case record := <-hellos:
		return record
	case <-time.After(10 * time.Second):
		t.Fatal("no ClientHello was captured")
		return nil
	}
}

// isGREASE reports whether a value is one of the reserved GREASE codepoints
// (draft-davidben-tls-grease-01): the same byte twice, with 0a0a in the low nibbles. JA4 ignores
// them in both counts and both hashes.
func isGREASE(value uint16) bool {
	return value&0x0f0f == 0x0a0a && byte(value>>8) == byte(value)
}

// truncatedJA4Hash is a SHA-256 of the comma-joined list, in lower case, cut to twelve characters.
func truncatedJA4Hash(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:12]
}

// rawExtension is one extension of a captured ClientHello: its codepoint and the bytes after the
// length prefix.
type rawExtension struct {
	id   uint16
	data []byte
}

// parsedHello is the part of a ClientHello these tests read.
type parsedHello struct {
	ciphers    []uint16
	extensions []rawExtension
}

// extension answers the first extension with id, or nil when the hello does not carry one.
func (h parsedHello) extension(id uint16) []byte {
	for _, ext := range h.extensions {
		if ext.id == id {
			return ext.data
		}
	}
	return nil
}

// parseClientHello walks the record a client actually put on the wire. It is a hand-written walk
// rather than a call into uTLS' own fingerprinting because the assertion has to be about the
// bytes the server sees, not about the spec we believe we configured.
func parseClientHello(t *testing.T, record []byte) parsedHello {
	t.Helper()
	if len(record) < 5 || record[0] != 0x16 {
		t.Fatalf("captured %d bytes, which is not a TLS handshake record", len(record))
	}
	handshake := record[5:]
	if len(handshake) < 4 || handshake[0] != 0x01 {
		t.Fatalf("handshake type = %#x, want a ClientHello", handshake[0])
	}
	body := handshake[4:]

	pos := 0
	take := func(length int) []byte {
		if length < 0 || pos+length > len(body) {
			t.Fatalf("ClientHello ends inside a field at offset %d (need %d of %d)", pos, length, len(body))
		}
		out := body[pos : pos+length]
		pos += length
		return out
	}
	_ = take(2)  // legacy_version, frozen at 1.2 by every TLS 1.3 client
	_ = take(32) // random
	_ = take(int(take(1)[0]))
	cipherBlock := take(int(binary.BigEndian.Uint16(take(2))))
	_ = take(int(take(1)[0])) // compression methods
	extensionBlock := take(int(binary.BigEndian.Uint16(take(2))))

	out := parsedHello{ciphers: make([]uint16, 0, len(cipherBlock)/2)}
	for i := 0; i+1 < len(cipherBlock); i += 2 {
		out.ciphers = append(out.ciphers, binary.BigEndian.Uint16(cipherBlock[i:]))
	}
	for i := 0; i+4 <= len(extensionBlock); {
		id := binary.BigEndian.Uint16(extensionBlock[i:])
		length := int(binary.BigEndian.Uint16(extensionBlock[i+2:]))
		i += 4
		if i+length > len(extensionBlock) {
			t.Fatalf("extension %04x overruns the block", id)
		}
		out.extensions = append(out.extensions, rawExtension{id: id, data: extensionBlock[i : i+length]})
		i += length
	}
	return out
}

// applicationProtocols decodes the ALPN and ALPS framing: a list length, then length-prefixed
// names.
func applicationProtocols(data []byte) []string {
	if len(data) < 2 {
		return nil
	}
	length := int(binary.BigEndian.Uint16(data))
	data = data[2:]
	if length > len(data) {
		length = len(data)
	}
	var protocols []string
	for i := 0; i < length; {
		size := int(data[i])
		i++
		if i+size > len(data) {
			return protocols
		}
		protocols = append(protocols, string(data[i:i+size]))
		i += size
	}
	return protocols
}

// ja4Of computes the JA4 fingerprint of one captured ClientHello record, following FoxIO's
// specification: transport, version, SNI, cipher and extension counts and the first ALPN value;
// then a truncated SHA-256 of the hex-sorted cipher list; then one of the hex-sorted extension ids
// (without SNI and ALPN, which the prefix already carries) joined to the signature algorithms in
// the order they were sent.
//
// It is computed from the bytes rather than from the spec that produced them, so the assertion is
// about what a bot check would hash, not about what we believe we configured.
func ja4Of(t *testing.T, record []byte) string {
	t.Helper()
	parsed := parseClientHello(t, record)
	ciphers, extensions := parsed.ciphers, parsed.extensions

	var (
		hasSNI     bool
		firstALPN  string
		signatures []uint16
		versions   []uint16
	)
	for _, ext := range extensions {
		switch ext.id {
		case 0x0000:
			hasSNI = true
		case 0x0010:
			// A list length, then one length-prefixed name: JA4 describes the first.
			if len(ext.data) >= 3 {
				nameLength := int(ext.data[2])
				if 3+nameLength <= len(ext.data) {
					firstALPN = string(ext.data[3 : 3+nameLength])
				}
			}
		case 0x000d:
			for i := 2; i+1 < len(ext.data); i += 2 {
				signatures = append(signatures, binary.BigEndian.Uint16(ext.data[i:]))
			}
		case 0x002b:
			for i := 1; i+1 < len(ext.data); i += 2 {
				versions = append(versions, binary.BigEndian.Uint16(ext.data[i:]))
			}
		}
	}

	version := uint16(0x0303)
	for _, candidate := range versions {
		if !isGREASE(candidate) && candidate > version {
			version = candidate
		}
	}
	versionWords := map[uint16]string{
		0x0304: "13", 0x0303: "12", 0x0302: "11", 0x0301: "10", 0x0300: "s3",
	}
	versionWord, known := versionWords[version]
	if !known {
		t.Fatalf("unexpected version %04x in the hello", version)
	}

	alpn := "00"
	if firstALPN != "" {
		first, last := firstALPN[0], firstALPN[len(firstALPN)-1]
		if isASCIIAlphanumeric(first) && isASCIIAlphanumeric(last) {
			alpn = string(first) + string(last)
		}
	}

	cipherHex := make([]string, 0, len(ciphers))
	for _, cipher := range ciphers {
		if isGREASE(cipher) {
			continue
		}
		cipherHex = append(cipherHex, fmt.Sprintf("%04x", cipher))
	}
	extensionCount := 0
	extensionHex := make([]string, 0, len(extensions))
	for _, ext := range extensions {
		if isGREASE(ext.id) {
			continue
		}
		extensionCount++
		if ext.id == 0x0000 || ext.id == 0x0010 {
			continue
		}
		extensionHex = append(extensionHex, fmt.Sprintf("%04x", ext.id))
	}
	signatureHex := make([]string, 0, len(signatures))
	for _, signature := range signatures {
		if isGREASE(signature) {
			continue
		}
		signatureHex = append(signatureHex, fmt.Sprintf("%04x", signature))
	}
	sort.Strings(cipherHex)
	sort.Strings(extensionHex)

	count := func(n int) string {
		if n > 99 {
			n = 99
		}
		return fmt.Sprintf("%02d", n)
	}
	sni := "i"
	if hasSNI {
		sni = "d"
	}
	prefix := "t" + versionWord + sni + count(len(cipherHex)) + count(extensionCount) + alpn
	return prefix + "_" + truncatedJA4Hash(strings.Join(cipherHex, ",")) + "_" +
		truncatedJA4Hash(strings.Join(extensionHex, ",")+"_"+strings.Join(signatureHex, ","))
}

func isASCIIAlphanumeric(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// helloServerName is the name the captured connections claim. It is not the fixture's address:
// SNI is part of the fingerprint, and a hello sent to an address measures as "i" instead of "d".
const helloServerName = "html.duckduckgo.com"

// helloAsThePreset captures the ClientHello uTLS builds for the profile with no edit of ours: the
// browser that was captured. The fixture never answers, so the handshake ends in an error that is
// the point — the hello has already been sent by then.
func helloAsThePreset(t *testing.T) []byte {
	t.Helper()
	addr, hellos := captureClientHello(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	uconn := utls.UClient(conn, &utls.Config{
		ServerName:         helloServerName,
		InsecureSkipVerify: true,
	}, chromeWindows.hello)
	_ = uconn.Handshake()
	return helloFrom(t, hellos)
}

// helloAsThisClient captures the ClientHello browserClient actually puts on the wire, through the
// same handshake path a search takes.
func helloAsThisClient(t *testing.T) []byte {
	t.Helper()
	addr, hellos := captureClientHello(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := newBrowserClient(chromeWindows)
	_, _ = client.handshake(context.Background(), conn, helloServerName)
	return helloFrom(t, hellos)
}

// TestBrowserClientPresentsChromesJa4 pins the handshake against the value the outside world
// computes from it. The published JA4 of the captured Chrome 155 ClientHello is
// t13d1517h2_8daaf6152771_cb7bf5808d99 (the 133 one was t13d1516h2_…_d8a2da3f94cd); if our bytes
// produce it, the hello is Chrome's and not merely Chrome-shaped.
func TestBrowserClientPresentsChromesJa4(t *testing.T) {
	const chrome155JA4 = "t13d1517h2_8daaf6152771_cb7bf5808d99"

	t.Run("profile", func(t *testing.T) {
		if got := ja4Of(t, helloAsThePreset(t)); got != chrome155JA4 {
			t.Errorf("JA4 = %s, want the published %s", got, chrome155JA4)
		}
	})

	// The one place this client is deliberately not a browser: it speaks HTTP/1.1, so the ALPN
	// field of the fingerprint is h1. Asserted rather than assumed, so the difference cannot
	// grow unnoticed.
	t.Run("client", func(t *testing.T) {
		const want = "t13d1517h1_8daaf6152771_cb7bf5808d99"
		if got := ja4Of(t, helloAsThisClient(t)); got != want {
			t.Errorf("JA4 = %s, want %s", got, want)
		}
	})
}

// TestALPNAndALPSSendOneProtocolList pins the pair the two protocol lists have to make. Chrome
// derives its ALPS list from the protocols it offers in ALPN, so a client that advertises h2 in
// ALPS while its ALPN says http/1.1 only is offering something no browser sends — which is what
// this package did while the two lists were set independently. The preset half proves the cut is
// still needed (a uTLS release could change the preset), the client half pins what we send.
func TestALPNAndALPSSendOneProtocolList(t *testing.T) {
	// 0x0010 is ALPN; 0x44cd is the codepoint uTLS uses for Chrome's newer ALPS form.
	const alpnCode, alpsCode uint16 = 0x0010, 0x44cd

	protocolsOf := func(t *testing.T, record []byte) (alpn, alps []string) {
		t.Helper()
		hello := parseClientHello(t, record)
		alpn = applicationProtocols(hello.extension(alpnCode))
		alps = applicationProtocols(hello.extension(alpsCode))
		if len(alpn) == 0 {
			t.Fatal("the hello carries no ALPN extension")
		}
		if len(alps) == 0 {
			t.Fatal("the hello carries no ALPS extension, so it is not the Chrome profile")
		}
		return alpn, alps
	}

	t.Run("the captured profile offers h2 in both", func(t *testing.T) {
		alpn, alps := protocolsOf(t, helloAsThePreset(t))
		if !slices.Contains(alpn, "h2") || !slices.Contains(alps, "h2") {
			t.Fatalf("preset offers ALPN %v and ALPS %v, want h2 in both — the cut below exists for this", alpn, alps)
		}
	})

	t.Run("this client offers only what it speaks", func(t *testing.T) {
		alpn, alps := protocolsOf(t, helloAsThisClient(t))
		want := []string{chromeALPN}
		if !slices.Equal(alpn, want) {
			t.Errorf("ALPN = %v, want %v", alpn, want)
		}
		if !slices.Equal(alps, want) {
			t.Errorf("ALPS = %v, want %v — a browser derives its ALPS list from ALPN, so a mismatch is a pair no client produces",
				alps, want)
		}
	})
}
