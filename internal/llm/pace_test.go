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
