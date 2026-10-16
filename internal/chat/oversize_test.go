package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/llm"
	"kichat/internal/store"
)

// groqChatListing is the one model these tests drive, in the shape the live listing
// answers with: a 131k window and no statement at all about the per-minute token
// allowance that actually limits a single request.
const groqChatListing = `{"object":"list","data":[{"id":"openai/gpt-oss-20b","object":"model","owned_by":"OpenAI",` +
	`"active":true,"context_window":131072,"input_modalities":["text"],"output_modalities":["text"],` +
	`"supported_features":["tools","reasoning"],"max_completion_tokens":65536}]}`

// groqOversizedBody is Groq's refusal for a request bigger than a whole minute of the
// account's token allowance, verbatim in the shape the docs and bug reports show. It
// matters that it says `rate_limit_exceeded`: the envelope is the same one the
// throttle uses, so a client that reads only the code retries a request that can
// never fit.
const groqOversizedBody = `{"error":{"message":"Request too large for model ` + "`openai/gpt-oss-20b`" +
	` in organization ` + "`org_01kz7a9rsze9da1b4q1yv7w8bx`" + ` service tier ` + "`on_demand`" + ` on tokens ` +
	`per minute (TPM): Limit 10000, Requested 15088, please reduce your message size and try again. ` +
	`Need more tokens? Upgrade to Dev Tier today at https://console.groq.com/settings/billing",` +
	`"type":"tokens","code":"rate_limit_exceeded"}}`

// groqServer is a Groq-shaped backend: it lists one model, refuses selected
// generations the way the vendor does, and answers the rest with a streamed reply.
type groqServer struct {
	refuseAt map[int]bool
	reply    string
	// limitTokens, when > 0, is the per-request budget every response states in the
	// header the vendor sends. It is the fixture's own statement of the limit, and
	// the only place a Groq-shaped server publishes one without refusing anything.
	limitTokens int
	// remainingTokens and resetTokens are the same statement read for the minute
	// rather than for one request: what is left of the allowance and how long that
	// lasts. Both are sent when resetTokens is set, because 0 remaining is the case
	// they exist for — a spent minute, which is what a turn is paced against
	// (internal/llm/pace.go).
	remainingTokens int
	resetTokens     string

	mu     sync.Mutex
	calls  []string // request bodies, in the order they arrived
	atCall int
}

func (g *groqServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openai/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, groqChatListing)
		case "/openai/v1/chat/completions":
			raw, _ := io.ReadAll(r.Body)
			g.mu.Lock()
			n := g.atCall
			g.atCall++
			g.calls = append(g.calls, string(raw))
			refuse := g.refuseAt[n]
			limit := g.limitTokens
			remaining := g.remainingTokens
			reset := g.resetTokens
			g.mu.Unlock()
			if limit > 0 {
				// The budget header the vendor sends on every response, answers and
				// refusals alike.
				w.Header().Set("X-Ratelimit-Limit-Tokens", fmt.Sprint(limit))
			}
			if reset != "" {
				w.Header().Set("X-Ratelimit-Remaining-Tokens", fmt.Sprint(remaining))
				w.Header().Set("X-Ratelimit-Reset-Tokens", reset)
			}
			if refuse {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				fmt.Fprint(w, groqOversizedBody)
				return
			}
			// The reported prompt_tokens is the request's own size, the way the vendor
			// reports it. A constant here would be a fixture that lies about what the
			// answer cost, and the measured value is what the next turn's plan — and
			// the ceiling check inside it — is computed from.
			prompt := len(raw) / 4
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", g.reply)
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],"+
				"\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":4,\"total_tokens\":%d}}\n\n", prompt, prompt+4)
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}
}

func (g *groqServer) bodies() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

// groqChat is the chat these tests drive: the one model the fixture listing offers,
// on the backend the fixture calls Groq.
func groqChat(t *testing.T, st *store.Store) store.Chat {
	t.Helper()
	c := store.Chat{
		ID: "chat-1", Title: "Groq", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "groq", Model: "openai/gpt-oss-20b", SystemPrompt: "Du bist ein Assistent.",
		Temperature: 0.7, TopP: 0.9, AutoCompact: false,
	}
	if err := st.CreateChat(c); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	return c
}

