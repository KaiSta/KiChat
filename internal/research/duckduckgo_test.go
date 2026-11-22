package research

import (
	"context"
	"errors"
	"fmt"
	"io"
	"kichat/internal/config"
	"net/http"
	"net/http/cookiejar"
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
	// succeed where the first was refused. Nor is a refusal retried: measured on
	// 2026-11-03, the flag outlives any retry by minutes, so a second look is a
	// request sent into a host that has already answered.
	if n := atomic.LoadInt32(&liteHits); n != 0 {
		t.Errorf("lite requests = %d, want 0", n)
	}
	if n := atomic.LoadInt32(&htmlHits); n != 1 {
		t.Errorf("html requests = %d, want 1 (a refusal is not retried)", n)
	}
}

// TestDuckDuckGoPausesAfterABlock pins the cooldown a refusal starts: the searches that
// follow must not reach the host at all, and must say how long the pause has left rather
// than failing with a refusal they never received.
func TestDuckDuckGoPausesAfterABlock(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(ddgChallengeFixture))
	}))
	defer srv.Close()

	p := newTestDDGProvider(srv.URL, srv.URL)
	p.cooldown = time.Minute

	if _, err := p.Search(context.Background(), SearchRequest{Query: "Mietvertrag", Count: 10}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("first err = %v, want ErrBlocked", err)
	}
	_, err := p.Search(context.Background(), SearchRequest{Query: "Mietvertrag", Count: 10})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second err = %v, want ErrRateLimited, so the chain can try another provider", err)
	}
	if !strings.Contains(err.Error(), "paused") {
		t.Errorf("err = %v, want it to say the provider is paused", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("requests = %d, want 1: the pause is a request not sent", n)
	}
}

