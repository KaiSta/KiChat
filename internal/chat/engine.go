package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"uuid"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/tools"
	"kichat/internal/trace"
	"kichat/internal/uimsg"
	"kichat/internal/workspace"
)

// Measurements remember what a backend said the prompt cost, so the context
// meter can report a real number instead of an estimate.
type Measurement struct {
	Backend             string
	Model               string
	PromptTokens        int
	Seq                 int
	ReplayToolReasoning bool
	At                  time.Time
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
	// NumCtx, including zero, replaces the chat context for this turn only.
	NumCtx *int
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
	// Trace records the shape of a turn — backend, model, timings, token counts and
	// the classified kind of a failure — and never its content. Nil is a no-op, which
	// is what a test engine gets.
	Trace *trace.Logger

	turns *TurnState

	// condensationOnce guards the lazily built ledger of pages this process has
	// already condensed per chat (see digest.go), which is what makes a second read
	// of the same page the escape hatch back to the full text.
	condensationOnce   sync.Once
	condensationLedger *condensationLedger
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
		return uimsg.Errorf("chat.turnRunning")
	}

	// What the user just wrote is stored here, before the turn's goroutine starts — not
	// inside it.
	//
	// It is the only thing the window has to draw about this click, and the turn's opening
	// is slow: it reads the chat, then asks every configured backend for its model list
	// (`resolveModel`, because the capabilities it reports decide whether native tool calling
	// is used). Written in there, the message stayed invisible for as long as that took while
	// the composer had already turned into Stop, and the window showed nothing at all about a
	// message the person had just sent.
	if err := e.beginUserTurn(req); err != nil {
		cancel()
		e.turns.release(req.ChatID, handle)
		return err
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

// maxToolRounds is how many model/tool rounds this chat's reply may take, and it is
// chosen by mode rather than shared.
//
// A project's reply is a piece of work that is supposed to keep going — read, edit, run the
// tests, read the failure, edit again — so it reads its own figure
// (`Settings.CodingToolIterations`, 60 by default). Every other chat ends when the model
// stops asking for tools, and its figure guards against a model that never does. One shared
// number could not be both: a chat setting of ten, which is somebody's idea of a question
// being answered, cut a refactor at ten rounds.
//
// A zero in the project's field falls back to the chat's number rather than to one round:
// it is what a `config.Settings` literal in a test carries, and a settings blob has had it
// repaired by `normalise` long before it gets here.
func maxToolRounds(chat store.Chat, settings config.Settings) int {
	if chat.IsProject() && settings.CodingToolIterations > 0 {
		return settings.CodingToolIterations
	}
	return settings.ToolIterations
}

// runTurn performs one full exchange, including any tool iterations.
func (e *Engine) runTurn(ctx context.Context, req TurnRequest) {
	started := time.Now()
	turnID := e.Trace.Turn()
	// The end event is built as the turn goes and written by the deferred call, so a
	// turn that fails before it reaches the model is still one line in the trace. That
	// is the case a bug report usually brings: "it says no model is configured", and
	// the trace says whether the chat's row or the picker was the empty side.
	end := trace.Event{Event: trace.EvTurnEnd, Turn: turnID, Chat: req.ChatID}
	fail := func(err error) {
		end.Level = trace.LevelError
		end.Phase = "failed"
		end.Kind = trace.KindOf(err)
		end.Status = trace.StatusOf(err)
	}
	defer func() {
		end.MS = time.Since(started).Milliseconds()
		e.Trace.Record(end)
	}()

	settings := e.Settings()
	chat, err := e.Store.GetChat(req.ChatID)
	if err != nil {
		fail(err)
		e.emit(events.Error, map[string]any{"chatId": req.ChatID, "error": err.Error()})
		return
	}

	if req.NumCtx != nil {
		chat.NumCtx = *req.NumCtx
	}
	backendID, modelID := turnModel(chat, req)
	if backendID == "" || modelID == "" {
		err := uimsg.Errorf("chat.noModelInChat")
		fail(err)
		e.emit(events.Error, map[string]any{
			"chatId": req.ChatID,
			"error":  err.Error(),
		})
		return
	}
	provider, ok := e.Registry.Provider(backendID)
	if !ok {
		fail(fmt.Errorf("backend %q is not configured", backendID))
		e.emit(events.Error, map[string]any{
			"chatId": req.ChatID,
			"error":  uimsg.Errorf("chat.backendNotConfigured", backendID).Error(),
		})
		return
	}
	end.Backend, end.Model = backendID, modelID
	e.Trace.Record(trace.Event{
		Event: trace.EvTurnStart, Turn: turnID, Chat: req.ChatID,
		Backend: backendID, Model: modelID,
	})
	modelInfo := e.resolveModel(ctx, backendID, modelID, settings)

	// A manual regenerate drops the previous assistant reply first.
	if req.Regenerate {
		if err := e.dropTrailingAssistant(req.ChatID); err != nil {
			fail(err)
			e.emit(events.Error, map[string]any{"chatId": req.ChatID, "error": err.Error()})
			return
		}
	}

	// The user's turn is already stored: Send writes it before this goroutine starts, so the
	// window can draw it while the model listing below is still running. A crash still cannot
	// lose it, and a turn that never gets this far was never announced to the chat.

	// One numbering for the whole turn. The pre-flight search and every tool call that
	// follows it number their results from this one list, so [7] in the answer is one
	// site rather than the seventh hit of each search; the list is written onto the
	// answer when the turn ends, which is what the transcript's source list renders.
	sources := tools.NewSourceList()

	// Optionally seed the turn with web results before the model sees it.
	if err := e.seedSearch(ctx, &chat, req.Text, settings, modelInfo.ContextLimit, sources); err != nil {
		e.emit(events.Warning, map[string]any{
			"chatId":  req.ChatID,
			"warning": uimsg.Errorf("chat.searchFirstFailed", err.Error()).Error(),
		})
	}

	// A fold before the request goes out is one decision with two bounds — the user's
	// threshold and a backend's published per-request ceiling — and the planner holds
	// both (PlanCompaction), so the turn asks once. While these were two calls the two
	// bounds could both fire in one turn: the threshold run folded to its keep-turns
	// boundary and the ceiling run then folded further for its target, two summaries for
	// one request.
	//
	// The ceiling is not the window, which is the case this exists for: Groq's
	// per-minute token allowance is also the largest single request the account may send,
	// so a chat planned against the 131k window its listing advertises is refused within
	// the first few thousand tokens. It is learned from one refusal and from the budget
	// header of any response (llm.RequestCeiling), and from then on the request is planned
	// against the number the meter shows rather than the window.
	if err := e.autoFold(ctx, &chat, provider, backendID, modelID, modelInfo, settings, false); err != nil {
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

	in := generateInput{
		Chat:        chat,
		Provider:    provider,
		BackendID:   backendID,
		ModelID:     modelID,
		Turn:        turnID,
		Model:       modelInfo,
		Settings:    settings,
		UseNative:   useNativeTools,
		MaxToolIter: maxToolRounds(chat, settings),
		Sources:     sources,
	}
	result, genErr := e.generate(ctx, in)

	// The answer carries the numbering it was written against. Only a reply with text gets
	// the list: a row that only asked for tools has no citations to resolve, and a list
	// under it would be a source panel for a message that says nothing.
	if result.LastMessageID != "" && result.Text != "" {
		e.persist(req.ChatID, uimsg.Errorf("chat.thingSources"),
			e.Store.SetMessageSources(result.LastMessageID, sources.All()))
	}

	// A model that named a tool the request never declared is refused by the vendor
	// before a token streams, and its sentence — "attempted to call tool 'scr scrape_url'
	// which was not in request.tools" — names a tool the app never sent. Quoted, it reads
	// as a bug in the app's tool list; explained, it says what happened and what helps.
	// The call itself never reaches the app, so there is nothing to repair here — only
	// something to say (see llm.MalformedToolCall).
	if llm.MalformedToolCall(genErr) {
		genErr = errMalformedToolCall
	}

	// A request the backend refused for its own size is a verdict rather than a
	// hiccup — the same body produces the same answer — so the history is folded and
	// the turn is sent once more. This is what keeps a long chat from becoming an
	// unusable one, and it is the only retry here: a second refusal is reported with
	// the limit the vendor named instead of its envelope (turnFailureText).
	outcome := foldNothing
	if genErr != nil && ctx.Err() == nil {
		if too, ok := llm.RequestTooLarge(genErr); ok {
			outcome = e.foldForSize(ctx, &chat, provider, backendID, modelID, modelInfo, settings, too)
			if outcome == foldFolded {
				result, genErr = e.generate(ctx, in)
			}
		}
	}

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

	// What the turn became, for the end event the defer writes. Only a failure is
	// classified: a turn the user stopped is an interruption, not an error.
	end.Phase = status
	end.Count = len(result.ToolCalls)
	if result.Usage != nil {
		end.Tokens = result.Usage.PromptTokens
		end.Out = result.Usage.CompletionTokens
	}
	if genErr != nil && ctx.Err() == nil {
		fail(genErr)
		e.emit(events.Error, map[string]any{
			"chatId":    req.ChatID,
			"messageId": result.PrimaryMessageID,
			"error":     turnFailureText(provider.Name(), genErr, outcome),
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
// to the model list when a backend cannot answer directly. It refreshes the
// catalogue, which makes it the right call for a turn — the capabilities it reports
// decide whether native tool calling is used.
func (e *Engine) resolveModel(ctx context.Context, backendID, modelID string, settings config.Settings) llm.ModelInfo {
	return resolveModelFrom(e.Registry.ListModels(ctx, false), backendID, modelID, settings)
}

// resolveModelCached resolves a model against the catalogue already in hand and
// only reaches the network if there is no catalogue at all.
//
// It is what a budget preview uses: opening a chat and switching a model in a
// picker are not worth a listing request, and asking for one unconditionally made
// them wait on every configured backend (20 seconds each, and one /api/show per
// Ollama model on top of that). The one exception is a process that has never
// listed anything — a chat opened before the first refresh — where a single
// tightly bounded listing is cheaper than reporting a window the app does not know.
func (e *Engine) resolveModelCached(backendID, modelID string, settings config.Settings) llm.ModelInfo {
	models := e.Registry.Snapshot()
	if len(models) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		models = e.Registry.ListModels(ctx, false)
	}
	return resolveModelFrom(models, backendID, modelID, settings)
}

// resolveModelFrom is the lookup itself, shared by the fresh and the cached path so
// the two cannot disagree about an override or a fallback.
func resolveModelFrom(models []llm.ModelInfo, backendID, modelID string, settings config.Settings) llm.ModelInfo {
	for _, m := range llm.MergeOverrides(models, settings.ModelOverrides) {
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
//
// extraSystem is prompt text for this request; turnNotes is text for the conversation — the revert
// and disk notes — and it travels in the request's tail instead (see Assemble). The two are
// separate parameters rather than one string because a note that moves between rounds must not sit
// in message zero: it would invalidate the cache of the whole history behind it, twice per turn.
//
// continuation asks for the rest of a reply the output limit cut short. It is empty for every
// round that is not a continuation, and it is a parameter of its own rather than more turnNotes
// because it is sent as a user turn after the reply rather than appended to it (Assemble says
// why).
func (e *Engine) buildAssembly(ctx context.Context, chat *store.Chat,
	backendID, modelID string, settings config.Settings, model llm.ModelInfo,
	extraSystem, turnNotes, continuation string) (Assembly, []llm.ToolDefinition, error) {

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

	replayToolReasoning := false
	if provider, ok := e.Registry.Provider(backendID); ok && settings.NativeTools && model.Has(llm.CapTools) && len(toolDefs) > 0 {
		if replayer, ok := provider.(llm.ToolReasoningReplayer); ok {
			replayToolReasoning = replayer.ReplaysToolReasoning()
		}
	}
	// The old prompt did not count the same history if tool reasoning replay changed.
	measuredValid = measuredValid && measurement.ReplayToolReasoning == replayToolReasoning

	in := AssemblyInput{
		Chat:        working,
		Messages:    messages,
		Compactions: compactions,
		Documents:   fullDocs,
		Tools:       toolDefs,
		Location:    settings.Location,
		// Every tool the app can execute, so a stored call that names none of them is
		// demoted rather than replayed (see toolhistory.go). Deliberately not the
		// offered list: a tool that is momentarily unavailable — search with both
		// providers down, the index disabled — is still a tool the app can answer.
		// The project tools are the exception: engineKnownTools explains why.
		KnownTools:          engineKnownTools(e, chat),
		ReplayToolReasoning: replayToolReasoning,

		Limit:         limit,
		LimitSource:   limitSource,
		Reserve:       reserve,
		Ceiling:       e.ceilingFor(backendID, modelID),
		MeasuredValid: measuredValid,
		TurnNotes:     turnNotes,
		Continuation:  continuation,
	}
	if measuredValid {
		in.Measured = measurement.PromptTokens
		in.MeasuredSeq = measurement.Seq
	}
	assembled := Assemble(in)

	// A read whose result the request had to drop is a read the model may ask for again, and
	// the tool's own memory is what would refuse it: it answers an unchanged range with "you
	// already read this", which is true about the transcript and useless about the request. The
	// memory is dropped for exactly the files that were dropped, and for those only — every
	// other read keeps the shortcut.
	if len(assembled.AgedReads) > 0 && e.Tools != nil && chat.Workspace != "" {
		if root, err := workspace.Open(chat.Workspace); err == nil {
			for _, rel := range assembled.AgedReads {
				if abs, err := root.Resolve(rel); err == nil {
					e.Tools.ForgetReads(chat.ID, abs)
				}
			}
		}
	}
	return assembled, toolDefs, nil
}

// ceilingFor is what the backend has published about the largest single request it
// accepts for this model, or the zero value when it has published nothing.
//
// It is read from the provider the registry holds rather than passed in by each
// caller, because it is one fact about one backend and model: the turn's budget, the
// preview the model switcher shows and the summarisation budget all have to see the
// same number, or the meter would tell the truth on one screen and not on another.
// The answer is a number the provider already holds — no request is made to ask — or
// the one the user typed into the model's override, which is the only source that can
// speak before the backend has answered anything at all (llm.ModelOverride.RequestCeiling).
func (e *Engine) ceilingFor(backendID, modelID string) llm.Ceiling {
	if e == nil || backendID == "" || modelID == "" {
		return llm.Ceiling{}
	}
	published := llm.Ceiling{}
	if e.Registry != nil {
		if provider, ok := e.Registry.Provider(backendID); ok {
			if ceiler, ok := provider.(llm.RequestCeiling); ok {
				published = ceiler.RequestCeiling(modelID)
			}
		}
	}
	manual := 0
	if e.Settings != nil {
		manual = e.Settings().ModelOverrides[backendID+"/"+modelID].RequestCeiling
	}
	// The smaller of the two, for the reason noteRequestCeiling keeps the smaller of
	// two vendor numbers: a request planned under the tighter one is sendable under
	// both, while planning under the looser one spends a refusal rediscovering it.
	// The manual number therefore cannot loosen what a header or a refusal stated, and
	// a vendor statement that undercuts the user's entry takes over from it — which is
	// the direction a transcription of a limits page should be corrected in.
	switch {
	case manual > 0 && (published.Tokens <= 0 || manual < published.Tokens):
		return llm.Ceiling{Tokens: manual, Source: llm.CeilingFromManual}
	case published.Tokens > 0:
		return published
	}
	return llm.Ceiling{}
}

// errMalformedToolCall is what a user reads instead of the vendor's validation sentence.
//
// It names the cause, says where it came from (the model, not the tool list) and gives
// the two things that help: send it again, or pick a different model for this chat. A
// malformed generation is not a fact about the conversation, so nothing is repaired or
// dropped behind the user's back.
var errMalformedToolCall = uimsg.Errorf("chat.malformedToolCall")

// warn reports something the user should know without failing the turn. It exists as
// a method because `generate` holds the stream in a variable called `events`, which
// shadows the events package for the whole loop body.
func (e *Engine) warn(chatID, warning string) {
	e.emit(events.Warning, map[string]any{"chatId": chatID, "warning": warning})
}

// engineKnownTools is every tool name this engine can execute for this chat, or nil when
// it has no tool service at all — which is what leaves a history untouched in the preview
// engines a test builds (see AssemblyInput.KnownTools).
//
// It stays deliberately wider than the offered set for the reason buildAssembly gives:
// a tool that is momentarily unavailable is still a tool the app can answer, so search
// with both providers down is not demoted from a history. The project tools are the one
// exception, and they are a different kind of exclusion: a chat with no folder cannot be
// answered at all, whatever the moment, because there is no root to read. Leaving them in
// would mean a research chat that happened to hallucinate `read_file` had it *executed*
// and answered with a real file, which is the one failure this filter exists to prevent.
//
// Known and offered are therefore filtered by the same predicate (projectTool), so the
// two can never drift apart.
func engineKnownTools(e *Engine, chat *store.Chat) []string {
	if e == nil || e.Tools == nil {
		return nil
	}
	names := definitionNames(e.Tools.Definitions())
	if chat != nil && chat.IsProject() {
		return names
	}
	kept := make([]string, 0, len(names))
	for _, name := range names {
		if !projectTool(name) {
			kept = append(kept, name)
		}
	}
	return kept
}

// projectTool reports whether a tool reads the project folder. It is the predicate both
// the advertised set and the known-name set are filtered by.
func projectTool(name string) bool {
	for _, project := range tools.ProjectToolNames() {
		if name == project {
			return true
		}
	}
	return false
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
		case tools.ToolGenerateImage:
			// The tool is advertised only when a picture server is configured,
			// which is what keeps a model from spending a tool round discovering
			// that image generation is switched off.
			if e.Tools.Images == nil {
				continue
			}
		default:
			// The project tools are advertised to a project and to nothing else. A chat
			// with no folder has no files to name, and the file tools are the one group
			// whose absence is a boundary rather than a configuration: there is no path
			// they could resolve against. `IsProject` asks both halves, so a chat whose
			// folder was deleted keeps the mode and loses the tools rather than reading
			// from somewhere else (see workspaceFor).
			if !projectTool(t.Name) {
				break
			}
			if chat == nil || !chat.IsProject() {
				continue
			}
			// A write tool needs one more thing than a folder: somebody to ask. A build
			// whose service has no confirmation gate cannot let a change through — the
			// execution refuses it either way (fail closed, see tools.approve) — so the
			// tool is withheld rather than offered as a call that can only be declined.
			// The difference is a round the model spends discovering an answer the app
			// already knew.
			if tools.NeedsApproval(t.Name) && e.Tools.Confirm == nil {
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
		backendID, modelID, settings, model, "", "", "")
	if err != nil {
		return
	}
	e.emit(events.Budget, map[string]any{
		"chatId": chatID,
		"budget": assembly.Budget,
	})
}

// turnModel is the backend and model a turn runs on: the chat's own, or the pair this send
// names (the per-chat model switch).
//
// One function rather than the same six lines twice, because the row Send writes and the turn
// that follows it must not be able to record different models.
func turnModel(chat store.Chat, req TurnRequest) (backendID, modelID string) {
	backendID, modelID = chat.Backend, chat.Model
	if req.Backend != "" {
		backendID = req.Backend
	}
	if req.Model != "" {
		modelID = req.Model
	}
	return backendID, modelID
}

// beginUserTurn stores what the user just sent, before the turn starts.
//
// It is the row the window draws the moment the message is sent, and it may not wait for
// anything the turn does — see the call site in Send for what it was waiting for.
//
// Four sends write nothing, each for its own reason: a regenerate re-runs a turn that is already
// in the chat; an empty one has nothing to store; and a chat with no model, or with a backend
// that is no longer configured, is refused by the turn itself — where a row would be a message
// the app never answered and never offered to resend.
//
// A failure is returned rather than emitted: nothing was started, so the send simply did not
// happen, and the window takes its Stop away and shows the sentence.
func (e *Engine) beginUserTurn(req TurnRequest) error {
	if req.Regenerate || (strings.TrimSpace(req.Text) == "" && len(req.AttachmentIDs) == 0) {
		return nil
	}
	chat, err := e.Store.GetChat(req.ChatID)
	if err != nil {
		// The turn reports an unreadable chat with the event it already uses; answering the
		// send from here would report the same failure twice.
		return nil
	}
	backendID, modelID := turnModel(chat, req)
	if backendID == "" || modelID == "" {
		return nil
	}
	// The same two refusals the turn makes right after this point, checked here too so that a
	// send which cannot run leaves nothing behind. The lookup only scans the registered
	// providers, so it costs no request.
	if _, ok := e.Registry.Provider(backendID); !ok {
		return nil
	}
	return e.appendUserMessage(req, backendID, modelID)
}

// appendUserMessage persists the user's turn and links any attachments.
func (e *Engine) appendUserMessage(req TurnRequest, backendID, modelID string) error {
	msg, err := e.Store.AppendMessage(store.Message{
		ID:        uuid.New().String(),
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
		ID:        uuid.New().String(),
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
