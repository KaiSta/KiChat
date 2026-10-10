package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/tools"
)

// Measurements remember what a backend said the prompt cost, so the context
// meter can report a real number instead of an estimate.
type Measurement struct {
	Backend      string
	Model        string
	PromptTokens int
	Seq          int
	At           time.Time
}

// TurnRequest asks the engine to produce one assistant reply.
type TurnRequest struct {
	ChatID string
	// Text is the user's message. Empty means "regenerate".
	Text string
	// AttachmentIDs are files to attach to this message.
	AttachmentIDs []string
	// Backend/Model override the chat default for this turn only, which is what
	// makes "try this prompt on another model" safe.
	Backend string
	Model   string
	// Regenerate drops the trailing assistant reply before running.
	Regenerate bool
}

// Engine drives conversations.
//
// The state that outlives one Engine value — which chats have a turn in flight,
// and what the backend last reported a prompt to cost — lives in a TurnState
// instead of in this struct, because the app rebuilds its Engine when settings are
// saved or a managed server comes up and a reply in flight has to survive that.
type Engine struct {
	Store    *store.Store
	Registry *llm.Registry
	Tools    *tools.Service
	Settings func() config.Settings
	Emit     func(name string, payload any)

	turns *TurnState
}

// turnHandle is one claimed chat: how to cancel its turn, and which chat it is.
type turnHandle struct {
	cancel context.CancelFunc
	chatID string
}

// NewEngine builds an engine with a turn state of its own.
func NewEngine(st *store.Store, registry *llm.Registry, toolService *tools.Service,
	settings func() config.Settings, emit func(string, any)) *Engine {
	return NewEngineWithState(st, registry, toolService, settings, emit, NewTurnState())
}

// NewEngineWithState builds an engine that claims its turns in an existing state.
//
// This is what the app uses. Every engine it builds shares one TurnState, so a
// rebuild can replace the services without losing the turn that is running on the
// engine it replaces — Stop still finds it, and a second message still gets refused.
func NewEngineWithState(st *store.Store, registry *llm.Registry, toolService *tools.Service,
	settings func() config.Settings, emit func(string, any), turns *TurnState) *Engine {
	if turns == nil {
		turns = NewTurnState()
	}
	return &Engine{
		Store:    st,
		Registry: registry,
		Tools:    toolService,
		Settings: settings,
		Emit:     emit,
		turns:    turns,
	}
}

// IsRunning reports whether a chat currently has a generation in flight.
func (e *Engine) IsRunning(chatID string) bool {
	return e.turns.running(chatID)
}

// ActiveChats lists chats with a generation in flight.
func (e *Engine) ActiveChats() []string {
	return e.turns.chats()
}

// Stop cancels a running turn. The partial reply is kept and marked
// interrupted, and any tool call left in flight is closed off so the next
// request is still valid.
func (e *Engine) Stop(chatID string) bool {
	handle, ok := e.turns.handle(chatID)
	if !ok {
		return false
	}
	handle.cancel()
	e.emitStatus(chatID, "stopping")
	return true
}

// StopAll cancels every running turn.
func (e *Engine) StopAll() {
	for _, h := range e.turns.handles() {
		h.cancel()
	}
}

// StopAllAndWait cancels every running turn and waits up to d for them to unwind,
// reporting whether they all did. Shutdown uses it so a cancelled turn cannot go on
// writing to a store that is about to be closed.
func (e *Engine) StopAllAndWait(d time.Duration) bool {
	e.StopAll()
	return e.turns.waitIdle(d)
}

func (e *Engine) emitStatus(chatID, phase string) {
	if e.Emit == nil {
		return
	}
	e.Emit(events.Status, map[string]any{"chatId": chatID, "phase": phase})
}

func (e *Engine) emit(name string, payload any) {
	if e.Emit != nil {
		e.Emit(name, payload)
	}
}

// Measurement returns the last recorded measurement for a chat.
func (e *Engine) Measurement(chatID string) (Measurement, bool) {
	return e.turns.measurement(chatID)
}

func (e *Engine) setMeasurement(chatID string, m Measurement) {
	e.turns.setMeasurement(chatID, m)
}

// InvalidateMeasurement drops the recorded prompt size, which is required after
// a model switch: a different tokenizer makes the old number meaningless.
func (e *Engine) InvalidateMeasurement(chatID string) {
	e.turns.invalidateMeasurement(chatID)
}

