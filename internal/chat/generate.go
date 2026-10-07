package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kichat/internal/config"
	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/tools"
)

type generateInput struct {
	Chat        store.Chat
	Provider    llm.Provider
	BackendID   string
	ModelID     string
	Model       llm.ModelInfo
	Settings    config.Settings
	UseNative   bool
	MaxToolIter int
}

type generateResult struct {
	PrimaryMessageID string
	LastMessageID    string
	Text             string
	Reasoning        string
	ToolCalls        []llm.ToolCall
	Stats            *llm.GenerationStats
	Usage            *llm.Usage
}

// generate runs the model, executing tools until the model stops asking for
// them. Every model reply becomes its own assistant message, so a transcript
// replays exactly as it happened.
func (e *Engine) generate(ctx context.Context, in generateInput) (generateResult, error) {
	var result generateResult

	maxIter := in.MaxToolIter
	if maxIter <= 0 {
		maxIter = 1
	}

	for iteration := 0; iteration < maxIter; iteration++ {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}

		extraSystem := ""
		if !in.UseNative {
			extraSystem = tools.TextProtocolHint
		}
		assembly, toolDefs, err := e.buildAssembly(ctx, &in.Chat, in.Provider,
			in.BackendID, in.ModelID, in.Settings, in.Model, extraSystem)
		if err != nil {
			return result, err
		}
		e.emit("chat:budget", map[string]any{"chatId": in.Chat.ID, "budget": assembly.Budget})

		row, err := e.createAssistantMessage(in.Chat.ID, in.BackendID, in.ModelID)
		if err != nil {
			return result, err
		}
		if result.PrimaryMessageID == "" {
			result.PrimaryMessageID = row.ID
		}
		e.emit("chat:start", map[string]any{
			"chatId":    in.Chat.ID,
			"messageId": row.ID,
			"backend":   in.BackendID,
			"model":     in.ModelID,
			"iteration": iteration,
		})

		req := llm.ChatRequest{
			Model:       in.ModelID,
			Messages:    assembly.Messages,
			Temperature: in.Chat.Temperature,
			TopP:        in.Chat.TopP,
			NumCtx:      in.Chat.NumCtx,
			MaxTokens:   in.Settings.Sampler.MaxTokens,
		}
		if in.UseNative && len(toolDefs) > 0 {
			req.Tools = toolDefs
		}

		events, err := in.Provider.Chat(ctx, req)
		if err != nil {
			row.Status = store.StatusCancelled
			row.Content = ""
			_ = e.Store.UpdateMessage(row)
			return result, err
		}

		text, reasoning, calls, stats, usage, streamErr := e.consume(ctx, in.Chat.ID, row.ID, events)

		// A model that cannot call tools natively may have emitted a tool call in
		// text form; parse it out and hide the protocol from the user.
		if !in.UseNative && len(calls) == 0 {
			if parsed, visible := tools.ParseTextToolCalls(text); len(parsed) > 0 {
				calls = parsed
				text = visible
			}
		}

		row.Content = text
		row.Reasoning = reasoning
		row.Status = store.StatusComplete
		if stats != nil {
			row.TokensPerSecond = stats.TokensPerSecond
			row.TTFTMs = stats.TimeToFirstTokenMs
			row.GenerationMs = stats.GenerationMs
			row.StatsSource = stats.Source
			row.StopReason = stats.StopReason
		}
		if usage != nil {
			row.PromptTokens = usage.PromptTokens
			row.CompletionTokens = usage.CompletionTokens
		}
		if ctx.Err() != nil {
			row.Status = store.StatusInterrupted
			row.StopReason = "interrupted"
		}
		// Persist the reply together with its tool calls before executing them, so
		// the transcript is replayable even if the process dies mid-tool.
		row.ToolCallsJSON = encodeToolCalls(calls)
		_ = e.Store.UpdateMessage(row)

		result.Text = joinText(result.Text, text)
		result.Reasoning = joinText(result.Reasoning, reasoning)
		result.ToolCalls = append(result.ToolCalls, calls...)
		result.Stats = stats
		result.Usage = usage
		result.LastMessageID = row.ID

		if usage != nil && usage.PromptTokens > 0 {
			e.setMeasurement(in.Chat.ID, Measurement{
				Backend:      in.BackendID,
				Model:        in.ModelID,
				PromptTokens: usage.PromptTokens,
				Seq:          row.Seq,
				At:           time.Now(),
			})
		}

		if streamErr != nil && ctx.Err() == nil {
			return result, streamErr
		}
		if len(calls) == 0 {
			return result, nil
		}
		if ctx.Err() != nil {
			// Close out every pending call so no assistant tool_calls is left
			// without a matching tool result. Backends reject such a history.
			e.cancelRemainingCalls(in.Chat.ID, row, calls, 0)
			return result, ctx.Err()
		}

		e.emitStatus(in.Chat.ID, "tool")
		for i, call := range calls {
			if ctx.Err() != nil {
				e.cancelRemainingCalls(in.Chat.ID, row, calls, i)
				return result, ctx.Err()
			}
			e.runTool(ctx, in.Chat.ID, row, call)
		}
		e.emitStatus(in.Chat.ID, "idle")
	}
	return result, nil
}

