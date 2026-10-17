package llm

import (
	"context"
	"strings"
	"sync"
	"time"
)

// newCustomProvider builds a provider for any OpenAI-compatible server (llama.cpp,
// vLLM, a remote gateway). It has no native metadata namespace, so context
// limits have to come from the user's per-model overrides.
func newCustomProvider(id, name, baseURL, apiKey string) *openAICompatProvider {
	baseURL = strings.TrimRight(baseURL, "/")
	chatPaths := []string{baseURL + "/v1/chat/completions"}
	modelsPath := baseURL + "/v1/models"
	if strings.HasSuffix(baseURL, "/v1") {
		chatPaths = []string{baseURL + "/chat/completions"}
		modelsPath = baseURL + "/models"
	}
	// Add the bare OpenAI path as a fallback, unless the base URL already names
	// it: a base ending in /v1 makes the two identical, and a duplicate entry
	// would only retry the same 404.
	if fallback := baseURL + "/chat/completions"; chatPaths[len(chatPaths)-1] != fallback {
		chatPaths = append(chatPaths, fallback)
	}
	return &openAICompatProvider{
		id:               id,
		name:             name,
		baseURL:          baseURL,
		apiKey:           apiKey,
		headers:          map[string]string{},
		chatPaths:        chatPaths,
		modelsPath:       "", // no rich metadata namespace
		openAIModelsPath: modelsPath,
	}
}

// BackendConfig describes one configured backend.
type BackendConfig struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"` // lmstudio | ollama | custom | deepseek | mistral | nvidia | groq | llamacpp
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"apiKey"`
	Enabled bool   `json:"enabled"`
}

// ModelOverride lets the user correct or supply metadata a backend does not
// report — most importantly the context window of plain OpenAI-compatible
// servers.
type ModelOverride struct {
	ContextLimit int      `json:"contextLimit,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// RequestCeiling is the largest single request the user's account may send to
	// this model, in tokens, as they read it off the backend's own console — 0 when
	// they have not said.
	//
	// It exists because one backend publishes that number to a signed-in page and
	// nowhere a client can ask: Groq's per-request budget is the organization's TPM,
	// which appears in no listing and is stated on the wire only in a *response* — so
	// a chat that is already long has nothing to plan against until it has been
	// refused once. The docs' tables cannot stand in for it: the Rate Limits page and
	// the Models page disagree by more than an order of magnitude for the same models
	// (GROQ.md §6.2), and the exact figure for an organization is only on its Limits
	// page. A number the user typed there, and this app then plans against, is the
	// one source that is both authoritative and available before the first request.
	//
	// It only ever tightens: a ceiling the vendor states on the wire wins when it is
	// the smaller of the two, so a stale entry costs extra folding but never an
	// oversized request.
	RequestCeiling int `json:"requestCeiling,omitempty"`
}

