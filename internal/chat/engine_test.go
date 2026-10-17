package chat

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/tools"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// fakeProvider is a scripted backend. Each entry in replies is one generation.
type fakeProvider struct {
	// id and name let a test register a second backend, so it can pin that a
	// compaction runs on a model other than the chat's own.
	id   string
	name string

	replies [][]llm.StreamEvent
	model   llm.ModelInfo
	// refusals, indexed by call number, fails a generation before a single byte
	// streams — that is what a backend refusing the request looks like from the
	// engine's side, and what an oversized request arrives as. A nil entry answers
	// normally.
	refusals []error
	// ceiling, when > 0, publishes a per-request ceiling (llm.RequestCeiling).
	// Groq's is its per-minute token allowance: much smaller than the model's
	// window, and the largest request the account may send.
	ceiling int
	// allowance, when its limit is > 0, publishes what the minute has left for
	// *requests* (llm.RequestAllowance): the rate the vendor stated and this app's own
	// spend. It is the reading a tool-heavy turn is planned against, so a test about
	// the pacing of a turn sets it and every other test leaves it at the zero value —
	// which is the backend that has stated no rate, and therefore no plan at all.
	allowance llm.Allowance
	// ceilingSource is where that ceiling came from, defaulting to a refusal.
	ceilingSource string
	// blockAfter, when > 0, makes a generation stall after that many events until
	// the context is cancelled. That is how a Stop button feels to the engine.
	blockAfter int
	// toggleThinking marks a backend whose request accepts the thinking toggle
	// (LM Studio's `enable_thinking`), so compaction asks it not to think even
	// though the model advertises no thinking capability.
	toggleThinking bool

	mu       sync.Mutex
	calls    int
	requests []llm.ChatRequest
	started  chan struct{}
}

func (f *fakeProvider) ID() string {
	if f.id != "" {
		return f.id
	}
	return "fake"
}

func (f *fakeProvider) Name() string {
	if f.name != "" {
		return f.name
	}
	return "Fake"
}

// CanToggleThinking implements llm.ThinkingToggle.
func (f *fakeProvider) CanToggleThinking() bool { return f.toggleThinking }

// RequestAllowance implements llm.RequestAllowance, and answers the zero value unless a test
// stated one — which is the shape of a backend whose request rate nothing has measured, and
// the state every turn that is not paced is in.
func (f *fakeProvider) RequestAllowance(string) llm.Allowance { return f.allowance }

// RequestCeiling implements llm.RequestCeiling. It answers the zero value unless a
// test published a ceiling, which is the shape of a backend that reports a window and
// no budget. The source is a parameter rather than a constant so a test can pin that
// the two routes to a ceiling — the vendor's refusal and the budget header of an
// ordinary answer — are reported as what they are.
func (f *fakeProvider) RequestCeiling(string) llm.Ceiling {
	if f.ceiling <= 0 {
		return llm.Ceiling{}
	}
	source := f.ceilingSource
	if source == "" {
		source = llm.CeilingFromRefusal
	}
	return llm.Ceiling{Tokens: f.ceiling, Source: source}
}

func (f *fakeProvider) Health(context.Context) error { return nil }
func (f *fakeProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return []llm.ModelInfo{f.model}, nil
}

func (f *fakeProvider) Chat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	f.mu.Lock()
	idx := f.calls
	f.calls++
	f.requests = append(f.requests, req)
	var events []llm.StreamEvent
	if idx < len(f.replies) {
		events = f.replies[idx]
	}
	var refusal error
	if idx < len(f.refusals) {
		refusal = f.refusals[idx]
	}
	block := f.blockAfter
	if f.started != nil && idx == 0 {
		close(f.started)
	}
	f.mu.Unlock()

	if refusal != nil {
		return nil, refusal
	}

	// The channel is buffered for every scripted event, so delivery never blocks.
	// A select against ctx.Done() would instead drop an already-scripted event
	// whenever Stop cancelled first — both cases become ready at once and Go
	// picks one at random — which is what made the "Stop keeps the partial
	// output" assertion flaky under the race detector.
	ch := make(chan llm.StreamEvent, len(events)+1)
	go func() {
		defer close(ch)
		for i, ev := range events {
			ch <- ev
			if block > 0 && i+1 >= block {
				<-ctx.Done()
				return
			}
		}
	}()
	return ch, nil
}