// Send starts a turn. It returns immediately; progress arrives as events.
func (e *Engine) Send(req TurnRequest) error {
	ctx, cancel := context.WithCancel(context.Background())
	handle := &turnHandle{cancel: cancel, chatID: req.ChatID}
	// The claim is the only decision about whether the chat is free: checking and
	// reserving are one step, so a rebuild that replaced this engine cannot leave a
	// second turn able to start beside the first.
	if !e.turns.claim(req.ChatID, handle) {
		cancel()
		return errors.New("für diesen Chat läuft bereits eine Generierung")
	}

	go func() {
		defer func() {
			cancel()
			e.turns.release(req.ChatID, handle)
			e.emitStatus(req.ChatID, "idle")
		}()
		e.runTurn(ctx, req)
	}()
	return nil
}

// runTurn performs one full exchange, including any tool iterations.
func (e *Engine) runTurn(ctx context.Context, req TurnRequest) {
	settings := e.Settings()
	chat, err := e.Store.GetChat(req.ChatID)
	if err != nil {
		e.emit(events.Error, map[string]any{"chatId": req.ChatID, "error": err.Error()})
		return
	}

	backendID, modelID := chat.Backend, chat.Model
	if req.Backend != "" {
		backendID = req.Backend
	}
	if req.Model != "" {
		modelID = req.Model
	}
	if backendID == "" || modelID == "" {
		e.emit(events.Error, map[string]any{
			"chatId": req.ChatID,
			"error":  "Kein Modell ausgewählt. Bitte oben im Chat ein Modell wählen.",
		})
		return
	}
	provider, ok := e.Registry.Provider(backendID)
	if !ok {
		e.emit(events.Error, map[string]any{
			"chatId": req.ChatID,
			"error":  fmt.Sprintf("Backend %q ist nicht konfiguriert.", backendID),
		})
		return
	}

	modelInfo := e.resolveModel(ctx, backendID, modelID, settings)

	// A manual regenerate drops the previous assistant reply first.
	if req.Regenerate {
		if err := e.dropTrailingAssistant(req.ChatID); err != nil {
			e.emit(events.Error, map[string]any{"chatId": req.ChatID, "error": err.Error()})
			return
		}
	}

	// Persist the user message before generating, so a crash never loses it.
	if strings.TrimSpace(req.Text) != "" || len(req.AttachmentIDs) > 0 {
		if err := e.appendUserMessage(req, backendID, modelID); err != nil {
			e.emit(events.Error, map[string]any{"chatId": req.ChatID, "error": err.Error()})
			return
		}
	}

	// Optionally seed the turn with web results before the model sees it.
	if err := e.seedSearch(ctx, &chat, req.Text, settings, modelInfo.ContextLimit); err != nil {
		e.emit(events.Warning, map[string]any{
			"chatId":  req.ChatID,
			"warning": fmt.Sprintf("Vorab-Recherche fehlgeschlagen: %s", err.Error()),
		})
	}

	if err := e.maybeAutoCompact(ctx, &chat, provider, modelID, settings); err != nil {
		// A failed compaction is reported but does not abort the turn: the user
		// still deserves an answer, and an over-long context will surface as a
		// backend error which is easier to understand.
		e.emit(events.Warning, map[string]any{"chatId": req.ChatID, "warning": err.Error()})
	}

	// Assistant rows are created by generate as each model reply begins, so a
	// tool round produces a faithful sequence of messages.
	//
	// Native tool calling is used only when the model advertises it. A model that
	// advertises nothing at all (an Ollama modelfile without a capability list,
	// or a custom endpoint) falls back to the text protocol, which works
	// everywhere: sending `tools` a backend cannot honour degrades the reply
	// rather than enabling tools, and the fallback is what makes that safe.
	useNativeTools := settings.NativeTools && modelInfo.Has(llm.CapTools)

	result, genErr := e.generate(ctx, generateInput{
		Chat:        chat,
		Provider:    provider,
		BackendID:   backendID,
		ModelID:     modelID,
		Model:       modelInfo,
		Settings:    settings,
		UseNative:   useNativeTools,
		MaxToolIter: settings.ToolIterations,
	})

	// The reply's own status is the honest one to publish: generate already
	// distinguished an interrupted answer from one the backend stopped delivering
	// and from a request that never started, and the stored row carries whichever
	// it was. Reporting "complete" for those made the end-of-turn event disagree
	// with the row until something else refreshed the view from the database.
	status := result.Status
	if status == "" {
		if ctx.Err() != nil {
			status = store.StatusInterrupted
		} else {
			status = store.StatusComplete
		}
	}

	if genErr != nil && ctx.Err() == nil {
		e.emit(events.Error, map[string]any{
			"chatId":    req.ChatID,
			"messageId": result.PrimaryMessageID,
			"error":     genErr.Error(),
		})
	}

	e.emit(events.Done, map[string]any{
		"chatId":        req.ChatID,
		"messageId":     result.PrimaryMessageID,
		"lastMessageId": result.LastMessageID,
		"status":        status,
		"stats":         result.Stats,
		"usage":         result.Usage,
	})

	e.emitBudget(req.ChatID, &chat, backendID, modelID, settings, modelInfo)
	if chat.Title == "" && settings.AutoTitle {
		e.autoTitle(ctx, &chat, provider, modelID, req.Text)
	}
}

