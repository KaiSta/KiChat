package llm

import (
	"context"
	"net/http"
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
