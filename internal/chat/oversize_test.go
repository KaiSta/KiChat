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
	"kichat/internal/tools"
	"kichat/internal/uimsg"
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

	// The user is told why, with the limit the vendor named in it. The sentence is the
	// window's, so what is pinned is the code and the number it carries — the refusal names
	// the limit, and the note inside it names the requested size.
	sawWarning := false
	for _, ev := range captured.snapshot() {
		if ev.name != events.Warning {
			continue
		}
		message, ok := uimsg.Parse(fmt.Sprint(ev.payload["warning"]))
		if !ok || message.Code != "chat.tooLargeRejected" {
			continue
		}
		note, ok := uimsg.Parse(message.Args[1])
		if ok && note.Code == "chat.ceilingNoteRequested" &&
			len(note.Args) == 2 && note.Args[0] == "10000" {
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
	// What the user reads comes from the dictionary now, so the assertion is on the
	// code and the numbers it carries: the situation, the stated limit, and what this
	// request was counted at — the three things the sentence has to name.
	message, ok := uimsg.Parse(failure)
	if !ok {
		t.Fatalf("error %q is not a message the window can translate", failure)
	}
	if message.Code != "chat.tooLargeNoHistoryLimitRequested" {
		t.Errorf("code = %q, want chat.tooLargeNoHistoryLimitRequested", message.Code)
	}
	want := []string{"Fake", "10000", "15088"}
	if len(message.Args) != len(want) {
		t.Fatalf("args = %v, want %v", message.Args, want)
	}
	for i := range want {
		if message.Args[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, message.Args[i], want[i])
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
	message, ok := uimsg.Parse(failure)
	if !ok {
		t.Fatalf("error %q is not a message the window can translate", failure)
	}
	if message.Code != "chat.tooLargeFoldFailedLimitRequested" {
		t.Errorf("code = %q, want the fold-failed code rather than nothing-to-fold", message.Code)
	}
	// The cause is named, and it is the summariser rather than the chat: that warning
	// and the sentence above describe one event, so they cannot disagree. The summariser's
	// own refusal arrives nested inside the code, which is how one message carries both.
	sawCause := false
	for _, w := range warnings {
		message, ok := uimsg.Parse(w)
		if !ok || message.Code != "chat.foldFailed" || len(message.Args) != 1 {
			continue
		}
		cause, ok := uimsg.Parse(message.Args[0])
		if ok && cause.Code == "chat.summaryFailed" && strings.Contains(cause.Args[0], "antwortet nicht") {
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

// TestAnAutomaticCeilingFoldAimsAtTheCeiling pins what a preventive fold is allowed
// to cost. The planner resolves an aim — the usage ratio that puts the folded request
// at the ceiling — and the walk used to price that aim against the caller's own Target,
// which an automatic run leaves at 0. Zero in that walk means "fold as far as the cut
// allows", so every ceiling fold threw the whole memory away to keep only the turn it
// was answering, on a backend whose ceiling folds on most tool-heavy turns.
func TestAnAutomaticCeilingFoldAimsAtTheCeiling(t *testing.T) {
	const (
		window  = 131072
		ceiling = 2000
		turns   = 10
	)
	var messages []store.Message
	for i := 0; i < turns; i++ {
		messages = append(messages,
			store.Message{Seq: i*2 + 1, Role: "user", Content: strings.Repeat("wort ", 50)},
			store.Message{Seq: i*2 + 2, Role: "assistant", Content: strings.Repeat("wort ", 50)},
		)
	}

	plan := PlanCompaction(PlanInput{
		Messages: messages,
		Budget: ContextBudget{
			Limit: window, Ceiling: ceiling, EffectiveLimit: ceiling,
			Projected: 2400, ProjectedRatio: 2400.0 / ceiling,
			System: 200,
		},
		Ceiling:         ceiling,
		KeepRecentTurns: 2,
		Reserve:         100,
	})
	if !plan.Needed {
		t.Fatalf("no fold planned for a request at %d of a %d-token ceiling", 2400, ceiling)
	}

	// Two turns kept verbatim is the aim: their 200 tokens plus the 200-token prompt,
	// the 100-token reserve and a 1200-token summary come to 1700, which fits the
	// ceiling. Folding further would buy nothing a request can spend.
	if want := 2*turns - 3; plan.KeepFromSeq != want {
		t.Fatalf("kept history starts at seq %d, want %d — the fold was not aimed at the ceiling",
			plan.KeepFromSeq, want)
	}
	if plan.FoldedCount != 2*(turns-2) {
		t.Fatalf("folded %d messages, want the %d older turns and no more",
			plan.FoldedCount, 2*(turns-2))
	}
	// The turn the fold is answering is never part of it.
	if plan.ToSeq >= messages[len(messages)-2].Seq {
		t.Fatalf("the fold reached seq %d, into the turn being answered", plan.ToSeq)
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

// TestTheSummariserFollowsTheCorrectedCeiling is the second request a size fold makes.
// The caller that learned the app's estimate sits below the vendor's count hands
// compactWith a ceiling expressed in the app's own units, and the summarisation has
// to be budgeted against that one — against the published ceiling it would be the one
// oversized request left in a fold whose whole job is to remove them.
func TestTheSummariserFollowsTheCorrectedCeiling(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	// A history long enough that an uncorrected summarisation would fill the published
	// ceiling's transcript budget rather than fit in the output cap's shadow.
	seedLongHistory(t, st, 60)

	provider := &fakeProvider{
		model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		ceiling: 30000,
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: "Zusammenfassung"}, {Type: llm.EventDone}},
		},
	}
	engine, _ := newTestEngine(t, st, provider)

	if _, err := engine.Compact(context.Background(), "chat-1", CompactOptions{
		Force: true, SummaryCeiling: 6000,
	}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	summaryTokens := llm.EstimateMessagesTokens(provider.requestAt(0).Messages)
	if summaryTokens > 6000 {
		t.Errorf("the summarisation request was estimated at %d tokens against the corrected "+
			"ceiling of 6000", summaryTokens)
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

// TestTheFoldAimsWithTheVendorsOwnCountOfTheRequest pins the correction the recovery
// needs when the vendor's tokenizer and the app's estimator disagree. One refusal
// states two numbers for the same request — `Requested`, the vendor's count, and the
// app's own estimate of the body it sent — and their ratio is how far the fold aims
// below the ceiling. Without it the fold reaches a size the app believes is the
// ceiling and the vendor refuses again, which ends the turn because a refusal is
// folded only once.
func TestTheFoldAimsWithTheVendorsOwnCountOfTheRequest(t *testing.T) {
	chat := store.Chat{ID: "c"}
	settings := config.Default()
	settings.Compact.Threshold = 0
	model := llm.ModelInfo{ID: "m", ContextLimit: 131072}

	// Nothing counted: the plain ceiling, exactly as before.
	if got, want := sizeTargetWithCount(chat, settings, model, 10000, 0, 0),
		sizeTarget(chat, settings, model, 10000); got != want {
		t.Errorf("target without a count = %v, want %v", got, want)
	}
	// The vendor counted the request at twice the app's estimate, so the fold has to
	// aim at half the ceiling to land on it: 5000 of a 131k window.
	got := sizeTargetWithCount(chat, settings, model, 10000, 20000, 10000)
	if want := 5000.0 / 131072; math.Abs(got-want) > 1e-9 {
		t.Errorf("target for a two-to-one undercount = %v, want %v", got, want)
	}
	// An over-count is the safe direction and must not fold less: the request is
	// already under the ceiling in the vendor's units.
	if got, want := sizeTargetWithCount(chat, settings, model, 10000, 8000, 10000),
		sizeTarget(chat, settings, model, 10000); got != want {
		t.Errorf("target for an over-count = %v, want the plain %v", got, want)
	}
	// A ratio read out of two tiny numbers is noise; a chat that small cannot be over
	// a ten-thousand-token ceiling anyway.
	if got, want := sizeTargetWithCount(chat, settings, model, 10000, 400, 50),
		sizeTarget(chat, settings, model, 10000); got != want {
		t.Errorf("target for a tiny estimate = %v, want the plain %v", got, want)
	}
	// A wild ratio is capped rather than allowed to fold the chat to nothing.
	if got, want := sizeTargetWithCount(chat, settings, model, 10000, 90000, 1000),
		2500.0/131072; math.Abs(got-want) > 1e-9 {
		t.Errorf("target for a wild undercount = %v, want %v", got, want)
	}
}

// countingServer is a vendor whose tokenizer counts a request at a multiple of the
// app's own estimate and refuses anything over its limit. It is the failure the
// correction exists for, modelled directly: the app's body looks small enough to it,
// the vendor's count is not, and a fold aimed with the app's arithmetic is refused a
// second time.
type countingServer struct {
	limit      int
	multiplier float64
	reply      string

	mu     sync.Mutex
	counts []int
}

func (c *countingServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openai/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, groqChatListing)
		case "/openai/v1/chat/completions":
			raw, _ := io.ReadAll(r.Body)
			requested := int(float64(estimateBodyTokens(string(raw))) * c.multiplier)
			c.mu.Lock()
			c.counts = append(c.counts, requested)
			c.mu.Unlock()
			w.Header().Set("X-Ratelimit-Limit-Tokens", fmt.Sprint(c.limit))
			if requested > c.limit {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				fmt.Fprintf(w, `{"error":{"message":"Request too large for model `+"`openai/gpt-oss-20b`"+
					` on tokens per minute (TPM): Limit %d, Requested %d, please reduce your message size and `+
					`try again.","type":"tokens","code":"rate_limit_exceeded"}}`, c.limit, requested)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", c.reply)
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],"+
				"\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":4,\"total_tokens\":%d}}\n\n", requested, requested+4)
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}
}

func (c *countingServer) vendorCounts() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.counts...)
}

// estimateBodyTokens prices a request body the way the app prices a request: the
// messages, their tool calls, and the tool schemas. It is the same estimator, so a
// body the app believes is N tokens is N here too — and the server's own multiplier
// is what stands in for a tokenizer that counts more.
func estimateBodyTokens(raw string) int {
	var body struct {
		Messages []struct {
			Content   any    `json:"content"`
			Reasoning string `json:"reasoning_content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Parameters  any    `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		return len(raw) / 4
	}
	total := 0
	for _, m := range body.Messages {
		total += 4
		switch content := m.Content.(type) {
		case string:
			total += llm.EstimateTokens(content)
		case []any:
			for _, part := range content {
				if s, ok := part.(string); ok {
					total += llm.EstimateTokens(s)
				}
			}
		}
		total += llm.EstimateTokens(m.Reasoning) / 2
		for _, tc := range m.ToolCalls {
			total += llm.EstimateTokens(tc.Function.Name) + llm.EstimateTokens(tc.Function.Arguments) + 8
		}
	}
	for _, tool := range body.Tools {
		total += llm.EstimateTokens(tool.Function.Name) + llm.EstimateTokens(tool.Function.Description) + 8
		if tool.Function.Parameters != nil {
			if schema, err := json.Marshal(tool.Function.Parameters); err == nil {
				total += llm.EstimateTokens(string(schema))
			}
		}
	}
	return total
}

// TestAFoldedRetryFitsWhenTheVendorCountsMoreThanTheApp is the turn this fix is
// about: before it, the fold made the chat smaller and the retry was refused anyway,
// because the fold had been aimed with the app's estimate rather than the vendor's
// count. The turn then ended as an error even though the history had already been
// summarised — the most expensive possible outcome, since the summary was paid for.
func TestAFoldedRetryFitsWhenTheVendorCountsMoreThanTheApp(t *testing.T) {
	st := newTestStore(t)
	groqChat(t, st)
	// Enough history that the fold has real work to do: the retry has to drop most of
	// it, and a fold aimed with the app's own arithmetic would keep turns whose real
	// count is over the ceiling.
	filler := strings.Repeat("Der Verlauf einer Recherche ist lang und wird länger. ", 40)
	for i := 0; i < 40; i++ {
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

	counter := &countingServer{limit: 10000, multiplier: 2, reply: "Antwort nach der Kompression"}
	srv := httptest.NewServer(counter.handler(t))
	defer srv.Close()
	engine, captured, _ := groqEngine(t, st, srv)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Und weiter?"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 10*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	for _, ev := range captured.snapshot() {
		if ev.name == events.Error {
			t.Fatalf("the turn failed after folding: %v", ev.payload["error"])
		}
	}
	messages, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	newest := messages[len(messages)-1]
	if newest.Role != "assistant" || newest.Status != store.StatusComplete ||
		!strings.Contains(newest.Content, "Antwort nach der Kompression") {
		t.Fatalf("newest row = %s/%s %q, want a complete answer after the fold",
			newest.Role, newest.Status, newest.Content)
	}

	// The last request — the answer — fits the vendor's count, and the first did not.
	counts := counter.vendorCounts()
	if len(counts) != 3 {
		t.Fatalf("requests = %d, want a refusal, a summary and an answer: %v", len(counts), counts)
	}
	if counts[0] <= counter.limit {
		t.Errorf("the fixture never refused: first request counted at %d of %d", counts[0], counter.limit)
	}
	if counts[2] > counter.limit {
		t.Errorf("the answered request counted at %d, over the %d limit", counts[2], counter.limit)
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

// settingsWithOverride is an engine's settings with one model override added, built the
// way the settings panel leaves it. It is a function because Engine.Settings is one: the
// engine reads the current snapshot on every call, so a test cannot hand it a value once.
func settingsWithOverride(base config.Settings, backend, model string, ov llm.ModelOverride) func() config.Settings {
	return func() config.Settings {
		s := base
		s.ModelOverrides = map[string]llm.ModelOverride{backend + "/" + model: ov}
		return s
	}
}

// TestTheUsersOwnCeilingFoldsBeforeTheRequestGoesOut is the one route to a per-request
// ceiling that speaks before the backend does.
//
// Groq states that budget nowhere a client can ask: the listing publishes windows, the
// two documentation tables disagree by more than an order of magnitude for the same
// models, and the exact figure for an organization is on a signed-in page while the wire
// carries it only on a *response* (GROQ.md §6.2). So the first request of a long chat is
// refused once, and the prevention the header enables only works from the turn after it.
// A figure the user read off their Limits page and typed into the model's override is the
// only number available at that moment — and it has to drive the same preventive fold,
// not a second code path beside it.
func TestTheUsersOwnCeilingFoldsBeforeTheRequestGoesOut(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedLongHistory(t, st, 4)

	// Nothing is published by this provider: no ceiling has been learned, because no
	// request has been sent yet — which is exactly the state a fresh process is in.
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: "Zusammenfassung"}, {Type: llm.EventDone}},
			{{Type: llm.EventDelta, Text: "Antwort"}, {Type: llm.EventDone}},
		},
	}
	engine, captured := newTestEngine(t, st, provider)
	if got := provider.requestCount(); got != 0 {
		t.Fatalf("backend calls before the turn = %d, want none", got)
	}
	engine.Settings = settingsWithOverride(engine.Settings(), "fake", "fake-model",
		llm.ModelOverride{RequestCeiling: 2000})

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Eine neue Frage"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	if got := len(summariesOf(t, st)); got != 1 {
		t.Fatalf("compactions = %d, want the fold the typed ceiling asked for before the request", got)
	}
	// The summary and the answer: the fold ran first, so nothing was refused on the way.
	if n := provider.requestCount(); n != 2 {
		t.Fatalf("backend calls = %d, want the summary and the answer", n)
	}
	for _, ev := range captured.snapshot() {
		if ev.name == events.Error {
			t.Errorf("a turn the typed ceiling could be planned against failed: %v", ev.payload["error"])
		}
	}
}

