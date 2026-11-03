package research

import (
	"context"
	"errors"
	"fmt"
	"kichat/internal/config"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		t.Errorf("html requests = %d, want %d (one look and two retries)", n, ddgBlockTries)
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

// TestDuckDuckGoOutlastsARefusalThatRefusesTheSecondSearch is the report this policy was
// fixed against: two searches started together, the first answered and the second refused —
// and it stayed refused, so one of two parallel calls failed every time. The host here refuses
// anything arriving within 400 ms of the previous request, which is the shape of the real
// burst refusal, and both searches have to come back with results.
func TestDuckDuckGoOutlastsARefusalThatRefusesTheSecondSearch(t *testing.T) {
	var mu sync.Mutex
	var last time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		now := time.Now()
		tooSoon := !last.IsZero() && now.Sub(last) < 400*time.Millisecond
		last = now
		mu.Unlock()
		if tooSoon {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(ddgChallengeFixture))
			return
		}
		_, _ = w.Write([]byte(ddgFixture))
	}))
	defer srv.Close()

	p := newTestDDGProvider(srv.URL, srv.URL)
	// Pacing on, at a scale a test can afford: 150 ms apart, growing to 600 ms.
	p.pacer = newDDGPacer(150*time.Millisecond, 600*time.Millisecond)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, errs[n] = p.Search(context.Background(), SearchRequest{
				Query: fmt.Sprintf("Anfrage %d", n), Count: 3})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("parallel search %d was refused: %v", i, err)
		}
	}
}

// TestDuckDuckGoRunsParallelSearchesOneAtATime is the shape the model actually produces:
// two search_web calls in one round, which run in parallel goroutines. Spacing them two
// seconds apart is not enough — the host refuses a second request that soon — so the provider
// serves them in sequence, and the second request cannot start before the first has answered.
func TestDuckDuckGoRunsParallelSearchesOneAtATime(t *testing.T) {
	var mu sync.Mutex
	var inFlight, peak, hits int
	var firstDone, secondStart time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		hits++
		n := hits
		if n == 2 {
			secondStart = time.Now()
		}
		mu.Unlock()

		// A search takes a moment; without that, two requests could overlap without a test
		// noticing, because the handler would finish before the next one arrived.
		time.Sleep(60 * time.Millisecond)

		mu.Lock()
		inFlight--
		if n == 1 {
			firstDone = time.Now()
		}
		mu.Unlock()
		_, _ = w.Write([]byte(ddgFixture))
	}))
	defer srv.Close()

	p := newTestDDGProvider(srv.URL, srv.URL)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, errs[n] = p.Search(context.Background(), SearchRequest{
				Query: fmt.Sprintf("Anfrage %d", n), Count: 3})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("parallel search %d failed: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if peak != 1 {
		t.Errorf("%d requests were in flight at once, want them one at a time", peak)
	}
	if firstDone.IsZero() || secondStart.IsZero() {
		t.Fatalf("expected both searches to reach the host, got %d requests", hits)
	}
	if secondStart.Before(firstDone) {
		t.Errorf("the second search started %v before the first had answered", firstDone.Sub(secondStart))
	}
}

// TestDDGPacerGrowsTheGapAndPutsItBack: a refusal doubles the gap up to the ceiling and
// holds the next slot one whole gap away; a real answer restores the base, so one bad
// burst does not slow every later search.
//
// The first refusal doubles it too. It used to leave the gap at the base, which put the retry
// one base gap behind the request it repeated — inside the window that request was refused
// for — so a search refused once was refused again and gave up.
func TestDDGPacerGrowsTheGapAndPutsItBack(t *testing.T) {
	p := newDDGPacer(20*time.Millisecond, 80*time.Millisecond)
	p.Blocked()
	if p.gap != 40*time.Millisecond {
		t.Errorf("gap after one refusal = %v, want the doubled 40ms", p.gap)
	}
	started := time.Now()
	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if waited := time.Since(started); waited < 40*time.Millisecond {
		t.Errorf("waited %v after a refusal, want the cooldown of the grown gap", waited)
	}
	p.Blocked()
	if p.gap != 80*time.Millisecond {
		t.Errorf("gap after two refusals = %v, want the 80ms ceiling", p.gap)
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

// TestDDGPacerHoldsACallerThatIsAlreadyWaiting is the half of the cooldown that was
// missing: a caller asleep on its reserved slot has to be pushed back when another
// caller's refusal moves the queue, or the burst it belongs to fires inside the
// cooldown anyway. Booking the slot when it was asked for, rather than when it is
// used, made the grown gap apply only to searches that had not started yet.
func TestDDGPacerHoldsACallerThatIsAlreadyWaiting(t *testing.T) {
	p := newDDGPacer(100*time.Millisecond, 400*time.Millisecond)
	// The first search takes the first slot; the second is asleep on the next one.
	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	fired := make(chan time.Time, 1)
	go func() {
		if err := p.Wait(context.Background()); err == nil {
			fired <- time.Now()
		}
	}()
	// Give the second caller time to reach its sleep: this is the state the old code
	// could not recover from.
	time.Sleep(30 * time.Millisecond)
	refused := time.Now()
	p.Blocked()

	select {
	case at := <-fired:
		// The refusal doubles the gap to 200 ms and holds the next slot one whole
		// gap out. Firing at the pre-booked 100 ms would be 70 ms after the refusal,
		// inside the cooldown it just created.
		if waited := at.Sub(refused); waited < 150*time.Millisecond {
			t.Errorf("the waiting caller fired %v after the refusal, want the grown 200ms cooldown", waited)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting caller never fired")
	}
}

// TestDDGPacerDoesNotSpendASlotItDeclines: a caller turned away because the queue is too long
// must leave the queue where it found it. Reserving the slot and then declining made a busy
// pacer busier, so a burst could starve the search that came last for no reason of its own.
func TestDDGPacerDoesNotSpendASlotItDeclines(t *testing.T) {
	p := newDDGPacer(time.Minute, time.Minute)
	p.Blocked()
	before := p.next
	if err := p.Wait(context.Background()); !errors.Is(err, errDDGBusy) {
		t.Fatalf("err = %v, want errDDGBusy", err)
	}
	if !p.next.Equal(before) {
		t.Errorf("a declined wait moved the next slot from %v to %v", before, p.next)
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

// TestDuckDuckGoPostsTheEndpointsOwnFields pins the form the endpoint's own page submits: q,
// the empty page cursor b, and the region as l. kl — which this provider used to send — is the
// site's stored setting, not a field of this form, and a form carrying it is one no browser
// posts. The field is checked for presence rather than for a value: b is empty on a first page
// and its presence is the point.
func TestDuckDuckGoPostsTheEndpointsOwnFields(t *testing.T) {
	var posted url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		posted = r.PostForm
		_, _ = w.Write([]byte(ddgFixture))
	}))
	defer srv.Close()

	p := newTestDDGProvider(srv.URL, srv.URL)
	if _, err := p.Search(context.Background(), SearchRequest{
		Query: "Mietrecht", Country: "DE", SearchLang: "de", Count: 10}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	if posted.Get("q") != "Mietrecht" {
		t.Errorf("q = %q, want Mietrecht", posted.Get("q"))
	}
	if !posted.Has("b") {
		t.Errorf("the page cursor b is missing from %v", posted)
	}
	if got := posted.Get("l"); got != "de-de" {
		t.Errorf("l = %q, want de-de", got)
	}
	if posted.Has("kl") {
		t.Errorf("kl is not a field of this form, but was sent: %v", posted)
	}
}