// groqEngine wires the engine to a real Groq provider — the registry's own `groq`
// kind, so the address rules, the key check and the refusal classification are the
// ones that ship — pointed at the test server.
func groqEngine(t *testing.T, st *store.Store, srv *httptest.Server) (*Engine, *eventLog, llm.Provider) {
	t.Helper()
	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = 3

	registry := llm.NewRegistry([]llm.BackendConfig{{
		ID: "groq", Name: "Groq", Kind: "groq", BaseURL: srv.URL + "/openai/v1",
		APIKey: "test-key", Enabled: true,
	}})
	provider, ok := registry.Provider("groq")
	if !ok {
		t.Fatal("the groq kind built no provider")
	}
	log := &eventLog{}
	return NewEngine(st, registry, nil, func() config.Settings { return settings }, log.emit), log, provider
}

// TestGroqsOversizedRefusalFoldsTheChatAndAnswers is the whole point of the change,
// driven through the provider that ships.
//
// Before it, this turn ended as a failure: the refusal is a 413 whose envelope says
// `rate_limit_exceeded`, so nothing about it looked like a request that was too big
// for the account, and the chat could not continue — every later turn sent a body
// that was at least as large and was refused the same way. What the turn does now is
// what the refusal itself asks for: the history is folded, and the same user message
// goes out again in a smaller request.
func TestGroqsOversizedRefusalFoldsTheChatAndAnswers(t *testing.T) {
	st := newTestStore(t)
	groqChat(t, st)
	seedLongHistory(t, st, 8)

	g := &groqServer{refuseAt: map[int]bool{0: true}, reply: "Antwort nach der Kompression"}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	engine, captured, provider := groqEngine(t, st, srv)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Und weiter?"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 10*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	// The turn ends with an answer, not with an error event.
	for _, ev := range captured.snapshot() {
		if ev.name == events.Error {
			t.Fatalf("the turn failed: %v", ev.payload["error"])
		}
	}
	messages, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	newest := messages[len(messages)-1]
	if newest.Role != "assistant" || newest.Status != store.StatusComplete {
		t.Fatalf("newest row = %s/%s, want a complete assistant reply", newest.Role, newest.Status)
	}
	if !strings.Contains(newest.Content, "Antwort nach der Kompression") {
		t.Errorf("reply = %q, want the model's answer", newest.Content)
	}
	// The refused request left no row behind: a red "failed" entry for a request that
	// was answered on the next attempt would read as a broken chat.
	for _, m := range messages {
		if m.Status == store.StatusFailed {
			t.Errorf("message %d was left marked failed: %q", m.Seq, m.Content)
		}
	}

	// The fold exists, and it is named for what happened rather than as a button the
	// user pressed.
	summaries := summariesOf(t, st)
	if len(summaries) != 1 {
		t.Fatalf("compactions = %d, want the one the refusal caused", len(summaries))
	}
	if summaries[0].Trigger != triggerTooLarge {
		t.Errorf("trigger = %q, want %q", summaries[0].Trigger, triggerTooLarge)
	}

	// The user is told why, with the limit the vendor named in it.
	sawWarning := false
	for _, ev := range captured.snapshot() {
		if ev.name != events.Warning {
			continue
		}
		text, _ := ev.payload["warning"].(string)
		if strings.Contains(text, "zu groß") && strings.Contains(text, "10000") {
			sawWarning = true
		}
	}
	if !sawWarning {
		t.Errorf("no warning named the limit: %v", captured.snapshot())
	}

	// Three requests: the refused one, the summarisation, and the answer. And the
	// answer's body is genuinely smaller than the one that was refused — the
	// measurement the recovery rests on.
	bodies := g.bodies()
	if len(bodies) != 3 {
		t.Fatalf("requests = %d, want a refusal, a summary and an answer: %v", len(bodies), bodies)
	}
	if len(bodies[2]) >= len(bodies[0]) {
		t.Errorf("the retry was not smaller: %d bytes against the refused %d", len(bodies[2]), len(bodies[0]))
	}
	if strings.Contains(bodies[2], "Frage 0:") {
		t.Error("the retry still carried a turn the memory replaced")
	}
	if !strings.Contains(bodies[2], "Und weiter?") {
		t.Error("the retry lost the message the user actually sent")
	}

	// And the ceiling the refusal stated is kept, per model, for every later turn.
	ceiler, ok := provider.(llm.RequestCeiling)
	if !ok {
		t.Fatal("the Groq provider publishes no ceiling, so the next turn cannot be planned against one")
	}
	if got := ceiler.RequestCeiling("openai/gpt-oss-20b"); got.Tokens != 10000 {
		t.Errorf("ceiling = %d, want the 10000 the refusal named", got.Tokens)
	}
}