// TestATypedCeilingNeverLoosensWhatTheBackendStated pins the direction of the merge. The
// typed number exists to fill a silence, not to overrule a measurement: a vendor that
// states a smaller budget has to win, or a stale entry in Settings would let the app send
// requests it already knows are too big. The reverse — a typed figure tighter than the
// header's — is kept, because the cost of being wrong in that direction is an extra fold
// rather than a refused turn.
func TestATypedCeilingNeverLoosensWhatTheBackendStated(t *testing.T) {
	cases := []struct {
		name       string
		published  int
		manual     int
		want       int
		wantSource string
	}{
		{"the vendor's smaller number wins", 2000, 64000, 2000, llm.CeilingFromRefusal},
		{"the typed smaller number wins", 8000, 1000, 1000, llm.CeilingFromManual},
		{"the typed number stands alone", 0, 4000, 4000, llm.CeilingFromManual},
		{"neither states one", 0, 0, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			provider := &fakeProvider{
				model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072},
				ceiling: c.published,
			}
			engine, _ := newTestEngine(t, newTestStore(t), provider)
			engine.Settings = settingsWithOverride(engine.Settings(), "fake", "fake-model",
				llm.ModelOverride{RequestCeiling: c.manual})

			got := engine.ceilingFor("fake", "fake-model")
			if got.Tokens != c.want {
				t.Fatalf("ceiling = %d, want %d", got.Tokens, c.want)
			}
			if got.Source != c.wantSource {
				t.Fatalf("source = %q, want %q", got.Source, c.wantSource)
			}
		})
	}
}

