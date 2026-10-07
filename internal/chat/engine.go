package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"kichat/internal/config"
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
type Engine struct {
	Store    *store.Store
	Registry *llm.Registry
	Tools    *tools.Service
	Settings func() config.Settings
	Emit     func(name string, payload any)

	mu           sync.Mutex
	active       map[string]*turnHandle
	measurements map[string]Measurement
}

type turnHandle struct {
	cancel    context.CancelFunc
	messageID string
	chatID    string
}

// NewEngine builds an engine.
func NewEngine(st *store.Store, registry *llm.Registry, toolService *tools.Service,
	settings func() config.Settings, emit func(string, any)) *Engine {
	return &Engine{
		Store:        st,
		Registry:     registry,
		Tools:        toolService,
		Settings:     settings,
		Emit:         emit,
		active:       map[string]*turnHandle{},
		measurements: map[string]Measurement{},
	}
}

// IsRunning reports whether a chat currently has a generation in flight.
func (e *Engine) IsRunning(chatID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.active[chatID]
	return ok
}

// ActiveChats lists chats with a generation in flight.
func (e *Engine) ActiveChats() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.active))
	for id := range e.active {
		out = append(out, id)
	}
	return out
}

// Stop cancels a running turn. The partial reply is kept and marked
// interrupted, and any tool call left in flight is closed off so the next
// request is still valid.
func (e *Engine) Stop(chatID string) bool {
	e.mu.Lock()
	handle, ok := e.active[chatID]
	e.mu.Unlock()
	if !ok {
		return false
	}
	handle.cancel()
	e.emitStatus(chatID, "stopping")
	return true
}

// StopAll cancels every running turn.
func (e *Engine) StopAll() {
	e.mu.Lock()
	handles := make([]*turnHandle, 0, len(e.active))
	for _, h := range e.active {
		handles = append(handles, h)
	}
	e.mu.Unlock()
	for _, h := range handles {
		h.cancel()
	}
}

func (e *Engine) emitStatus(chatID, phase string) {
	if e.Emit == nil {
		return
	}
	e.Emit("chat:status", map[string]any{"chatId": chatID, "phase": phase})
}

func (e *Engine) emit(name string, payload any) {
	if e.Emit != nil {
		e.Emit(name, payload)
	}
}

// Measurement returns the last recorded measurement for a chat.
func (e *Engine) Measurement(chatID string) (Measurement, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.measurements[chatID]
	return m, ok
}

func (e *Engine) setMeasurement(chatID string, m Measurement) {
	e.mu.Lock()
	e.measurements[chatID] = m
	e.mu.Unlock()
}

// InvalidateMeasurement drops the recorded prompt size, which is required after
// a model switch: a different tokenizer makes the old number meaningless.
func (e *Engine) InvalidateMeasurement(chatID string) {
	e.mu.Lock()
	delete(e.measurements, chatID)
	e.mu.Unlock()
}

// Send starts a turn. It returns immediately; progress arrives as events.
func (e *Engine) Send(req TurnRequest) error {
	e.mu.Lock()
	if _, busy := e.active[req.ChatID]; busy {
		e.mu.Unlock()
		return errors.New("für diesen Chat läuft bereits eine Generierung")
	}
	ctx, cancel := context.WithCancel(context.Background())
	handle := &turnHandle{cancel: cancel, chatID: req.ChatID}
	e.active[req.ChatID] = handle
	e.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			e.mu.Lock()
			delete(e.active, req.ChatID)
			e.mu.Unlock()
			e.emitStatus(req.ChatID, "idle")
		}()
		e.runTurn(ctx, req, handle)
	}()
	return nil
}

