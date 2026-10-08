package chat

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"kichat/internal/llm"
	"kichat/internal/store"
)

// Segment kinds, used by the Context Inspector to label each slice.
const (
	SegmentSystem      = "system"
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
	System      int    `json:"system"`
	Memory      int    `json:"memory"`
	Attachments int    `json:"attachments"`
	History     int    `json:"history"`
	ToolSchemas int    `json:"toolSchemas"`
	Reserve     int    `json:"reserve"`

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

	Limit       int
	LimitSource string
	Reserve     int

	// Measured is the previous turn's reported prompt_tokens; valid only for the
	// backend/model that produced it.
	Measured      int
	MeasuredValid bool
	// MeasuredSeq is the newest message sequence that the measurement included.
	MeasuredSeq int
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
}

// Assemble builds the complete request payload and the budget for a chat.
func Assemble(in AssemblyInput) Assembly {
	var budget ContextBudget
	budget.Limit = in.Limit
	budget.LimitSource = in.LimitSource
	budget.Reserve = in.Reserve
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

	// The rendered system prompt is always the first message.
	renderedSystem := RenderPrompt(in.Chat.SystemPrompt, TemplateVars{
		Now:          time.Now(),
		Location:     in.Location,
		Model:        in.Chat.Model,
		Backend:      in.Chat.Backend,
		ContextUsed:  in.Measured,
		ContextLimit: in.Limit,
		Tools:        toolsAvailable,
	})

	msgs := make([]llm.Message, 0, len(in.Messages)+3)
	msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: renderedSystem})

	budget.System = llm.EstimateTokens(renderedSystem)
	budget.Segments = append(budget.Segments, ContextSegment{
		Kind:   SegmentSystem,
		Label:  "System-Prompt",
		Tokens: budget.System,
		Detail: fmt.Sprintf("%d Zeichen, Variablen aufgelöst", len(renderedSystem)),
	})

	// A notice about indexed documents keeps retrieval discoverable without
	// paying for their full text up front.
	notice := buildAttachmentNotice(in)
	if notice != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: notice})
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
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: wrapped})
		tokens := llm.EstimateTokens(wrapped)
		budget.Memory += tokens
		budget.Segments = append(budget.Segments, ContextSegment{
			Kind:   SegmentMemory,
			Label:  "Gedächtnis (Kompaktierung)",
			Tokens: tokens,
			Detail: fmt.Sprintf("Zusammenfassung der Nachrichten %d–%d", latest.FromSeq, latest.ToSeq),
		})
	}

	// Verbatim history, plus any inlined attachment text.
	historyCount := 0
	maxIncluded := 0
	for _, m := range in.Messages {
		if m.Seq < firstVerbatim {
			continue
		}
		if m.Seq > maxIncluded {
			maxIncluded = m.Seq
		}
		lm := toLLMMessage(m)
		// Priced before the attachment text is appended: inlined document text
		// belongs to Attachments below, and counting it here as well made the
		// meter read half again as large as the request it describes.
		historyTokens := llm.EstimateTokens(lm.Content) + llm.EstimateTokens(lm.Reasoning)/2
		if imgs := imagesByMessage[m.ID]; len(imgs) > 0 {
			lm.Images = imgs
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
	if budget.Limit > 0 {
		budget.Percent = float64(budget.Used) / float64(budget.Limit) * 100
		budget.ProjectedRatio = float64(budget.Projected) / float64(budget.Limit)
		budget.OverLimit = budget.Projected > budget.Limit
	}

	return Assembly{
		Messages:           msgs,
		Budget:             budget,
		FirstVerbatimSeq:   firstVerbatim,
		MemorySummary:      memorySummary,
		MemoryCompactionID: memoryID,
		AttachmentNotice:   notice,
		MaxIncludedSeq:     maxIncluded,
	}
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