// TestTheMeterShowsTheTypedCeiling is the other half of the same fact: the number the
// fold uses is the number the meter paints. A typed ceiling that reached the planner but
// not the budget would show a chat as comfortable while its next request was about to be
// folded, and the inspector would name a source that is not the one in force.
func TestTheMeterShowsTheTypedCeiling(t *testing.T) {
	st := newTestStore(t)
	chat := makeChat(t, st)
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
	}
	engine, _ := newTestEngine(t, st, provider)
	engine.Settings = settingsWithOverride(engine.Settings(), "fake", "fake-model",
		llm.ModelOverride{RequestCeiling: 2000})

	assembly, _, err := engine.buildAssembly(context.Background(), &chat, "fake", "fake-model",
		engine.Settings(), provider.model, "", "", "")
	if err != nil {
		t.Fatalf("buildAssembly: %v", err)
	}
	if assembly.Budget.Ceiling != 2000 {
		t.Fatalf("budget ceiling = %d, want the typed 2000", assembly.Budget.Ceiling)
	}
	if assembly.Budget.CeilingSource != llm.CeilingFromManual {
		t.Fatalf("ceiling source = %q, want %q", assembly.Budget.CeilingSource, llm.CeilingFromManual)
	}
	if assembly.Budget.EffectiveLimit != 2000 {
		t.Fatalf("effective limit = %d, want the ceiling rather than the 131k window",
			assembly.Budget.EffectiveLimit)
	}
}

