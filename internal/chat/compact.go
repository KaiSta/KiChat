package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"kichat/internal/llm"
	"kichat/internal/store"
)

// Complete runs a generation to completion without streaming it to the UI. Used
// for compaction summaries and auto-titling.
func Complete(ctx context.Context, p llm.Provider, req llm.ChatRequest) (string, *llm.Usage, error) {
	events, err := p.Chat(ctx, req)
	if err != nil {
		return "", nil, err
	}
	var sb strings.Builder
	var usage *llm.Usage
	for ev := range events {
		switch ev.Type {
		case llm.EventDelta:
			sb.WriteString(ev.Text)
		case llm.EventUsage:
			usage = ev.Usage
		case llm.EventError:
			return sb.String(), usage, errors.New(ev.Error)
		case llm.EventDone:
			// terminal
		}
	}
	if ctx.Err() != nil {
		return sb.String(), usage, ctx.Err()
	}
	return sb.String(), usage, nil
}

// PlanInput describes the state compaction is planned against.
type PlanInput struct {
	Messages    []store.Message
	Compactions []store.Compaction
	Budget      ContextBudget

	Threshold        float64
	KeepRecentTurns  int
	Reserve          int
	MaxSummaryTokens int

	// Force runs compaction even when the threshold is not reached (manual
	// compaction). Target is the desired usage ratio afterwards, 0 for "as much
	// as possible".
	Force  bool
	Target float64

	// MeasuredTokens is the authoritative used-token figure, used for the
	// before/after reporting.
	MeasuredTokens int
}

// CompactPlan is the decision of what to fold.
type CompactPlan struct {
	Needed bool
	Reason string

	FromSeq int
	ToSeq   int

	// KeepFromSeq is the first sequence number that stays verbatim.
	KeepFromSeq int
	// FoldedCount is how many messages would be replaced by the summary.
	FoldedCount int
	// TokensBefore / TokensAfterEstimate allow an honest before/after report.
	TokensBefore        int
	TokensAfterEstimate int
}

// PlanCompaction decides whether to compact and where to cut. Cut points are
// always aligned to user-message boundaries so a tool call is never separated
// from its result — that would produce a request the backend rejects.
func PlanCompaction(in PlanInput) CompactPlan {
	keepTurns := in.KeepRecentTurns
	if keepTurns < 1 {
		keepTurns = 1
	}

	var plan CompactPlan
	if !in.Force {
		if in.Budget.Limit <= 0 {
			return plan
		}
		threshold := in.Threshold
		if threshold <= 0 {
			threshold = 0.80
		}
		if in.Budget.ProjectedRatio < threshold {
			return plan
		}
		plan.Reason = fmt.Sprintf("Kontext bei %.0f%% (Schwelle %.0f%%)",
			in.Budget.ProjectedRatio*100, threshold*100)
	} else {
		plan.Reason = "manuell ausgelöst"
	}

	// The oldest message is always the start of what we may fold.
	firstSeq := 0
	if len(in.Messages) > 0 {
		firstSeq = in.Messages[0].Seq
	}
	// A previous compaction already folded everything up to its ToSeq.
	prevTo := 0
	for _, c := range in.Compactions {
		if c.ToSeq > prevTo {
			prevTo = c.ToSeq
		}
	}
	fromSeq := prevTo + 1
	if fromSeq < firstSeq {
		fromSeq = firstSeq
	}

	// Cut at the user-turn boundary that leaves keepTurns turns verbatim.
	var userSeqs []int
	for _, m := range in.Messages {
		if m.Seq >= fromSeq && m.Role == "user" {
			userSeqs = append(userSeqs, m.Seq)
		}
	}
	keepFrom := fromSeq
	if len(userSeqs) > keepTurns {
		keepFrom = userSeqs[len(userSeqs)-keepTurns]
	}
	toSeq := keepFrom - 1

	if toSeq < fromSeq {
		plan.Needed = false
		if plan.Reason == "" {
			plan.Reason = "nichts zu komprimieren"
		} else {
			plan.Reason += " — aber kein Verlauf zum Komprimieren"
		}
		return plan
	}

	// Manual compaction with a target ratio: pull the cut later until the
	// estimate fits, so "compact to 50%" actually means it.
	if in.Force && in.Target > 0 && in.Budget.Limit > 0 {
		budgetTarget := int(in.Target * float64(in.Budget.Limit))
		remaining := in.Budget.System + in.Budget.Memory + in.Budget.ToolSchemas
		for _, m := range in.Messages {
			if m.Seq < keepFrom {
				continue
			}
			remaining += llm.EstimateTokens(m.Content) + llm.EstimateTokens(m.Reasoning)/2
		}
		summaryBudget := in.MaxSummaryTokens
		if summaryBudget <= 0 {
			summaryBudget = 1200
		}
		// Walk the cut point backwards (folding more) until it fits.
		for i := len(userSeqs) - 1; i >= 0; i-- {
			if remaining+summaryBudget <= budgetTarget {
				break
			}
			if userSeqs[i]-1 < fromSeq {
				break
			}
			toSeq = userSeqs[i] - 1
			keepFrom = userSeqs[i]
			remaining -= llm.EstimateTokens(seqContent(in.Messages, userSeqs[i]))
		}
	}

	folded := 0
	before := 0
	for _, m := range in.Messages {
		if m.Seq < fromSeq || m.Seq > toSeq {
			continue
		}
		folded++
		before += llm.EstimateTokens(m.Content) + llm.EstimateTokens(m.Reasoning)/2
	}
	if folded == 0 {
		return plan
	}

	summaryEstimate := in.MaxSummaryTokens
	if summaryEstimate <= 0 {
		summaryEstimate = 1200
	}
	// A summary we instruct the model to keep terse should not be budgeted as
	// larger than what it replaces, or compaction would appear to save nothing.
	if half := before / 2; half < summaryEstimate {
		summaryEstimate = half
	}
	if summaryEstimate < 1 {
		summaryEstimate = 1
	}

	plan.Needed = true
	plan.FromSeq = fromSeq
	plan.ToSeq = toSeq
	plan.KeepFromSeq = keepFrom
	plan.FoldedCount = folded
	plan.TokensBefore = before
	plan.TokensAfterEstimate = summaryEstimate
	return plan
}

