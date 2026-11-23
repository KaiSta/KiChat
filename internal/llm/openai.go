package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// openAICompatProvider talks to any server exposing OpenAI-style
// /chat/completions with SSE streaming. LM Studio is the primary user of this
// code; the generic "custom endpoint" provider reuses it verbatim.
//
// It prefers LM Studio's richer native namespace (/api/v0) when present, since
// that namespace reports tokens/second, time-to-first-token and the loaded
// context length directly. When the namespace is missing it degrades to
// /v1/chat/completions and computes the numbers itself.
type openAICompatProvider struct {
	id      string
	name    string
	baseURL string
	apiKey  string
	headers map[string]string

	// chatPaths are tried in order; a 404 retires the path permanently.
	chatPaths []string
	// modelsPath returns rich model metadata; empty if unsupported.
	modelsPath string
	// openAIModelsPath is the plain /v1/models fallback.
	openAIModelsPath string
	// propsPath returns backend-specific generation properties; empty if
	// unsupported. llama.cpp's /props is the only place that reports the context
	// window a model is actually loaded with, which `-c` — defaulting to
	// "loaded from model" — leaves unknowable client-side.
	propsPath string

	// requiresKey marks a backend that cannot answer without a credential. A
	// request without one is not attempted at all, so the user is told what is
	// missing instead of being handed the vendor's 401 body.
	requiresKey bool
	// modelsFlavour selects how the rich model listing is read. Both vendors
	// return a `data` array; they disagree about everything inside it.
	modelsFlavour modelListFlavour

	// thinkingToggle marks a backend whose request can turn thinking off with
	// `enable_thinking` even though its model metadata advertises no thinking
	// capability (LM Studio). DeepSeek and plain OpenAI-compatible servers do not
	// document the field, so they are left alone.
	thinkingToggle bool

	// chatTemplateKwargs marks a backend that turns thinking off by handing the
	// parameter to its chat template instead of sending a field of its own.
	// llama.cpp works this way, through `chat_template_kwargs`; LM Studio does
	// not, so the two cases stay apart rather than sharing one field name that
	// only one of them accepts.
	chatTemplateKwargs bool

	// seedField is the request field this backend reads a pinned sampling seed
	// from. The OpenAI-compatible servers — and DeepSeek, and llama.cpp — call
	// it `seed`; Mistral's published request body has no such property at all
	// and calls it `random_seed`, so the name travels with the flavour instead
	// of living in the body builder. It matters because a field name the vendor
	// does not know is at best a silent no-op: Mistral publishes the body with
	// `additionalProperties: false`.
	seedField string

	// reasoningEffortOff marks a backend that documents a field of its own for
	// "do not think": Mistral's `reasoning_effort: "none"`. LM Studio and
	// llama.cpp express the same request in their own way (above); a server
	// that documents neither is sent neither.
	reasoningEffortOff bool

	// nativeChatPath is LM Studio's v1 chat route, and v1ModelsPath is the
	// listing that says which models expose a reasoning setting. Both are used
	// only for a request that explicitly asks not to think: that route is the
	// only place the setting exists, and it is deliberately narrow (no tools, no
	// assistant turns), so every other request stays on chatPaths.
	nativeChatPath string
	v1ModelsPath   string

	mu          sync.Mutex
	chatPathIdx int
	// reasoningChoice maps a model to the reasoning setting to send for it,
	// learned from the v1 listing. A model missing from the map exposes no
	// reasoning setting, and none is sent: measured, LM Studio answers such a
	// request with HTTP 400 "does not expose reasoning configuration", so the
	// setting may never be sent blind.
	reasoningChoice map[string]string
	// reasoningLoaded marks the listing as read (even when it produced no
	// choices) so an ordinary turn never pays for it twice.
	reasoningLoaded bool
	// v1Unavailable records a 404 from the v1 namespace, which is permanent:
	// a build without it is not asked again. A transient failure is deliberately
	// not recorded, so the next summarisation retries instead of silently
	// keeping the optimisation off for the rest of the session.
	v1Unavailable bool
	// reasoningEffortRejected records a request that lost the
	// `reasoning_effort` field to a 400/422. The field is an optimisation for
	// compaction, so it is retired for the session rather than sent again to
	// the same answer.
	reasoningEffortRejected bool

	// props caches the reading of propsPath. `-c` is fixed when llama-server
	// starts, so the number only changes when the server behind the URL is
	// replaced — which is precisely what an explicit re-check announces, and
	// InvalidateCache is where it is dropped.
	props           *llamaCppProps
	propsLoaded     bool
	propsGeneration uint64
	// propsAt is when the last /props reading was taken, so a server replaced
	// underneath the app is noticed; see propsTTL.
	propsAt time.Time

	// requestCeilings remembers, per model, the largest single request the account
	// may send and where that number came from. Groq's per-minute token allowance is
	// that number, and its listing reports windows rather than budgets, so the number
	// is read from the vendor's own statements: a refusal's sentence, or the budget
	// header every response carries.
	ceilingMu       sync.Mutex
	requestCeilings map[string]Ceiling

	// windowMu guards learnedWindows: the context window a refusal stated for a
	// model, remembered because the listing of a backend that publishes none —
	// NVIDIA hosted, a plain OpenAI-compatible server — never will. It is the only
	// source of a window that arrives after the app has already talked to the
	// model, and it is what keeps one refusal from being paid again every turn.
	windowMu       sync.Mutex
	learnedWindows map[string]int

	// requestBudgets remembers, per model, the last thing a response said about the
	// minute's token allowance: what was left of it and how long that lasts. It is
	// what a turn is paced against, so a chat that would otherwise spend the minute
	// in three turns waits by seconds instead of being refused (internal/llm/pace.go).
	budgetMu       sync.Mutex
	requestBudgets map[string]modelBudget

	// requestRates remembers, per model, the minute's *request* rate: the number a
	// refusal named, against a count of the requests this app has had answered in the
	// trailing minute. It is the one allowance no header of this API states — the
	// request headers beside the token ones describe the day — so a tool-heavy turn
	// that spends a minute of requests is paced on evidence the app had to gather
	// differently (internal/llm/pace.go).
	rateMu       sync.Mutex
	requestRates map[string]*requestRate
}

// modelListFlavour names the shape of a model listing. The zero value is LM
// Studio's native namespace, which is what every existing configuration uses.
type modelListFlavour int

const (
	flavourLMStudio modelListFlavour = iota
	flavourDeepSeek
	flavourLlamaCpp
	flavourMistral
	flavourNvidia
	flavourGroq
)

// The field Mistral documents for "do not think". Its request schema spells the
// reasoning control as `reasoning_effort: "high" | "none"`, where "none"
// disables the reasoning effort, and deprecates `prompt_mode: "reasoning"` in
// its favour. The value is a plain string rather than a nested object, so it
// needs no builder of its own the way llama.cpp's template kwargs do.
const (
	reasoningEffortField = "reasoning_effort"
	reasoningEffortNone  = "none"
)

