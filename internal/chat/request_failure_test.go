package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/llm"
	"kichat/internal/store"
)

// brokenProvider refuses every request before a single byte streams, which is what
// a backend that is down, a wrong port or a rejected key looks like from here.
type brokenProvider struct{ model llm.ModelInfo }

func (p *brokenProvider) ID() string   { return "fake" }
func (p *brokenProvider) Name() string { return "Broken" }
func (p *brokenProvider) Health(context.Context) error {
	return nil
}
func (p *brokenProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return []llm.ModelInfo{p.model}, nil
}
func (p *brokenProvider) Chat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	return nil, errors.New("Verbindung abgelehnt")
}

// TestARequestThatNeverStartedIsStoredAsFailed: a backend that refuses the request
// is a failure, not a cancellation. It used to be stored as "cancelled" — the
// status of a tool call the user abandoned — so the transcript read back as though
// the user had pressed Stop.
func TestARequestThatNeverStartedIsStoredAsFailed(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	provider := &brokenProvider{model: llm.ModelInfo{
		ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded,
	}}

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
		t.Fatal("no assistant row was written for the failed request")
	}
	if assistant.Status != store.StatusFailed {
		t.Errorf("status = %q, want %q", assistant.Status, store.StatusFailed)
	}
	if assistant.StopReason != "request_error" {
		t.Errorf("stop reason = %q, want request_error", assistant.StopReason)
	}

	sawError := false
	for _, ev := range log.snapshot() {
		if ev.name == events.Error {
			sawError = true
		}
		if ev.name == events.Done && ev.payload["status"] != store.StatusFailed {
			t.Errorf("the end-of-turn event said %v, want %q", ev.payload["status"], store.StatusFailed)
		}
	}
	if !sawError {
		t.Error("the failed request was never reported to the UI")
	}
}

// TestAFailedTurnIsNotReplayedAndTheChatStillSends is the refusal a Mistral chat
// hit: the failed request above leaves an assistant row with empty content and no
// calls, and replaying it is the one shape Mistral validates — HTTP 400, code 3240,
// "Assistant message must have either content or tool_calls, but not none". The
// refused turn writes another empty row, so the chat could never send again. The
// transcript keeps the failed attempt; the next request must not carry it.
func TestAFailedTurnIsNotReplayedAndTheChatStillSends(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	provider := &fakeProvider{
		model:    llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		refusals: []error{errors.New("400 Bad Request")},
		replies:  [][]llm.StreamEvent{nil, {{Type: llm.EventDelta, Text: "Antwort"}}},
	}
	engine, _ := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "erste Frage"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the failed turn did not finish")
	}
	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "zweite Frage"}); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the second turn did not finish")
	}

	for _, m := range provider.lastRequest().Messages {
		if m.Role == llm.RoleAssistant && m.Content == "" && len(m.ToolCalls) == 0 {
			t.Fatalf("the failed turn was replayed to the backend: %+v", m)
		}
	}

	// The row itself stays, so the transcript still shows what failed.
	messages, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	empty := 0
	for _, m := range messages {
		if m.Role == string(llm.RoleAssistant) && m.Content == "" {
			empty++
		}
	}
	if empty != 1 {
		t.Fatalf("stored empty assistant rows = %d, want the one failed attempt", empty)
	}
}
