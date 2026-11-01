package chat

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/workspace"
)

// Segment kinds, used by the Context Inspector to label each slice.
const (
	SegmentSystem = "system"
	// SegmentProject is a project's own guidance file. It is its own kind rather than
	// part of the system prompt because it is not something the app says — it is text
	// out of the user's working tree, it changes between turns, and the meter should let
	// somebody see what it is costing them.
	SegmentProject = "project"
	// SegmentGit is the same folder's version-control state: which branch, and what has changed
	// since the last commit. It is a kind of its own for the same reason the guidance file is —
	// it is read out of the user's tree rather than said by the app, and the meter should show
	// what it costs.
	SegmentGit         = "git"
	SegmentMemory      = "memory"
	SegmentAttachments = "attachments"
	SegmentHistory     = "history"
	SegmentTools       = "tools"
	SegmentReserve     = "reserve"
)

// ContextSegment is one labelled slice of the context window.
type ContextSegment struct {
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Tokens int    `json:"tokens"`
	Detail string `json:"detail,omitempty"`
}

// ContextBudget is the full accounting of the context window.
type ContextBudget struct {
	Limit       int    `json:"limit"`
	LimitSource string `json:"limitSource"` // backend | override | unknown

	// Ceiling is the backend's own limit on the size of *one request*, when it
	// publishes one, and CeilingSource says where the number came from (llm's
	// CeilingFromRefusal / CeilingFromHeader). It is not the window: Groq's
	// per-minute token allowance bounds a single request from below the 131k window
	// its listing advertises, which is why a chat at 2 % of that window was already
	// unable to be sent.
	Ceiling       int    `json:"ceiling"`
	CeilingSource string `json:"ceilingSource"`
	// EffectiveLimit is what the next request actually has to fit under — the smaller
	// of the window and the ceiling — and it is what Percent, ProjectedRatio and
	// OverLimit are computed against. Reporting them against the window would show
	// headroom that does not exist.
	EffectiveLimit int `json:"effectiveLimit"`

	System      int `json:"system"`
	Memory      int `json:"memory"`
	Attachments int `json:"attachments"`
	History     int `json:"history"`
	ToolSchemas int `json:"toolSchemas"`
	Reserve     int `json:"reserve"`

	// Estimated is computed from the assembled prompt with the heuristic.
	Estimated int `json:"estimated"`
	// Measured is the backend's own prompt_tokens from the most recent turn.
	// When valid it is authoritative; DeltaSince covers whatever was added to
	// the conversation after that measurement was taken.
	Measured      int  `json:"measured"`
	MeasuredValid bool `json:"measuredValid"`
	// MeasuredSeq is the highest message sequence included in that measurement.
	MeasuredSeq int `json:"measuredSeq"`
	// DeltaSince is the estimated cost of messages newer than MeasuredSeq.
	DeltaSince int `json:"deltaSince"`

	// Used is what the meter displays: Measured when valid, else Estimated.
	Used    int     `json:"used"`
	Percent float64 `json:"percent"`
	// Projected adds the response reserve and predicts the next request.
	Projected      int     `json:"projected"`
	ProjectedRatio float64 `json:"projectedRatio"`
	OverLimit      bool    `json:"overLimit"`

	Segments []ContextSegment `json:"segments"`
}