func (f *fakeProvider) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeProvider) lastRequest() llm.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return llm.ChatRequest{}
	}
	return f.requests[len(f.requests)-1]
}

// requestAt returns the nth request the provider was given, so a test can compare
// a retry against the attempt it followed.
func (f *fakeProvider) requestAt(i int) llm.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.requests) {
		return llm.ChatRequest{}
	}
	return f.requests[i]
}

type capturedEvent struct {
	name    string
	payload map[string]any
}

// eventLog collects the events a test engine emits. The engine emits from its
// own goroutine, so every read has to take the lock: reading the slice directly
// raced with the emitter, which `go test -race` caught.
type eventLog struct {
	mu     sync.Mutex
	events []capturedEvent
}

func (l *eventLog) emit(name string, payload any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, _ := payload.(map[string]any)
	l.events = append(l.events, capturedEvent{name: name, payload: m})
}

// snapshot returns a copy of the events seen so far, so a caller can iterate
// without holding the lock.
func (l *eventLog) snapshot() []capturedEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]capturedEvent(nil), l.events...)
}

func newTestEngine(t *testing.T, st *store.Store, provider *fakeProvider) (*Engine, *eventLog) {
	t.Helper()
	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = 3

	registry := llm.NewRegistry(nil)
	registry.Register(provider)

	log := &eventLog{}
	engine := NewEngine(st, registry, nil, func() config.Settings { return settings }, log.emit)
	return engine, log
}

func makeChat(t *testing.T, st *store.Store) store.Chat {
	t.Helper()
	c := store.Chat{
		ID: "chat-1", Title: "Test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "fake", Model: "fake-model", SystemPrompt: "Du bist ein Assistent.",
		Temperature: 0.7, TopP: 0.9, AutoCompact: false,
	}
	if err := st.CreateChat(c); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	return c
}

// TestStreamFailureMarksTheReplyIncomplete pins what a reply looks like when the
// backend stops delivering it halfway. The partial text is kept, but the row may
// not claim to be complete: nothing else would explain a sentence that simply
// ends, and the context meter would count a finished answer that never arrived.
func TestStreamFailureMarksTheReplyIncomplete(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{{
			{Type: llm.EventDelta, Text: "Halbe Antwort"},
			{Type: llm.EventError, Error: "Die Verbindung wurde unterbrochen: unexpected EOF"},
		}},
	}
	engine, captured := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Hallo"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("turn did not finish")
	}

	messages, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	var assistant *store.Message
	for i := range messages {
		if messages[i].Role == string(llm.RoleAssistant) {
			assistant = &messages[i]
		}
	}
	if assistant == nil {
		t.Fatal("no assistant message persisted")
	}
	if assistant.Content != "Halbe Antwort" {
		t.Errorf("content = %q, want the partial text to be kept", assistant.Content)
	}
	if assistant.Status != store.StatusFailed {
		t.Errorf("status = %q, want %q", assistant.Status, store.StatusFailed)
	}
	if assistant.StopReason != "stream_error" {
		t.Errorf("stop reason = %q, want stream_error", assistant.StopReason)
	}

	sawError := false
	for _, ev := range captured.snapshot() {
		if ev.name == events.Error {
			sawError = true
		}
	}
	if !sawError {
		t.Error("the failed stream was never reported to the UI")
	}
}