// TestDuckDuckGoResumesAfterTheCooldown is the other half: the pause ends by itself, so a
// provider that was refused is asked again rather than staying silent for the rest of the run.
func TestDuckDuckGoResumesAfterTheCooldown(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(ddgChallengeFixture))
			return
		}
		_, _ = w.Write([]byte(ddgFixture))
	}))
	defer srv.Close()

	p := newTestDDGProvider(srv.URL, srv.URL)
	p.cooldown = 20 * time.Millisecond

	if _, err := p.Search(context.Background(), SearchRequest{Query: "Mietvertrag", Count: 10}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("first err = %v, want ErrBlocked", err)
	}
	time.Sleep(40 * time.Millisecond)
	results, err := p.Search(context.Background(), SearchRequest{Query: "Mietvertrag", Count: 10})
	if err != nil {
		t.Fatalf("second err = %v, want the results once the pause is over", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("requests = %d, want 2", n)
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

// TestDuckDuckGoStopsAskingAfterARefusal is the report this policy was fixed against: four
// searches started together, two of them failing. Measured on 2026-11-03, a refused search was
// refused again 4 s and 8 s later and only worked about three minutes after that - so the retry
// that used to follow a refusal could not succeed, and spent 18 s of the turn proving it while
// sending two more requests into a host that had already answered.
//
// The contract now: one refusal is answered once, by silence, and the searches behind it say so
// at once. They still fail - nothing can rescue this identity for minutes - but they fail in
// milliseconds, so the chain can use another provider instead of stalling the turn.
func TestDuckDuckGoStopsAskingAfterARefusal(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(ddgChallengeFixture))
	}))
	defer srv.Close()

	p := newTestDDGProvider(srv.URL, srv.URL)
	p.cooldown = time.Minute

	const searches = 4
	var wg sync.WaitGroup
	errs := make([]error, searches)
	start := time.Now()
	for i := 0; i < searches; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, errs[n] = p.Search(context.Background(), SearchRequest{
				Query: fmt.Sprintf("Anfrage %d", n), Count: 3})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if !errors.Is(err, ErrBlocked) && !errors.Is(err, ErrRateLimited) {
			t.Errorf("search %d err = %v, want the refusal or the pause", i, err)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("requests = %d, want 1: a refused host is not asked again", n)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the round took %v, want the refusal reported at once", elapsed)
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

// ddgGridFixture is the check as a reader met it on 2026-11-22, trimmed from the live interstitial
// that the test does not ship: the answer form is `#challenge-form`, the page carries `#img-form`
// (the frame target the answer is posted from) before it, the form's own hidden field is `ct`, and
// every square is a checkbox named `image-check_<hash>` whose picture is
// `../assets/anomaly/images/challenge/<hash>.jpg` — relative to the page, which is why the parse
// needs the page's address. The pictures are listed after the boxes and in reverse on purpose: a
// pairing by position would pass a fixture that listed them in step and then pair the wrong
// picture with the wrong box on the real page.
const ddgGridFixture = `<!DOCTYPE html><html lang="de"><head>
  <title>DuckDuckGo</title>
</head><body>
  <form id="img-form" action="//duckduckgo.com/anomaly.js?sv=html&cc=sre&gk=d4cd0dabcf4caa22ad92fab40844c786" target="ifr" method="POST"></form>
  <form id="challenge-form" action="//duckduckgo.com/anomaly.js?sv=html&cc=sre&gk=d4cd0dabcf4caa22ad92fab40844c786" method="POST">
    <div class="anomaly-modal__mask">
      <div class="anomaly-modal__modal is-ie" data-testid="anomaly-modal">
        <div class="anomaly-modal__title">Unfortunately, bots use DuckDuckGo too.</div>
        <div class="anomaly-modal__description">Please complete the following challenge to confirm this search was made by a human.</div>
        <input type="hidden" name="ct" value="AgEAB5wdSrjEZZ2e">
        <div class="anomaly-modal__instructions">Select all squares containing a duck:</div>
        <div class="anomaly-modal__puzzle">
          <div class="anomaly-modal__box" data-index="0"><input type="checkbox" class="anomaly-modal__check" name="image-check_036ef386126084b6275ff34fabda050d" id="image-check_036ef386126084b6275ff34fabda050d"></div>
          <div class="anomaly-modal__box" data-index="1"><input type="checkbox" class="anomaly-modal__check" name="image-check_4f03c9125f97677d3fa603568fc1e2a8" id="image-check_4f03c9125f97677d3fa603568fc1e2a8"></div>
          <div class="anomaly-modal__box" data-index="2"><input type="checkbox" class="anomaly-modal__check" name="image-check_978d9b368b0c4230ea350737bfac6838" id="image-check_978d9b368b0c4230ea350737bfac6838"></div>
          <div class="anomaly-modal__box" data-index="3"><input type="checkbox" class="anomaly-modal__check" name="image-check_2715fb5fbd4f8b0e5f866ddcaafe1a6c" id="image-check_2715fb5fbd4f8b0e5f866ddcaafe1a6c"></div>
          <div class="anomaly-modal__box" data-index="4"><input type="checkbox" class="anomaly-modal__check" name="image-check_9d5e5407b2f04d4f295d2fc1814c6ddd" id="image-check_9d5e5407b2f04d4f295d2fc1814c6ddd"></div>
          <div class="anomaly-modal__box" data-index="5"><input type="checkbox" class="anomaly-modal__check" name="image-check_cb61254942740da72a6dae65642b577d" id="image-check_cb61254942740da72a6dae65642b577d"></div>
          <div class="anomaly-modal__box" data-index="6"><input type="checkbox" class="anomaly-modal__check" name="image-check_386bbfda8ebf70ab247457acc6aaf1ff" id="image-check_386bbfda8ebf70ab247457acc6aaf1ff"></div>
          <div class="anomaly-modal__box" data-index="7"><input type="checkbox" class="anomaly-modal__check" name="image-check_19a35de270e182a720528e91465f7484" id="image-check_19a35de270e182a720528e91465f7484"></div>
          <div class="anomaly-modal__box" data-index="8"><input type="checkbox" class="anomaly-modal__check" name="image-check_b81a45b3407b44b7318acc768eb01c88" id="image-check_b81a45b3407b44b7318acc768eb01c88"></div>
        </div>
        <img class="anomaly-modal__image" alt=" " id="image-b81a45b3407b44b7318acc768eb01c88" src="../assets/anomaly/images/challenge/b81a45b3407b44b7318acc768eb01c88.jpg" data-id="b81a45b3407b44b7318acc768eb01c88.jpg">
        <img class="anomaly-modal__image" alt=" " id="image-19a35de270e182a720528e91465f7484" src="../assets/anomaly/images/challenge/19a35de270e182a720528e91465f7484.jpg" data-id="19a35de270e182a720528e91465f7484.jpg">
        <img class="anomaly-modal__image" alt=" " id="image-386bbfda8ebf70ab247457acc6aaf1ff" src="../assets/anomaly/images/challenge/386bbfda8ebf70ab247457acc6aaf1ff.jpg" data-id="386bbfda8ebf70ab247457acc6aaf1ff.jpg">
        <img class="anomaly-modal__image" alt=" " id="image-cb61254942740da72a6dae65642b577d" src="../assets/anomaly/images/challenge/cb61254942740da72a6dae65642b577d.jpg" data-id="cb61254942740da72a6dae65642b577d.jpg">
        <img class="anomaly-modal__image" alt=" " id="image-9d5e5407b2f04d4f295d2fc1814c6ddd" src="../assets/anomaly/images/challenge/9d5e5407b2f04d4f295d2fc1814c6ddd.jpg" data-id="9d5e5407b2f04d4f295d2fc1814c6ddd.jpg">
        <img class="anomaly-modal__image" alt=" " id="image-2715fb5fbd4f8b0e5f866ddcaafe1a6c" src="../assets/anomaly/images/challenge/2715fb5fbd4f8b0e5f866ddcaafe1a6c.jpg" data-id="2715fb5fbd4f8b0e5f866ddcaafe1a6c.jpg">
        <img class="anomaly-modal__image" alt=" " id="image-978d9b368b0c4230ea350737bfac6838" src="../assets/anomaly/images/challenge/978d9b368b0c4230ea350737bfac6838.jpg" data-id="978d9b368b0c4230ea350737bfac6838.jpg">
        <img class="anomaly-modal__image" alt=" " id="image-4f03c9125f97677d3fa603568fc1e2a8" src="../assets/anomaly/images/challenge/4f03c9125f97677d3fa603568fc1e2a8.jpg" data-id="4f03c9125f97677d3fa603568fc1e2a8.jpg">
        <img class="anomaly-modal__image" alt=" " id="image-036ef386126084b6275ff34fabda050d" src="../assets/anomaly/images/challenge/036ef386126084b6275ff34fabda050d.jpg" data-id="036ef386126084b6275ff34fabda050d.jpg">
        <input type="submit" class="anomaly-modal__submit" value="Submit">
      </div>
    </div>
  </form>
</body></html>`

// ddgResultsAfterCheck is the page the answer redirects to: one ordinary result.
const ddgResultsAfterCheck = `<html><body>
<div class="result results_links"><div class="result__body"><h2 class="result__title">
<a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.org%2Fsolved">Gelöst</a></h2>
<a class="result__snippet">Nach der Prüfung.</a></div></div>
</body></html>`

// TestParseDDGChallengeGrid pins the parse: the tiles, each one paired with its own image by the
// hash the checkbox and the image share, the form's action made absolute, the hidden field kept
// (it is the form's own token and inventing it would be guessing), and the instruction.
func TestParseDDGChallengeGrid(t *testing.T) {
	challenge, ok := parseDDGChallenge(ddgGridFixture, "Mietrecht", config.LangGerman, ddgHTMLEndpoint)
	if !ok {
		t.Fatal("the grid did not parse as a check")
	}
	if len(challenge.Tiles) != 9 {
		t.Fatalf("tiles = %d, want 9: %+v", len(challenge.Tiles), challenge.Tiles)
	}
	for _, tile := range challenge.Tiles {
		hash := strings.TrimPrefix(tile.Name, "image-check_")
		want := "https://html.duckduckgo.com/assets/anomaly/images/challenge/" + hash + ".jpg"
		if tile.URL != want {
			t.Errorf("tile %s names %q, want %q — the pairing is by hash, not by position", tile.Name, tile.URL, want)
		}
	}
	if want := "https://duckduckgo.com/anomaly.js?sv=html&cc=sre&gk=d4cd0dabcf4caa22ad92fab40844c786"; challenge.Action != want {
		t.Errorf("action = %q, want %q", challenge.Action, want)
	}
	if challenge.Fields["ct"] != "AgEAB5wdSrjEZZ2e" {
		t.Errorf("fields = %v, want the form's hidden token", challenge.Fields)
	}
	if !strings.Contains(challenge.Instruction, "duck") {
		t.Errorf("instruction = %q", challenge.Instruction)
	}
	if challenge.Query != "Mietrecht" {
		t.Errorf("query = %q, want the one that was refused", challenge.Query)
	}
}

// TestDuckDuckGoOffersTheCheckToTheWindow is the shape the window depends on: the refusal stays a
// refusal (the chain must still fall through) and the check the page carried reaches the pending
// store and the handler, which is what opens the dialog on it.
func TestDuckDuckGoOffersTheCheckToTheWindow(t *testing.T) {
	ClearPendingChallenge()
	t.Cleanup(ClearPendingChallenge)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, ddgGridFixture)
	}))
	defer srv.Close()

	p := newTestDDGProvider(srv.URL, srv.URL)
	var offered Challenge
	p.onChallenge = func(challenge Challenge) { offered = challenge }

	_, err := p.Search(context.Background(), SearchRequest{
		Query: "Mietrecht", Count: 5, Lang: config.LangGerman})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want it to still classify as ErrBlocked", err)
	}
	if len(offered.Tiles) != 9 {
		t.Errorf("the handler was told about %d tiles, want 9", len(offered.Tiles))
	}
	pending, ok := PendingChallenge()
	if !ok || len(pending.Tiles) != 9 {
		t.Errorf("pending = %+v (%v), want the check kept for the window", pending, ok)
	}
	if !strings.Contains(err.Error(), "Captcha") {
		t.Errorf("err = %v, the sentence the model reads must still name the check", err)
	}
}

