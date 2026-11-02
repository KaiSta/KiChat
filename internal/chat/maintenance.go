package chat

import (
	"context"
	"strings"
	"time"
	"uuid"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/uimsg"
)

// newID returns a fresh identifier.
func newID() string { return uuid.New().String() }

// CompactOptions controls a compaction run.
type CompactOptions struct {
	// Force runs even when no threshold has been reached (manual compaction).
	Force bool
	// Threshold and Ceiling are the two bounds an automatic fold is decided by, resolved
	// by the caller because they are the caller's facts — this chat's own settings and
	// the provider's published number — and left to the planner to compare
	// (PlanCompaction). Both are 0 for a run somebody asked for: Force runs the fold
	// whatever they say.
	Threshold float64
	Ceiling   int
	// Focus is an optional instruction about what to preserve.
	Focus string
	// Target is a desired usage ratio afterwards, 0 for "as much as possible".
	Target float64
	// SummaryCeiling is the per-request budget the summariser's own call has to fit
	// under, in the app's estimated tokens, when the caller knows the app's estimate
	// undercounts the backend's tokenizer. 0 leaves the summariser budgeted against
	// the ceiling the provider publishes, which is the ordinary case.
	//
	// It exists because the summarisation is a request against the same limit as the
	// turn that was refused: sizing it to a ceiling the vendor counts at twice the
	// estimate makes the fold fail on the very call that was meant to make the chat
	// fit (internal/chat/oversize.go).
	SummaryCeiling int
	// FromSeq/ToSeq narrow the range to fold. Zero means "let the planner decide".
	FromSeq int
	ToSeq   int
	// Trigger overrides the name the transcript's card shows for this run. It
	// exists because two folds are forced without being the user's: the one that
	// answers a backend refusing the request for its own size, which has to be
	// explained rather than look like a button somebody pressed.
	Trigger string
	// ProtectNewestTurn keeps the fold off the message that started the turn, so a
	// run that has nothing older to fold folds nothing. Every fold that exists to
	// make a request smaller sets it; a manual run does not, because there the
	// alternative to folding the newest turn is folding nothing at all.
	ProtectNewestTurn bool
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
	// A manual compaction is a turn in every way that matters: it reads the whole
	// transcript, it writes, it can run for minutes, and Stop has to be able to end
	// it. So it claims the chat like one. That claim is what refuses a chat that is
	// already generating — and, the other way round, what keeps a new message from
	// starting beside a compaction that is summarising the history it would be
	// appended to. It used to check IsRunning and then run unregistered, so Stop
	// could not reach it and a message sent during one landed in the fold.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	handle := &turnHandle{cancel: cancel, chatID: chatID}
	if !e.turns.claim(chatID, handle) {
		return CompactResult{}, uimsg.Errorf("chat.compactionWhileRunning")
	}
	defer e.turns.release(chatID, handle)

	settings := e.Settings()
	chat, err := e.Store.GetChat(chatID)
	if err != nil {
		return CompactResult{}, err
	}
	backendID, modelID := chat.Backend, chat.Model
	if backendID == "" || modelID == "" {
		return CompactResult{}, uimsg.Errorf("model.noneSelected")
	}
	provider, ok := e.Registry.Provider(backendID)
	if !ok {
		return CompactResult{}, uimsg.Errorf("chat.backendNotConfigured", backendID)
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

// DeleteCompaction restores the folded history while holding the same chat claim as
// generation and compaction, so a summary cannot be saved on top of deleted memory.
func (e *Engine) DeleteCompaction(chatID, compactionID string) error {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle := &turnHandle{cancel: cancel, chatID: chatID}
	if !e.turns.claim(chatID, handle) {
		return uimsg.Errorf("chat.compactionWhileRunning")
	}
	defer e.turns.release(chatID, handle)
	compactions, err := e.Store.ListCompactions(chatID)
	if err != nil {
		return err
	}
	for _, c := range compactions {
		if c.ID == compactionID {
			if err := e.Store.DeleteCompactionsFrom(chatID, c.ToSeq); err != nil {
				return err
			}
			e.InvalidateMeasurement(chatID)
			return nil
		}
	}
	return uimsg.Errorf("compaction.gone", compactionID)
}

// autoFold folds a chat's history *before* the request goes out, which is the one place a
// turn decides to fold without anybody pressing anything. Both of the reasons that can
// happen are the planner's, not this function's: the user's threshold and a backend's
// published per-request ceiling are two bounds of one decision (PlanCompaction, through
// automaticBound), compared once instead of by two calls that each folded on their own.
//
// The two calls are what this replaces, and the cost they had was not only duplicated
// code: a chat that had crossed both folded twice in the same turn — the threshold run
// keeping its keep-turns boundary and the ceiling run then folding further for its target —
// so it paid for two summaries back to back to arrive where one fold reaches.
//
// What is left here is resolving the two facts the decision needs and leaving early where
// there is nothing to decide. Every send runs this, so the early return is also what keeps a
// chat with no bound at all from being assembled for an answer that cannot change.
//
// midTurn is the between-rounds call (generate.go). A tool round is exactly where a turn
// grows — the assert before the turn priced neither the reply nor the tool result that the
// previous round added — so the size has to be asked again before each request. The ceiling
// below is always asked: crossing it costs the whole answer. The user's threshold is asked
// when they switched the between-rounds check on — or always in a project, which has no
// moment between answers to fold at (autoFoldThreshold has the argument). A window fold pays
// for itself differently from a ceiling: it is one summary call inside an answer somebody is
// waiting for, and in a chat the alternative is ending the answer and folding before the
// next message.
func (e *Engine) autoFold(ctx context.Context, chat *store.Chat, provider llm.Provider,
	backendID, modelID string, modelInfo llm.ModelInfo, settings config.Settings,
	midTurn bool) error {

	// The user's bound exists only while they have automatic compaction switched on, and 0
	// is how its absence reaches the planner — which is why nothing defaults it here: the
	// planner's default belongs to the user's setting, not to a caller that switched the
	// feature off (effectiveThreshold is where it lives).
	threshold := autoFoldThreshold(settings, *chat, midTurn)
	// The backend's bound is a number it published, and it is not the window: Groq's
	// per-minute token allowance is also the largest single request the account may send.
	// Only a backend that publishes one has it (llm.RequestCeiling), learned from a
	// refusal's own sentence or from the budget header of an ordinary answer — or typed
	// in by the user, which is the one route that speaks before the backend has answered
	// anything (llm.ModelOverride.RequestCeiling). ceilingFor merges those, so the fold
	// decided here and the budget the meter paints cannot disagree about the limit.
	ceiling := e.ceilingFor(backendID, modelID).Tokens
	if threshold <= 0 && ceiling <= 0 {
		return nil
	}
	// A window nobody reports cannot be folded *against*: the planner prices the kept
	// turns as a ratio of it, so it cannot be evaluated at all — compactWith refuses the
	// run for the same reason. That refusal is not forwarded here, and the reason is this
	// function's own contract: it runs before *every* send and an automatic run has nobody
	// to report back to, so forwarding it put the same warning in front of the user once
	// per message for as long as the model stayed undescribed. Measured: that is the
	// default state of the NVIDIA backend, whose listing reports no context window for any
	// of its models, so a backend that was working looked broken on every turn.
	//
	// The condition is a standing fact about the model, not an incident, and it is already
	// visible where it belongs: the context meter reads "unknown" and the inspector names
	// the source of the number. The manual path keeps the refusal with its reason, because
	// there the user asked and there is somebody to answer — pinned by
	// TestManualCompactStillRefusesAnUnknownWindow.
	//
	// limitFor is used rather than modelInfo.ContextLimit directly so the guard cannot
	// drift from the number compactWith will see: a per-chat NumCtx makes an otherwise
	// unknown window usable, and compacting against it must still work.
	if limit, _ := limitFor(*chat, modelInfo); limit <= 0 {
		return nil
	}
	// One call, and the planner decides: both numbers are handed over rather than acted
	// on, so the fold that happens and the bound it was decided by cannot disagree. The
	// created compaction is irrelevant here — an automatic run has nobody to report back
	// to, and "nothing to fold" is already the silent outcome.
	_, err := e.compactWith(ctx, chat, provider, backendID, modelID, modelInfo, settings,
		CompactOptions{Threshold: threshold, Ceiling: ceiling})
	return err
}

// compactWith performs one compaction. It returns the compaction it created, or
// nil when there was nothing to fold — a planner that decided no compaction was
// needed, or a range that turned out to hold no messages.
func (e *Engine) compactWith(ctx context.Context, chat *store.Chat, provider llm.Provider,
	backendID, modelID string, modelInfo llm.ModelInfo, settings config.Settings,
	opts CompactOptions) (*store.Compaction, error) {

	assembly, _, err := e.buildAssembly(ctx, chat, backendID, modelID, settings, modelInfo, "", "", "")
	if err != nil {
		return nil, err
	}
	if assembly.Budget.Limit <= 0 {
		return nil, uimsg.Errorf("chat.contextUnknown")
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
		Messages:          messages,
		Compactions:       compactions,
		Budget:            assembly.Budget,
		Threshold:         opts.Threshold,
		Ceiling:           opts.Ceiling,
		KeepRecentTurns:   effectiveKeepTurns(*chat, settings),
		Reserve:           settings.Compact.ReserveForResponse,
		MaxSummaryTokens:  settings.Compact.MaxSummaryTokens,
		Force:             opts.Force,
		Target:            opts.Target,
		ProtectNewestTurn: opts.ProtectNewestTurn,
	})
	if !plan.Needed {
		return nil, nil
	}
	// The active memory covers a contiguous prefix. A requested range may end
	// only before another user turn (or at the end), and cannot skip its start.
	if opts.FromSeq < 0 || opts.ToSeq < 0 || (opts.FromSeq != 0 && opts.FromSeq != plan.FromSeq) {
		return nil, uimsg.Errorf("chat.compactionRange")
	}
	if opts.ToSeq != 0 {
		valid := false
		for i, m := range messages {
			if m.Seq != opts.ToSeq || m.Seq < plan.FromSeq {
				continue
			}
			valid = i == len(messages)-1 || messages[i+1].Role == "user"
			break
		}
		if start, streaming := runningTurn(messages); streaming && (start == 0 || opts.ToSeq >= start) {
			valid = false
		}
		if !valid {
			return nil, uimsg.Errorf("chat.compactionRange")
		}
		plan.ToSeq = opts.ToSeq
		plan.KeepFromSeq = opts.ToSeq + 1
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

	// The summariser may be a different model than the chat's own, in which case
	// its own window is what this call has to live in — the plan above stays
	// priced against the chat's window, because the plan is about the chat.
	target, err := e.summariser(ctx, provider, backendID, modelID, modelInfo,
		assembly.Budget.Limit, chat.NumCtx, settings)
	if err != nil {
		return nil, err
	}
	limit := target.Limit
	if limit <= 0 {
		// An unknown window for the summariser. The chat's own is the only honest
		// fallback, and it is reported: the alternative is a summary silently
		// written through a different budget than the one that was configured.
		limit = assembly.Budget.Limit
		if target.Configured {
			e.emit(events.Warning, map[string]any{
				"chatId":  chat.ID,
				"warning": uimsg.Errorf("chat.summaryWindowUnknown", target.Model).Error(),
			})
		}
	}
	// A ceiling the summariser is planned against is smaller than its window, and a
	// request sized to the window would be refused for its own size: Groq's per-minute
	// token allowance is that ceiling, so a transcript budgeted against a 131k window is
	// one more oversized request — which is how a compaction that exists to make a
	// chat fit ends up failing on the same refusal. The summary call is budgeted
	// against the ceiling instead, and the ceiling is the backend's own number or the
	// one the user typed for that model, whichever is tighter (ceilingFor).
	if ceiling := e.ceilingFor(target.Backend, target.Model).Tokens; ceiling > 0 && ceiling < limit {
		limit = ceiling
	}
	// And the caller's corrected reading of the same ceiling, when it has one: it
	// was measured for the chat's model, so it is only applied to a summariser that
	// is that model. A second model on the same backend has its own tokenizer as far
	// as this number is concerned, and the published ceiling is the honest budget for
	// it.
	if opts.SummaryCeiling > 0 && target.Backend == backendID && target.Model == modelID &&
		opts.SummaryCeiling < limit {
		limit = opts.SummaryCeiling
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
	margin := limit / 16
	if room := limit - overhead - margin - summaryMinTranscriptTokens; outputTokens > room {
		outputTokens = room
	}
	if outputTokens < 1 {
		outputTokens = 1
	}
	budget := PlanTranscriptBudget(limit, outputTokens+margin, overhead)
	transcriptMessages := append([]store.Message(nil), toFold...)
	type documentBlock struct{ filename, text string }
	var documents []documentBlock
	for i := range transcriptMessages {
		m := &transcriptMessages[i]
		for _, a := range m.Attachments {
			if a.Placement != store.PlacementInline {
				continue
			}
			// A text-only summary cannot see an image. Keep that turn verbatim
			// rather than replace the picture with an invented description.
			if isImageAttachment(a) {
				return nil, uimsg.Errorf("chat.compactionImage")
			}
			if a.DocumentID == "" {
				return nil, uimsg.Errorf("chat.compactionAttachmentMissing", a.Filename)
			}
			d, err := e.Store.GetDocument(a.DocumentID)
			if err != nil {
				return nil, uimsg.Errorf("chat.compactionAttachment", a.Filename, uimsg.Sentence(err))
			}
			block := "<attachment id=\"" + a.ID + "\">\n" + renderDocumentBlock(d, a) + "\n</attachment>"
			documents = append(documents, documentBlock{a.Filename, block})
			m.Content += "\n\n" + block
		}
	}
	// Leave room for the actual transcript, not merely its role headings.
	if budget.Total <= 0 || budget.Block <= llm.EstimateTokens(truncationMarker) {
		return nil, uimsg.Errorf("chat.compactionNoTranscript")
	}
	transcript := RenderTranscript(transcriptMessages, budget)
	usable := false
	for _, line := range strings.Split(transcript, "\n") {
		switch strings.TrimSpace(line) {
		case "", "…[gekürzt]", "### Nutzer", "### Assistent", "### Tool-Ergebnis", "### Websuche":
		default:
			usable = true
		}
	}
	if !usable {
		return nil, uimsg.Errorf("chat.compactionNoTranscript")
	}
	for _, document := range documents {
		if !strings.Contains(transcript, document.text) {
			return nil, uimsg.Errorf("chat.compactionAttachmentTooLarge", document.filename)
		}
	}

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
	disableThinking := target.Info.Has(llm.CapThink)
	if t, ok := target.Provider.(llm.ThinkingToggle); ok && t.CanToggleThinking() {
		disableThinking = true
	}
	summary, err := Summarize(ctx, target.Provider, target.Model, SummarizeOptions{
		PreviousSummary: previous,
		Transcript:      transcript,
		Focus:           opts.Focus,
		Temperature:     settings.Compact.SummaryTemperature,
		MaxTokens:       maxTokens,
		NumCtx:          target.NumCtx,
		DisableThinking: disableThinking,
		ContextLimit:    limit,
	})
	if err != nil {
		return nil, err
	}

	trigger := triggerName(opts.Force)
	if opts.Trigger != "" {
		trigger = opts.Trigger
	}
	compaction := store.Compaction{
		ID:           newID(),
		ChatID:       chat.ID,
		Trigger:      trigger,
		FromSeq:      plan.FromSeq,
		ToSeq:        plan.ToSeq,
		Summary:      summary,
		Focus:        opts.Focus,
		TokensBefore: plan.TokensBefore,
		TokensAfter:  llm.EstimateTokens(summary),
		// The model that actually wrote the memory, which is not necessarily the
		// chat's own: the card in the transcript names it, and a memory written
		// by a different model is a fact worth being able to see.
		Model:   target.Model,
		Backend: target.Backend,
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

// summaryTarget is the model that writes a compaction's memory: which provider and
// model to call, the window it has to work in, and the per-chat override to send.
type summaryTarget struct {
	Provider llm.Provider
	Backend  string
	Model    string
	Info     llm.ModelInfo
	// Limit is the context window the summarisation call is budgeted against.
	Limit int
	// NumCtx is a per-chat context override. It belongs to the chat's own model,
	// so it is carried over only when that is also the summariser.
	NumCtx int
	// Configured marks a model chosen in the settings instead of the chat's own.
	Configured bool
}

// summariser resolves which model writes the memory.
//
// The chat's own model is the default: it is already loaded, and chatLimit/numCtx
// already encode any per-chat context override. A configured summary model wins
// instead, and a choice that cannot be honoured is an error rather than a silent
// fallback — a user who picked a non-thinking model for this job has to hear that
// the choice was not applied, or the fix for an empty summary stays hidden.
// (Measured on a local reasoning model: 48 of 48 output tokens went to reasoning
// and not a single message was produced.)
func (e *Engine) summariser(ctx context.Context, provider llm.Provider, backendID, modelID string,
	modelInfo llm.ModelInfo, chatLimit, numCtx int, settings config.Settings) (summaryTarget, error) {

	own := summaryTarget{
		Provider: provider,
		Backend:  backendID,
		Model:    modelID,
		Info:     modelInfo,
		Limit:    chatLimit,
		NumCtx:   numCtx,
	}
	ref := settings.Compact.SummaryModel
	if ref.Backend == "" || ref.Model == "" {
		return own, nil
	}
	if ref.Backend == backendID && ref.Model == modelID {
		// Spelled out, but the same model: nothing to resolve, and the per-chat
		// override still applies to it.
		return own, nil
	}
	summaryProvider, ok := e.Registry.Provider(ref.Backend)
	if !ok {
		return summaryTarget{}, uimsg.Errorf("chat.summaryBackendUnknown", ref.Model, ref.Backend)
	}
	info := e.resolveModel(ctx, ref.Backend, ref.Model, settings)
	return summaryTarget{
		Provider:   summaryProvider,
		Backend:    ref.Backend,
		Model:      ref.Model,
		Info:       info,
		Limit:      info.ContextLimit,
		Configured: true,
	}, nil
}

// The names a compaction's card can carry. `auto` and `manual` are how the memory
// came to exist — a threshold nobody watched, or a button somebody pressed — and
// `too_large` is neither: the backend refused the request for its own size, so the
// history was folded because there was no other way to answer. A card that said
// "manual" there would credit the user with a fold they never asked for.
const (
	triggerAuto     = "auto"
	triggerManual   = "manual"
	triggerTooLarge = "too_large"
)

func triggerName(force bool) string {
	if force {
		return triggerManual
	}
	return triggerAuto
}

// autoFoldThreshold is the user's bound as one automatic fold may use it: 0 when their
// automatic compaction is off for this chat — or, mid-turn, when they have not asked for
// the between-rounds check.
//
// One function rather than the same two conditions written twice, so the fold before a
// turn and the fold between its rounds cannot disagree about what "switched on" means.
// The mid-turn switch is subordinate deliberately: a chat with automatic compaction off
// does not regain it between rounds, which is the reading a checkbox in the compaction
// panel has to have.
//
// A **project** is the one exception to the switch, and the reason it is written down here
// rather than as a second condition in the loop. `Compact.MidTurnCheck` is a choice a chat
// can make because a chat has an alternative: the answer ends, and the fold happens before
// the next message. A project has no such moment — its reply is one piece of work, and the
// round it is in the middle of *is* the answer, so a request that outgrows the window costs
// that work rather than delaying it. The assembly only measures an over-full request, it
// never trims one (Assemble, in context.go), so nothing else folds it either. A project therefore
// asks its threshold between rounds whatever the switch says; the master switches above are
// not overridden, so compaction switched off still means no fold.
func autoFoldThreshold(settings config.Settings, chat store.Chat, midTurn bool) float64 {
	if !settings.Compact.Enabled || !chat.AutoCompact {
		return 0
	}
	if midTurn && !settings.Compact.MidTurnCheck && !chat.IsProject() {
		return 0
	}
	return effectiveThreshold(chat, settings)
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