// noteRequestCeiling remembers a ceiling the vendor stated, so the next request for
// that model can be planned against the real limit instead of spending another
// refusal to rediscover it.
//
// The tighter of the number already known and the new one is kept, and that choice is
// deliberate. Both sources describe one limit read at two moments, and a plan that
// fits the smaller of two statements about it fits both: keeping the tighter can cost
// one fold that was not needed, while keeping the looser can cost a refused turn. It
// also makes the two writers order-independent — a budget header that arrives after a
// refusal cannot loosen the ceiling a refusal proved.
func (p *openAICompatProvider) noteRequestCeiling(model string, c Ceiling) {
	if model == "" || c.Tokens <= 0 || c.Source == "" {
		return
	}
	p.ceilingMu.Lock()
	defer p.ceilingMu.Unlock()
	if p.requestCeilings == nil {
		p.requestCeilings = map[string]Ceiling{}
	}
	if known, ok := p.requestCeilings[model]; ok && known.Tokens > 0 && known.Tokens < c.Tokens {
		return
	}
	p.requestCeilings[model] = c
}

// RequestCeiling implements llm.RequestCeiling: the largest single request this
// provider knows the account may send for a model, and where the number came from.
//
// The zero value means it has never been told, which is the state of a session before
// its first answer: no route of Groq's API publishes a budget without being asked a
// question, so the number arrives on the first response — the answer or the refusal —
// and every request after it is planned against the real limit.
func (p *openAICompatProvider) RequestCeiling(model string) Ceiling {
	p.ceilingMu.Lock()
	defer p.ceilingMu.Unlock()
	return p.requestCeilings[model]
}

// noteContextWindow remembers the window a refusal stated for a model.
//
// Only a refusal that named the model's own window reaches this (limits.go), and
// the newest reading is the one kept: a deployment can be raised or lowered, and
// the latest refusal is the only evidence of either. What it may not do is widen a
// window a listing publishes — that direction is handled in applyLearnedWindows,
// where a stale learned number cannot override a larger published one.
func (p *openAICompatProvider) noteContextWindow(model string, window int) {
	if model == "" || window <= 0 {
		return
	}
	p.windowMu.Lock()
	defer p.windowMu.Unlock()
	if p.learnedWindows == nil {
		p.learnedWindows = map[string]int{}
	}
	p.learnedWindows[model] = window
}

// learnedWindow is the window a refusal stated for one model, or 0 when none has.
func (p *openAICompatProvider) learnedWindow(model string) int {
	p.windowMu.Lock()
	defer p.windowMu.Unlock()
	return p.learnedWindows[model]
}

// learnedWindowSnapshot copies the measured windows, for a registry that is about
// to replace this provider and wants to carry them across the rebuild.
func (p *openAICompatProvider) learnedWindowSnapshot() map[string]int {
	p.windowMu.Lock()
	defer p.windowMu.Unlock()
	if len(p.learnedWindows) == 0 {
		return nil
	}
	out := make(map[string]int, len(p.learnedWindows))
	for model, window := range p.learnedWindows {
		out[model] = window
	}
	return out
}

// adoptLearnedWindows takes over measurements made by the provider this one
// replaces. The registry only calls it for the same backend id at the same address
// and kind, so a window learned from one deployment is never applied to another.
func (p *openAICompatProvider) adoptLearnedWindows(windows map[string]int) {
	if len(windows) == 0 {
		return
	}
	p.windowMu.Lock()
	defer p.windowMu.Unlock()
	if p.learnedWindows == nil {
		p.learnedWindows = make(map[string]int, len(windows))
	}
	for model, window := range windows {
		if window > 0 {
			p.learnedWindows[model] = window
		}
	}
}

// applyLearnedWindows overlays the windows refusals have stated onto a listing.
//
// A learned window replaces an absent one and tightens a larger published one; it
// never widens a smaller one. The two errors are not symmetric: planning against a
// window that is too large spends a refused request, while planning against one
// that is too small costs a fold that was not needed. That is the same direction
// `fromLMStudioEntry` takes when it prefers a model's loaded context length to its
// maximum, and the same one noteRequestCeiling takes with a vendor's ceiling.
func (p *openAICompatProvider) applyLearnedWindows(models []ModelInfo) []ModelInfo {
	for i := range models {
		learned := p.learnedWindow(models[i].ID)
		if learned > 0 && (models[i].ContextLimit <= 0 || learned < models[i].ContextLimit) {
			models[i].ContextLimit = learned
		}
	}
	return models
}

// requestBudget learns a model's per-request ceiling from the budget a response
// carries. It is attached to a Groq chat request's context, where the model being
// planned for is known and a budget exists.
type requestBudget struct {
	provider *openAICompatProvider
	model    string
}

// ResponseHeaders reads the ceiling out of one response. The vendor's own refusal is
// not the only place it states the limit: every response, the answer as much as the
// failure, carries the tokens-per-minute allowance of the model that produced it,
// which is why the first oversized request of a session can be folded instead of
// sent.
func (b requestBudget) ResponseHeaders(h http.Header) {
	if tokens := ceilingFromHeaders(h); tokens > 0 {
		b.provider.noteRequestCeiling(b.model, Ceiling{Tokens: tokens, Source: CeilingFromHeader})
	}
	// And the remainder, which is the same header family read for a different
	// question: the ceiling says whether one request fits at all, the remainder says
	// whether there is anything left of the minute to spend on it (pace.go).
	b.provider.noteRequestBudget(b.model, h)
	// And the request itself, counted here because here is where a response happens at
	// all: the minute's *request* rate is a count, so what it is measured against is
	// how many of this model's requests the vendor has answered for this app inside the
	// trailing minute — every response, the refusals as much as the answers (pace.go).
	b.provider.noteRequestSpent(b.model, time.Now())
}

// keyCheck reports a backend that needs a credential and has none.
func (p *openAICompatProvider) keyCheck() error {
	if p.requiresKey && strings.TrimSpace(p.apiKey) == "" {
		return ErrMissingAPIKey
	}
	return nil
}

// seedKey is the request field this backend reads a pinned seed from, which is
// `seed` everywhere but Mistral.
func (p *openAICompatProvider) seedKey() string {
	if p.seedField != "" {
		return p.seedField
	}
	return "seed"
}

func newLMStudioProvider(baseURL, apiKey string) *openAICompatProvider {
	baseURL = strings.TrimRight(baseURL, "/")
	return &openAICompatProvider{
		id:      "lmstudio",
		name:    "LM Studio",
		baseURL: baseURL,
		apiKey:  apiKey,
		headers: map[string]string{},
		chatPaths: []string{
			baseURL + "/api/v0/chat/completions",
			baseURL + "/v1/chat/completions",
		},
		modelsPath:       baseURL + "/api/v0/models",
		openAIModelsPath: baseURL + "/v1/models",
		nativeChatPath:   baseURL + "/api/v1/chat",
		v1ModelsPath:     baseURL + "/api/v1/models",
		thinkingToggle:   true,
	}
}

func (p *openAICompatProvider) ID() string   { return p.id }
func (p *openAICompatProvider) Name() string { return p.name }

// ReplaysReasoning reports that this wire carries an assistant reply's reasoning only for the one
// vendor that requires it beside tool calls; every other OpenAI-compatible backend here receives
// the reply without it.
func (p *openAICompatProvider) ReplaysReasoning(withTools bool) bool {
	return withTools && p.modelsFlavour == flavourDeepSeek
}

