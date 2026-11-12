package chat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/uimsg"
)

// TestAutoCompactStaysQuietWhenTheWindowIsUnknown pins the difference between a
// model the backend cannot describe and an incident.
//
// The automatic run happens before every send, so anything it forwards is a
// warning the user meets once per message. That was the default state of the
// NVIDIA backend: no route of that API reports a context window, so every model
// arrives with none, the threshold — a ratio of that window — cannot be
// evaluated, and the refusal put the same toast in front of the user on every
// turn of the chat. The backend was working; the panel said otherwise, repeatedly.
//
// The refusal itself is real and is not what changed. What this pins is that the
// *automatic* path does not carry it, because an automatic run has nobody to
// report back to and the condition is already visible where it belongs: the
// context meter reads "unknown" and the inspector names the source of the number.
func TestAutoCompactStaysQuietWhenTheWindowIsUnknown(t *testing.T) {
	st := newTestStore(t)
	chat := makeChat(t, st)
	chat.AutoCompact = true
	if err := st.UpdateChat(chat); err != nil {
		t.Fatalf("enable auto-compaction: %v", err)
	}

	provider := &fakeProvider{
		// No window at all, which is what every model of a backend whose listing
		// reports none looks like.
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{{
			{Type: llm.EventDelta, Text: "Antwort"},
			{Type: llm.EventDone},
		}},
	}
	engine, captured := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Frage"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("turn did not finish")
	}

	for _, ev := range captured.snapshot() {
		if ev.name == events.Warning {
			t.Errorf("a warning was emitted on a turn whose model is merely undescribed: %v", ev.payload["warning"])
		}
	}
	// And nothing was spent trying: the only generation is the answer itself.
	if n := provider.requestCount(); n != 1 {
		t.Errorf("backend calls = %d, want 1 — only the turn, with no summariser run", n)
	}
}

// TestManualCompactStillRefusesAnUnknownWindow is the other half, and the reason
// the guard above is in the automatic path rather than in compactWith: the
// refusal is a real answer to a real request. A user who presses Compact on a
// model nobody describes asked a question, so they get the reason — and no
// summariser call is spent finding it out.
func TestManualCompactStillRefusesAnUnknownWindow(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", State: llm.StateLoaded},
	}
	engine, _ := newTestEngine(t, st, provider)

	_, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true})
	if err == nil {
		t.Fatal("a manual compaction ran against a model whose window nobody reports")
	}
	if message, ok := uimsg.Parse(err.Error()); !ok || message.Code != "chat.contextUnknown" {
		t.Errorf("error = %v, want the code naming the unknown window", err)
	}
	if n := provider.requestCount(); n != 0 {
		t.Errorf("the summariser was called %d times, want 0", n)
	}
}

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
		config.Default(), llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192}, "", "", "")
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

// TestCompactReservesRoomForReasoningInTheSummariser is the regression test for
// the "empty summary" failure. A reasoning model spends `max_tokens` on private
// thinking before it writes a word of the answer, so a budget sized to the
// summary alone comes back empty (stop_reason "length") and compaction fails with
// nothing folded. The summariser must therefore be granted room to think beyond
// the summary itself — and the request must still fit the model's window.
func TestCompactReservesRoomForReasoningInTheSummariser(t *testing.T) {
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

	const window = 8192
	provider := &fakeProvider{
		model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: window, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "Gedächtnis."}}},
	}
	engine, _ := newTestEngine(t, st, provider)

	if _, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	req := provider.lastRequest()
	if req.MaxTokens == 0 {
		t.Fatal("the summariser was never called")
	}
	cfg := config.Default()
	// The cap is generous — room for a reasoning model to think and still answer —
	// and never exceeds the Cline-sized ceiling on a window this size.
	if req.MaxTokens > summaryOutputTokens {
		t.Errorf("summariser max_tokens = %d exceeds the %d-token cap", req.MaxTokens, summaryOutputTokens)
	}
	if req.MaxTokens <= cfg.Compact.MaxSummaryTokens {
		t.Errorf("summariser max_tokens = %d is no larger than the %d-token summary target — "+
			"a reasoning model would spend it all thinking",
			req.MaxTokens, cfg.Compact.MaxSummaryTokens)
	}
	if used := llm.EstimateMessagesTokens(req.Messages); used+req.MaxTokens > window {
		t.Errorf("summariser request = %d prompt + %d output = %d tokens, over the %d-token window",
			used, req.MaxTokens, used+req.MaxTokens, window)
	}
}

// TestCompactSummariserCapMatchesCline pins the number Cline uses for its
// summariser output on a roomy window: 8192 — large enough that a reasoning model
// can spend its thinking and still write a summary, which is the whole point of
// the cap. A small window trims it only to keep the request inside the window.
func TestCompactSummariserCapMatchesCline(t *testing.T) {
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

	provider := &fakeProvider{
		model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "Gedächtnis."}}},
	}
	engine, _ := newTestEngine(t, st, provider)
	if _, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := provider.lastRequest().MaxTokens; got != summaryOutputTokens {
		t.Errorf("summariser max_tokens = %d on a 128k window, want the %d-token cap",
			got, summaryOutputTokens)
	}
}