// AssemblyInput carries everything needed to build a request body.
type AssemblyInput struct {
	Chat        store.Chat
	Messages    []store.Message
	Compactions []store.Compaction
	Documents   []store.Document
	Tools       []llm.ToolDefinition
	Location    string

	// KnownTools are the tool names the app can execute, whether or not they are
	// offered in this request. A history call that names none of them is not a call
	// anybody can answer, and Assemble demotes it to text (see toolhistory.go). Nil
	// leaves the history alone, so a caller that does not know the tool set — a
	// preview, a test building an assembly by hand — is never rewritten behind its
	// back; the engine always passes the full set.
	KnownTools []string

	Limit       int
	LimitSource string
	Reserve     int
	// Ceiling is the backend's own per-request limit for this model, or the zero
	// value when it publishes none.
	Ceiling llm.Ceiling

	// Measured is the previous turn's reported prompt_tokens; valid only for the
	// backend/model that produced it.
	Measured      int
	MeasuredValid bool
	// MeasuredSeq is the newest message sequence that the measurement included.
	MeasuredSeq int

	// TurnNotes is text about the conversation rather than about this request: the revert and
	// disk notes. It travels in the request's tail, with the clock (see the append after the
	// history), because it is sent on the round that opens a turn and gone from the next one —
	// in message zero that would invalidate the cache of the whole history twice per turn.
	TurnNotes string
}

// Assembly is the result: the messages to send plus the budget breakdown.
type Assembly struct {
	Messages []llm.Message
	Budget   ContextBudget

	// FirstVerbatimSeq is the seq of the oldest message kept in full. Messages
	// before it are represented by the memory summary.
	FirstVerbatimSeq int
	// MemorySummary is the compaction summary in force, if any.
	MemorySummary      string
	MemoryCompactionID string
	// AttachmentNotice describes indexed documents.
	AttachmentNotice string
	// MaxIncludedSeq is the highest stored message sequence present in the
	// request. It lets a backend's reported prompt_tokens be tied to an exact
	// conversation state.
	MaxIncludedSeq int

	// AgedReads are the files whose `read_file` result was dropped from the request's tail
	// to keep the reply from re-sending everything it has ever read (see ageToolResults).
	// The caller uses them for one thing: the app's memory that the turn has already read that
	// file has to go too, or the model asking for it again is answered "unchanged" about text
	// it can no longer see.
	AgedReads []string
}