// TestSolveChallengePostsTheTickedTilesAndReturnsTheResults covers the whole answer: the ticks and
// the form's own fields are posted to the form's action over the same client, the redirect is
// followed, the cookies the check and the answer set come along, the results are returned, and the
// pause the refusal started is lifted so the next search asks the host again.
func TestSolveChallengePostsTheTickedTilesAndReturnsTheResults(t *testing.T) {
	ClearPendingChallenge()
	t.Cleanup(ClearPendingChallenge)

	var (
		mu          sync.Mutex
		answers     url.Values
		cookieAtEnd string
		searches    int
	)
	// TLS, because the answer is only allowed to leave over https: the guard that keeps the
	// form's action from pointing anywhere is not loosened for a fixture.
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/anomaly/images/challenge/") {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = io.WriteString(w, "\xff\xd8tile")
			return
		}
		switch r.URL.Path {
		case "/anomaly.js":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			answers = r.PostForm
			mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "solved", Value: "1", Path: "/"})
			http.Redirect(w, r, "/results?q=Mietrecht", http.StatusSeeOther)
		case "/results":
			mu.Lock()
			cookieAtEnd = r.Header.Get("Cookie")
			mu.Unlock()
			_, _ = io.WriteString(w, ddgResultsAfterCheck)
		default:
			mu.Lock()
			searches++
			attempt := searches
			mu.Unlock()
			if attempt == 1 {
				// The first search is refused with the check; the host trusts the identity
				// again once it has been answered.
				http.SetCookie(w, &http.Cookie{Name: "issued", Value: "1", Path: "/"})
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, strings.ReplaceAll(ddgGridFixture, "//duckduckgo.com", srv.URL))
				return
			}
			_, _ = io.WriteString(w, ddgResultsAfterCheck)
		}
	}))
	defer srv.Close()

	p := newTestDDGProvider(srv.URL, srv.URL+"/lite")
	// A jar of this test's own: the process-wide one belongs to the identity and other tests
	// would then be reading this one's cookies.
	p.client.jar, _ = cookiejar.New(nil)
	p.client.insecure = true

	if _, err := p.Search(context.Background(), SearchRequest{
		Query: "Mietrecht", Count: 5, Lang: config.LangGerman}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("search err = %v, want ErrBlocked", err)
	}
	challenge, ok := PendingChallenge()
	if !ok {
		t.Fatal("no check was kept for the window")
	}

	// A tile is asked for by the name the window shows, over the same identity, and a name the
	// check does not have is answered without a request.
	body, kind, err := p.fetchTile(context.Background(), challenge.Tiles[2].Name)
	if err != nil {
		t.Fatalf("fetchTile: %v", err)
	}
	if string(body) != "\xff\xd8tile" || kind != "image/jpeg" {
		t.Errorf("tile = %q (%s), want the picture the host served", body, kind)
	}
	if _, _, err := p.fetchTile(context.Background(), "image-check_none"); !errors.Is(err, ErrNoChallenge) {
		t.Errorf("a tile the check does not have: err = %v, want ErrNoChallenge", err)
	}

	ticked := []string{challenge.Tiles[0].Name, challenge.Tiles[4].Name}
	results, err := p.SolveChallenge(context.Background(), ticked)
	if err != nil {
		t.Fatalf("SolveChallenge: %v", err)
	}
	if len(results) != 1 || results[0].URL != "https://example.org/solved" {
		t.Fatalf("results = %+v, want the page the answer redirected to", results)
	}

	mu.Lock()
	posted, cookie := answers, cookieAtEnd
	mu.Unlock()
	for _, name := range ticked {
		if posted.Get(name) != "on" {
			t.Errorf("posted %v, want %s=on", posted, name)
		}
	}
	if unticked := challenge.Tiles[1].Name; posted.Get(unticked) != "" {
		t.Errorf("an unticked tile was sent: %s", unticked)
	}
	if posted.Get("ct") != "AgEAB5wdSrjEZZ2e" {
		t.Errorf("the form's own hidden field was dropped: %v", posted)
	}
	if !strings.Contains(cookie, "issued=1") || !strings.Contains(cookie, "solved=1") {
		t.Errorf("cookies on the results request = %q, want the ones the check and the answer set", cookie)
	}
	if _, pending := PendingChallenge(); pending {
		t.Error("the answered check was left pending")
	}

	// The pause the refusal started is over: without that, this search would be refused by our
	// own cooldown before the host was asked at all.
	again, err := p.Search(context.Background(), SearchRequest{
		Query: "Mietrecht zwei", Count: 5, Lang: config.LangGerman})
	if err != nil {
		t.Fatalf("search after the answer: %v", err)
	}
	if len(again) != 1 {
		t.Errorf("results after the answer = %+v, want the host's answer", again)
	}
}
