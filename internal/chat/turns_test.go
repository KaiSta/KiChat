package chat

import (
	"context"
	"sync"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/llm"
)

// TestARebuiltEngineKeepsTheRunningTurn is the regression test for the reply that
// used to be orphaned: the app replaces its engine whenever settings are saved
// (a theme or language switch saves them) or a managed server comes up, and the
// new engine knew nothing about the turn the old one was streaming. Stop then found
// no handle and did nothing, and a second message started a second generation in
// the same chat.
func TestARebuiltEngineKeepsTheRunningTurn(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	provider := &fakeProvider{
		model:      llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		blockAfter: 1,
		replies: [][]llm.StreamEvent{{
			{Type: llm.EventDelta, Text: "Ich schreibe …"},
			{Type: llm.EventDelta, Text: "nie gesendet."},
		}},
	}
	provider.started = make(chan struct{})

	settings := config.Default()
	settings.AutoTitle = false
	registry := llm.NewRegistry(nil)
	registry.Register(provider)
	log := &eventLog{}
	// The one value the app keeps across a rebuild.
	state := NewTurnState()
	build := func() *Engine {
		return NewEngineWithState(st, registry, nil,
			func() config.Settings { return settings }, log.emit, state)
	}

	first := build()
	if err := first.Send(TurnRequest{ChatID: "chat-1", Text: "Frage"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the generation never started")
	}

	// The rebuild: a new engine over the running one, exactly as a settings save
	// does it.
	second := build()

	if !second.IsRunning("chat-1") {
		t.Fatal("the rebuilt engine does not see the running turn")
	}
	if got := second.ActiveChats(); len(got) != 1 || got[0] != "chat-1" {
		t.Errorf("ActiveChats = %v, want [chat-1]", got)
	}
	// A second message waits behind the running turn now, and the queue is part of the same
	// shared state: the rebuilt engine has to see the message the old one would have taken.
	if err := second.Send(TurnRequest{ChatID: "chat-1", Text: "zweite Frage"}); err != nil {
		t.Errorf("Send while a turn runs: %v", err)
	}
	if got := second.Queued("chat-1"); len(got) != 1 || got[0].Text != "zweite Frage" {
		t.Errorf("Queued = %+v, want the message waiting behind the running turn", got)
	}
	// Taken back out by the user: Stop deliberately leaves the queue alone, and this test is
	// about the turn the rebuild inherited rather than about what a queue does afterwards.
	if waiting := second.Queued("chat-1"); !second.DropQueued("chat-1", waiting[0].ID) {
		t.Fatal("DropQueued did not find the message it had just listed")
	}
	if !second.Stop("chat-1") {
		t.Fatal("Stop on the rebuilt engine did not reach the turn it inherited")
	}
	if !waitFor(t, 3*time.Second, func() bool { return !second.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish after Stop")
	}
}

// TestARunningCompactionIsStoppableAndBlocksATurn: a manual compaction used to
// check IsRunning and then run unregistered, so Stop could not cancel it and a
// message sent during one was appended to the history being folded. It claims the
// chat now, like any other turn.
func TestARunningCompactionIsStoppableAndBlocksATurn(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedTurns(t, st)

	provider := &fakeProvider{
		model:      llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		blockAfter: 1,
		replies: [][]llm.StreamEvent{{
			{Type: llm.EventDelta, Text: "## Festgestellte Fakten"},
			{Type: llm.EventDelta, Text: "- nie gesendet"},
		}},
	}
	provider.started = make(chan struct{})
	engine, _ := newTestEngine(t, st, provider)

	done := make(chan error, 1)
	go func() {
		_, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true})
		done <- err
	}()

	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the summarisation never reached the backend")
	}
	if !engine.IsRunning("chat-1") {
		t.Fatal("a running compaction is not reported as a turn")
	}
	// A compaction claims the chat like any other turn, so the message waits behind it rather
	// than starting beside it. It is dropped by hand below, because Stop leaves a queue alone.
	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "dazwischen"}); err != nil {
		t.Errorf("Send beside a running compaction: %v", err)
	}
	if got := engine.Queued("chat-1"); len(got) != 1 {
		t.Errorf("Queued = %+v, want the message waiting for the compaction", got)
	}
	// Dropped by hand, because Stop leaves the queue alone: this test is about the compaction.
	if waiting := engine.Queued("chat-1"); !engine.DropQueued("chat-1", waiting[0].ID) {
		t.Fatal("DropQueued did not find the message it had just listed")
	}
	if !engine.Stop("chat-1") {
		t.Fatal("Stop did not find the running compaction")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled compaction reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the compaction was not cancelled by Stop")
	}
	if engine.IsRunning("chat-1") {
		t.Error("the chat is still claimed after the compaction returned")
	}
	if got := engine.Queued("chat-1"); len(got) != 0 {
		t.Errorf("the queue still holds %d message(s) after the compaction was stopped", len(got))
	}
	compactions, err := st.ListCompactions("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(compactions) != 0 {
		t.Errorf("a cancelled compaction still wrote a row: %+v", compactions)
	}
}