// Assemble builds the complete request payload and the budget for a chat.
func Assemble(in AssemblyInput) Assembly {
	var budget ContextBudget
	budget.Limit = in.Limit
	budget.LimitSource = in.LimitSource
	budget.Reserve = in.Reserve
	budget.Ceiling = in.Ceiling.Tokens
	budget.CeilingSource = in.Ceiling.Source
	budget.EffectiveLimit = in.Limit
	// A ceiling below the window is the binding limit, and a window the backend does
	// not report is not a reason to ignore a ceiling it does: the smaller of the two
	// known numbers is what one request has to fit under.
	if budget.Ceiling > 0 && (budget.EffectiveLimit <= 0 || budget.Ceiling < budget.EffectiveLimit) {
		budget.EffectiveLimit = budget.Ceiling
	}
	budget.Measured = in.Measured
	budget.MeasuredValid = in.MeasuredValid

	// The newest compaction wins; it already merges earlier summaries, which is
	// what keeps compaction cost linear rather than quadratic.
	var latest *store.Compaction
	ordered := append([]store.Compaction(nil), in.Compactions...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ToSeq < ordered[j].ToSeq })
	if len(ordered) > 0 {
		latest = &ordered[len(ordered)-1]
	}

	firstVerbatim := 0
	memorySummary := ""
	memoryID := ""
	if latest != nil {
		firstVerbatim = latest.ToSeq + 1
		memorySummary = latest.Summary
		memoryID = latest.ID
	}

	// Group inlined document text by the message it belongs to, and keep images
	// apart: an image's document holds base64 bytes, which must travel as an
	// image part rather than as document text.
	inlineByMessage := map[string][]store.Document{}
	imagesByMessage := map[string][]string{}
	docByID := map[string]store.Document{}
	for _, d := range in.Documents {
		docByID[d.ID] = d
	}
	attachmentByDoc := map[string]store.Attachment{}
	for _, m := range in.Messages {
		for _, a := range m.Attachments {
			if a.DocumentID != "" {
				attachmentByDoc[a.DocumentID] = a
			}
			if a.Placement != store.PlacementInline {
				continue
			}
			d, ok := docByID[a.DocumentID]
			if !ok {
				continue
			}
			if isImageAttachment(a) {
				imagesByMessage[m.ID] = append(imagesByMessage[m.ID], d.Text)
				continue
			}
			inlineByMessage[m.ID] = append(inlineByMessage[m.ID], d)
		}
	}

	toolTokens := llm.EstimateToolTokens(in.Tools)
	toolsAvailable := make([]string, 0, len(in.Tools))
	for _, t := range in.Tools {
		toolsAvailable = append(toolsAvailable, t.Name)
	}

	// The rendered system prompt is always the first message — and the only
	// system message. Everything the app puts ahead of the history (the
	// attachment notice, a compaction's memory) travels inside it rather than as
	// a message of its own.
	//
	// A strict chat template accepts exactly one: the ternary models run on a
	// Qwen3 template that reads messages[0] as the system prompt and raises
	// "System message must be at the beginning." for any other system message.
	// A second leading one therefore made every turn after a compact fail — the
	// tokens fit, the shape did not. Joining the parts preserves their order and
	// their delimiters, so the memory is still a system message rather than a
	// fake user turn.
	renderedSystem := RenderPrompt(in.Chat.SystemPrompt, TemplateVars{
		Now:          time.Now(),
		Location:     in.Location,
		Model:        in.Chat.Model,
		Backend:      in.Chat.Backend,
		ContextUsed:  in.Measured,
		ContextLimit: in.Limit,
		Tools:        toolsAvailable,
	})

	systemParts := []string{renderedSystem}

	budget.System = llm.EstimateTokens(renderedSystem)
	budget.Segments = append(budget.Segments, ContextSegment{
		Kind:   SegmentSystem,
		Label:  "System-Prompt",
		Tokens: budget.System,
		Detail: fmt.Sprintf("%d Zeichen, Variablen aufgelöst", len(renderedSystem)),
	})

	// A project's own instructions, read from its folder on every turn. They travel as
	// markup rather than as prose because the rule for using them lives in the system
	// prompt, which exists once per interface language, while this payload is the same
	// in all of them. The read is a stat in the common case: internal/workspace memoises
	// on the file's size and modification time.
	if guidance, detail, ok := projectGuidance(in.Chat); ok {
		systemParts = append(systemParts, guidance)
		tokens := llm.EstimateTokens(guidance)
		budget.Memory += tokens
		budget.Segments = append(budget.Segments, ContextSegment{
			Kind:   SegmentProject,
			Label:  workspace.AgentsFileName,
			Tokens: tokens,
			Detail: detail,
		})
	}

	// The working tree, when the folder is a git repository. It is silent for a folder that is
	// not a repository, which is the ordinary case of a scratch folder.
	//
	// It deliberately does *not* join the system message, and that is a cost decision rather
	// than a cosmetic one. The block changes the moment the assistant edits a file, and a
	// hosted backend's prompt cache matches the request from its start (DEEPSEEK.md §5), so a
	// block that differs from round to round inside message zero makes every round of a coding
	// reply a miss for the whole history behind it — the first edit would re-bill the entire
	// prompt at the miss price, on each of the reply's rounds. It rides at the end of the
	// request instead (see the append after the history below), so the system message and the
	// history stay byte-identical across a reply and only the newest block is uncached.
	//
	// The full list of paths is for the turn's first request only, the one that opens with the
	// user's question; every round after that carries the summary. Forty paths with their line
	// counts reach the 4 KiB cap, and a block at the tail is the one part of the request that is
	// never a cache hit — so the list is worth paying for once, as orientation, and not on each
	// of the rounds of a reply that is already holding the files it wrote.
	firstRound := len(in.Messages) == 0 ||
		in.Messages[len(in.Messages)-1].Role == string(llm.RoleUser)
	var gitState string
	if state, detail, ok := projectGitState(in.Chat, firstRound); ok {
		gitState = state
		tokens := llm.EstimateTokens(state)
		budget.Memory += tokens
		budget.Segments = append(budget.Segments, ContextSegment{
			Kind:   SegmentGit,
			Label:  "Git-Status",
			Tokens: tokens,
			Detail: detail,
		})
	}

	// A notice about indexed documents keeps retrieval discoverable without
	// paying for their full text up front.
	notice := buildAttachmentNotice(in)
	if notice != "" {
		systemParts = append(systemParts, notice)
		tokens := llm.EstimateTokens(notice)
		budget.Memory += tokens
		budget.Segments = append(budget.Segments, ContextSegment{
			Kind:   SegmentMemory,
			Label:  "Dokument-Index",
			Tokens: tokens,
			Detail: "Hinweis auf indizierte Dateien (Text bleibt ausgelagert)",
		})
	}

	if memorySummary != "" {
		wrapped := "<conversation_memory>\n" + memorySummary + "\n</conversation_memory>"
		systemParts = append(systemParts, wrapped)
		tokens := llm.EstimateTokens(wrapped)
		budget.Memory += tokens
		budget.Segments = append(budget.Segments, ContextSegment{
			Kind:   SegmentMemory,
			Label:  "Gedächtnis (Kompaktierung)",
			Tokens: tokens,
			Detail: fmt.Sprintf("Zusammenfassung der Nachrichten %d–%d", latest.FromSeq, latest.ToSeq),
		})
	}

	msgs := make([]llm.Message, 0, len(in.Messages)+1)
	msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: strings.Join(systemParts, "\n\n")})

	// The newest user message opens the reply being assembled, and that boundary is what tool
	// ageing is scoped to: the results of *this* answer are the ones that grow between the
	// folds, and an earlier turn's evidence is compaction's to summarise.
	newestUserSeq := 0
	for _, m := range in.Messages {
		if m.Role == string(llm.RoleUser) && m.Seq > newestUserSeq {
			newestUserSeq = m.Seq
		}
	}

	// Verbatim history, plus any inlined attachment text.
	historyCount := 0
	maxIncluded := 0
	// currentReply is index-aligned with msgs: it says which of them belong to the reply
	// being answered, which is what ageToolResults is scoped to. It starts with the system
	// message's own entry, which msgs already carries.
	currentReply := []bool{false}
	for _, m := range in.Messages {
		if m.Seq < firstVerbatim {
			continue
		}
		lm := toLLMMessage(m)
		// A picture is a part of a *user* turn only: the OpenAI-compatible schema has
		// no image part for an assistant message, and a backend that validates the
		// shape refuses the whole request for one (GROQ.md §6). It is reachable — a
		// picture a tool generated is filed as an attachment of the assistant row
		// that asked for it — so it is kept off the wire and out of the meter, which
		// describes what is sent. The attachment stays in the transcript and the
		// renderer.
		if lm.Role != llm.RoleAssistant {
			lm.Images = imagesByMessage[m.ID]
		}
		// An assistant turn that produced nothing is not sent. Reasoning is not part
		// of the wire format, so a reply that only thought — or one a failed or
		// stopped request left behind — reaches the backend as
		// `{"role":"assistant","content":""}` with no calls. Mistral validates that
		// shape and refuses the whole request (HTTP 400, code 3240; MISTRAL.md §6),
		// and the refused turn then writes yet another empty row, so every later
		// send of that chat is refused for a message the user cannot see or delete.
		// The row stays in the transcript; it is only left out of the request. A
		// turn that carries an inlined document is kept whatever its text, because
		// the document is the message.
		if lm.Role == llm.RoleAssistant && lm.Content == "" && len(lm.ToolCalls) == 0 &&
			len(inlineByMessage[m.ID]) == 0 {
			continue
		}
		if m.Seq > maxIncluded {
			maxIncluded = m.Seq
		}
		// A seeded search is stored as a system message and belongs next to the
		// question it was run for, but the template allows a system message only
		// at position 0. It therefore travels as a user turn carrying the search
		// payload and its untrusted-data notice; the stored row, and the source
		// card the user reads back, are unchanged.
		if lm.Role == llm.RoleSystem {
			lm.Role = llm.RoleUser
		}
		// Priced before the attachment text is appended: inlined document text
		// belongs to Attachments below, and counting it here as well made the
		// meter read half again as large as the request it describes.
		historyTokens := llm.EstimateTokens(lm.Content) + llm.EstimateTokens(lm.Reasoning)/2
		if imgs := lm.Images; len(imgs) > 0 {
			tokens := llm.EstimateImageTokens(len(imgs))
			budget.Attachments += tokens
			budget.Segments = append(budget.Segments, ContextSegment{
				Kind:   SegmentAttachments,
				Label:  "Bilder",
				Tokens: tokens,
				Detail: fmt.Sprintf("%d Bild(er) in Nachricht #%d", len(imgs), m.Seq),
			})
		}
		if docs := inlineByMessage[m.ID]; len(docs) > 0 {
			var sb strings.Builder
			sb.WriteString(lm.Content)
			tokens := 0
			for _, d := range docs {
				sb.WriteString("\n\n")
				sb.WriteString(renderDocumentBlock(d, attachmentByDoc[d.ID]))
				tokens += d.TokenCount
			}
			lm.Content = sb.String()
			budget.Attachments += tokens
			budget.Segments = append(budget.Segments, ContextSegment{
				Kind:   SegmentAttachments,
				Label:  "Dateianhänge",
				Tokens: tokens,
				Detail: fmt.Sprintf("%d Datei(en) in Nachricht #%d", len(docs), m.Seq),
			})
		}
		budget.History += historyTokens
		historyCount++
		msgs = append(msgs, lm)
		currentReply = append(currentReply, m.Seq > newestUserSeq)
	}

	// A stored call can name a tool the app does not have — a model that mangled the
	// name on a backend that does not validate calls is the way that happens. It is
	// demoted to text here rather than replayed as a tool call nobody can answer, and
	// the cost of the rewrite is added to the history segment so the meter still
	// describes the request that is sent.
	if in.KnownTools != nil {
		var rewriteDelta int
		msgs, rewriteDelta = sanitiseToolHistory(msgs, toolNameSet(in.KnownTools))
		budget.History += rewriteDelta
	}

	// A reply's tool results are the part of the request that grows between folds, and every
	// round re-sends all of them. The newest ones are what the round is working from; the
	// older ones are dropped to a notice (ageToolResults) and the saving comes off the history
	// segment, so the meter keeps describing the request that is sent.
	msgs, agedReads, agedTokens := ageToolResults(msgs, currentReply)
	budget.History -= agedTokens

	// The per-turn state rides at the very end of the request, after everything the cache is
	// meant to keep: the exact clock, in a project the working tree, and the turn's notes about the
	// conversation. The last message is the one that changed anyway — the user's question on the
	// first round, the newest tool result on every round after it — so nothing in front of it
	// shifts, where a block in the system message shifted all of it.
	//
	// The clock belongs here for the same reason, and it is why `{{datetime}}` renders at day
	// precision (prompt.go): a reply's rounds are minutes apart, so a minute-granular value in
	// message zero made every round a miss for the whole history behind it. After the history, a
	// value that moves costs only its own tokens.
	//
	// Appended to that message rather than sent as one of its own: on the first round the newest
	// message is the user's question, and a second user turn in a row is a shape the strict chat
	// templates this app already works around refuse (see the note on the single system message
	// above). Appending text changes neither the roles nor the count. A request with no history at
	// all — a preview — has only the system message, and no cache worth protecting, so the block
	// is left out rather than written into message zero.
	//
	// `{{context_used}}` and `{{time}}` can still move message zero between requests. Both are the
	// user's own text and are left alone; the shipped presets use neither.
	turnState := turnClock(time.Now())
	notes := strings.TrimSpace(in.TurnNotes)
	// Priced together and apart from the git block, which has a segment of its own: what the
	// meter shows is what is sent, and the two are read for different reasons.
	priced := turnState
	if notes != "" {
		priced += "\n\n" + notes
	}
	if tokens := llm.EstimateTokens(priced); tokens > 0 {
		budget.Memory += tokens
		budget.Segments = append(budget.Segments, ContextSegment{
			Kind:   SegmentMemory,
			Label:  "Zeit und Hinweise",
			Tokens: tokens,
			Detail: "Zeitpunkt und Notizen dieses Turns, am Ende des Requests statt im System-Prompt",
		})
	}
	if gitState != "" {
		turnState += "\n\n" + gitState
	}
	// The notes go last so they are the newest thing the model reads: a file that was put back or
	// changed on disk is a correction, and a correction buried under a list of paths is read as
	// context.
	if notes != "" {
		turnState += "\n\n" + notes
	}
	if len(msgs) > 1 {
		last := len(msgs) - 1
		if msgs[last].Content == "" {
			msgs[last].Content = turnState
		} else {
			msgs[last].Content += "\n\n" + turnState
		}
	}

	budget.ToolSchemas = toolTokens
	if toolTokens > 0 {
		budget.Segments = append(budget.Segments, ContextSegment{
			Kind:   SegmentTools,
			Label:  "Tool-Schemas",
			Tokens: toolTokens,
			Detail: fmt.Sprintf("%d Tools deklariert", len(in.Tools)),
		})
	}
	if budget.History > 0 {
		budget.Segments = append(budget.Segments, ContextSegment{
			Kind:   SegmentHistory,
			Label:  "Verlauf",
			Tokens: budget.History,
			Detail: fmt.Sprintf("%d Nachrichten im Volltext", historyCount),
		})
	}
	if in.Reserve > 0 {
		budget.Segments = append(budget.Segments, ContextSegment{
			Kind:   SegmentReserve,
			Label:  "Reserve für Antwort",
			Tokens: in.Reserve,
			Detail: "Platz, der für die nächste Antwort frei bleibt",
		})
	}

	budget.Estimated = budget.System + budget.Memory + budget.Attachments +
		budget.History + budget.ToolSchemas

	// A measurement is only comparable when every message it counted is still
	// part of the request. Since it counts the whole prompt, a measurement taken
	// at the fold boundary itself (MeasuredSeq == firstVerbatim-1) counted exactly
	// the messages that are now replaced by the summary, so it over-reports the
	// window by their full size and has to be dropped as well.
	measuredValid := budget.MeasuredValid && budget.Measured > 0 &&
		in.MeasuredSeq >= firstVerbatim && in.MeasuredSeq >= 0
	budget.MeasuredValid = measuredValid
	if measuredValid {
		// The measurement predates the newest messages, so add their estimated
		// cost rather than under-reporting the window.
		delta := 0
		for _, m := range in.Messages {
			if m.Seq <= in.MeasuredSeq || m.Seq < firstVerbatim {
				continue
			}
			delta += llm.EstimateTokens(m.Content) + llm.EstimateTokens(m.Reasoning)/2
			// Attachments of newer messages are part of the next request and no
			// measurement can have counted them: they were added afterwards.
			delta += llm.EstimateImageTokens(len(imagesByMessage[m.ID]))
			for _, d := range inlineByMessage[m.ID] {
				delta += d.TokenCount
			}
		}
		budget.MeasuredSeq = in.MeasuredSeq
		budget.DeltaSince = delta
		budget.Used = budget.Measured + delta
	} else {
		budget.Used = budget.Estimated
	}
	budget.Projected = budget.Used + in.Reserve
	// Against the effective limit, not the window: the meter exists to say whether the
	// next request can be sent, and on a backend whose per-request ceiling is smaller
	// than its window a percentage of the window answers a different question. It is
	// also what the automatic threshold reads (PlanCompaction compares
	// ProjectedRatio), so the quiet fold and the meter cannot disagree about how full
	// the request is.
	if budget.EffectiveLimit > 0 {
		budget.Percent = float64(budget.Used) / float64(budget.EffectiveLimit) * 100
		budget.ProjectedRatio = float64(budget.Projected) / float64(budget.EffectiveLimit)
		budget.OverLimit = budget.Projected > budget.EffectiveLimit
	}

	return Assembly{
		Messages:           msgs,
		Budget:             budget,
		FirstVerbatimSeq:   firstVerbatim,
		MemorySummary:      memorySummary,
		MemoryCompactionID: memoryID,
		AttachmentNotice:   notice,
		MaxIncludedSeq:     maxIncluded,
		AgedReads:          agedReads,
	}
}

