package llm

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// llama.cpp ships `llama-server`, an OpenAI-compatible HTTP server that KiChat
// could already reach through the generic "custom endpoint" kind. That path is
// deliberately metadata-blind, and two things the app does depend on metadata it
// cannot get there:
//
//   - how large the context window actually is, which decides when compaction
//     has to run, and
//   - whether the model can be handed tools and images.
//
// llama.cpp publishes both, in a namespace of its own, so it gets a provider of
// its own. Every shape below was read off llama.cpp's server documentation
// (tools/server/README.md) rather than guessed; the field names are quoted.
//
// GET /props is the namespace's centre of gravity:
//
//	"default_generation_settings": { "n_ctx": 1024, ... }
//	"chat_template": "...", "chat_template_caps": {...},
//	"modalities": { "vision": false }, "model_path": "...", "build_info": "..."
//
// `n_ctx` is the window the model is loaded *with*, which is the number a prompt
// has to fit in. /v1/models cannot report it — it reports `meta.n_ctx_train`, the
// window the model was trained for. The difference is not academic: `-c,
// --ctx-size N` documents its default as `0, 0 = loaded from model`, so the
// loaded window is neither known client-side nor derivable from the GGUF. Only
// the server can say it. Reading `n_ctx_train` and calling it the context limit
// would promise a window the server will refuse.
//
// The rest follows from the same reading: `chat_template_caps` says whether the
// loaded template handles tools, `modalities.vision` whether it takes an image,
// and the final frame of a chat completion carries a `timings` object — measured
// tokens/second — so the speed badge can name llama.cpp instead of saying
// "estimated".
//
// One assumption worth recording, because it is the opposite of the obvious one:
// tools do **not** need `--jinja`. The flag reads
// `--jinja, --no-jinja (default: enabled)`, so a current build parses the model's
// chat template by default, and `chat_template_caps` is how it reports what that
// template can do.

// LlamaCppDefaultBaseURL is where llama-server listens when it is not told
// otherwise: `--host` defaults to 127.0.0.1 and `--port` to 8080. It is exported
// so a settings blob with the entry half-filled can be repaired to the same
// address the provider would use.
const LlamaCppDefaultBaseURL = "http://127.0.0.1:8080"

// newLlamaCppProvider builds the llama.cpp provider. An empty base URL falls back
// to the documented default, so a half-filled settings entry still works.
func newLlamaCppProvider(id, name, baseURL, apiKey string) *openAICompatProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = LlamaCppDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if id == "" {
		id = "llamacpp"
	}
	if name == "" {
		name = "llama.cpp"
	}
	// A base URL that already names /v1 must not be given it twice, and a base
	// without one needs the OpenAI path as the fallback: llama-server answers
	// both /v1/chat/completions and /chat/completions, and a 404 retires the
	// first permanently.
	chatPaths := []string{baseURL + "/v1/chat/completions"}
	if fallback := baseURL + "/chat/completions"; fallback != chatPaths[0] {
		chatPaths = append(chatPaths, fallback)
	}
	// /props and /v1/models hang off the server root, never off a /v1 base.
	root := strings.TrimSuffix(baseURL, "/v1")

	return &openAICompatProvider{
		id:      id,
		name:    name,
		baseURL: baseURL,
		apiKey:  apiKey,
		headers: map[string]string{},
		// /v1/models is the listing a direct server is documented to answer, and
		// the one that carries `meta` and `architecture`. It is also what a
		// router forwards the loaded instance's info into, so one path serves
		// both shapes.
		chatPaths:        chatPaths,
		modelsPath:       root + "/v1/models",
		openAIModelsPath: root + "/v1/models",
		propsPath:        root + "/props",
		modelsFlavour:    flavourLlamaCpp,
		// llama.cpp has no `enable_thinking` field of its own. It hands the
		// parameter to the chat template via `chat_template_kwargs`, which is
		// why the toggle is nested rather than top-level.
		chatTemplateKwargs: true,
	}
}

