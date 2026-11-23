package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCustomProviderChatPathsAreNotDuplicated: a base URL ending in /v1 used to
// make the primary and the fallback chat paths identical, so a 404 retired one
// and retried the exact same URL instead of trying the other shape.
func TestCustomProviderChatPathsAreNotDuplicated(t *testing.T) {
	cases := []struct {
		baseURL string
		want    []string
	}{
		{
			baseURL: "http://host:8080",
			want:    []string{"http://host:8080/v1/chat/completions", "http://host:8080/chat/completions"},
		},
		{
			baseURL: "http://host:8080/v1",
			want:    []string{"http://host:8080/v1/chat/completions"},
		},
	}
	for _, tc := range cases {
		p := newCustomProvider("custom", "Custom", tc.baseURL, "")
		if len(p.chatPaths) != len(tc.want) {
			t.Errorf("%s: chatPaths = %v, want %v", tc.baseURL, p.chatPaths, tc.want)
			continue
		}
		for i := range tc.want {
			if p.chatPaths[i] != tc.want[i] {
				t.Errorf("%s: chatPaths[%d] = %q, want %q", tc.baseURL, i, p.chatPaths[i], tc.want[i])
			}
		}
	}
}

// TestCustomListingUsesMaxModelLenWhenTheServerPublishesIt: the plain listing is
// shared by every OpenAI-compatible endpoint, and vLLM's shape states the window
// it is serving. Reading it means a custom endpoint pointed at vLLM needs no
// override for a number the server already sent; an entry without the field keeps
// zero, which is the state a bare OpenAI-compatible server is in.
func TestCustomListingUsesMaxModelLenWhenTheServerPublishesIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[
			{"id":"nvidia/nemotron-3-nano-30b","object":"model","max_model_len":262144},
			{"id":"plain/model","object":"model"}
		]}`)
	}))
	defer srv.Close()

	models, err := newCustomProvider("custom", "Custom", srv.URL, "").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if got := byID["nvidia/nemotron-3-nano-30b"].ContextLimit; got != 262144 {
		t.Errorf("context window = %d, want the published 262144", got)
	}
	if got := byID["plain/model"].ContextLimit; got != 0 {
		t.Errorf("context window = %d, want 0 when the server states none", got)
	}
}

// TestACustomEndpointRemembersTheWindowARefusalStated: a server whose listing
// publishes no window states it when a request is too large, and that sentence is
// the only automatic source for such a backend. The learned number tightens a
// listing that publishes a larger one — a deployment that rejects above 131072 must
// not be planned against the 262144 its card advertises — and leaves a model that
// was never refused at 0.
func TestACustomEndpointRemembersTheWindowARefusalStated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 131072 tokens. Please reduce the length of the messages or completion.","type":"BadRequestError","code":400}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[
			{"id":"vllm/model","object":"model","max_model_len":262144},
			{"id":"plain/model","object":"model"}
		]}`)
	}))
	defer srv.Close()

	p := newCustomProvider("custom", "Custom", srv.URL, "")
	if _, err := p.Chat(context.Background(), ChatRequest{
		Model:    "vllm/model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("Chat error = %v, want an oversized-request refusal", err)
	}

	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if got := byID["vllm/model"].ContextLimit; got != 131072 {
		t.Errorf("context window = %d, want the 131072 the refusal named", got)
	}
	if got := byID["plain/model"].ContextLimit; got != 0 {
		t.Errorf("context window for a model that was never refused = %d, want 0", got)
	}
}

// TestASettingsSaveKeepsTheWindowARefusalLearned: a settings save rebuilds every
// provider, and the window a refusal measured is a fact about the deployment rather
// than a setting. Carrying it across the rebuild keeps a theme change from making
// the next oversized request pay for the same number again; a changed address is a
// different deployment and starts with no window at all.
func TestASettingsSaveKeepsTheWindowARefusalLearned(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 131072 tokens. Please reduce the length of the messages or completion.","type":"BadRequestError","code":400}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"openai/gpt-oss-20b","object":"model","owned_by":"openai"}]}`)
	}))
	defer refusing.Close()

	configs := []BackendConfig{{
		ID: "nvidia", Name: "NVIDIA", Kind: "nvidia", BaseURL: refusing.URL, APIKey: "k", Enabled: true,
	}}
	r := NewRegistry(configs)
	provider, ok := r.Provider("nvidia")
	if !ok {
		t.Fatal("the nvidia kind built no provider")
	}
	if _, err := provider.Chat(context.Background(), ChatRequest{
		Model:    "openai/gpt-oss-20b",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("Chat error = %v, want an oversized-request refusal", err)
	}

	windowOf := func() int {
		for _, m := range r.ListModels(context.Background(), true) {
			if m.ID == "openai/gpt-oss-20b" {
				return m.ContextLimit
			}
		}
		return 0
	}

	r.Reconfigure(configs)
	if got := windowOf(); got != 131072 {
		t.Errorf("context window after the same-address rebuild = %d, want the learned 131072", got)
	}

	// A new address is a different deployment, even under the same backend id.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"openai/gpt-oss-20b","object":"model","owned_by":"openai"}]}`)
	}))
	defer other.Close()
	r.Reconfigure([]BackendConfig{{
		ID: "nvidia", Name: "NVIDIA", Kind: "nvidia", BaseURL: other.URL, APIKey: "k", Enabled: true,
	}})
	if got := windowOf(); got != 0 {
		t.Errorf("context window after the address changed = %d, want 0 (a different deployment)", got)
	}
}

// TestRegistryBuildsTheNativeLlamaCppProvider: the kind is wired, and it is the
// enriching one rather than a plain custom endpoint — which is the whole reason
// the kind exists. A config that fell through to newCustomProvider would still
// list models, so only the loaded window proves which provider was built.
func TestRegistryBuildsTheNativeLlamaCppProvider(t *testing.T) {
	srv, _ := llamaCppServer(t, llamaCppListing, llamaCppPropsBody, http.StatusOK)

	r := NewRegistry([]BackendConfig{{
		ID: "llamacpp", Name: "llama.cpp", Kind: "llamacpp", BaseURL: srv.URL, Enabled: true,
	}})
	p, ok := r.Provider("llamacpp")
	if !ok {
		t.Fatal("the llamacpp kind produced no provider")
	}
	if compat, ok := p.(*openAICompatProvider); !ok || compat.modelsFlavour != flavourLlamaCpp {
		t.Fatalf("provider = %T, want the native llama.cpp flavour", p)
	}

	catalog := r.ListModels(context.Background(), true)
	if len(catalog) != 1 {
		t.Fatalf("catalogue = %+v, want the one model the server lists", catalog)
	}
	if catalog[0].ContextLimit != 8192 || catalog[0].Backend != "llamacpp" {
		t.Errorf("catalogue entry = %+v, want the loaded 8192 window on the llamacpp backend", catalog[0])
	}
}

// TestEveryShippedBackendKindBuildsAProvider: a kind that Reconfigure has no case
// for is dropped silently — the user sees a backend that is enabled, healthy and
// yet has no models — so every kind the app ships is required to produce one.
func TestEveryShippedBackendKindBuildsAProvider(t *testing.T) {
	configs := DefaultBackendConfigs()
	for i := range configs {
		configs[i].Enabled = true
	}
	r := NewRegistry(configs)
	for _, c := range configs {
		if _, ok := r.Provider(c.ID); !ok {
			t.Errorf("shipped backend %q (kind %q) produced no provider: Reconfigure has no case for that kind", c.ID, c.Kind)
		}
	}
}