// TestSummarizeAsksTheModelNotToThink pins the in-prompt soft switch. A summary
// is a mechanical task, and `/no_think` is the only way to stop a reasoning model
// thinking first on a backend with no reasoning parameter — LM Studio's chat
// endpoint has nowhere to put "thinking: off".
func TestSummarizeAsksTheModelNotToThink(t *testing.T) {
	provider := &fakeProvider{replies: [][]llm.StreamEvent{{
		{Type: llm.EventDelta, Text: "Gedächtnis."},
	}}}
	if _, err := Summarize(context.Background(), provider, "fake-model", SummarizeOptions{
		Transcript: "### Nutzer\nHallo", MaxTokens: 1200,
	}); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	req := provider.lastRequest()
	if len(req.Messages) == 0 {
		t.Fatal("the summariser was never called")
	}
	last := req.Messages[len(req.Messages)-1]
	if !strings.Contains(last.Content, noThinkTag) {
		t.Errorf("summariser prompt does not carry %q:\n%s", noThinkTag, last.Content)
	}
}

// TestSummarizeExplainsAReasoningOnlyAnswer pins the message a user sees when a
// thinking model spends its whole output budget before writing a word of the
// summary. "Empty summary" alone hides both the cause and the fix; the error has
// to name the reasoning that ate the budget and the setting that grants more.
func TestSummarizeExplainsAReasoningOnlyAnswer(t *testing.T) {
	// Every attempt reasons and is cut off, so the final message has to name that
	// cause. The retry asks again, so the same reply is scripted for each attempt.
	onlyReasoning := []llm.StreamEvent{
		{Type: llm.EventReasoning, Text: "Der Nutzer will eine Zusammenfassung."},
		{Type: llm.EventDone, Stats: &llm.GenerationStats{StopReason: "length"}},
	}
	provider := &fakeProvider{
		replies: [][]llm.StreamEvent{onlyReasoning, onlyReasoning},
	}
	_, err := Summarize(context.Background(), provider, "fake-model", SummarizeOptions{
		Transcript: "### Nutzer\nHallo", MaxTokens: 1200,
	})
	if err == nil {
		t.Fatal("a reasoning-only answer was accepted as a summary")
	}
	if message, ok := uimsg.Parse(err.Error()); !ok || message.Code != "chat.summaryBudgetSpent" ||
		len(message.Args) != 2 || message.Args[0] != "length" || message.Args[1] == "0" {
		t.Errorf("error %q does not name the reasoning that ate the budget, with its stop reason and length", err)
	}
}

// TestSummarizeRejectsAnEmptyAnswer keeps the plain case honest: a model that
// returns nothing at all is still an error, with the message users already know.
func TestSummarizeRejectsAnEmptyAnswer(t *testing.T) {
	provider := &fakeProvider{replies: [][]llm.StreamEvent{{{Type: llm.EventDone}}}}
	_, err := Summarize(context.Background(), provider, "fake-model", SummarizeOptions{
		Transcript: "### Nutzer\nHallo", MaxTokens: 1200,
	})
	if message, ok := uimsg.Parse(err.Error()); err == nil || !ok || message.Code != "chat.summaryEmpty" {
		t.Fatalf("err = %v, want the empty-summary code", err)
	}
}

// TestSummarizeRetriesWhenReasoningAteTheBudget pins the fix for a thinking model
// whose private reasoning consumes the whole output budget and leaves no summary.
// The retry gives it room for the thinking it actually did plus a full answer, so
// a model that reasons and then answers still produces a summary.
func TestSummarizeRetriesWhenReasoningAteTheBudget(t *testing.T) {
	provider := &fakeProvider{replies: [][]llm.StreamEvent{
		{
			{Type: llm.EventReasoning, Text: strings.Repeat("nachdenken ", 40)},
			{Type: llm.EventDone, Stats: &llm.GenerationStats{StopReason: "length"}},
		},
		{
			{Type: llm.EventReasoning, Text: "noch etwas"},
			{Type: llm.EventDelta, Text: "## Festgestellte Fakten\n- Alles gut."},
			{Type: llm.EventDone, Stats: &llm.GenerationStats{StopReason: "stop"}},
		},
	}}
	const window = 40000

	got, err := Summarize(context.Background(), provider, "fake-model", SummarizeOptions{
		Transcript:   "### Nutzer\nHallo",
		MaxTokens:    1200,
		ContextLimit: window,
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !strings.Contains(got, "Festgestellte Fakten") {
		t.Errorf("summary = %q, want the content the model produced on the retry", got)
	}
	if n := provider.requestCount(); n != 2 {
		t.Fatalf("summariser called %d times, want 2 (one bounded try, then one with room to think)", n)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("recorded %d requests, want 2", len(provider.requests))
	}
	first, second := provider.requests[0], provider.requests[1]
	if second.MaxTokens <= first.MaxTokens {
		t.Errorf("retry max_tokens = %d, want more than the first %d", second.MaxTokens, first.MaxTokens)
	}
	if room := window - llm.EstimateMessagesTokens(second.Messages); second.MaxTokens > room {
		t.Errorf("retry max_tokens = %d exceeds the window room %d", second.MaxTokens, room)
	}
}

// TestSummarizeDoesNotRetryAGenuineEmptyAnswer: a model that simply returned
// nothing (no reasoning, a clean stop) will do so again, so it is reported once
// rather than retried.
func TestSummarizeDoesNotRetryAGenuineEmptyAnswer(t *testing.T) {
	provider := &fakeProvider{replies: [][]llm.StreamEvent{{
		{Type: llm.EventDone, Stats: &llm.GenerationStats{StopReason: "stop"}},
	}}}
	_, err := Summarize(context.Background(), provider, "fake-model", SummarizeOptions{
		Transcript: "### Nutzer\nHallo", MaxTokens: 1200, ContextLimit: 40000,
	})
	if err == nil {
		t.Fatal("an empty answer was accepted as a summary")
	}
	if n := provider.requestCount(); n != 1 {
		t.Errorf("summariser called %d times, want 1 — an empty answer must not be retried", n)
	}
}

// TestCompactDisablesThinkingForAThinkingModel pins that compaction asks a model
// which advertises thinking to skip it: a summary is not a reasoning task, and
// turning thinking off is what keeps a thinking model from spending its budget
// before it writes a word of the summary.
func TestCompactDisablesThinkingForAThinkingModel(t *testing.T) {
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

	provider := &fakeProvider{
		model: llm.ModelInfo{
			ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded,
			Capabilities: []string{llm.CapThink},
		},
		replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "Gedächtnis."}}},
	}
	engine, _ := newTestEngine(t, st, provider)
	if _, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	req := provider.lastRequest()
	if req.Think == nil || *req.Think {
		t.Errorf("think = %v, want false for a model that advertises thinking", req.Think)
	}
}