// TestGroqWithoutAnythingToFoldSaysWhatToDo is the other end of the same turn: a
// refusal the app cannot answer by folding, because there is no history yet — the
// first message of a chat is already larger than the whole allowance.
//
// Nothing can be invented here, so the vendor's envelope is replaced with the one
// thing that helps: which limit was hit and what reduces it. It also pins that no
// second request is spent — the body would be identical, and so would the refusal.
func TestGroqWithoutAnythingToFoldSaysWhatToDo(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	too := &llm.RequestTooLargeError{
		Cause:     &llm.HTTPError{StatusCode: 413, Status: "413 Request Entity Too Large", Body: groqOversizedBody},
		Limit:     10000,
		Requested: 15088,
	}
	provider := &fakeProvider{
		model:    llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		refusals: []error{too},
	}
	engine, captured := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Ein einzelner, riesiger Prompt"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	var failure string
	for _, ev := range captured.snapshot() {
		if ev.name == events.Error {
			failure, _ = ev.payload["error"].(string)
		}
	}
	if failure == "" {
		t.Fatal("a turn that could not be rescued reported no error")
	}
	for _, want := range []string{"zu groß", "10000", "15088", "Kürze die Nachricht"} {
		if !strings.Contains(failure, want) {
			t.Errorf("error text %q does not mention %q", failure, want)
		}
	}
	// The vendor's envelope is what it replaced, and the app is not the place to read
	// an organization id and a billing URL.
	if strings.Contains(failure, "rate_limit_exceeded") {
		t.Errorf("error text %q still carries the vendored envelope", failure)
	}
	if n := provider.requestCount(); n != 1 {
		t.Errorf("backend calls = %d, want 1: a request with nothing to fold is not retried", n)
	}
}