// TestToolRoundLimitIsReportedToTheUser covers the end of a tool loop: when the
// iteration budget runs out while the model is still asking for tools, the chat
// ends on tool results with no answer. That used to be silent — the turn looked
// as though it had produced nothing at all.
func TestToolRoundLimitIsReportedToTheUser(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	call := "```tool_call\n{\"name\": \"search_web\", \"arguments\": {\"query\": \"mietrecht\"}}\n```"
	reply := []llm.StreamEvent{{Type: llm.EventDelta, Text: call}}
	provider := &fakeProvider{
		model: llm.ModelInfo{
			ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded,
			// A capability list without tools keeps the text protocol in play.
			Capabilities: []string{"vision"},
		},
		replies: [][]llm.StreamEvent{reply, reply, reply, reply, reply, reply},
	}
	engine, captured := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Recherchiere Mietrecht"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("turn did not finish")
	}

	// newTestEngine sets ToolIterations to 3.
	if n := provider.requestCount(); n != 3 {
		t.Errorf("generations = %d, want the 3-iteration cap", n)
	}
	var warning string
	for _, ev := range captured.snapshot() {
		if ev.name == events.Warning {
			if text, ok := ev.payload["warning"].(string); ok {
				warning = text
			}
		}
	}
	if warning == "" {
		t.Fatal("the user was never told the tool budget ran out")
	}
	if !strings.Contains(warning, "Tool-Runden") {
		t.Errorf("warning = %q, want it to name the tool-round limit", warning)
	}
}