// TestCompactAsksAToggleBackendNotToThink pins that compaction reaches the LM
// Studio lever too: a backend whose model metadata advertises no thinking
// capability, but which implements llm.ThinkingToggle, is asked not to think.
func TestCompactAsksAToggleBackendNotToThink(t *testing.T) {
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

	provider := &fakeProvider{
		toggleThinking: true,
		model:          llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		replies:        [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "Gedächtnis."}}},
	}
	engine, _ := newTestEngine(t, st, provider)
	if _, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	req := provider.lastRequest()
	if req.Think == nil || *req.Think {
		t.Errorf("think = %v, want false for a backend that accepts the toggle", req.Think)
	}
}

// --- summarising: inline thinking and the summary model ---------------------

// seedTurns writes the minimal foldable history: one user turn and its answer.
func seedTurns(t *testing.T, st *store.Store) {
	t.Helper()
	for _, m := range []struct {
		id   string
		seq  int
		role string
		text string
	}{
		{"m1", 1, "user", "Erste Frage"},
		{"m2", 2, "assistant", "Erste Antwort"},
	} {
		if err := st.CreateMessage(store.Message{
			ID: m.id, ChatID: "chat-1", Seq: m.seq, Role: m.role, Content: m.text,
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAShapelessMemoryIsAskedOnceMore pins the cheap half of validating a summary. A
// long answer that ignored every section the contract asks for is prose, and prose is what
// each later turn has to search instead of scan — so it is asked once more with the shape
// named. Nothing rejects a summary: a retry that also comes back shapeless leaves the
// first answer in place, because a memory with the wrong shape still knows more than none.
func TestAShapelessMemoryIsAskedOnceMore(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedLongHistory(t, st, 4)

	shapeless := strings.Repeat("Das ist eine Zusammenfassung ohne einen einzigen Abschnitt. ", 60)
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: shapeless}, {Type: llm.EventDone}},
			{{Type: llm.EventDelta, Text: "## Festgestellte Fakten\n- Der Kessel braucht 1,5 bar."}, {Type: llm.EventDone}},
		},
	}
	engine, _ := newTestEngine(t, st, provider)

	got, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !got.Created {
		t.Fatal("nothing was compacted")
	}
	if n := provider.requestCount(); n != 2 {
		t.Fatalf("backend calls = %d, want the summary and its one nudge", n)
	}
	if !strings.Contains(got.Compaction.Summary, "Festgestellte Fakten") {
		t.Errorf("the second answer did not replace the shapeless memory: %q", got.Compaction.Summary)
	}
	// What the retry sends is the draft, not the history: the folded transcript must not
	// travel a second time just to ask for headings.
	second := provider.requestAt(1)
	retry := second.Messages[len(second.Messages)-1].Content
	if !strings.Contains(retry, "ohne einen einzigen Abschnitt") {
		t.Errorf("the retry does not carry the answer it is recasting: %q", truncateForTest(retry, 300))
	}
	if strings.Contains(retry, "<conversation>") || strings.Contains(retry, "### Nutzer") {
		t.Errorf("the retry re-sends the folded transcript instead of the draft: %q", truncateForTest(retry, 300))
	}
}

// TestAShortShapelessMemoryIsNotAskedAgain is the other half, and the reason the nudge is
// gated on size: a second summary request is minutes on a CPU-hosted model, and a model
// that answered a page-long instruction with three words is not fixed by asking again. The
// memory is kept as it is, without a retry.
func TestAShortShapelessMemoryIsNotAskedAgain(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedLongHistory(t, st, 4)

	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: "Zusammenfassung"}, {Type: llm.EventDone}},
		},
	}
	engine, _ := newTestEngine(t, st, provider)

	got, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got.Compaction.Summary != "Zusammenfassung" {
		t.Errorf("stored memory = %q, want the answer as it came", got.Compaction.Summary)
	}
	if n := provider.requestCount(); n != 1 {
		t.Fatalf("backend calls = %d, want no retry for a three-word answer", n)
	}
}