// TestAFoldThatFailsIsNotReportedAsNothingToFold pins the three outcomes apart. A fold
// that wrote a memory, a fold with nothing older to fold, and a fold that broke are
// three different situations for the person reading the failure: the first asks for a
// smaller message, the second says the chat is already as small as it can be, and the
// third says the app tried and could not. Reporting the broken summariser as an empty
// history would also contradict the warning the same turn already emitted about it.
func TestAFoldThatFailsIsNotReportedAsNothingToFold(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	// Enough turns that the cut leaves some of them to fold: the size fold keeps the
	// newest turn verbatim, so a chat shorter than the keep-turns setting has nothing
	// older to summarise and never reaches the summariser at all.
	seedLongHistory(t, st, 10)

	too := &llm.RequestTooLargeError{
		Cause:     &llm.HTTPError{StatusCode: 413, Status: "413 Request Entity Too Large", Body: groqOversizedBody},
		Limit:     10000,
		Requested: 15088,
	}
	provider := &fakeProvider{
		model:    llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		refusals: []error{too, errors.New("das Zusammenfassungs-Backend antwortet nicht")},
	}
	engine, captured := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Und weiter?"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	var failure string
	var warnings []string
	for _, ev := range captured.snapshot() {
		switch ev.name {
		case events.Error:
			failure, _ = ev.payload["error"].(string)
		case events.Warning:
			if text, ok := ev.payload["warning"].(string); ok {
				warnings = append(warnings, text)
			}
		}
	}
	if !strings.Contains(failure, "ließ sich nicht komprimieren") {
		t.Errorf("error text %q does not say the fold failed", failure)
	}
	if strings.Contains(failure, "es gab keinen Verlauf") {
		t.Errorf("error text %q blames an empty history for a fold that broke", failure)
	}
	// The cause is named, and it is the summariser rather than the chat: that warning
	// and the sentence above describe one event, so they cannot disagree.
	sawCause := false
	for _, w := range warnings {
		if strings.Contains(w, "konnte nicht komprimiert werden") && strings.Contains(w, "antwortet nicht") {
			sawCause = true
		}
	}
	if !sawCause {
		t.Errorf("no warning named the failing summariser: %v", warnings)
	}
	// And nothing was retried: the body the backend refused is the body that would go
	// out again, which is what the missing fold is about.
	if got := provider.requestCount(); got != 2 {
		t.Errorf("backend calls = %d, want the refusal and the summariser: %v", got, warnings)
	}
	// No memory was written, so the transcript must not claim one.
	if got := len(summariesOf(t, st)); got != 0 {
		t.Errorf("compactions = %d, want none after a fold that failed", got)
	}
}