// TestAToolRoundFoldsBeforeItsNextRequestCrossesTheCeiling is the mid-turn half of the
// prevention: the check before a turn prices the history as it stands, and a tool round
// then adds a model reply and a tool result that neither that check nor the request that
// followed it ever saw.
//
// The turn is arranged so that only the *second* check can fire — the ceiling is placed
// just above what the turn would send, measured rather than guessed, so the fold before
// the turn stays quiet and the reply the model writes is what crosses the line. A refusal
// at that point would cost the whole answer, because the fold that answers it restarts the
// turn (foldForSize runs on the turn's error, not on a round's).
func TestAToolRoundFoldsBeforeItsNextRequestCrossesTheCeiling(t *testing.T) {
	st := newTestStore(t)
	chat := makeChat(t, st)
	seedLongHistory(t, st, 12)

	call := "```tool_call\n{\"name\": \"search_web\", \"arguments\": {\"query\": \"mietrecht\"}}\n```"
	report := strings.Repeat("Der Bericht ist lang und wird länger. ", 120)
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			// First round: a tool call, in the text protocol a model without native
			// tools uses, with a report long enough to be the growth the fold exists for.
			{{Type: llm.EventDelta, Text: call + "\n\n" + report}},
			// The fold's summarisation, then the round it made room for.
			{{Type: llm.EventDelta, Text: "Zusammenfassung"}, {Type: llm.EventDone}},
			{{Type: llm.EventDelta, Text: "Antwort"}, {Type: llm.EventDone}},
		},
	}
	engine, captured := newTestEngine(t, st, provider)

	before, _, err := engine.buildAssembly(context.Background(), &chat, "fake", "fake-model",
		engine.Settings(), provider.model, tools.TextProtocolHint(config.LangEnglish), "", "")
	if err != nil {
		t.Fatalf("buildAssembly: %v", err)
	}
	projected := before.Budget.Projected
	if projected <= 0 {
		t.Fatalf("projected = %d, want a measured request size", projected)
	}
	// The fold point is 0.9 × ceiling, so a ceiling at 1.125 × projected leaves the turn's
	// own check below its line (0.9 × 1.125 = 1.0125) while any real growth crosses it.
	provider.ceiling = projected + projected/8 + 1
	if provider.ceiling <= projected {
		t.Fatalf("ceiling %d does not leave the turn's own check quiet (projected %d)",
			provider.ceiling, projected)
	}

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Eine neue Frage"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	if got := len(summariesOf(t, st)); got != 1 {
		t.Fatalf("compactions = %d, want the one the second round's check asked for", got)
	}
	// Round one, the summary the second round paid for, then round two.
	if n := provider.requestCount(); n != 3 {
		t.Fatalf("backend calls = %d, want the round, its fold, and the next round", n)
	}
	second := provider.requestAt(2)
	var carriedOldest, keptNewest bool
	for _, m := range second.Messages {
		if strings.Contains(m.Content, "Frage 0:") {
			carriedOldest = true
		}
		if strings.Contains(m.Content, "Eine neue Frage") {
			keptNewest = true
		}
	}
	if carriedOldest {
		t.Error("the round after the fold still carried the oldest turn verbatim")
	}
	if !keptNewest {
		t.Error("the fold swallowed the message the user just sent")
	}
	for _, ev := range captured.snapshot() {
		if ev.name == events.Error {
			t.Errorf("a long tool turn failed although its fold could fit: %v", ev.payload["error"])
		}
	}
}