// TestTheSummaryResendsTheRequestPrefixWhenItFits pins the prompt-cache path: with room for
// it, the summarisation carries the chat's own system message and the folded messages as they
// were actually sent, instead of a re-rendered transcript. Those bytes are what the server
// already holds for the user's turn, so on a local llama.cpp server the fold reads them
// rather than evaluating the history again; a transcript is different text and always a miss.
func TestTheSummaryResendsTheRequestPrefixWhenItFits(t *testing.T) {
	st := newTestStore(t)
	chat := makeChat(t, st)
	seedLongHistory(t, st, 4)

	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: "## Festgestellte Fakten\n- etwas"}, {Type: llm.EventDone}},
		},
	}
	engine, _ := newTestEngine(t, st, provider)

	if _, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if n := provider.requestCount(); n != 1 {
		t.Fatalf("backend calls = %d, want the summary alone", n)
	}

	request := provider.requestAt(0)
	if len(request.Messages) < 3 {
		t.Fatalf("the summarisation sent %d messages, want the request and an instruction", len(request.Messages))
	}
	// The chat's own system message, unchanged: that is where the cache hit starts.
	if got := request.Messages[0].Content; !strings.HasPrefix(got, chat.SystemPrompt) {
		t.Errorf("the request does not open with the chat's own system message: %q", got)
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role != llm.RoleUser {
		t.Fatalf("last message role = %q, want the instruction as a user turn", last.Role)
	}
	joined := ""
	for _, m := range request.Messages {
		joined += m.Content + "\n"
	}
	if strings.Contains(joined, "### Nutzer") || strings.Contains(joined, "### Assistent") {
		t.Errorf("a rendered transcript was sent where the request prefix fits: %q",
			truncateForTest(joined, 400))
	}
	// The folded turns travel verbatim, which is the whole point of the path.
	if !strings.Contains(joined, "Frage 0:") {
		t.Errorf("the folded history is missing from the prefix: %q", truncateForTest(joined, 400))
	}
}

// TestThePrefixRequestCarriesTheMemoryOnce pins the saving on a chat that already has a
// memory. The prefix carries it in the system message it rides in, and the merge instruction
// names it there instead of quoting it again — before this the fold paid for the memory twice
// on every crossing turn, once to carry it and once to ask for it to be merged.
func TestThePrefixRequestCarriesTheMemoryOnce(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedLongHistory(t, st, 4)

	const memory = "## Festgestellte Fakten\n- MAG-Fülldruck 1,5 bar."
	if err := st.CreateCompaction(store.Compaction{
		ID: "c1", ChatID: "chat-1", Trigger: triggerManual,
		FromSeq: 1, ToSeq: 2, Summary: memory, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed compaction: %v", err)
	}

	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: "## Festgestellte Fakten\n- neu"}, {Type: llm.EventDone}},
		},
	}
	engine, _ := newTestEngine(t, st, provider)

	if _, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	joined := ""
	for _, m := range provider.requestAt(0).Messages {
		joined += m.Content + "\n"
	}
	if strings.Contains(joined, "<existing_memory>") {
		t.Errorf("the prefix request still quotes the memory it carries: %q", truncateForTest(joined, 500))
	}
	if !strings.Contains(joined, "<conversation_memory>") {
		t.Errorf("the prefix does not carry the memory at all: %q", truncateForTest(joined, 500))
	}
	if got := strings.Count(joined, memory); got != 1 {
		t.Errorf("the memory appears %d times in the summarisation request, want the system message's once: %q",
			got, truncateForTest(joined, 500))
	}
}

// TestThePrefixPathDoesNotReadTheDocumentAgain pins the fix that stops a fold from reading
// attachments it is about to re-send verbatim. The assembled request already carries the
// inlined text, so a document that has gone since the turn is not the fold's problem —
// before this the prefix path could still fail with chat.compactionAttachment over a record
// it never needed, and did a store round trip per attachment on every fold.
func TestThePrefixPathDoesNotReadTheDocumentAgain(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedLongHistory(t, st, 4)
	messages, err := st.ListMessages("chat-1")
	if err != nil || len(messages) == 0 {
		t.Fatalf("seed: %v", err)
	}
	if err := st.CreateAttachment(store.Attachment{
		ID: "a1", ChatID: "chat-1", MessageID: messages[0].ID, Filename: "facts.txt",
		Placement: store.PlacementInline, DocumentID: "gone",
	}); err != nil {
		t.Fatal(err)
	}

	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: "## Festgestellte Fakten\n- etwas"}, {Type: llm.EventDone}},
		},
	}
	engine, _ := newTestEngine(t, st, provider)

	if _, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true}); err != nil {
		t.Fatalf("the fold read a document the request already carried: %v", err)
	}
	joined := ""
	for _, m := range provider.requestAt(0).Messages {
		joined += m.Content + "\n"
	}
	if strings.Contains(joined, "<conversation>") {
		t.Fatalf("the fold did not take the prefix path, so this test proves nothing: %q",
			truncateForTest(joined, 300))
	}
}

// TestTheSummaryFallsBackToATranscriptWhenThePrefixDoesNotFit is the guard on the path above:
// re-sending the request is an improvement only while the request it makes still fits beside
// the answer the call may write. When it does not, the transcript is what goes out.
func TestTheSummaryFallsBackToATranscriptWhenThePrefixDoesNotFit(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	// Twelve turns whose prefix is several times the window.
	seedLongHistory(t, st, 12)

	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: "## Festgestellte Fakten\n- etwas"}, {Type: llm.EventDone}},
		},
	}
	engine, _ := newTestEngine(t, st, provider)

	if _, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	joined := ""
	for _, m := range provider.requestAt(0).Messages {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "### Nutzer") {
		t.Errorf("the summariser was sent the request instead of a transcript it cannot fit: %q",
			truncateForTest(joined, 400))
	}
}