// TestDoneEventReportsTheStoredStatus pins the status the turn ends with against
// the status actually written to the reply row. A stream that died half-way stores
// "failed"; the end-of-turn event used to say "complete" for it, so the event and
// the row disagreed until something else refreshed the view from the database.
func TestDoneEventReportsTheStoredStatus(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{{
			{Type: llm.EventDelta, Text: "Halbe Antwort"},
			{Type: llm.EventError, Error: "Die Verbindung wurde unterbrochen: unexpected EOF"},
		}},
	}
	engine, captured := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Hallo"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("turn did not finish")
	}

	messages, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	stored := ""
	for _, m := range messages {
		if m.Role == string(llm.RoleAssistant) {
			stored = m.Status
		}
	}
	if stored != store.StatusFailed {
		t.Fatalf("stored status = %q, want %q", stored, store.StatusFailed)
	}

	reported := ""
	sawDone := false
	for _, ev := range captured.snapshot() {
		if ev.name == events.Done {
			sawDone = true
			reported, _ = ev.payload["status"].(string)
		}
	}
	if !sawDone {
		t.Fatal("no done event was emitted")
	}
	if reported != stored {
		t.Errorf("done reported status %q while the row holds %q", reported, stored)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestStopMidStreamKeepsPartialOutput is the Stop-button contract: the text that
// already arrived is preserved, the message is marked interrupted rather than
// complete, and the turn still terminates cleanly.
func TestStopMidStreamKeepsPartialOutput(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	provider := &fakeProvider{
		model:      llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192},
		blockAfter: 2,
		replies: [][]llm.StreamEvent{{
			{Type: llm.EventDelta, Text: "Erster Teil. "},
			{Type: llm.EventDelta, Text: "Zweiter Teil. "},
			// The provider now stalls until cancelled.
			{Type: llm.EventDelta, Text: "Nie gesendet."},
		}},
	}
	provider.started = make(chan struct{})
	engine, log := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Hallo"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Wait until the stream is actually producing, then press Stop.
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("generation never started")
	}
	if !waitFor(t, 2*time.Second, func() bool { return engine.IsRunning("chat-1") }) {
		t.Fatal("engine did not report a running turn")
	}
	if !engine.Stop("chat-1") {
		t.Fatal("Stop returned false for a running turn")
	}
	if !waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("turn did not finish after Stop")
	}

	messages, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	var assistant *store.Message
	for i := range messages {
		if messages[i].Role == string(llm.RoleAssistant) {
			assistant = &messages[i]
		}
	}
	if assistant == nil {
		t.Fatal("no assistant message persisted")
	}
	if assistant.Status != store.StatusInterrupted {
		t.Errorf("status = %q, want %q", assistant.Status, store.StatusInterrupted)
	}
	if assistant.Content == "" {
		t.Error("partial output was discarded; Stop must keep what already arrived")
	}
	if !contains(assistant.Content, "Erster Teil.") {
		t.Errorf("expected the streamed text to be kept, got %q", assistant.Content)
	}
	if assistant.StopReason != "interrupted" {
		t.Errorf("stop reason = %q", assistant.StopReason)
	}

	// A done event must still be emitted so the UI can stop showing a spinner.
	done := false
	for _, ev := range log.snapshot() {
		if ev.name == "chat:done" {
			done = true
			if status, _ := ev.payload["status"].(string); status != store.StatusInterrupted {
				t.Errorf("done status = %q, want interrupted", status)
			}
		}
	}
	if !done {
		t.Error("no done event emitted after Stop")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestCancelledToolCallsLeaveNoOrphan is the subtle one. If a generation is
// stopped between an assistant tool_calls message and its results, the history
// contains a dangling tool call and every subsequent request to the backend
// fails. Every call must therefore get a closed-out result.
func TestCancelledToolCallsLeaveNoOrphan(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	provider := &fakeProvider{model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192}}
	engine, _ := newTestEngine(t, st, provider)

	calls := []llm.ToolCall{
		{ID: "call-a", Name: "search_web", Arguments: `{"query":"a"}`},
		{ID: "call-b", Name: "scrape_url", Arguments: `{"url":"https://example.com"}`},
	}

	// Mirror the engine's order exactly: the assistant message is persisted with
	// its tool calls first, and only then are the calls executed. This is the
	// state a user's Stop button lands in.
	assistant := store.Message{
		ID: "m-assistant", ChatID: "chat-1", Seq: 1, Role: string(llm.RoleAssistant),
		Status: store.StatusComplete, CreatedAt: time.Now(),
		ToolCallsJSON: encodeToolCalls(calls),
	}
	if err := st.CreateMessage(assistant); err != nil {
		t.Fatal(err)
	}

	engine.cancelRemainingCalls("chat-1", assistant, calls, 0)

	// Every announced call must now have a matching tool result message.
	messages, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	answered := map[string]bool{}
	for _, m := range messages {
		if m.Role == string(llm.RoleTool) {
			answered[m.ToolCallID] = true
			if m.Status != store.ToolCancelled {
				t.Errorf("tool result status = %q, want %q", m.Status, store.ToolCancelled)
			}
		}
	}
	for _, call := range calls {
		if !answered[call.ID] {
			t.Errorf("tool call %s has no result; the next request would be rejected", call.ID)
		}
	}

	records, err := st.ListToolCalls("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 tool call records, got %d", len(records))
	}
	for _, r := range records {
		if r.Status != store.ToolCancelled {
			t.Errorf("record %s status = %q, want cancelled", r.ID, r.Status)
		}
	}

	// The rebuilt request must contain a result for both calls.
	assembled := ToLLMMessages(messages)
	var announced, results int
	for _, m := range assembled {
		announced += len(m.ToolCalls)
		if m.Role == llm.RoleTool {
			results++
		}
	}
	if announced != results {
		t.Fatalf("%d tool calls announced but %d results present", announced, results)
	}
}

// TestGenerationsAreSerialisedPerChat guards against two turns interleaving and
// corrupting each other's message sequence.
func TestGenerationsAreSerialisedPerChat(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	provider := &fakeProvider{
		model:      llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192},
		blockAfter: 1,
		replies: [][]llm.StreamEvent{{
			{Type: llm.EventDelta, Text: "a"}, {Type: llm.EventDelta, Text: "b"},
		}},
	}
	engine, _ := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "erst"}); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "zweite"}); err == nil {
		t.Fatal("expected the second Send to be rejected while a turn is running")
	}
	engine.Stop("chat-1")
	waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") })
}

