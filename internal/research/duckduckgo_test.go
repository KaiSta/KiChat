package research

import (
	"context"
	"errors"
	"fmt"
	"kichat/internal/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// newTestDDGProvider points both scraped endpoints at test servers, switches off
// the pacing that separates requests in production and makes a retry cost no more
// than the call it repeats, so a test that exercises the fallback or the retry
// does not spend the production intervals sleeping.
func newTestDDGProvider(endpoint, lite string) *DuckDuckGoProvider {
	p := NewDuckDuckGoProvider(endpoint)
	p.LiteURL = lite
	p.pacer = newDDGPacer(0, 0)
	p.retryBase = time.Millisecond
	p.jitter = nil
	return p
}

// TestDuckDuckGoReportsBotChallenge pins the measured behaviour: a refused
// request must read as a refusal, not as "keine Ergebnisse gefunden (Layout
// möglicherweise geändert)" — that message sends the reader hunting for a
// selector bug in a page DuckDuckGo never served.
func TestDuckDuckGoReportsBotChallenge(t *testing.T) {
	var htmlHits, liteHits int32
	challenged := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&htmlHits, 1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(ddgChallengeFixture))
	}))
	defer challenged.Close()
	// A lite endpoint that does answer: it must never be asked, and answering makes
	// that failure visible instead of passing for a second refusal.
	lite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&liteHits, 1)
		_, _ = w.Write([]byte(ddgLiteFixture))
	}))
	defer lite.Close()

	p := newTestDDGProvider(challenged.URL, lite.URL)

	_, err := p.Search(context.Background(), SearchRequest{
		Query: "Baden Zeitung Karlsruhe heute", Count: 10, Lang: config.LangGerman})
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
	// delay on a request that is known to fail. The retries are the first
	// layout's, and they are bounded.
	if n := atomic.LoadInt32(&liteHits); n != 0 {
		t.Errorf("lite requests = %d, want 0", n)
	}
	if n := atomic.LoadInt32(&htmlHits); n != ddgBlockTries {
		t.Errorf("html requests = %d, want %d (one look and one retry)", n, ddgBlockTries)
	}
}

// TestDuckDuckGoRetriesABlockWithBackoff pins what the retry is for: a refusal is
// usually the burst the host has just seen rather than a verdict about this client,
// so the next look — after the backoff, and after the cooldown the pacer took —
// answers with results instead of the same challenge.
func TestDuckDuckGoRetriesABlockWithBackoff(t *testing.T) {
	var hits int32
	var first, second time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			first = time.Now()
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(ddgChallengeFixture))
			return
		}
		second = time.Now()
		_, _ = w.Write([]byte(ddgFixture))
	}))
	defer srv.Close()

	p := newTestDDGProvider(srv.URL, srv.URL)
	results, err := p.Search(context.Background(), SearchRequest{Query: "Mietvertrag", Count: 10})
	if err != nil {
		t.Fatalf("err = %v, want the retry to return the results", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
	if wait := second.Sub(first); wait < p.retryBase {
		t.Errorf("retry after %v, want at least the backoff %v", wait, p.retryBase)
	}
}

// TestDuckDuckGoRetriesAServerFault: a 503 is a hiccup rather than an answer, and the
// same loop retries it.
func TestDuckDuckGoRetriesAServerFault(t *testing.T) {
	var htmlHits int32
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&htmlHits, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(ddgFixture))
	}))
	defer html.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer broken.Close()

	p := newTestDDGProvider(html.URL, broken.URL)
	results, err := p.Search(context.Background(), SearchRequest{Query: "Mietvertrag", Count: 10})
	if err != nil {
		t.Fatalf("err = %v, want the retry to return the results", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if n := atomic.LoadInt32(&htmlHits); n != 2 {
		t.Errorf("html requests = %d, want 2 (one look and one retry)", n)
	}
}

// TestDuckDuckGoSpacesAParallelBurst is the shape a model produces: one round of
// search_web calls running together. They have to reach the host one gap apart —
// fired at the same instant, only the first came back with results and the rest with
// the challenge.
func TestDuckDuckGoSpacesAParallelBurst(t *testing.T) {
	var mu sync.Mutex
	var arrivals []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
		_, _ = w.Write([]byte(ddgFixture))
	}))
	defer srv.Close()

	p := newTestDDGProvider(srv.URL, srv.URL)
	// Pacing back on, at a scale a test can afford: 40 ms apart.
	p.pacer = newDDGPacer(40*time.Millisecond, 120*time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := p.Search(context.Background(), SearchRequest{
				Query: fmt.Sprintf("Anfrage %d", n), Count: 3})
			if err != nil {
				t.Errorf("parallel search %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(arrivals) != 3 {
		t.Fatalf("requests = %d, want 3 (one per search)", len(arrivals))
	}
	for i := 1; i < len(arrivals); i++ {
		if gap := arrivals[i].Sub(arrivals[i-1]); gap < 30*time.Millisecond {
			t.Errorf("requests %d and %d arrived %v apart, want the 40ms pacing gap",
				i, i+1, gap)
		}
	}
}

// TestDDGPacerGrowsTheGapAndPutsItBack: a refusal doubles the gap up to the ceiling and
// holds the next slot one whole gap away; a real answer restores the base, so one bad
// burst does not slow every later search.
func TestDDGPacerGrowsTheGapAndPutsItBack(t *testing.T) {
	p := newDDGPacer(20*time.Millisecond, 80*time.Millisecond)
	p.Blocked()
	if p.gap != 20*time.Millisecond {
		t.Errorf("gap after one refusal = %v, want 20ms", p.gap)
	}
	started := time.Now()
	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if waited := time.Since(started); waited < 20*time.Millisecond {
		t.Errorf("waited %v after a refusal, want the cooldown to be observed", waited)
	}
	p.Blocked()
	p.Blocked()
	if p.gap != 80*time.Millisecond {
		t.Errorf("gap after three refusals = %v, want the 80ms ceiling", p.gap)
	}
	p.Blocked()
	if p.gap != 80*time.Millisecond {
		t.Errorf("gap = %v, want it capped at 80ms", p.gap)
	}
	p.Succeeded()
	if p.gap != 20*time.Millisecond {
		t.Errorf("gap after a success = %v, want the base back", p.gap)
	}
}

// TestDDGPacerRefusesToQueueForMinutes: the wait is bounded, because a search that queues
// behind a long cooldown spends the turn to learn what the cooldown already said.
func TestDDGPacerRefusesToQueueForMinutes(t *testing.T) {
	p := newDDGPacer(time.Minute, time.Minute)
	p.Blocked()
	if err := p.Wait(context.Background()); !errors.Is(err, errDDGBusy) {
		t.Fatalf("err = %v, want errDDGBusy", err)
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
