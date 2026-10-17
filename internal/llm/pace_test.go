package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// groqBudgetServer is a Groq-shaped account that publishes its minute on every
// response: the allowance, what is left of it and how long that lasts. It is the only
// place those three numbers exist — no route of this API states them without being
// asked a question — so a test that wants a spent minute has to have answered one.
type groqBudgetServer struct {
	limit     string
	remaining string
	reset     string
	reply     string

	mu    sync.Mutex
	calls int
}

func (g *groqBudgetServer) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		g.mu.Lock()
		g.calls++
		g.mu.Unlock()
		if g.limit != "" {
			w.Header().Set(rateLimitTokensHeader, g.limit)
		}
		if g.remaining != "" {
			w.Header().Set(rateLimitRemainingTokensHeader, g.remaining)
		}
		if g.reset != "" {
			w.Header().Set(rateLimitResetTokensHeader, g.reset)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", g.reply)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *groqBudgetServer) requestCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// groqBudgetProvider is the provider that ships for this vendor, pointed at a test
// server: the address rules, the key check and the header reading are the real ones.
func groqBudgetProvider(t *testing.T, srv *httptest.Server) *openAICompatProvider {
	t.Helper()
	return newGroqProvider("groq", "Groq", srv.URL+"/openai/v1", "test-key")
}

// runTurn runs one request to completion and reports the error, if the request phase
// had one. It is named apart from the drain helper other files in this package use:
// this one drives a provider through its own Chat, so the header reading and the
// pacing that sits in front of the request are the real ones.
func runTurn(t *testing.T, p Provider, ctx context.Context, req ChatRequest) error {
	t.Helper()
	stream, err := p.Chat(ctx, req)
	if err != nil {
		return err
	}
	for range stream {
	}
	return nil
}

// TestTheMinuteIsReadFromTheResponsesOwnHeaders: the three numbers, from the response
// that stated them, and nothing invented for a response that states none.
//
// The limit is the same header the per-request ceiling comes from, which is the fact
// that makes this vendor's budget plannable at all: the tokens-per-minute allowance is
// also the largest request the account may send, so one number answers both questions.
func TestTheMinuteIsReadFromTheResponsesOwnHeaders(t *testing.T) {
	cases := []struct {
		name      string
		limit     string
		remaining string
		reset     string
		want      modelBudget
		ok        bool
	}{
		{
			name:  "the measured shape",
			limit: "18000", remaining: "17997", reset: "7.66s",
			want: modelBudget{Limit: 18000, Remaining: 17997, Reset: 7660 * time.Millisecond}, ok: true,
		},
		{
			name:  "a minute and a half",
			limit: "8000", remaining: "12", reset: "1m30s",
			want: modelBudget{Limit: 8000, Remaining: 12, Reset: 90 * time.Second}, ok: true,
		},
		{
			name:  "a spent minute",
			limit: "8000", remaining: "0", reset: "7.66s",
			want: modelBudget{Limit: 8000, Remaining: 0, Reset: 7660 * time.Millisecond}, ok: true,
		},
		// A remainder with no readable reset says how much is left but not for how
		// long, and only the two together say whether waiting helps.
		{name: "no reset", limit: "8000", remaining: "12", reset: ""},
		{name: "an unreadable reset", limit: "8000", remaining: "12", reset: "soon"},
		{name: "a reset of zero", limit: "8000", remaining: "12", reset: "0s"},
		// The limit may be missing — "12 tokens left" is still a reading — but a
		// response that states no remainder states nothing to plan against.
		{name: "no remainder", limit: "8000", reset: "7.66s"},
		{name: "a fractional remainder", limit: "8000", remaining: "12.5", reset: "7.66s"},
		{name: "a negative remainder", limit: "8000", remaining: "-1", reset: "7.66s"},
		{name: "nothing at all", limit: "", remaining: "", reset: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &groqBudgetServer{limit: tc.limit, remaining: tc.remaining, reset: tc.reset, reply: "hi"}
			srv := g.server(t)
			p := groqBudgetProvider(t, srv)

			if err := runTurn(t, p, context.Background(), ChatRequest{
				Model: "openai/gpt-oss-20b", Messages: []Message{{Role: RoleUser, Content: "hi"}},
			}); err != nil {
				t.Fatalf("Chat: %v", err)
			}

			got, ok := p.budgetFor("openai/gpt-oss-20b")
			if ok != tc.ok {
				t.Fatalf("a budget was read = %v, want %v (%+v)", ok, tc.ok, got)
			}
			if !tc.ok {
				return
			}
			if got.Limit != tc.want.Limit || got.Remaining != tc.want.Remaining || got.Reset != tc.want.Reset {
				t.Errorf("budget = %+v, want %+v", got, tc.want)
			}
			if got.At.IsZero() {
				t.Error("the statement has no time, so the window can never be seen to have rolled over")
			}
		})
	}
}