// seedLongHistory plants enough turns, of enough length, for a fold to have
// something to fold and for the estimate to reach a small ceiling. The default
// fixture is two short messages: fine for a threshold, useless for a limit that is
// two percent of the window.
func seedLongHistory(t *testing.T, st *store.Store, turns int) {
	t.Helper()
	filler := strings.Repeat("Der Verlauf einer Recherche ist lang und wird länger. ", 16)
	for i := 0; i < turns; i++ {
		seq := i*2 + 1
		if err := st.CreateMessage(store.Message{
			ID: fmt.Sprintf("u%d", i), ChatID: "chat-1", Seq: seq, Role: "user",
			Content: fmt.Sprintf("Frage %d: %s", i, filler), CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateMessage(store.Message{
			ID: fmt.Sprintf("a%d", i), ChatID: "chat-1", Seq: seq + 1, Role: "assistant",
			Content: fmt.Sprintf("Antwort %d: %s", i, filler), CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAKnownCeilingFoldsTheHistoryBeforeTheRequestGoesOut is the second half of the
// fix: paying the refusal once per model, not once per turn.
//
// The ceiling an account's real limit comes from is learned from one refusal, and
// from then on the request is planned against it. Here the window is 131k — so the
// automatic threshold, a ratio of that window, would never fire — while the ceiling
// is small, which is exactly Groq's shape. The fold happens before the request, so
// the backend is never asked a question it has already refused.
func TestAKnownCeilingFoldsTheHistoryBeforeTheRequestGoesOut(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedLongHistory(t, st, 4)

	provider := &fakeProvider{
		model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		ceiling: 2000,
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: "Zusammenfassung"}, {Type: llm.EventDone}},
			{{Type: llm.EventDelta, Text: "Antwort"}, {Type: llm.EventDone}},
		},
	}
	engine, captured := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Eine neue Frage"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	if got := len(summariesOf(t, st)); got != 1 {
		t.Fatalf("compactions = %d, want the fold the ceiling asked for before the request", got)
	}
	// The answer is the second call: the fold ran first, so the request that went out
	// no longer carries the turns the memory replaced — and nothing was refused.
	if n := provider.requestCount(); n != 2 {
		t.Fatalf("backend calls = %d, want the summary and the answer", n)
	}
	answer := provider.requestAt(1)
	var carried bool
	for _, m := range answer.Messages {
		if strings.Contains(m.Content, "Frage 0:") {
			carried = true
		}
	}
	if carried {
		t.Errorf("the answer carried a folded turn verbatim: %+v", answer.Messages)
	}
	var keepsNewest bool
	for _, m := range answer.Messages {
		if strings.Contains(m.Content, "Eine neue Frage") {
			keepsNewest = true
		}
	}
	if !keepsNewest {
		t.Error("the fold swallowed the message the user just sent")
	}
	for _, ev := range captured.snapshot() {
		if ev.name == events.Error {
			t.Errorf("a turn the ceiling could be planned against failed: %v", ev.payload["error"])
		}
	}
}

// TestBothBoundsCrossedCostsOneFold is the regression the merge is for. While the two
// bounds were two calls in the send pipeline, a chat that had crossed *both* folded twice
// in one turn: the threshold run folded to its keep-turns boundary, and the ceiling run
// then folded further for its target — two summaries, one request, twice the wait. The
// planner holds both bounds now, so the decision is one fold and the turn pays for one.
func TestBothBoundsCrossedCostsOneFold(t *testing.T) {
	st := newTestStore(t)
	chat := makeChat(t, st)
	// The user's bound is switched on for this chat, which is the state the app creates
	// (app.go) and the shipped settings enable.
	chat.AutoCompact = true
	if err := st.UpdateChat(chat); err != nil {
		t.Fatalf("enable auto-compaction: %v", err)
	}
	// More turns than KeepRecentTurns on purpose: with fewer, the threshold fold has
	// nothing to cut and only the ceiling would fold, so the count below could not tell the
	// two implementations apart.
	const turns = 8
	seedLongHistory(t, st, turns)

	provider := &fakeProvider{
		model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		ceiling: 2000,
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: "Zusammenfassung"}, {Type: llm.EventDone}},
			{{Type: llm.EventDelta, Text: "Antwort"}, {Type: llm.EventDone}},
		},
	}
	engine, _ := newTestEngine(t, st, provider)
	// Without both bounds live the count below would say nothing about the merge: the
	// per-chat flag is set above, and the setting is the shipped one.
	if !engine.Settings().Compact.Enabled {
		t.Fatal("this test needs the shipped automatic-compaction setting to be enabled")
	}
	if keep := effectiveKeepTurns(chat, engine.Settings()); keep >= turns {
		t.Fatalf("this test needs more turns (%d) than the %d it keeps verbatim", turns, keep)
	}

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Eine neue Frage"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	if got := len(summariesOf(t, st)); got != 1 {
		t.Errorf("compactions = %d, want 1: the two bounds are one decision, not two folds", got)
	}
	// The summary and the answer. A second fold would show up here as a third call — and
	// as a second memory in the transcript, which is what the count above reads.
	if n := provider.requestCount(); n != 2 {
		t.Errorf("backend calls = %d, want the one summary and the answer", n)
	}
	// One fold has to reach as far as the two did, or the saving would be bought with a
	// request that is still too large: the oldest turn is gone from the request that went
	// out, and the message the user just wrote is still in it.
	answer := provider.requestAt(1)
	var carried bool
	for _, m := range answer.Messages {
		if strings.Contains(m.Content, "Frage 0:") {
			carried = true
		}
	}
	if carried {
		t.Errorf("the answer still carried the oldest turn verbatim: %+v", answer.Messages)
	}
}

