package chat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
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
// A manual run now always folds when any unfolded message is left (see
// TestManualCompactAlwaysFoldsWhenOnlyTheNewestTurnIsLeft), so the genuinely
// empty case is a conversation whose whole history is already covered: there is
// nothing to fold, and the run must still report that as nothing rather than
// handing back the old compaction.
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
	// The previous compaction covers the whole history, so there is no unfolded
	// message left for a manual run to fold.
	if err := st.CreateCompaction(store.Compaction{
		ID: "c-old", ChatID: "chat-1", Trigger: "manual",
		FromSeq: 1, ToSeq: 2, Summary: "Eine frühere Zusammenfassung.",
		CreatedAt: time.Now(),
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
	if got.Created {
		t.Errorf("Created = true for a fully folded history: %+v", got.Compaction)
	}
	if got.Compaction.ID != "" {
		t.Errorf("returned the pre-existing compaction %q as this run's result", got.Compaction.ID)
	}
	if n := provider.requestCount(); n != 0 {
		t.Errorf("the summariser was called %d times for a fully folded history, want 0", n)
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

// TestCompactRefusesWhileATurnIsRunning is the guard for a race the planner
// cannot see: a turn writes its assistant row token by token, so compacting while
// it runs would summarise half an answer and freeze that fragment into memory.
// The manual entry point refuses; the automatic one is triggered by the turn
// itself and is unaffected.
func TestCompactRefusesWhileATurnIsRunning(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	if err := st.CreateMessage(store.Message{
		ID: "m1", ChatID: "chat-1", Seq: 1, Role: "user",
		Content: "Hallo", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	provider := &fakeProvider{
		model:      llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		blockAfter: 1,
		replies: [][]llm.StreamEvent{{
			{Type: llm.EventDelta, Text: "Ich schreibe …"},
			{Type: llm.EventDelta, Text: "nie gesendet."},
		}},
	}
	provider.started = make(chan struct{})
	engine, _ := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Frage"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("generation never started")
	}
	if !waitFor(t, 2*time.Second, func() bool { return engine.IsRunning("chat-1") }) {
		t.Fatal("engine did not report a running turn")
	}

	if _, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true}); err == nil {
		t.Error("compacting while a turn was running should be refused")
	}
	compactions, err := st.ListCompactions("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(compactions) != 0 {
		t.Errorf("a refused run still wrote a compaction: %+v", compactions)
	}

	engine.Stop("chat-1")
	if !waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("turn did not finish after Stop")
	}
}

// TestPlanCompactionNeverFoldsAStreamingMessage is the second half of the same
// guard, at the planner: even when the caller does not check, a message that is
// still being written is never part of a cut.
func TestPlanCompactionNeverFoldsAStreamingMessage(t *testing.T) {
	messages := []store.Message{
		msg(1, "user", "Erste Frage"),
		msg(2, "assistant", "Erste Antwort"),
		{ID: "m3", Seq: 3, Role: "user", Content: "Zweite Frage", Status: store.StatusComplete},
		{ID: "m4", Seq: 4, Role: "assistant", Content: "Halbe Ant", Status: store.StatusStreaming},
	}
	plan := PlanCompaction(PlanInput{
		Messages:  messages,
		Budget:    ContextBudget{Limit: 10000, ProjectedRatio: 0.99},
		Threshold: 0.80, KeepRecentTurns: 6, Reserve: 1024, Force: true,
	})
	if !plan.Needed {
		t.Fatalf("the finished turns before the streaming one should still be foldable: %+v", plan)
	}
	// The streaming reply is seq 4 and its question is seq 3: nothing from that
	// turn may be folded, and the tail has to start at the question.
	if plan.ToSeq >= 3 || plan.KeepFromSeq != 3 {
		t.Fatalf("cut = [%d,%d] keep %d reached the turn being written",
			plan.FromSeq, plan.ToSeq, plan.KeepFromSeq)
	}

	// With nothing finished older than the streaming reply, there is no cut at all.
	onlyStreaming := PlanCompaction(PlanInput{
		Messages:  []store.Message{{ID: "m1", Seq: 1, Role: "assistant", Content: "halb", Status: store.StatusStreaming}},
		Budget:    ContextBudget{Limit: 10000, ProjectedRatio: 0.99},
		Threshold: 0.80, KeepRecentTurns: 6, Reserve: 1024, Force: true,
	})
	if onlyStreaming.Needed {
		t.Errorf("a streaming-only conversation must not be folded: %+v", onlyStreaming)
	}
}

// TestManualCompactAlwaysFoldsWhenOnlyTheNewestTurnIsLeft pins the button's
// promise: a manual compaction always compacts. The state right after a previous
// compaction plus one new turn used to produce nothing at all — every older turn
// was already folded and the planner kept the newest turn verbatim — so pressing
// Compact reported "nothing to compact" while the new turn (including a seeded
// search payload) was still filling the window. A manual run now extends the cut
// to the end of the chat in that case.
// TestCompactKeepsTheSummariserRequestInsideItsWindow is why the transcript
// budget is derived from the window at all.
//
// The cap it replaced was per block, so nothing bounded the transcript as a
// whole: a conversation far larger than a small model's window arrived as a
// single request that model could not hold, and the backend had to reject it or
// truncate it silently. Here a fold of a conversation many times the 8k window is
// checked against the window itself, which is the property that was missing.
func TestCompactKeepsTheSummariserRequestInsideItsWindow(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	// Each message is on its own larger than the whole window, and there are
	// twelve turns of them.
	big := strings.Repeat("Ein langer Absatz mit Inhalt. ", 1000)
	add := func(id string, seq int, role, content string) {
		t.Helper()
		if err := st.CreateMessage(store.Message{
			ID: id, ChatID: "chat-1", Seq: seq, Role: role, Content: content,
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 12; i++ {
		add(fmt.Sprintf("u%d", i), i*2-1, "user", fmt.Sprintf("Frage %d %s", i, big))
		add(fmt.Sprintf("a%d", i), i*2, "assistant", fmt.Sprintf("Antwort %d %s", i, big))
	}

	const window = 8192
	provider := &fakeProvider{
		model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: window, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "Gedächtnis."}}},
	}
	engine, _ := newTestEngine(t, st, provider)

	got, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !got.Created {
		t.Fatal("nothing was folded")
	}

	req := provider.lastRequest()
	if len(req.Messages) == 0 {
		t.Fatal("the summariser was never called")
	}
	used := llm.EstimateMessagesTokens(req.Messages)
	if used > window {
		t.Fatalf("the summariser request estimates at %d tokens for a %d-token window", used, window)
	}
	// The transcript is the part that had to give way — the request fits because
	// it was cut, not because the conversation happened to be small.
	if body := req.Messages[len(req.Messages)-1].Content; !strings.Contains(body, "gekürzt") {
		t.Errorf("the transcript was not cut to fit the window (%d tokens used):\n%s", used, body)
	}
}

func TestManualCompactAlwaysFoldsWhenOnlyTheNewestTurnIsLeft(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	add := func(id string, seq int, role, content string) {
		t.Helper()
		if err := st.CreateMessage(store.Message{
			ID: id, ChatID: "chat-1", Seq: seq, Role: role, Content: content,
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("m1", 1, "user", "Erste Frage")
	add("m2", 2, "assistant", "Erste Antwort")
	if err := st.CreateCompaction(store.Compaction{
		ID: "c-old", ChatID: "chat-1", Trigger: "auto", FromSeq: 1, ToSeq: 2,
		Summary: "Die erste Runde.", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// One new turn since the compaction, with the seeded search payload that
	// makes it worth folding.
	add("m3", 3, "user", "Zweite Frage")
	add("m4", 4, "system", "<search>Seiteninhalt</search>")
	add("m5", 5, "assistant", "Zweite Antwort")

	provider := &fakeProvider{
		model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "Zweite Runde."}}},
	}
	engine, _ := newTestEngine(t, st, provider)

	got, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !got.Created {
		t.Fatalf("a manual compaction over one unfolded turn did nothing")
	}
	if got.Compaction.Trigger != "manual" {
		t.Errorf("trigger = %q, want manual", got.Compaction.Trigger)
	}
	// The whole unfolded tail is folded, newest turn included.
	if got.Compaction.FromSeq != 3 || got.Compaction.ToSeq != 5 {
		t.Errorf("folded %d..%d, want 3..5", got.Compaction.FromSeq, got.Compaction.ToSeq)
	}
	// The new summary continues the old one instead of starting over.
	if req := provider.lastRequest(); len(req.Messages) == 0 ||
		!strings.Contains(req.Messages[len(req.Messages)-1].Content, "Die erste Runde.") {
		t.Errorf("summariser did not receive the previous summary: %+v", req.Messages)
	}

	// The context really shrinks: nothing is left in the verbatim history.
	chat, err := st.GetChat("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	assembly, _, err := engine.buildAssembly(context.Background(), &chat, "fake", "fake-model",
		config.Default(), llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192}, "")
	if err != nil {
		t.Fatal(err)
	}
	if assembly.FirstVerbatimSeq != 6 {
		t.Errorf("FirstVerbatimSeq = %d, want 6 — the folded tail must leave the prompt", assembly.FirstVerbatimSeq)
	}
	if assembly.Budget.History != 0 {
		t.Errorf("history still costs %d tokens after a full fold", assembly.Budget.History)
	}
}
