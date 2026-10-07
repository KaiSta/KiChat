package chat

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"kichat/internal/llm"
	"kichat/internal/store"
)

// completion is the outcome of a generation run to its end rather than streamed.
// The reasoning and the stop reason are kept alongside the text because a
// reasoning model can spend its whole output budget on private thinking and never
// write an answer: without them an empty result is indistinguishable from a model
// that simply said nothing, and the cause (and its fix) cannot be named. See
// Summarize.
type completion struct {
	text       string
	reasoning  string
	stopReason string
	usage      *llm.Usage
}

// runToCompletion drives a generation to its last event and collects everything a
// caller might need to interpret the result. The text collected before an error
// is kept, as the previous implementation did, so a partial answer is never lost.
func runToCompletion(ctx context.Context, p llm.Provider, req llm.ChatRequest) (completion, error) {
	events, err := p.Chat(ctx, req)
	if err != nil {
		return completion{}, err
	}
	var out completion
	var text, reasoning strings.Builder
	for ev := range events {
		switch ev.Type {
		case llm.EventDelta:
			text.WriteString(ev.Text)
		case llm.EventReasoning:
			reasoning.WriteString(ev.Text)
		case llm.EventUsage:
			out.usage = ev.Usage
		case llm.EventDone:
			if ev.Stats != nil {
				out.stopReason = ev.Stats.StopReason
			}
		case llm.EventError:
			out.text, out.reasoning = text.String(), reasoning.String()
			return out, errors.New(ev.Error)
		}
	}
	out.text, out.reasoning = text.String(), reasoning.String()
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	return out, nil
}