// TestAStatementIsAnchoredToItsOwnResponse pins the difference between a budget and a
// counter: nothing refills it, so a window that has passed is a window that is whole
// again — and the remainder of a statement that is still running is the remainder.
func TestAStatementIsAnchoredToItsOwnResponse(t *testing.T) {
	now := time.Now()
	spent := modelBudget{Limit: 8000, Remaining: 0, Reset: 500 * time.Millisecond, At: now}

	if got := spent.remainingAt(now); got != 0 {
		t.Errorf("remaining in the window = %d, want the 0 the response stated", got)
	}
	if got := spent.remainingAt(now.Add(600 * time.Millisecond)); got != 8000 {
		t.Errorf("remaining after the reset = %d, want the whole 8000 back", got)
	}
	if got := spent.resetIn(now); got != 500*time.Millisecond {
		t.Errorf("resetIn = %s, want the 500ms the response stated", got)
	}
	if got := spent.resetIn(now.Add(time.Second)); got != 0 {
		t.Errorf("resetIn after the window = %s, want 0", got)
	}
}

// TestTheNewestStatementWins is the rule that makes two writers of one number safe
// here, and it is the opposite of the ceiling's. The ceiling is one fixed limit that a
// refusal and a header describe, so the tighter statement is kept; this is a reading of
// a moving quantity, and keeping an older, smaller remainder would make the app wait
// for a minute that has already been refilled.
func TestTheNewestStatementWins(t *testing.T) {
	p := newGroqProvider("groq", "Groq", "", "k")
	const model = "openai/gpt-oss-20b"

	stale := http.Header{}
	stale.Set(rateLimitTokensHeader, "8000")
	stale.Set(rateLimitRemainingTokensHeader, "0")
	stale.Set(rateLimitResetTokensHeader, "60s")
	p.noteRequestBudget(model, stale)

	fresh := http.Header{}
	fresh.Set(rateLimitTokensHeader, "8000")
	fresh.Set(rateLimitRemainingTokensHeader, "8000")
	fresh.Set(rateLimitResetTokensHeader, "60s")
	p.noteRequestBudget(model, fresh)

	got, ok := p.budgetFor(model)
	if !ok {
		t.Fatal("the budget was lost")
	}
	if got.Remaining != 8000 {
		t.Errorf("remaining = %d, want the newer 8000 — the older remainder was kept", got.Remaining)
	}
}

