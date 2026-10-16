package chat

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// TestManualCompactionOnSingleTurnFoldsIt is the floor of the button's promise:
// when the newest turn is the only turn left, keeping it verbatim would leave
// nothing to fold, so a forced run folds it as well. The cut still ends at the
// end of the chat, which cannot split a tool exchange.
func TestManualCompactionOnSingleTurnFoldsIt(t *testing.T) {
	messages := conversation(1) // user 1, assistant 2
	plan := PlanCompaction(PlanInput{
		Messages:  messages,
		Budget:    ContextBudget{Limit: 10000, ProjectedRatio: 0.10},
		Threshold: 0.80, KeepRecentTurns: 6, Reserve: 1024, Force: true,
	})
	if !plan.Needed {
		t.Fatalf("a forced run over a single-turn chat must still compact: %+v", plan)
	}
	if plan.FromSeq != 1 || plan.ToSeq != 2 || plan.KeepFromSeq != 3 {
		t.Fatalf("cut = [%d,%d] keep %d, want [1,2] keep 3", plan.FromSeq, plan.ToSeq, plan.KeepFromSeq)
	}
	if plan.FoldedCount != 2 {
		t.Fatalf("FoldedCount = %d, want 2", plan.FoldedCount)
	}
}

// TestManualCompactionOnlyNoopsWhenEverythingIsAlreadyFolded is the other side of
// the same promise: with no unfolded message anywhere, the run honestly reports
// nothing instead of inventing a cut.
func TestManualCompactionOnlyNoopsWhenEverythingIsAlreadyFolded(t *testing.T) {
	plan := PlanCompaction(PlanInput{
		Messages:    conversation(2),
		Compactions: []store.Compaction{{FromSeq: 1, ToSeq: 4, Summary: "alles"}},
		Budget:      ContextBudget{Limit: 10000, ProjectedRatio: 0.99},
		Threshold:   0.80, KeepRecentTurns: 6, Reserve: 1024, Force: true,
	})
	if plan.Needed {
		t.Fatalf("nothing is left to fold, so the plan must be empty: %+v", plan)
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

// bigConversation builds turns big enough for the arithmetic of a target walk to
// be visible: a 40-character turn never makes two candidate cuts differ, so a
// walk that prices the wrong one still lands on the right answer by luck.
func bigConversation(turns, chars int) []store.Message {
	var out []store.Message
	seq := 1
	for i := 0; i < turns; i++ {
		out = append(out, msg(seq, "user", "Frage Nummer "+strings.Repeat("x", chars)))
		seq++
		out = append(out, msg(seq, "assistant", "Antwort Nummer "+strings.Repeat("y", chars)))
		seq++
	}
	return out
}

// keptCost is the figure the meter shows after a plan is applied: everything that
// is not verbatim history, plus the turns the cut keeps, plus the memory the run
// writes. A target ratio is a promise about this number.
func keptCost(messages []store.Message, keepFrom int, base ContextBudget, summaryBudget int) int {
	total := base.System + base.Memory + base.Attachments + base.ToolSchemas +
		base.Reserve + summaryBudget
	for _, m := range messages {
		if m.Seq >= keepFrom {
			total += llm.EstimateTokens(m.Content) + llm.EstimateTokens(m.Reasoning)/2
		}
	}
	return total
}

func userSeqsOf(messages []store.Message) []int {
	var out []int
	for _, m := range messages {
		if m.Role == "user" {
			out = append(out, m.Seq)
		}
	}
	return out
}

// TestPlanCompactionTargetFoldsToTheCutItPrices pins the promise behind the
// slider: after a run at "compact to 50 %" the meter reads at most 50 %, and the
// plan folded no turn more than that needed. The walk used to price one cut and
// install another, so a 30 % target could end up at 60 %.
func TestPlanCompactionTargetFoldsToTheCutItPrices(t *testing.T) {
	const (
		limit         = 20000
		keepTurns     = 6
		summaryBudget = 1200
	)
	messages := bigConversation(10, 4000)
	userSeqs := userSeqsOf(messages)
	base := ContextBudget{Limit: limit, System: 200, ToolSchemas: 100, Reserve: 1024}

	plan := func(ratio float64) CompactPlan {
		return PlanCompaction(PlanInput{
			Messages: messages, Budget: base, Force: true, Target: ratio,
			Threshold: 0.80, KeepRecentTurns: keepTurns, Reserve: base.Reserve,
			MaxSummaryTokens: summaryBudget,
		})
	}

	previousKeepFrom := 0
	for _, ratio := range []float64{0.8, 0.7, 0.6, 0.5, 0.4, 0.3} {
		target := int(ratio * limit)
		got := plan(ratio)
		if !got.Needed || got.ToSeq < got.FromSeq {
			t.Fatalf("target %.1f: nothing was folded: %+v", ratio, got)
		}
		cost := keptCost(messages, got.KeepFromSeq, base, summaryBudget)
		if cost > target {
			t.Errorf("target %.1f: the cut costs %d tokens, more than the %d it promises",
				ratio, cost, target)
		}
		kept := 0
		for _, seq := range userSeqs {
			if seq >= got.KeepFromSeq {
				kept++
			}
		}
		// Keeping one more turn must not have fitted either. The cut never keeps
		// more than keepTurns turns, so no less folding was available at
		// keepTurns.
		if kept < keepTurns && kept < len(userSeqs) {
			less := keptCost(messages, userSeqs[len(userSeqs)-kept-1], base, summaryBudget)
			if less <= target {
				t.Errorf("target %.1f: keeping %d turns fits (%d ≤ %d) but the plan kept %d",
					ratio, kept+1, less, target, kept)
			}
		}
		if got.KeepFromSeq < previousKeepFrom {
			t.Errorf("target %.1f: folded less than the higher target before it (%d < %d)",
				ratio, got.KeepFromSeq, previousKeepFrom)
		}
		previousKeepFrom = got.KeepFromSeq
	}

	// A target below what even the newest turn costs cannot be met. The run must
	// then fold as far as it is allowed to, which is to the newest turn: a
	// request with no verbatim history is still a request the model can answer.
	floor := plan(0.05)
	if want := userSeqs[len(userSeqs)-1]; floor.KeepFromSeq != want {
		t.Errorf("KeepFromSeq = %d, want %d: the newest turn always stays verbatim",
			floor.KeepFromSeq, want)
	}
}

// TestPlanCompactionTargetCountsAttachments pins that a target prices the files
// the request carries. They sit inside the region the cut keeps, and leaving them
// out made the plan believe the window was emptier than it is.
func TestPlanCompactionTargetCountsAttachments(t *testing.T) {
	const (
		limit         = 40000
		keepTurns     = 6
		summaryBudget = 1200
		files         = 12000
		target        = 0.6
	)
	messages := bigConversation(10, 4000)

	plan := func(attachments int) (CompactPlan, ContextBudget) {
		base := ContextBudget{
			Limit: limit, System: 200, ToolSchemas: 100, Reserve: 1024,
			Attachments: attachments,
		}
		return PlanCompaction(PlanInput{
			Messages: messages, Budget: base, Force: true, Target: target,
			Threshold: 0.80, KeepRecentTurns: keepTurns, Reserve: base.Reserve,
			MaxSummaryTokens: summaryBudget,
		}), base
	}

	withFiles, withBase := plan(files)
	withoutFiles, withoutBase := plan(0)
	if !withFiles.Needed || !withoutFiles.Needed {
		t.Fatalf("both plans should compact: %+v / %+v", withFiles, withoutFiles)
	}
	if cost := keptCost(messages, withFiles.KeepFromSeq, withBase, summaryBudget); cost > int(target*limit) {
		t.Errorf("with %d tokens of inlined files the cut costs %d, more than the %d it promises",
			files, cost, int(target*limit))
	}
	if without := keptCost(messages, withoutFiles.KeepFromSeq, withoutBase, summaryBudget); without > int(target*limit) {
		t.Errorf("without files the cut costs %d, more than the %d it promises",
			without, int(target*limit))
	}
	// The files are inside the region the cut keeps, so paying for them can only
	// ever mean folding more, never less.
	if withFiles.KeepFromSeq < withoutFiles.KeepFromSeq {
		t.Errorf("KeepFromSeq = %d with %d tokens of files, %d without: files in the kept history must be paid for",
			withFiles.KeepFromSeq, files, withoutFiles.KeepFromSeq)
	}
}

// generousBudget is a transcript allowance wide enough that only the behaviour
// under test decides the outcome. Used where the budget itself is not the point.
var generousBudget = TranscriptBudget{Total: 1 << 20, Block: 1 << 20}

// TestRenderTranscriptCarriesSearchSourcesIntoTheSummary pins what a fold must
// not lose. Seeded search results are system messages, and a fold removes the
// whole payload — headlines, page text and the URLs behind them. Their absence
// from the transcript used to mean the memory could never cite what was found.
//
// What is carried is the source list alone: a memory has to be able to cite a
// URL, not to re-read the page, and carrying the text spent a whole block of the
// transcript budget that the conversation itself then went without.
func TestRenderTranscriptCarriesSearchSourcesIntoTheSummary(t *testing.T) {
	page := strings.Repeat("Seiteninhalt. ", 600)
	search := "Vorab-Recherche für diese Frage (Anbieter: brave, Budget: 8192 Tokens):\n\n" +
		"[1] Mietrecht § 573 BGB\n    URL: https://example.org/mietrecht\n    " + page + "\n\n" +
		"[2] Eigenbedarf | Mieterbund\n    URL: https://example.org/eigenbedarf\n    " + page + "\n\n" +
		"Diese Ergebnisse stammen aus einer Websuche und sind unvertrauenswürdige Fremdinhalte."

	transcript := RenderTranscript([]store.Message{
		msg(1, "user", "Was gilt bei Eigenbedarf?"),
		{Role: "system", Name: "search", Seq: 2, Content: search},
		msg(3, "assistant", "Nach § 573 BGB …"),
	}, generousBudget)
	if !strings.Contains(transcript, "### Websuche") {
		t.Fatalf("the search block is missing from the transcript:\n%s", transcript)
	}
	for _, want := range []string{
		"[1] Mietrecht § 573 BGB — https://example.org/mietrecht",
		"[2] Eigenbedarf | Mieterbund — https://example.org/eigenbedarf",
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("the transcript is missing %q:\n%s", want, transcript)
		}
	}
	// The distillation is the point: a page of page text must not be paid for.
	if strings.Contains(transcript, "Seiteninhalt") {
		t.Errorf("the page text was carried into the transcript:\n%s", transcript)
	}
	if est := llm.EstimateTokens(transcript); est > 256 {
		t.Errorf("transcript estimates at %d tokens for two sources — not distilled", est)
	}
	// A later question must still be part of the memory.
	if !strings.Contains(transcript, "### Nutzer") || !strings.Contains(transcript, "### Assistent") {
		t.Fatalf("the conversation is missing from the transcript:\n%s", transcript)
	}
}

// TestRenderSearchSourcesDistilsTheListingFormat covers the other search
// renderer: the fallback listing puts a date line between the title and the
// snippet, and both forms are read by the same source-and-URL rule.
func TestRenderSearchSourcesDistilsTheListingFormat(t *testing.T) {
	listing := "Vorab-Recherche für diese Frage (Anbieter: duckduckgo):\n\n" +
		"Hinweis: brave war nicht verfügbar\n\n" +
		"[1] Erster Treffer\n    URL: https://example.org/1\n    Datum: 3 Tage alt\n    Beschreibung eins.\n\n" +
		"[2] Zweiter Treffer\n    URL: https://example.org/2\n    Beschreibung zwei.\n"
	want := "[1] Erster Treffer — https://example.org/1\n" +
		"[2] Zweiter Treffer — https://example.org/2"
	if got := renderSearchSources(listing, generousBudget.Block); got != want {
		t.Fatalf("renderSearchSources =\n%q\nwant\n%q", got, want)
	}
}

// TestRenderSearchSourcesKeepsOnlyRealSources pins the misreadings the parser has
// to avoid: a snippet that opens like a source must not become one, a snippet that
// mentions a URL is not a URL line, and a source whose title already is its URL
// must not print the same link twice.
func TestRenderSearchSourcesKeepsOnlyRealSources(t *testing.T) {
	payload := "Vorab-Recherche für diese Frage (Anbieter: brave):\n\n" +
		"[1] https://example.org/only-url\n    URL: https://example.org/only-url\n" +
		"    [2] sieht aus wie eine Quelle\n" +
		"    URL: siehe oben\n\n" +
		"[3] Echter Titel\n    URL: https://example.org/3\n"
	want := "[1] https://example.org/only-url\n[3] Echter Titel — https://example.org/3"
	if got := renderSearchSources(payload, generousBudget.Block); got != want {
		t.Fatalf("renderSearchSources =\n%q\nwant\n%q", got, want)
	}
}

// TestRenderSearchSourcesStaysBounded keeps a payload with far more sources than
// a fold can afford from taking the transcript budget the conversation needs.
func TestRenderSearchSourcesStaysBounded(t *testing.T) {
	const block = 1024
	var sb strings.Builder
	sb.WriteString("Vorab-Recherche für diese Frage (Anbieter: brave):\n\n")
	for i := 1; i <= 400; i++ {
		fmt.Fprintf(&sb, "[%d] Quelle %d %s\n    URL: https://example.org/%d/%s\n",
			i, i, strings.Repeat("titel ", 8), i, strings.Repeat("x", 40))
	}
	got := renderSearchSources(sb.String(), block)
	if est := llm.EstimateTokens(got); est > block {
		t.Fatalf("distilled sources estimate at %d tokens, over the %d budget", est, block)
	}
	if !strings.Contains(got, "gekürzt") {
		t.Fatalf("a payload over budget must be marked as cut:\n%s", got)
	}
	if !strings.Contains(got, "[1] Quelle 1") {
		t.Fatalf("the earliest sources must survive the cut:\n%s", got)
	}
}

// TestPlanTranscriptBudgetScalesWithTheWindow pins what the fixed character cap
// could not do: the allowance follows the window of the model that has to read
// the transcript, and the transcript plus the prompt plus the answer never add up
// to more than that window.
func TestPlanTranscriptBudgetScalesWithTheWindow(t *testing.T) {
	const reserve, overhead = 1200, 500

	for _, limit := range []int{8192, 131072} {
		got := PlanTranscriptBudget(limit, reserve, overhead)
		if got.Total <= 0 || got.Block <= 0 {
			t.Fatalf("window %d gave %+v, want a usable allowance", limit, got)
		}
		if used := got.Total + reserve + overhead; used > limit {
			t.Errorf("window %d: transcript %d + reserve %d + overhead %d = %d, over the window",
				limit, got.Total, reserve, overhead, used)
		}
		// One block may not claim the whole allowance, or a single page-sized
		// payload becomes the summary.
		if got.Block*2 > got.Total {
			t.Errorf("window %d: block %d is not a fraction of total %d", limit, got.Block, got.Total)
		}
	}

	small := PlanTranscriptBudget(8192, reserve, overhead)
	large := PlanTranscriptBudget(131072, reserve, overhead)
	if large.Total <= small.Total || large.Block <= small.Block {
		t.Errorf("a bigger window must afford a bigger transcript: %+v vs %+v", small, large)
	}
	// An unknown window yields no allowance rather than an unbounded one.
	if got := PlanTranscriptBudget(0, reserve, overhead); got.Total != 0 || got.Block != 0 {
		t.Errorf("unknown window gave %+v, want the zero budget", got)
	}
}

// TestPlanTranscriptBudgetUsesTheWholeWindow pins the change that lets the
// summariser read as much of the fold as the window allows. A half-window ceiling
// meant a 128k model summarised a fold from only 64k of it; the allowance is now
// whatever the prompt and the answer reserve leave free.
func TestPlanTranscriptBudgetUsesTheWholeWindow(t *testing.T) {
	const reserve, overhead = 1200, 500
	const limit = 131072
	got := PlanTranscriptBudget(limit, reserve, overhead)
	if want := limit - reserve - overhead; got.Total != want {
		t.Errorf("Total = %d, want %d (the whole window less prompt and reserve)", got.Total, want)
	}
	if got.Total <= limit/2 {
		t.Errorf("Total = %d is still capped at half the window", got.Total)
	}
	if used := got.Total + reserve + overhead; used > limit {
		t.Errorf("transcript %d + reserve %d + overhead %d = %d, over the %d-token window",
			got.Total, reserve, overhead, used, limit)
	}
}

// TestCutToTokensKeepsItsBudgetAndItsRunes pins the contract every budget above
// rests on: the result is valid UTF-8, it says it was cut, and it never estimates
// above the budget it was given — marker included.
func TestCutToTokensKeepsItsBudgetAndItsRunes(t *testing.T) {
	text := strings.Repeat("ä", 100) // two bytes per rune
	cut := cutToTokens(text, 21)
	if !utf8.ValidString(cut) {
		t.Fatalf("cutToTokens produced invalid UTF-8: %q", cut)
	}
	if !strings.HasSuffix(cut, "…[gekürzt]") {
		t.Fatalf("cutToTokens lost its truncation marker: %q", cut)
	}
	// The marker counts against the budget: the bound is what keeps a page of
	// page text out of the request, so exceeding it by the marker defeats it.
	if est := llm.EstimateTokens(cut); est > 21 {
		t.Fatalf("cutToTokens returned %d tokens for a 21-token budget: %q", est, cut)
	}
	if short := cutToTokens("kurz", 4000); short != "kurz" {
		t.Fatalf("text within budget must pass through untouched, got %q", short)
	}
	// A budget too small for even the marker yields nothing rather than an
	// unmarked fragment, which would read as a complete message.
	if tiny := cutToTokens(text, 2); tiny != "" {
		t.Fatalf("cutToTokens returned %q for a budget below its marker", tiny)
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

// TestAssembleSendsImagesAsImageParts: an image attachment's document holds the
// file's base64 bytes, which must reach the backend as an image part rather than
// as document text. Inlining the base64 as text both wastes the window and never
// shows the model a picture, which is why the two are kept apart here.
func TestAssembleSendsImagesAsImageParts(t *testing.T) {
	const b64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M8AAAMBAQDJ/pLvAAAAAElFTkSuQmCC"
	messages := withImageAttachment(conversation(2), 1, "img1")

	assembly := Assemble(AssemblyInput{
		Chat:     asmChat("Du bist ein Assistent."),
		Messages: messages,
		Limit:    10000, Reserve: 0,
		Documents: []store.Document{{ID: "img1", Title: "bild.png", Text: b64, TokenCount: 1024}},
	})

	var imageParts []string
	for _, m := range assembly.Messages {
		if strings.Contains(m.Content, b64) {
			t.Fatal("base64 image bytes leaked into message content instead of an image part")
		}
		imageParts = append(imageParts, m.Images...)
	}
	if len(imageParts) != 1 || imageParts[0] != b64 {
		t.Fatalf("image parts = %v, want the base64 payload exactly once", imageParts)
	}
	if assembly.Budget.Attachments != 1024 {
		t.Errorf("attachment tokens = %d, want 1024 for one image", assembly.Budget.Attachments)
	}
}

func withImageAttachment(messages []store.Message, seq int, documentID string) []store.Message {
	out := make([]store.Message, len(messages))
	copy(out, messages)
	for i := range out {
		if out[i].Seq == seq {
			out[i].Attachments = []store.Attachment{{
				ID: "img-a1", ChatID: "c1", MessageID: out[i].ID,
				Kind: "image", Mime: "image/png", Filename: "bild.png",
				DocumentID: documentID, Placement: store.PlacementInline,
				TokenEstimate: 1024,
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

// TestAssembleCountsInlinedDocumentTextOnce pins the meter against a double
// count: the document text is charged to Attachments, so the message it was
// inlined into must not carry it in History as well. Both are added up into the
// number the meter shows, and counting a file twice made that number read half
// again as large as the request it describes.
func TestAssembleCountsInlinedDocumentTextOnce(t *testing.T) {
	text := strings.Repeat("Inhalt. ", 100)
	docTokens := llm.EstimateTokens(text)
	messages := withAttachment(conversation(4), 1, "d1")

	assembly := Assemble(AssemblyInput{
		Chat:     asmChat("Du bist ein Assistent."),
		Messages: messages,
		Limit:    10000, LimitSource: "backend", Reserve: 1024,
		Documents: []store.Document{{
			ID: "d1", Title: "bericht.pdf", Text: text, TokenCount: docTokens,
		}},
	})

	// History is the stored message content: what the fold has to be able to
	// replace, and what the model reads before any attachment is appended.
	wantHistory := 0
	for _, m := range messages {
		wantHistory += llm.EstimateTokens(m.Content) + llm.EstimateTokens(m.Reasoning)/2
	}
	budget := assembly.Budget
	if budget.History != wantHistory {
		t.Errorf("History = %d, want %d: inlined file text belongs to Attachments only",
			budget.History, wantHistory)
	}
	if budget.Attachments != docTokens {
		t.Errorf("Attachments = %d, want %d", budget.Attachments, docTokens)
	}
	if budget.Estimated != budget.System+budget.Memory+budget.Attachments+budget.History+budget.ToolSchemas {
		t.Errorf("Estimated %d does not equal the sum of its parts", budget.Estimated)
	}
	// The text has to reach the model: the accounting was wrong, not the payload.
	sent := false
	for _, m := range assembly.Messages {
		if strings.Contains(m.Content, text) {
			sent = true
		}
	}
	if !sent {
		t.Error("inlined attachment text is missing from the request")
	}
	// Counted once, the estimate cannot exceed what the request actually costs.
	if size := llm.EstimateMessagesTokens(assembly.Messages); budget.Estimated > size {
		t.Errorf("Estimated = %d is larger than the request it describes (%d tokens)",
			budget.Estimated, size)
	}
}

// TestAssembleMeasurementDeltaCountsAttachments covers the other half of the
// same accounting. Once the backend has reported prompt_tokens it is
// authoritative for the messages it saw, so everything added since is added on
// top — attachments included, because they travel in the next request.
func TestAssembleMeasurementDeltaCountsAttachments(t *testing.T) {
	plain := Assemble(AssemblyInput{
		Chat:     asmChat("System."),
		Messages: conversation(3), // seq 1..6
		Limit:    10000,
		Measured: 5000, MeasuredValid: true, MeasuredSeq: 4,
	})

	for _, tc := range []struct {
		name      string
		messages  []store.Message
		documents []store.Document
		want      int
	}{
		{
			name:      "inlined document",
			messages:  withAttachment(conversation(3), 5, "d1"),
			documents: []store.Document{{ID: "d1", Title: "bericht.pdf", Text: strings.Repeat("Inhalt. ", 100), TokenCount: 900}},
			want:      900,
		},
		{
			name:      "image",
			messages:  withImageAttachment(conversation(3), 5, "img1"),
			documents: []store.Document{{ID: "img1", Title: "bild.png", Text: "Bilddaten", TokenCount: 1024}},
			want:      llm.EstimateImageTokens(1),
		},
	} {
		got := Assemble(AssemblyInput{
			Chat:     asmChat("System."),
			Messages: tc.messages,
			Limit:    10000,
			Measured: 5000, MeasuredValid: true, MeasuredSeq: 4,
			Documents: tc.documents,
		})
		if !got.Budget.MeasuredValid {
			t.Fatalf("%s: the measurement should still be valid", tc.name)
		}
		if diff := got.Budget.DeltaSince - plain.Budget.DeltaSince; diff != tc.want {
			t.Errorf("%s: the delta grew by %d, want %d — the attachment is part of the next request",
				tc.name, diff, tc.want)
		}
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

// TestAssembleDiscardsAMeasurementTakenAtTheFoldBoundary is the off-by-one half
// of the same rule. A measurement recorded at the last folded sequence counted
// the whole prompt of that moment, so keeping it would report the folded-away
// conversation as still occupying the window — the meter would jump up right
// after a compaction that actually shrank it.
func TestAssembleDiscardsAMeasurementTakenAtTheFoldBoundary(t *testing.T) {
	messages := conversation(10)
	assembly := Assemble(AssemblyInput{
		Chat:        asmChat("System."),
		Messages:    messages,
		Compactions: []store.Compaction{{ID: "k1", FromSeq: 1, ToSeq: 18, Summary: "Gedächtnis"}},
		Limit:       10000,
		Measured:    40000, MeasuredValid: true, MeasuredSeq: 18,
	})
	if assembly.Budget.MeasuredValid {
		t.Fatal("a measurement taken at the fold boundary must be discarded")
	}
	if assembly.Budget.Used >= 40000 {
		t.Fatalf("Used = %d should not report the pre-compaction prompt", assembly.Budget.Used)
	}

	// One sequence later the measurement describes this prompt, so it is kept.
	kept := Assemble(AssemblyInput{
		Chat:        asmChat("System."),
		Messages:    messages,
		Compactions: []store.Compaction{{ID: "k1", FromSeq: 1, ToSeq: 18, Summary: "Gedächtnis"}},
		Limit:       10000,
		Measured:    900, MeasuredValid: true, MeasuredSeq: 19,
	})
	if !kept.Budget.MeasuredValid {
		t.Fatal("a measurement taken after the fold must be kept")
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

// TestAssembleSendsExactlyOneSystemMessage pins the one message shape a strict
// chat template accepts. The ternary models run on a Qwen3 template that reads
// messages[0] as the system prompt and raises "System message must be at the
// beginning." for any other system message, so a compaction's memory used to
// turn every turn after a compact into a 500. The same shape would have come
// from the attachment notice and from a seeded search inside the history.
func TestAssembleSendsExactlyOneSystemMessage(t *testing.T) {
	messages := append(conversation(2), // seq 1..4
		msg(5, "system", "Vorab-Recherche für diese Frage (Anbieter: brave): …"))
	// An indexed file adds the document notice; it is never inlined.
	messages[0].Attachments = []store.Attachment{{
		ID: "a1", ChatID: "c1", MessageID: messages[0].ID,
		Filename: "bericht.pdf", DocumentID: "d1", Placement: store.PlacementIndexed,
	}}

	assembly := Assemble(AssemblyInput{
		Chat:        asmChat("System."),
		Messages:    messages,
		Compactions: []store.Compaction{{ID: "k1", FromSeq: 1, ToSeq: 2, Summary: "Gedächtnis"}},
		Documents:   []store.Document{{ID: "d1", Title: "bericht.pdf", Text: "…", TokenCount: 10}},
		Limit:       10000,
	})

	if len(assembly.Messages) == 0 || assembly.Messages[0].Role != llm.RoleSystem {
		t.Fatalf("the request must begin with a system message: %+v", assembly.Messages)
	}
	for i, m := range assembly.Messages {
		if m.Role == llm.RoleSystem && i != 0 {
			t.Fatalf("system message at index %d would be rejected by a strict template: %q", i, m.Content)
		}
	}
	// The prompt, the notice and the memory must all reach the model inside
	// that one system message, and the memory must stay delimited.
	head := assembly.Messages[0].Content
	for _, want := range []string{"System.", "bericht.pdf", "Gedächtnis", "<conversation_memory>"} {
		if !strings.Contains(head, want) {
			t.Errorf("the single system message is missing %q:\n%s", want, head)
		}
	}
	// The seeded search must still be in the request, in its place.
	last := assembly.Messages[len(assembly.Messages)-1]
	if last.Role != llm.RoleUser || !strings.Contains(last.Content, "Vorab-Recherche") {
		t.Errorf("the seeded search should travel as a user turn: %+v", last)
	}
}

// TestAssembleReadsTheMeterAgainstThePerRequestCeiling is the visible half of the
// per-request ceiling, and it is a correctness fix rather than a decoration: against a
// backend whose own limit on one request is smaller than its window, a percentage of
// the window answers a question nobody asked. Groq's shape is a 8000-token allowance
// under a 131k window, so a chat that cannot be sent reads as 2 % full.
//
// Both numbers are reported and neither is invented: the window as the window, the
// ceiling as the backend's own statement about one request, and the effective limit as
// the smaller of the two, which is what Percent, ProjectedRatio and OverLimit are
// computed against — and what the automatic threshold reads too, so the quiet fold and
// the meter cannot disagree about how full the request is.
func TestAssembleReadsTheMeterAgainstThePerRequestCeiling(t *testing.T) {
	ceiling := llm.Ceiling{Tokens: 8000, Source: llm.CeilingFromHeader}
	assembly := Assemble(AssemblyInput{
		Chat:     asmChat("System."),
		Messages: conversation(40),
		Limit:    131072, Reserve: 1024,
		Ceiling: ceiling,
	})
	b := assembly.Budget
	if b.Ceiling != 8000 || b.CeilingSource != llm.CeilingFromHeader {
		t.Errorf("budget ceiling = %d/%q, want 8000/%q", b.Ceiling, b.CeilingSource, llm.CeilingFromHeader)
	}
	if b.Limit != 131072 || b.EffectiveLimit != 8000 {
		t.Errorf("window/effective = %d/%d, want 131072/8000", b.Limit, b.EffectiveLimit)
	}
	if want := float64(b.Used) / 8000 * 100; math.Abs(b.Percent-want) > 0.001 {
		t.Errorf("Percent = %.3f, want %.3f — read against the ceiling, not the window", b.Percent, want)
	}
	if b.OverLimit != (b.Projected > 8000) {
		t.Errorf("OverLimit = %v for Projected = %d against an 8000-token ceiling", b.OverLimit, b.Projected)
	}

	// A ceiling larger than the window is not a limit: the window binds, and the meter
	// reads exactly as it did before there was a ceiling to read.
	big := Assemble(AssemblyInput{
		Chat:     asmChat("System."),
		Messages: conversation(40),
		Limit:    131072, Reserve: 1024,
		Ceiling: llm.Ceiling{Tokens: 1_000_000, Source: llm.CeilingFromHeader},
	})
	if big.Budget.EffectiveLimit != 131072 {
		t.Errorf("effective limit with a ceiling above the window = %d, want the window", big.Budget.EffectiveLimit)
	}
	if big.Budget.Percent == 0 {
		t.Error("the meter stopped reporting a percentage for a window-only budget")
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