// resolveModel looks up model metadata, applying user overrides and falling back
// to the model list when a backend cannot answer directly.
func (e *Engine) resolveModel(ctx context.Context, backendID, modelID string, settings config.Settings) llm.ModelInfo {
	models := llm.MergeOverrides(e.Registry.ListModels(ctx, false), settings.ModelOverrides)
	for _, m := range models {
		if m.ID == modelID && m.Backend == backendID {
			return m
		}
	}
	// Unknown to the catalogue: fall back to an override or a conservative default.
	info := llm.ModelInfo{ID: modelID, Backend: backendID}
	if ov, ok := settings.ModelOverrides[backendID+"/"+modelID]; ok {
		info.ContextLimit = ov.ContextLimit
		info.Capabilities = ov.Capabilities
	}
	if info.ContextLimit == 0 {
		info.ContextLimit = 8192
	}
	return info
}

// limitFor decides the context window to plan against, and where the number
// came from. Reporting the source keeps the meter honest.
func limitFor(chat store.Chat, model llm.ModelInfo) (int, string) {
	if chat.NumCtx > 0 {
		return chat.NumCtx, "override"
	}
	if model.ContextLimit > 0 {
		return model.ContextLimit, "backend"
	}
	return 0, "unknown"
}

func capacitySuffix(limit int, source string) string {
	switch source {
	case "backend":
		return fmt.Sprintf("%d (Backend)", limit)
	case "override":
		return fmt.Sprintf("%d (manuell gesetzt)", limit)
	default:
		return "unbekannt"
	}
}

// buildAssembly prepares the request payload and its budget for a chat.
//
// It takes no provider, deliberately: the assembly is built from what the store
// holds, which is what lets a budget be previewed for a backend that is not
// configured at all — and it leaves nothing here that could dereference a nil one.
func (e *Engine) buildAssembly(ctx context.Context, chat *store.Chat,
	backendID, modelID string, settings config.Settings, model llm.ModelInfo,
	extraSystem string) (Assembly, []llm.ToolDefinition, error) {

	messages, err := e.Store.ListMessages(chat.ID)
	if err != nil {
		return Assembly{}, nil, err
	}
	compactions, err := e.Store.ListCompactions(chat.ID)
	if err != nil {
		return Assembly{}, nil, err
	}
	documents, err := e.Store.ListDocuments(chat.ID)
	if err != nil {
		return Assembly{}, nil, err
	}
	fullDocs := make([]store.Document, 0, len(documents))
	for _, d := range documents {
		full, err := e.Store.GetDocument(d.ID)
		if err != nil {
			continue
		}
		fullDocs = append(fullDocs, full)
	}

	toolDefs := e.toolDefinitions(ctx, chat, model, settings)

	limit, limitSource := limitFor(*chat, model)
	reserve := settings.Compact.ReserveForResponse
	if reserve <= 0 {
		reserve = 1024
	}

	working := *chat
	// The prompt template reports the model that actually answers this turn,
	// which may be a per-turn override rather than the chat's stored default.
	working.Backend = backendID
	working.Model = modelID
	if extraSystem != "" {
		working.SystemPrompt = working.SystemPrompt + "\n\n" + extraSystem
	}

	measurement, hasMeasurement := e.Measurement(chat.ID)
	measuredValid := hasMeasurement && measurement.Backend == backendID && measurement.Model == modelID

	in := AssemblyInput{
		Chat:          working,
		Messages:      messages,
		Compactions:   compactions,
		Documents:     fullDocs,
		Tools:         toolDefs,
		Location:      settings.Location,
		Limit:         limit,
		LimitSource:   limitSource,
		Reserve:       reserve,
		MeasuredValid: measuredValid,
	}
	if measuredValid {
		in.Measured = measurement.PromptTokens
		in.MeasuredSeq = measurement.Seq
	}
	return Assemble(in), toolDefs, nil
}

