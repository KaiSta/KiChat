package llm

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func countingServer(t *testing.T, attempts *int32, fail func(n int32, w http.ResponseWriter) bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(attempts, 1)
		if fail(n, w) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONRetriesTransientFailures(t *testing.T) {
	var attempts int32
	srv := countingServer(t, &attempts, func(n int32, w http.ResponseWriter) bool {
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"model is loading"}`))
			return true
		}
		return false
	})

	var out struct {
		OK bool `json:"ok"`
	}
	err := doJSON(context.Background(), &http.Client{Timeout: 5 * time.Second},
		http.MethodGet, srv.URL, nil, nil, &out)
	if err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if !out.OK {
		t.Error("the retried response body was not decoded into out")
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestDoJSONDoesNotRetryAClientError(t *testing.T) {
	var attempts int32
	srv := countingServer(t, &attempts, func(_ int32, w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
		return true
	})

	err := doJSON(context.Background(), &http.Client{Timeout: 5 * time.Second},
		http.MethodGet, srv.URL, nil, nil, nil)
	if err == nil {
		t.Fatal("want an error from a 400 response")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
		t.Errorf("error = %v, want an *HTTPError with status 400", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1: a 400 must not be retried", got)
	}
}

// TestDoJSONStopsWhenContextEnds pins that a Stop does not have to wait out the
// backoff: once the context is done, no further attempt is made.
func TestDoJSONStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		cancel() // ends the wait between attempts
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	if err := doJSON(ctx, &http.Client{Timeout: 5 * time.Second},
		http.MethodGet, srv.URL, nil, nil, nil); err == nil {
		t.Fatal("want an error once the context ended")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1: the context ended before the retry", got)
	}
}

func TestPostStreamRetriesTheRequestPhase(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"ok\":true}\n\n"))
	}))
	t.Cleanup(srv.Close)

	rc, err := postStream(context.Background(), srv.URL, nil, map[string]any{"stream": true})
	if err != nil {
		t.Fatalf("postStream: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) == 0 {
		t.Error("the retried stream returned an empty body")
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// groqRateLimitBody is the envelope a real Groq account answered with, kept byte
// for byte: the per-minute input-token limit for `qwen/qwen3.8-27b` on the
// `on_demand` tier is 7000, the request below cost 6248, and the wait is written
// into the sentence rather than left to the status code.
const groqRateLimitBody = `{"error":{"message":"Rate limit reached for model ` +
	"`qwen/qwen3.8-27b`" + ` in organization ` + "`org_01kz7a9rsze9da1b4q1yv7w8bx`" +
	` service tier ` + "`on_demand`" + ` on input tokens per minute (ITPM): Limit 7000, ` +
	`Used 1911, Requested 6248. Please try again in 9.934285714s. Need more tokens? ` +
	`Upgrade to Dev Tier today at https://console.groq.com/settings/billing",` +
	`"type":"tokens","code":"rate_limit_exceeded"}}`

// TestRetryWaitHonoursTheBackendsOwnHint is the arithmetic rather than the sleep: a
// throttle that names its wait is obeyed up to one window, and a backend that names
// nothing still gets the doubling backoff it always had.
//
// This is the fix for a real turn that failed: 250 ms and 500 ms of backoff against
// a limit defined per minute spends every attempt inside the window that was just
// refused, so a request the vendor said to retry in ten seconds was reported as a
// failure instead.
func TestRetryWaitHonoursTheBackendsOwnHint(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		attempt int
		want    time.Duration
	}{
		{"no hint, first attempt", &HTTPError{StatusCode: 429}, 0, 250 * time.Millisecond},
		{"no hint, second attempt", &HTTPError{StatusCode: 429}, 1, 500 * time.Millisecond},
		{"no hint, later attempt", &HTTPError{StatusCode: 429}, 5, 2 * time.Second},
		{"a hint is obeyed", &HTTPError{StatusCode: 429, RetryAfter: 1500 * time.Millisecond}, 0, 1500 * time.Millisecond},
		{"a hint is obeyed however late the attempt", &HTTPError{StatusCode: 429, RetryAfter: 9 * time.Second}, 1, 9 * time.Second},
		{"a hint beyond one window is capped", &HTTPError{StatusCode: 429, RetryAfter: time.Hour}, 0, time.Minute},
		{"a 5xx that names a wait", &HTTPError{StatusCode: 503, RetryAfter: 3 * time.Second}, 0, 3 * time.Second},
		{"a transport failure has no hint", &net.OpError{Op: "dial", Err: errors.New("refused")}, 0, 250 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := retryWait(tc.err, tc.attempt); got != tc.want {
			t.Errorf("retryWait(%s) = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestRetryHintIsReadFromTheHeaderAndTheRefusal: the two places a wait is stated.
// The prose form is not a nicety — the backend this was written for puts the number
// in its sentence, and reading only the header would treat it as saying nothing.
func TestRetryHintIsReadFromTheHeaderAndTheRefusal(t *testing.T) {
	withHeader := func(value string) http.Header {
		h := http.Header{}
		h.Set("Retry-After", value)
		return h
	}
	cases := []struct {
		name   string
		header http.Header
		body   string
		want   time.Duration
	}{
		{"delta-seconds", withHeader("12"), "", 12 * time.Second},
		{"fractional delta-seconds", withHeader("0.5"), "", 500 * time.Millisecond},
		{"a garbage header is not a wait", withHeader("soon"), "", 0},
		{"seconds in the sentence", nil, `{"error":"Please try again in 1.5 seconds"}`, 1500 * time.Millisecond},
		{"milliseconds in the sentence", nil, `{"error":"retry in 250ms"}`, 250 * time.Millisecond},
		{"minutes in the sentence", nil, `{"error":"try again in 2 minutes"}`, 2 * time.Minute},
		{"a refusal that says nothing", nil, `{"error":{"code":"invalid_api_key"}}`, 0},
		{"an empty body", nil, "", 0},
	}
	for _, tc := range cases {
		if got := retryHint(tc.header, tc.body); got != tc.want {
			t.Errorf("retryHint(%s) = %s, want %s", tc.name, got, tc.want)
		}
	}

	// The vendor's own envelope, beside a header that cannot be parsed at all, so
	// the sentence is what answers: read as the ~9.93 seconds it asks for rather
	// than as a number this app invented. A window rather than an equality because
	// the value is fractional and the float it is parsed through is not exact.
	if got := retryHint(withHeader("soon"), groqRateLimitBody); got < 9934*time.Millisecond || got > 9935*time.Millisecond {
		t.Errorf("the Groq rate-limit sentence yielded %s, want the ~9.934s it names", got)
	}

	// The other documented spelling of the header, an HTTP date.
	when := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	got := retryHint(withHeader(when), "")
	if got <= 0 || got > 3*time.Second {
		t.Errorf("an HTTP-date Retry-After yielded %s, want a positive wait within it", got)
	}
}

// TestAThrottledRequestWaitsTheHintOut drives the retry loop itself, with a wait
// small enough to sleep through: the hint reaches the wait, and the second attempt
// is made after it rather than after the backoff.
func TestAThrottledRequestWaitsTheHintOut(t *testing.T) {
	const hint = 300 * time.Millisecond
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 2 {
			w.Header().Set("Retry-After", "0.3")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded","message":"Please try again in 0.3s."}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	var out struct {
		OK bool `json:"ok"`
	}
	if err := doJSON(context.Background(), &http.Client{Timeout: 5 * time.Second},
		http.MethodPost, srv.URL, nil, map[string]any{"stream": true}, &out); err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if !out.OK {
		t.Error("the retried response body was not decoded into out")
	}
	if elapsed := time.Since(start); elapsed < hint {
		t.Errorf("the retry came after %s, want at least the %s the backend asked for", elapsed, hint)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// TestAThrottledStreamWaitsTheHintOut covers the construction site a chat turn
// actually goes through: the streaming request phase, where a 429 arrives before any
// token has been read. The hint has to be filled in there too, or the one path the
// user hits keeps retrying inside the minute that refused it.
//
// The wait is short only so the test is: what it pins is the distinction, so the hint
// is well above the 250 ms first backoff (600 ms against it) and the assertion is the
// hint's own floor rather than an elapsed-time coincidence.
func TestAThrottledStreamWaitsTheHintOut(t *testing.T) {
	const backoff = 250 * time.Millisecond
	// Groq's envelope with its wait shortened; the real one, at 9.93 seconds, is
	// parsed in TestRetryHintIsReadFromTheHeaderAndTheRefusal instead of slept through.
	const throttled = `{"error":{"message":"Rate limit reached for model ` + "`qwen/qwen3.8-27b`" +
		` on input tokens per minute (ITPM): Limit 7000, Used 1911, Requested 6248. ` +
		`Please try again in 600ms.","type":"tokens","code":"rate_limit_exceeded"}}`

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 2 {
			// No header at all, as a backend that says it only in prose.
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(throttled))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"ok\":true}\n\n"))
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	rc, err := postStream(context.Background(), srv.URL, nil, map[string]any{"stream": true})
	if err != nil {
		t.Fatalf("postStream: %v", err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if elapsed := time.Since(start); elapsed <= backoff+100*time.Millisecond {
		t.Errorf("the stream retried after %s, which is the backoff rather than the 600ms the envelope names", elapsed)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// TestAnExhaustedThrottleKeepsTheWaitInTheError: once the attempts are spent the
// error still carries what the backend asked for, so the refusal the user ends up
// reading names the wait instead of only the status.
func TestAnExhaustedThrottleKeepsTheWaitInTheError(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Retry-After", "0.2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(groqRateLimitBody))
	}))
	t.Cleanup(srv.Close)

	err := doJSON(context.Background(), &http.Client{Timeout: 5 * time.Second},
		http.MethodPost, srv.URL, nil, map[string]any{"stream": true}, nil)
	if err == nil {
		t.Fatal("want an error from a request that is always throttled")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v, want an *HTTPError", err)
	}
	if httpErr.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %s, want the wait the backend named", httpErr.RetryAfter)
	}
	if got := atomic.LoadInt32(&attempts); got != maxAttempts {
		t.Errorf("attempts = %d, want %d", got, maxAttempts)
	}
}

// TestHTTPErrorRedactsOurOwnCredential covers a gateway that echoes the request
// back in its error page: the key must not survive into the message the user sees.
func TestHTTPErrorRedactsOurOwnCredential(t *testing.T) {
	const key = "sk-not-a-real-key-1234567890"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// A proxy-style error page that repeats what it was sent.
		_, _ = w.Write([]byte(`{"error":"bad request","echoed":"` +
			r.Header.Get("Authorization") + `"}`))
	}))
	t.Cleanup(srv.Close)

	err := doJSON(context.Background(), &http.Client{Timeout: 5 * time.Second},
		http.MethodPost, srv.URL, map[string]string{"Authorization": "Bearer " + key}, nil, nil)
	if err == nil {
		t.Fatal("want an error from a 400 response")
	}
	message := err.Error()
	if strings.Contains(message, key) || strings.Contains(message, "Bearer "+key) {
		t.Errorf("the API key leaked into the error: %s", message)
	}
	if !strings.Contains(message, "[redacted]") {
		t.Errorf("expected a redaction marker in the error: %s", message)
	}
}

// A header that is not a credential must be left alone, so a body that happens
// to quote a content type is not mangled.
func TestRedactionLeavesNonCredentialHeadersAlone(t *testing.T) {
	err := &HTTPError{StatusCode: 400, Body: `sent as application/json`}
	redactHeaders(err, map[string]string{"Content-Type": "application/json"})
	if err.Body != `sent as application/json` {
		t.Errorf("body was rewritten: %s", err.Body)
	}
}

func TestRetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"throttled", &HTTPError{StatusCode: http.StatusTooManyRequests}, true},
		{"server error", &HTTPError{StatusCode: http.StatusInternalServerError}, true},
		{"unavailable", &HTTPError{StatusCode: http.StatusServiceUnavailable}, true},
		{"bad request", &HTTPError{StatusCode: http.StatusBadRequest}, false},
		{"not found", &HTTPError{StatusCode: http.StatusNotFound}, false},
		{"cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"wrapped cancellation", context.DeadlineExceeded, false},
		{"transport failure", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
	}
	for _, tc := range cases {
		if got := retryable(tc.err); got != tc.want {
			t.Errorf("retryable(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// recordingWatcher keeps what a request told it about its waits, in order.
//
// The calls arrive on the goroutine doing the request, so a mutex is not needed to
// avoid a data race with itself — it is here because the streaming path fires a
// watcher from the goroutine that reads the body while a test reads the record from
// its own, and one guarded type covers both.
type recordingWatcher struct {
	mu     sync.Mutex
	waits  []time.Duration
	errs   []error
	waited int
	onWait func()
}

func (w *recordingWatcher) Waiting(wait time.Duration, err error) {
	w.mu.Lock()
	w.waits = append(w.waits, wait)
	w.errs = append(w.errs, err)
	fire := w.onWait
	w.mu.Unlock()
	if fire != nil {
		fire()
	}
}

func (w *recordingWatcher) Waited() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.waited++
}

func (w *recordingWatcher) seen() ([]time.Duration, []error, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.waits...), append([]error(nil), w.errs...), w.waited
}

// TestARetryReportsItsWaitToTheWatcher: a caller that has a window to paint is told
// how long the request is about to sleep and what it is sleeping for, so a wait a
// backend named can be shown as itself instead of as a turn that looks hung.
func TestARetryReportsItsWaitToTheWatcher(t *testing.T) {
	var attempts int32
	srv := countingServer(t, &attempts, func(n int32, w http.ResponseWriter) bool {
		if n == 1 {
			w.Header().Set("Retry-After", "0.2")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached. Please try again in 0.2s."}}`))
			return true
		}
		return false
	})

	watcher := &recordingWatcher{}
	ctx := WithRetryWatcher(context.Background(), watcher)

	var out struct {
		OK bool `json:"ok"`
	}
	if err := doJSON(ctx, &http.Client{Timeout: 5 * time.Second},
		http.MethodGet, srv.URL, nil, nil, &out); err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if !out.OK {
		t.Error("the request was not retried successfully after the reported wait")
	}

	waits, errs, waited := watcher.seen()
	if len(waits) != 1 {
		t.Fatalf("the watcher was told about %d waits, want 1: %v", len(waits), waits)
	}
	// The hint, not the 250 ms backoff: the point of the report is that it carries
	// the number the backend committed to.
	if waits[0] < 150*time.Millisecond || waits[0] > 400*time.Millisecond {
		t.Errorf("the reported wait was %v, want about 200ms", waits[0])
	}
	var httpErr *HTTPError
	if len(errs) != 1 || !errors.As(errs[0], &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the reported cause was %v, want the 429 that caused the wait", errs)
	}
	// Retraction, so a caller that shows the wait is told to stop showing it: the
	// phase would otherwise stand for the rest of the turn.
	if waited != 1 {
		t.Errorf("the watcher was told the wait was over %d times, want 1", waited)
	}
}

// TestAWaitCutShortByAStopIsStillRetracted: the wait also ends when the context
// does — a Stop — and a caller must be told then too, or the phase it set says a
// throttle is still being waited out after the turn is over.
func TestAWaitCutShortByAStopIsStillRetracted(t *testing.T) {
	var attempts int32
	srv := countingServer(t, &attempts, func(_ int32, w http.ResponseWriter) bool {
		// Long enough that sleeping it would be obvious, and reported so the wait is
		// the backend's own number rather than the backoff.
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached. Please try again in 30s."}}`))
		return true
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watcher := &recordingWatcher{onWait: cancel}
	ctx = WithRetryWatcher(ctx, watcher)

	started := time.Now()
	var out struct {
		OK bool `json:"ok"`
	}
	err := doJSON(ctx, &http.Client{Timeout: 30 * time.Second},
		http.MethodGet, srv.URL, nil, nil, &out)
	if err == nil {
		t.Fatal("doJSON returned no error after the wait was cancelled")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("the cancelled wait took %v — it was waited out rather than cut short", elapsed)
	}
	waits, _, waited := watcher.seen()
	if len(waits) != 1 {
		t.Fatalf("the watcher was told about %d waits, want 1", len(waits))
	}
	if waited != 1 {
		t.Errorf("the watcher was told the wait was over %d times, want 1 — it is left showing a wait that ended", waited)
	}
}

// TestAPlainBackoffIsNotReportedToTheWatcher: a 5xx with no retry hint is retried
// after the client's own 250 ms, and the caller is told nothing about it. A line of
// status text that appears and vanishes inside a blink is a glitch on screen, and
// the first attempt is the one that would produce it.
func TestAPlainBackoffIsNotReportedToTheWatcher(t *testing.T) {
	var attempts int32
	srv := countingServer(t, &attempts, func(n int32, w http.ResponseWriter) bool {
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"model is loading"}`))
			return true
		}
		return false
	})

	watcher := &recordingWatcher{}
	ctx := WithRetryWatcher(context.Background(), watcher)

	var out struct {
		OK bool `json:"ok"`
	}
	if err := doJSON(ctx, &http.Client{Timeout: 5 * time.Second},
		http.MethodGet, srv.URL, nil, nil, &out); err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2 — the 503 should still be retried", got)
	}
	if waits, _, waited := watcher.seen(); len(waits) != 0 || waited != 0 {
		t.Errorf("the watcher was told about %v (%d retractions), want silence for a guessed backoff", waits, waited)
	}
}