// runTurn performs one full exchange, including any tool iterations.
func (e *Engine) runTurn(ctx context.Context, req TurnRequest, handle *turnHandle) {
	settings := e.Settings()
	chat, err := e.Store.GetChat(req.ChatID)
	if err != nil {
		e.emit("chat:error", map[string]any{"chatId": req.ChatID, "error": err.Error()})
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
		e.emit("chat:error", map[string]any{
			"chatId": req.ChatID,
			"error":  "Kein Modell ausgewählt. Bitte oben im Chat ein Modell wählen.",
		})
		return
	}
	provider, ok := e.Registry.Provider(backendID)
	if !ok {
		e.emit("chat:error", map[string]any{
			"chatId": req.ChatID,
			"error":  fmt.Sprintf("Backend %q ist nicht konfiguriert.", backendID),
		})
		return
	}

	modelInfo := e.resolveModel(ctx, backendID, modelID, settings)

	// A manual regenerate drops the previous assistant reply first.
	if req.Regenerate {
		if err := e.dropTrailingAssistant(req.ChatID); err != nil {
			e.emit("chat:error", map[string]any{"chatId": req.ChatID, "error": err.Error()})
			return
		}
	}

	// Persist the user message before generating, so a crash never loses it.
	if strings.TrimSpace(req.Text) != "" || len(req.AttachmentIDs) > 0 {
		if err := e.appendUserMessage(req, backendID, modelID); err != nil {
			e.emit("chat:error", map[string]any{"chatId": req.ChatID, "error": err.Error()})
			return
		}
	}

	// Optionally seed the turn with web results before the model sees it.
	if err := e.seedSearch(ctx, &chat, req.Text, settings); err != nil {
		e.emit("chat:warning", map[string]any{
			"chatId":  req.ChatID,
			"warning": fmt.Sprintf("Vorab-Recherche fehlgeschlagen: %s", err.Error()),
		})
	}

	if err := e.maybeAutoCompact(ctx, &chat, provider, modelID, settings); err != nil {
		// A failed compaction is reported but does not abort the turn: the user
		// still deserves an answer, and an over-long context will surface as a
		// backend error which is easier to understand.
		e.emit("chat:warning", map[string]any{"chatId": req.ChatID, "warning": err.Error()})
	}

	// Assistant rows are created by generate as each model reply begins, so a
	// tool round produces a faithful sequence of messages.
	useNativeTools := settings.NativeTools && modelInfo.Has(llm.CapTools)
	// If the model advertised nothing at all (Ollama with a missing modelfile
	// capability list, or a custom endpoint) fall back to the text protocol,
	// which works everywhere.
	if len(modelInfo.Capabilities) == 0 && settings.NativeTools {
		useNativeTools = true
	}

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
	handle.messageID = result.PrimaryMessageID

	status := store.StatusComplete
	if ctx.Err() != nil {
		status = store.StatusInterrupted
	}

	if genErr != nil && ctx.Err() == nil {
		e.emit("chat:error", map[string]any{
			"chatId":    req.ChatID,
			"messageId": result.PrimaryMessageID,
			"error":     genErr.Error(),
		})
	}

	e.emit("chat:done", map[string]any{
		"chatId":        req.ChatID,
		"messageId":     result.PrimaryMessageID,
		"lastMessageId": result.LastMessageID,
		"status":        status,
		"stats":         result.Stats,
		"usage":         result.Usage,
	})

	e.emitBudget(req.ChatID, &chat, provider, backendID, modelID, settings, modelInfo)
	if chat.Title == "" && settings.AutoTitle {
		e.autoTitle(ctx, &chat, provider, modelID, req.Text)
	}
}

func maxSeqOf(st *store.Store, chatID string) int {
	seq, err := st.NextSeq(chatID)
	if err != nil {
		return 0
	}
	return seq - 1
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
func (e *Engine) buildAssembly(ctx context.Context, chat *store.Chat, provider llm.Provider,
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
func (e *Engine) emitBudget(chatID string, chat *store.Chat, provider llm.Provider,
	backendID, modelID string, settings config.Settings, model llm.ModelInfo) {
	assembly, _, err := e.buildAssembly(context.Background(), chat, provider,
		backendID, modelID, settings, model, "")
	if err != nil {
		return
	}
	e.emit("chat:budget", map[string]any{
		"chatId": chatID,
		"budget": assembly.Budget,
	})
}

// appendUserMessage persists the user's turn and links any attachments.
func (e *Engine) appendUserMessage(req TurnRequest, backendID, modelID string) error {
	seq, err := e.Store.NextSeq(req.ChatID)
	if err != nil {
		return err
	}
	msg := store.Message{
		ID:        uuid.NewString(),
		ChatID:    req.ChatID,
		Seq:       seq,
		Role:      string(llm.RoleUser),
		Content:   req.Text,
		Status:    store.StatusComplete,
		CreatedAt: time.Now(),
		Backend:   backendID,
		Model:     modelID,
	}
	if err := e.Store.CreateMessage(msg); err != nil {
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
	e.emit("chat:message", map[string]any{"chatId": req.ChatID, "message": msg})
	return nil
}

// createAssistantMessage inserts the placeholder row for the reply.
func (e *Engine) createAssistantMessage(chatID, backendID, modelID string) (store.Message, error) {
	seq, err := e.Store.NextSeq(chatID)
	if err != nil {
		return store.Message{}, err
	}
	msg := store.Message{
		ID:        uuid.NewString(),
		ChatID:    chatID,
		Seq:       seq,
		Role:      string(llm.RoleAssistant),
		Status:    store.StatusStreaming,
		CreatedAt: time.Now(),
		Backend:   backendID,
		Model:     modelID,
	}
	if err := e.Store.CreateMessage(msg); err != nil {
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
