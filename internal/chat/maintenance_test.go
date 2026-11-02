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
