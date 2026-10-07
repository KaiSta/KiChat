package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"kichat/internal/config"
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

// Compact folds older history into a memory summary. It is the single code path
// behind both automatic and manual compaction; only Force and the range differ.
func (e *Engine) Compact(ctx context.Context, chatID string, opts CompactOptions) (store.Compaction, error) {
	settings := e.Settings()
	chat, err := e.Store.GetChat(chatID)
	if err != nil {
		return store.Compaction{}, err
	}
	backendID, modelID := chat.Backend, chat.Model
	if backendID == "" || modelID == "" {
		return store.Compaction{}, errors.New("kein Modell ausgewählt")
	}
	provider, ok := e.Registry.Provider(backendID)
	if !ok {
		return store.Compaction{}, fmt.Errorf("Backend %q ist nicht konfiguriert", backendID)
	}
	modelInfo := e.resolveModel(ctx, backendID, modelID, settings)

	if err := e.compactWith(ctx, &chat, provider, backendID, modelID, modelInfo, settings, opts); err != nil {
		return store.Compaction{}, err
	}
	compactions, err := e.Store.ListCompactions(chatID)
	if err != nil || len(compactions) == 0 {
		return store.Compaction{}, errors.New("keine Kompaktierung erstellt")
	}
	return compactions[len(compactions)-1], nil
}

// maybeAutoCompact compacts when the projected next request would cross the
// configured threshold. It runs before sending, never mid-stream.
func (e *Engine) maybeAutoCompact(ctx context.Context, chat *store.Chat, provider llm.Provider,
	modelID string, settings config.Settings) error {

	if !settings.Compact.Enabled || !chat.AutoCompact {
		return nil
	}
	modelInfo := e.resolveModel(ctx, chat.Backend, modelID, settings)
	return e.compactWith(ctx, chat, provider, chat.Backend, modelID, modelInfo, settings, CompactOptions{})
}

func (e *Engine) compactWith(ctx context.Context, chat *store.Chat, provider llm.Provider,
	backendID, modelID string, modelInfo llm.ModelInfo, settings config.Settings,
	opts CompactOptions) error {

	assembly, _, err := e.buildAssembly(ctx, chat, provider, backendID, modelID, settings, modelInfo, "")
	if err != nil {
		return err
	}
	if assembly.Budget.Limit <= 0 {
		return errors.New("Kontextfenster des Modells ist unbekannt")
	}

	messages, err := e.Store.ListMessages(chat.ID)
	if err != nil {
		return err
	}
	compactions, err := e.Store.ListCompactions(chat.ID)
	if err != nil {
		return err
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
		return nil
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
		return nil
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

	summary, err := Summarize(ctx, provider, modelID, SummarizeOptions{
		PreviousSummary: previous,
		Transcript:      RenderTranscript(toFold),
		Focus:           opts.Focus,
		Temperature:     settings.Compact.SummaryTemperature,
		MaxTokens:       settings.Compact.MaxSummaryTokens,
		NumCtx:          chat.NumCtx,
	})
	if err != nil {
		return err
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
		return err
	}

	// The prompt has changed shape, so the previous measurement no longer
	// describes it.
	e.InvalidateMeasurement(chat.ID)
	e.emit("chat:compaction", map[string]any{"chatId": chat.ID, "compaction": compaction})
	return nil
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
	e.emit("chat:title", map[string]any{"chatId": chat.ID, "title": title})
}