// Complete runs a generation to completion without streaming it to the UI. Used
// for compaction summaries and auto-titling.
func Complete(ctx context.Context, p llm.Provider, req llm.ChatRequest) (string, *llm.Usage, error) {
	out, err := runToCompletion(ctx, p, req)
	return out.text, out.usage, err
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

	// A forced manual compaction should still produce a real cut when the chat is
	// shorter than keepTurns: otherwise the button returns "nothing to compact"
	// even though there is history that could be summarised.
	if toSeq < fromSeq {
		if !in.Force {
			plan.Needed = false
			if plan.Reason == "" {
				plan.Reason = "nichts zu komprimieren"
			} else {
				plan.Reason += " — aber kein Verlauf zum Komprimieren"
			}
			return plan
		}
		// Fold everything up to the newest user turn, which is the last turn that
		// could contain a tool exchange we are not allowed to split.
		if len(userSeqs) > 0 {
			keepFrom = userSeqs[len(userSeqs)-1]
		}
		toSeq = keepFrom - 1
	}

	// Manual compaction with a target ratio: pull the cut later until the
	// estimate fits, so "compact to 50%" actually means it. The reserve counts
	// towards the target, because the number the meter shows for a chat after this
	// run (`projected`) includes it — the auto threshold reads the projected ratio,
	// so pricing the manual target without the reserve would make the same
	// percentage mean two different window states.
	if in.Force && in.Target > 0 && in.Budget.Limit > 0 && toSeq >= fromSeq {
		budgetTarget := int(in.Target * float64(in.Budget.Limit))
		remaining := in.Budget.System + in.Budget.Memory + in.Budget.ToolSchemas + in.Reserve
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
		// Folding a turn removes the whole turn, so each candidate cut point is
		// priced with its answer and tool traffic rather than only the prompt that
		// starts it. Counting just the user message makes the estimate believe it
		// has folded less than it has, and it then folds turns it did not need to.
		turnTokens := turnTokenSpans(in.Messages, userSeqs)
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
			remaining -= turnTokens[i]
		}
	}

	// A manual compaction is a promise that the context gets compacted, so it
	// must never end as a silent no-op while foldable history exists. That is the
	// state right after a previous compaction with a single new turn: every older
	// turn is already folded and only the newest one is left, which the cut above
	// keeps verbatim. Extending the cut to the end of the chat folds that turn
	// too, and the end of the chat is always a valid cut point — nothing follows
	// it, so no tool call is separated from its result.
	if in.Force && toSeq < fromSeq {
		if last := lastMessageSeq(in.Messages); last >= fromSeq {
			toSeq = last
			keepFrom = last + 1
		}
	}

	// A message that is still streaming is being written right now: its content is
	// half an answer, and it is replaced in place when the turn finishes. Folding
	// it would freeze that fragment into the memory, so the cut stops before the
	// turn that owns it — the question has to stay with its answer, and the tail of
	// the prompt must not begin with an assistant message. With nothing finished
	// older than that turn, there is nothing to fold at all.
	if start, streaming := runningTurn(in.Messages); streaming {
		// No owner found means the reply being written has no question in front of
		// it, so no cut can be proven safe. Fold nothing rather than guess.
		if start <= fromSeq {
			return plan
		}
		if toSeq > start-1 {
			toSeq = start - 1
			keepFrom = start
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

// runningTurn reports the user message that owns the reply currently being
// written, and whether any reply is streaming at all. start is 0 when the reply
// being written has no question in front of it.
func runningTurn(messages []store.Message) (start int, streaming bool) {
	reply := 0
	for _, m := range messages {
		if m.Status == store.StatusStreaming {
			reply = m.Seq
			break
		}
	}
	if reply == 0 {
		return 0, false
	}
	for _, m := range messages {
		if m.Role == "user" && m.Seq < reply && m.Seq > start {
			start = m.Seq
		}
	}
	return start, true
}

// lastMessageSeq returns the highest sequence number in the conversation.
func lastMessageSeq(messages []store.Message) int {
	last := 0
	for _, m := range messages {
		if m.Seq > last {
			last = m.Seq
		}
	}
	return last
}

// turnTokenSpans prices each user turn: the user message, its answer, and any
// tool traffic before the next user turn. Folding a turn removes all of it, so
// the cost of a cut point has to be measured over the whole span.
func turnTokenSpans(messages []store.Message, userSeqs []int) []int {
	last := lastMessageSeq(messages)
	spans := make([]int, len(userSeqs))
	for i, seq := range userSeqs {
		end := last
		if i+1 < len(userSeqs) {
			end = userSeqs[i+1] - 1
		}
		for _, m := range messages {
			if m.Seq < seq || m.Seq > end {
				continue
			}
			spans[i] += llm.EstimateTokens(m.Content) + llm.EstimateTokens(m.Reasoning)/2
		}
	}
	return spans
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

// summaryOutputTokens is the summariser's default output cap. It is deliberately
// generous and independent of the target summary size: `max_tokens` bounds a
// reasoning model's private thinking and its answer together, so a cap sized to
// the summary alone is spent entirely on the thinking and returns no summary at
// all — the "empty summary" failure, and the one Cline raised its own default from
// 4096 to 8192 to fix. The model is still instructed to be terse, so it normally
// stops far short; the cap only stops a runaway generation from filling the
// window. It is a cap, not a target.
const summaryOutputTokens = 8192

// noThinkTag is the in-prompt soft switch (Qwen3 and compatibles honour it) that
// turns reasoning off without any backend parameter. It is the only lever that
// reaches a local backend with no reasoning control — LM Studio's OpenAI-compatible
// endpoint among them — where the request has nowhere to put "thinking: off".
const noThinkTag = "/no_think"

// summaryMinTranscriptTokens is the least transcript the summariser may be left
// with when the window is small. The output cap above is trimmed only far enough
// to leave this much room, so a small window still gives the model something to
// read — the cap is never held to a fraction of the window.
const summaryMinTranscriptTokens = 1024

// summaryAttempts bounds how often a summarisation is retried when a thinking
// model keeps spending its output budget before it writes a word of the answer.
// Each retry grows the budget by the reasoning observed, so one or two is normally
// enough; the cap keeps a model whose thinking keeps getting longer from looping.
const summaryAttempts = 3

// Summarize folds the plan's range into a memory summary. When a previous
// summary exists it is merged in, which keeps the cost of repeated compaction
// linear in the number of messages rather than quadratic.
//
// A summary is not a reasoning task, so thinking is switched off where the
// backend allows it. Where it does not, a thinking model can still spend the
// whole output budget thinking and return nothing — `max_tokens` bounds thinking
// and answer together — so the call is retried with enough room to finish that
// thinking AND write the summary, measured from the reasoning that actually came
// back. The content itself stays bounded: the extra budget goes to the thinking,
// not to a larger memory.
func Summarize(ctx context.Context, p llm.Provider, model string, opts SummarizeOptions) (string, error) {
	if p == nil {
		return "", errors.New("kein Backend verfügbar")
	}
	temp := opts.Temperature
	if temp <= 0 {
		temp = 0.2
	}
	// Ask the model not to think. A summary is a mechanical task, and `max_tokens`
	// bounds thinking and answer together, so a reasoning model that thinks first
	// can spend the whole budget before writing a word. `/no_think` is the in-prompt
	// soft switch — the only lever that reaches a local backend with no reasoning
	// parameter (LM Studio). A model that does not know the tag treats it as an
	// ignorable trailing note.
	userPrompt := summarisePrompt(opts) + "\n\n" + noThinkTag
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: summarySystemPrompt},
		{Role: llm.RoleUser, Content: userPrompt},
	}
	promptTokens := llm.EstimateMessagesTokens(messages)

	answer := opts.MaxTokens
	if answer <= 0 {
		answer = 1024
	}
	budget := answer
	var last completion
	for attempt := 0; attempt < summaryAttempts; attempt++ {
		req := llm.ChatRequest{
			Model:       model,
			Messages:    messages,
			Temperature: temp,
			MaxTokens:   budget,
			NumCtx:      opts.NumCtx,
		}
		if opts.DisableThinking {
			off := false
			req.Think = &off
		}
		out, err := runToCompletion(ctx, p, req)
		if err != nil {
			return "", fmt.Errorf("Zusammenfassung fehlgeschlagen: %w", err)
		}
		if text := strings.TrimSpace(out.text); text != "" {
			return text, nil
		}
		last = out
		// Only a model that reasoned and ran out of budget is worth another look;
		// one that simply said nothing will say nothing again.
		if out.reasoning == "" && !strings.EqualFold(out.stopReason, "length") {
			break
		}
		grown := llm.EstimateTokens(out.reasoning) + answer
		if opts.ContextLimit > 0 {
			if room := opts.ContextLimit - promptTokens; room > 0 && grown > room {
				grown = room
			}
		}
		if grown <= budget {
			break
		}
		budget = grown
	}
	return "", emptySummaryError(last)
}

// emptySummaryError explains a summary that came back empty. The usual cause is a
// reasoning model: because `max_tokens` bounds thinking and answer together, a
// model that spent the whole budget thinking returns stop_reason "length" with no
// content at all. Naming that cause names the fix — raise the summary budget —
// instead of leaving "empty summary" to be read as a model that refused to answer.
func emptySummaryError(out completion) error {
	if out.reasoning != "" || strings.EqualFold(out.stopReason, "length") {
		stop := out.stopReason
		if stop == "" {
			stop = "unbekannt"
		}
		return fmt.Errorf("das Modell hat sein Ausgabebudget beim Reasoning aufgebraucht und "+
			"keine Zusammenfassung geschrieben (stop_reason %q, %d Zeichen Reasoning) — erhöhe "+
			"„Max. Zusammenfassung (Tokens)“ in den Einstellungen", stop, len([]rune(out.reasoning)))
	}
	return errors.New("das Modell hat eine leere Zusammenfassung geliefert")
}

// summarisePrompt builds the user message of a summarisation call. It is split
// out of Summarize so the transcript budget can be measured against exactly the
// text that will be sent: the wrappers, the existing memory it has to merge and
// the focus line all share the window with the transcript, so they are charged
// before its allowance is set.
func summarisePrompt(opts SummarizeOptions) string {
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
	return sb.String()
}

// SummarizeOptions configures a summarisation call.
type SummarizeOptions struct {
	PreviousSummary string
	Transcript      string
	Focus           string
	Temperature     float64
	MaxTokens       int
	NumCtx          int
	// DisableThinking asks the backend to skip reasoning for this call. A summary
	// is not a reasoning task, and a thinking model spends the output budget on
	// private thinking before it writes a word of it. Only a backend with a
	// documented control acts on it (Ollama); elsewhere it is ignored and the
	// retry in Summarize adapts the budget instead.
	DisableThinking bool
	// ContextLimit is the model's window, used to cap a retry budget so a larger
	// allowance never overruns what the model can hold. Zero means unknown.
	ContextLimit int
}

// TranscriptBudget is the token allowance for one summarisation transcript,
// derived from the window of the model that has to read it.
type TranscriptBudget struct {
	// Total bounds the transcript as a whole.
	Total int
	// Block bounds one message's contribution, so a single page-sized search
	// payload or tool result cannot crowd out the conversation around it.
	Block int
}

const (
	// transcriptBlockDivisor splits the transcript allowance into blocks, so one
	// page-sized payload cannot claim all of it. Four holds a single block to a
	// quarter, which is what stops a fold from becoming a summary of a web page.
	transcriptBlockDivisor = 4
)

// PlanTranscriptBudget derives the summarisation transcript's allowance from the
// window of the model that will read it.
//
// The fixed character cap this replaced could not do that job, and bounded the
// wrong thing: the same 4000 characters are a modest block for a 128k model and an
// eighth of the whole window for an 8k one, and being per block it bounded nothing
// as a whole — a long conversation could reach a small model as a request larger
// than its window, which the backend answers with an error or a silent truncation.
//
// The allowance is now simply what the window leaves once the prompt that carries
// the transcript and a reserve for the answer (see compactWith) are charged. It is
// deliberately not capped at a fraction of the window: a half-window ceiling threw
// away half the history a large model could have read, so a fold summarised by a
// 128k model was cut to 64k for no reason. Exceeding the window is the one failure
// this function exists to prevent, and the reserve — which carries its own safety
// margin — is what keeps the transcript clear of it.
//
// A zero contextLimit means the window is unknown; the caller is expected to have
// refused that case before here, and a zero budget renders an empty transcript
// rather than an unbounded one.
func PlanTranscriptBudget(contextLimit, reserveTokens, overheadTokens int) TranscriptBudget {
	if contextLimit <= 0 {
		return TranscriptBudget{}
	}
	free := contextLimit - reserveTokens - overheadTokens
	if free < 0 {
		free = 0
	}
	return TranscriptBudget{Total: free, Block: free / transcriptBlockDivisor}
}

// RenderTranscript flattens messages into a plain transcript for summarisation,
// marking tool activity so the summary can record what was actually looked up.
//
// The budget bounds it at both levels: every message is cut to budget.Block, and
// the finished transcript to budget.Total.
func RenderTranscript(messages []store.Message, budget TranscriptBudget) string {
	var sb strings.Builder
	for _, m := range messages {
		switch m.Role {
		case "system":
			// A seeded search message is a system message, and it is usually the
			// largest thing a fold removes. Only its sources are worth carrying
			// into the memory, so the payload is distilled to them rather than
			// excerpted — see renderSearchSources.
			sources := renderSearchSources(m.Content, budget.Block)
			if sources == "" {
				continue
			}
			fmt.Fprintf(&sb, "\n### Websuche\n%s\n", sources)
		case "user":
			if m.Content == "" {
				continue
			}
			fmt.Fprintf(&sb, "\n### Nutzer\n%s\n", cutToTokens(m.Content, budget.Block))
		case "assistant":
			body := m.Content
			if m.Reasoning != "" && body == "" {
				body = "(nur Reasoning)"
			}
			if body == "" && m.ToolCallsJSON == "" {
				continue
			}
			fmt.Fprintf(&sb, "\n### Assistent\n%s\n", cutToTokens(body, budget.Block))
			if m.ToolCallsJSON != "" {
				fmt.Fprintf(&sb, "(Tool-Aufruf: %s)\n", m.ToolCallsJSON)
			}
		case "tool":
			fmt.Fprintf(&sb, "\n### Tool-Ergebnis\n%s\n", cutToTokens(m.Content, budget.Block))
		}
	}
	// The per-message cap bounds each message, not the transcript: enough
	// messages still add up past the window of the model that has to read them.
	return cutToTokens(sb.String(), budget.Total)
}

// searchSourceHeading matches the line that opens one source in a seeded search
// payload. Both renderers write it flush left as "[3] Title", while URL and
// snippet lines are indented — so requiring the bracket at column 0 is what keeps
// a snippet that happens to begin with "[2]" from being read as a new source.
var searchSourceHeading = regexp.MustCompile(`^\[(\d+)\][ \t]*(.*)$`)

// renderSearchSources distils a seeded search payload into one line per source:
// its number, its title and its URL.
//
// A fold deletes the whole payload, so what the memory keeps has to be chosen.
// Carrying the raw text kept the URLs but paid roughly a page of tokens for prose
// the memory cannot use, and crowded out the conversation the summary exists to
// preserve. The sources are the part that must survive, so they are carried and
// the page text is dropped on purpose.
func renderSearchSources(content string, maxTokens int) string {
	var sb strings.Builder
	number, title := "", ""

	// flush writes the source being collected. A second URL inside one source
	// block is not possible, and a heading that never had one still closes its
	// entry so a later URL cannot attach itself to the wrong title.
	flush := func(url string) {
		title = strings.TrimSpace(title)
		if title != "" || url != "" {
			line := title
			switch {
			case url != "" && url != title:
				if line == "" {
					line = url
				} else {
					line += " — " + url
				}
			case line == "":
				line = url
			}
			if number != "" {
				line = "[" + number + "] " + line
			}
			sb.WriteString(line + "\n")
		}
		number, title = "", ""
	}

	for _, raw := range strings.Split(content, "\n") {
		if m := searchSourceHeading.FindStringSubmatch(raw); m != nil {
			flush("")
			number, title = m[1], m[2]
			continue
		}
		rest, ok := strings.CutPrefix(strings.TrimSpace(raw), "URL:")
		if !ok {
			continue
		}
		// Require a scheme: a snippet that merely starts with "URL:" must not
		// be mistaken for the real one.
		if url := strings.TrimSpace(rest); strings.HasPrefix(url, "http") {
			flush(url)
		}
	}
	flush("")

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return ""
	}
	return cutToTokens(out, maxTokens)
}

