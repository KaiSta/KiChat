package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"kichat/internal/chat"
	"kichat/internal/config"
	"kichat/internal/ingest"
	"kichat/internal/llm"
	"kichat/internal/store"
)

// blockingProvider streams one delta and then waits for the turn to be cancelled,
// which is how a Stop button feels to the engine.
type blockingProvider struct {
	started chan struct{}
	once    sync.Once
}

func (p *blockingProvider) ID() string   { return "blocking" }
func (p *blockingProvider) Name() string { return "Blocking" }
func (p *blockingProvider) Health(context.Context) error {
	return nil
}
func (p *blockingProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return []llm.ModelInfo{{
		ID: "blocking-model", Backend: "blocking", ContextLimit: 4096, State: llm.StateLoaded,
	}}, nil
}
func (p *blockingProvider) Chat(ctx context.Context, _ llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.once.Do(func() { close(p.started) })
	events := make(chan llm.StreamEvent, 1)
	events <- llm.StreamEvent{Type: llm.EventDelta, Text: "Ich schreibe …"}
	go func() {
		defer close(events)
		<-ctx.Done()
	}()
	return events, nil
}

// TestRebuildingServicesKeepsTheRunningTurn pins the wiring in rebuildServices: the
// turn state belongs to the App, not to the engine a settings save replaces. Without
// it the reply kept streaming from an engine the bound methods could no longer see,
// so Stop did nothing and a second message started a second generation in the same
// chat — and the theme and language switches save settings, so this was two clicks
// away from any running reply.
func TestRebuildingServicesKeepsTheRunningTurn(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	now := time.Now()
	if err := st.CreateChat(store.Chat{
		ID: "c1", Title: "Test", CreatedAt: now, UpdatedAt: now,
		Backend: "blocking", Model: "blocking-model", Temperature: 0.7, TopP: 0.9,
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}

	settings := config.Default()
	settings.AutoTitle = false

	a := &App{store: st, extractor: ingest.New(), turns: chat.NewTurnState()}
	a.settingsMu.Lock()
	a.settings = settings
	a.settingsMu.Unlock()
	a.rebuildServices(settings)

	first := a.currentEngine()
	provider := &blockingProvider{started: make(chan struct{})}
	first.Registry.Register(provider)

	if err := first.Send(chat.TurnRequest{ChatID: "c1", Text: "Frage"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the generation never started")
	}

	// The settings save: a new engine is installed over the running one.
	a.rebuildServices(settings)
	second := a.currentEngine()
	if second == first {
		t.Fatal("rebuildServices did not install a new engine")
	}
	if !second.IsRunning("c1") {
		t.Fatal("the rebuilt engine does not see the running turn")
	}
	// A second message waits behind the inherited turn — the queue, like the running turn
	// itself, is state the rebuild must not lose.
	if err := second.Send(chat.TurnRequest{ChatID: "c1", Text: "zweite Frage"}); err != nil {
		t.Errorf("Send after the rebuild: %v", err)
	}
	if got := second.Queued("c1"); len(got) != 1 || got[0].Text != "zweite Frage" {
		t.Errorf("Queued = %+v, want the message waiting behind the inherited turn", got)
	}
	// Dropped by hand: Stop leaves the queue alone, and this provider blocks for ever, so the
	// message would otherwise start a turn that nothing here is waiting for.
	if waiting := second.Queued("c1"); !second.DropQueued("c1", waiting[0].ID) {
		t.Fatal("DropQueued did not find the message it had just listed")
	}
	if !second.Stop("c1") {
		t.Fatal("Stop did not reach the turn the rebuild inherited")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && second.IsRunning("c1") {
		time.Sleep(10 * time.Millisecond)
	}
	if second.IsRunning("c1") {
		t.Fatal("the inherited turn did not finish after Stop")
	}
}