// llamaCppProps is the part of GET /props the app acts on.
type llamaCppProps struct {
	// DefaultGenerationSettings carries the loaded context window under `n_ctx`.
	DefaultGenerationSettings struct {
		NCtx int `json:"n_ctx"`
	} `json:"default_generation_settings"`
	// ChatTemplate is the model's own Jinja source. It is read for one thing:
	// whether the template mentions `enable_thinking` at all.
	ChatTemplate string `json:"chat_template"`
	// ChatTemplateCaps is read as a *map*, and that is the whole point.
	// llama.cpp's own documentation shows the field as `{}` for a template it
	// could not analyse; decoding that into a struct would turn "the server did
	// not say" into Go's `false` and silently withdraw tool support from a
	// server that never denied it. Absence and false are different answers.
	ChatTemplateCaps map[string]bool `json:"chat_template_caps"`
	Modalities       struct {
		Vision bool `json:"vision"`
	} `json:"modalities"`
	ModelPath  string `json:"model_path"`
	TotalSlots int    `json:"total_slots"`
	BuildInfo  string `json:"build_info"`
	// IsSleeping marks a server that unloaded its weights after
	// `--sleep-idle-seconds`. It is read here only so the field is on record; a
	// sleeping server wakes on its own when asked to answer.
	IsSleeping bool `json:"is_sleeping"`
}

// llamaCppModelEntry is one entry of llama.cpp's listing. A direct server
// answers with exactly one, whose `id` is the model file path unless `--alias`
// replaced it.
//
// `meta` is a pointer because the server sends null while a model is still
// loading — a zero-valued struct would be indistinguishable from a model with no
// vocabulary.
type llamaCppModelEntry struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
	Meta    *struct {
		NCtxTrain int `json:"n_ctx_train"`
		NParams   int `json:"n_params"`
		NVocab    int `json:"n_vocab"`
		Size      int `json:"size"`
	} `json:"meta"`
	Architecture *struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
}

// llamaCppTimings is the `timings` object llama.cpp attaches to a chat
// completion, alongside the standard `usage`. These numbers are measured by the
// server, which is what lets the badge name the backend rather than admit to an
// estimate.
type llamaCppTimings struct {
	CacheN             int     `json:"cache_n"`
	PromptN            int     `json:"prompt_n"`
	PromptMs           float64 `json:"prompt_ms"`
	PromptPerSecond    float64 `json:"prompt_per_second"`
	PredictedN         int     `json:"predicted_n"`
	PredictedMs        float64 `json:"predicted_ms"`
	PredictedPerSecond float64 `json:"predicted_per_second"`
}

func (p *openAICompatProvider) fromLlamaCppEntry(m llamaCppModelEntry) ModelInfo {
	info := ModelInfo{
		ID:      m.ID,
		Backend: p.id,
		Type:    "llm",
		// The server is started with `-m` and does not begin answering before
		// the weights are resident, so the model is loaded by definition.
		// Reporting "not loaded" the way a local model waiting for VRAM is
		// reported would grey the row out and keep it out of the default-model
		// choice, on a server that is plainly up.
		State: StateLoaded,
	}
	if m.Meta != nil {
		// Only the ceiling comes from here. `n_ctx_train` is the window the model
		// was trained for, and it is the *default* loaded window — `-c` unset
		// means "loaded from model" — but it is not the loaded one once a user
		// passes `-c 4096` to fit a small GPU. Reporting it as ContextLimit would
		// promise a window the server refuses and quietly truncate the prompt
		// from the front, losing the system prompt. ContextLimit is therefore
		// filled in from /props alone, which reports what the server actually
		// allocated; with no /props the app falls back to its own conservative
		// default, which is the safe way round.
		info.MaxContext = m.Meta.NCtxTrain
	}
	if m.Architecture != nil {
		for _, modality := range m.Architecture.InputModalities {
			if modality == "image" {
				info.Type = "vlm"
				info.Capabilities = withCap(info.Capabilities, CapVision)
				break
			}
		}
	}
	return info
}

