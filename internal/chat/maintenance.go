package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/llm"
	"kichat/internal/store"
)

// newID returns a fresh identifier.
func newID() string { return uuid.NewString() }

// CompactOptions controls a compaction run.
type CompactOptions struct {
	// Force runs even when the threshold has not been reached (manual compaction).
	Force bool
	// Focus is an optional instruction about what to preserve.
	Focus string
	// Target is a desired usage ratio afterwards, 0 for "as much as possible".
	Target float64
	// FromSeq/ToSeq narrow the range to fold. Zero means "let the planner decide".
	FromSeq int
	ToSeq   int
}

// CompactResult reports what a compaction run actually did. A run that finds
// nothing to fold is a normal outcome rather than a failure — asking to compact a
// short conversation is not an error — so it is reported rather than raised, and
// the caller can say "nothing to compact" instead of showing a failure.
//
// Created also distinguishes a new compaction from an older one that merely still
// exists: without it a no-op run would hand back the previous compaction as though
// it had just produced it.
type CompactResult struct {
	Created    bool             `json:"created"`
	Compaction store.Compaction `json:"compaction"`
}

// Compact folds older history into a memory summary. It is the single code path
// behind both automatic and manual compaction; only Force and the range differ.
//
// A manual run is refused while a turn is in flight. The turn owns the newest
// messages — an assistant row is written to as tokens arrive — so compacting now
// would summarise half an answer and freeze that fragment into memory. The
// automatic run is triggered by a turn and is the caller that may do it.
func (e *Engine) Compact(ctx context.Context, chatID string, opts CompactOptions) (CompactResult, error) {
	if e.IsRunning(chatID) {
		return CompactResult{}, errors.New("für diesen Chat läuft noch eine Generierung — " +
			"kompaktieren ist möglich, sobald sie fertig ist")
	}
	settings := e.Settings()
	chat, err := e.Store.GetChat(chatID)
	if err != nil {
		return CompactResult{}, err
	}
	backendID, modelID := chat.Backend, chat.Model
	if backendID == "" || modelID == "" {
		return CompactResult{}, errors.New("kein Modell ausgewählt")
	}
	provider, ok := e.Registry.Provider(backendID)
	if !ok {
		return CompactResult{}, fmt.Errorf("Backend %q ist nicht konfiguriert", backendID)
	}
	modelInfo := e.resolveModel(ctx, backendID, modelID, settings)

	created, err := e.compactWith(ctx, &chat, provider, backendID, modelID, modelInfo, settings, opts)
	if err != nil {
		return CompactResult{}, err
	}
	if created == nil {
		return CompactResult{Created: false}, nil
	}
	return CompactResult{Created: true, Compaction: *created}, nil
}

// maybeAutoCompact compacts when the projected next request would cross the
// configured threshold. It runs before sending, never mid-stream.
func (e *Engine) maybeAutoCompact(ctx context.Context, chat *store.Chat, provider llm.Provider,
	modelID string, settings config.Settings) error {

	if !settings.Compact.Enabled || !chat.AutoCompact {
		return nil
	}
	modelInfo := e.resolveModel(ctx, chat.Backend, modelID, settings)
	// The created compaction is irrelevant here: an automatic run has nobody to
	// report back to, and "nothing to fold" is already the silent outcome.
	_, err := e.compactWith(ctx, chat, provider, chat.Backend, modelID, modelInfo, settings, CompactOptions{})
	return err
}