// HealthStatus is a backend reachability report for the UI.
type HealthStatus struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	BaseURL string `json:"baseUrl"`
	// Enabled is false for a shipped backend the user has not switched on. It is
	// carried because the two states need different words: a backend that was
	// never contacted is not unreachable, and a panel that cannot tell them apart
	// tells the user to repair an address that was never asked.
	Enabled bool   `json:"enabled"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Models  int    `json:"models"`
}

// Registry owns the configured providers and a short-lived catalogue of every
// model across all of them.
type Registry struct {
	mu        sync.RWMutex
	providers []Provider
	configs   []BackendConfig
	catalog   []ModelInfo
	catalogAt time.Time
}

const catalogTTL = 30 * time.Second

// NewRegistry builds providers from configuration.
func NewRegistry(configs []BackendConfig) *Registry {
	r := &Registry{}
	r.Reconfigure(configs)
	return r
}

// Reconfigure swaps the provider set, e.g. after the user edits settings.
func (r *Registry) Reconfigure(configs []BackendConfig) {
	providers := make([]Provider, 0, len(configs))
	for _, c := range configs {
		if !c.Enabled {
			continue
		}
		switch c.Kind {
		case "lmstudio":
			p := newLMStudioProvider(c.BaseURL, c.APIKey)
			p.id = c.ID
			if c.Name != "" {
				p.name = c.Name
			}
			providers = append(providers, p)
		case "ollama":
			providers = append(providers, newOllamaProvider(c.BaseURL))
		case "custom":
			providers = append(providers, newCustomProvider(c.ID, c.Name, c.BaseURL, c.APIKey))
		case "deepseek":
			providers = append(providers, newDeepSeekProvider(c.ID, c.Name, c.BaseURL, c.APIKey))
		case "mistral":
			providers = append(providers, newMistralProvider(c.ID, c.Name, c.BaseURL, c.APIKey))
		case "nvidia":
			providers = append(providers, newNvidiaProvider(c.ID, c.Name, c.BaseURL, c.APIKey))
		case "groq":
			providers = append(providers, newGroqProvider(c.ID, c.Name, c.BaseURL, c.APIKey))
		case "llamacpp":
			providers = append(providers, newLlamaCppProvider(c.ID, c.Name, c.BaseURL, c.APIKey))
		}
	}
	r.mu.Lock()
	r.providers = providers
	r.configs = configs
	r.catalog = nil
	r.catalogAt = time.Time{}
	r.mu.Unlock()
}

// Register adds a provider directly rather than deriving it from configuration.
// Tests use it to substitute a scripted backend; it is also the extension point
// for provider types that need custom construction.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = append(r.providers, p)
	r.catalog = nil
	r.catalogAt = time.Time{}
}

// Configs returns the current backend configuration.
func (r *Registry) Configs() []BackendConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]BackendConfig(nil), r.configs...)
}

// Provider returns a provider by id.
func (r *Registry) Provider(id string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.providers {
		if p.ID() == id {
			return p, true
		}
	}
	return nil, false
}

// Providers returns the configured providers in order.
func (r *Registry) Providers() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Provider(nil), r.providers...)
}

// Health probes every backend concurrently.
// Health probes the enabled backends and reports one row per *configured* one,
// switched on or not.
//
// A disabled backend builds no provider, and leaving it out entirely is what the
// settings screen read as a failed probe: the row disappeared, the panel fell
// back to "unreachable", and a user who pasted a key into a shipped-but-off
// entry was told to repair an address that had never been contacted. The row is
// therefore reported with Enabled unset and no Error — nothing failed, because
// nothing was sent — the same way RuntimeStatus carries an entry for every id in
// the settings so a row with no model configured still has its state explained.
func (r *Registry) Health(ctx context.Context) []HealthStatus {
	configs := r.Configs()
	byID := map[string]Provider{}
	for _, p := range r.Providers() {
		byID[p.ID()] = p
	}

	out := make([]HealthStatus, len(configs))
	var wg sync.WaitGroup
	for i, cfg := range configs {
		name := cfg.Name
		if name == "" {
			if p, ok := byID[cfg.ID]; ok {
				name = p.Name()
			}
		}
		st := HealthStatus{
			ID:      cfg.ID,
			Name:    name,
			Kind:    cfg.Kind,
			BaseURL: cfg.BaseURL,
			Enabled: cfg.Enabled,
		}
		p, ok := byID[cfg.ID]
		if !ok {
			// Switched off, or a kind that builds no provider: there is no result
			// to report, only the state to explain.
			out[i] = st
			continue
		}
		wg.Add(1)
		go func(i int, p Provider, st HealthStatus) {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			if err := p.Health(probeCtx); err != nil {
				st.Error = err.Error()
			} else {
				st.OK = true
				if models, err := p.ListModels(probeCtx); err == nil {
					st.Models = len(models)
				}
			}
			out[i] = st
		}(i, p, st)
	}
	wg.Wait()
	return out
}

// ListModels returns the merged catalogue. A failing backend contributes nothing
// rather than failing the whole listing, so one dead server does not hide the
// others' models.
func (r *Registry) ListModels(ctx context.Context, forceRefresh bool) []ModelInfo {
	r.mu.RLock()
	fresh := !r.catalogAt.IsZero() && time.Since(r.catalogAt) < catalogTTL
	if fresh && !forceRefresh && r.catalog != nil {
		out := append([]ModelInfo(nil), r.catalog...)
		r.mu.RUnlock()
		return out
	}
	providers := append([]Provider(nil), r.providers...)
	r.mu.RUnlock()

	type result struct {
		models []ModelInfo
		// ok marks a provider that answered. It is what lets a listing in which
		// every backend failed avoid overwriting the catalogue that was working a
		// moment ago: caching the empty merge made one network blip hide every
		// model from the picker for the whole thirty-second window.
		ok bool
	}
	results := make([]result, len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Add(1)
		go func(i int, p Provider) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			models, err := p.ListModels(cctx)
			if err != nil {
				return
			}
			results[i] = result{models: models, ok: true}
		}(i, p)
	}
	wg.Wait()

	merged := []ModelInfo{}
	anyOK := false
	for _, res := range results {
		if !res.ok {
			continue
		}
		anyOK = true
		merged = append(merged, res.models...)
	}

	r.mu.Lock()
	if !anyOK && len(r.catalog) > 0 {
		// Nothing answered. The previous listing is still the best answer this
		// process has, so it is kept and returned rather than replaced by "no
		// models", which is what a down backend would otherwise look like.
		merged = append([]ModelInfo(nil), r.catalog...)
	}
	r.catalog = merged
	// The timestamp moves either way: a failed round must not make every caller
	// pay for another twenty-second attempt.
	r.catalogAt = time.Now()
	r.mu.Unlock()
	return append([]ModelInfo(nil), merged...)
}

// Snapshot returns the catalogue exactly as it was last read, without asking any
// backend. A budget preview uses it, because it has to answer immediately and a
// model listing is not worth a request there (see chat.Engine.resolveModelCached).
func (r *Registry) Snapshot() []ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]ModelInfo(nil), r.catalog...)
}

// Invalidate drops the cached catalogue so the next listing refetches, and
// clears any per-model cache inside the providers as well. Both are needed for
// an explicit re-check to report what the backend says right now: a context
// window that changed because the model was loaded, unloaded or reconfigured
// can otherwise be masked by a provider cache that outlives the catalogue.
func (r *Registry) Invalidate() {
	r.mu.Lock()
	r.catalog = nil
	r.catalogAt = time.Time{}
	providers := append([]Provider(nil), r.providers...)
	r.mu.Unlock()

	// Called outside the lock: a provider's cache has its own mutex, and holding
	// the registry lock across a network-free cache clear invites a deadlock the
	// moment one of them grows a call back into the registry.
	for _, p := range providers {
		if inv, ok := p.(CacheInvalidator); ok {
			inv.InvalidateCache()
		}
	}
}

// FindModel looks up a model in the catalogue.
func (r *Registry) FindModel(ctx context.Context, ref Ref) (ModelInfo, bool) {
	for _, m := range r.ListModels(ctx, false) {
		if m.ID == ref.Model && m.Backend == ref.Backend {
			return m, true
		}
	}
	return ModelInfo{}, false
}

// MergeOverrides applies user-supplied corrections on top of backend metadata.
// The map key is "backend/model".
func MergeOverrides(models []ModelInfo, overrides map[string]ModelOverride) []ModelInfo {
	if len(overrides) == 0 {
		return models
	}
	for i := range models {
		key := models[i].Backend + "/" + models[i].ID
		ov, ok := overrides[key]
		if !ok {
			continue
		}
		if ov.ContextLimit > 0 {
			models[i].ContextLimit = ov.ContextLimit
		}
		if len(ov.Capabilities) > 0 {
			models[i].Capabilities = ov.Capabilities
		}
	}
	return models
}

// DefaultBackendConfigs returns the backends KiChat ships with: the two local
// ones, pointing at their standard ports, plus DeepSeek, Mistral, NVIDIA and
// llama.cpp.
//
// The hosted services ship **disabled**, unlike the local pair, and for the same
// reason in every case: they are metered and need a key, so enabling one by
// default would probe a paid API on every health refresh without being asked,
// and — because a hosted model is ready the moment a key is accepted — its
// models would win the loaded-model race and quietly become the default for new
// chats. A user who wants one turns it on and pastes a key in Settings.
//
// They are offered rather than hidden because the entry is the only place the key
// can be pasted, and an entry that does not exist cannot be found. They differ in
// what they are for: DeepSeek for its reasoning models, Mistral for a European
// host whose smallest models are cheap enough to leave switched on, and NVIDIA
// for a catalogue that spans several vendors at once and whose keys are free to
// create.
//
// llama.cpp ships disabled as well, and for a different reason: 127.0.0.1:9931 is
// its documented default today, but it is a default rather than a port the server
// owns the way LM Studio owns 1234 or Ollama owns 11434. `llama-server` is
// normally started by hand, on a port the user picked (and on 8080 on any Docker
// setup, which maps it explicitly), so the same machine can host several of them
// and none of them has to be on the number this offers. The address is therefore
// offered rather than probed: a user who runs llama-server enables this entry and
// points it at the port they used.
func DefaultBackendConfigs() []BackendConfig {
	return []BackendConfig{
		{
			ID:      "lmstudio",
			Name:    "LM Studio",
			Kind:    "lmstudio",
			BaseURL: "http://127.0.0.1:1234",
			Enabled: true,
		},
		{
			ID:      "ollama",
			Name:    "Ollama",
			Kind:    "ollama",
			BaseURL: "http://127.0.0.1:11434",
			Enabled: true,
		},
		{
			ID:      "llamacpp",
			Name:    "llama.cpp",
			Kind:    "llamacpp",
			BaseURL: LlamaCppDefaultBaseURL,
			Enabled: false,
		},
		{
			ID:      "deepseek",
			Name:    "DeepSeek",
			Kind:    "deepseek",
			BaseURL: DeepSeekDefaultBaseURL,
			Enabled: false,
		},
		{
			ID:      "mistral",
			Name:    "Mistral",
			Kind:    "mistral",
			BaseURL: MistralDefaultBaseURL,
			Enabled: false,
		},
		{
			ID:      "nvidia",
			Name:    "NVIDIA",
			Kind:    "nvidia",
			BaseURL: NvidiaDefaultBaseURL,
			Enabled: false,
		},
		{
			ID:      "groq",
			Name:    "Groq",
			Kind:    "groq",
			BaseURL: GroqDefaultBaseURL,
			Enabled: false,
		},
	}
}