// TestGroqLearnsTheBudgetFromAnAnswerAndFoldsTheNextTurn is the path the budget header
// unlocks, driven through the provider that ships: the ceiling is learned from an
// ordinary answer instead of from a refusal, and the next turn is planned against it.
//
// The difference it makes is the first oversized request of a session. Before this, the
// ceiling existed only after a refusal had been paid, so a session whose history was
// already too large sent one request it could not win — and on a chat opened from
// storage that is every session. Here nothing is refused at all: the second turn's
// request is smaller than the first because the first answer said how small it had to
// be.
func TestGroqLearnsTheBudgetFromAnAnswerAndFoldsTheNextTurn(t *testing.T) {
	st := newTestStore(t)
	groqChat(t, st)
	seedLongHistory(t, st, 30)

	// 8000 tokens per request — the account's measured allowance — under the 131k window
	// of the fixture listing, and answers that state it.
	g := &groqServer{limitTokens: 8000, reply: "Antwort"}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	engine, captured, provider := groqEngine(t, st, srv)

	// The first turn goes out as it is: nothing has been answered yet, so nothing has
	// stated a budget, and the app does not invent one.
	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Frage eins"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 10*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the first turn did not finish")
	}
	if got := len(summariesOf(t, st)); got != 0 {
		t.Fatalf("compactions after the first turn = %d, want none: no budget is known yet", got)
	}

	// The answer stated the budget, so the app knows the real limit for this model.
	ceiler, ok := provider.(llm.RequestCeiling)
	if !ok {
		t.Fatal("the Groq provider publishes no ceiling")
	}
	if got := ceiler.RequestCeiling("openai/gpt-oss-20b"); got.Tokens != 8000 || got.Source != llm.CeilingFromHeader {
		t.Fatalf("ceiling after an ordinary answer = %+v, want the 8000 the response stated", got)
	}

	// The second turn is folded before it goes out — by the ceiling, with the chat's own
	// automatic threshold off, because that threshold is a ratio of the 131k window and
	// would never fire here.
	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Frage zwei"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 10*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the second turn did not finish")
	}
	for _, ev := range captured.snapshot() {
		if ev.name == events.Error {
			t.Fatalf("a turn the budget could be planned against failed: %v", ev.payload["error"])
		}
	}
	if got := len(summariesOf(t, st)); got != 1 {
		t.Fatalf("compactions = %d, want the fold the known budget asked for", got)
	}
	bodies := g.bodies()
	if len(bodies) != 3 {
		t.Fatalf("requests = %d, want two answers and the summary between them: %v", len(bodies), bodies)
	}
	if len(bodies[2]) >= len(bodies[0]) {
		t.Errorf("the second turn's request was not smaller: %d bytes against the first's %d",
			len(bodies[2]), len(bodies[0]))
	}

	// And the number a user can see: the meter reads against the budget the backend
	// stated, with the window beside it, and says where the budget came from.
	var budget ContextBudget
	var haveBudget bool
	for _, ev := range captured.snapshot() {
		if ev.name == events.Budget {
			budget, haveBudget = ev.payload["budget"].(ContextBudget)
		}
	}
	if !haveBudget {
		t.Fatal("no budget event was emitted, so the meter was never told about the ceiling")
	}
	if budget.Ceiling != 8000 || budget.CeilingSource != llm.CeilingFromHeader {
		t.Errorf("budget ceiling = %d/%q, want 8000/%q", budget.Ceiling, budget.CeilingSource, llm.CeilingFromHeader)
	}
	if budget.Limit != 131072 || budget.EffectiveLimit != 8000 {
		t.Errorf("window/effective = %d/%d, want 131072/8000", budget.Limit, budget.EffectiveLimit)
	}
	if want := float64(budget.Used) / 8000 * 100; math.Abs(budget.Percent-want) > 0.001 {
		t.Errorf("Percent = %.3f, want %.3f — the meter has to read against the budget, not the window",
			budget.Percent, want)
	}
}