// turnClock is the exact time of the request being assembled, for its tail.
//
// ISO rather than a sentence: the tail is read by a model answering in whatever language the
// user writes, and the block in front of it (a project's working tree) is the only other prose
// there. Minutes, not seconds — a second is a difference no answer can act on.
func turnClock(now time.Time) string {
	zone, _ := now.Zone()
	return fmt.Sprintf("<turn_clock>%s (%s)</turn_clock>", now.Format("2006-01-02 15:04"), zone)
}

// isImageAttachment reports whether an attachment's payload is an image. An
// image's document text is the raw base64 of the file, so it is sent as an
// image part instead of being inlined as document text.
func isImageAttachment(a store.Attachment) bool {
	return strings.HasPrefix(strings.ToLower(a.Mime), "image/") || a.Kind == "image"
}

// renderDocumentBlock wraps document text in an explicit delimiter. The wrapper
// is what makes the untrusted-data rule enforceable: file contents are data.
func renderDocumentBlock(d store.Document, a store.Attachment) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<document title=%q", d.Title)
	if a.Filename != "" && a.Filename != d.Title {
		fmt.Fprintf(&sb, " filename=%q", a.Filename)
	}
	if d.PageCount > 0 {
		fmt.Fprintf(&sb, " pages=%d", d.PageCount)
	}
	if d.SourceURL != "" {
		fmt.Fprintf(&sb, " source=%q", d.SourceURL)
	}
	sb.WriteString(">\n")
	sb.WriteString(d.Text)
	sb.WriteString("\n</document>")
	return sb.String()
}