// TestAToolResultIsCutToWhatTheRequestCanCarry is the failure the measured room
// exists for, in the shape it was reported: the model thinks, asks for a
// web search, and the round that reads the result is refused because the result alone
// is bigger than the account's whole per-request allowance.
//
// A history fold cannot repair that. The result belongs to the turn being answered and
// a fold stops at the newest user turn, so the only thing that can make the round
// sendable is cutting the result where it is handed to the model. The document below
// is the offline twin of a scraped page — same verbatim body, same size, no network.
func TestAToolResultIsCutToWhatTheRequestCanCarry(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	const ceiling = 6000
	body := strings.Repeat("Ein langer Absatz mit Inhalt. ", 1600)
	if tokens := llm.EstimateTokens(body); tokens < 2*ceiling {
		t.Fatalf("the fixture's chunk is %d tokens; it has to exceed the %d-token ceiling by a wide margin",
			tokens, ceiling)
	}
	if err := st.CreateDocument(store.Document{
		ID: "doc-1", ChatID: "chat-1", Title: "bericht", Text: body,
		TokenCount: llm.EstimateTokens(body),
	}); err != nil {
		t.Fatalf("create document: %v", err)
	}
	if err := st.InsertChunks([]store.Chunk{{
		DocumentID: "doc-1", ChatID: "chat-1", Ordinal: 0, Text: body,
		Tokens: llm.EstimateTokens(body),
	}}); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}

	call := "```tool_call\n{\"name\": \"read_pages\", \"arguments\": " +
		"{\"document\": \"bericht\", \"from\": 0, \"to\": 0}}\n```"
	provider := &fakeProvider{
		model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		ceiling: ceiling,
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: call}},
			{{Type: llm.EventDelta, Text: "Antwort"}, {Type: llm.EventDone}},
		},
	}

	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = 3
	registry := llm.NewRegistry(nil)
	registry.Register(provider)
	log := &eventLog{}
	engine := NewEngine(st, registry, tools.New(st, nil, nil),
		func() config.Settings { return settings }, log.emit)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Lies den Bericht."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	if got := provider.requestCount(); got != 2 {
		t.Fatalf("backend calls = %d, want the round and the answer after it", got)
	}
	second := provider.requestAt(1)
	carried := ""
	for _, m := range second.Messages {
		if m.Role == llm.RoleTool {
			carried = m.Content
		}
	}
	if carried == "" {
		t.Fatal("the second request carried no tool result")
	}
	if !strings.Contains(carried, "gekürzt") {
		t.Fatalf("the tool result reached the model whole (%d tokens) although it is bigger "+
			"than the %d-token ceiling", llm.EstimateTokens(carried), ceiling)
	}
	if used := llm.EstimateMessagesTokens(second.Messages); used > ceiling {
		t.Fatalf("the second request estimates at %d tokens for a %d-token ceiling", used, ceiling)
	}
	for _, ev := range log.snapshot() {
		if ev.name == events.Error {
			t.Errorf("the turn failed although the result could be cut to fit: %v", ev.payload["error"])
		}
	}
}