// TestASpentMinuteIsWaitedOutBeforeTheRequestGoesOut is the whole point of pacing: the
// request the account cannot pay for is not sent to be refused, it is held back for
// exactly as long as the backend said the allowance lasts — and the wait is reported,
// so a turn that waits is not a turn that looks hung.
func TestASpentMinuteIsWaitedOutBeforeTheRequestGoesOut(t *testing.T) {
	// The remainder is long enough that the bounds below are about the wait rather
	// than about how long the test itself took to get back to the provider: the wait
	// is computed from the moment the *statement* arrived, so a slow test would shrink
	// it and a tight lower bound would flake.
	g := &groqBudgetServer{limit: "8000", remaining: "0", reset: "1200ms", reply: "hi"}
	srv := g.server(t)
	p := groqBudgetProvider(t, srv)

	// The first request cannot be paced: no response has stated the minute yet, so
	// there is nothing to plan against and the app invents nothing.
	watcher := &recordingWatcher{}
	ctx := WithRetryWatcher(context.Background(), watcher)
	if err := runTurn(t, p, ctx, ChatRequest{
		Model: "openai/gpt-oss-20b", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if waits, _, _ := watcher.seen(); len(waits) != 0 {
		t.Errorf("the first request was paced with %v, want none: nothing had stated a budget", waits)
	}

	// The second waits out the remainder of the minute it just spent — and it does so
	// with the answer's room asked for, which is the case a large `max_tokens` setting
	// would otherwise switch off: the room is a cap the user set, not a promise, so it
	// must not read as a request too big to send.
	started := time.Now()
	if err := runTurn(t, p, ctx, ChatRequest{
		Model:     "openai/gpt-oss-20b",
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
		MaxTokens: 4000,
	}); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed < 700*time.Millisecond {
		t.Errorf("the second request went out after %v — it was sent against a spent minute", elapsed)
	}
	waits, errs, waited := watcher.seen()
	if len(waits) != 1 {
		t.Fatalf("the watcher was told about %d waits, want the one the request took: %v", len(waits), waits)
	}
	if waits[0] < 600*time.Millisecond || waits[0] > 1200*time.Millisecond {
		t.Errorf("the wait reported was %s, want the ~1.2s the reset header named", waits[0])
	}
	// No failure behind it: nothing was refused, so the phase this becomes is the
	// pacing one rather than a retry's.
	if errs[0] != nil {
		t.Errorf("the wait carried the error %v, want none: no request was refused", errs[0])
	}
	if waited != 1 {
		t.Errorf("the wait was retracted %d times, want 1: a phase left standing says the turn is hung", waited)
	}
	if got := g.requestCount(); got != 2 {
		t.Errorf("requests = %d, want 2 — the held-back request was never sent", got)
	}
}

// TestARequestTheAllowanceCoversIsNotWaitedFor: pacing is not a delay added to every
// turn. A minute with room in it is spent as before.
func TestARequestTheAllowanceCoversIsNotWaitedFor(t *testing.T) {
	g := &groqBudgetServer{limit: "18000", remaining: "17997", reset: "7.66s", reply: "hi"}
	srv := g.server(t)
	p := groqBudgetProvider(t, srv)

	watcher := &recordingWatcher{}
	ctx := WithRetryWatcher(context.Background(), watcher)
	for i := 0; i < 2; i++ {
		started := time.Now()
		if err := runTurn(t, p, ctx, ChatRequest{
			Model: "openai/gpt-oss-20b", Messages: []Message{{Role: RoleUser, Content: "hi"}},
		}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		// A wait here would have been the 7.66s the header names, so a loose bound still
		// tells the two apart — and the watcher's silence below is the real assertion.
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Errorf("request %d took %v, want no wait at all", i, elapsed)
		}
	}
	if waits, _, _ := watcher.seen(); len(waits) != 0 {
		t.Errorf("a request the allowance covers was paced with %v", waits)
	}
}

// TestAnUnwatchedRequestIsNeverPaced is the boundary that keeps pacing honest. A wait
// is only taken where it can be shown: a listing, a health probe or an availability
// check carries no watcher, has no phase to say anything with, and is bounded by its
// own timeout — parking one of those silently for a minute would look like a hung app
// and then fail anyway. Those requests go out as before, with the vendor's own 429 and
// its hint as the answer.
func TestAnUnwatchedRequestIsNeverPaced(t *testing.T) {
	g := &groqBudgetServer{limit: "8000", remaining: "0", reset: "60s", reply: "hi"}
	srv := g.server(t)
	p := groqBudgetProvider(t, srv)

	for i := 0; i < 2; i++ {
		started := time.Now()
		if err := runTurn(t, p, context.Background(), ChatRequest{
			Model: "openai/gpt-oss-20b", Messages: []Message{{Role: RoleUser, Content: "hi"}},
		}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Errorf("an unwatched request waited %v, want none", elapsed)
		}
	}
	if got := g.requestCount(); got != 2 {
		t.Errorf("requests = %d, want 2: an unwatched request is sent, not held", got)
	}
}

// TestARequestLargerThanTheMinuteIsNotWaitedFor: a request bigger than a whole minute's
// allowance can never be sent, so waiting for the window would delay the fold that
// exists for it by the very minute it is trying to fit inside.
func TestARequestLargerThanTheMinuteIsNotWaitedFor(t *testing.T) {
	g := &groqBudgetServer{limit: "1000", remaining: "0", reset: "60s", reply: "hi"}
	srv := g.server(t)
	p := groqBudgetProvider(t, srv)

	long := strings.Repeat("wort ", 2000)
	req := ChatRequest{Model: "openai/gpt-oss-20b", Messages: []Message{{Role: RoleUser, Content: long}}}
	if want := EstimateMessagesTokens(req.Messages); want < 1000 {
		t.Fatalf("the fixture request is only %d tokens against a 1000-token allowance: it is the wrong shape", want)
	}

	watcher := &recordingWatcher{}
	ctx := WithRetryWatcher(context.Background(), watcher)
	// The first request teaches the budget; the second is the oversized one, which must
	// not wait for a minute it could never fit in.
	if err := runTurn(t, p, ctx, req); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	started := time.Now()
	if err := runTurn(t, p, ctx, req); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("the oversized request waited %v, want none: no wait can make it fit", elapsed)
	}
	if waits, _, _ := watcher.seen(); len(waits) != 0 {
		t.Errorf("the oversized request was paced with %v", waits)
	}
}

// TestAResetBeyondTheWindowIsNotWaitedFor: these allowances are defined per minute, so
// a reset the vendor does not send is not a minute this app is willing to park a turn
// for. The request goes out and the vendor's own refusal, with its hint, is the answer.
func TestAResetBeyondTheWindowIsNotWaitedFor(t *testing.T) {
	g := &groqBudgetServer{limit: "8000", remaining: "0", reset: "5m", reply: "hi"}
	srv := g.server(t)
	p := groqBudgetProvider(t, srv)

	watcher := &recordingWatcher{}
	ctx := WithRetryWatcher(context.Background(), watcher)
	if err := runTurn(t, p, ctx, ChatRequest{
		Model: "openai/gpt-oss-20b", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	started := time.Now()
	if err := runTurn(t, p, ctx, ChatRequest{
		Model: "openai/gpt-oss-20b", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("a reset of five minutes was waited out (%v)", elapsed)
	}
	if waits, _, _ := watcher.seen(); len(waits) != 0 {
		t.Errorf("the request was paced with %v, want none beyond one window", waits)
	}
}

// TestAStopEndsAPacedWait: a Stop during the wait ends the turn, exactly as it does
// during a retry's wait — the request is never sent, and the phase is retracted rather
// than left claiming a wait that is over.
func TestAStopEndsAPacedWait(t *testing.T) {
	g := &groqBudgetServer{limit: "8000", remaining: "0", reset: "30s", reply: "hi"}
	srv := g.server(t)
	p := groqBudgetProvider(t, srv)

	// The first request establishes the spent minute.
	if err := runTurn(t, p, context.Background(), ChatRequest{
		Model: "openai/gpt-oss-20b", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("first Chat: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watcher := &recordingWatcher{onWait: cancel}
	started := time.Now()
	err := runTurn(t, p, WithRetryWatcher(ctx, watcher), ChatRequest{
		Model: "openai/gpt-oss-20b", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("the cancelled wait took %v — it was waited out rather than cut short", elapsed)
	}
	// The failed request phase is reported as the cancellation it is; what matters here
	// is that it never reached the wire.
	if err == nil {
		t.Error("Chat reported no error after the wait was cancelled")
	} else if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Chat error = %v, want the cancellation", err)
	}
	waits, _, waited := watcher.seen()
	if len(waits) != 1 {
		t.Fatalf("the watcher was told about %d waits, want 1", len(waits))
	}
	if waited != 1 {
		t.Errorf("the wait was retracted %d times, want 1", waited)
	}
	if got := g.requestCount(); got != 1 {
		t.Errorf("requests = %d, want 1: the held-back request was sent anyway", got)
	}
}

// groqRateServer is a Groq-shaped account that counts *requests*: it allows `limit` of
// them inside a trailing minute and refuses the rest with the envelope the vendor sends
// — which is the only place this API states a requests-per-minute limit, since the
// header family beside it describes the day.
//
// It sends none of the token headers, so the two halves of the pacing cannot quietly
// cover for each other in a test: what is measured here is the request rate alone.
type groqRateServer struct {
	limit int
	// wait is the pause the refusal's own sentence names ("Please try again in 0.2s"). A
	// short one keeps a test out of the shared client's retry path: the sentence is what
	// `retryHint` falls back to when the refusal carries no header, and it is obeyed.
	wait  string
	reply string

	mu    sync.Mutex
	at    []time.Time
	calls int
}

// rateRefusalBody is the vendor's requests-per-minute refusal, verbatim in the shape it
// was measured in: HTTP 429, `type: "requests"`, and the limit, the spend and the wait
// in one sentence. It takes its numbers rather than reading the server's fields so the
// handler cannot build it from state another request is mutating.
func rateRefusalBody(limit int, wait string) string {
	return fmt.Sprintf(`{"error":{"message":"Rate limit reached for model `+"`openai/gpt-oss-20b`"+
		` in organization `+"`org_01kz7a9rsze9da1b4q1yv7w8bx`"+` on requests per minute (RPM): `+
		`Limit %d, Used %d, Requested 1. Please try again in %s. Visit `+
		`https://console.groq.com/docs/rate-limits for more information.","type":"requests",`+
		`"code":"rate_limit_exceeded"}}`, limit, limit, wait)
}

func (g *groqRateServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		now := time.Now()
		g.mu.Lock()
		g.calls++
		spent := 0
		for _, at := range g.at {
			if now.Sub(at) < time.Minute {
				spent++
			}
		}
		g.at = append(g.at, now)
		limit, wait, reply := g.limit, g.wait, g.reply
		g.mu.Unlock()

		if spent >= limit {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, rateRefusalBody(limit, wait))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", reply)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func (g *groqRateServer) requestCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// rateOf reads the request-rate state the way no production caller does: pacing only
// ever asks whether the window has room, while a test has to see the limit it was
// taught and the count it keeps, which is the whole evidence this half rests on.
func rateOf(t *testing.T, p *openAICompatProvider, model string) requestRate {
	t.Helper()
	p.rateMu.Lock()
	defer p.rateMu.Unlock()
	r := p.requestRates[model]
	if r == nil {
		return requestRate{}
	}
	return requestRate{Limit: r.Limit, At: append([]time.Time(nil), r.At...)}
}

// TestTheRequestWindowIsATrailingMinute pins the arithmetic of the only allowance the
// app has to measure itself, and the two directions it can be wrong in: a window with
// room waits for nothing, and a full one waits exactly as long as its oldest request
// has left to run — which is when one slot frees, not when a clock-aligned minute ends.
func TestTheRequestWindowIsATrailingMinute(t *testing.T) {
	now := time.Now()
	full := &requestRate{Limit: 3, At: []time.Time{
		now.Add(-40 * time.Second), now.Add(-20 * time.Second), now.Add(-5 * time.Second),
	}}
	if got := full.roomIn(now); got != 20*time.Second {
		t.Errorf("a full window waits %s, want the 20s its oldest request has left", got)
	}

	// One request has fallen out of the window, so there is room for one more.
	aged := &requestRate{Limit: 3, At: []time.Time{
		now.Add(-61 * time.Second), now.Add(-20 * time.Second), now.Add(-5 * time.Second),
	}}
	if got := aged.roomIn(now); got != 0 {
		t.Errorf("a window one request short of its limit waits %s, want none", got)
	}
	if got := aged.spentIn(now); got != 2 {
		t.Errorf("spent in the window = %d, want the 2 that are still inside it", got)
	}
	if len(aged.At) != 2 {
		t.Errorf("the window still holds %d requests, want it trimmed to 2", len(aged.At))
	}

	// Two under the limit is still under it: the vendor refuses the request that would
	// *reach* the limit, so a count below it is room.
	under := &requestRate{Limit: 3, At: []time.Time{now.Add(-30 * time.Second), now.Add(-10 * time.Second)}}
	if got := under.roomIn(now); got != 0 {
		t.Errorf("two requests of a three-request minute waits %s, want none", got)
	}

	// And a rate nobody stated is not a rate to wait for.
	unknown := &requestRate{At: []time.Time{now.Add(-time.Second)}}
	if got := unknown.roomIn(now); got != 0 {
		t.Errorf("an unstated limit waits %s, want none", got)
	}
}

// TestARequestRateRefusalTeachesTheLimitAndPacesTheNextRequest is this half's whole
// point: no header states a requests-per-minute limit, so a refusal that names one is
// the only place it can be learned — and once it is known, the next request waits for
// the window instead of being refused by it.
func TestARequestRateRefusalTeachesTheLimitAndPacesTheNextRequest(t *testing.T) {
	// One request per minute, so the fixture needs no thirty round trips to be full.
	g := &groqRateServer{limit: 1, wait: "0.2s", reply: "hi"}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	p := groqBudgetProvider(t, srv)

	const model = "openai/gpt-oss-20b"
	watcher := &recordingWatcher{}
	ctx := WithRetryWatcher(context.Background(), watcher)

	// The first request goes out, and that is the point of the first half: the fixture's
	// minute is full after it, but nothing has *stated* a rate yet, and the app invents
	// none.
	if err := runTurn(t, p, ctx, ChatRequest{
		Model: model, Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if got := rateOf(t, p, model); got.Limit != 0 {
		t.Errorf("a rate of %d was known before anything refused one", got.Limit)
	}
	if waits, _, _ := watcher.seen(); len(waits) != 0 {
		t.Errorf("the first request was paced with %v, want none: nothing had stated a rate", waits)
	}

	// The second request is the one the minute refuses, and the refusal is where the
	// rate comes from. The shared client obeys the 0.2s the sentence names and tries
	// twice more, so this Chat reaches the vendor three times and ends as the failure it
	// is.
	if err := runTurn(t, p, ctx, ChatRequest{
		Model: model, Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("the second request answered, so nothing taught the request rate")
	}
	learned := rateOf(t, p, model)
	if learned.Limit != 1 {
		t.Errorf("request rate = %d, want the 1 the refusal's own sentence named", learned.Limit)
	}
	// Every response counts, the refusals as much as an answer: what a rate is measured
	// against is how many requests reached the vendor.
	if got := g.requestCount(); got != 4 {
		t.Fatalf("the fixture saw %d requests, want the answer, the refusal and its two retries", got)
	}
	if len(learned.At) != 4 {
		t.Errorf("the window holds %d requests, want the 4 that were answered", len(learned.At))
	}

	// The next request is held back for the rest of that minute — reported, with no
	// failure behind it, because it has not been sent yet. The wait is cut short by the
	// context so the test does not sit through it; what matters is the number shown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paced := &recordingWatcher{onWait: cancel}
	started := time.Now()
	_ = runTurn(t, p, WithRetryWatcher(ctx, paced), ChatRequest{
		Model: model, Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("the paced wait took %v — it was waited out rather than cut short", elapsed)
	}
	waits, errs, waited := paced.seen()
	if len(waits) != 1 {
		t.Fatalf("the watcher was told about %d waits, want the one this request was held for", len(waits))
	}
	if waits[0] < 55*time.Second || waits[0] > time.Minute {
		t.Errorf("the wait shown was %s, want the rest of the trailing minute", waits[0])
	}
	if errs[0] != nil {
		t.Errorf("the wait carried the error %v, want none: this request was not sent", errs[0])
	}
	if waited != 1 {
		t.Errorf("the wait was retracted %d times, want 1", waited)
	}
	if got := g.requestCount(); got != 4 {
		t.Errorf("the fixture saw %d requests, want the held-back one never sent", got)
	}
}

// TestATokenThrottleDoesNotTeachARequestRate: the two limits share an envelope and a
// code, so the reading has to tell them apart by what each one says. A token throttle
// teaches nothing about requests, or a chat with tokens to spare would be parked on a
// request count nothing had bounded.
func TestATokenThrottleDoesNotTeachARequestRate(t *testing.T) {
	// The sentences are the measured and documented token shapes. Their durations are
	// shortened to a fifth of a second, because the shared client *obeys* the wait a
	// refusal names and the classifier reads the phrase and the limit, not the seconds —
	// a verbatim 9.9s would cost this test forty seconds of sleeping to assert the same
	// fact.
	bodies := map[string]string{
		"input tokens": `{"error":{"message":"Rate limit reached for model ` + "`openai/gpt-oss-20b`" +
			` in organization ` + "`org_1`" + ` on input tokens per minute (ITPM): Limit 7000, Used 1911, ` +
			`Requested 6248. Please try again in 0.2s.","type":"tokens","code":"rate_limit_exceeded"}}`,
		"the documented token shape": `{"error":{"message":"Rate limit reached for model ` +
			"`openai/gpt-oss-20b`" + ` in organization ` + "`org_1`" + ` on tokens per minute (TPM): ` +
			`Limit 8000, Used 8000, Requested 12. Please try again in 0.2s.","type":"tokens",` +
			`"code":"rate_limit_exceeded"}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, body)
			}))
			defer srv.Close()
			p := groqBudgetProvider(t, srv)

			_, err := p.Chat(context.Background(), ChatRequest{
				Model: "openai/gpt-oss-20b", Messages: []Message{{Role: RoleUser, Content: "hi"}},
			})
			if err == nil {
				t.Fatal("the refusal was swallowed")
			}
			if got := rateOf(t, p, "openai/gpt-oss-20b"); got.Limit != 0 {
				t.Errorf("a token throttle taught a request rate of %d", got.Limit)
			}
		})
	}
}

// TestARequestRateOnlyEverTightens is the rule that keeps two statements about one
// limit safe, and it is the ceiling's: fitting the smaller of two numbers fits both,
// while the looser one costs the refusal it was learned from again. What it must not do
// is touch the count, which is a measurement rather than a statement.
func TestARequestRateOnlyEverTightens(t *testing.T) {
	p := newGroqProvider("groq", "Groq", "", "k")
	const model = "openai/gpt-oss-20b"
	now := time.Now()

	p.noteRequestRate(model, 30)
	p.noteRequestSpent(model, now)
	p.noteRequestRate(model, 10)
	if got := rateOf(t, p, model); got.Limit != 10 {
		t.Errorf("rate after a tighter statement = %d, want 10", got.Limit)
	}
	p.noteRequestRate(model, 50)
	got := rateOf(t, p, model)
	if got.Limit != 10 {
		t.Errorf("rate after a looser statement = %d, want the 10 already known", got.Limit)
	}
	if len(got.At) != 1 {
		t.Errorf("the window holds %d requests, want the one that was counted", len(got.At))
	}
	// And a statement with no number states nothing.
	p.noteRequestRate(model, 0)
	if got := rateOf(t, p, model); got.Limit != 10 {
		t.Errorf("rate after an empty statement = %d, want 10", got.Limit)
	}
}

// TestTheRequestRateCountIsPerModelAndPerResponse pins what the count is: one per
// response the vendor gave, under the model that asked. It is the app's own spend and
// nothing else — a model nobody asked has no count, and the number is never carried
// between models whose allowances are separate.
func TestTheRequestRateCountIsPerModelAndPerResponse(t *testing.T) {
	g := &groqRateServer{limit: 100, wait: "0.2s", reply: "hi"}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	p := groqBudgetProvider(t, srv)

	for i := 0; i < 2; i++ {
		if err := runTurn(t, p, context.Background(), ChatRequest{
			Model: "openai/gpt-oss-20b", Messages: []Message{{Role: RoleUser, Content: "hi"}},
		}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
	}
	if err := runTurn(t, p, context.Background(), ChatRequest{
		Model: "qwen/qwen3.8-27b", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if got := len(rateOf(t, p, "openai/gpt-oss-20b").At); got != 2 {
		t.Errorf("the model that was asked twice counts %d, want 2", got)
	}
	if got := len(rateOf(t, p, "qwen/qwen3.8-27b").At); got != 1 {
		t.Errorf("the model that was asked once counts %d, want 1", got)
	}
	if got := rateOf(t, p, "llama-3.3-70b-versatile"); len(got.At) != 0 || got.Limit != 0 {
		t.Errorf("a model nobody asked has %+v, want nothing", got)
	}
}

// TestAnUnwatchedRequestIsNotPacedByTheRequestRate: the boundary is about the *wait*,
// not about the limit. A request with no watcher carries no phase to explain a pause
// with, so it goes out — and a count that is already over the limit is exactly the case
// where the vendor's own 429 and its hint do the work instead.
func TestAnUnwatchedRequestIsNotPacedByTheRequestRate(t *testing.T) {
	g := &groqRateServer{limit: 100, wait: "0.2s", reply: "hi"}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	p := groqBudgetProvider(t, srv)

	const model = "openai/gpt-oss-20b"
	p.noteRequestRate(model, 1)
	p.noteRequestSpent(model, time.Now())

	started := time.Now()
	if err := runTurn(t, p, context.Background(), ChatRequest{
		Model: model, Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("an unwatched request waited %v, want none", elapsed)
	}
	if got := g.requestCount(); got != 1 {
		t.Errorf("the fixture saw %d requests, want the one that was sent", got)
	}
}

// TestPacingIsGroqOnly: the numbers are a measurement of one vendor. Another
// OpenAI-compatible service's rate-limit headers say nothing about the allowance a
// request is planned against here, and waiting on them would delay turns for a rule
// nothing measured.
func TestPacingIsGroqOnly(t *testing.T) {
	g := &groqBudgetServer{limit: "8000", remaining: "0", reset: "30s", reply: "hi"}
	srv := g.server(t)
	p := &openAICompatProvider{
		id:        "custom",
		name:      "Custom",
		baseURL:   srv.URL,
		chatPaths: []string{srv.URL + "/chat/completions"},
	}

	watcher := &recordingWatcher{}
	ctx := WithRetryWatcher(context.Background(), watcher)
	for i := 0; i < 2; i++ {
		started := time.Now()
		if err := runTurn(t, p, ctx, ChatRequest{
			Model: "some-model", Messages: []Message{{Role: RoleUser, Content: "hi"}},
		}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Errorf("a request to another backend waited %v", elapsed)
		}
	}
	if waits, _, _ := watcher.seen(); len(waits) != 0 {
		t.Errorf("another backend's headers paced a request with %v", waits)
	}
	// And nothing was read out of them either: the budget belongs to the vendor whose
	// numbers were measured, so a lookalike header teaches nothing here.
	if _, ok := p.budgetFor("some-model"); ok {
		t.Error("a budget was learned from a backend whose headers mean nothing to this app")
	}
}

// TestNoRateAndAFittingSeriesAreNotSpaced pins the two cases in which a plan must not exist.
// An unstated rate is not a rate to pace against, and a series that fits in the minute's room
// is not delayed at all: the window rolls continuously, so taking a slot now cannot cost a
// later one, and the earliest request is therefore the best one.
func TestNoRateAndAFittingSeriesAreNotSpaced(t *testing.T) {
	if got := (Allowance{}).Spread(10); got != nil {
		t.Errorf("an unstated rate planned %v", got)
	}
	if got := (Allowance{Limit: 4, Spent: 0}).Spread(4); got != nil {
		t.Errorf("a series that fits exactly was planned: %v", got)
	}
	if got := (Allowance{Limit: 4, Spent: 2}).Spread(2); got != nil {
		t.Errorf("a series inside the room was planned: %v", got)
	}
	// One past the room, and there is a plan.
	if got := (Allowance{Limit: 4, Spent: 2}).Spread(3); len(got) != 3 {
		t.Errorf("a series past the room was not planned: %v", got)
	}
	// And nothing is planned for nothing.
	if got := (Allowance{Limit: 4}).Spread(0); got != nil {
		t.Errorf("an empty series planned %v", got)
	}
}

// TestASeriesIsSpacedAtTheAllowanceRate is the arithmetic the whole feature is: thirty
// requests a minute is one every two seconds, so a series that does not fit goes out one per
// slot — starting immediately, while the minute can still pay for the first one.
func TestASeriesIsSpacedAtTheAllowanceRate(t *testing.T) {
	const (
		limit    = 30
		requests = 40
	)
	waits := Allowance{Limit: limit}.Spread(requests)
	if len(waits) != requests {
		t.Fatalf("the plan covers %d requests, want %d", len(waits), requests)
	}
	period := time.Minute / limit
	for i, wait := range waits {
		want := time.Duration(i) * period
		if wait != want {
			t.Errorf("request %d goes at %s, want %s — one slot per %s", i, wait, want, period)
		}
	}
}

// TestASpentMinuteIsWaitedOutAndThenSpaced: the window has nothing to give until its oldest
// request leaves it, so that wait comes first and the rate applies from there. This is the
// state a tool-heavy turn most often starts in — the previous round spent the last slot.
func TestASpentMinuteIsWaitedOutAndThenSpaced(t *testing.T) {
	a := Allowance{
		Limit: 3, Spent: 3,
		Free: []time.Duration{10 * time.Second, 30 * time.Second, 50 * time.Second},
	}
	if a.Room() != 0 {
		t.Fatalf("room = %d, want a full minute", a.Room())
	}
	waits := a.Spread(4)
	want := []time.Duration{10 * time.Second, 30 * time.Second, 50 * time.Second, 70 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("plan = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("request %d goes at %s, want %s", i, waits[i], want[i])
		}
	}
}

// TestASlotIsNeverTakenBeforeTheWindowHasRoom is why the plan is a simulation rather than a
// division. Twenty-nine of a thirty-request minute are still inside it and all of them leave
// at once ten seconds from now: spacing the second request one period (two seconds) after the
// first would put it in a window that holds thirty requests already — the request the vendor
// refuses. The plan waits for the slot instead.
func TestASlotIsNeverTakenBeforeTheWindowHasRoom(t *testing.T) {
	const limit = 30
	free := make([]time.Duration, limit-1)
	for i := range free {
		free[i] = 10 * time.Second
	}
	waits := Allowance{Limit: limit, Spent: len(free), Free: free}.Spread(3)
	if len(waits) != 3 {
		t.Fatalf("plan = %v, want three requests", waits)
	}
	if waits[0] != 0 {
		t.Errorf("the first request waits %s, want none: the minute has one slot left", waits[0])
	}
	if waits[1] != 10*time.Second {
		t.Errorf("the second request goes at %s, want the 10s its slot needs", waits[1])
	}
	// From there the rate applies again: the window is nearly empty and the period is two
	// seconds.
	if waits[2] != 12*time.Second {
		t.Errorf("the third request goes at %s, want 12s", waits[2])
	}
}

// TestTheAllowanceIsWhatTheAppCountedForTheModel: the reading is the same state the
// per-request guard spends — the rate a refusal taught, and this app's own count with the
// moment each of those requests frees its slot — and it is empty for anything nobody has
// stated.
func TestTheAllowanceIsWhatTheAppCountedForTheModel(t *testing.T) {
	p := newGroqProvider("groq", "Groq", "", "k")
	const model = "openai/gpt-oss-20b"

	if got := p.RequestAllowance(model); got.Known() || got.Spent != 0 || len(got.Free) != 0 {
		t.Errorf("a model nobody asked reads %+v, want nothing", got)
	}

	// A rate with nothing spent: the number is known and the minute is whole, which is not
	// the same reading as a rate nobody stated.
	p.noteRequestRate(model, 3)
	got := p.RequestAllowance(model)
	if got.Limit != 3 || got.Spent != 0 || !got.Known() || got.Room() != 3 {
		t.Errorf("allowance = %+v, want the 3 the refusal named with nothing spent", got)
	}

	// Two requests answered inside the minute: the count, and the slots they hold.
	p.noteRequestSpent(model, time.Now())
	p.noteRequestSpent(model, time.Now())
	got = p.RequestAllowance(model)
	if got.Spent != 2 || got.Room() != 1 {
		t.Errorf("allowance = %+v, want 2 of 3 spent", got)
	}
	if len(got.Free) != 2 {
		t.Fatalf("free = %v, want a slot for each counted request", got.Free)
	}
	for _, free := range got.Free {
		if free <= 0 || free > time.Minute {
			t.Errorf("a slot frees in %s, want the remainder of the trailing minute", free)
		}
	}

	// A full minute is no room and a wait, which is the first thing a plan does with it.
	p.noteRequestSpent(model, time.Now())
	full := p.RequestAllowance(model)
	if full.Room() != 0 || len(full.Free) != 3 {
		t.Fatalf("a full minute reads %+v, want no room and three slots freeing", full)
	}
	if waits := full.Spread(1); len(waits) != 1 || waits[0] <= 0 {
		t.Errorf("a one-request plan against a full minute = %v, want the wait for its slot", waits)
	}
}

// TestTheAllowanceIsNotReadFromAnotherVendor is the boundary the pacing already has: these
// numbers are a measurement of Groq, so another OpenAI-compatible service's refusal teaches
// this app nothing it may plan a turn against.
func TestTheAllowanceIsNotReadFromAnotherVendor(t *testing.T) {
	p := &openAICompatProvider{
		id: "custom", name: "Custom", baseURL: "http://127.0.0.1:1",
		chatPaths: []string{"http://127.0.0.1:1/chat/completions"},
	}
	// Even with the state written, which is what a refusal would do if the classification were
	// ever reached for another flavour.
	p.noteRequestRate("m", 5)
	p.noteRequestSpent("m", time.Now())
	if got := p.RequestAllowance("m"); got.Known() || got.Spent != 0 || len(got.Free) != 0 {
		t.Errorf("another vendor's numbers became a plan: %+v", got)
	}
}