// CanToggleThinking reports whether this backend accepts `enable_thinking` in the
// request body, or — for llama.cpp — as a chat template parameter.
//
// The answer is deliberately optimistic for llama.cpp: the provider claims the
// ability, and the request itself is built conservatively, so a template that
// turns out not to read the parameter is left alone rather than sent a field it
// would reject. Compaction asks this question without a context to look a
// template up in, and it also adapts its own output budget, so claiming too
// little would cost a working model the optimisation outright.
func (p *openAICompatProvider) CanToggleThinking() bool {
	return p.thinkingToggle || p.chatTemplateKwargs
}

func (p *openAICompatProvider) authHeaders() map[string]string {
	h := map[string]string{}
	for k, v := range p.headers {
		h[k] = v
	}
	if p.apiKey != "" {
		h["Authorization"] = "Bearer " + p.apiKey
	}
	return h
}

func (p *openAICompatProvider) Health(ctx context.Context) error {
	_, err := p.ListModels(ctx)
	if err != nil {
		// Both are wrapped, not just printed: a caller that wants to know whether
		// the backend is down or merely missing a key has to be able to ask, and
		// "%s" buried the cause in the text where errors.Is cannot see it.
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

// --- model discovery -------------------------------------------------------

type lmStudioModelEntry struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Type              string   `json:"type"`
	Publisher         string   `json:"publisher"`
	Arch              string   `json:"arch"`
	CompatibilityType string   `json:"compatibility_type"`
	Quantization      string   `json:"quantization"`
	State             string   `json:"state"`
	MaxContextLength  int      `json:"max_context_length"`
	LoadedContextLen  int      `json:"loaded_context_length"`
	Capabilities      []string `json:"capabilities"`
}

func (p *openAICompatProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if err := p.keyCheck(); err != nil {
		return nil, err
	}
	if p.modelsPath != "" {
		// Decoded entry by entry rather than into one vendor's struct: the two
		// listings share only the envelope, so each entry is read in full by the
		// mapper that knows its shape.
		var resp struct {
			Data []json.RawMessage `json:"data"`
		}
		err := doJSON(ctx, jsonClient, http.MethodGet, p.modelsPath, p.authHeaders(), nil, &resp)
		if err == nil {
			return p.applyLearnedWindows(p.enrichLlamaCpp(ctx, p.mapModelEntries(resp.Data))), nil
		}
		if he, ok := err.(*HTTPError); !ok || he.StatusCode != 404 {
			// A real failure (connection refused, auth) — surface it.
			return nil, err
		}
		// 404: this build predates the native namespace. Fall through.
	}

	models, err := p.listOpenAIModels(ctx)
	if err != nil {
		return nil, err
	}
	return p.applyLearnedWindows(p.enrichLlamaCpp(ctx, models)), nil
}

// mapModelEntries reads a rich listing with the mapper for its flavour. An entry
// that cannot be decoded is skipped rather than failing the listing: one
// unexpected model must not hide the others, the same way a dead backend does
// not hide a live one.
func (p *openAICompatProvider) mapModelEntries(raw []json.RawMessage) []ModelInfo {
	// Mistral's listing is read as a whole rather than entry by entry, because
	// which ids belong together is a fact about the list: one model is published
	// under several aliases.
	if p.modelsFlavour == flavourMistral {
		return p.mapMistralEntries(raw)
	}
	// NVIDIA's is read as a whole as well, for the opposite reason to Mistral's:
	// nothing is grouped or dropped there, but an entry with no id has to be
	// skipped rather than offered as a nameless row.
	if p.modelsFlavour == flavourNvidia {
		return p.mapNvidiaEntries(raw)
	}
	// Groq's is its own shape: a rich listing whose fields decide the mapping,
	// including whether an entry can be offered at all (see groq.go).
	if p.modelsFlavour == flavourGroq {
		return p.mapGroqEntries(raw)
	}
	out := make([]ModelInfo, 0, len(raw))
	for _, entry := range raw {
		if p.modelsFlavour == flavourDeepSeek {
			var m deepSeekModelEntry
			if err := json.Unmarshal(entry, &m); err != nil {
				continue
			}
			out = append(out, p.fromDeepSeekEntry(m))
			continue
		}
		if p.modelsFlavour == flavourLlamaCpp {
			var m llamaCppModelEntry
			if err := json.Unmarshal(entry, &m); err != nil {
				continue
			}
			out = append(out, p.fromLlamaCppEntry(m))
			continue
		}
		var m lmStudioModelEntry
		if err := json.Unmarshal(entry, &m); err != nil {
			continue
		}
		out = append(out, p.fromLMStudioEntry(m))
	}
	return out
}

func (p *openAICompatProvider) fromLMStudioEntry(m lmStudioModelEntry) ModelInfo {
	ctxLimit := m.MaxContextLength
	if m.LoadedContextLen > 0 {
		ctxLimit = m.LoadedContextLen
	}
	state := m.State
	if state == "" {
		state = StateNotLoaded
	}
	return ModelInfo{
		ID:           m.ID,
		Backend:      p.id,
		Publisher:    m.Publisher,
		Arch:         m.Arch,
		Quantization: m.Quantization,
		State:        state,
		Type:         m.Type,
		ContextLimit: ctxLimit,
		MaxContext:   m.MaxContextLength,
		Capabilities: m.Capabilities,
	}
}

// listOpenAIModels is the lowest-common-denominator listing. It reads the
// context window only when the server publishes one: vLLM — and a self-hosted
// NVIDIA NIM built on it — puts `max_model_len` on every entry, while a bare
// OpenAI-compatible server states no window at all, and there the user's
// per-model override is where the number comes from. Tool support is never
// reported by this shape.
func (p *openAICompatProvider) listOpenAIModels(ctx context.Context) ([]ModelInfo, error) {
	var resp struct {
		Data []struct {
			ID          string `json:"id"`
			MaxModelLen int    `json:"max_model_len"`
		} `json:"data"`
	}
	if err := doJSON(ctx, jsonClient, http.MethodGet, p.openAIModelsPath, p.authHeaders(), nil, &resp); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(resp.Data))
	for _, m := range resp.Data {
		out = append(out, ModelInfo{
			ID:           m.ID,
			Backend:      p.id,
			State:        StateNotLoaded,
			ContextLimit: m.MaxModelLen,
		})
	}
	return out, nil
}

// --- chat ------------------------------------------------------------------

type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// deltaContent is a `content` value. Every backend this app talked to before
// Mistral sent a plain string, and that is still what the field means here.
// Mistral's reasoning models send a list of typed chunks instead, with the
// model's thinking inside it:
//
//	"content": [{"type":"thinking","thinking":[{"type":"text","text":"…"}]}]
//	"content": [{"type":"text","text":"…"}]
//
// Decoding both is not a nicety. A plain string field makes json.Unmarshal fail
// on the whole chunk, and the stream loop tolerates a malformed frame by
// skipping it — so every frame of such an answer would be dropped and the reply
// would arrive empty, with no error to explain it.
type deltaContent struct {
	Text      string
	Reasoning string
}