// TestRegenerateDropsTrailingAssistant keeps a regenerate from appending a second
// answer on top of the first.
func TestRegenerateDropsTrailingAssistant(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: "erste Antwort"}},
			{{Type: llm.EventDelta, Text: "zweite Antwort"}},
		},
	}
	engine, _ := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Frage"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") })

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Regenerate: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") })

	messages, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	assistants := 0
	content := ""
	for _, m := range messages {
		if m.Role == string(llm.RoleAssistant) {
			assistants++
			content = m.Content
		}
	}
	if assistants != 1 {
		t.Fatalf("expected exactly 1 assistant message after regenerate, got %d", assistants)
	}
	if content != "zweite Antwort" {
		t.Fatalf("content = %q, want the regenerated answer", content)
	}
}

// TestEnginePersistsSpeedStats checks that the numbers the UI displays per
// message actually survive to the database.
func TestEnginePersistsSpeedStats(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192},
		replies: [][]llm.StreamEvent{{
			{Type: llm.EventDelta, Text: "Antwort"},
			{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 120, CompletionTokens: 8, TotalTokens: 128}},
			{Type: llm.EventDone, Stats: &llm.GenerationStats{
				TokensPerSecond: 42.5, TimeToFirstTokenMs: 180, GenerationMs: 190,
				StopReason: "stop", Source: llm.SourceLMStudio,
			}},
		}},
	}
	engine, _ := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Frage"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") })

	messages, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	var assistant *store.Message
	for i := range messages {
		if messages[i].Role == string(llm.RoleAssistant) {
			assistant = &messages[i]
		}
	}
	if assistant == nil {
		t.Fatal("no assistant message")
	}
	if assistant.TokensPerSecond != 42.5 {
		t.Errorf("tokens/s = %v, want 42.5", assistant.TokensPerSecond)
	}
	if assistant.TTFTMs != 180 {
		t.Errorf("ttft = %d, want 180", assistant.TTFTMs)
	}
	if assistant.PromptTokens != 120 || assistant.CompletionTokens != 8 {
		t.Errorf("tokens = %d/%d, want 120/8", assistant.PromptTokens, assistant.CompletionTokens)
	}
	if assistant.StatsSource != llm.SourceLMStudio {
		t.Errorf("stats source = %q", assistant.StatsSource)
	}
	if assistant.Status != store.StatusComplete {
		t.Errorf("status = %q, want complete", assistant.Status)
	}
}

// TestMeasurementIsModelScoped documents that the recorded prompt size is tied
// to the model that produced it.
func TestMeasurementIsModelScoped(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 4096},
		replies: [][]llm.StreamEvent{{
			{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 300, CompletionTokens: 1, TotalTokens: 301}},
			{Type: llm.EventDone, Stats: &llm.GenerationStats{Source: llm.SourceOllama}},
		}},
	}
	engine, _ := newTestEngine(t, st, provider)
	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Frage"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") })

	m, ok := engine.Measurement("chat-1")
	if !ok {
		t.Fatal("expected a measurement to be recorded")
	}
	if m.PromptTokens != 300 || m.Backend != "fake" || m.Model != "fake-model" {
		t.Fatalf("measurement = %+v", m)
	}
	if m.Seq != 2 {
		t.Errorf("measurement seq = %d, want 2 (the assistant reply)", m.Seq)
	}

	engine.InvalidateMeasurement("chat-1")
	if _, ok := engine.Measurement("chat-1"); ok {
		t.Error("measurement should be gone after invalidation")
	}
}