// TestThePrefixIsOnlyOfferedWhenItCanBeNamed pins the conditions summarisationPrefix refuses
// on: a boundary that is not in the request, an alignment that a rewrite invalidated, and a
// picture — a summarisation is a text request, and the images belong to the conversation.
func TestThePrefixIsOnlyOfferedWhenItCanBeNamed(t *testing.T) {
	assembly := Assembly{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "Vorgaben"},
			{Role: llm.RoleUser, Content: "Frage eins"},
			{Role: llm.RoleAssistant, Content: "Antwort eins"},
			{Role: llm.RoleUser, Content: "Frage zwei"},
		},
		MessageSeqs: []int{0, 1, 2, 3},
	}
	plan := CompactPlan{Needed: true, FromSeq: 1, ToSeq: 2, KeepFromSeq: 3}

	prefix := summarisationPrefix(assembly, plan)
	if len(prefix) != 3 || prefix[1].Content != "Frage eins" {
		t.Fatalf("prefix = %+v, want the three messages above the kept turn", prefix)
	}
	if got := summarisationPrefix(assembly, CompactPlan{KeepFromSeq: 9}); got != nil {
		t.Errorf("a boundary outside the request produced a prefix: %+v", got)
	}
	if got := summarisationPrefix(Assembly{Messages: assembly.Messages}, plan); got != nil {
		t.Errorf("an assembly whose alignment was lost produced a prefix: %+v", got)
	}
	withImage := Assembly{Messages: append([]llm.Message(nil), assembly.Messages...), MessageSeqs: []int{0, 1, 2, 3}}
	withImage.Messages[1].Images = []string{"base64…"}
	if got := summarisationPrefix(withImage, plan); got != nil {
		t.Errorf("a prefix carrying an image was offered to a text summarisation: %+v", got)
	}
}

// TestEffectiveKeepTokens pins the resolution of the size bound, which nothing else covers:
// a configured value is used as typed, and 0 derives a quarter of the limit the plan is
// priced against, held inside 2000-15000. The clamp is the whole point of the derivation —
// it is what stops one number from meaning "most of an 8k window" on a small model and "a
// tenth" on a 131k one — and the ends are opencode's rather than measured here, so what they
// mean has to be stated by a test rather than assumed.
func TestEffectiveKeepTokens(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured int
		limit      int
		want       int
	}{
		// A configured value wins outright: it is the user's number, not a share of anything,
		// so the clamp below does not apply to it.
		{"configured", 4000, 131072, 4000},
		{"configured below the floor is still the user's answer", 500, 131072, 500},
		{"configured without a known limit", 4000, 0, 4000},
		// 0 derives a quarter of the effective limit...
		{"a quarter of a middle window", 0, 32000, 8000},
		{"a quarter of a small window", 0, 8192, 2048},
		// ...and is held at both ends.
		{"clamped to the ceiling", 0, 131072, 15000},
		{"the ceiling itself is a quarter, so it stands", 0, 60000, 15000},
		{"clamped to the floor", 0, 4000, 2000},
		// No limit is no quarter to take, and 0 is what leaves the walk off (PlanCompaction).
		{"unknown limit", 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := config.Default()
			settings.Compact.KeepRecentTokens = tc.configured
			if got := effectiveKeepTokens(settings, tc.limit); got != tc.want {
				t.Fatalf("effectiveKeepTokens(configured %d, limit %d) = %d, want %d",
					tc.configured, tc.limit, got, tc.want)
			}
		})
	}
}

func summariesOf(t *testing.T, st *store.Store) []store.Compaction {
	t.Helper()
	list, err := st.ListCompactions("chat-1")
	if err != nil {
		t.Fatalf("list compactions: %v", err)
	}
	return list
}

// engineWithSummaryModel builds an engine with a chat backend and, optionally, a
// second backend used for summarisation, with the summary model set in the
// settings as a user would set it.
func engineWithSummaryModel(t *testing.T, st *store.Store, chatProvider, summaryProvider *fakeProvider,
	ref llm.Ref) *Engine {

	t.Helper()
	settings := config.Default()
	settings.AutoTitle = false
	settings.Compact.SummaryModel = ref
	registry := llm.NewRegistry(nil)
	registry.Register(chatProvider)
	if summaryProvider != nil {
		registry.Register(summaryProvider)
	}
	log := &eventLog{}
	return NewEngine(st, registry, nil, func() config.Settings { return settings }, log.emit)
}