// TestTheCutFollowsTheBackendsOwnPromptCount is the end-to-end half of the room's
// arithmetic. A backend that states what the request cost must be believed over the
// assembly's estimate, because the estimate runs low in the direction that lets a tool
// result through that the request cannot carry — the same error §6.2 item 1 of GROQ.md
// corrects when it aims a fold.
//
// The stated count is placed where the two answers differ by a lot: the estimate prices
// this prompt at a few hundred tokens, so a room taken from it would be over a thousand
// tokens larger than the one the backend's own number allows.
func TestTheCutFollowsTheBackendsOwnPromptCount(t *testing.T) {
	const (
		ceiling        = 6000
		reportedPrompt = 1200
	)
	st := newTestStore(t)
	makeChat(t, st)

	body := strings.Repeat("Ein langer Absatz mit Inhalt. ", 1600)
	if tokens := llm.EstimateTokens(body); tokens < 2*ceiling {
		t.Fatalf("the fixture's chunk is %d tokens; it has to exceed the %d-token ceiling by a wide margin",
			tokens, ceiling)
	}
	if err := st.CreateDocument(store.Document{
		ID: "doc-1", ChatID: "chat-1", Title: "bericht", Text: body,
		TokenCount: llm.EstimateTokens(body),
	}); err != nil {
		t.Fatalf("create document: %v", err)
	}
	if err := st.InsertChunks([]store.Chunk{{
		DocumentID: "doc-1", ChatID: "chat-1", Ordinal: 0, Text: body,
		Tokens: llm.EstimateTokens(body),
	}}); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}

	call := "```tool_call\n{\"name\": \"read_pages\", \"arguments\": " +
		"{\"document\": \"bericht\", \"from\": 0, \"to\": 0}}\n```"
	provider := &fakeProvider{
		model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		ceiling: ceiling,
		replies: [][]llm.StreamEvent{
			{
				{Type: llm.EventDelta, Text: call},
				{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: reportedPrompt}},
				{Type: llm.EventDone},
			},
			{{Type: llm.EventDelta, Text: "Antwort"}, {Type: llm.EventDone}},
		},
	}

	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = 3
	registry := llm.NewRegistry(nil)
	registry.Register(provider)
	log := &eventLog{}
	engine := NewEngine(st, registry, tools.New(st, nil, nil),
		func() config.Settings { return settings }, log.emit)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Lies den Bericht."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}
	// The round and the answer after it: a summarisation here would mean the tool result
	// was never read, and the assertion below would be about the wrong request.
	if got := provider.requestCount(); got != 2 {
		t.Fatalf("backend calls = %d, want the round and the answer after it", got)
	}

	carried := ""
	for _, m := range provider.requestAt(1).Messages {
		if m.Role == llm.RoleTool {
			carried = m.Content
		}
	}
	if carried == "" {
		t.Fatal("the second request carried no tool result")
	}
	// The limit, less the count the backend gave, less the reserve for the answer. The turn's
	// clock rides on this very message and the fake backend's figure is a fixed 1200 rather than
	// a count of the request it was sent — a real one counts the clock too, which is why the
	// room it leaves is already that much smaller — so the result is measured as stored.
	want := ceiling - reportedPrompt - settings.Compact.ReserveForResponse
	if got := llm.EstimateTokens(withoutTurnState(carried)); got > want {
		t.Fatalf("the tool result is %d tokens; with the backend's own count of %d the %d-token "+
			"ceiling leaves %d", got, reportedPrompt, ceiling, want)
	}
}

