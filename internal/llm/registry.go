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
	Kind    string `json:"kind"` // lmstudio | ollama | custom | deepseek
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
}

// HealthStatus is a backend reachability report for the UI.
type HealthStatus struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	BaseURL string `json:"baseUrl"`
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
func (r *Registry) Health(ctx context.Context) []HealthStatus {
	providers := r.Providers()
	out := make([]HealthStatus, len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Add(1)
		go func(i int, p Provider) {
			defer wg.Done()
			cfg := r.configFor(p.ID())
			st := HealthStatus{ID: p.ID(), Name: p.Name(), Kind: cfg.Kind, BaseURL: cfg.BaseURL}
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
		}(i, p)
	}
	wg.Wait()
	return out
}

func (r *Registry) configFor(id string) BackendConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.configs {
		if c.ID == id {
			return c
		}
	}
	return BackendConfig{}
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
			results[i] = result{models: models}
		}(i, p)
	}
	wg.Wait()

	merged := []ModelInfo{}
	for _, res := range results {
		merged = append(merged, res.models...)
	}

	r.mu.Lock()
	r.catalog = merged
	r.catalogAt = time.Now()
	r.mu.Unlock()
	return append([]ModelInfo(nil), merged...)
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
// ones, pointing at their standard ports, and DeepSeek.
//
// DeepSeek ships **disabled**, unlike the local pair. It is a metered remote
// service that needs a key, so enabling it by default would probe a paid API on
// every health refresh without being asked, and — because a hosted model is
// ready the moment a key is accepted — its models would win the loaded-model
// race and quietly become the default for new chats. A user who wants it turns
// it on and pastes a key in Settings.
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
			ID:      "deepseek",
			Name:    "DeepSeek",
			Kind:    "deepseek",
			BaseURL: DeepSeekDefaultBaseURL,
			Enabled: false,
		},
	}
}