// TestCompactStripsInlineThinkingFromASummary pins that a model which inlines its
// deliberation in the content does not get that deliberation stored as the chat's
// memory. The memory is re-read on every later turn, so private reasoning in it
// would be permanent — and it is not what the user asked to remember.
func TestCompactStripsInlineThinkingFromASummary(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedTurns(t, st)

	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{{{Type: llm.EventDelta,
			Text: "<think>Der Nutzer fragte nach Peru. Ich sollte Lima nennen.</think>\n\n" +
				"## Festgestellte Fakten\n- Lima ist die Hauptstadt von Peru."}}},
	}
	engine, _ := newTestEngine(t, st, provider)
	if _, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	list := summariesOf(t, st)
	if len(list) != 1 {
		t.Fatalf("%d compactions, want 1", len(list))
	}
	got := list[0].Summary
	if strings.Contains(got, "<think>") || strings.Contains(got, "Ich sollte Lima nennen") {
		t.Errorf("the stored memory still carries the model's private thinking:\n%s", got)
	}
	if !strings.Contains(got, "Lima ist die Hauptstadt von Peru") {
		t.Errorf("the stored memory lost the answer itself:\n%s", got)
	}
	if provider.requestCount() != 1 {
		t.Errorf("%d summariser calls, want 1: stripping the thinking is not a retry",
			provider.requestCount())
	}
}

// TestCompactRetriesAnAnswerThatWasOnlyThinking pins the other half of the rule:
// an answer made of nothing but an inline thinking block is not a summary. It is
// treated as the reasoning-budget failure — retried with room for that thinking —
// rather than stored as the chat's memory.
func TestCompactRetriesAnAnswerThatWasOnlyThinking(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedTurns(t, st)

	provider := &fakeProvider{
		// A window wide enough that the retry can actually be given more room than
		// the first attempt; on a small one the prompt alone leaves none.
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 32768, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: "<analysis>Ich prüfe erst, ob die Frage schon beantwortet ist.</analysis>"}},
			{{Type: llm.EventDelta, Text: "## Festgestellte Fakten\n- Lima ist die Hauptstadt von Peru."}},
		},
	}
	engine, _ := newTestEngine(t, st, provider)
	if _, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	list := summariesOf(t, st)
	if len(list) != 1 {
		t.Fatalf("%d compactions, want 1", len(list))
	}
	if strings.Contains(list[0].Summary, "Ich prüfe erst") {
		t.Errorf("a thinking-only answer was stored as the chat's memory:\n%s", list[0].Summary)
	}
	if provider.requestCount() != 2 {
		t.Fatalf("%d summariser calls, want a retry", provider.requestCount())
	}
	first, retry := provider.requestAt(0), provider.requestAt(1)
	if retry.MaxTokens <= first.MaxTokens {
		t.Errorf("retry budget %d, want more than the first attempt's %d: the thinking is what ate it",
			retry.MaxTokens, first.MaxTokens)
	}
}

// TestATitleIsNotAskedAgainWhenReasoningAteTheBudget pins the fix for a title that could
// never be produced. On a reasoning backend the output cap is spent thinking, so the reply
// is `length` with an empty body — and the empty title is what made the guard send the same
// request again on every turn of an untitled chat. The request now carries thinking off and
// a cap a title can use, and an empty truncated answer names the chat from its message
// instead of vanishing into another retry.
func TestATitleIsNotAskedAgainWhenReasoningAteTheBudget(t *testing.T) {
	st := newTestStore(t)
	chat := makeChat(t, st)
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192,
			State: llm.StateLoaded, Capabilities: []string{llm.CapThink}},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDone, Stats: &llm.GenerationStats{StopReason: "length"}}},
		},
	}
	engine, _ := newTestEngine(t, st, provider)
	settings := config.Default()

	engine.autoTitle(context.Background(), &chat, provider, "fake", "fake-model", provider.model, settings,
		"Prüfe den Kesseldruck und sag mir, ob 1,5 bar reicht")

	req := provider.requestAt(0)
	if req.Think == nil || *req.Think {
		t.Error("the title asked a reasoning model to think")
	}
	if req.MaxTokens != titleMaxTokens {
		t.Errorf("title budget = %d, want %d", req.MaxTokens, titleMaxTokens)
	}
	if !strings.Contains(req.Messages[0].Content, noThinkTag) {
		t.Error("the title did not carry the in-prompt thinking switch")
	}
	if chat.Title == "" {
		t.Fatal("the chat is still untitled, so the next turn asks again")
	}
	if !strings.Contains(chat.Title, "Kesseldruck") {
		t.Errorf("title = %q, want the message it is named after", chat.Title)
	}
}

// TestALongTitleIsCutRatherThanAskedAgain is the other endless retry: an answer over
// titleMaxRunes used to be discarded, which left the chat untitled and the next turn asking
// again for a title the model had already written.
func TestALongTitleIsCutRatherThanAskedAgain(t *testing.T) {
	st := newTestStore(t)
	chat := makeChat(t, st)
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: strings.Repeat("Kesseldruck und Wartung prüfen ", 12)},
				{Type: llm.EventDone}},
		},
	}
	engine, _ := newTestEngine(t, st, provider)

	engine.autoTitle(context.Background(), &chat, provider, "fake", "fake-model", provider.model,
		config.Default(), "Prüfe den Kesseldruck")

	if chat.Title == "" {
		t.Fatal("a long answer left the chat untitled")
	}
	if n := len([]rune(chat.Title)); n > titleMaxRunes {
		t.Errorf("title is %d runes, want at most %d", n, titleMaxRunes)
	}
}