// enrichLlamaCpp folds what /props knows into a listing only /v1/models could
// supply, and is a no-op for every other backend. A failure to read /props never
// fails the listing: the models are what the caller asked for, and everything
// here is an enrichment.
func (p *openAICompatProvider) enrichLlamaCpp(ctx context.Context, models []ModelInfo) []ModelInfo {
	if p.modelsFlavour != flavourLlamaCpp || p.propsPath == "" {
		return models
	}
	props := p.propsFor(ctx)
	if props == nil {
		return models
	}
	// Only an explicit false withholds tools. An absent key means the server did
	// not report on the template, and llama.cpp's own default for the field is
	// true. The app answers a missing tool claim by never using the native tool
	// protocol, so guessing "no" would cost a working model its tools silently;
	// guessing "yes" costs nothing when it is wrong, because such a model simply
	// answers in prose.
	supportsTools := true
	if v, ok := props.ChatTemplateCaps["supports_tool_calls"]; ok {
		supportsTools = v
	}
	thinking := llamaCppTemplateTogglesThinking(props.ChatTemplate)
	for i := range models {
		if props.DefaultGenerationSettings.NCtx > 0 {
			// The window the model is loaded with is the one a prompt has to fit
			// in, and the whole reason this provider exists: `-c` defaults to
			// 0 = "loaded from model", so nothing else can report it.
			models[i].ContextLimit = props.DefaultGenerationSettings.NCtx
		}
		if supportsTools {
			models[i].Capabilities = withCap(models[i].Capabilities, CapTools)
		}
		if thinking {
			models[i].Capabilities = withCap(models[i].Capabilities, CapThink)
		}
		// /props reports vision as a plain boolean where the listing reports it
		// as an input modality. Both are read, because either may be the one the
		// build in front of us fills in.
		if props.Modalities.Vision {
			models[i].Type = "vlm"
			models[i].Capabilities = withCap(models[i].Capabilities, CapVision)
		}
	}
	return models
}

// propsFor returns the cached /props reading, fetching it once. A missing route
// (404) is remembered, because a build without /props will not grow one. Any
// other failure is deliberately not remembered, so a server that was merely
// still loading its model is asked again instead of being written off for the
// rest of the session.
func (p *openAICompatProvider) propsFor(ctx context.Context) *llamaCppProps {
	p.mu.Lock()
	if p.propsLoaded {
		cached := p.props
		p.mu.Unlock()
		return cached
	}
	p.mu.Unlock()

	var props llamaCppProps
	loaded := &props
	err := doJSON(ctx, jsonClient, http.MethodGet, p.propsPath, p.authHeaders(), nil, &props)
	if err != nil {
		var he *HTTPError
		if !errors.As(err, &he) || he.StatusCode != http.StatusNotFound {
			// Unhelpful but not absent: report nothing and cache nothing, so the
			// next listing tries again.
			return nil
		}
		loaded = nil
	}

	p.mu.Lock()
	p.props = loaded
	p.propsLoaded = true
	p.mu.Unlock()
	return loaded
}

// llamaCppThinkingParam is the chat template parameter that switches thinking on
// and off. It is a named constant because two places have to agree on it: the
// template scan that decides whether the parameter is accepted at all, and the
// request fragment that sends it.
const llamaCppThinkingParam = "enable_thinking"

// llamaCppTemplateTogglesThinking reports whether the loaded chat template reads
// the thinking parameter.
//
// It is the guard on sending `chat_template_kwargs` at all. llama.cpp validates
// the parameters a request hands to the template, so offering one the template
// never reads is a rejected request rather than an ignored extra — a hard-coded
// toggle would break every model whose template does not have it. Reading the
// template source is the cheap way to know: a template that honours the
// parameter has to name it.
func llamaCppTemplateTogglesThinking(template string) bool {
	return strings.Contains(template, llamaCppThinkingParam)
}

// llamaCppThinkingOff builds the request fragment that asks the template not to
// think. It is nested because that is llama.cpp's shape for template parameters;
// a top-level `enable_thinking` is LM Studio's.
func llamaCppThinkingOff() map[string]any {
	return map[string]any{
		"chat_template_kwargs": map[string]any{llamaCppThinkingParam: false},
	}
}

// templateTogglesThinking reports whether the model loaded behind this provider
// accepts the thinking parameter.
//
// A server that cannot be asked answers false, which is the safe way round: the
// request is then sent without the parameter rather than with one the template
// may reject. That also means an unreachable /props costs the "do not think"
// optimisation rather than the whole turn — compaction adapts its output budget
// for exactly this case.
func (p *openAICompatProvider) templateTogglesThinking(ctx context.Context) bool {
	if p.propsPath == "" {
		return false
	}
	props := p.propsFor(ctx)
	return props != nil && llamaCppTemplateTogglesThinking(props.ChatTemplate)
}

// withCap appends a capability unless it is already listed. Two sources report
// vision for the same model — the listing's input modalities and /props — and a
// duplicate would show up twice in the UI.
func withCap(caps []string, capability string) []string {
	for _, c := range caps {
		if c == capability {
			return caps
		}
	}
	return append(caps, capability)
}