// compactWith performs one compaction. It returns the compaction it created, or
// nil when there was nothing to fold — a planner that decided no compaction was
// needed, or a range that turned out to hold no messages.
func (e *Engine) compactWith(ctx context.Context, chat *store.Chat, provider llm.Provider,
	backendID, modelID string, modelInfo llm.ModelInfo, settings config.Settings,
	opts CompactOptions) (*store.Compaction, error) {

	assembly, _, err := e.buildAssembly(ctx, chat, backendID, modelID, settings, modelInfo, "")
	if err != nil {
		return nil, err
	}
	if assembly.Budget.Limit <= 0 {
		return nil, errors.New("Kontextfenster des Modells ist unbekannt")
	}

	messages, err := e.Store.ListMessages(chat.ID)
	if err != nil {
		return nil, err
	}
	compactions, err := e.Store.ListCompactions(chat.ID)
	if err != nil {
		return nil, err
	}

	plan := PlanCompaction(PlanInput{
		Messages:         messages,
		Compactions:      compactions,
		Budget:           assembly.Budget,
		Threshold:        effectiveThreshold(*chat, settings),
		KeepRecentTurns:  effectiveKeepTurns(*chat, settings),
		Reserve:          settings.Compact.ReserveForResponse,
		MaxSummaryTokens: settings.Compact.MaxSummaryTokens,
		Force:            opts.Force,
		Target:           opts.Target,
	})
	if !plan.Needed {
		return nil, nil
	}
	// A manual call may pin the range explicitly.
	if opts.ToSeq > 0 && opts.ToSeq > plan.FromSeq {
		plan.ToSeq = opts.ToSeq
		plan.KeepFromSeq = opts.ToSeq + 1
	}
	if opts.FromSeq > 0 {
		plan.FromSeq = opts.FromSeq
	}

	var toFold []store.Message
	for _, m := range messages {
		if m.Seq >= plan.FromSeq && m.Seq <= plan.ToSeq {
			toFold = append(toFold, m)
		}
	}
	if len(toFold) == 0 {
		return nil, nil
	}

	e.emitStatus(chat.ID, "compacting")
	defer e.emitStatus(chat.ID, "idle")

	// Carry forward the summary in force, so compaction merges rather than
	// starting over.
	previous := ""
	for _, c := range compactions {
		if c.ToSeq < plan.FromSeq {
			previous = c.Summary
		}
	}
	if previous == "" && assembly.MemorySummary != "" {
		previous = assembly.MemorySummary
	}

	// The transcript's allowance comes from the window of the model that has to
	// read it, less the prompt that carries it and a reserve for the answer.
	// Measuring the prompt here, with the transcript left out, is what makes that
	// subtraction exact instead of a guess.
	overhead := llm.EstimateMessagesTokens([]llm.Message{
		{Role: llm.RoleSystem, Content: summarySystemPrompt},
		{Role: llm.RoleUser, Content: summarisePrompt(SummarizeOptions{
			PreviousSummary: previous,
			Focus:           opts.Focus,
		})},
	})

	// The reserve is the summariser's output cap, and that cap is deliberately
	// generous: `max_tokens` bounds a reasoning model's private thinking and its
	// answer together, so a cap sized to the summary alone is spent entirely on the
	// thinking and returns no summary at all. Cline caps its own summariser at 8192
	// for exactly this. A larger configured summary size raises it, and it is held
	// back only far enough to keep a minimal transcript and the request itself
	// inside the window — never to a fraction of the window, which would shrink the
	// cap on a small context for no reason. A slice is added on top as slack for the
	// estimator undershooting the backend's real tokenizer.
	outputTokens := summaryOutputTokens
	if configured := settings.Compact.MaxSummaryTokens; configured > outputTokens {
		outputTokens = configured
	}
	if room := assembly.Budget.Limit - overhead - summaryMinTranscriptTokens; outputTokens > room {
		outputTokens = room
	}
	if outputTokens < 1 {
		outputTokens = 1
	}
	margin := assembly.Budget.Limit / 16
	budget := PlanTranscriptBudget(assembly.Budget.Limit, outputTokens+margin, overhead)
	transcript := RenderTranscript(toFold, budget)

	// A cap, not a target: the model is told to be terse, so it stops far short of
	// it. The cap only stops a runaway generation from filling the window — the
	// failure that produced a 20k-token "memory" for a 13k-token fold.
	maxTokens := outputTokens

	// Summarising is not a reasoning task, so the model is asked to skip thinking.
	// A model that advertises thinking gets the backend's own control; a backend
	// whose model metadata advertises none but whose request still accepts the
	// toggle (LM Studio's `enable_thinking`) is asked the same way. Either way the
	// in-prompt soft switch goes out, and Summarize's retry covers a model that
	// thinks regardless.
	disableThinking := modelInfo.Has(llm.CapThink)
	if t, ok := provider.(llm.ThinkingToggle); ok && t.CanToggleThinking() {
		disableThinking = true
	}
	summary, err := Summarize(ctx, provider, modelID, SummarizeOptions{
		PreviousSummary: previous,
		Transcript:      transcript,
		Focus:           opts.Focus,
		Temperature:     settings.Compact.SummaryTemperature,
		MaxTokens:       maxTokens,
		NumCtx:          chat.NumCtx,
		DisableThinking: disableThinking,
		ContextLimit:    assembly.Budget.Limit,
	})
	if err != nil {
		return nil, err
	}

	compaction := store.Compaction{
		ID:           newID(),
		ChatID:       chat.ID,
		Trigger:      triggerName(opts.Force),
		FromSeq:      plan.FromSeq,
		ToSeq:        plan.ToSeq,
		Summary:      summary,
		Focus:        opts.Focus,
		TokensBefore: plan.TokensBefore,
		TokensAfter:  llm.EstimateTokens(summary),
		Model:        modelID,
		Backend:      backendID,
	}
	if err := e.Store.CreateCompaction(compaction); err != nil {
		return nil, err
	}

	// The prompt has changed shape, so the previous measurement no longer
	// describes it.
	e.InvalidateMeasurement(chat.ID)
	e.emit(events.Compaction, map[string]any{"chatId": chat.ID, "compaction": compaction})
	return &compaction, nil
}

