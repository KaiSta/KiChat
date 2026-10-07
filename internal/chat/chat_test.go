package chat

import (
	"strings"
	"testing"
	"time"

	"kichat/internal/llm"
	"kichat/internal/store"
)

func TestRenderPromptSubstitutesKnownVariables(t *testing.T) {
	now := time.Date(2026, 10, 6, 23, 45, 0, 0, time.UTC)
	got := RenderPrompt(
		"Datum: {{datetime}} | Ort: {{location}} | Modell: {{model}} | Limit: {{context_limit}} | Tools: {{tool_list}}",
		TemplateVars{
			Now:          now,
			Location:     "Karlsdorf-Neuthard",
			Model:        "qwen3.8-27b",
			ContextLimit: 48128,
			Tools:        []string{"search_web", "scrape_url"},
		})
	for _, want := range []string{
		"Dienstag", "06.10.2026",
		"Karlsdorf-Neuthard", "qwen3.8-27b", "48128 Tokens",
		"search_web, scrape_url",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered prompt missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "{{") {
		t.Errorf("unsubstituted variable left in %q", got)
	}
}

// TestRenderPromptCoversEveryDocumentedVariable keeps the variable list and the
// substitution map from drifting apart.
func TestRenderPromptCoversEveryDocumentedVariable(t *testing.T) {
	for _, name := range KnownVars {
		out := RenderPrompt("{{"+name+"}}", TemplateVars{Now: time.Now(), Tools: []string{"a"}})
		if out == "{{"+name+"}}" {
			t.Errorf("variable %q is listed as known but was not substituted", name)
		}
	}
}

// TestRenderPromptLeavesTyposVisible means a mistyped variable shows up in the
// preview instead of silently disappearing.
func TestRenderPromptLeavesTyposVisible(t *testing.T) {
	got := RenderPrompt("Hallo {{datetim}}", TemplateVars{Now: time.Now()})
	if got != "Hallo {{datetim}}" {
		t.Fatalf("got %q", got)
	}
	unresolved := UnresolvedVars("[{{datetime}}] [{{nope}}] [{{alsoBad}}]")
	if len(unresolved) != 2 || unresolved[0] != "alsoBad" || unresolved[1] != "nope" {
		t.Fatalf("unresolved = %v", unresolved)
	}
}

// --- compaction planning ---------------------------------------------------

func msg(seq int, role, content string) store.Message {
	return store.Message{ID: string(rune('a' + seq)), Seq: seq, Role: role, Content: content}
}

// conversation builds a realistic alternating transcript.
func conversation(turns int) []store.Message {
	var out []store.Message
	seq := 1
	for i := 0; i < turns; i++ {
		out = append(out, msg(seq, "user", "Frage Nummer "+strings.Repeat("x", 40)))
		seq++
		out = append(out, msg(seq, "assistant", "Antwort Nummer "+strings.Repeat("y", 40)))
		seq++
	}
	return out
}

func TestPlanCompactionDoesNothingBelowThreshold(t *testing.T) {
	plan := PlanCompaction(PlanInput{
		Messages:  conversation(20),
		Budget:    ContextBudget{Limit: 10000, ProjectedRatio: 0.25},
		Threshold: 0.80, KeepRecentTurns: 3, Reserve: 1024,
	})
	if plan.Needed {
		t.Fatalf("compaction should not trigger at 25%%: %+v", plan)
	}
}

func TestPlanCompactionFoldsOldTurnsAndKeepsRecentOnes(t *testing.T) {
	messages := conversation(10) // messages, seq 1..20
	plan := PlanCompaction(PlanInput{
		Messages:  messages,
		Budget:    ContextBudget{Limit: 10000, ProjectedRatio: 0.95},
		Threshold: 0.80, KeepRecentTurns: 3, Reserve: 1024,
	})
	if !plan.Needed {
		t.Fatalf("expected compaction to trigger: %+v", plan)
	}
	if plan.FromSeq != 1 {
		t.Errorf("FromSeq = %d, want 1", plan.FromSeq)
	}
	// 10 user messages, keep the last 3 turns → the 8th user message is seq 15.
	if plan.KeepFromSeq != 15 {
		t.Errorf("KeepFromSeq = %d, want 15", plan.KeepFromSeq)
	}
	if plan.ToSeq != 14 {
		t.Errorf("ToSeq = %d, want 14", plan.ToSeq)
	}
	if plan.FoldedCount != 14 {
		t.Errorf("FoldedCount = %d, want 14", plan.FoldedCount)
	}
	if plan.TokensBefore <= 0 || plan.TokensAfterEstimate <= 0 {
		t.Errorf("token estimates missing: %+v", plan)
	}
	if plan.TokensAfterEstimate >= plan.TokensBefore {
		t.Errorf("summary estimate %d should be smaller than folded content %d",
			plan.TokensAfterEstimate, plan.TokensBefore)
	}
}

