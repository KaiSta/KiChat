package chat

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"kichat/internal/config"
	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/uimsg"
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

	// Threshold is the fullness the user asked automatic compaction to happen at, as a
	// ratio of the effective limit — or 0 when their automatic compaction is off for
	// this chat. 0 means "no preference" rather than "a default": this bound is the
	// user's and it can be switched off, so its absence has to be expressible.
	Threshold float64

	// Ceiling is the backend's own limit on the size of a single request, in tokens, or
	// 0 when it publishes none (llm.RequestCeiling). Unlike Threshold it is not a
	// preference: crossing it costs a refused turn, so it folds whether or not the user
	// asked for automatic compaction. It arrives as a token figure because that is the
	// form the backend publishes it in; automaticBound converts it into a ratio.
	Ceiling int

	KeepRecentTurns int
	// KeepRecentTokens bounds the same kept tail by size. A turn is a unit of
	// conversation, not of size: six of them are a few hundred tokens of short
	// questions and several windows of tool traffic, so a keep-turns boundary alone
	// cannot say how large the request after the fold will be. Measured: 1 910-token
	// turns in an 8 192-token window folded to 179 % of the window, because keeping
	// six turns is no saving at all when each turn is a quarter of the window.
	//
	// Zero leaves the walk off, so a caller that models only turns — every test that
	// predates this field — gets exactly the cut it got before. It is an upper bound
	// on the kept history and not a target: the fold never keeps more than
	// KeepRecentTurns turns and never more than this many tokens, whichever is smaller.
	KeepRecentTokens int
	Reserve          int
	MaxSummaryTokens int

	// Force runs compaction even when the threshold is not reached (manual
	// compaction). Target is the desired usage ratio afterwards, 0 for "as much
	// as possible".
	Force  bool
	Target float64

	// ProtectNewestTurn refuses the escape hatch below, which folds the newest user
	// turn when there is nothing older to fold. A size fold must never take it: the
	// message the user just sent is what the next request has to carry, and folding
	// it would answer the question with a summary of itself while leaving the request
	// exactly as large.
	ProtectNewestTurn bool
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

// foldCause says which of the two automatic bounds made a fold happen. It is a name
// rather than a bool because the two make different promises to the reader: one is the
// threshold the user set, the other a limit the backend published.
type foldCause int

const (
	// foldCauseNone: no automatic fold — either the projection is below both bounds or
	// there is no bound at all, which is a chat with automatic compaction switched off
	// and a backend that publishes no ceiling.
	foldCauseNone foldCause = iota
	// foldCauseThreshold: the user's threshold, a preference. It applies while their
	// automatic compaction is switched on, which is why an absent Threshold (0) leaves
	// this bound out entirely rather than falling back to a default.
	foldCauseThreshold
	// foldCauseCeiling: the backend's per-request ceiling, a bound rather than a
	// preference — crossing it costs a refused turn, so it folds whether or not the user
	// asked for automatic compaction.
	foldCauseCeiling
)

// automaticBound is the one decision both automatic folds come from, and comparing them
// here rather than in two callers is the point: the backend's per-request ceiling is
// expressed as a ratio of the same effective limit the user's threshold is, so the
// planner asks a single question — "is the next request past the tighter of the two
// bounds" — instead of two questions whose answers had to be reasoned about together.
//
// The caller keeps the cause because the two bounds differ in what crossing them means:
// the threshold says the context is fuller than the user wants, the ceiling says the
// next request would be refused. That difference is the target (see atCeiling), not a
// second decision about whether to fold.
//
// bound is 0 with a cause of foldCauseNone; it is otherwise the ratio to compare the
// projection against.
func automaticBound(in PlanInput) (float64, foldCause) {
	switch {
	case in.Threshold <= 0 && in.Ceiling <= 0:
		return 0, foldCauseNone
	case in.Ceiling <= 0:
		return in.Threshold, foldCauseThreshold
	case in.Threshold <= 0:
		if ratio := ceilingRatio(in.Ceiling, effectiveLimit(in)); ratio > 0 {
			return ratio, foldCauseCeiling
		}
		return 0, foldCauseNone
	default:
		ratio := ceilingRatio(in.Ceiling, effectiveLimit(in))
		if ratio <= 0 || ratio >= in.Threshold {
			return in.Threshold, foldCauseThreshold
		}
		return ratio, foldCauseCeiling
	}
}

// atCeiling reports whether the projected next request has reached the fold point of a
// ceiling it has to fit under — i.e. whether sending it as it stands would be refused for
// its own size. It is asked separately from automaticBound because a fold can be triggered
// by the user's lower threshold while the request is already at the ceiling, and then the
// ceiling is what the fold has to reach.
func atCeiling(in PlanInput) bool {
	ratio := ceilingRatio(in.Ceiling, effectiveLimit(in))
	return ratio > 0 && in.Budget.ProjectedRatio >= ratio
}

// effectiveLimit is what the next request actually has to fit under: the smaller of the
// model's window and the backend's ceiling. Budget.EffectiveLimit is that number; a budget
// built without one (a caller filling in only Limit) falls back to Limit, which is the same
// number whenever no ceiling binds.
func effectiveLimit(in PlanInput) int {
	if in.Budget.EffectiveLimit > 0 {
		return in.Budget.EffectiveLimit
	}
	return in.Budget.Limit
}

// projection is the projected size of the next request in tokens, recovered from the ratio
// the planner is handed when the budget carries no figure of its own.
func projection(in PlanInput) int {
	if in.Budget.Projected > 0 {
		return in.Budget.Projected
	}
	if limit := effectiveLimit(in); limit > 0 {
		return int(in.Budget.ProjectedRatio*float64(limit) + 0.5)
	}
	return 0
}

// ceilingFoldRatio is how close to a published ceiling a request may come before the
// history is folded. It is below 1 because the meter's number is partly an estimate (the
// token counter is a heuristic, deliberately — see llm.EstimateTokens) while the ceiling is
// exact, and undershooting the fold point costs one summarisation while overshooting it
// costs a refused turn.
const ceilingFoldRatio = 0.9