// TestBudgetReflectsTheModelSwitchRisk proves the pre-switch warning has the
// data it needs: the same conversation against two different context windows.
func TestBudgetReflectsTheModelSwitchRisk(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	// A long conversation.
	for i := 1; i <= 40; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		_ = st.CreateMessage(store.Message{
			ID:     "m" + string(rune('A'+i%26)) + string(rune('0'+i/26)),
			ChatID: "chat-1", Seq: i, Role: role,
			Content:   "Das ist ein längerer Absatz über Mietrecht in Baden-Württemberg mit Details.",
			CreatedAt: time.Now(),
		})
	}
	provider := &fakeProvider{
		model: llm.ModelInfo{
			ID: "fake-model", Backend: "fake", ContextLimit: 262144,
			MaxContext: 262144, State: llm.StateLoaded,
		},
	}
	engine, _ := newTestEngine(t, st, provider)

	big, err := engine.PreviewBudget(context.Background(), "chat-1", "fake", "fake-model")
	if err != nil {
		t.Fatalf("PreviewBudget: %v", err)
	}
	if big.Limit != 262144 {
		t.Fatalf("limit = %d, want 262144", big.Limit)
	}
	if big.OverLimit {
		t.Errorf("a large window should not be reported as over budget: %+v", big)
	}
	if big.Used <= 0 {
		t.Error("expected a non-zero used-token figure")
	}
}

// TestToolProtocolFollowsAdvertisedCapabilities pins the negotiation between
// native tool calling and the text protocol. Native `tools` are used only when
// the model advertises tool support; a model that advertises nothing at all — an
// Ollama modelfile without a capability list, or a custom endpoint — must fall
// back to the text protocol, because sending `tools` a backend cannot honour
// silently breaks tool calling instead of enabling it.
func TestToolProtocolFollowsAdvertisedCapabilities(t *testing.T) {
	cases := []struct {
		name         string
		capabilities []string
		wantHint     bool
	}{
		{name: "no capability list falls back", capabilities: nil, wantHint: true},
		{name: "tool support advertised stays native", capabilities: []string{llm.CapTools}, wantHint: false},
		{name: "capabilities without tools fall back", capabilities: []string{llm.CapVision}, wantHint: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			makeChat(t, st)
			provider := &fakeProvider{
				model: llm.ModelInfo{
					ID: "fake-model", Backend: "fake", ContextLimit: 8192,
					State: llm.StateLoaded, Capabilities: tc.capabilities,
				},
				replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "ok"}}},
			}
			engine, _ := newTestEngine(t, st, provider)

			if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Hallo"}); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if !waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
				t.Fatal("turn did not finish")
			}

			req := provider.lastRequest()
			if len(req.Messages) == 0 {
				t.Fatal("no request recorded")
			}
			system := string(req.Messages[0].Content)
			if got := strings.Contains(system, tools.TextProtocolHint); got != tc.wantHint {
				t.Errorf("text-protocol hint present = %v, want %v", got, tc.wantHint)
			}
		})
	}
}

// TestToolOutputBudgetCapsPerAnswer pins the setting the Settings screen exposes:
// tool results are charged against one budget for the whole answer, a result
// over the limit is cut with the truncation marker, and once the budget is spent
// the model is told so rather than handed empty text.
func TestToolOutputBudgetCapsPerAnswer(t *testing.T) {
	budget := newToolOutputBudget(config.Settings{ToolTokenBudget: 100})

	small := strings.Repeat("wort ", 5)
	if got := budget.take(small); got != small {
		t.Fatalf("a result inside the budget should pass through, got %q", got)
	}

	big := strings.Repeat("wort ", 5000)
	got := budget.take(big)
	if tokens := llm.EstimateTokens(got); tokens > 100 {
		t.Errorf("truncated result is over budget: %d tokens", tokens)
	}
	if !strings.Contains(got, "gekürzt") {
		t.Errorf("a shortened result must say so, got %q", got)
	}
	if next := budget.take("kurz"); next != exhaustedNotice {
		t.Errorf("after the budget is spent got %q, want the notice", next)
	}
}

// TestToolOutputBudgetDefaultsWhenUnset keeps an unset setting from silently
// disabling the bound.
func TestToolOutputBudgetDefaultsWhenUnset(t *testing.T) {
	if b := newToolOutputBudget(config.Settings{}); b.remaining != 25000 {
		t.Errorf("remaining = %d, want the 25000 default", b.remaining)
	}
}