// TestTheBetweenRoundFoldAsksTheThresholdOnlyWhenTheUserSaidSo pins the two calls to
// autoFold. Before a turn both bounds are the planner's — the user's threshold as much as
// a published ceiling. Between rounds the ceiling is always asked, because crossing it
// costs the whole answer; the threshold is asked only when Compact.MidTurnCheck is on,
// because a window fold is a summarisation inside an answer somebody is waiting for. That
// switch is also the only mid-turn check a backend without a ceiling can get.
func TestTheBetweenRoundFoldAsksTheThresholdOnlyWhenTheUserSaidSo(t *testing.T) {
	fold := func(t *testing.T, midTurn, midTurnCheck bool, ceiling int) int {
		t.Helper()
		st := newTestStore(t)
		chat := makeChat(t, st)
		// The shipped default switches automatic compaction on, and this chat opts in:
		// the threshold is live and very low, so only the mid-turn gate can keep the
		// fold from happening.
		chat.AutoCompact = true
		if err := st.UpdateChat(chat); err != nil {
			t.Fatalf("enable auto-compaction: %v", err)
		}
		seedLongHistory(t, st, 12)
		provider := &fakeProvider{
			model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
			ceiling: ceiling,
			replies: [][]llm.StreamEvent{
				{{Type: llm.EventDelta, Text: "Zusammenfassung"}, {Type: llm.EventDone}},
			},
		}
		engine, _ := newTestEngine(t, st, provider)
		settings := engine.Settings()
		settings.Compact.Threshold = 0.01
		settings.Compact.MidTurnCheck = midTurnCheck
		if err := engine.autoFold(context.Background(), &chat, provider, "fake", "fake-model",
			provider.model, settings, midTurn); err != nil {
			t.Fatalf("autoFold: %v", err)
		}
		return len(summariesOf(t, st))
	}

	if got := fold(t, true, false, 0); got != 0 {
		t.Fatalf("between-round folds with no ceiling and the check off = %d, want none", got)
	}
	if got := fold(t, false, false, 0); got != 1 {
		t.Fatalf("the fold before the turn = %d, want the one the threshold asks for", got)
	}
	// The new switch: the same call, the same absent ceiling, one fold.
	if got := fold(t, true, true, 0); got != 1 {
		t.Fatalf("between-round folds with the check on = %d, want the one the threshold asks for", got)
	}
	if got := fold(t, true, false, 2000); got != 1 {
		t.Fatalf("between-round folds with a ceiling = %d, want the one it asks for", got)
	}
	// Both bounds live at once is still one decision and therefore one fold.
	if got := fold(t, true, true, 2000); got != 1 {
		t.Fatalf("between-round folds with both bounds = %d, want one", got)
	}
}

// TestABetweenRoundCheckFoldsAChatShorterThanKeepRecentTurns pins the shape a turn that grows
// through tool calls usually has: a few turns, each one long, and a context over the threshold.
// A targetless automatic fold is refused by the planner when the cut would keep every turn, so
// the switch did nothing in exactly the chats it exists for, and the growth became a fold only
// once the backend refused the request for its size.
func TestABetweenRoundCheckFoldsAChatShorterThanKeepRecentTurns(t *testing.T) {
	fold := func(t *testing.T, midTurn bool) int {
		t.Helper()
		st := newTestStore(t)
		chat := makeChat(t, st)
		chat.AutoCompact = true
		if err := st.UpdateChat(chat); err != nil {
			t.Fatalf("enable auto-compaction: %v", err)
		}
		// Three turns: fewer than the six the cut keeps verbatim by default.
		for i := 0; i < 3; i++ {
			for _, role := range []string{"user", "assistant"} {
				if _, err := st.AppendMessage(store.Message{
					ID: fmt.Sprintf("%s-%d", role, i), ChatID: chat.ID, Role: role,
					Content: strings.Repeat("Frage mit Inhalt. ", 400),
					Status:  store.StatusComplete,
				}); err != nil {
					t.Fatalf("seed %s %d: %v", role, i, err)
				}
			}
		}
		provider := &fakeProvider{
			model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
			replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "Zusammenfassung"}, {Type: llm.EventDone}}},
		}
		engine, _ := newTestEngine(t, st, provider)
		settings := engine.Settings()
		settings.Compact.Threshold = 0.01
		settings.Compact.MidTurnCheck = true
		if err := engine.autoFold(context.Background(), &chat, provider, "fake", "fake-model",
			provider.model, settings, midTurn); err != nil {
			t.Fatalf("autoFold: %v", err)
		}
		return len(summariesOf(t, st))
	}

	if got := fold(t, true); got != 1 {
		t.Fatalf("between-round folds on a chat shorter than keepRecentTurns = %d, want the one the threshold asks for", got)
	}
	// The check before a turn keeps its own behaviour: there the user has a fold point between
	// two messages, so a short chat is left alone rather than folded unasked.
	if got := fold(t, false); got != 0 {
		t.Fatalf("the fold before the turn = %d, want none on a chat shorter than keepRecentTurns", got)
	}
}