// ceilingRatio is a ceiling expressed against the limit the planner measures with — the
// effective limit, which is the ceiling itself whenever the ceiling binds — and it is 0 when
// there is nothing to compare.
//
// Expressed this way, a ceiling *larger* than the window grows into a bound above the
// window's own: 1.1 windows becomes 0.99 of it, so a ceiling nobody could cross cannot fold
// earlier than the window that already can. More than 1/ceilingFoldRatio windows lands past
// 1 and is no bound at all: at any projection that reached it the window itself is already
// over its limit, so the user's threshold is the earlier of the two and the only one worth
// folding for.
func ceilingRatio(ceiling, effectiveLimit int) float64 {
	if ceiling <= 0 || effectiveLimit <= 0 {
		return 0
	}
	return ceilingFoldRatio * float64(ceiling) / float64(effectiveLimit)
}

// ceilingTarget is the usage ratio of the chat's own window that puts the folded request at
// the ceiling: the plan prices the turns it keeps against Budget.Limit, so aiming at
// ceiling/window aims the folded request at the ceiling itself. Without it a "fit the
// request" fold would fold nothing, because 2 % of a 131k window looks empty to the planner.
func ceilingTarget(ceiling, limit int) float64 {
	if ceiling <= 0 || limit <= 0 {
		return 0
	}
	return clampRatio(float64(ceiling) / float64(limit))
}

// clampRatio keeps a target inside the band the planner can act on.
//
// The floor is tiny and still necessary, and the reason is the planner's own convention:
// `Target: 0` means "fold as much as your cut allows" and *skips* the target walk entirely,
// so a ceiling so small that the ratio rounded to zero would silently fold only to the
// keep-turns boundary — the one thing a size fold exists to go past when the limit demands
// it. Above that, the walk's own limits are the bound: it never folds the turn that started
// the request, however small the target is. The cap keeps a ceiling larger than the window
// from asking the planner for no reduction at all.
func clampRatio(ratio float64) float64 {
	switch {
	case ratio < 0.001:
		return 0.001
	case ratio > 0.95:
		return 0.95
	default:
		return ratio
	}
}

