package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// stubProvider is a Provider with no backend behind it, so a test can observe
// what the registry does to its providers without any network involved.
type stubProvider struct {
	id      string
	invalid int32
}

func (p *stubProvider) ID() string                                      { return p.id }
func (p *stubProvider) Name() string                                    { return p.id }
func (p *stubProvider) Health(context.Context) error                    { return nil }
func (p *stubProvider) ListModels(context.Context) ([]ModelInfo, error) { return nil, nil }
func (p *stubProvider) Chat(context.Context, ChatRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent)
	close(ch)
	return ch, nil
}

func (p *stubProvider) InvalidateCache() { atomic.AddInt32(&p.invalid, 1) }

// plainProvider implements Provider and nothing else, so the registry's optional
// capability assertion has to tolerate it.
type plainProvider struct{ id string }

func (p *plainProvider) ID() string                                      { return p.id }
func (p *plainProvider) Name() string                                    { return p.id }
func (p *plainProvider) Health(context.Context) error                    { return nil }
func (p *plainProvider) ListModels(context.Context) ([]ModelInfo, error) { return nil, nil }
func (p *plainProvider) Chat(context.Context, ChatRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent)
	close(ch)
	return ch, nil
}

// TestRegistryInvalidateReachesProviderCaches: a forced re-check must clear the
// providers' own caches as well as the merged catalogue, and must not trip over a
// provider that has no such cache.
func TestRegistryInvalidateReachesProviderCaches(t *testing.T) {
	reg := NewRegistry(nil)
	stub := &stubProvider{id: "stub"}
	reg.Register(stub)
	reg.Register(&plainProvider{id: "plain"})

	reg.Invalidate()

	if got := atomic.LoadInt32(&stub.invalid); got != 1 {
		t.Errorf("InvalidateCache called %d times, want 1", got)
	}
}

// TestOllamaRecheckSeesAChangedContextWindow covers the reason the button can be
// trusted on Ollama. Its /api/show detail is cached for ten minutes, so a model
// reloaded with a different num_ctx would otherwise keep reporting its previous
// window no matter how often the user re-checked.
func TestOllamaRecheckSeesAChangedContextWindow(t *testing.T) {
	var showCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"m","model":"m","details":{"family":"qwen35","parameter_size":"3B","quantization_level":"Q4_K_M"}}]}`)
		case "/api/ps":
			fmt.Fprint(w, `{"models":[]}`)
		case "/api/show":
			// The window the backend reports changes after the first read, which is
			// what a reload with a different num_ctx looks like from here.
			n := atomic.AddInt32(&showCalls, 1)
			limit := 4096
			if n > 1 {
				limit = 8192
			}
			fmt.Fprintf(w, `{"details":{"family":"qwen35"},"model_info":{"qwen35.context_length":%d},"capabilities":["tools"]}`, limit)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	provider := newOllamaProvider(srv.URL)
	ctx := context.Background()

	if got := ollamaContextLimit(t, provider, ctx); got != 4096 {
		t.Fatalf("first listing window = %d, want 4096", got)
	}
	// Still the cached number even though the backend has moved on: this is the
	// staleness the re-check exists to break.
	if got := ollamaContextLimit(t, provider, ctx); got != 4096 {
		t.Fatalf("second listing window = %d, want the cached 4096; the fixture no longer exercises the cache", got)
	}

	provider.InvalidateCache()

	if got := ollamaContextLimit(t, provider, ctx); got != 8192 {
		t.Errorf("window after InvalidateCache = %d, want the re-read 8192", got)
	}
}

func ollamaContextLimit(t *testing.T, p *ollamaProvider, ctx context.Context) int {
	t.Helper()
	models, err := p.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.ID == "m" {
			return m.ContextLimit
		}
	}
	t.Fatal("model m is missing from the listing")
	return 0
}

// TestLMStudioListingPrefersTheLoadedWindow pins the pair LM Studio reports. The
// maximum is what the model could use; the loaded length is what it is actually
// using, and the meter has to compare against the second or it promises room the
// model does not have. This is the difference between "128K" and the 8k a model
// was loaded with.
func TestLMStudioListingPrefersTheLoadedWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[
			{"id":"loaded-model","object":"model","type":"llm","state":"loaded",
			 "max_context_length":128000,"loaded_context_length":8192,"capabilities":["tool_use"]},
			{"id":"idle-model","object":"model","type":"llm","state":"not-loaded",
			 "max_context_length":131072,"capabilities":["tool_use"]}
		]}`)
	}))
	defer srv.Close()

	models, err := newLMStudioProvider(srv.URL, "").ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}

	loaded, ok := byID["loaded-model"]
	if !ok {
		t.Fatal("loaded-model missing from the listing")
	}
	if loaded.ContextLimit != 8192 {
		t.Errorf("loaded model window = %d, want the loaded 8192 rather than the maximum", loaded.ContextLimit)
	}
	if loaded.MaxContext != 128000 {
		t.Errorf("loaded model maximum = %d, want 128000", loaded.MaxContext)
	}

	// A model the backend has not loaded reports no loaded_context_length. The
	// maximum is the only number available then, and it stays the planning
	// assumption — the point here is that it is not what a *loaded* model returns.
	idle, ok := byID["idle-model"]
	if !ok {
		t.Fatal("idle-model missing from the listing")
	}
	if idle.ContextLimit != 131072 {
		t.Errorf("unloaded model window = %d, want the maximum 131072", idle.ContextLimit)
	}
}