// TestPlanCompactionCutsOnlyAtUserBoundaries is the guard that keeps a tool call
// and its result together: cutting anywhere else produces a request the backend
// rejects.
func TestPlanCompactionCutsOnlyAtUserBoundaries(t *testing.T) {
	messages := []store.Message{
		msg(1, "user", "Recherchiere Mietrecht"),
		msg(2, "assistant", "Ich suche."),
		msg(3, "tool", "Suchtreffer …"),
		msg(4, "assistant", "Ergebnis: § 573 BGB."),
		msg(5, "user", "Und in Baden-Württemberg?"),
		msg(6, "assistant", "Antwort zwei."),
		msg(7, "user", "Was ist mit 2027?"),
		msg(8, "assistant", "Antwort drei."),
	}
	plan := PlanCompaction(PlanInput{
		Messages:  messages,
		Budget:    ContextBudget{Limit: 5000, ProjectedRatio: 0.99},
		Threshold: 0.80, KeepRecentTurns: 2, Reserve: 512,
	})
	if !plan.Needed {
		t.Fatalf("expected compaction: %+v", plan)
	}
	// Keeping the last 2 user turns means seq 5 onward, so the cut is at seq 4
	// which is the end of a complete turn, never mid tool exchange.
	if plan.KeepFromSeq != 5 || plan.ToSeq != 4 {
		t.Fatalf("cut = [%d,%d], want [5,4] (KeepFrom=%d)", plan.FromSeq, plan.ToSeq, plan.KeepFromSeq)
	}
	for _, m := range messages {
		if m.Seq != plan.KeepFromSeq {
			continue
		}
		if m.Role != "user" {
			t.Fatalf("cut must land on a user message, landed on %q", m.Role)
		}
	}
}

func TestPlanCompactionWithTooFewTurnsFoldsNothing(t *testing.T) {
	plan := PlanCompaction(PlanInput{
		Messages:  conversation(2),
		Budget:    ContextBudget{Limit: 10000, ProjectedRatio: 0.99},
		Threshold: 0.80, KeepRecentTurns: 6, Reserve: 1024,
	})
	if plan.Needed {
		t.Fatalf("nothing should be folded when all turns are recent: %+v", plan)
	}
}

// TestManualCompactionOnShortChatFoldsWhatExists documents the new manual-
// compaction behaviour: a forced run over a conversation whose only turns are
// fewer than keepRecentTurns still picks a real cut (the newest user turn is the
// last turn that could split a tool exchange).
func TestManualCompactionOnShortChatFoldsWhatExists(t *testing.T) {
	plan := PlanCompaction(PlanInput{
		Messages:  conversation(2),
		Budget:    ContextBudget{Limit: 10000, ProjectedRatio: 0.99},
		Threshold: 0.80, KeepRecentTurns: 6, Reserve: 1024, Force: true,
	})
	if !plan.Needed {
		t.Fatalf("forced run over a short chat should still pick a cut: %+v", plan)
	}
	if plan.FromSeq != 1 {
		t.Fatalf("FromSeq = %d, want 1", plan.FromSeq)
	}
	// The cut lands before the newest user turn, so its whole exchange stays
	// verbatim and only the older turn is folded.
	if plan.ToSeq != 2 {
		t.Fatalf("ToSeq = %d, want 2 — fold turn one, keep the newest user turn verbatim", plan.ToSeq)
	}
	if plan.KeepFromSeq != 3 {
		t.Fatalf("KeepFromSeq = %d, want 3 (the newest user message)", plan.KeepFromSeq)
	}
	if plan.FoldedCount != 2 {
		t.Fatalf("FoldedCount = %d, want 2", plan.FoldedCount)
	}
}

func TestPlanCompactionResumesAfterPreviousCompaction(t *testing.T) {
	messages := conversation(12)
	plan := PlanCompaction(PlanInput{
		Messages:    messages,
		Compactions: []store.Compaction{{FromSeq: 1, ToSeq: 10, Summary: "alt"}},
		Budget:      ContextBudget{Limit: 10000, ProjectedRatio: 0.95},
		Threshold:   0.80, KeepRecentTurns: 2, Reserve: 1024,
	})
	if !plan.Needed {
		t.Fatalf("expected a second compaction: %+v", plan)
	}
	if plan.FromSeq != 11 {
		t.Fatalf("FromSeq = %d, want 11 so already-folded history is not summarised twice", plan.FromSeq)
	}
}

