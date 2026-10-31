package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"kichat/internal/chat"
	"kichat/internal/config"
	"kichat/internal/ingest"
	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/uimsg"
)

// usageProvider answers one turn and reports what it cost, which is what writes a
// measurement for the chat — the number the context meter shows.
type usageProvider struct{}

func (p *usageProvider) ID() string                   { return "usage" }
func (p *usageProvider) Name() string                 { return "Usage" }
func (p *usageProvider) Health(context.Context) error { return nil }

func (p *usageProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return []llm.ModelInfo{{ID: "usage-model", Backend: "usage", ContextLimit: 8192}}, nil
}

func (p *usageProvider) Chat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	events := make(chan llm.StreamEvent, 3)
	events <- llm.StreamEvent{Type: llm.EventDelta, Text: "Antwort"}
	events <- llm.StreamEvent{Type: llm.EventUsage,
		Usage: &llm.Usage{PromptTokens: 120, CompletionTokens: 2, TotalTokens: 122}}
	events <- llm.StreamEvent{Type: llm.EventDone,
		Stats: &llm.GenerationStats{StopReason: "stop"}}
	close(events)
	return events, nil
}

// clearTestApp is an App with a real store and engine, no window and no server.
func clearTestApp(t *testing.T) (*App, *store.Store) {
	t.Helper()
	settings := config.Default()
	settings.AutoTitle = false
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := newCleanupTestApp(t, settings, t.TempDir())
	a.store, a.extractor, a.turns = st, ingest.New(), chat.NewTurnState()
	a.rebuildServices(settings)
	return a, st
}

// clearTestProject creates the conversation this feature exists for: a project, so the
// chat row carries the settings a clear has to keep. It names the backend the calling test
// registered, because the engine resolves the provider by the chat's own pair and a chat
// pointing at a backend nobody serves never starts a turn.
func clearTestProject(t *testing.T, st *store.Store, id, backend, model string) {
	t.Helper()
	now := time.Now()
	if err := st.CreateChat(store.Chat{
		ID: id, Title: "Projekt", CreatedAt: now, UpdatedAt: now,
		Backend: backend, Model: model, SystemPrompt: "Du bist ein Coding-Agent.",
		Temperature: 0.7, TopP: 0.9,
		Mode: store.ModeCoding, Workspace: t.TempDir(),
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
}

// waitNotRunning waits for a chat's turn to end, so a test never asserts against a turn
// that is still writing.
func waitNotRunning(t *testing.T, e *chat.Engine, chatID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.IsRunning(chatID) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.IsRunning(chatID) {
		t.Fatalf("the turn in %s never finished", chatID)
	}
}

// TestClearingAChatForgetsEverythingItsConversationHeld covers what the binding adds over
// the store's own clear: the two pieces of state that live in memory. The measured prompt
// size describes messages that no longer exist, and "always allow changes in this chat" was
// an answer about a conversation that has ended — a permission that outlived it would still
// let a tool write to the folder unprompted.
func TestClearingAChatForgetsEverythingItsConversationHeld(t *testing.T) {
	a, st := clearTestApp(t)
	clearTestProject(t, st, "p1", "usage", "usage-model")
	a.currentEngine().Registry.Register(&usageProvider{})

	// A real turn, so there is a measurement to forget as well as messages.
	if err := a.currentEngine().Send(chat.TurnRequest{ChatID: "p1", Text: "Was steht hier?"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitNotRunning(t, a.currentEngine(), "p1")

	before, err := st.ListMessages("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("the turn wrote no messages, so this test would prove nothing")
	}
	if _, ok := a.currentEngine().Measurement("p1"); !ok {
		t.Fatal("the turn recorded no measurement, so the meter assertion below would prove nothing")
	}
	a.allowKind("p1", consentKindCommands)

	if err := a.ClearChat("p1"); err != nil {
		t.Fatalf("ClearChat: %v", err)
	}

	messages, err := st.ListMessages("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Errorf("%d messages survived the clear: %+v", len(messages), messages)
	}
	chat, err := st.GetChat("p1")
	if err != nil {
		t.Fatalf("the chat did not survive the clear: %v", err)
	}
	if !chat.IsProject() || chat.SystemPrompt == "" {
		t.Errorf("the cleared chat lost what makes it the project: %+v", chat)
	}
	if _, ok := a.currentEngine().Measurement("p1"); ok {
		t.Error("the cleared chat kept a measured prompt size for messages that are gone")
	}
	if a.kindAllowed("p1", consentKindCommands) {
		t.Error("the cleared chat kept a permission granted to the conversation that just ended")
	}
}

// TestClearingIsRefusedWhileTheChatIsAnswering pins the one state a clear must not accept.
// A running turn appends its reply and its tool results as it goes, so clearing underneath
// it would delete those messages and then watch new ones arrive — a chat that is neither
// cleared nor whole. The refusal carries a code rather than a sentence, so the window shows
// it in its own language.
func TestClearingIsRefusedWhileTheChatIsAnswering(t *testing.T) {
	a, st := clearTestApp(t)
	clearTestProject(t, st, "p1", "blocking", "blocking-model")
	provider := &blockingProvider{started: make(chan struct{})}
	a.currentEngine().Registry.Register(provider)

	if err := a.currentEngine().Send(chat.TurnRequest{ChatID: "p1", Text: "Frage"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-provider.started

	err := a.ClearChat("p1")
	if err == nil {
		t.Fatal("a clear during a running turn reported success")
	}
	message, ok := uimsg.Parse(err.Error())
	if !ok {
		t.Fatalf("the refusal carries no code, so the window would show raw text: %v", err)
	}
	if message.Code != "chat.turnRunning" {
		t.Errorf("the refusal names %q, want the same condition a second send is refused with", message.Code)
	}
	messages, listErr := st.ListMessages("p1")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(messages) == 0 {
		t.Error("the refused clear emptied the conversation anyway")
	}

	// Stop ends the turn, and the same command then works: the refusal is about the state
	// the chat is in, not about the command.
	if !a.currentEngine().Stop("p1") {
		t.Fatal("Stop did not reach the running turn")
	}
	waitNotRunning(t, a.currentEngine(), "p1")
	if err := a.ClearChat("p1"); err != nil {
		t.Fatalf("ClearChat after Stop: %v", err)
	}
}

// TestClearingAConversationThatIsGoneIsRefused keeps the answer honest: a window holding a
// chat id the database no longer knows must not be told the context was cleared, because
// nothing was.
func TestClearingAConversationThatIsGoneIsRefused(t *testing.T) {
	a, _ := clearTestApp(t)

	err := a.ClearChat("chat-that-never-existed")
	if err == nil {
		t.Fatal("clearing a chat that does not exist reported success")
	}
	message, ok := uimsg.Parse(err.Error())
	if !ok {
		t.Fatalf("the refusal carries no code: %v", err)
	}
	if message.Code != "chat.gone" {
		t.Errorf("the refusal names %q, want chat.gone", message.Code)
	}
}