// UnmarshalJSON accepts a string, a list of typed chunks, or null. Anything
// else — an object, a number — is left empty rather than failing the chunk, so
// a shape this app does not know cannot take a generation down with it. Unknown
// part types inside the list are ignored the same way, with the known ones
// around them kept.
func (c *deltaContent) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		return json.Unmarshal(raw, &c.Text)
	}
	if raw[0] != '[' {
		return nil
	}
	// The parts are decoded one by one rather than as one typed slice, and a part
	// that cannot be read is skipped on its own. Decoding the whole list at once
	// would mean one chunk type this app has never seen — a citation whose
	// `content` is a list, say — failing the array it sits in and taking the text
	// next to it down as well, which is the same fault at a smaller scale as the
	// string field this type replaced.
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	for _, part := range parts {
		var p struct {
			Type string `json:"type"`
			// A text chunk carries its words in `text`…
			Text string `json:"text"`
			// …and a thinking chunk carries them in `thinking`, which is itself
			// a list of text chunks. `content` is read as well because the two
			// field names appear for one and the same thing across the vendor's
			// chunk types, and a part that uses the other one would otherwise be
			// silently empty.
			Thinking []struct {
				Text string `json:"text"`
			} `json:"thinking"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(part, &p); err != nil {
			continue
		}
		switch p.Type {
		case "thinking":
			for _, t := range p.Thinking {
				c.Reasoning += t.Text
			}
			if len(p.Thinking) == 0 {
				c.Reasoning += p.Content
			}
		case "text":
			c.Text += p.Text
			if p.Text == "" {
				c.Text += p.Content
			}
		}
	}
	return nil
}

type openAIDelta struct {
	Role             string                `json:"role"`
	Content          deltaContent          `json:"content"`
	Reasoning        string                `json:"reasoning"`
	ReasoningContent string                `json:"reasoning_content"`
	Thinking         string                `json:"thinking"`
	ToolCalls        []openAIToolCallDelta `json:"tool_calls"`
}

func (d openAIDelta) reasoningText() string {
	switch {
	case d.Reasoning != "":
		return d.Reasoning
	case d.ReasoningContent != "":
		return d.ReasoningContent
	case d.Thinking != "":
		return d.Thinking
	default:
		// Mistral's thinking arrives as a chunk type inside `content` rather
		// than in a field of the delta.
		return d.Content.Reasoning
	}
}

type openAIChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int         `json:"index"`
		Delta        openAIDelta `json:"delta"`
		Message      openAIDelta `json:"message"`
		FinishReason *string     `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		// DeepSeek reports its prefix-cache split at the top level of `usage`
		// rather than inside prompt_tokens_details, with the hit count on its own
		// and the rest as misses. Reading it is what makes a cached round visible
		// at all: without it the hit count is simply zero for every DeepSeek
		// reply, which is indistinguishable from a cache that never hit
		// (DEEPSEEK.md §5).
		PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
		PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
		// Some OpenAI-compatible servers report the thinking share here instead
		// of in a namespace of their own. Reading it keeps the context meter
		// honest about a model that spent its budget thinking.
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Stats *struct {
		TokensPerSecond  float64 `json:"tokens_per_second"`
		TimeToFirstToken float64 `json:"time_to_first_token"`
		GenerationTime   float64 `json:"generation_time"`
		StopReason       string  `json:"stop_reason"`
	} `json:"stats"`
	// Timings is llama.cpp's own measurement of the generation, sent on the final
	// frame alongside `usage`. It is the difference between the badge saying
	// "llama.cpp" and it saying "estimated".
	Timings   *llamaCppTimings `json:"timings"`
	ModelInfo *struct {
		ContextLength int    `json:"context_length"`
		Arch          string `json:"arch"`
		Quant         string `json:"quant"`
	} `json:"model_info"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// wireMessage is the message shape sent to OpenAI-compatible servers.
type wireMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content"`
	ReasoningContent *string        `json:"reasoning_content,omitempty"`
	Name             string         `json:"name,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

func toWireMessages(messages []Message, replayReasoning bool) []wireMessage {
	out := make([]wireMessage, 0, len(messages))
	for _, m := range messages {
		wm := wireMessage{
			Role:       string(m.Role),
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
		}
		// DeepSeek requires every assistant's reasoning when tools are present, even
		// for assistant messages without a tool call. Other backends must not receive it.
		if replayReasoning && m.Role == RoleAssistant {
			wm.ReasoningContent = &m.Reasoning
		}
		// A picture is a part of a *user* turn only. The schema has no image part for
		// an assistant message, and a strict backend refuses the whole request when it
		// finds one — Groq answers HTTP 400 "'messages.N.content.0.type' : value is
		// not one of the allowed values ['text']" (GROQ.md §6). That was reachable:
		// a picture a tool generated is stored as an inline attachment of the
		// assistant row that asked for it, and that row's content is empty because it
		// only made the call, so the parts list began with the image. The attachment
		// stays in the transcript and the renderer; the model reads the call and its
		// result instead.
		if len(m.Images) > 0 && m.Role != RoleAssistant {
			parts := []map[string]any{}
			if m.Content != "" {
				parts = append(parts, map[string]any{"type": "text", "text": m.Content})
			}
			for _, img := range m.Images {
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": imageDataURL(img)},
				})
			}
			wm.Content = parts
		} else {
			wm.Content = m.Content
		}
		if len(m.ToolCalls) > 0 {
			wm.ToolCalls = make([]wireToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				var wt wireToolCall
				wt.ID = tc.ID
				wt.Type = "function"
				wt.Function.Name = tc.Name
				wt.Function.Arguments = tc.Arguments
				wm.ToolCalls = append(wm.ToolCalls, wt)
			}
		}
		out = append(out, wm)
	}
	return out
}

// imageDataURL wraps a base64 image payload in a data URL, sniffing the media
// type from the payload's own prefix. The wire format has no separate mime field,
// and a JPEG mislabelled as PNG is rejected by servers that validate the declared
// type.
func imageDataURL(b64 string) string {
	return "data:" + imageMimeType(b64) + ";base64," + b64
}

// imageMimeType reads the media type from the base64 of a file's magic bytes.
func imageMimeType(b64 string) string {
	switch {
	case strings.HasPrefix(b64, "iVBORw0KGgo"):
		return "image/png"
	case strings.HasPrefix(b64, "/9j/"):
		return "image/jpeg"
	case strings.HasPrefix(b64, "R0lGOD"):
		return "image/gif"
	case strings.HasPrefix(b64, "UklGR"):
		return "image/webp"
	case strings.HasPrefix(b64, "Qk"):
		return "image/bmp"
	default:
		return "image/png"
	}
}

func (p *openAICompatProvider) currentChatPath() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.chatPathIdx >= len(p.chatPaths) {
		p.chatPathIdx = len(p.chatPaths) - 1
	}
	return p.chatPaths[p.chatPathIdx]
}

func (p *openAICompatProvider) retireChatPath() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.chatPathIdx < len(p.chatPaths)-1 {
		p.chatPathIdx++
	}
}

// reasoningEffortRetired reports whether a previous request already lost the
// `reasoning_effort` field to a rejection, so the field is not offered again in
// this session.
func (p *openAICompatProvider) reasoningEffortRetired() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reasoningEffortRejected
}

// retireReasoningEffort reacts to a failed request that carried
// `reasoning_effort`. A 400 or a 422 is the vendor refusing the field — a
// schema violation is a 422 on Mistral — and the caller then retries without
// it; the answer says whether the error was that rejection, so the caller knows
// a retry is worth making.
//
// Only those two statuses retire the field. A timeout, a 429 or a 500 is a fact
// about the moment, not about the field, and must not cost a working model its
// optimisation for the rest of the session.
func (p *openAICompatProvider) retireReasoningEffort(err error) bool {
	var he *HTTPError
	if !errors.As(err, &he) {
		return false
	}
	if he.StatusCode != http.StatusBadRequest && he.StatusCode != http.StatusUnprocessableEntity {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reasoningEffortRejected = true
	return true
}

// InvalidateCache forgets the learned reasoning settings, so a re-check reads
// the v1 listing again instead of handing an existing installation the answer
// that was cached before the user loaded a different model.
func (p *openAICompatProvider) InvalidateCache() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reasoningChoice = nil
	p.reasoningLoaded = false
	p.v1Unavailable = false
	// llama.cpp's loaded window is chosen when the server starts, so a re-check
	// after restarting it is the only way the app learns the new one. Dropping
	// the reading here is what makes the UI's re-check button do its job.
	p.props = nil
	p.propsLoaded = false
	p.propsGeneration++
}

// --- reasoning control (LM Studio v1) --------------------------------------

// reasoningSettingFor returns the reasoning setting to send for a model and
// whether one should be sent at all.
//
// Only LM Studio's native v1 route can turn thinking off, and only for a model
// that publishes a reasoning configuration: sending `reasoning` for any other
// model is rejected with HTTP 400 "does not expose reasoning configuration"
// (measured against a model without one). So the capability is *read* first and
// the setting derived from it — a hard-coded request field would break every
// ordinary model.
//
// "off" is what a summary wants, because a summary is not a reasoning task. A
// model that allows only graded settings gets "low", the least reasoning it
// permits; a model whose only setting is "on" gets nothing at all, and the
// caller's output budget has to absorb the thinking.
func (p *openAICompatProvider) reasoningSettingFor(ctx context.Context, model string) (string, bool) {
	if p.v1ModelsPath == "" {
		return "", false
	}
	if !p.reasoningCached() {
		p.loadReasoningChoices(ctx)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.v1Unavailable {
		return "", false
	}
	setting, ok := p.reasoningChoice[model]
	return setting, ok
}

func (p *openAICompatProvider) reasoningCached() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reasoningLoaded || p.v1Unavailable
}

// loadReasoningChoices reads the v1 model listing once and reduces it to the
// reasoning setting each model should be asked for.
func (p *openAICompatProvider) loadReasoningChoices(ctx context.Context) {
	var resp struct {
		Models []struct {
			Key          string `json:"key"`
			Capabilities struct {
				Reasoning *struct {
					AllowedOptions []string `json:"allowed_options"`
				} `json:"reasoning"`
			} `json:"capabilities"`
		} `json:"models"`
	}
	err := doJSON(ctx, jsonClient, http.MethodGet, p.v1ModelsPath, p.authHeaders(), nil, &resp)

	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		var he *HTTPError
		switch {
		case errors.As(err, &he) && he.StatusCode == http.StatusNotFound:
			// This build has no v1 namespace; there is nothing to learn and no
			// point asking again.
			p.reasoningLoaded = true
			p.v1Unavailable = true
		default:
			// The server was up but unhelpful. Nothing is remembered, so the next
			// summarisation retries rather than losing the optimisation for good.
		}
		return
	}
	choices := make(map[string]string, 1)
	for _, m := range resp.Models {
		if m.Capabilities.Reasoning == nil {
			continue
		}
		if setting, ok := leastReasoning(m.Capabilities.Reasoning.AllowedOptions); ok {
			choices[m.Key] = setting
		}
	}
	p.reasoningChoice = choices
	// Read once, and only once: without this the listing would be fetched on
	// every summary and would refill a setting that was just retired for a model.
	p.reasoningLoaded = true
}

// leastReasoning picks the least reasoning a model permits, preferring to switch
// it off outright. A model offering only ["low","medium","high"] cannot be
// silenced; asking for its lowest grade is the closest thing to it.
func leastReasoning(options []string) (string, bool) {
	for _, want := range []string{"off", "low"} {
		for _, have := range options {
			if have == want {
				return want, true
			}
		}
	}
	return "", false
}

// retireReasoning reacts to a native request that failed before producing
// anything. A 404 means the namespace is gone; a 400 that names `reasoning`
// means the cached setting was wrong for this model, which is then dropped so
// the next call goes straight to the OpenAI-compatible path instead of paying
// for the same rejection again. Only that one model is forgotten: a re-read
// would return the same listing, so nothing would be gained by it.
func (p *openAICompatProvider) retireReasoning(err error, model string) {
	var he *HTTPError
	if !errors.As(err, &he) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case he.StatusCode == http.StatusNotFound:
		p.v1Unavailable = true
	case he.StatusCode == http.StatusBadRequest && strings.Contains(he.Body, "reasoning"):
		delete(p.reasoningChoice, model)
	}
}

// nativeChatEvent is one frame of LM Studio's /api/v1/chat event stream. The
// route's own field names are used because the two namespaces agree about almost
// nothing: it reports `input_tokens` rather than `prompt_tokens`, has no
// finish_reason, and splits thinking into its own event family.
type nativeChatEvent struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
	Result *struct {
		Output []struct {
			Type    string `json:"type"` // message | reasoning | tool_call
			Content string `json:"content"`
		} `json:"output"`
		Stats *nativeStats `json:"stats"`
	} `json:"result"`
}

type nativeStats struct {
	InputTokens           int     `json:"input_tokens"`
	TotalOutputTokens     int     `json:"total_output_tokens"`
	ReasoningOutputTokens int     `json:"reasoning_output_tokens"`
	TokensPerSecond       float64 `json:"tokens_per_second"`
	TimeToFirstToken      float64 `json:"time_to_first_token_seconds"`
}

// expressibleOnNative reports whether a request can be carried by the native
// route at all. The route takes a system prompt and a flat list of input parts:
// there is no assistant role and no place for an image, a tool schema or a tool
// result. Any of those means "not this route" rather than a quietly shortened
// prompt — a summary must never be written from part of a conversation.
//
// A pinned seed is excluded for the same reason: the compat body honours it, and
// a request that asked for a reproducible answer must not be handed to a route
// that would silently ignore the request. (NumCtx and KeepAlive need no such
// care: only Ollama reads them at all.)
//
// This is checked before the capability listing is read, so a request the route
// cannot carry never costs a round trip to find that out.
func expressibleOnNative(req ChatRequest) bool {
	if len(req.Tools) > 0 || req.Seed != nil {
		return false
	}
	users := 0
	for _, m := range req.Messages {
		if len(m.Images) > 0 || len(m.ToolCalls) > 0 || m.ToolCallID != "" {
			return false
		}
		switch m.Role {
		case RoleSystem:
		case RoleUser:
			users++
		default:
			return false
		}
	}
	return users > 0
}

// nativeV1Request builds the /api/v1/chat body for a request that must not
// think. The caller has already established, via expressibleOnNative, that the
// route can carry it.
func nativeV1Request(req ChatRequest, reasoning string) (map[string]any, bool) {
	if !expressibleOnNative(req) {
		return nil, false
	}
	var (
		system []string
		input  = make([]map[string]any, 0, len(req.Messages))
	)
	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			if m.Content != "" {
				system = append(system, m.Content)
			}
			continue
		}
		input = append(input, map[string]any{"type": "text", "content": m.Content})
	}
	if len(input) == 0 {
		return nil, false
	}

	body := map[string]any{
		"model":       req.Model,
		"input":       input,
		"stream":      true,
		"temperature": req.Temperature,
		"reasoning":   reasoning,
		// An internal call is not a conversation: without this the exchange is
		// kept and answered with a response_id, so every compaction would litter
		// the user's LM Studio chat history.
		"store": false,
	}
	if len(system) > 0 {
		body["system_prompt"] = strings.Join(system, "\n\n")
	}
	if req.TopP > 0 {
		body["top_p"] = req.TopP
	}
	if req.MaxTokens > 0 {
		body["max_output_tokens"] = req.MaxTokens
	}
	return body, true
}

// nativeUsage converts the v1 token accounting, including the reasoning share
// that the OpenAI-compatible namespace cannot report at all.
func nativeUsage(s *nativeStats) *Usage {
	if s == nil {
		return nil
	}
	return &Usage{
		PromptTokens:     s.InputTokens,
		CompletionTokens: s.TotalOutputTokens,
		TotalTokens:      s.InputTokens + s.TotalOutputTokens,
		ReasoningTokens:  s.ReasoningOutputTokens,
	}
}

// nativeStatsEvent converts the reported speed and derives the stop reason the
// route does not send. A request that ended exactly on its own cap is reported
// as "length", which is what tells Summarize's retry that the answer was cut off
// instead of merely short — the case a reasoning model hits when it thinks past
// its budget.
func nativeStatsEvent(s *nativeStats, maxTokens int) *GenerationStats {
	if s == nil {
		return nil
	}
	st := &GenerationStats{
		TokensPerSecond:    s.TokensPerSecond,
		TimeToFirstTokenMs: int64(s.TimeToFirstToken * 1000),
		Source:             SourceLMStudio,
	}
	if maxTokens > 0 && s.TotalOutputTokens >= maxTokens {
		st.StopReason = "length"
	}
	return st
}

func (p *openAICompatProvider) Chat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	if err := p.keyCheck(); err != nil {
		return nil, err
	}
	// Groq's per-request token budget is published on its responses and nowhere
	// else — its listing reports windows. Watching them is what turns the ceiling
	// from something a refusal teaches into something the first ordinary answer
	// does, so the second oversized request of a session is folded before it is sent.
	// Another backend's headers are not read: what this header means here is a
	// measurement of this vendor, and a guess about a different service's would fold
	// chats that need no folding.
	if p.modelsFlavour == flavourGroq {
		ctx = withResponseWatcher(ctx, requestBudget{provider: p, model: req.Model})
		// And, before the request goes out, the wait that keeps a long chat inside the
		// minute it is spending: the allowance this model has left, against what this
		// request is about to cost. It is reported on the same watcher a retry uses, so
		// it appears as a phase with the seconds the vendor's own header named.
		p.paceRequest(ctx, req)
	}
	// A request that explicitly asks not to think goes to LM Studio's native
	// route when the model publishes a reasoning setting: that route is the only
	// place the setting exists, and it is the difference between a summary and a
	// model that spends every token of its budget thinking (measured on
	// qwen3.8-4b-distill: 48 of 48 output tokens on reasoning, and no message
	// item at all, i.e. content empty).
	if p.nativeChatPath != "" && req.Think != nil && !*req.Think && expressibleOnNative(req) {
		if setting, ok := p.reasoningSettingFor(ctx, req.Model); ok {
			if nativeBody, buildable := nativeV1Request(req, setting); buildable {
				if rc, err := postStream(ctx, p.nativeChatPath, p.authHeaders(), nativeBody); err == nil {
					events := make(chan StreamEvent, 64)
					go p.consumeNative(ctx, rc, events, req.Model, req.MaxTokens)
					return events, nil
				} else {
					// The route failed before a single token, so nothing is lost by
					// falling through: an optimisation must never be the reason a
					// compaction fails. It is also the one place a stale capability
					// answer shows up, so it is retired here.
					p.retireReasoning(err, req.Model)
				}
			}
		}
	}
	body := map[string]any{
		"model":       req.Model,
		"messages":    toWireMessages(req.Messages, p.ReplaysReasoning(len(req.Tools) > 0)),
		"stream":      true,
		"temperature": req.Temperature,
		// OpenAI-compatible servers omit usage from a stream unless it is asked
		// for. Without prompt_tokens the context meter could never show a
		// measured value, only an estimate, so this is requested explicitly.
		"stream_options": map[string]any{"include_usage": true},
	}
	if req.TopP > 0 {
		body["top_p"] = req.TopP
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Seed != nil {
		// The vendor's own name for it: Mistral calls a pinned seed
		// `random_seed` and has no `seed` property at all.
		body[p.seedKey()] = *req.Seed
	}
	// reasoningEffortSent records whether this request carried the
	// `reasoning_effort` field, so a vendor that refuses it can be answered with
	// one retry without the field instead of a failed turn.
	reasoningEffortSent := false
	if req.Think != nil && !*req.Think {
		switch {
		case p.chatTemplateKwargs && p.templateTogglesThinking(ctx):
			// llama.cpp reads the toggle out of the chat template's parameters
			// and rejects a parameter the loaded template does not use, so it is
			// only sent once the template has been seen to name it.
			for k, v := range llamaCppThinkingOff() {
				body[k] = v
			}
		case p.thinkingToggle:
			// LM Studio has no dedicated reasoning parameter either, but its
			// request accepts the in-body `enable_thinking` toggle that
			// Qwen-style chat templates read. Without it a reasoning model
			// spends the whole output budget thinking and returns no summary at
			// all.
			body["enable_thinking"] = false
		case p.reasoningEffortOff && !p.reasoningEffortRetired():
			// Mistral documents a field of its own for exactly this request:
			// `"none"` disables the reasoning effort. Only a model the caller
			// asked to silence ever gets here, and the caller derives that from
			// the model's own `capabilities.reasoning`, so the field cannot
			// reach a model that has no reasoning to switch off.
			body[reasoningEffortField] = reasoningEffortNone
			reasoningEffortSent = true
		}
	}
	if len(req.Tools) > 0 {
		tools := make([]wireTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			var wt wireTool
			wt.Type = "function"
			wt.Function.Name = t.Name
			wt.Function.Description = t.Description
			wt.Function.Parameters = t.Schema
			tools = append(tools, wt)
		}
		body["tools"] = tools
	}

	path := p.currentChatPath()
	rc, err := postStream(ctx, path, p.authHeaders(), body)
	if err != nil {
		if he, ok := err.(*HTTPError); ok && he.StatusCode == 404 && len(p.chatPaths) > 1 {
			p.retireChatPath()
			rc, err = postStream(ctx, p.currentChatPath(), p.authHeaders(), body)
		}
		// An optimisation may never be the reason a turn fails: a vendor that
		// refuses `reasoning_effort` loses that field and nothing else, and the
		// request goes out again without it. Nothing has been streamed yet, so
		// the answer is unaffected and the price is one round trip, once.
		if err != nil && reasoningEffortSent && p.retireReasoningEffort(err) {
			delete(body, reasoningEffortField)
			rc, err = postStream(ctx, p.currentChatPath(), p.authHeaders(), body)
		}
		if err != nil {
			// A refusal states the ceiling that produced it, per model, where a
			// listing reports windows and not budgets. Keeping it is what lets the
			// next turn fold the history *before* it is refused instead of paying
			// for the same refusal again. The response's own header has already
			// been read above when it carried one, and noteRequestCeiling keeps the
			// tighter of the two, so the two routes cannot disagree about the
			// limit they are describing.
			if too, ok := RequestTooLarge(err); ok {
				p.noteRequestCeiling(req.Model, Ceiling{Tokens: too.Limit, Source: CeilingFromRefusal})
				// A refusal that names the model's window is the only place a
				// deployment which publishes none ever states it (limits.go).
				p.noteContextWindow(req.Model, too.Window)
			}
			// The minute's request rate is stated nowhere else — the header family's
			// `requests` numbers are the day's — so a refusal that names one is the
			// only place it can be learned. Keeping it is what turns a tool-heavy
			// turn's next request from something to be refused into something to
			// wait for (pace.go).
			if limit, ok := requestsPerMinuteLimit(err); ok {
				p.noteRequestRate(req.Model, limit)
			}
			return nil, err
		}
	}

	events := make(chan StreamEvent, 64)
	go p.consume(ctx, rc, events, req.Model)
	return events, nil
}

// consume translates the SSE body into StreamEvents and closes the channel.
func (p *openAICompatProvider) consume(ctx context.Context, rc io.ReadCloser, events chan<- StreamEvent, model string) {
	defer close(events)
	defer rc.Close()

	start := time.Now()
	var (
		ttft          time.Duration
		sawFirstToken bool
		finishReason  string
		usage         *Usage
		stats         *GenerationStats
		ctxLength     int
		arch, quant   string
		textLen       int
		timings       *llamaCppTimings
	)

	toolCalls := map[int]*ToolCall{}

	send := func(ev StreamEvent) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	reader := newSSEReader(rc)
	for {
		if ctx.Err() != nil {
			return
		}
		payload, err := reader.Next()
		if err != nil {
			// Only a clean end of the body finishes the answer. Anything else — a
			// reset connection, a truncated body — used to end the stream as if the
			// model had stopped, which stores half a reply as a complete one.
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				send(StreamEvent{Type: EventError, Error: fmt.Sprintf("Die Verbindung wurde unterbrochen: %s", err)})
				return
			}
			break
		}
		if len(payload) == 0 {
			continue
		}
		if string(payload) == "[DONE]" {
			break
		}
		var chunk openAIChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			// A single malformed frame should not abort a long generation.
			continue
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			send(StreamEvent{Type: EventError, Error: chunk.Error.Message})
			return
		}
		if chunk.Usage != nil {
			u := &Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
			}
			if chunk.Usage.PromptTokensDetails != nil {
				u.CachedPromptTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			}
			// The top-level spelling wins where a vendor sends both: it is the field
			// that vendor documents, and a stale or zeroed `cached_tokens` beside it
			// would otherwise hide every hit.
			if chunk.Usage.PromptCacheHitTokens > 0 {
				u.CachedPromptTokens = chunk.Usage.PromptCacheHitTokens
			}
			if chunk.Usage.CompletionTokensDetails != nil {
				u.ReasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
			}
			usage = u
		}
		if chunk.Stats != nil {
			stats = &GenerationStats{
				TokensPerSecond: chunk.Stats.TokensPerSecond,
				// LM Studio reports these in seconds.
				TimeToFirstTokenMs: int64(chunk.Stats.TimeToFirstToken * 1000),
				GenerationMs:       int64(chunk.Stats.GenerationTime * 1000),
				StopReason:         chunk.Stats.StopReason,
				Source:             SourceLMStudio,
			}
		}
		if chunk.Timings != nil {
			timings = chunk.Timings
		}
		if chunk.ModelInfo != nil {
			ctxLength = chunk.ModelInfo.ContextLength
			arch = chunk.ModelInfo.Arch
			quant = chunk.ModelInfo.Quant
		}

		for _, choice := range chunk.Choices {
			delta := choice.Delta
			if choice.Message.Content.Text != "" || choice.Message.Content.Reasoning != "" ||
				choice.Message.Reasoning != "" ||
				choice.Message.ReasoningContent != "" || choice.Message.Thinking != "" ||
				len(choice.Message.ToolCalls) > 0 {
				// Non-streaming server: treat the full message as one delta.
				delta = mergeDeltas(delta, choice.Message)
			}

			if r := delta.reasoningText(); r != "" {
				if !sawFirstToken {
					ttft = time.Since(start)
					sawFirstToken = true
				}
				if !send(StreamEvent{Type: EventReasoning, Text: r}) {
					return
				}
			}
			if delta.Content.Text != "" {
				if !sawFirstToken {
					ttft = time.Since(start)
					sawFirstToken = true
				}
				textLen += len(delta.Content.Text)
				if !send(StreamEvent{Type: EventDelta, Text: delta.Content.Text}) {
					return
				}
			}
			for _, tc := range delta.ToolCalls {
				entry, ok := toolCalls[tc.Index]
				if !ok {
					entry = &ToolCall{}
					toolCalls[tc.Index] = entry
				}
				if tc.ID != "" {
					entry.ID = tc.ID
				}
				if tc.Function.Name != "" {
					// The name arrives as fragments, so it is accumulated — except when a
					// server repeats the whole name in every delta, where appending would
					// produce "search_websearch_web" and the call would no longer resolve.
					// A fragment that starts with what we have is the full name again.
					if strings.HasPrefix(tc.Function.Name, entry.Name) {
						entry.Name = tc.Function.Name
					} else {
						entry.Name += tc.Function.Name
					}
				}
				entry.Arguments += tc.Function.Arguments
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
		}
	}

	// Emit accumulated tool calls in index order so the engine can execute them.
	indexes := make([]int, 0, len(toolCalls))
	for i := range toolCalls {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		tc := toolCalls[i]
		if tc.Name == "" {
			continue
		}
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d_%d", i, time.Now().UnixNano())
		}
		if tc.Arguments == "" {
			tc.Arguments = "{}"
		}
		if !send(StreamEvent{Type: EventToolCall, ToolCall: tc}) {
			return
		}
	}

	elapsed := time.Since(start)
	// llama.cpp measures the generation itself and reports it in `timings`, so
	// these numbers are the server's rather than our own stopwatch, and the
	// badge names the backend instead of admitting to an estimate. They replace
	// the estimate rather than sitting beside it.
	//
	// `prompt_tokens` already counts the cached part of the prompt — the server
	// sends `prompt_tokens_details.cached_tokens` for that — so `timings.cache_n`
	// needs no mapping of its own into the usage object.
	if timings != nil && timings.PredictedPerSecond > 0 {
		stats = &GenerationStats{
			TokensPerSecond: timings.PredictedPerSecond,
			// The decode loop timed itself; the client can only measure from
			// the request to the last byte, which includes the wait for the
			// first token and any queueing.
			GenerationMs: int64(timings.PredictedMs),
			Source:       SourceLlamaCpp,
		}
	}
	if usage == nil {
		// No usage from the server; derive a conservative estimate so the meter
		// still moves. Marked as estimated so the UI never presents it as fact.
		completion := EstimateTokens(strings.Repeat("x", textLen))
		usage = &Usage{CompletionTokens: completion, TotalTokens: completion}
	}
	if stats == nil || stats.TokensPerSecond == 0 {
		genMs := elapsed.Milliseconds()
		tps := 0.0
		// Measure in microseconds: a short or cached generation can complete
		// inside a single millisecond, and reporting 0 tokens/s for a response
		// that clearly produced tokens would be wrong.
		if micros := elapsed.Microseconds(); micros > 0 && usage.CompletionTokens > 0 {
			tps = float64(usage.CompletionTokens) / (float64(micros) / 1e6)
		}
		stats = &GenerationStats{
			TokensPerSecond:    tps,
			TimeToFirstTokenMs: ttft.Milliseconds(),
			GenerationMs:       genMs,
			StopReason:         finishReason,
			Source:             SourceEstimated,
		}
	} else if stats.TimeToFirstTokenMs == 0 {
		stats.TimeToFirstTokenMs = ttft.Milliseconds()
	}
	if stats.StopReason == "" {
		stats.StopReason = finishReason
	}
	stats.TotalMs = elapsed.Milliseconds()

	send(StreamEvent{Type: EventUsage, Usage: usage})
	if ctxLength > 0 {
		send(StreamEvent{Type: EventModelInfo, ModelInfo: &ModelInfo{
			ID:           model,
			Backend:      p.id,
			ContextLimit: ctxLength,
			Arch:         arch,
			Quantization: quant,
		}})
	}
	send(StreamEvent{Type: EventDone, Stats: stats})
}

// consumeNative translates LM Studio's native event stream — chat.start,
// reasoning.delta, message.delta, chat.end — into the very same StreamEvents the
// OpenAI-compatible path emits, so nothing downstream can tell the two apart.
func (p *openAICompatProvider) consumeNative(ctx context.Context, rc io.ReadCloser,
	events chan<- StreamEvent, model string, maxTokens int) {

	defer close(events)
	defer rc.Close()

	start := time.Now()
	var (
		ttft              time.Duration
		sawFirstToken     bool
		sawMessageDelta   bool
		sawReasoningDelta bool
		textLen           int
		usage             *Usage
		stats             *GenerationStats
	)

	send := func(ev StreamEvent) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	firstToken := func() {
		if !sawFirstToken {
			ttft = time.Since(start)
			sawFirstToken = true
		}
	}

	reader := newSSEReader(rc)
	for {
		if ctx.Err() != nil {
			return
		}
		payload, err := reader.Next()
		if err != nil {
			// Only a clean end of the body finishes the answer. A truncated
			// stream ends here too, but it is reported, because half a summary
			// stored as memory is worse than a failed compaction.
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				send(StreamEvent{Type: EventError, Error: fmt.Sprintf("Die Verbindung wurde unterbrochen: %s", err)})
				return
			}
			break
		}
		if len(payload) == 0 {
			continue
		}
		var ev nativeChatEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			// One malformed frame must not abort a long generation.
			continue
		}
		switch ev.Type {
		case "reasoning.delta":
			if ev.Content == "" {
				continue
			}
			sawReasoningDelta = true
			firstToken()
			if !send(StreamEvent{Type: EventReasoning, Text: ev.Content}) {
				return
			}
		case "message.delta":
			if ev.Content == "" {
				continue
			}
			// This is the event that only exists because the request asked not
			// to think: `reasoning.delta` carries the private deliberation that
			// used to consume the entire budget and leave this one empty.
			sawMessageDelta = true
			firstToken()
			textLen += len(ev.Content)
			if !send(StreamEvent{Type: EventDelta, Text: ev.Content}) {
				return
			}
		case "error":
			msg := "LM Studio meldete einen Fehler beim Streamen"
			if ev.Error != nil && ev.Error.Message != "" {
				msg = ev.Error.Message
			}
			send(StreamEvent{Type: EventError, Error: msg})
			return
		case "chat.end":
			// The final frame repeats the whole aggregated result, deltas and
			// all. Only the numbers are taken from it, so no text is emitted
			// twice — except when no delta of that kind ever arrived, where the
			// aggregate is the only copy there is.
			if ev.Result == nil {
				continue
			}
			for _, item := range ev.Result.Output {
				switch {
				case item.Type == "reasoning" && item.Content != "" && !sawReasoningDelta:
					sawReasoningDelta = true
					if !send(StreamEvent{Type: EventReasoning, Text: item.Content}) {
						return
					}
				case item.Type == "message" && item.Content != "" && !sawMessageDelta:
					sawMessageDelta = true
					firstToken()
					textLen += len(item.Content)
					if !send(StreamEvent{Type: EventDelta, Text: item.Content}) {
						return
					}
				}
			}
			usage = nativeUsage(ev.Result.Stats)
			stats = nativeStatsEvent(ev.Result.Stats, maxTokens)
		}
	}

	elapsed := time.Since(start)
	if usage == nil {
		// No numbers from the server; derive a conservative estimate so the meter
		// still moves, marked as estimated so it is never presented as fact.
		completion := EstimateTokens(strings.Repeat("x", textLen))
		usage = &Usage{CompletionTokens: completion, TotalTokens: completion}
	}
	if stats == nil {
		stats = &GenerationStats{Source: SourceEstimated}
	}
	if stats.TimeToFirstTokenMs == 0 {
		stats.TimeToFirstTokenMs = ttft.Milliseconds()
	}
	if stats.GenerationMs == 0 {
		// This route reports no generation time of its own; what is left after
		// the first token is the honest approximation of it.
		stats.GenerationMs = elapsed.Milliseconds() - stats.TimeToFirstTokenMs
	}
	if stats.TokensPerSecond == 0 && usage.CompletionTokens > 0 {
		if micros := elapsed.Microseconds(); micros > 0 {
			stats.TokensPerSecond = float64(usage.CompletionTokens) / (float64(micros) / 1e6)
		}
	}
	stats.TotalMs = elapsed.Milliseconds()

	send(StreamEvent{Type: EventUsage, Usage: usage})
	send(StreamEvent{Type: EventDone, Stats: stats})
}

func mergeDeltas(a, b openAIDelta) openAIDelta {
	// Adding an empty string is a no-op, so neither needs a guard.
	a.Content.Text += b.Content.Text
	a.Content.Reasoning += b.Content.Reasoning
	switch {
	case b.Reasoning != "":
		a.Reasoning += b.Reasoning
	case b.ReasoningContent != "":
		a.ReasoningContent += b.ReasoningContent
	case b.Thinking != "":
		a.Thinking += b.Thinking
	}
	if len(b.ToolCalls) > 0 {
		a.ToolCalls = append(a.ToolCalls, b.ToolCalls...)
	}
	return a
}