// toolDefinitions decides which tools to advertise for this model and chat.
func (e *Engine) toolDefinitions(ctx context.Context, chat *store.Chat, model llm.ModelInfo,
	settings config.Settings) []llm.ToolDefinition {
	if e.Tools == nil {
		return nil
	}
	all := e.Tools.Definitions()
	out := make([]llm.ToolDefinition, 0, len(all))
	for _, t := range all {
		switch t.Name {
		case tools.ToolSearchWeb:
			if e.Tools.Chain == nil || !anyProviderAvailable(e.Tools.Chain) {
				continue
			}
		case tools.ToolSearchDocs, tools.ToolReadPages:
			if e.Store == nil || !e.Store.FTS {
				continue
			}
		case tools.ToolScrapeURL:
			if e.Tools.Scraper == nil {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

func anyProviderAvailable(chain interface{ ProviderAvailability() map[string]bool }) bool {
	for _, ok := range chain.ProviderAvailability() {
		if ok {
			return true
		}
	}
	return false
}

// emitBudget publishes the current context budget so the meter can update.
func (e *Engine) emitBudget(chatID string, chat *store.Chat,
	backendID, modelID string, settings config.Settings, model llm.ModelInfo) {
	assembly, _, err := e.buildAssembly(context.Background(), chat,
		backendID, modelID, settings, model, "")
	if err != nil {
		return
	}
	e.emit(events.Budget, map[string]any{
		"chatId": chatID,
		"budget": assembly.Budget,
	})
}

// appendUserMessage persists the user's turn and links any attachments.
func (e *Engine) appendUserMessage(req TurnRequest, backendID, modelID string) error {
	msg, err := e.Store.AppendMessage(store.Message{
		ID:        uuid.NewString(),
		ChatID:    req.ChatID,
		Role:      string(llm.RoleUser),
		Content:   req.Text,
		Status:    store.StatusComplete,
		CreatedAt: time.Now(),
		Backend:   backendID,
		Model:     modelID,
	})
	if err != nil {
		return err
	}
	for _, id := range req.AttachmentIDs {
		attachment, err := e.Store.GetAttachment(id)
		if err != nil {
			continue
		}
		if attachment.ChatID != req.ChatID {
			continue
		}
		attachment.MessageID = msg.ID
		if err := e.Store.UpdateAttachment(attachment); err != nil {
			return err
		}
	}
	e.emit(events.Message, map[string]any{"chatId": req.ChatID, "message": msg})
	return nil
}

// createAssistantMessage inserts the placeholder row for the reply.
func (e *Engine) createAssistantMessage(chatID, backendID, modelID string) (store.Message, error) {
	msg, err := e.Store.AppendMessage(store.Message{
		ID:        uuid.NewString(),
		ChatID:    chatID,
		Role:      string(llm.RoleAssistant),
		Status:    store.StatusStreaming,
		CreatedAt: time.Now(),
		Backend:   backendID,
		Model:     modelID,
	})
	if err != nil {
		return store.Message{}, err
	}
	return msg, nil
}

// dropTrailingAssistant removes the last assistant reply so it can be
// regenerated.
func (e *Engine) dropTrailingAssistant(chatID string) error {
	messages, err := e.Store.ListMessages(chatID)
	if err != nil {
		return err
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == string(llm.RoleAssistant) {
			return e.Store.DeleteMessagesFromSeq(chatID, messages[i].Seq)
		}
		// Tool results belong to the assistant turn, so drop them too.
		if messages[i].Role != string(llm.RoleTool) {
			break
		}
	}
	return nil
}

func encodeToolCalls(calls []llm.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	raw, err := json.Marshal(calls)
	if err != nil {
		return ""
	}
	return string(raw)
}