// TestASpentMinuteWaitsBeforeTheTurnGoesOut is the third thing the same allowance
// says, beside "this request is too large" (§6.2) and "this request is larger than a
// whole minute" (§6.1): there is simply nothing left of the minute to spend.
//
// The vendor's answer to that is a 429 naming a wait of seconds, which the shared
// client already obeys and shows. What this pins is that the wait no longer has to be
// *asked for*: the answer to the first turn stated what was left of the minute, so
// the second turn waits before it is sent, and the user is told the request is being
// held back rather than retried.
func TestASpentMinuteWaitsBeforeTheTurnGoesOut(t *testing.T) {
	st := newTestStore(t)
	groqChat(t, st)

	// A spent minute with a second and a half left in it, which is long enough for the
	// bounds below to be about the wait rather than about the scheduler. The allowance
	// is set far above the window so the per-request ceiling cannot bind: what this
	// measures is the pacing and nothing else.
	g := &groqServer{
		limitTokens: 200000, remainingTokens: 0, resetTokens: "1500ms", reply: "Antwort",
	}
	srv := httptest.NewServer(g.handler(t))
	defer srv.Close()
	engine, captured, _ := groqEngine(t, st, srv)

	// The first turn is not paced: nothing has answered yet, so nothing has stated a
	// budget, and the app does not invent one.
	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Frage eins"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 10*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the first turn did not finish")
	}
	for _, ev := range captured.snapshot() {
		if ev.name == events.Status && ev.payload["phase"] == pacedPhase {
			t.Error("the first turn was paced against a budget no response had stated yet")
		}
	}

	// The second turn waits, and the wait is the one the backend's own reset header
	// named — not a guess about how long a throttle lasts.
	started := time.Now()
	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Frage zwei"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 10*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the second turn did not finish")
	}
	elapsed := time.Since(started)
	if elapsed < time.Second {
		t.Errorf("the turn took %v — it was sent without waiting out the spent minute", elapsed)
	}

	// What the user was shown: the pacing phase with the seconds, and then the phase
	// retracted, because a wait left standing reads as a hung turn.
	var paced int
	var waitMs int64
	for _, ev := range captured.snapshot() {
		if ev.name != events.Status {
			continue
		}
		if ev.payload["phase"] == pacedPhase {
			paced++
			waitMs, _ = ev.payload["waitMs"].(int64)
		}
	}
	if paced == 0 {
		t.Fatal("no pacing phase was emitted, so a turn that waits a second looks hung")
	}
	// The wait is measured from the moment the first answer stated the minute, so the
	// bounds allow for however long this test took to get back to the engine — and a
	// turn that was never paced reports no wait at all, which is what paced == 0 above
	// is for.
	if waitMs < 700 || waitMs > 1500 {
		t.Errorf("the wait shown was %d ms, want the ~1500 the reset header named", waitMs)
	}
	statuses := []map[string]any{}
	for _, ev := range captured.snapshot() {
		if ev.name == events.Status {
			statuses = append(statuses, ev.payload)
		}
	}
	if got := statuses[len(statuses)-1]["phase"]; got != "idle" {
		t.Errorf("the turn ended in phase %v, want idle: the wait is left standing", got)
	}

	// And the turn answered afterwards: a wait in the middle of a turn is not the same
	// thing as a turn that produced nothing.
	messages, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	newest := messages[len(messages)-1]
	if newest.Role != "assistant" || newest.Status != store.StatusComplete {
		t.Fatalf("newest row = %s/%s, want a complete assistant reply", newest.Role, newest.Status)
	}
	if !strings.Contains(newest.Content, "Antwort") {
		t.Errorf("reply = %q, want the model's answer", newest.Content)
	}
}

// TestTheSummariserIsBudgetedAgainstTheCeiling is the failure that hides inside the
// fix: the call that makes a chat fit is a request too, and on Groq it is subject to
// the same per-minute allowance. A transcript budgeted against the summariser's 131k
// window is one more oversized request — a compaction that can never run.
func TestTheSummariserIsBudgetedAgainstTheCeiling(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedLongHistory(t, st, 4)

	provider := &fakeProvider{
		model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		ceiling: 2000,
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: "Zusammenfassung"}, {Type: llm.EventDone}},
		},
	}
	engine, _ := newTestEngine(t, st, provider)

	if _, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	summaryTokens := llm.EstimateMessagesTokens(provider.requestAt(0).Messages)
	if summaryTokens >= 2000 {
		t.Errorf("the summarisation request was estimated at %d tokens against a ceiling of 2000: "+
			"it would be refused for its own size", summaryTokens)
	}
}