// TestPlanCompactionForcedTargetFoldsMore covers "compact until below 50%".
func TestPlanCompactionForcedTargetFoldsMore(t *testing.T) {
	messages := conversation(20)
	gentle := PlanCompaction(PlanInput{
		Messages: messages, Compactions: nil,
		Budget:    ContextBudget{Limit: 20000, System: 200, ToolSchemas: 100},
		Threshold: 0.80, KeepRecentTurns: 6, Reserve: 1024, Force: true,
	})
	aggressive := PlanCompaction(PlanInput{
		Messages: messages, Compactions: nil,
		Budget:    ContextBudget{Limit: 20000, System: 200, ToolSchemas: 100},
		Threshold: 0.80, KeepRecentTurns: 6, Reserve: 1024, Force: true, Target: 0.3,
	})
	if !gentle.Needed || !aggressive.Needed {
		t.Fatalf("both plans should compact: gentle=%+v aggressive=%+v", gentle, aggressive)
	}
	if aggressive.ToSeq < gentle.ToSeq {
		t.Fatalf("a lower target must fold at least as much: gentle.ToSeq=%d aggressive.ToSeq=%d",
			gentle.ToSeq, aggressive.ToSeq)
	}
}

// --- context assembly ------------------------------------------------------

func asmChat(prompt string) store.Chat {
	return store.Chat{
		ID: "c1", Backend: "lmstudio", Model: "m1",
		SystemPrompt: prompt, Temperature: 0.7,
	}
}

func TestAssembleItemisesTheBudget(t *testing.T) {
	messages := conversation(4)
	assembly := Assemble(AssemblyInput{
		Chat:     asmChat("Du bist ein Assistent."),
		Messages: messages,
		Limit:    10000, LimitSource: "backend", Reserve: 1024,
		Documents: []store.Document{{
			ID: "d1", Title: "bericht.pdf", Text: strings.Repeat("Inhalt. ", 100), TokenCount: 900,
		}},
		Tools: []llm.ToolDefinition{{Name: "search_web", Description: "sucht"}},
	})
	// Attach the document to a message so it is counted as inlined content.
	assembly = Assemble(AssemblyInput{
		Chat:     asmChat("Du bist ein Assistent."),
		Messages: withAttachment(messages, 1, "d1"),
		Limit:    10000, LimitSource: "backend", Reserve: 1024,
		Documents: []store.Document{{
			ID: "d1", Title: "bericht.pdf", Text: strings.Repeat("Inhalt. ", 100), TokenCount: 900,
		}},
		Tools: []llm.ToolDefinition{{Name: "search_web", Description: "sucht"}},
	})

	budget := assembly.Budget
	if budget.System <= 0 {
		t.Error("system prompt tokens not counted")
	}
	if budget.History <= 0 {
		t.Error("history tokens not counted")
	}
	if budget.Attachments != 900 {
		t.Errorf("attachment tokens = %d, want 900", budget.Attachments)
	}
	if budget.ToolSchemas <= 0 {
		t.Error("tool schema tokens not counted")
	}
	if budget.Estimated != budget.System+budget.Memory+budget.Attachments+budget.History+budget.ToolSchemas {
		t.Errorf("Estimated %d does not equal the sum of its parts", budget.Estimated)
	}
	if budget.Projected != budget.Used+1024 {
		t.Errorf("Projected = %d, want Used+reserve", budget.Projected)
	}
	if len(budget.Segments) < 4 {
		t.Errorf("expected an itemised breakdown, got %d segments", len(budget.Segments))
	}
	// The inlined document text must actually reach the model.
	foundDoc := false
	for _, m := range assembly.Messages {
		if strings.Contains(m.Content, "<document ") {
			foundDoc = true
		}
	}
	if !foundDoc {
		t.Error("inlined attachment text is missing from the request")
	}
}

func withAttachment(messages []store.Message, seq int, documentID string) []store.Message {
	out := make([]store.Message, len(messages))
	copy(out, messages)
	for i := range out {
		if out[i].Seq == seq {
			out[i].Attachments = []store.Attachment{{
				ID: "a1", ChatID: "c1", MessageID: out[i].ID,
				Filename: "bericht.pdf", DocumentID: documentID,
				Placement: store.PlacementInline,
			}}
		}
	}
	return out
}

