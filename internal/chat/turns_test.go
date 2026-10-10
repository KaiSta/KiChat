package chat

import (
	"context"
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
	if err := second.Send(TurnRequest{ChatID: "chat-1", Text: "zweite Frage"}); err == nil {
		t.Error("the rebuilt engine accepted a second turn for a chat that was already generating")
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
	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "dazwischen"}); err == nil {
		t.Error("a turn started beside a running compaction")
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
	compactions, err := st.ListCompactions("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(compactions) != 0 {
		t.Errorf("a cancelled compaction still wrote a row: %+v", compactions)
	}
}