// TestASizeFoldIsNamedInTheTranscript keeps the trigger honest: a fold nobody pressed
// is not "manual". The card in the transcript is the only place a user learns why
// turns became a memory, and crediting them with a button press is a lie about the
// one run they need explained.
func TestASizeFoldIsNamedInTheTranscript(t *testing.T) {
	if triggerTooLarge == triggerManual || triggerTooLarge == triggerAuto {
		t.Fatalf("trigger names collide: %q", triggerTooLarge)
	}
	if got := triggerName(false); got != triggerAuto {
		t.Errorf("triggerName(false) = %q, want %q", got, triggerAuto)
	}
	if got := triggerName(true); got != triggerManual {
		t.Errorf("triggerName(true) = %q, want %q", got, triggerManual)
	}
}

// TestTheSizeTargetAimsTheFoldAtTheCeiling pins the arithmetic the recovery rests
// on. The planner prices the turns it keeps against the chat's window, so a ceiling
// that is a fraction of that window has to be handed to it as a fraction — otherwise
// a "fit the request" fold would fold nothing, because 2% of the window looks empty.
func TestTheSizeTargetAimsTheFoldAtTheCeiling(t *testing.T) {
	chat := store.Chat{ID: "c"}
	settings := config.Default()
	settings.Compact.Threshold = 0
	model := llm.ModelInfo{ID: "m", ContextLimit: 131072}

	// Groq's shape: a 10000-token allowance under a 131k window.
	target := sizeTarget(chat, settings, model, 10000)
	if got := target * float64(model.ContextLimit); got < 9900 || got > 10100 {
		t.Errorf("a 10000-token ceiling aims at %d tokens of the window, want 10000", int(got))
	}
	// With no ceiling the request was over the *window*, so the aim is the automatic
	// threshold — the user's setting, and 0.8 when they have not touched it.
	if got := sizeTarget(chat, settings, model, 0); got != effectiveThreshold(chat, settings) {
		t.Errorf("target without a ceiling = %v, want the configured threshold %v",
			got, effectiveThreshold(chat, settings))
	}
	// A ceiling so small that the ratio rounds to nothing still has to reach the
	// planner as a positive target: 0 means "fold only as far as the keep-turns
	// boundary", which is the opposite of what a size fold is for.
	if got := sizeTarget(chat, settings, model, 1); got <= 0 || got > 0.001 {
		t.Errorf("target for a one-token ceiling = %v, want the smallest positive target", got)
	}
	// An unknown window has no ratio to compute against; the planner is left to fold
	// what it can rather than being handed an infinity.
	if got := sizeTarget(chat, settings, llm.ModelInfo{ID: "m"}, 10000); got != 0 {
		t.Errorf("target with an unknown window = %v, want 0", got)
	}
}

// TestASizeRefusalThroughTheSharedClientIsClassified keeps the chat-layer tests
// honest about what they are faking: the envelope these tests hand the engine is the
// one the real client classifies. It goes through a real HTTP response, so a change
// to the transport that stopped classifying refusals fails here.
func TestASizeRefusalThroughTheSharedClientIsClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		fmt.Fprint(w, groqOversizedBody)
	}))
	defer srv.Close()

	p := llm.NewRegistry([]llm.BackendConfig{{
		ID: "groq", Name: "Groq", Kind: "groq", BaseURL: srv.URL, APIKey: "k", Enabled: true,
	}})
	provider, ok := p.Provider("groq")
	if !ok {
		t.Fatal("the groq kind built no provider")
	}
	_, err := provider.Chat(context.Background(), llm.ChatRequest{
		Model:    "openai/gpt-oss-20b",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, llm.ErrRequestTooLarge) {
		t.Fatalf("error = %v, want an oversized-request refusal", err)
	}
	too, ok := llm.RequestTooLarge(err)
	if !ok || too.Limit != 10000 {
		t.Fatalf("verdict = %+v, want the 10000 the envelope named", too)
	}
	// The JSON is only here to keep the constant honest about what it is.
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(groqOversizedBody), &envelope); err != nil {
		t.Fatalf("the fixture is not the envelope it claims to be: %v", err)
	}
	if envelope.Error.Code != "rate_limit_exceeded" {
		t.Errorf("fixture code = %q, want the one that makes this refusal hard to read", envelope.Error.Code)
	}
}
