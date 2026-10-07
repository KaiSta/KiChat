package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// deepSeekListing is the shape /models answers with, trimmed to the fields that
// matter. The two entries are the models the service actually ships: a
// text-and-image one that reasons, and a text-only one.
const deepSeekListing = `{"object":"list","data":[
	{"id":"deepseek-flash","object":"model","owned_by":"deepseek","name":"DeepSeek-V4.1-Flash",
	 "context_window":1048576,"max_output_tokens":393216,
	 "input_modalities":["text","image"],"output_modalities":["text"],
	 "effort":{"supported_levels":["low","high","max"],"default_level":"high"}},
	{"id":"deepseek-v4-pro","object":"model","owned_by":"deepseek","name":"DeepSeek-V4-Pro",
	 "context_window":1048576,"max_output_tokens":393216,
	 "input_modalities":["text"],"output_modalities":["text"],
	 "effort":{"supported_levels":["low","high","max"],"default_level":"high"}}
]}`

// TestDeepSeekListingCarriesWhatThePlanningNeeds is the reason DeepSeek gets a
// listing of its own rather than the lowest-common-denominator one: this API
// reports the context window, so a DeepSeek model can be planned against without
// the user typing an override for it.
func TestDeepSeekListingCarriesWhatThePlanningNeeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, deepSeekListing)
	}))
	defer srv.Close()

	models, err := newDeepSeekProvider("deepseek", "DeepSeek", srv.URL, "test-key").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if len(byID) != 2 {
		t.Fatalf("listed %d models, want 2: %+v", len(byID), models)
	}

	flash, ok := byID["deepseek-flash"]
	if !ok {
		t.Fatalf("deepseek-flash missing from the listing: %+v", models)
	}
	if flash.ContextLimit != 1048576 || flash.MaxContext != 1048576 {
		t.Errorf("flash windows = %d/%d, want 1048576 both", flash.ContextLimit, flash.MaxContext)
	}
	if flash.Backend != "deepseek" || flash.Publisher != "deepseek" {
		t.Errorf("flash backend/publisher = %q/%q", flash.Backend, flash.Publisher)
	}
	// A hosted model answers as soon as the request is authenticated, so it must
	// not be reported as awaiting a load: the picker greys those out and the
	// default-model choice skips them.
	if flash.State != StateLoaded {
		t.Errorf("flash state = %q, want %q", flash.State, StateLoaded)
	}
	// It takes images, so the row has to offer the attachment path.
	if flash.Type != "vlm" || !flash.Has(CapVision) {
		t.Errorf("flash type/caps = %q/%v, want a vision model", flash.Type, flash.Capabilities)
	}
	// Tools are the claim the app acts on: without it the native protocol is not
	// used at all, so a missing badge silently costs the model its tools.
	if !flash.Has(CapTools) {
		t.Errorf("flash does not claim tools: %v", flash.Capabilities)
	}
	if !flash.Has(CapThink) {
		t.Errorf("flash does not claim thinking: %v", flash.Capabilities)
	}

	pro := byID["deepseek-v4-pro"]
	if pro.Type != "llm" || pro.Has(CapVision) {
		t.Errorf("v4-pro type/caps = %q/%v, want a text-only model", pro.Type, pro.Capabilities)
	}
	if pro.ContextLimit != 1048576 {
		t.Errorf("v4-pro window = %d, want 1048576", pro.ContextLimit)
	}
}

// TestDeepSeekListingSkipsAnEntryItCannotRead keeps one malformed model from
// hiding the rest, the same way one dead backend does not hide a live one.
func TestDeepSeekListingSkipsAnEntryItCannotRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The first entry has a context window that is a string rather than a
		// number, which is what a listing that grows a field can look like.
		fmt.Fprint(w, `{"data":[{"id":"broken","context_window":"lots"},{"id":"fine","context_window":4096}]}`)
	}))
	defer srv.Close()

	models, err := newDeepSeekProvider("deepseek", "DeepSeek", srv.URL, "k").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "fine" {
		t.Fatalf("listing = %+v, want only the readable entry", models)
	}
	if models[0].ContextLimit != 4096 {
		t.Errorf("window = %d, want 4096", models[0].ContextLimit)
	}
}