func seqContent(messages []store.Message, seq int) string {
	for _, m := range messages {
		if m.Seq == seq {
			return m.Content + m.Reasoning
		}
	}
	return ""
}

// summarySystemPrompt drives the summarisation call. It is deliberately explicit
// about preservation: a compaction that loses a URL or a number silently
// corrupts every later answer.
const summarySystemPrompt = `Du komprimierst den Verlauf eines Recherche-Chats zu einem dauerhaften Gedächtnis.

Regeln:
- Schreibe in der Sprache des Verlaufs.
- Übernimm Namen, Zahlen, Daten, Preise, Gesetzesangaben und URLs WÖRTLICH. Kürze oder paraphrasiere sie nicht.
- Erfinde nichts. Wenn etwas unklar blieb, schreibe es unter "Offene Punkte".
- Fasse dich kurz: Stichpunkte, keine Prosa, keine Wiederholungen.

Antworte ausschließlich mit Markdown in genau dieser Struktur:

## Festgestellte Fakten
- ...
## Entscheidungen
- ...
## Offene Punkte
- ...
## Quellen
- [Titel](URL)
## Dateien & Dokumente
- ...
## Vorgaben & Präferenzen
- ...

Lasse einen Abschnitt weg, wenn er leer ist.`

// Summarize folds the plan's range into a memory summary. When a previous
// summary exists it is merged in, which keeps the cost of repeated compaction
// linear in the number of messages rather than quadratic.
func Summarize(ctx context.Context, p llm.Provider, model string, opts SummarizeOptions) (string, error) {
	if p == nil {
		return "", errors.New("kein Backend verfügbar")
	}
	var sb strings.Builder
	if opts.PreviousSummary != "" {
		sb.WriteString("<existing_memory>\n")
		sb.WriteString(opts.PreviousSummary)
		sb.WriteString("\n</existing_memory>\n\n")
		sb.WriteString("Der folgende Verlauf schließt an das bestehende Gedächtnis an. ")
		sb.WriteString("Führe beides zu EINEM Gedächtnis zusammen, ohne Informationen zu verlieren.\n\n")
	} else {
		sb.WriteString("Komprimiere den folgenden Verlauf zu einem Gedächtnis.\n\n")
	}
	if opts.Focus != "" {
		sb.WriteString("Besonderer Fokus des Nutzers: ")
		sb.WriteString(opts.Focus)
		sb.WriteString("\n\n")
	}
	sb.WriteString("<conversation>\n")
	sb.WriteString(opts.Transcript)
	sb.WriteString("\n</conversation>")

	temp := opts.Temperature
	if temp <= 0 {
		temp = 0.2
	}
	req := llm.ChatRequest{
		Model: model,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: summarySystemPrompt},
			{Role: llm.RoleUser, Content: sb.String()},
		},
		Temperature: temp,
		MaxTokens:   opts.MaxTokens,
		NumCtx:      opts.NumCtx,
	}
	text, _, err := Complete(ctx, p, req)
	if err != nil {
		return "", fmt.Errorf("Zusammenfassung fehlgeschlagen: %w", err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("das Modell hat eine leere Zusammenfassung geliefert")
	}
	return text, nil
}

// SummarizeOptions configures a summarisation call.
type SummarizeOptions struct {
	PreviousSummary string
	Transcript      string
	Focus           string
	Temperature     float64
	MaxTokens       int
	NumCtx          int
}

// RenderTranscript flattens messages into a plain transcript for summarisation,
// marking tool activity so the summary can record what was actually looked up.
func RenderTranscript(messages []store.Message) string {
	var sb strings.Builder
	for _, m := range messages {
		switch m.Role {
		case "system":
			continue
		case "user":
			if m.Content == "" {
				continue
			}
			fmt.Fprintf(&sb, "\n### Nutzer\n%s\n", m.Content)
		case "assistant":
			body := m.Content
			if m.Reasoning != "" && body == "" {
				body = "(nur Reasoning)"
			}
			if body == "" && m.ToolCallsJSON == "" {
				continue
			}
			fmt.Fprintf(&sb, "\n### Assistent\n%s\n", body)
			if m.ToolCallsJSON != "" {
				fmt.Fprintf(&sb, "(Tool-Aufruf: %s)\n", m.ToolCallsJSON)
			}
		case "tool":
			content := m.Content
			if len(content) > 4000 {
				content = content[:4000] + "\n…[gekürzt]"
			}
			fmt.Fprintf(&sb, "\n### Tool-Ergebnis\n%s\n", content)
		}
	}
	return sb.String()
}
