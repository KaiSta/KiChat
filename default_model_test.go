package main

import (
	"context"
	"path/filepath"
	"testing"

	"kichat/internal/chat"
	"kichat/internal/config"
	"kichat/internal/ingest"
	"kichat/internal/llm"
	"kichat/internal/store"
)

// stubProvider is a backend a test can shape: a fixed catalogue and no server.
type stubProvider struct{ models []llm.ModelInfo }

func (p *stubProvider) ID() string                                          { return "stub" }
func (p *stubProvider) Name() string                                        { return "Stub" }
func (p *stubProvider) Health(context.Context) error                        { return nil }
func (p *stubProvider) ListModels(context.Context) ([]llm.ModelInfo, error) { return p.models, nil }
func (p *stubProvider) Chat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	events := make(chan llm.StreamEvent)
	close(events)
	return events, nil
}

// newDefaultModelTestApp is an App whose catalogue is the given models and whose
// settings name `preferred` as the default for a new chat.
func newDefaultModelTestApp(t *testing.T, models []llm.ModelInfo, preferred llm.Ref) *App {
	t.Helper()
	settings := config.Default()
	settings.DefaultModel = preferred
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := newCleanupTestApp(t, settings, t.TempDir())
	a.store, a.extractor, a.turns = st, ingest.New(), chat.NewTurnState()
	a.rebuildServices(settings)
	a.currentEngine().Registry.Register(&stubProvider{models: models})
	return a
}

// The catalogue the two tests below share: one loaded model and one small one that
// is not loaded, so "preferred" and "loaded" are distinguishable answers.
func defaultModelCatalogue() []llm.ModelInfo {
	return []llm.ModelInfo{
		{ID: "big", Backend: "stub", ContextLimit: 8192, State: llm.StateLoaded},
		{ID: "small", Backend: "stub", ContextLimit: 4096},
	}
}

// TestANewChatStartsOnTheConfiguredDefaultModel pins the setting's whole point: the
// model chosen in Settings is what a new chat opens on, even when another model is
// already loaded and would otherwise win. It also pins the two boundaries — an
// explicit pair from the caller still wins, and an unset default keeps the old
// "first loaded, else first listed" behaviour.
func TestANewChatStartsOnTheConfiguredDefaultModel(t *testing.T) {
	a := newDefaultModelTestApp(t, defaultModelCatalogue(), llm.Ref{Backend: "stub", Model: "small"})

	created, err := a.CreateChat("", "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if created.Backend != "stub" || created.Model != "small" {
		t.Errorf("a new chat started on %s/%s, want the configured stub/small even though big is loaded",
			created.Backend, created.Model)
	}

	// The picker's own "new chat with this model" path passes a pair, and that has to
	// keep working: the default is what happens when nobody chooses.
	explicit, err := a.CreateChat("stub", "big")
	if err != nil {
		t.Fatalf("CreateChat with an explicit model: %v", err)
	}
	if explicit.Model != "big" {
		t.Errorf("an explicit model was replaced by the default: got %q, want big", explicit.Model)
	}

	// Unset: the behaviour every installation predating the field has.
	legacy := newDefaultModelTestApp(t, defaultModelCatalogue(), llm.Ref{})
	created, err = legacy.CreateChat("", "")
	if err != nil {
		t.Fatalf("CreateChat without a default: %v", err)
	}
	if created.Model != "big" {
		t.Errorf("with no default a new chat started on %q, want the loaded big", created.Model)
	}
}

// TestAConfiguredModelThatVanishedFallsBack pins the repair for a model that is no
// longer served — its backend switched off, its server stopped publishing it, or the
// availability check hiding it. The chat must open on something that answers rather
// than on a name the picker does not list.
func TestAConfiguredModelThatVanishedFallsBack(t *testing.T) {
	a := newDefaultModelTestApp(t, defaultModelCatalogue(), llm.Ref{Backend: "stub", Model: "gone"})

	created, err := a.CreateChat("", "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if created.Model != "big" {
		t.Errorf("a new chat started on %q, want the loaded big after the configured gone was not listed",
			created.Model)
	}
}

// countingProvider is a backend that counts how often its catalogue was asked for, which
// is what makes "opening a chat does not list the backends" checkable instead of a claim
// about code nobody exercises.
type countingProvider struct {
	models []llm.ModelInfo
	calls  int
}

func (p *countingProvider) ID() string                   { return "stub" }
func (p *countingProvider) Name() string                 { return "Stub" }
func (p *countingProvider) Health(context.Context) error { return nil }
func (p *countingProvider) Chat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	events := make(chan llm.StreamEvent)
	close(events)
	return events, nil
}

func (p *countingProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	p.calls++
	return p.models, nil
}

// TestANewChatDoesNotAskTheBackendsAgain pins what starting a chat costs. The default
// model is resolved against the catalogue the process already holds: a fresh listing
// waits on every configured backend, and pressing "new chat" used to run one as soon as
// the registry's thirty-second window had passed.
func TestANewChatDoesNotAskTheBackendsAgain(t *testing.T) {
	a := newDefaultModelTestApp(t, defaultModelCatalogue(), llm.Ref{Backend: "stub", Model: "small"})
	// Registered after the app is built, so the catalogue starts empty and the first chat
	// is the one listing that is still worth asking for.
	counted := &countingProvider{models: defaultModelCatalogue()}
	a.currentEngine().Registry.Register(counted)

	first, err := a.CreateChat("", "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if first.Model != "small" {
		t.Errorf("the first chat started on %q, want the configured stub/small", first.Model)
	}
	if counted.calls != 1 {
		t.Fatalf("%d listings for the first chat, want the one an empty catalogue needs", counted.calls)
	}

	for i := 0; i < 3; i++ {
		next, err := a.CreateChat("", "")
		if err != nil {
			t.Fatalf("CreateChat %d: %v", i, err)
		}
		if next.Model != "small" {
			t.Errorf("chat %d started on %q, want the configured stub/small", i, next.Model)
		}
	}
	if counted.calls != 1 {
		t.Errorf("the backends were listed %d times for four chats, want the catalogue in hand to answer it",
			counted.calls)
	}
}
