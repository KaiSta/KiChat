package chat

import (
	"context"
	"sync"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/llm"
)

// throttlingProvider is a backend whose request has to wait: it reports a wait on the
// watcher its request carries — in the same place the shared HTTP client reports one,
// and with the same kind of error — and only then streams a reply.
//
// It is a provider rather than an HTTP server because what this file pins is the
// engine's half of the chain: that the turn's request carries a watcher at all, and
// that what the watcher is told becomes a phase on the right chat. The client's half
// — the wait reported, its length, the retraction — is pinned against a real server
// in internal/llm.
type throttlingProvider struct {
	model llm.ModelInfo
	wait  time.Duration

	mu      sync.Mutex
	watcher llm.RetryWatcher
}

func (p *throttlingProvider) ID() string                   { return "fake" }
func (p *throttlingProvider) Name() string                 { return "Throttling" }
func (p *throttlingProvider) Health(context.Context) error { return nil }
func (p *throttlingProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return []llm.ModelInfo{p.model}, nil
}

func (p *throttlingProvider) Chat(ctx context.Context, _ llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	watcher := llm.RetryWatcherOf(ctx)
	p.mu.Lock()
	p.watcher = watcher
	p.mu.Unlock()

	if watcher != nil {
		watcher.Waiting(p.wait, &llm.HTTPError{
			StatusCode: 429,
			Body:       `{"error":{"code":"rate_limit_exceeded","message":"Please try again in 9.1s."}}`,
		})
		watcher.Waited()
	}

	// Buffered and then closed: every scripted event survives a Stop that arrives at
	// the same moment, which is the reason the other fake in this package does it too.
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Type: llm.EventDelta, Text: "Antwort"}
	close(ch)
	return ch, nil
}

// carriedWatcher returns the watcher the request was given, read under the lock
// because the turn ran on its own goroutine.
func (p *throttlingProvider) carriedWatcher() llm.RetryWatcher {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.watcher
}

// TestAThrottledWaitIsShownAsAPhaseOnTheChat is the reason the phase exists: a retry
// that obeys a backend's own rate limit can sleep for most of a minute, and without
// this a turn that is merely waiting looks exactly like a turn that has hung.
//
// Three things are pinned: the request carries a watcher, the wait is published as a
// phase carrying the length the backend named, and the phase is retracted — so the
// panel says why the turn is slow rather than leaving something standing after it.
func TestAThrottledWaitIsShownAsAPhaseOnTheChat(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	provider := &throttlingProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		wait:  9123 * time.Millisecond,
	}

	settings := config.Default()
	settings.AutoTitle = false
	registry := llm.NewRegistry(nil)
	registry.Register(provider)
	log := &eventLog{}
	engine := NewEngine(st, registry, nil, func() config.Settings { return settings }, log.emit)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Frage"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	if provider.carriedWatcher() == nil {
		t.Fatal("the turn's request carried no watcher, so a wait could not be shown at all")
	}

	var statuses []map[string]any
	for _, ev := range log.snapshot() {
		if ev.name == events.Status {
			statuses = append(statuses, ev.payload)
		}
	}
	if len(statuses) < 2 {
		t.Fatalf("%d status events were emitted, want the wait and its retraction: %v", len(statuses), statuses)
	}

	// The chat is part of the payload, because a phase belongs to one conversation and
	// the store keeps them per chat.
	if got := statuses[0]["chatId"]; got != "chat-1" {
		t.Errorf("the wait was reported on chat %v, want chat-1", got)
	}
	if got := statuses[0]["phase"]; got != throttledPhase {
		t.Errorf("the first phase of the turn was %v, want %q", got, throttledPhase)
	}
	// The length travels with it: "waiting" alone does not say whether this is a second
	// or a minute, and the number is the one fact that makes the wait legible.
	if got, _ := statuses[0]["waitMs"].(int64); got != 9123 {
		t.Errorf("the wait shown was %v ms, want 9123", statuses[0]["waitMs"])
	}
	if got := statuses[1]["phase"]; got != "idle" {
		t.Errorf("the phase after the wait was %v, want idle — the wait is left standing", got)
	}
	// Nothing may still be claiming a wait once the turn is over, whatever the last
	// status event is.
	if got := statuses[len(statuses)-1]["phase"]; got != "idle" {
		t.Errorf("the turn ended in phase %v, want idle", got)
	}

	// And the turn went on to answer: a wait in the middle of a turn is not the same
	// thing as a turn that produced nothing.
	messages, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	answered := false
	for i := range messages {
		if messages[i].Role == string(llm.RoleAssistant) && messages[i].Content != "" {
			answered = true
		}
	}
	if !answered {
		t.Error("the reply did not arrive after the reported wait")
	}
}