func triggerName(force bool) string {
	if force {
		return "manual"
	}
	return "auto"
}

// effectiveThreshold prefers the per-chat value over the global one.
func effectiveThreshold(chat store.Chat, settings config.Settings) float64 {
	if chat.CompactThreshold > 0 && chat.CompactThreshold < 1 {
		return chat.CompactThreshold
	}
	if settings.Compact.Threshold > 0 {
		return settings.Compact.Threshold
	}
	return 0.80
}

func effectiveKeepTurns(chat store.Chat, settings config.Settings) int {
	if chat.KeepRecentTurns > 0 {
		return chat.KeepRecentTurns
	}
	if settings.Compact.KeepRecentTurns > 0 {
		return settings.Compact.KeepRecentTurns
	}
	return 6
}

// autoTitle names a chat from its first message using the same model, cheaply.
func (e *Engine) autoTitle(ctx context.Context, chat *store.Chat, provider llm.Provider,
	modelID, firstMessage string) {

	source := strings.TrimSpace(firstMessage)
	if source == "" {
		return
	}
	if len(source) > 600 {
		source = source[:600]
	}
	titleCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	text, _, err := Complete(titleCtx, provider, llm.ChatRequest{
		Model: modelID,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "Du erzeugst einen kurzen Titel für einen Chat. " +
				"Antworte mit 3 bis 6 Wörtern, ohne Anführungszeichen, ohne Satzzeichen am Ende, " +
				"in der Sprache der Nachricht."},
			{Role: llm.RoleUser, Content: source},
		},
		Temperature: 0.2,
		MaxTokens:   32,
	})
	if err != nil {
		return
	}
	title := strings.TrimSpace(text)
	title = strings.Trim(title, "\"'`*# ")
	title = strings.SplitN(title, "\n", 2)[0]
	if title == "" || len(title) > 80 {
		return
	}
	if err := e.Store.SetChatTitle(chat.ID, title); err != nil {
		return
	}
	chat.Title = title
	e.emit(events.Title, map[string]any{"chatId": chat.ID, "title": title})
}