// TestATitleRunsOnTheSummaryModel is the cheap-routing half: a user who pointed summaries at
// a smaller model meant the background work too, and naming a chat is that kind of work.
func TestATitleRunsOnTheSummaryModel(t *testing.T) {
	st := newTestStore(t)
	chat := makeChat(t, st)
	own := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
	}
	cheap := &fakeProvider{
		id:    "cheap",
		model: llm.ModelInfo{ID: "cheap-model", Backend: "cheap", ContextLimit: 8192, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: "Kesseldruck prüfen"}, {Type: llm.EventDone}},
		},
	}
	settings := config.Default()
	settings.Compact.SummaryModel = llm.Ref{Backend: "cheap", Model: "cheap-model"}
	registry := llm.NewRegistry(nil)
	registry.Register(own)
	registry.Register(cheap)
	engine := NewEngine(st, registry, nil, func() config.Settings { return settings }, func(string, any) {})

	engine.autoTitle(context.Background(), &chat, own, "fake", "fake-model", own.model, settings,
		"Prüfe den Kesseldruck")

	if own.requestCount() != 0 {
		t.Errorf("the chat's own model was asked for a title: %d calls", own.requestCount())
	}
	if cheap.requestCount() != 1 {
		t.Fatalf("summary model calls = %d, want the title", cheap.requestCount())
	}
	if chat.Title != "Kesseldruck prüfen" {
		t.Errorf("title = %q, want the summary model's answer", chat.Title)
	}
}

// TestCompactRunsOnTheConfiguredSummaryModel pins the escape hatch for a model
// that cannot be talked out of thinking: a user picks a different model for
// summarisation, and it is that model's window that prices the call. A summary
// budgeted against the chat model's window would be a request the summary model
// cannot hold.
func TestCompactRunsOnTheConfiguredSummaryModel(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedTurns(t, st)

	chatProvider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
	}
	// The helper backend mirrors LM Studio: it accepts the thinking toggle even
	// though its model metadata advertises no reasoning capability.
	summaryProvider := &fakeProvider{
		id: "helper", name: "Helper", toggleThinking: true,
		model:   llm.ModelInfo{ID: "helper-model", Backend: "helper", ContextLimit: 4096, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "Gedächtnis."}}},
	}
	engine := engineWithSummaryModel(t, st, chatProvider, summaryProvider,
		llm.Ref{Backend: "helper", Model: "helper-model"})

	if _, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if n := chatProvider.requestCount(); n != 0 {
		t.Errorf("the chat's own model ran %d generations for the summary, want none", n)
	}
	req := summaryProvider.lastRequest()
	if req.Model != "helper-model" {
		t.Errorf("summarised on %q, want the configured model", req.Model)
	}
	if req.Think == nil || *req.Think {
		t.Errorf("think = %v, want false: a separate summary model is chosen for exactly this",
			req.Think)
	}
	if req.MaxTokens <= 0 || req.MaxTokens >= 8192 {
		t.Errorf("summary budget = %d, want it bounded by the summary model's 4096-token window",
			req.MaxTokens)
	}

	list := summariesOf(t, st)
	if len(list) != 1 {
		t.Fatalf("%d compactions, want 1", len(list))
	}
	if list[0].Backend != "helper" || list[0].Model != "helper-model" {
		t.Errorf("compaction recorded %q/%q, want the model that actually wrote it",
			list[0].Backend, list[0].Model)
	}
}

// TestCompactReportsAnUnconfiguredSummaryBackend pins that the setting is never
// silently ignored. Quietly falling back to the chat's own model would hide both
// the mistake and its fix, and the user would be left wondering why a summary is
// still empty.
func TestCompactReportsAnUnconfiguredSummaryBackend(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedTurns(t, st)

	chatProvider := &fakeProvider{
		model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "Gedächtnis."}}},
	}
	engine := engineWithSummaryModel(t, st, chatProvider, nil, llm.Ref{Backend: "ghost", Model: "x"})

	_, err := engine.Compact(context.Background(), "chat-1", CompactOptions{Force: true})
	if err == nil {
		t.Fatal("a summary model on an unconfigured backend was accepted silently")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error %q does not name the backend that is missing", err)
	}
	if n := chatProvider.requestCount(); n != 0 {
		t.Errorf("the compaction ran on the chat's model anyway: %d generations", n)
	}
}

func TestCompactionRejectsIncompleteSummary(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedTurns(t, st)
	p := &fakeProvider{model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 32768},
		replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "Partial memory"}, {Type: llm.EventDone, Stats: &llm.GenerationStats{StopReason: "length"}}}}}
	e, _ := newTestEngine(t, st, p)
	result, err := e.Compact(context.Background(), "chat-1", CompactOptions{Force: true})
	message, ok := uimsg.Parse(fmt.Sprint(err))
	if !ok || message.Code != "chat.summaryTruncated" || result.Created || len(summariesOf(t, st)) != 0 {
		t.Fatalf("partial memory accepted: result=%+v error=%v", result, err)
	}
}

func TestCompactionRejectsMaxTokensStop(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedTurns(t, st)
	p := &fakeProvider{model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 32768},
		replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "Partial memory"}, {Type: llm.EventDone, Stats: &llm.GenerationStats{StopReason: "max_tokens"}}}}}
	e, _ := newTestEngine(t, st, p)
	result, err := e.Compact(context.Background(), "chat-1", CompactOptions{Force: true})
	message, ok := uimsg.Parse(fmt.Sprint(err))
	if !ok || message.Code != "chat.summaryTruncated" || result.Created || len(summariesOf(t, st)) != 0 {
		t.Fatalf("partial memory accepted: result=%+v error=%v", result, err)
	}
}