// runTool executes one tool call and records both the call and its result.
func (e *Engine) runTool(ctx context.Context, chatID string, row store.Message, call llm.ToolCall) {
	record := store.ToolCallRecord{
		ID:        call.ID,
		MessageID: row.ID,
		ChatID:    chatID,
		Name:      call.Name,
		ArgsJSON:  call.Arguments,
		Status:    store.ToolPending,
		CreatedAt: time.Now(),
	}
	_ = e.Store.UpsertToolCall(record)
	e.emit("chat:tool", map[string]any{"chatId": chatID, "toolCall": record})

	start := time.Now()
	output, execErr := e.executeTool(ctx, chatID, call)
	record.DurationMs = time.Since(start).Milliseconds()

	if execErr != nil {
		record.Status = store.ToolError
		record.Error = execErr.Error()
		output = fmt.Sprintf("Tool-Fehler: %s", execErr.Error())
	} else {
		record.Status = store.ToolOK
	}
	record.ResultJSON = resultJSON(output)
	_ = e.Store.UpsertToolCall(record)
	e.emit("chat:tool", map[string]any{"chatId": chatID, "toolCall": record})

	e.appendToolResult(chatID, call, output, record.Status)
}

func (e *Engine) executeTool(ctx context.Context, chatID string, call llm.ToolCall) (string, error) {
	if e.Tools == nil {
		return "", fmt.Errorf("keine Tools verfügbar")
	}
	return e.Tools.Execute(ctx, chatID, call.Name, call.Arguments)
}

// cancelRemainingCalls writes a cancelled tool result for every call from index
// onwards. Without this, stopping a generation mid-tool leaves an assistant
// message referencing tool calls that never produced a result, and the next
// request would be rejected by the backend.
func (e *Engine) cancelRemainingCalls(chatID string, row store.Message, calls []llm.ToolCall, from int) {
	for i := from; i < len(calls); i++ {
		call := calls[i]
		record := store.ToolCallRecord{
			ID:         call.ID,
			MessageID:  row.ID,
			ChatID:     chatID,
			Name:       call.Name,
			ArgsJSON:   call.Arguments,
			ResultJSON: resultJSON(cancelledToolOutput),
			Status:     store.ToolCancelled,
			CreatedAt:  time.Now(),
		}
		_ = e.Store.UpsertToolCall(record)
		e.emit("chat:tool", map[string]any{"chatId": chatID, "toolCall": record})
		e.appendToolResult(chatID, call, cancelledToolOutput, store.ToolCancelled)
	}
}

const cancelledToolOutput = "Der Nutzer hat die Generierung abgebrochen. Dieses Tool wurde nicht ausgeführt."

// appendToolResult persists a tool result as a message so the next iteration of
// the loop sees it.
func (e *Engine) appendToolResult(chatID string, call llm.ToolCall, output, status string) {
	seq, err := e.Store.NextSeq(chatID)
	if err != nil {
		return
	}
	msg := store.Message{
		ID:         newID(),
		ChatID:     chatID,
		Seq:        seq,
		Role:       string(llm.RoleTool),
		Content:    output,
		Name:       call.Name,
		ToolCallID: call.ID,
		Status:     status,
		CreatedAt:  time.Now(),
	}
	if err := e.Store.CreateMessage(msg); err != nil {
		return
	}
	e.emit("chat:message", map[string]any{"chatId": chatID, "message": msg})
}

func resultJSON(output string) string {
	raw, err := json.Marshal(map[string]any{"content": output})
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// --- streaming -------------------------------------------------------------

// consume reads the stream, emits batched deltas to the UI, and accumulates the
// reply. Batching keeps a fast local model at 60+ tokens/s from flooding the
// webview with one event per token.
func (e *Engine) consume(ctx context.Context, chatID, messageID string,
	events <-chan llm.StreamEvent) (text, reasoning string, calls []llm.ToolCall,
	stats *llm.GenerationStats, usage *llm.Usage, err error) {

	const (
		flushInterval = 50 * time.Millisecond
		flushChars    = 200
	)

	var (
		textBuf      strings.Builder
		reasoningBuf strings.Builder
		lastFlush    = time.Now()
	)

	flush := func(force bool) {
		if textBuf.Len() == 0 && reasoningBuf.Len() == 0 {
			return
		}
		if !force && textBuf.Len() < flushChars && time.Since(lastFlush) < flushInterval {
			return
		}
		if textBuf.Len() > 0 {
			e.emit("chat:delta", map[string]any{
				"chatId": chatID, "messageId": messageID, "text": textBuf.String(),
			})
			textBuf.Reset()
		}
		if reasoningBuf.Len() > 0 {
			e.emit("chat:reasoning", map[string]any{
				"chatId": chatID, "messageId": messageID, "text": reasoningBuf.String(),
			})
			reasoningBuf.Reset()
		}
		lastFlush = time.Now()
	}

	for ev := range events {
		switch ev.Type {
		case llm.EventDelta:
			text += ev.Text
			textBuf.WriteString(ev.Text)
			flush(false)
		case llm.EventReasoning:
			reasoning += ev.Text
			reasoningBuf.WriteString(ev.Text)
			flush(false)
		case llm.EventToolCall:
			if ev.ToolCall != nil {
				calls = append(calls, *ev.ToolCall)
			}
		case llm.EventUsage:
			usage = ev.Usage
		case llm.EventDone:
			stats = ev.Stats
		case llm.EventError:
			err = fmt.Errorf("%s", ev.Error)
		}
	}
	flush(true)
	return text, reasoning, calls, stats, usage, err
}

func joinText(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n\n" + b
	}
}