// heldProvider holds the first request open until the test releases it, and answers every later
// one at once. That is the shape a chat has while the user types the next message: an answer is
// being written, and the second question arrives before it ends.
type heldProvider struct {
	*fakeProvider
	release chan struct{}
	once    sync.Once
}

func (h *heldProvider) Chat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	h.mu.Lock()
	first := h.calls == 0
	h.mu.Unlock()
	if first {
		<-h.release
	}
	return h.fakeProvider.Chat(ctx, req)
}

// open releases the held request once, however often the test asks: a turn left waiting would
// hold the store open past the end of the test.
func (h *heldProvider) open() { h.once.Do(func() { close(h.release) }) }

// TestAQueuedMessageIsSentWhenTheRunningTurnEnds is the whole point of the queue: a message
// typed while an answer streams is kept, shown, and sent afterwards — never refused, and never
// written into the history of the answer that was already being written.
func TestAQueuedMessageIsSentWhenTheRunningTurnEnds(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	provider := &heldProvider{
		fakeProvider: &fakeProvider{
			model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
			replies: [][]llm.StreamEvent{
				{{Type: llm.EventDelta, Text: "Erste Antwort"}, {Type: llm.EventDone}},
				{{Type: llm.EventDelta, Text: "Zweite Antwort"}, {Type: llm.EventDone}},
			},
		},
		release: make(chan struct{}),
	}
	engine, _ := newTestEngine(t, st, provider)
	t.Cleanup(provider.open)

	userMessages := func() []string {
		messages, err := st.ListMessages("chat-1")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range messages {
			if m.Role == string(llm.RoleUser) {
				out = append(out, m.Content)
			}
		}
		return out
	}

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "erste Frage"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return engine.IsRunning("chat-1") }) {
		t.Fatal("the first turn never claimed the chat")
	}

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "zweite Frage"}); err != nil {
		t.Fatalf("Send while the first turn runs: %v", err)
	}
	queued := engine.Queued("chat-1")
	if len(queued) != 1 || queued[0].Text != "zweite Frage" {
		t.Fatalf("Queued = %+v, want one message waiting", queued)
	}
	if queued[0].AttachmentIDs == nil {
		t.Error("the queued entry carries a null attachment list; the window maps over it")
	}
	// The waiting message is not in the transcript yet: its turn stores it when it starts, so the
	// reply being written is answered by the history that was there before it.
	if got := userMessages(); len(got) != 1 {
		t.Errorf("the transcript already holds %v, want only the first question", got)
	}

	provider.open()
	if !waitFor(t, 5*time.Second, func() bool {
		return len(engine.Queued("chat-1")) == 0 && !engine.IsRunning("chat-1")
	}) {
		t.Fatal("the queued message was never sent")
	}
	if got := provider.requestCount(); got != 2 {
		t.Errorf("the backend was asked %d time(s), want 2 — the queued message never ran", got)
	}
	if got := userMessages(); len(got) != 2 || got[1] != "zweite Frage" {
		t.Errorf("transcript holds %v, want both questions in order", got)
	}
}

// TestStopLeavesTheQueueInPlace pins what Stop does now: it ends the running answer and nothing
// else. The message the user queued is still theirs, so it starts the moment the chat is free —
// and the window says so, because a new answer after a click that read as "stop everything"
// would otherwise be unexplained.
func TestStopLeavesTheQueueInPlace(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	provider := &fakeProvider{
		model:      llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		blockAfter: 1,
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: "Anfang"}, {Type: llm.EventDelta, Text: "nie zu Ende"}},
			{{Type: llm.EventDelta, Text: "Zweite Antwort"}},
		},
	}
	engine, _ := newTestEngine(t, st, provider)
	// The second turn blocks too, so it is cancelled here rather than left holding the store.
	t.Cleanup(func() {
		engine.Stop("chat-1")
		waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") })
	})

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "erste Frage"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return provider.requestCount() == 1 }) {
		t.Fatal("the first turn never reached the backend")
	}
	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "zweite Frage"}); err != nil {
		t.Fatalf("Send while the first turn runs: %v", err)
	}

	if !engine.Stop("chat-1") {
		t.Fatal("Stop did not find the running turn")
	}
	// The queued text was not dropped: it is the message the next turn runs on, so it has been
	// taken up rather than left behind — from a chat that never had two turns at once.
	if !waitFor(t, 3*time.Second, func() bool { return provider.requestCount() == 2 }) {
		t.Fatal("the queued message did not start after Stop")
	}
	if got := engine.Queued("chat-1"); len(got) != 0 {
		t.Errorf("Queued = %+v, want the message taken up rather than left behind", got)
	}
	messages, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	for _, m := range messages {
		if m.Role == string(llm.RoleUser) {
			asked = append(asked, m.Content)
		}
	}
	if len(asked) != 2 || asked[1] != "zweite Frage" {
		t.Errorf("the transcript holds %v, want both questions", asked)
	}
}