func TestCompactionRejectsMissingTranscript(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedTurns(t, st)
	p := &fakeProvider{model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 4096}}
	e, _ := newTestEngine(t, st, p)
	_, err := e.Compact(context.Background(), "chat-1", CompactOptions{Force: true, Focus: strings.Repeat("focus ", 10000)})
	message, ok := uimsg.Parse(fmt.Sprint(err))
	if !ok || message.Code != "chat.compactionNoTranscript" || p.requestCount() != 0 || len(summariesOf(t, st)) != 0 {
		t.Fatalf("empty transcript was not refused: %v", err)
	}
}

func TestCompactionPreservesInlineDocumentAndRefusesImage(t *testing.T) {
	for _, image := range []bool{false, true} {
		t.Run(fmt.Sprintf("image=%v", image), func(t *testing.T) {
			st := newTestStore(t)
			makeChat(t, st)
			if err := st.CreateMessage(store.Message{ID: "m1", ChatID: "chat-1", Seq: 1, Role: "user", Content: "Read this"}); err != nil {
				t.Fatal(err)
			}
			a := store.Attachment{ID: "a1", ChatID: "chat-1", MessageID: "m1", Filename: "facts.txt", Placement: store.PlacementInline, DocumentID: "d1"}
			if image {
				a.Kind = "image"
				a.Mime = "image/png"
			}
			if err := st.CreateAttachment(a); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateDocument(store.Document{ID: "d1", AttachmentID: "a1", ChatID: "chat-1", Title: "facts.txt", Text: "The secret fact is 42."}); err != nil {
				t.Fatal(err)
			}
			p := &fakeProvider{model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 32768}, replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "The fact is 42."}}}}
			e, _ := newTestEngine(t, st, p)
			result, err := e.Compact(context.Background(), "chat-1", CompactOptions{Force: true})
			if image {
				message, ok := uimsg.Parse(fmt.Sprint(err))
				if !ok || message.Code != "chat.compactionImage" || result.Created || p.requestCount() != 0 {
					t.Fatalf("image folded: %+v %v", result, err)
				}
				return
			}
			if err != nil || !result.Created {
				t.Fatalf("document fold: %+v %v", result, err)
			}
			if !strings.Contains(p.lastRequest().Messages[1].Content, "The secret fact is 42.") {
				t.Fatal("document content was not sent to summariser")
			}
		})
	}
}

func TestCompactionRangeMustBeContiguousAndEndAtTurnBoundary(t *testing.T) {
	for _, opts := range []CompactOptions{{Force: true, FromSeq: 2}, {Force: true, ToSeq: 1}, {Force: true, ToSeq: 999}} {
		st := newTestStore(t)
		makeChat(t, st)
		seedTurns(t, st)
		p := &fakeProvider{model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 32768}}
		e, _ := newTestEngine(t, st, p)
		_, err := e.Compact(context.Background(), "chat-1", opts)
		message, ok := uimsg.Parse(fmt.Sprint(err))
		if !ok || message.Code != "chat.compactionRange" || p.requestCount() != 0 {
			t.Fatalf("range %+v accepted: %v", opts, err)
		}
	}
}

func TestUndoCannotRaceCompaction(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	seedTurns(t, st)
	if err := st.CreateMessage(store.Message{ID: "m3", ChatID: "chat-1", Seq: 3, Role: "user", Content: "next question"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCompaction(store.Compaction{ID: "old", ChatID: "chat-1", FromSeq: 1, ToSeq: 2, Summary: "old memory"}); err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 32768}, started: make(chan struct{}), blockAfter: 1, replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "partial"}, {Type: llm.EventDelta, Text: "more"}}}}
	e, _ := newTestEngine(t, st, p)
	done := make(chan error, 1)
	go func() { _, err := e.Compact(context.Background(), "chat-1", CompactOptions{Force: true}); done <- err }()
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("compaction did not start")
	}
	if err := e.DeleteCompaction("chat-1", "old"); err == nil {
		t.Error("undo succeeded while compaction was running")
	}
	if len(summariesOf(t, st)) != 1 {
		t.Error("undo removed the existing memory")
	}
	e.Stop("chat-1")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("compaction did not stop")
	}
	if err := e.DeleteCompaction("chat-1", "old"); err != nil {
		t.Fatalf("undo after compaction: %v", err)
	}
}

func TestCompactionRefusesDocumentCutFromTranscript(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	if err := st.CreateMessage(store.Message{ID: "m1", ChatID: "chat-1", Seq: 1, Role: "user", Content: "Read this"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAttachment(store.Attachment{ID: "a1", ChatID: "chat-1", MessageID: "m1", Filename: "long.txt", Placement: store.PlacementInline, DocumentID: "d1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDocument(store.Document{ID: "d1", AttachmentID: "a1", ChatID: "chat-1", Title: "long.txt", Text: strings.Repeat("important facts ", 4000)}); err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192}}
	e, _ := newTestEngine(t, st, p)
	_, err := e.Compact(context.Background(), "chat-1", CompactOptions{Force: true})
	message, ok := uimsg.Parse(fmt.Sprint(err))
	if !ok || message.Code != "chat.compactionAttachmentTooLarge" || p.requestCount() != 0 || len(summariesOf(t, st)) != 0 {
		t.Fatalf("cut attachment accepted: %v", err)
	}
}
