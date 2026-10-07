package chat

import (
	"context"
	"testing"
	"time"

	"kichat/internal/llm"
	"kichat/internal/store"
)

// TestManualCompactWithoutHistoryIsNotAFailure is the regression test for the
// error the UI reported when pressing Compact: a manual run over a conversation
// that has no foldable range came back as "keine Kompaktierung erstellt", which the
// window showed as a failure. Today a manual run over an empty chat is the clearest
// such case: pressing Compact on a chat with no messages is an ordinary answer, so
// it comes back as Created=false with no error, and no summariser call is made for it.
func TestManualCompactWithoutHistoryIsNotAFailure(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
	}
	engine, _ := newTestEngine(t, st, provider)

	got, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true})
	if err != nil {
		t.Fatalf("compacting a conversation with nothing to fold returned an error: %v", err)
	}
	if got.Created {
		t.Errorf("Created = true, want false — an empty chat produced %q", got.Compaction.ID)
	}
	if got.Compaction.ID != "" {
		t.Errorf("Compaction = %+v, want the zero value when nothing was created", got.Compaction)
	}
	if n := provider.requestCount(); n != 0 {
		t.Errorf("the summariser was called %d times, want 0", n)
	}
}

// TestCompactDoesNotReportAnOlderCompactionAsNew is the second half of the same
// contract. The previous implementation read the newest stored compaction back
// after a run, so a run that folded nothing returned the *existing* compaction as
// its result — the caller could not tell "I compacted" from "there was nothing to
// do", and would close the panel as though it had worked.
//
// The new contract changes the short-chat case: a manual run over a conversation
// that is shorter than keepRecentTurns now folds the range that exists, so this
// test now models the genuinely-noop tail — the newest user turn is already the
// keep boundary, so even under the new behaviour nothing is left to fold.
func TestCompactDoesNotReportAnOlderCompactionAsNew(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	// Two user turns, both already covered by an earlier compaction.
	if err := st.CreateMessage(store.Message{
		ID: "m1", ChatID: "chat-1", Seq: 1, Role: "user",
		Content: "Hallo", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMessage(store.Message{
		ID: "m2", ChatID: "chat-1", Seq: 2, Role: "assistant",
		Content: "Hallo zurück.", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// The previous compaction already covered the first exchange, so fromSeq
	// starts after it.
	if err := st.CreateCompaction(store.Compaction{
		ID: "c-old", ChatID: "chat-1", Trigger: "manual",
		FromSeq: 1, ToSeq: 2, Summary: "Eine frühere Zusammenfassung.",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// The only uncovered messages are the newest user turn and its answer: even
	// under the always-fold rule there is nothing older left to cut.
	if err := st.CreateMessage(store.Message{
		ID: "m3", ChatID: "chat-1", Seq: 3, Role: "user",
		Content: "Noch eine Frage.", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMessage(store.Message{
		ID: "m4", ChatID: "chat-1", Seq: 4, Role: "assistant",
		Content: "Noch eine Antwort.", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
	}
	engine, _ := newTestEngine(t, st, provider)

	got, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	// This run should still be a no-op: the only uncovered user turn is the
	// keep boundary.
	if got.Created {
		t.Errorf("Created = true for a run whose only uncovered user turn is the keep boundary: %+v", got.Compaction)
	}
	if got.Compaction.ID != "" {
		t.Errorf("returned the pre-existing compaction %q as this run's result", got.Compaction.ID)
	}

	// The old compaction must survive untouched: reporting nothing is not the
	// same as deleting it.
	compactions, err := st.ListCompactions("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(compactions) != 1 || compactions[0].ID != "c-old" {
		t.Errorf("compactions = %+v, want just the pre-existing c-old", compactions)
	}
}