func buildAttachmentNotice(in AssemblyInput) string {
	var lines []string
	for _, m := range in.Messages {
		for _, a := range m.Attachments {
			if a.Placement != store.PlacementIndexed {
				continue
			}
			if a.Warning != "" {
				lines = append(lines, fmt.Sprintf("- %s — Warnung: %s", a.Filename, a.Warning))
				continue
			}
			lines = append(lines, fmt.Sprintf("- %s (%d Seiten, ca. %d Tokens)",
				a.Filename, a.Pages, a.TokenEstimate))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Indizierte Dokumente in diesem Chat. Der Volltext ist NICHT im Kontext, " +
		"sondern über die Tools search_documents und read_pages abrufbar:\n")
	for _, line := range lines {
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// toLLMMessage converts a stored message into the wire form.
func toLLMMessage(m store.Message) llm.Message {
	lm := llm.Message{
		Role:       llm.Role(m.Role),
		Content:    m.Content,
		Reasoning:  m.Reasoning,
		Name:       m.Name,
		ToolCallID: m.ToolCallID,
	}
	if m.ToolCallsJSON != "" {
		var calls []llm.ToolCall
		if err := json.Unmarshal([]byte(m.ToolCallsJSON), &calls); err == nil {
			lm.ToolCalls = calls
		}
	}
	return lm
}

// ToLLMMessages converts stored messages without attachment inlining.
func ToLLMMessages(msgs []store.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toLLMMessage(m))
	}
	return out
}