// TestTheMidTurnCheckFoldsALocalToolTurnWhenTheUserAsks is the behaviour the setting
// exists for. A local model publishes no per-request ceiling, so without it the only size
// check is the one before the turn — and a tool round that grows past the window's
// threshold is never noticed, because the growth does not exist yet when that check runs.
//
// The growth is the model's own report, deliberately: it is the one part of a round that
// nothing cuts to fit (a tool result is budgeted against the room left), and it is what
// the next round has to carry.
func TestTheMidTurnCheckFoldsALocalToolTurnWhenTheUserAsks(t *testing.T) {
	run := func(t *testing.T, midTurnCheck bool) (summaries int, carriedOldest bool) {
		t.Helper()
		st := newTestStore(t)
		chat := makeChat(t, st)
		chat.AutoCompact = true
		if err := st.UpdateChat(chat); err != nil {
			t.Fatalf("enable auto-compaction: %v", err)
		}
		// More turns than KeepRecentTurns, or the planner has nothing older to fold and
		// both sides of this test would agree for the wrong reason.
		seedLongHistory(t, st, 8)

		call := "```tool_call\n{\"name\": \"search_web\", \"arguments\": {\"query\": \"mietrecht\"}}\n```"
		report := strings.Repeat("Der Bericht ist lang und wird länger. ", 450)
		replies := [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: call + "\n\n" + report}},
		}
		if midTurnCheck {
			replies = append(replies, []llm.StreamEvent{
				{Type: llm.EventDelta, Text: "Zusammenfassung"}, {Type: llm.EventDone},
			})
		}
		replies = append(replies, []llm.StreamEvent{
			{Type: llm.EventDelta, Text: "Antwort"}, {Type: llm.EventDone},
		})

		provider := &fakeProvider{
			// No ceiling: the shape of every local backend here.
			model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
			replies: replies,
		}
		engine, captured := newTestEngine(t, st, provider)
		if midTurnCheck {
			settings := engine.Settings()
			settings.Compact.MidTurnCheck = true
			settings.Compact.Threshold = 0.8
			engine.Settings = func() config.Settings { return settings }
		}

		// The check before the turn has to stay quiet, or the fold would be the one that
		// already existed and this test would prove nothing about the mid-turn path.
		before, _, err := engine.buildAssembly(context.Background(), &chat, "fake", "fake-model",
			engine.Settings(), provider.model, tools.TextProtocolHint(config.LangEnglish), "", "")
		if err != nil {
			t.Fatalf("buildAssembly: %v", err)
		}
		if before.Budget.ProjectedRatio >= 0.8 {
			t.Fatalf("the turn's own check already folds at %.2f", before.Budget.ProjectedRatio)
		}

		if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Eine neue Frage"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
			t.Fatal("the turn did not finish")
		}
		for _, ev := range captured.snapshot() {
			if ev.name == events.Error {
				t.Errorf("the turn failed: %v", ev.payload["error"])
			}
		}

		last := provider.requestAt(provider.requestCount() - 1)
		for _, m := range last.Messages {
			if strings.Contains(m.Content, "Frage 0:") {
				carriedOldest = true
			}
		}
		return len(summariesOf(t, st)), carriedOldest
	}

	summaries, carriedOldest := run(t, false)
	if summaries != 0 {
		t.Fatalf("compactions without the setting = %d, want none", summaries)
	}
	if !carriedOldest {
		t.Fatal("the control case lost its oldest turn, so the two cases below would not" +
			" differ for the reason this test is about")
	}

	summaries, carriedOldest = run(t, true)
	if summaries != 1 {
		t.Fatalf("compactions with the setting on = %d, want the one the second round asked for", summaries)
	}
	if carriedOldest {
		t.Error("the round after the fold still carried the oldest turn verbatim")
	}
}