// TestAssembleMeasurementDelta covers the subtle accounting rule: the backend's
// prompt_tokens describes the previous request, so messages added since must be
// added on top rather than ignored.
func TestAssembleMeasurementDelta(t *testing.T) {
	messages := conversation(3) // seq 1..6

	// Measurement taken when only the first 4 messages existed.
	assembly := Assemble(AssemblyInput{
		Chat:     asmChat("System."),
		Messages: messages,
		Limit:    10000, Reserve: 0,
		Measured: 5000, MeasuredValid: true, MeasuredSeq: 4,
	})
	if !assembly.Budget.MeasuredValid {
		t.Fatal("measurement should still be valid")
	}
	if assembly.Budget.DeltaSince <= 0 {
		t.Fatalf("expected the two newer messages to be counted, delta = %d", assembly.Budget.DeltaSince)
	}
	if assembly.Budget.Used != 5000+assembly.Budget.DeltaSince {
		t.Fatalf("Used = %d, want 5000+%d", assembly.Budget.Used, assembly.Budget.DeltaSince)
	}
	if assembly.Budget.Used <= assembly.Budget.Measured {
		t.Fatal("used tokens must not under-report the window")
	}

	// With nothing new, the measured value is used verbatim.
	exact := Assemble(AssemblyInput{
		Chat:     asmChat("System."),
		Messages: messages,
		Limit:    10000,
		Measured: 5000, MeasuredValid: true, MeasuredSeq: 6,
	})
	if exact.Budget.Used != 5000 {
		t.Fatalf("Used = %d, want exactly 5000", exact.Budget.Used)
	}
	if exact.Budget.DeltaSince != 0 {
		t.Fatalf("DeltaSince = %d, want 0", exact.Budget.DeltaSince)
	}
}

// TestAssembleDiscardsMeasurementAcrossCompaction keeps the meter honest when
// the messages a measurement counted have been folded away.
func TestAssembleDiscardsMeasurementAcrossCompaction(t *testing.T) {
	messages := conversation(10)
	assembly := Assemble(AssemblyInput{
		Chat:        asmChat("System."),
		Messages:    messages,
		Compactions: []store.Compaction{{ID: "k1", FromSeq: 1, ToSeq: 18, Summary: "Gedächtnis"}},
		Limit:       10000,
		Measured:    40000, MeasuredValid: true, MeasuredSeq: 4,
	})
	if assembly.Budget.MeasuredValid {
		t.Fatal("a measurement covering folded-away messages must be discarded")
	}
	if assembly.Budget.Used != assembly.Budget.Estimated {
		t.Fatalf("Used = %d should fall back to Estimated = %d",
			assembly.Budget.Used, assembly.Budget.Estimated)
	}
}

// TestAssembleCompactionReplacesOldHistory verifies folded messages are gone from
// the request but the summary is present, and that nothing was deleted.
func TestAssembleCompactionReplacesOldHistory(t *testing.T) {
	messages := conversation(6) // seq 1..12
	assembly := Assemble(AssemblyInput{
		Chat:        asmChat("System."),
		Messages:    messages,
		Compactions: []store.Compaction{{ID: "k1", FromSeq: 1, ToSeq: 8, Summary: "Fakten: § 573 BGB"}},
		Limit:       10000,
	})
	if assembly.FirstVerbatimSeq != 9 {
		t.Fatalf("FirstVerbatimSeq = %d, want 9", assembly.FirstVerbatimSeq)
	}
	joined := ""
	for _, m := range assembly.Messages {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "Fakten: § 573 BGB") {
		t.Error("memory summary missing from the request")
	}
	if !strings.Contains(joined, "<conversation_memory>") {
		t.Error("memory summary should be delimited so it is distinguishable")
	}
	// Content from the folded range must be absent, and recent content present.
	if strings.Contains(joined, "Frage Nummer") && strings.Count(joined, "Frage Nummer") > 3 {
		t.Error("folded history is still being sent verbatim")
	}
	if !strings.Contains(joined, strings.Repeat("x", 40)) {
		t.Error("recent history should still be sent verbatim")
	}
}

func TestAssembleFlagsAnOverfullWindow(t *testing.T) {
	assembly := Assemble(AssemblyInput{
		Chat:     asmChat("System."),
		Messages: conversation(40),
		Limit:    1000, Reserve: 500,
	})
	if !assembly.Budget.OverLimit {
		t.Fatal("expected OverLimit for a conversation far larger than the window")
	}
	if assembly.Budget.Percent < 100 {
		t.Fatalf("Percent = %.1f, want >= 100", assembly.Budget.Percent)
	}
}