// PlanCompaction decides whether to compact and where to cut. Cut points are
// always aligned to user-message boundaries so a tool call is never separated
// from its result — that would produce a request the backend rejects.
func PlanCompaction(in PlanInput) CompactPlan {
	keepTurns := in.KeepRecentTurns
	if keepTurns < 1 {
		keepTurns = 1
	}

	// target is the usage ratio a fold aims at, 0 for "as far as the cut allows". Both
	// automatic bounds resolve it here, so the cut and the reason a plan carries always
	// describe one decision.
	var plan CompactPlan
	target := in.Target
	if !in.Force {
		if in.Budget.Limit <= 0 {
			return plan
		}
		bound, cause := automaticBound(in)
		if cause == foldCauseNone || in.Budget.ProjectedRatio < bound {
			return plan
		}
		if cause == foldCauseCeiling {
			plan.Reason = fmt.Sprintf("Anfrage bei %d von %d Token des Anfrage-Limits",
				projection(in), in.Ceiling)
		} else {
			plan.Reason = fmt.Sprintf("Kontext bei %.0f%% (Schwelle %.0f%%)",
				in.Budget.ProjectedRatio*100, bound*100)
		}
		// Whatever fired, a request that has reached a ceiling's fold point decides the
		// aim: sending it would be refused for its own size, and folding only to the
		// keep-turns boundary can leave it exactly that large. The two coincide in the
		// ordinary case — the ceiling is the tighter bound, so it is what fired.
		if atCeiling(in) {
			target = ceilingTarget(in.Ceiling, in.Budget.Limit)
		} else if projection(in) > effectiveLimit(in) {
			// The same argument without a published ceiling: the next request would not
			// fit the window as it stands. Folding to the keep-turns boundary is only
			// enough while a kept turn is small, and a **project**'s history is the
			// opposite shape — a few long work sessions rather than many short questions.
			// There the kept turns alone can be several windows, so a targetless fold
			// either refuses (fewer turns than KeepRecentTurns: nothing to fold, and the
			// oversized request goes out) or trims one turn's worth and leaves the rest
			// over the line. Aiming at the bound that fired is what turns the fold into
			// one that makes the request sendable.
			//
			// It is the bound, not a ceiling: the user's threshold is the fullness they
			// asked the context to come down to, and this branch only decides to honour it
			// when leaving it alone would send a request the window cannot hold.
			// Measured: a two-turn project priced 23 031 tokens against a 16 384-token
			// window, folded nothing before the turn, and only folded once the first tool
			// round had produced something else to fold.
			target = bound
		}
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

	// The kept tail is a size as well as a count. A turn says how much of the
	// conversation stays verbatim, not how much of the window it costs, so the walk
	// below folds whole turns — oldest first — until the tail that is left is inside the
	// token budget. Folding a turn moves the cut forward, towards the newest message; it
	// stops before that message's own turn, because that is the exchange being answered
	// and folding it would take the question into the memory.
	//
	// The cut stays on user boundaries (invariant 3), so a turn is not split to make
	// room and a single turn larger than the budget leaves the tail over it. That is the
	// one shape this cannot fix, and the honest answer there is the fold plus whatever the
	// target walk does — not a prompt that begins on an assistant message.
	if in.KeepRecentTokens > 0 && toSeq >= fromSeq {
		spans := turnTokenSpans(in.Messages, userSeqs)
		kept := 0
		for _, m := range in.Messages {
			if m.Seq >= keepFrom {
				kept += llm.EstimateTokens(m.Content) + llm.EstimateTokens(m.Reasoning)/2
			}
		}
		idx := 0
		for i, seq := range userSeqs {
			if seq >= keepFrom {
				idx = i
				break
			}
		}
		for kept > in.KeepRecentTokens && idx+1 < len(userSeqs) {
			idx++
			keepFrom = userSeqs[idx]
			kept -= spans[idx-1]
			toSeq = keepFrom - 1
		}
	}

	// A fold exists to make a request smaller, so it has to produce a real cut even
	// when the chat is shorter than keepTurns: otherwise the run reports "nothing to
	// compact" while there is history that could be summarised.
	if toSeq < fromSeq {
		// A run with a target exists to make the next request fit, and refusing it here
		// because the chat is shorter than keepRecentTurns would leave exactly the
		// oversized request it was asked to prevent. It folds what exists — the turns
		// before the newest, which is as far as a cut can go without splitting the
		// exchange the user just started. A run without a target keeps the older answer:
		// nothing older than keepRecentTurns means nothing to fold, and a manual run's
		// escape hatch below is what keeps its promise instead.
		if !in.Force && target <= 0 {
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

	// Fold turns until the kept history fits the target, so "compact to 50%" and "fit
	// under the ceiling" both mean it. One walk serves both: the estimate is the same
	// either way and only the origin of the target differs. The reserve counts towards
	// the target because the meter's `projected` number, which both bounds read,
	// includes it.
	if target > 0 && in.Budget.Limit > 0 && toSeq >= fromSeq {
		// The aim is `target`, not `in.Target`: a ceiling fold resolves its own target
		// above (ceilingTarget) and leaves the caller's at 0 — where 0 means "fold as far
		// as the cut allows", so pricing the walk against it folded the whole memory away
		// to save two turns on every tool-heavy turn. Pinned by
		// TestAnAutomaticCeilingFoldAimsAtTheCeiling.
		budgetTarget := int(target * float64(in.Budget.Limit))
		// Attachments are part of the request too and sit inside the region this
		// cut keeps, so leaving them out prices the kept history too cheaply.
		base := in.Budget.System + in.Budget.Memory + in.Budget.ToolSchemas +
			in.Budget.Attachments + in.Reserve
		kept := 0
		for _, m := range in.Messages {
			if m.Seq >= keepFrom {
				kept += llm.EstimateTokens(m.Content) + llm.EstimateTokens(m.Reasoning)/2
			}
		}
		summaryBudget := in.MaxSummaryTokens
		if summaryBudget <= 0 {
			summaryBudget = 1200
		}
		// Folding a turn removes the whole turn, so every cut point is priced with
		// its answer and tool traffic rather than only the prompt that starts it.
		turnTokens := turnTokenSpans(in.Messages, userSeqs)
		// Walk outwards from the turn this cut begins with, so the figure and the
		// cut always describe the same state: anchoring the cut at the i-th turn
		// counted backwards from the end prices one cut and installs another.
		for i := 0; i < len(userSeqs)-1; i++ {
			if userSeqs[i] < keepFrom {
				continue // an earlier compaction already folded this turn
			}
			if base+kept+summaryBudget <= budgetTarget {
				break
			}
			kept -= turnTokens[i]
			toSeq = userSeqs[i+1] - 1
			keepFrom = userSeqs[i+1]
		}
	}

	// A manual compaction is a promise that the context gets compacted, so it
	// must never end as a silent no-op while foldable history exists. That is the
	// state right after a previous compaction with a single new turn: every older
	// turn is already folded and only the newest one is left, which the cut above
	// keeps verbatim. Extending the cut to the end of the chat folds that turn
	// too, and the end of the chat is always a valid cut point — nothing follows
	// it, so no tool call is separated from its result. Only a forced run takes
	// it: an automatic fold has nobody to promise anything to, and folding the
	// message it is about would leave the request exactly as large as it was.
	if in.Force && toSeq < fromSeq && !in.ProtectNewestTurn {
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

// summarySystemPrompt picks the memory contract in the language the app is set
// to. The contract's language is not a detail: a German contract over an English
// transcript produced German memories, and the memory is part of the system
// message of every request that follows it, so the chat then answered in German
// while the user kept writing English. The rules, the headings and the language
// they name are translated together; naming the language in its own words is what
// a small local model follows.
func summarySystemPrompt(lang string) string {
	switch promptLanguage(lang) {
	case config.LangGerman:
		return summaryPromptDE
	case config.LangChinese:
		return summaryPromptZH
	default:
		return summaryPromptEN
	}
}

// summaryPromptDE is the original memory contract. Its headings are part of the
// German interface, because a German window's memory has always read this way.
const summaryPromptDE = `Du komprimierst den Verlauf eines Chats zu einem dauerhaften Gedächtnis.

Regeln:
- Schreibe auf Deutsch.
- Übernimm Namen, Zahlen, Daten, Preise, Gesetzesangaben und URLs WÖRTLICH. Kürze oder paraphrasiere sie nicht.
- Erfinde nichts. Wenn etwas unklar blieb, schreibe es unter "Offene Punkte".
- Befunde — was geprüft wurde, was dabei herauskam, mit Datei, Zeile und dem, was daran falsch ist — übernimm als je einen Stichpunkt, Bezeichner und Pfade WÖRTLICH. Die Kürze unten gilt für sie nicht: ein Befund, den man aus diesem Gedächtnis nicht mehr umsetzen kann, ist verloren.
- Fasse dich kurz: Stichpunkte, keine Prosa, keine Wiederholungen.
- Halte den Stand der Arbeit fest, soweit er aus dem Verlauf hervorgeht: was erledigt ist, was noch offen ist und was der nächste Schritt ist.
- Lass weg, was der nächsten Anfrage ohnehin mitgeht: die Systemvorgaben, die AGENTS.md-Dateien des Projekts und die Tool-Beschreibungen. Was der Nutzer selbst verlangt hat, gehört ins Gedächtnis.
- Erwähne die Kompaktierung nicht und schreibe nicht, dass Kontext verdichtet wurde.

Antworte ausschließlich mit Markdown in genau dieser Struktur:

## Festgestellte Fakten
- ...
## Entscheidungen
- ...
## Befunde & offene Arbeit
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

const summaryPromptEN = `You compact the history of a chat into a durable memory.

Rules:
- Write in English.
- Copy names, numbers, dates, prices, legal references and URLs VERBATIM. Do not shorten or paraphrase them.
- Invent nothing. If something stayed unclear, write it under "Open questions".
- Carry findings — what was examined, what came of it, the file, the line and what is wrong there — as one bullet each, with identifiers and paths VERBATIM. The brevity rule below does not apply to them: a finding that cannot be acted on from this memory alone has been lost.
- Be brief: bullet points, no prose, no repetition.
- Record the state of the work as far as the transcript shows it: what is done, what is still open, and what the next step is.
- Leave out what the next request carries anyway: the system instructions, the project's AGENTS.md files, and the tool descriptions. What the user asked for themselves belongs in the memory.
- Do not mention the compaction or that context was compacted.

Answer only with Markdown in exactly this structure:

## Established facts
- ...
## Decisions
- ...
## Findings & work in progress
- ...
## Open questions
- ...
## Sources
- [Title](URL)
## Files & documents
- ...
## Requirements & preferences
- ...

Omit a section when it is empty.`

const summaryPromptZH = `你将一段对话的历史压缩为持久记忆。

规则：
- 用中文书写。
- 名称、数字、日期、价格、法律条款和 URL 必须逐字保留，不得缩写或改写。
- 不要编造。仍不清楚的内容写入“未决问题”。
- 发现——检查了什么、结果如何、哪个文件的哪一行、那里错在哪里——每条发现单独一条要点，标识符与路径必须逐字保留。下方的“简明”规则不适用于它们：如果无法仅凭这份记忆着手处理，这条发现就已经丢失。
- 简明：用要点，不写散文，不重复。
- 记录工作进展（以记录所及为限）：已完成什么、还欠什么、下一步是什么。
- 省略下一次请求本来就会带上的内容：系统提示、项目的 AGENTS.md 文件和工具说明。用户自己提出的要求属于记忆。
- 不要提及压缩，也不要说明上下文曾被压缩。

只输出以下结构的 Markdown：

## 已确认的事实
- ...
## 决定
- ...
## 发现与未完成的工作
- ...
## 未决问题
- ...
## 来源
- [标题](URL)
## 文件与文档
- ...
## 要求与偏好
- ...

某一节为空时省略。`

// summaryRestructure is the second request: the first answer sent back for the shape it did
// not have. The check that asks for it only fires when the answer carried none of the
// contract's sections, so the sections it is missing are the structure itself — and the
// material for it is the answer, not the conversation it was distilled from.
//
// Sending the draft instead of the transcript is the whole point. The retry used to re-send
// the fold's history — up to 80% of the window, minutes of prompt on a CPU-hosted model, and
// a request no prefix cache could hit because of the appended sentence — to ask for headings
// that the model could add to what it had already written. The draft is bounded by the
// summariser's output cap, so the second call costs a fraction of the first.
func summaryRestructure(draft, lang string) string {
	var sb strings.Builder
	switch promptLanguage(lang) {
	case config.LangGerman:
		sb.WriteString("Übernimm das folgende Gedächtnis in die Struktur, die der Vertrag oben verlangt:\n\n")
		sb.WriteString("<draft>\n")
		sb.WriteString(draft)
		sb.WriteString("\n</draft>\n\n")
		sb.WriteString("Antworte nur mit dem Gedächtnis: ##-Überschriften wie im Vertrag, keine Vorrede. ")
		sb.WriteString("Lass keine Tatsache weg und erfinde nichts hinzu.")
	case config.LangChinese:
		sb.WriteString("请将下面的记忆改写成上面约定要求的结构：\n\n")
		sb.WriteString("<draft>\n")
		sb.WriteString(draft)
		sb.WriteString("\n</draft>\n\n")
		sb.WriteString("只输出记忆：使用约定要求的 ## 标题，不要前言。不要遗漏任何事实，也不要添加任何内容。")
	default:
		sb.WriteString("Recast the memory below into the structure the contract above requires:\n\n")
		sb.WriteString("<draft>\n")
		sb.WriteString(draft)
		sb.WriteString("\n</draft>\n\n")
		sb.WriteString("Answer with only the memory: the ## headings the contract defines, no preamble. ")
		sb.WriteString("Leave no fact out and add nothing.")
	}
	return sb.String()
}

// summaryHeadings are the section titles the three contracts ask for. They are listed
// together rather than per language because a model may answer in the transcript's
// language instead of the interface's, and a memory that used the other language's
// headings followed the structure even though it is not the one that was requested.
var summaryHeadings = map[string]bool{
	"Festgestellte Fakten": true, "Entscheidungen": true,
	"Befunde & offene Arbeit": true, "Offene Punkte": true, "Quellen": true,
	"Dateien & Dokumente": true, "Vorgaben & Präferenzen": true,

	"Established facts": true, "Decisions": true,
	"Findings & work in progress": true, "Open questions": true, "Sources": true,
	"Files & documents": true, "Requirements & preferences": true,

	"已确认的事实": true, "决定": true, "发现与未完成的工作": true,
	"未决问题": true, "来源": true, "文件与文档": true, "要求与偏好": true,
}

// summaryStructureMinTokens is how long a shapeless memory has to be before it is worth
// asking again. The nudge costs a second summary request, and on a CPU-hosted local model
// that is minutes: worth paying for a page of prose that every later turn has to search,
// not worth paying for the three words a model that ignored the instruction usually
// produces — there the answer is a different model, not a different prompt. It is also
// about two thirds of the default memory target (MaxSummaryTokens 1200), so a memory of
// the size the contract asks for is the one this rule is about.
const summaryStructureMinTokens = 400

// summaryHasStructure reports whether a memory carries at least one of the sections it
// was asked for. It deliberately does not judge the content: the question worth asking
// is whether the shape was followed at all, because one retry fixes that, while drifting
// prose is not something a second request reliably repairs — and a memory with the wrong
// shape still knows more than no memory at all.
func summaryHasStructure(summary string) bool {
	for _, line := range strings.Split(summary, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "##") {
			continue
		}
		if summaryHeadings[strings.TrimSpace(strings.TrimLeft(line, "#"))] {
			return true
		}
	}
	return false
}

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

// thinkingBlock matches an inline chain-of-thought block, which is how a backend
// with no separate reasoning channel delivers one. It is case-insensitive and
// spans lines, because a thinking block is prose, not a tag pair on one line.
var thinkingBlock = regexp.MustCompile(`(?is)<(?:think|thinking|analysis|reasoning)>.*?</(?:think|thinking|analysis|reasoning)>`)

// danglingThinkingOpen matches a thinking block whose closing tag never arrived,
// which is what a generation cut off mid-thought looks like.
var danglingThinkingOpen = regexp.MustCompile(`(?is)<(?:think|thinking|analysis|reasoning)>.*$`)

// stripThinking splits a model's answer into what it wants to say and what it
// thought while saying it. The thinking is returned rather than dropped, because
// a summary made of nothing but thinking is not an empty answer: it is the
// reasoning-budget failure, and both the retry and the error message need to see
// it to name the cause.
func stripThinking(text string) (answer, thinking string) {
	if !strings.Contains(text, "<") {
		return strings.TrimSpace(text), ""
	}
	blocks := thinkingBlock.FindAllString(text, -1)
	answer = thinkingBlock.ReplaceAllString(text, "")
	if open := danglingThinkingOpen.FindString(answer); open != "" {
		blocks = append(blocks, open)
		answer = strings.TrimSuffix(answer, open)
	}
	return strings.TrimSpace(answer), strings.TrimSpace(strings.Join(blocks, "\n"))
}

// summarisationPrefix returns the chat's own request up to the fold boundary — the system
// message, the memory in force and the history before the fold — so a summarisation can
// re-send it unchanged instead of writing a transcript of those messages.
//
// It exists for the prompt cache, and the arithmetic is the whole argument. The user's last
// turn sent exactly this prefix; a summary request that opens with the same bytes is a hit
// for it on any server that caches a prefix, which on a CPU-hosted llama.cpp server is the
// difference between reading the history and evaluating it again — most of the minutes a
// fold takes. A re-rendered transcript is different bytes (its own headings, distilled
// search payloads, clipped blocks), so it is always a miss.
//
// It is also lossless in a way the transcript is not: no distillation, no clipping, no
// `(nur Reasoning)` substitution. That is why it is offered first, with the transcript kept
// as the fallback for the request that would not fit.
//
// Nothing is returned when the boundary cannot be named — `MessageSeqs` is nil after a
// rewrite that changed the request's shape — when the kept range does not begin inside the
// request, when the prefix would be the system message alone (no history to summarise), or
// when it carries an image: a summarisation is a text request, and the pictures belong to
// the conversation, not to the instruction asking about it.
func summarisationPrefix(assembly Assembly, plan CompactPlan) []llm.Message {
	seqs := assembly.MessageSeqs
	if plan.KeepFromSeq <= 0 || len(seqs) != len(assembly.Messages) {
		return nil
	}
	end := -1
	for i, seq := range seqs {
		if seq == plan.KeepFromSeq {
			end = i
			break
		}
	}
	// end <= 1 would leave the system message alone above the instruction, which carries no
	// history at all — the transcript path is the honest answer there.
	if end <= 1 {
		return nil
	}
	for _, m := range assembly.Messages[:end] {
		if len(m.Images) > 0 {
			return nil
		}
	}
	return append([]llm.Message(nil), assembly.Messages[:end]...)
}

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
	lang := promptLanguage(opts.Lang)
	var userPrompt string
	if len(opts.Prefix) > 0 {
		// The prefix path: the chat's own request up to the fold boundary, then the
		// instruction on its own. No `<conversation>` wrapper and no transcript, because
		// the prefix already is the conversation — unchanged, which is what lets a server
		// holding it in its prompt cache read it instead of evaluating those tokens again.
		userPrompt = summariseTail(opts, lang, true)
	} else {
		userPrompt = summarisePrompt(opts)
	}
	// Ask the model not to think. A summary is a mechanical task, and `max_tokens`
	// bounds thinking and answer together, so a reasoning model that thinks first
	// can spend the whole budget before writing a word. `/no_think` is the in-prompt
	// soft switch — the only lever that reaches a local backend with no reasoning
	// parameter (LM Studio). A model that does not know the tag treats it as an
	// ignorable trailing note.
	return extractWithBudget(ctx, p, model, opts.Prefix, summarySystemPrompt(opts.Lang),
		userPrompt+"\n\n"+noThinkTag, summaryExtraction(opts))
}

// Restructure asks for the shape again, and only for the shape. It is a separate entry point
// rather than a flag on Summarize because it summarises nothing: there is no transcript and
// no prefix to send, only the first answer to recast (summaryRestructure), which is what makes
// the second call cheaper than the first instead of a second full pass over the history.
func Restructure(ctx context.Context, p llm.Provider, model, draft string, opts SummarizeOptions) (string, error) {
	return extractWithBudget(ctx, p, model, nil, summarySystemPrompt(opts.Lang),
		summaryRestructure(draft, opts.Lang)+"\n\n"+noThinkTag, summaryExtraction(opts))
}

// summaryExtraction is the half the two summary requests share: the same temperature, cap and
// failure codes, so a restructure fails the way the call it repairs would have failed instead
// of inventing a second set of outcomes for the same work.
func summaryExtraction(opts SummarizeOptions) extractionOptions {
	return extractionOptions{
		Temperature:     opts.Temperature,
		MaxTokens:       opts.MaxTokens,
		NumCtx:          opts.NumCtx,
		DisableThinking: opts.DisableThinking,
		ContextLimit:    opts.ContextLimit,
		FailureCode:     "chat.summaryFailed",
		EmptyCode:       "chat.summaryEmpty",
		BudgetCode:      "chat.summaryBudgetSpent",
		TruncatedError:  uimsg.Errorf("chat.summaryTruncated"),
	}
}

// extractionOptions are what a bounded, prompted extraction needs regardless of
// which prompt it is: a compaction's memory and a tool result's condensation are
// the same request shape over different text.
type extractionOptions struct {
	Temperature     float64
	MaxTokens       int
	NumCtx          int
	DisableThinking bool
	ContextLimit    int
	// FailureCode names the request in an error the user reads, so a failed
	// condensation does not report itself as a failed compaction.
	FailureCode string
	// EmptyCode and BudgetCode are the two sentences an empty answer can produce.
	// They differ per caller because the advice does: a compaction has a settings
	// field to raise, a condensation can also be had from a model that does not
	// think first.
	EmptyCode  string
	BudgetCode string
	// TruncatedError rejects an answer that stopped at the output limit. Only a fold
	// sets it: the memory it writes replaces history, so a sentence cut short is a
	// fact nothing can recover. A condensation's cap-cut summary is used as it is —
	// the page it stands in for is still on the tool card and comes back on a second
	// read (digest.go, DigestToolOutput).
	TruncatedError error
}

// extractWithBudget runs one prompted extraction and retries it when a reasoning
// model spent the whole output budget thinking and returned nothing. It is shared by
// the two callers in this package — the memory a fold writes and the condensation a
// tool result becomes — because `max_tokens` bounds thinking and answer together, so
// both have exactly the same failure to survive and neither should reinvent the
// arithmetic for it: the retry grows the budget by the reasoning that actually came
// back, measured rather than guessed.
func extractWithBudget(ctx context.Context, p llm.Provider, model string, prefix []llm.Message,
	systemPrompt, userPrompt string, opts extractionOptions) (string, error) {
	if p == nil {
		return "", uimsg.Errorf("chat.noBackend")
	}
	temp := opts.Temperature
	if temp <= 0 {
		temp = 0.2
	}
	// A prefix is the caller's own request, resent as it stands; without one this is the
	// system-prompt + transcript shape both callers share. Either way the instruction is
	// the last message, so the prompt's own tokens are what the retry arithmetic below
	// measures.
	var messages []llm.Message
	if len(prefix) > 0 {
		messages = make([]llm.Message, 0, len(prefix)+1)
		messages = append(messages, prefix...)
		messages = append(messages, llm.Message{Role: llm.RoleUser, Content: userPrompt})
	} else {
		messages = []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt},
			{Role: llm.RoleUser, Content: userPrompt},
		}
	}
	promptTokens := llm.EstimateMessagesTokens(messages)

	answer := opts.MaxTokens
	if answer <= 0 {
		answer = 1024
	}
	// The *first* attempt is held under the window as well, not just the retry below.
	// A backend validates the prompt and the output allowance together, so an output
	// cap that does not fit beside the prompt is an oversized request — refused before
	// a token is generated. That was theoretical while the condensation's cap was a
	// constant of 600 and the caller prepared the transcript for the window; it stopped
	// being theoretical once the cap became a setting, where 8000 of output next to a
	// page that fills a small window is a request no local model accepts. The answer
	// shrinks to the room that is left, and the page — not the cap — decides the rest.
	if opts.ContextLimit > 0 {
		room := opts.ContextLimit - promptTokens
		if room <= 0 {
			return "", uimsg.Errorf(opts.BudgetCode, "context_limit", 0)
		}
		if room < answer {
			answer = room
		}
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
			return "", uimsg.Errorf(opts.FailureCode, uimsg.Sentence(err))
		}
		// A backend without a separate reasoning channel inlines the model's
		// deliberation in the content — `<think>…</think>`, `<analysis>…</analysis>`
		// — and an answer that consisted of nothing but that is not an empty answer:
		// it is the reasoning-budget failure. So the blocks are removed before the
		// answer is judged, and what they contained is counted as reasoning, because
		// that is what it is.
		summary, thought := stripThinking(out.text)
		if summary != "" {
			if opts.TruncatedError != nil && llm.Truncated(out.stopReason) {
				return "", opts.TruncatedError
			}
			return summary, nil
		}
		if thought != "" {
			out.reasoning += thought
		}
		last = out
		// Only a model that reasoned and ran out of budget is worth another look;
		// one that simply said nothing will say nothing again.
		if out.reasoning == "" && !llm.Truncated(out.stopReason) {
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
	return "", emptyExtractionError(last, opts.EmptyCode, opts.BudgetCode)
}

// emptyExtractionError explains an answer that came back empty. The usual cause is a
// reasoning model: because `max_tokens` bounds thinking and answer together, a
// model that spent the whole budget thinking returns stop_reason "length" with no
// content at all. Naming that cause names the fix — the budget code carries that
// advice, per caller — instead of leaving an empty answer to be read as a model that
// refused to answer.
func emptyExtractionError(out completion, emptyCode, budgetCode string) error {
	if out.reasoning != "" || llm.Truncated(out.stopReason) {
		stop := out.stopReason
		if stop == "" {
			// A word rather than an empty argument, because it lands inside the
			// sentence the dictionary holds ("stop_reason {{1}}").
			stop = "unknown"
		}
		return uimsg.Errorf(budgetCode, stop, len([]rune(out.reasoning)))
	}
	return uimsg.Errorf(emptyCode)
}

// summariseTail is the instruction that follows the history: the existing memory it has to
// merge, the request itself, and the focus line. It is split out of summarisePrompt because
// the two paths place it differently — the transcript path wraps it around a rendered
// `<conversation>`, while the prefix path appends it to the chat's own messages, where the
// history is already above it.
//
// memoryAbove says the memory in force is already in the request: on the prefix path it is
// the system message's `<conversation_memory>`, so quoting it again as `<existing_memory>`
// would charge the fold for it twice and ask the model to "merge both" about one text. The
// instruction then names where it is instead. That costs a second wording of the merge
// sentence per language, which is worth paying: it is the sentence that keeps a fold from
// silently dropping the old memory, because a model told only "compact the following
// history" writes a memory of the new messages.
func summariseTail(opts SummarizeOptions, lang string, memoryAbove bool) string {
	var sb strings.Builder
	switch {
	case opts.PreviousSummary != "" && memoryAbove:
		switch lang {
		case config.LangGerman:
			sb.WriteString("Das Gedächtnis im System-Prompt oben deckt die älteren Nachrichten ab. ")
			sb.WriteString("Führe es mit dem folgenden Verlauf zu EINEM Gedächtnis zusammen, ohne Informationen zu verlieren.\n\n")
		case config.LangChinese:
			sb.WriteString("系统提示中上方的记忆已涵盖较早的消息。")
			sb.WriteString("请将其与以下记录合并为一份记忆，不要丢失任何信息。\n\n")
		default:
			sb.WriteString("The memory in the system prompt above already covers the older messages. ")
			sb.WriteString("Merge it with the following history into ONE memory without losing information.\n\n")
		}
	case opts.PreviousSummary != "":
		sb.WriteString("<existing_memory>\n")
		sb.WriteString(opts.PreviousSummary)
		sb.WriteString("\n</existing_memory>\n\n")
		switch lang {
		case config.LangGerman:
			sb.WriteString("Der folgende Verlauf schließt an das bestehende Gedächtnis an. ")
			sb.WriteString("Führe beides zu EINEM Gedächtnis zusammen, ohne Informationen zu verlieren.\n\n")
		case config.LangChinese:
			sb.WriteString("以下记录接续现有记忆。请将两者合并为一份记忆，不要丢失任何信息。\n\n")
		default:
			sb.WriteString("The following transcript continues the existing memory. ")
			sb.WriteString("Merge both into ONE memory without losing information.\n\n")
		}
	default:
		switch lang {
		case config.LangGerman:
			sb.WriteString("Komprimiere den folgenden Verlauf zu einem Gedächtnis.\n\n")
		case config.LangChinese:
			sb.WriteString("将以下记录压缩为一份记忆。\n\n")
		default:
			sb.WriteString("Compact the following transcript into a memory.\n\n")
		}
	}
	if opts.Focus != "" {
		switch lang {
		case config.LangGerman:
			sb.WriteString("Besonderer Fokus des Nutzers: ")
		case config.LangChinese:
			sb.WriteString("用户特别关注：")
		default:
			sb.WriteString("The user's particular focus: ")
		}
		sb.WriteString(opts.Focus)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// summarisePrompt builds the user message of a transcript-path summarisation: the tail
// instruction with the rendered transcript inside it. It is split out of Summarize so the
// transcript budget can be measured against exactly the text that will be sent — the
// wrappers, the existing memory it has to merge and the focus line all share the window
// with the transcript, so they are charged before its allowance is set.
func summarisePrompt(opts SummarizeOptions) string {
	lang := promptLanguage(opts.Lang)
	var sb strings.Builder
	sb.WriteString(summariseTail(opts, lang, false))
	sb.WriteString("<conversation>\n")
	sb.WriteString(opts.Transcript)
	sb.WriteString("\n</conversation>")
	return sb.String()
}

// SummarizeOptions configures a summarisation call.
type SummarizeOptions struct {
	PreviousSummary string
	Transcript      string
	// Prefix is the chat's own request up to the fold boundary, to be re-sent as it stands
	// instead of a transcript of it (summarisationPrefix). It is there for the prompt
	// cache: a local server has just processed exactly these tokens for the user's turn, so
	// a request that starts with the same bytes reads them instead of evaluating them —
	// minutes of prompt on a CPU-hosted model. An empty Prefix keeps the transcript path.
	Prefix []llm.Message
	Focus  string
	// Lang is the app's language (Settings → Language), which decides the
	// language of the memory and of the prompt that asks for it. It is the
	// setting rather than the transcript's language: the user chose it for the
	// window, and a wrong guess would rewrite the memory in the wrong language.
	Lang        string
	Temperature float64
	MaxTokens   int
	NumCtx      int
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
	// transcriptShortDivisor is the second rung of the ladder: when the whole
	// transcript does not fit, every message is first shortened to a quarter of its
	// block before any message is left out, so detail leaves the oldest history first
	// and whole messages disappear only as a last resort. Four rather than a larger
	// divisor because shortening is uniform text loss here: a sixteenth would turn every
	// message into a fragment before a single one was dropped.
	transcriptShortDivisor = 4
)

const (
	// keepTokensMin and keepTokensMax bound the tail derived from the window when no
	// size was configured. Below the floor a fold keeps so little that the memory
	// carries the exchange the user is working on; above the ceiling it keeps more
	// than a conversation usually holds, so the fold would save nothing. Both numbers
	// are opencode's (keep.tokens 15000, clamped to 2000-15000), which is the only
	// evidence available — this app has no measurement of its own for either end.
	keepTokensMin = 2000
	keepTokensMax = 15000
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
// The budget bounds it at two levels: every message is cut to budget.Block, and the
// finished transcript to budget.Total. The second level is a ladder rather than one cut
// of the finished text, because that cut dropped the text's tail — the newest folded
// messages, which are exactly the ones that join the memory to the turns kept verbatim.
// So messages are shortened oldest first, every message reaches the shorter rung before
// any is left out, and the newest one is shortened but never left out.
func RenderTranscript(messages []store.Message, budget TranscriptBudget) string {
	shortLimit := budget.Block / transcriptShortDivisor
	blocks := make([][2]string, 0, len(messages))
	for _, m := range messages {
		whole := renderTranscriptMessage(m, budget.Block)
		if whole == "" {
			continue
		}
		short := renderTranscriptMessage(m, shortLimit)
		if short == "" || llm.EstimateTokens(short) >= llm.EstimateTokens(whole) {
			short = whole
		}
		blocks = append(blocks, [2]string{whole, short})
	}
	if len(blocks) == 0 {
		return ""
	}

	// The note a cut transcript carries is paid for before what to keep is chosen, so
	// the sentence that says how much was left out cannot be the thing that overshoots
	// the budget. A note that would cost more than half the budget is dropped instead:
	// on a transcript that small the note is most of what the model would read.
	note := omissionNote(len(blocks))
	budgetCarriesNote := llm.EstimateTokens(note)*2 <= budget.Total
	if !budgetCarriesNote {
		note = ""
	}
	room := budget.Total - llm.EstimateTokens(note)

	// 0 whole, 1 shortened, 2 left out.
	level := make([]int, len(blocks))
	total := 0
	for _, b := range blocks {
		total += llm.EstimateTokens(b[0])
	}
	for i := range blocks {
		if total <= room {
			break
		}
		total += llm.EstimateTokens(blocks[i][1]) - llm.EstimateTokens(blocks[i][0])
		level[i] = 1
	}
	for i := 0; i < len(blocks)-1 && total > room; i++ {
		total -= llm.EstimateTokens(blocks[i][level[i]])
		level[i] = 2
	}

	var sb strings.Builder
	omitted := 0
	for _, l := range level {
		if l == 2 {
			omitted++
		}
	}
	if omitted > 0 && budgetCarriesNote {
		// Not written when the note itself did not fit: a transcript that small is
		// mostly note, and the count is worth less than the request's budget being
		// respected — the older messages are gone either way, and the blocks that did
		// survive are what the model has to work with.
		sb.WriteString(omissionNote(omitted))
		sb.WriteString("\n")
	}
	for i, b := range blocks {
		if level[i] == 2 {
			continue
		}
		sb.WriteString(b[level[i]])
	}
	return sb.String()
}

// renderTranscriptMessage renders one stored message as a transcript block at a body
// allowance of limit tokens. An empty string means the message contributes nothing.
func renderTranscriptMessage(m store.Message, limit int) string {
	var sb strings.Builder
	switch m.Role {
	case "system":
		// A seeded search message is a system message, and it is usually the
		// largest thing a fold removes. Only its sources are worth carrying
		// into the memory, so the payload is distilled to them rather than
		// excerpted — see renderSearchSources.
		sources := renderSearchSources(m.Content, limit)
		if sources == "" {
			return ""
		}
		fmt.Fprintf(&sb, "\n### Websuche\n%s\n", sources)
	case "user":
		if m.Content == "" {
			return ""
		}
		fmt.Fprintf(&sb, "\n### Nutzer\n%s\n", clipToTokens(m.Content, limit))
	case "assistant":
		body := m.Content
		if m.Reasoning != "" && body == "" {
			body = "(nur Reasoning)"
		}
		if body == "" && m.ToolCallsJSON == "" {
			return ""
		}
		fmt.Fprintf(&sb, "\n### Assistent\n%s\n", clipToTokens(body, limit))
		if m.ToolCallsJSON != "" {
			// The arguments are bounded too: a write_file call carries a whole file,
			// which used to reach the summariser unbounded at the end of a block.
			fmt.Fprintf(&sb, "(Tool-Aufruf: %s)\n", clipToTokens(m.ToolCallsJSON, limit))
		}
	case "tool":
		fmt.Fprintf(&sb, "\n### Tool-Ergebnis\n%s\n", clipToTokens(m.Content, limit))
	}
	return sb.String()
}

// omissionNote is how a shortened transcript says how much of it was left out. It is
// model-facing text, like the section headings, so it follows them rather than the
// interface language.
func omissionNote(omitted int) string {
	switch omitted {
	case 0:
		return ""
	case 1:
		return "[1 ältere Nachricht ausgelassen]"
	default:
		return fmt.Sprintf("[%d ältere Nachrichten ausgelassen]", omitted)
	}
}

// transcriptIsFiller reports whether a transcript line is one of the notes a fold adds
// rather than something the conversation said. It is what tells "the transcript said
// nothing" apart from "it said only that it had been cut".
func transcriptIsFiller(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "### ") {
		return true
	}
	return strings.HasPrefix(line, "…[gekürzt]") || strings.HasSuffix(line, "ausgelassen]")
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

// truncationMarker closes a block that was cut at its end. Its cost is counted inside the
// budget, not added on top of it: the budget is what bounds the request, so a block that
// overshoots it by the marker is exactly the overshoot the bound exists to stop.
const truncationMarker = "\n…[gekürzt]"

// clipMarker sits between the two halves of a block that was cut in the middle. It is a
// second marker rather than the same one twice because the two cuts mean different
// things: truncationMarker says the text continues after this point, which is true of a
// document summarised from its beginning (clipToTokens is not used there for that
// reason), while this one says text was removed from the middle.
const clipMarker = "\n…[gekürzt]…\n"

// cutToTokens trims s to a token budget from the end, cutting between runes and marking
// the cut so the reader can tell a short text from a shortened one. The result never
// estimates above budget.
//
// It keeps the beginning, which is the right cut whenever the text is a prefix of
// something larger: a document condensed for the summariser, a tool result cut to the
// room a request has left. Those callers say "the first N tokens were read", and a cut
// that also carried the end would make that sentence false. Transcript blocks want the
// other cut — see clipToTokens.
//
// The cut position is found by bisecting against llm.EstimateTokens rather than by
// converting tokens into characters: the budget is expressed in the estimator's own unit,
// and a characters-per-token constant would only approximate the number it was derived
// from.
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
	runes := []rune(s)
	head := longestPrefix(runes, room)
	if head == 0 {
		return ""
	}
	return string(runes[:head]) + truncationMarker
}

// clipToTokens trims s to a token budget, keeping both its beginning and its end.
//
// A head-only cut of a tool result drops the part that usually carries the answer: a build
// log's failure line, a test run's summary, the last entries of a listing. In a transcript
// block there is no "rest of the document" for the marker to point at — the block is the
// whole message — so losing the end loses the conclusion, and the two halves share the
// allowance instead.
func clipToTokens(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if llm.EstimateTokens(s) <= budget {
		return s
	}
	room := budget - llm.EstimateTokens(clipMarker)
	if room < 0 {
		return ""
	}
	runes := []rune(s)
	head := longestPrefix(runes, room/2)
	// The tail gets whatever the head did not need, so the marker is paid for once and
	// the two halves still fit the budget they were given.
	tail := longestSuffix(runes, room-llm.EstimateTokens(string(runes[:head])))
	if head+tail > len(runes) {
		// Two allowances that fit separately can still cover the whole text between them,
		// and then the same runes would be printed twice around the marker. The head's
		// cut is the one that was measured first; the tail gives the overlap back.
		tail = len(runes) - head
	}
	if head == 0 && tail == 0 {
		return ""
	}
	return string(runes[:head]) + clipMarker + string(runes[len(runes)-tail:])
}

// longestPrefix is the largest n whose first n runes estimate within budget.
func longestPrefix(runes []rune, budget int) int {
	if budget <= 0 {
		return 0
	}
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if llm.EstimateTokens(string(runes[:mid])) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// longestSuffix is the largest n whose last n runes estimate within budget.
func longestSuffix(runes []rune, budget int) int {
	if budget <= 0 {
		return 0
	}
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if llm.EstimateTokens(string(runes[len(runes)-mid:])) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}
