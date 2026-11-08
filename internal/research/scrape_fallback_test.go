package research

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestScrapeRetriesARefusalAsABrowser pins the fallback: a 403 from the ordinary client is
// retried once over the Chrome profile. The first request is the Firefox user agent the
// scraper sends normally; the second has to be the profile's user agent, or the retry is only
// a second ordinary request.
func TestScrapeRetriesARefusalAsABrowser(t *testing.T) {
	var hits atomic.Int32
	var browserSeen atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.Contains(r.UserAgent(), "Chrome/133") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		browserSeen.Store(true)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "browser answer")
	}))
	t.Cleanup(srv.Close)

	scraper := NewScraper(nil, ScrapeOptions{AllowPrivateHosts: true, Timeout: 2 * time.Second}, nil)
	result, err := scraper.Scrape(context.Background(), srv.URL+"/blocked", 1000)
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	if !strings.Contains(result.Markdown, "browser answer") {
		t.Errorf("the retry's answer did not reach the result: %q", result.Markdown)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("requests = %d, want the ordinary fetch and one retry", got)
	}
	if !browserSeen.Load() {
		t.Error("the retry did not use the browser profile")
	}
}

// TestScrapeDoesNotRetryARateLimitAsABrowser: 429 is a rate limit, not a verdict on the client
// identity. Retrying it with a new handshake would be the wrong answer and costs another
// request to a host that has already said to slow down.
func TestScrapeDoesNotRetryARateLimitAsABrowser(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	scraper := NewScraper(nil, ScrapeOptions{AllowPrivateHosts: true, Timeout: 2 * time.Second}, nil)
	_, err := scraper.Scrape(context.Background(), srv.URL+"/limited", 1000)
	var refused *httpError
	if !errors.As(err, &refused) || refused.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want the 429 reported", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("requests = %d, want no browser retry for a 429", got)
	}
}

// TestBrowserFallbackStillHonoursRobots: the retry happens after the robots check and must not
// be a way around it. The page is never fetched, so neither the ordinary client nor the
// fallback can reach it.
func TestBrowserFallbackStillHonoursRobots(t *testing.T) {
	var pageHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = io.WriteString(w, "User-agent: *\nDisallow: /\n")
			return
		}
		pageHits.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	scraper := NewScraper(nil, ScrapeOptions{
		AllowPrivateHosts: true,
		RespectRobots:     true,
		Timeout:           2 * time.Second,
	}, nil)
	if _, err := scraper.Scrape(context.Background(), srv.URL+"/page", 1000); err == nil {
		t.Fatal("the scrape succeeded despite robots.txt disallowing the page")
	}
	if got := pageHits.Load(); got != 0 {
		t.Errorf("page requests = %d, want robots to stop them all", got)
	}
}

// TestBrowserFallbackGuardsEveryRedirect: the ordinary client re-guards each redirect hop, and
// the fallback has to do the same. The guard rejects the second URL before it is requested.
func TestBrowserFallbackGuardsEveryRedirect(t *testing.T) {
	var nextHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/next", http.StatusFound)
			return
		}
		nextHits.Add(1)
		_, _ = io.WriteString(w, "should not be reached")
	}))
	t.Cleanup(srv.Close)

	target, err := url.Parse(srv.URL + "/start")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	client := newBrowserClient(chromeWindows)
	client.timeout = 2 * time.Second
	guard := func(u *url.URL) error {
		if u.Path == "/next" {
			return errors.New("blocked redirect")
		}
		return nil
	}
	if _, err := client.get(context.Background(), target, "en", guard); err == nil ||
		!strings.Contains(err.Error(), "blocked redirect") {
		t.Fatalf("err = %v, want the redirect guard's refusal", err)
	}
	if got := nextHits.Load(); got != 0 {
		t.Errorf("redirect target requests = %d, want the guard to stop it", got)
	}
}

// TestBrowserClientWritesABrowserNavigation pins the GET request itself: the order, the
// profile's user agent, and fetch metadata that describes a typed navigation rather than a
// form post. Without this the fallback could send the right user agent on the wrong request.
func TestBrowserClientWritesABrowserNavigation(t *testing.T) {
	addr, captured := startRawServer(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")

	client := newBrowserClient(chromeWindows)
	target, err := url.Parse("http://" + addr + "/page")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resp, err := client.get(context.Background(), target, "zh", nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	request := <-captured
	if lines := strings.Split(request.head, "\r\n"); lines[0] != "GET /page HTTP/1.1" {
		t.Errorf("request line = %q", lines[0])
	}
	order := []string{
		"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "Upgrade-Insecure-Requests",
		"User-Agent", "Accept", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-User",
		"Sec-Fetch-Dest", "Accept-Encoding", "Accept-Language",
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
	if !strings.Contains(request.head, "Sec-Fetch-Site: none\r\n") {
		t.Errorf("a typed navigation is not marked as one:\n%s", request.head)
	}
	if !strings.Contains(request.head, "Accept-Language: zh-CN,zh;q=0.9,en;q=0.8\r\n") {
		t.Errorf("accept-language does not follow the app language:\n%s", request.head)
	}
	for _, unwanted := range []string{"Origin:", "Content-Type:", "Content-Length:", "Referer:"} {
		if strings.Contains(request.head, unwanted) {
			t.Errorf("request carries %q:\n%s", unwanted, request.head)
		}
	}
	if request.body != "" {
		t.Errorf("GET body = %q, want empty", request.body)
	}
}
