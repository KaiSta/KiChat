package research

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ddgChallengeFixture is a trimmed capture of what both scraped endpoints
// answered a host DuckDuckGo did not trust: the anti-bot interstitial, served
// with HTTP 202 rather than an error status. It is the reason a block cannot be
// recognised by its status or by an empty parse — a genuine zero-result page has
// no result nodes either, so the interstitial's own markup is the only signal.
const ddgChallengeFixture = `<!DOCTYPE html><html lang="en"><head>
  <title>DuckDuckGo</title>
  <script src="/assets/anomaly.js?sv=html&cc=botnet&ti=1791488009&gk=d4cd0dabcf4caa22ad92fab40844c786"></script>
</head><body>
  <div class="anomaly-modal__mask">
    <div class="anomaly-modal__modal is-ie">
      <p class="anomaly-modal__description">Unfortunately, bots use DuckDuckGo too.
        Please complete the following challenge to confirm this search was made by a human.</p>
      <form class="challenge-form" action="//duckduckgo.com/anomaly.js?sv=html&cc=botnet" method="post">
        <span class="anomaly-modal__instructions">Select all squares containing a duck:</span>
        <input type="checkbox" class="anomaly-modal__check" name="image-check_0837b5f3563ad694d88dc4aa7d8485a8">
        <img class="anomaly-modal__image" src="//duckduckgo.com/challenge/0837b5f3563ad694d88dc4aa7d8485a8.jpg">
        <input type="submit" class="anomaly-modal__submit" value="Submit">
      </form>
    </div>
  </div>
</body></html>`

// newTestDDGProvider points both scraped endpoints at test servers and switches
// off the pacing that separates requests in production, so a test that exercises
// the two-layout fallback does not spend the interval waiting.
func newTestDDGProvider(endpoint, lite string) *DuckDuckGoProvider {
	p := NewDuckDuckGoProvider(endpoint)
	p.LiteURL = lite
	p.limiter = newRateLimiter(0)
	return p
}

// TestDuckDuckGoReportsBotChallenge pins the measured behaviour: a refused
// request must read as a refusal, not as "keine Ergebnisse gefunden (Layout
// möglicherweise geändert)" — that message sends the reader hunting for a
// selector bug in a page DuckDuckGo never served.
func TestDuckDuckGoReportsBotChallenge(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(ddgChallengeFixture))
	}))
	defer srv.Close()

	p := newTestDDGProvider(srv.URL, srv.URL)

	_, err := p.Search(context.Background(), SearchRequest{Query: "Baden Zeitung Karlsruhe heute", Count: 10})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want it to match ErrBlocked", err)
	}
	if strings.Contains(err.Error(), "Layout") {
		t.Errorf("a bot check was reported as a layout problem: %v", err)
	}
	if !strings.Contains(err.Error(), "Captcha") {
		t.Errorf("err = %v, want it to name the challenge", err)
	}
	// The challenge is handed out per client, so the second layout cannot
	// succeed where the first was refused: asking would only spend the pacing
	// delay on a request that is known to fail.
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

// TestDuckDuckGoEmptyResultIsNotABlock keeps the marker honest: a page that
// simply has no hits is still an empty result, which is what the layout message
// is for.
func TestDuckDuckGoEmptyResultIsNotABlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><div class="no-results">Keine Ergebnisse</div></body></html>`))
	}))
	defer srv.Close()

	p := newTestDDGProvider(srv.URL, srv.URL)

	_, err := p.Search(context.Background(), SearchRequest{Query: "x", Count: 10})
	if err == nil {
		t.Fatal("expected an error for a page without results")
	}
	if errors.Is(err, ErrBlocked) {
		t.Errorf("err = %v, want an empty-result error instead of a block", err)
	}
}

// TestDuckDuckGoLiteBlockSurfaces: when the first layout fails for an unrelated
// reason, a block on the second still has to reach the caller as a block instead
// of being buried in a list of failures.
func TestDuckDuckGoLiteBlockSurfaces(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer broken.Close()
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(ddgChallengeFixture))
	}))
	defer blocked.Close()

	p := newTestDDGProvider(broken.URL, blocked.URL)

	_, err := p.Search(context.Background(), SearchRequest{Query: "x", Count: 10})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want it to match ErrBlocked", err)
	}
}

// TestIsDDGChallengeOnRealResultMarkup is the false-positive guard: the two
// recorded result layouts must never be mistaken for the interstitial.
func TestIsDDGChallengeOnRealResultMarkup(t *testing.T) {
	for name, body := range map[string]string{
		"html": ddgFixture,
		"lite": ddgLiteFixture,
	} {
		if isDDGChallenge(body) {
			t.Errorf("%s result markup was detected as a challenge page", name)
		}
	}
	if !isDDGChallenge(ddgChallengeFixture) {
		t.Error("the recorded challenge page was not detected")
	}
}