// TestDeepSeekStreamParsesReasoningContent pins the field name this vendor uses.
// The shared delta parser accepts three spellings of reasoning; DeepSeek answers
// with `reasoning_content`, and if that case were ever dropped the thinking
// panel would go empty while the answer still arrived.
func TestDeepSeekStreamParsesReasoningContent(t *testing.T) {
	var (
		gotPath string
		gotAuth string
	)
	frames := []string{
		`{"choices":[{"delta":{"role":"assistant","content":null,"reasoning_content":""}}]}`,
		`{"choices":[{"delta":{"content":null,"reasoning_content":"Der Nutzer "}}]}`,
		`{"choices":[{"delta":{"content":null,"reasoning_content":"will ein Wort."}}]}`,
		`{"choices":[{"delta":{"content":"Hallo"}}]}`,
		`{"choices":[{"delta":{"content":", Welt"}}],"usage":{"prompt_tokens":31,"completion_tokens":7,"total_tokens":38}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, frame := range frames {
			fmt.Fprintf(w, "data: %s\n\n", frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	events, err := newDeepSeekProvider("deepseek", "DeepSeek", srv.URL, "test-key").
		Chat(context.Background(), ChatRequest{
			Model:    "deepseek-flash",
			Messages: []Message{{Role: RoleUser, Content: "Sag hallo"}},
		})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var (
		content   strings.Builder
		reasoning strings.Builder
		usage     *Usage
	)
	for ev := range events {
		switch ev.Type {
		case EventDelta:
			content.WriteString(ev.Text)
		case EventReasoning:
			reasoning.WriteString(ev.Text)
		case EventUsage:
			usage = ev.Usage
		case EventError:
			t.Fatalf("stream error: %s", ev.Error)
		}
	}

	if got, want := content.String(), "Hallo, Welt"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if got, want := reasoning.String(), "Der Nutzer will ein Wort."; got != want {
		t.Errorf("reasoning = %q, want %q", got, want)
	}
	if usage == nil || usage.PromptTokens != 31 || usage.CompletionTokens != 7 {
		t.Errorf("usage = %+v, want the numbers the stream reported", usage)
	}
	// A measured prompt_tokens is what the context meter needs, and only
	// stream_options.include_usage makes the API send it.
	if gotPath != "/chat/completions" {
		t.Errorf("chat path = %q, want the documented /chat/completions", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want the configured key", gotAuth)
	}
}

// TestDeepSeekFallsBackToTheV1Path covers the second configured route: the
// vendor documents /v1 as an alias of the same API, so it is what the request
// falls back to when the first path is not served.
func TestDeepSeekFallsBackToTheV1Path(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	events, err := newDeepSeekProvider("deepseek", "DeepSeek", srv.URL, "k").
		Chat(context.Background(), ChatRequest{Model: "deepseek-flash"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var content strings.Builder
	for ev := range events {
		if ev.Type == EventDelta {
			content.WriteString(ev.Text)
		}
	}
	if content.String() != "ok" {
		t.Errorf("content = %q, want the reply from the fallback path", content.String())
	}
}

// TestDeepSeekWithoutAKeyIsNotAttempted pins the deliberate refusal. The vendor
// answers an unauthenticated request with a long 401 body that never mentions
// where a key belongs, so nothing is sent and the missing credential is named
// instead. The server fails the test if it is reached at all.
func TestDeepSeekWithoutAKeyIsNotAttempted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the API was called without a key")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"Authentication Fails","type":"authentication_error"}}`)
	}))
	defer srv.Close()

	p := newDeepSeekProvider("deepseek", "DeepSeek", srv.URL, "")

	if _, err := p.ListModels(context.Background()); !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("ListModels error = %v, want ErrMissingAPIKey", err)
	}
	// The settings badge reports Health, so it has to carry both facts: the
	// backend is unusable, and why.
	if err := p.Health(context.Background()); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("Health error = %v, want ErrUnavailable wrapping ErrMissingAPIKey", err)
	}
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "deepseek-flash"}); !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("Chat error = %v, want ErrMissingAPIKey", err)
	}
}

// TestRegistryBuildsDeepSeekFromConfig covers the wiring: the kind selects the
// provider, an entry with no base URL still points at the vendor host, and a
// disabled entry contributes nothing to the catalogue.
func TestRegistryBuildsDeepSeekFromConfig(t *testing.T) {
	r := NewRegistry([]BackendConfig{{
		ID: "deepseek", Name: "DeepSeek", Kind: "deepseek", BaseURL: "", Enabled: true,
	}})
	p, ok := r.Provider("deepseek")
	if !ok {
		t.Fatal("the deepseek kind did not build a provider")
	}
	compat, ok := p.(*openAICompatProvider)
	if !ok {
		t.Fatalf("provider is %T, want the OpenAI-compatible implementation", p)
	}
	if compat.baseURL != DeepSeekDefaultBaseURL {
		t.Errorf("base URL = %q, want %q", compat.baseURL, DeepSeekDefaultBaseURL)
	}
	if !compat.requiresKey {
		t.Error("the DeepSeek provider does not require a key")
	}
	if got := compat.modelsFlavour; got != flavourDeepSeek {
		t.Errorf("models flavour = %v, want the DeepSeek listing", got)
	}

	off := NewRegistry([]BackendConfig{{
		ID: "deepseek", Name: "DeepSeek", Kind: "deepseek", Enabled: false,
	}})
	if _, ok := off.Provider("deepseek"); ok {
		t.Error("a disabled DeepSeek entry still built a provider")
	}
}

// TestShippedDeepSeekBackendIsOffByDefault pins a decision rather than a
// mechanic: DeepSeek is metered and remote, so it must not be probed or chosen on
// a fresh installation. A user who wants it enables it and adds a key.
func TestShippedDeepSeekBackendIsOffByDefault(t *testing.T) {
	var deepSeek *BackendConfig
	for i, c := range DefaultBackendConfigs() {
		if c.Kind == "deepseek" {
			deepSeek = &DefaultBackendConfigs()[i]
			if c.Enabled {
				t.Error("the shipped DeepSeek backend is enabled by default")
			}
			if c.BaseURL != DeepSeekDefaultBaseURL {
				t.Errorf("shipped base URL = %q, want %q", c.BaseURL, DeepSeekDefaultBaseURL)
			}
		}
	}
	if deepSeek == nil {
		t.Fatal("DeepSeek is not among the shipped backends")
	}
}
