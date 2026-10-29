package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/images"
	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/tools"
	"kichat/internal/trace"
	"kichat/internal/uimsg"
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
	// Turn is the trace's number for this turn, carried so a provider request and a
	// tool call can be read together with the turn they belong to.
	Turn string
	// Sources is the turn's numbering of the web sources, shared with the pre-flight
	// search so every list the model reads continues one sequence.
	Sources *tools.SourceList
}

type generateResult struct {
	PrimaryMessageID string
	LastMessageID    string
	Text             string
	Reasoning        string
	ToolCalls        []llm.ToolCall
	Stats            *llm.GenerationStats
	Usage            *llm.Usage
	// Status is the status of the last reply row that was written. It travels
	// back to the caller so the end-of-turn event can report the same thing the
	// database holds: reporting "complete" for a reply stored as failed made the
	// event and the row disagree until something else refreshed them.
	Status string
}

// persist reports a write that failed rather than dropping it on the floor. An
// answer the user can read but that never reached the database is gone at the
// next start, and discovering that later is worse than being told now. The turn
// carries on either way: the reply is already on screen, so a warning describes
// what happened more truthfully than aborting would. There is nothing to return
// — no caller can do anything about it, which is exactly why it used to be lost.
func (e *Engine) persist(chatID, what string, err error) {
	if err == nil {
		return
	}
	e.emit(events.Warning, map[string]any{
		"chatId":  chatID,
		"warning": fmt.Sprintf("%s konnte nicht gespeichert werden: %s", what, err.Error()),
	})
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
	// Tool results are charged against one budget for the whole answer, so a
	// single large scrape cannot fill the window on its own.
	toolBudget := newToolOutputBudget(in.Settings)

	// A tool-heavy turn is many requests back to back, and the minute's *request*
	// allowance can be smaller than the rounds the model will ask for. Reading it before
	// the first request is what turns that into a plan (pace.go): the turn's rounds are
	// spaced at the rate the allowance grants, so it does not spend every slot of the
	// minute and then sit at the wall with the rest of the answer still to come. It is
	// empty — and this loop is exactly as it was — for every backend that publishes no
	// such rate and for every turn that fits in what the minute has left.
	pacing := planTurnRequests(in.Provider, in.ModelID, maxIter)

	// The files the user put back since this chat's last turn, if any. Their tool results are
	// still in the history and still claim what the model wrote, so the note travels with the
	// request that opens the turn — and the marker is cleared once a request carrying it has
	// actually reached the model, so a turn that never left the app keeps its correction.
	revertNote := e.revertNote(in.Chat)
	revertDelivered := false
	if revertNote != "" {
		defer func() {
			if revertDelivered {
				e.persist(in.Chat.ID, "Die zurückgesetzten Dateien", e.Store.ClearReverts(in.Chat.ID))
			}
		}()
	}

	// And the other way a tool result goes false: the file was edited outside this app, so the
	// text on disk is no longer what the model wrote, while its own results still say it is. The
	// note is spent per outside version rather than per turn (Engine.externalNote), and a turn
	// that never reaches the model keeps it, exactly like the revert note above.
	diskNote, diskNoted := e.externalNote(in.Chat)
	diskDelivered := false
	if diskNote != "" {
		defer func() {
			if !diskDelivered {
				return
			}
			for path, version := range diskNoted {
				e.persist(in.Chat.ID, "Der Vermerk auf geänderte Dateien",
					e.Store.MarkFileSnapshotNoted(in.Chat.ID, path, version.disk, version.after))
			}
		}()
	}

	for iteration := 0; iteration < maxIter; iteration++ {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		// The planned wait for this round, reported as a phase of its own while it lasts.
		// A turn whose rounds are spread is answering, not stuck: the allowance is not
		// spent and nothing was refused, which is why this wait says something else.
		if !e.holdRound(ctx, in.Chat.ID, pacing.waitFor(iteration, time.Now())) {
			return result, ctx.Err()
		}

		// A tool round is where a turn grows: the request that just finished was priced
		// against a history without the reply it produced and without the tool results the
		// round then appended. On a backend whose per-request ceiling is a fraction of the
		// window, those two are enough to make the *next* request unsendable — and a
		// refusal here costs the whole answer, not one request, because the fold that
		// answers it restarts the turn. So the ceiling is asked again before every round
		// after the first, through the same planner and with the same arithmetic as the
		// check that ran before the turn.
		//
		// A backend that publishes no ceiling has no wall to fold for, and the window it
		// does have is only folded on request: an answer that folds itself mid-stream pays
		// a summarisation the user waits through, so Settings → Compaction carries the
		// switch (Compact.MidTurnCheck) rather than this loop deciding for them. With it on,
		// the check is the same one a local model gets before every turn — the threshold
		// against the effective limit — just asked where the round grew.
		if iteration > 0 && (e.ceilingFor(in.BackendID, in.ModelID).Tokens > 0 ||
			autoFoldThreshold(in.Settings, in.Chat, true) > 0) {
			if err := e.autoFold(ctx, &in.Chat, in.Provider, in.BackendID, in.ModelID,
				in.Model, in.Settings, true); err != nil {
				// Same contract as the fold before the turn: a failed summarisation is
				// reported, and the round is sent anyway. Refusing to send would turn a
				// fold that could not be written into an answer that never arrives.
				e.emit(events.Warning, map[string]any{"chatId": in.Chat.ID, "warning": err.Error()})
			}
		}

		extraSystem := ""
		imageToolOffered := e.Tools != nil && e.Tools.Images != nil
		// In `model` mode the chat model writes the caption itself, so the contract has
		// to be in force where it decides the argument. In `auto` mode it must not be: the
		// tool asks the same model for a caption afterwards, and instructing it to expand
		// its own prompt would expand it twice.
		captionRule := ""
		if imageToolOffered {
			// Resolved through the pipeline: an empty setting means "the model decides", and
			// a FLUX 3 profile says the model is drawn from captions. An explicit "off" is
			// the user overriding that, so it never becomes a rule.
			imageSettings := in.Settings.Images
			pipeline := images.PipelineFor(imageSettings.Pipeline, imageSettings.CLIDiffusionModel)
			if pipeline.CaptioningMode(imageSettings.Captioning) == images.CaptioningModel {
				captionRule = tools.ImageCaptionHint
			}
		}
		if !in.UseNative {
			extraSystem = tools.TextProtocolHint
			// Only the tools this request actually declares. The definitions are what the
			// engine filtered for this turn — no search provider, no index, no picture
			// server each remove a tool — and a hint that names one of those is a call
			// that can only come back as an error.
			if list := tools.TextProtocolTools(e.toolDefinitions(ctx, &in.Chat, in.Model, in.Settings)); list != "" {
				extraSystem += "\n\n" + list
			}
			// The image tool is only listed when a picture server is configured, so
			// the hint that describes it travels with it rather than always being
			// part of the text protocol.
			if imageToolOffered {
				extraSystem += "\n\n" + tools.ImageProtocolHint
				if captionRule != "" {
					extraSystem += "\n\n" + captionRule
				}
			}
		}
		// A native-tool model reads the tool description once and then chooses; a
		// local one that was asked to "zeichne" something picks the tool whose name
		// sounds like drawing even when a diagram was meant. One rule beside the
		// prompt's own tool rules is what kept that from happening in the measured
		// case (see tools.ImageUseHint), and it travels only while the tool does.
		if in.UseNative && imageToolOffered {
			if extraSystem != "" {
				extraSystem += "\n\n"
			}
			extraSystem += tools.ImageUseHint
			if captionRule != "" {
				extraSystem += "\n\n" + captionRule
			}
		}
		// Last, so the protocol hints above cannot overwrite them: these two are the only pieces
		// of extra system text about the conversation rather than about this request. Only the
		// round that opens the turn carries them — a later round in the same turn already has
		// them in the history of the request that preceded it. The revert goes first, because a
		// file that was both put back and edited since is the revert's to explain.
		if iteration == 0 && (revertNote != "" || diskNote != "") {
			if extraSystem != "" {
				extraSystem += "\n\n"
			}
			if revertNote != "" {
				extraSystem += revertNote
			}
			if revertNote != "" && diskNote != "" {
				extraSystem += "\n\n"
			}
			if diskNote != "" {
				extraSystem += diskNote
			}
		}
		assembly, toolDefs, err := e.buildAssembly(ctx, &in.Chat,
			in.BackendID, in.ModelID, in.Settings, in.Model, extraSystem)
		if err != nil {
			return result, err
		}
		e.emit(events.Budget, map[string]any{"chatId": in.Chat.ID, "budget": assembly.Budget})

		row, err := e.createAssistantMessage(in.Chat.ID, in.BackendID, in.ModelID)
		if err != nil {
			return result, err
		}
		if result.PrimaryMessageID == "" {
			result.PrimaryMessageID = row.ID
		}
		e.emit(events.Start, map[string]any{
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

		// The request carries a watcher, so a retry that has to wait out a backend's
		// own rate limit shows up as a phase instead of a turn that looks hung.
		// One line per round before it goes out, so a request that never returns is
		// still visible — the failure this trace exists for is usually the one that
		// leaves the window waiting.
		e.Trace.Record(trace.Event{
			Event: trace.EvLLMRequest, Turn: in.Turn, Chat: in.Chat.ID, Message: row.ID,
			Backend: in.BackendID, Model: in.ModelID, Count: iteration,
		})
		requestStart := time.Now()
		events, err := in.Provider.Chat(e.watchingRetries(ctx, in.Chat.ID), req)
		requestMS := time.Since(requestStart).Milliseconds()
		if err == nil && iteration == 0 {
			// The model has the request in hand, which is as good as the note being read. A
			// request that failed before this point leaves the markers alone, so the retry —
			// or the next turn — carries the correction instead of losing it.
			revertDelivered = revertNote != ""
			diskDelivered = diskNote != ""
		}
		if err != nil {
			e.traceReply(in, row.ID, requestMS, nil, err)
			// The request never produced a byte, so there is no partial answer to
			// keep: the row is a failure. It used to be stored as "cancelled",
			// which is the status of a tool call the *user* abandoned — a backend
			// that refused the request (a connection error, a 400, a missing key)
			// therefore read back as though the user had interrupted it.
			row.Status = store.StatusFailed
			row.StopReason = "request_error"
			row.Content = ""
			e.persist(in.Chat.ID, "Die Antwort", e.Store.UpdateMessage(row))
			result.Status = row.Status
			return result, err
		}

		text, reasoning, calls, stats, usage, streamErr := e.consume(ctx, in.Chat.ID, row.ID, events)
		e.traceReply(in, row.ID, time.Since(requestStart).Milliseconds(), usage, streamErr)

		// A model that cannot call tools natively may have emitted a tool call in
		// text form; parse it out and hide the protocol from the user.
		if !in.UseNative && len(calls) == 0 {
			if parsed, visible := tools.ParseTextToolCalls(text); len(parsed) > 0 {
				calls = parsed
				text = visible
			}
		}

		// Whatever the model named has to be a tool somebody can execute, before it is
		// written into the row every later turn replays. A mangled name is repaired
		// where the intent is unambiguous — `scr scrape_url` is the shape that reached
		// this app once — because the alternative is a chat that records a call it will
		// answer with `unbekanntes Tool` for the rest of its life.
		//
		// A native call no tool answers is dropped; a text-protocol call is kept and
		// answered with the executor's error, because no vendor validates a name the
		// model wrote in prose and the model can correct itself on the next round. The
		// reasoning is in toolhistory.go. A nil name list means there is no tool service
		// at all: no declared set to check against, so the executor's own error stands.
		if known := engineKnownTools(e, &in.Chat); len(calls) > 0 && known != nil {
			var notes, dropped []string
			calls, notes, dropped = resolveToolCalls(calls, known, in.UseNative)
			for _, note := range notes {
				e.warn(in.Chat.ID, note)
			}
			// Nothing left to execute: say it in the transcript rather than ending the
			// turn on an empty answer nobody can explain.
			if len(calls) == 0 && len(dropped) > 0 && strings.TrimSpace(text) == "" {
				text = fmt.Sprintf(droppedToolText, strings.Join(dropped, ", "))
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
		switch {
		case ctx.Err() != nil:
			row.Status = store.StatusInterrupted
			row.StopReason = "interrupted"
		case streamErr != nil:
			// The connection died mid-answer. The partial text is kept, but calling it
			// complete would leave a reply that simply stops with nothing to explain
			// it: neither the row nor the next request may claim it is finished.
			row.Status = store.StatusFailed
			row.StopReason = "stream_error"
		}
		// Persist the reply together with its tool calls before executing them, so
		// the transcript is replayable even if the process dies mid-tool.
		row.ToolCallsJSON = encodeToolCalls(calls)
		e.persist(in.Chat.ID, "Die Antwort", e.Store.UpdateMessage(row))
		result.Status = row.Status

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

		// A tool result is read by the *next* request, so it is budgeted against the room
		// that request has left rather than only against the user's figure for an answer.
		// On a backend whose per-request ceiling is a fraction of its window those are
		// different numbers, and the difference is a refused turn: Groq's allowance is a
		// few thousand tokens while the tool budget defaults to 25000, so one scraped page
		// was enough to make the round that reads it unsendable — and no fold can take it
		// back, because the result belongs to the turn being answered.
		//
		// The prompt the backend just described is its own count when it reports one, and
		// that is worth more than the assembly's estimate here: an estimate that runs low
		// leaves too much room, and the room is the whole point.
		promptTokens := 0
		if usage != nil {
			promptTokens = usage.PromptTokens
		}
		if room, bounded := roomForToolResults(assembly.Budget, promptTokens, text); bounded {
			toolBudget.capTo(room)
		}

		e.emitStatus(in.Chat.ID, "tool")
		if err := e.runRound(ctx, in.Chat.ID, row, calls, toolBudget, tools.TurnRef{
			BackendID: in.BackendID,
			ModelID:   in.ModelID,
			Turn:      in.Turn,
			Sources:   in.Sources,
		}); err != nil {
			return result, err
		}
		e.emitStatus(in.Chat.ID, "idle")
	}

	// Falling out of the loop means the iteration budget ran out while the model
	// was still calling tools, so the turn ends on tool results with no answer.
	// Saying so is better than a turn that appears to have produced nothing.
	extra := ""
	if result.Text != "" {
		extra = " Der bisherige Text steht oben, ist aber möglicherweise nicht abgeschlossen."
	}
	e.emit(events.Warning, map[string]any{
		"chatId": in.Chat.ID,
		"warning": fmt.Sprintf("Die maximale Zahl an Tool-Runden (%d) ist erreicht — es wurde keine "+
			"abschließende Antwort erzeugt.%s Sende eine neue Nachricht, damit das Modell die "+
			"Ergebnisse auswertet.", maxIter, extra),
	})
	return result, nil
}

// traceReply records one provider round: how long it took, what it cost and, when it
// failed, only the classified kind of the failure.
//
// A reply that died mid-stream is reported like any other failure — that is what a
// "the answer just stops" report is — while the text that did arrive is not touched.
func (e *Engine) traceReply(in generateInput, messageID string, ms int64, usage *llm.Usage, err error) {
	ok := err == nil
	ev := trace.Event{
		Event: trace.EvLLMResponse, Turn: in.Turn, Chat: in.Chat.ID, Message: messageID,
		Backend: in.BackendID, Model: in.ModelID, MS: ms, OK: &ok,
	}
	if usage != nil {
		ev.Tokens = usage.PromptTokens
		ev.Out = usage.CompletionTokens
	}
	if err != nil {
		ev.Level = trace.LevelError
		ev.Kind = trace.KindOf(err)
		ev.Status = trace.StatusOf(err)
	}
	e.Trace.Record(ev)
}

// runTool executes one tool call and records both the call and its result.
//
// The full result is stored for the UI, but only what fits the answer's tool
// budget is written into the tool message the model reads. The stored message is
// what is replayed on every later turn, so bounding it here is what actually
// keeps a large scrape out of the window.
// toolOutcome is one call's execution, before anything has been written about it.
//
// The split exists for concurrency: a round of six reads should cost one wait rather than six,
// and that means the executions can overlap while the records — and the cards — are still
// written in the order the model asked for them. Everything the window sees therefore happens
// in startTool and finishTool, on the round's own goroutine; executeCall touches nothing but
// the tool service.
type toolOutcome struct {
	result tools.Result
	err    error
	ms     int64
	// ran is "the call was executed". Cancellation can leave a call unstarted, and such a
	// call is recorded as cancelled rather than as a result nobody can explain.
	ran bool
}

// readParallelism bounds how many read-only calls run at once. It is a small number on
// purpose: these are file reads on one disk, and the win comes from overlapping the latency,
// not from a hundred goroutines contending for the same directory.
const readParallelism = 4

// startTool writes the call's pending record and tells the window the call exists. The record
// it returns is what finishTool updates, so the row keeps the time it was created at.
//
// A card that appears only when the result does is a card that arrives without warning in the
// middle of a long tool round; the pending row is what puts it on screen with "running" on it.
func (e *Engine) startTool(chatID string, row store.Message, call llm.ToolCall) store.ToolCallRecord {
	record := store.ToolCallRecord{
		ID:        call.ID,
		MessageID: row.ID,
		ChatID:    chatID,
		Name:      call.Name,
		ArgsJSON:  call.Arguments,
		Status:    store.ToolPending,
		CreatedAt: time.Now(),
	}
	e.persist(chatID, "Der Tool-Aufruf", e.Store.UpsertToolCall(record))
	e.emit(events.Tool, map[string]any{"chatId": chatID, "toolCall": record})
	return record
}

// executeCall runs one call and times it, and writes nothing.
func (e *Engine) executeCall(ctx context.Context, chatID string, row store.Message, call llm.ToolCall, turn tools.TurnRef) toolOutcome {
	start := time.Now()
	result, err := e.executeTool(ctx, chatID, row.ID, call, turn)
	return toolOutcome{result: result, err: err, ms: time.Since(start).Milliseconds(), ran: true}
}

// finishTool records what a call produced: the status, the two sizes, the text the model
// reads, and the trace line. It is the half that must stay in call order.
func (e *Engine) finishTool(ctx context.Context, chatID string, row store.Message, call llm.ToolCall, record store.ToolCallRecord, out toolOutcome, budget *toolOutputBudget, turn tools.TurnRef) {
	execErr := out.err
	output := out.result.Text
	record.DurationMs = out.ms

	// The tool's name, how long it took and how much it returned — the three things
	// that explain a slow or empty round. The result itself stays out of the trace.
	ok := execErr == nil
	toolEvent := trace.Event{
		Event: trace.EvToolCall, Turn: turn.Turn, Chat: chatID, Message: row.ID,
		Tool: call.Name, MS: record.DurationMs, Bytes: int64(len(output)), OK: &ok,
	}
	if execErr != nil {
		toolEvent.Level = trace.LevelError
		toolEvent.Kind = trace.KindOf(execErr)
	}
	e.Trace.Record(toolEvent)

	// A tool that produced a picture filed it as an attachment of the message
	// that asked for it, and the frontend renders a message's attachments — so it
	// has to be told they changed. Emitting only on success is deliberate: a call
	// that failed wrote nothing, and a refresh that finds nothing new is a repaint
	// for no reason.
	if execErr == nil && call.Name == tools.ToolGenerateImage {
		e.emit(events.Attachments, map[string]any{"chatId": chatID})
	}

	summary := out.result.Summary
	switch {
	case execErr == nil:
		record.Status = store.ToolOK
	case errors.Is(execErr, tools.ErrDenied):
		// The user said no. The call is recorded as a decision rather than a fault, and
		// the sentence the model reads is written to stop it asking again — a declined
		// change that is retried every round is a loop the user has to escape by
		// stopping the turn, which is a worse outcome than the change not being made.
		record.Status = store.ToolDenied
		summary = nil
		output = fmt.Sprintf("Nicht ausgeführt: %s Die Änderung wurde nicht vorgenommen; frage nicht "+
			"erneut danach. Wenn sie nötig ist, beschreibe, was du ändern wolltest und warum, und "+
			"überlasse dem Nutzer die Entscheidung.", uimsg.Sentence(execErr))
	default:
		record.Status = store.ToolError
		// The card translates the code; the model gets the sentence the tool wrote for
		// it, because a payload is not something it can correct a call from. This is the
		// one place a message has two audiences, and uimsg carries both.
		record.Error = execErr.Error()
		summary = nil
		output = fmt.Sprintf("Tool error: %s", uimsg.Sentence(execErr))
	}
	record.ResultJSON = resultJSON(output, summary)
	e.persist(chatID, "Der Tool-Aufruf", e.Store.UpsertToolCall(record))
	e.emit(events.Tool, map[string]any{"chatId": chatID, "toolCall": record})

	// The condensation happens after the record: the tool card keeps what the tool
	// actually returned, while the message the model reads — and the one every later
	// turn replays — holds what it was given instead. Storing the summary as the
	// result would hide the page from the user; storing the page in the message is
	// the cost this exists to avoid.
	//
	// It is skipped when the budget is already spent: take would replace the summary
	// with the exhausted notice, so the request would be paid for and thrown away.
	condensed := output
	condensedByModel := false
	if budget.remaining > 0 {
		condensed, condensedByModel = e.condenseToolOutput(ctx, chatID, call, output, turn)
	}
	delivered, withheld := budget.take(condensed)
	// What the result cost and what of it the model actually reads is what makes the
	// condensation visible: without it the feature is indistinguishable from a page
	// that happened to be short. Both numbers are measured on the strings that travel
	// — the tool's own output and the exact text `take` returns — so a result cut by
	// the answer's budget, by the second model, or by neither is reported as it is.
	//
	// One outcome has no size to report: a budget with nothing left answers with the
	// notice rather than with a prefix of the result, and a card that printed both sizes
	// anyway would say the model read 41 tokens of a search it never saw. `withheld`
	// records that, so the card can say so.
	//
	// The card is re-emitted rather than only stored: it was drawn before the second
	// model ran, and a count that appeared only after reopening the chat would be a
	// number the reader had to go looking for.
	if execErr == nil {
		record.SourceTokens = llm.EstimateTokens(output)
		record.ContextTokens = llm.EstimateTokens(delivered)
		record.Condensed = condensedByModel
		record.Withheld = withheld
		e.persist(chatID, "Der Tool-Aufruf", e.Store.UpsertToolCall(record))
		e.emit(events.Tool, map[string]any{"chatId": chatID, "toolCall": record})
	}
	e.appendToolResult(chatID, call, delivered, record.Status)
}

// runRound executes every call the model asked for in one round.
//
// The pending cards are written first, in the model's order, and then the calls run: the
// read-only ones together, because they touch nothing and each costs a disk wait, and then —
// after the last of them — the ones that need an answer, one after another, because each of
// those is a person reading a diff and two questions at once is a race for the same attention.
// Finishing is back in call order, so the transcript reads the way the model wrote it however
// the work overlapped.
func (e *Engine) runRound(ctx context.Context, chatID string, row store.Message, calls []llm.ToolCall, budget *toolOutputBudget, turn tools.TurnRef) error {
	records := make([]store.ToolCallRecord, len(calls))
	for i, call := range calls {
		records[i] = e.startTool(chatID, row, call)
	}

	outcomes := make([]toolOutcome, len(calls))
	var readers sync.WaitGroup
	slots := make(chan struct{}, readParallelism)
	for i, call := range calls {
		if tools.NeedsApproval(call.Name) {
			continue
		}
		readers.Add(1)
		slots <- struct{}{}
		go func(index int, c llm.ToolCall) {
			defer readers.Done()
			defer func() { <-slots }()
			outcomes[index] = e.executeCall(ctx, chatID, row, c, turn)
		}(i, call)
	}
	// The reads are finished before the gated calls start, and that is a correctness rule rather
	// than a tidiness one: every guarded tool invalidates what the chat remembers about files —
	// a command forgets all of it, `gofmt -w .` being why — so a read still running beside one
	// could remember a stamp for a file the command had just rewritten, and the next read of it
	// in this turn would answer "unchanged". It is also what the model meant: a round that runs
	// `gofmt -w .` and then reads a file expects the formatted text, not a race.
	readers.Wait()
	cancelled := false
	for i, call := range calls {
		if !tools.NeedsApproval(call.Name) {
			continue
		}
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		outcomes[i] = e.executeCall(ctx, chatID, row, call, turn)
	}

	// Finishing happens in call order whatever finished first, and a call that never ran is
	// written as cancelled: the assistant message already claims it, and a backend rejects a
	// history with a tool call that has no result at all.
	for i := range calls {
		if outcomes[i].ran {
			e.finishTool(ctx, chatID, row, calls[i], records[i], outcomes[i], budget, turn)
			continue
		}
		e.cancelRemainingCalls(chatID, row, calls[i:i+1], 0)
	}
	if cancelled {
		return ctx.Err()
	}
	return nil
}

// runTool runs one call end to end. It is runRound for a single call, and it stays because a
// caller with one call in hand should not have to build a slice to say so.
func (e *Engine) runTool(ctx context.Context, chatID string, row store.Message, call llm.ToolCall, budget *toolOutputBudget, turn tools.TurnRef) {
	record := e.startTool(chatID, row, call)
	e.finishTool(ctx, chatID, row, call, record, e.executeCall(ctx, chatID, row, call, turn), budget, turn)
}

// toolOutputBudget bounds how many tokens of tool results one answer carries into
// its next request. The setting is per answer rather than per call, so several
// tools cannot together exceed it. The budget is shared across the tool rounds
// of a single generate call.
type toolOutputBudget struct{ remaining int }

func newToolOutputBudget(settings config.Settings) *toolOutputBudget {
	remaining := settings.ToolTokenBudget
	if remaining <= 0 {
		remaining = 25000
	}
	return &toolOutputBudget{remaining: remaining}
}

// capTo tightens the budget to the room one request has left for tool results.
//
// The user's figure bounds an answer; it says nothing about whether the request
// that reads the answer will fit the backend. Where those differ, the backend wins,
// because the cost of ignoring it is not a shorter answer but a refused turn: on
// Groq the per-minute token allowance is also the largest single request the
// account may send, and it is a few thousand tokens against a setting that defaults
// to 25000. A result is not something a history fold can take back either — it
// belongs to the turn being answered, which the fold keeps verbatim — so the cut has
// to happen where the result is handed to the model.
//
// It only ever tightens, which is what keeps a backend that had nothing to cap against
// exactly as it was: a window larger than the spare tool budget leaves `remaining`
// where the setting put it, and a model whose limit nobody reported never reaches this
// call at all. A window *smaller* than the budget does cap now — that request was
// already unsendable, and the fold that answers the refusal cannot touch the result.
// A later round with more room cannot undo a cut an earlier one needed, because the
// rounds of one answer build one history between them.
func (b *toolOutputBudget) capTo(room int) {
	if room < 0 {
		room = 0
	}
	if room < b.remaining {
		b.remaining = room
	}
}

// roomForToolResults is how much of one request this round's tool results may fill,
// and whether there is a limit to answer at all.
//
// It is the request's own arithmetic rather than a second estimate: the assembly in
// hand priced everything the next request will carry except the reply this round just
// wrote and the results it asks for, so subtracting the first sizes the second. The
// reserve keeps room for the answer the next round has to produce.
//
// promptTokens is the backend's own count of the request that just went out, and it is
// used in place of the assembly's figure whenever it is reported. The assembly's number
// is a heuristic, and the error points the dangerous way here: an estimate that runs low
// leaves room that is not there, so the result is cut too late and the request is
// refused for its size — the failure this budget exists to prevent. Zero means the
// backend stated nothing, which is the case of a local server that reports no usage.
//
// bounded is false when no limit is known — a model whose window nobody reported,
// which is also the only case in which the tool budget is left alone. There is nothing
// to aim at then, and a bare 0 would mean "no room" rather than "nobody said", which
// is the difference between a withheld result and an untouched budget.
func roomForToolResults(budget ContextBudget, promptTokens int, replyText string) (room int, bounded bool) {
	if budget.EffectiveLimit <= 0 {
		return 0, false
	}
	base := budget.Projected
	if promptTokens > 0 {
		base = promptTokens + budget.Reserve
	}
	room = budget.EffectiveLimit - base - llm.EstimateTokens(replyText)
	if room < 0 {
		room = 0
	}
	return room, true
}

// exhaustedNotice is what the model sees once the budget is spent, so it learns
// that a result was withheld instead of reading an empty answer as a result.
const exhaustedNotice = "Das Token-Budget für Tool-Ergebnisse in dieser Antwort ist aufgebraucht. " +
	"Weitere Tool-Ergebnisse werden nicht mehr in den Kontext übernommen."

// take returns the part of a tool result the model may see and charges it
// against the budget. A result that does not fit is cut with the shared
// truncation marker, so the model can tell a short result from a shortened one.
//
// The second return value says the model got none of it and the notice instead. It is
// reported rather than left to be recognised from the text, because it is the one
// outcome a tool card must not render as a size: "4,374 → 41 tokens" is what a spent
// budget looked like on the card, and it reads as a summary the model wrote when in
// fact the search results never arrived.
func (b *toolOutputBudget) take(output string) (string, bool) {
	if b.remaining <= 0 {
		return exhaustedNotice, true
	}
	if tokens := llm.EstimateTokens(output); tokens <= b.remaining {
		b.remaining -= tokens
		return output, false
	}
	cut := cutToTokens(output, b.remaining)
	b.remaining = 0
	if cut == "" {
		return exhaustedNotice, true
	}
	return cut, false
}

// executeTool runs one tool call. The requesting message id travels with the
// call in the context, because that is what lets a tool which creates an
// attachment — generate_image is the only one — link it to the turn that asked
// for it. A field on the shared Service would be wrong: two chats can run turns
// at once, and the tool service is one value for the whole app.
func (e *Engine) executeTool(ctx context.Context, chatID, messageID string, call llm.ToolCall, turn tools.TurnRef) (tools.Result, error) {
	if e.Tools == nil {
		return tools.Result{}, uimsg.Detailf("chat.noTools", "no tools are available")
	}
	// The turn's backend and model travel with the call beside the message id, and so does
	// the call's own id: a tool that needs them must not read them from a field two
	// concurrent chats would share. The confirmation is the second reader — its answer comes
	// back naming the call whose buttons were pressed, which is how several waiting calls
	// stay told apart.
	ctx = tools.WithToolCall(tools.WithTurn(tools.WithCallingMessage(ctx, messageID), turn), call.ID)
	// A running command is the one tool whose result is minutes away and whose progress is
	// worth watching, so its output is reported as it arrives rather than only when it is
	// done. Told here, per call, rather than set on the service: it names the call the
	// window should draw the text on, and two chats can run commands at once.
	//
	// Only the terminal streams. A read answers once, and a reporter on every call would be
	// a channel nothing writes to.
	if call.Name == tools.ToolRunCommand {
		ctx = tools.WithCommandOutput(ctx, func(text string) {
			e.emit(events.ToolOutput, map[string]any{
				"chatId":     chatID,
				"toolCallId": call.ID,
				"text":       text,
			})
		})
	}
	return e.Tools.ExecuteWithSummary(ctx, chatID, call.Name, call.Arguments)
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
			ResultJSON: resultJSON(cancelledToolOutput, nil),
			Status:     store.ToolCancelled,
			CreatedAt:  time.Now(),
		}
		e.persist(chatID, "Der abgebrochene Tool-Aufruf", e.Store.UpsertToolCall(record))
		e.emit(events.Tool, map[string]any{"chatId": chatID, "toolCall": record})
		e.appendToolResult(chatID, call, cancelledToolOutput, store.ToolCancelled)
	}
}

const cancelledToolOutput = "Der Nutzer hat die Generierung abgebrochen. Dieses Tool wurde nicht ausgeführt."

// appendToolResult persists a tool result as a message so the next iteration of
// the loop sees it.
func (e *Engine) appendToolResult(chatID string, call llm.ToolCall, output, status string) {
	msg, err := e.Store.AppendMessage(store.Message{
		ID:         newID(),
		ChatID:     chatID,
		Role:       string(llm.RoleTool),
		Content:    output,
		Name:       call.Name,
		ToolCallID: call.ID,
		Status:     status,
		CreatedAt:  time.Now(),
	})
	if err != nil {
		// Not a cosmetic failure: the assistant message that asked for this tool is
		// already stored, so losing the result leaves a tool call without an answer
		// and every later request in this chat is rejected by the backend.
		e.persist(chatID, "Das Tool-Ergebnis", err)
		return
	}
	e.emit(events.Message, map[string]any{"chatId": chatID, "message": msg})
}

func resultJSON(output string, summary *tools.Summary) string {
	payload := map[string]any{"content": output}
	// The window's half travels beside the model's rather than replacing it: the card shows a
	// translated sentence and keeps the exact text, so a person can read the summary at a
	// glance and still check what the turn was handed.
	if summary != nil && summary.Code != "" {
		payload["summary"] = summary
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// --- streaming -------------------------------------------------------------

// consume reads the stream, emits batched deltas to the UI, and accumulates the
// reply. Batching keeps a fast local model at 60+ tokens/s from flooding the
// webview with one event per token.
// The channel parameter is named stream rather than events so it cannot shadow
// the events package that names the payloads emitted from here.
func (e *Engine) consume(ctx context.Context, chatID, messageID string,
	stream <-chan llm.StreamEvent) (text, reasoning string, calls []llm.ToolCall,
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
			e.emit(events.Delta, map[string]any{
				"chatId": chatID, "messageId": messageID, "text": textBuf.String(),
			})
			textBuf.Reset()
		}
		if reasoningBuf.Len() > 0 {
			e.emit(events.Reasoning, map[string]any{
				"chatId": chatID, "messageId": messageID, "text": reasoningBuf.String(),
			})
			reasoningBuf.Reset()
		}
		lastFlush = time.Now()
	}

	for ev := range stream {
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