// truncationMarker closes a cut block. Its cost is counted inside the budget,
// not added on top of it: the budget is what bounds the request, so a block that
// overshoots it by the marker is exactly the overshoot the bound exists to stop.
const truncationMarker = "\n…[gekürzt]"

// cutToTokens trims s to a token budget, cutting between runes and marking the
// cut so the summariser can tell a short message from a shortened one. The result
// never estimates above budget.
//
// The cut position is found by bisecting against llm.EstimateTokens rather than
// by converting tokens into characters: the budget is expressed in the
// estimator's own unit, and a characters-per-token constant would only
// approximate the number it was derived from.
func cutToTokens(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if llm.EstimateTokens(s) <= budget {
		return s
	}
	// The marker has to fit inside the budget too. When there is not even room
	// for it, nothing is returned: an unmarked fragment would read as complete.
	room := budget - llm.EstimateTokens(truncationMarker)
	if room < 0 {
		return ""
	}
	// Bisecting over runes means a cut can never split one, so the result is
	// always valid UTF-8. EstimateTokens counts per character class and is
	// additive across a concatenation, and the marker opens with a newline, so
	// the cut text plus the marker cannot exceed the budget it was given.
	runes := []rune(s)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if llm.EstimateTokens(string(runes[:mid])) <= room {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo]) + truncationMarker
}
