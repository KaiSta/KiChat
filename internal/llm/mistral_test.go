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

// mistralListing is the shape /v1/models answers with, trimmed to the fields
// that matter but keeping the five kinds of entry the live listing mixes
// together: a chat model, an alias of that same model under another id, one whose
// capabilities it reports itself, an embedding model and an OCR model. The last
// two are why this provider may not treat the array as a chat catalogue —
// mistral-ocr reports function_calling true while being unable to hold a
// conversation, so the tool flag alone would let it into the picker. The alias
// pair is why the listing is read as a whole: on the real API 27 chat-capable ids
// are only 8 models, and a per-entry mapper would offer all of them.
const mistralListing = `{"object":"list","data":[
	{"id":"ministral-8b-latest","object":"model","owned_by":"mistralai","name":"ministral-8b-latest",
	 "capabilities":{"completion_chat":true,"function_calling":true,"reasoning":false,"vision":true},
	 "max_context_length":262144,"type":"base","aliases":["ministral-8b-2512"]},
	{"id":"ministral-8b-2512","object":"model","owned_by":"mistralai","name":"ministral-8b-latest",
	 "capabilities":{"completion_chat":true,"function_calling":true,"reasoning":false,"vision":true},
	 "max_context_length":262144,"type":"base","aliases":["ministral-8b-latest"]},
	{"id":"mistral-small-latest","object":"model","owned_by":"mistralai","name":"mistral-small-2603",
	 "capabilities":{"completion_chat":true,"function_calling":true,"reasoning":true,"vision":true},
	 "max_context_length":262144,"type":"base"},
	{"id":"magistral-small-latest","object":"model","owned_by":"mistralai","name":"mistral-small-2603",
	 "capabilities":{"completion_chat":true,"function_calling":true,"reasoning":true,"vision":true},
	 "max_context_length":262144,"type":"base"},
	{"id":"mistral-embed-2312","object":"model","owned_by":"mistralai",
	 "capabilities":{"completion_chat":false,"function_calling":false,"reasoning":false,"vision":false},
	 "max_context_length":8192,"type":"base"},
	{"id":"mistral-ocr-latest","object":"model","owned_by":"mistralai",
	 "capabilities":{"completion_chat":false,"function_calling":true,"reasoning":false,"vision":true},
	 "max_context_length":16384,"type":"base"}
]}`

// TestMistralListingOffersOnlyConversationModels is the reason this provider has
// a listing of its own. Mistral publishes 46 models in one array — embeddings,
// moderation, OCR, speech and transcription beside the chat models — and only
// some of them can answer. Offering the rest would put rows in the model picker
// that fail on send, and reporting tools for them would be worse: an OCR model
// advertises function_calling while being unable to hold a conversation at all.
func TestMistralListingOffersOnlyConversationModels(t *testing.T) {
	// The server serves what the real API serves, under /v1: the base URL the
	// test hands over is the bare host, so a provider that failed to complete it
	// would ask for /models and get a 404 here.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, mistralListing)
	}))
	defer srv.Close()

	models, err := newMistralProvider("mistral", "Mistral", srv.URL, "test-key").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if len(models) != 2 {
		t.Fatalf("listed %d models, want only the 2 that can answer: %+v", len(models), models)
	}
	for _, id := range []string{"mistral-embed-2312", "mistral-ocr-latest"} {
		if _, ok := byID[id]; ok {
			t.Errorf("%s was offered as a chat model", id)
		}
	}
	// The aliases collapse. `ministral-8b-2512` is the dated id of the same model
	// the entry above already offers, and `magistral-small-latest` is
	// `mistral-small-2603` — the real API publishes 27 chat ids for 8 models, and
	// offering every id would fill the picker with rows that do the same thing.
	for _, id := range []string{"ministral-8b-2512", "magistral-small-latest"} {
		if _, ok := byID[id]; ok {
			t.Errorf("alias %s got its own row; the model is already offered", id)
		}
	}
	// Which alias survives is the rolling one: a picker row should follow the
	// vendor's release, and the shortest such id is the family's own name.
	if _, ok := byID["ministral-8b-latest"]; !ok {
		t.Errorf("the rolling alias was dropped in favour of the dated id: %+v", models)
	}

	small, ok := byID["mistral-small-latest"]
	if !ok {
		t.Fatalf("mistral-small-latest missing from the listing: %+v", models)
	}
	if small.ContextLimit != 262144 || small.MaxContext != 262144 {
		t.Errorf("windows = %d/%d, want 262144 both", small.ContextLimit, small.MaxContext)
	}
	if small.Backend != "mistral" || small.Publisher != "mistralai" {
		t.Errorf("backend/publisher = %q/%q", small.Backend, small.Publisher)
	}
	// Hosted: it answers as soon as the request is authenticated, so it must not
	// be reported as awaiting a load — the picker greys those out and the
	// default-model choice skips them.
	if small.State != StateLoaded {
		t.Errorf("state = %q, want %q", small.State, StateLoaded)
	}
	if small.ID != "mistral-small-latest" {
		t.Errorf("the small family is offered as %q, want the rolling alias", small.ID)
	}
	if small.Type != "vlm" || !small.Has(CapVision) {
		t.Errorf("type/caps = %q/%v, want a vision model", small.Type, small.Capabilities)
	}
	if !small.Has(CapTools) {
		t.Errorf("no tools claimed: %v", small.Capabilities)
	}
	if !small.Has(CapThink) {
		t.Errorf("no thinking claimed: %v", small.Capabilities)
	}

	// The per-model answer is the point: this listing says which models reason,
	// and a model it does not list as reasoning must not be given the badge.
	ministral, ok := byID["ministral-8b-latest"]
	if !ok {
		t.Fatalf("ministral-8b-latest missing from the listing: %+v", models)
	}
	if ministral.Has(CapThink) {
		t.Errorf("ministral-8b-latest claims thinking: %v", ministral.Capabilities)
	}
	if !ministral.Has(CapTools) || !ministral.Has(CapVision) {
		t.Errorf("ministral-8b-latest caps = %v, want tools and vision", ministral.Capabilities)
	}
}

// TestMistralStreamReadsThinkingChunksFromContent pins the shape that makes a
// Mistral integration more than a base URL. A reasoning model here does not send
// its thinking in a field of the delta — `delta.content` becomes a *list of typed
// chunks* and the thinking rides inside it:
//
//	{"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"…"}]}]}}
//
// The shared delta struct used to have a string here, which made json.Unmarshal
// fail on every frame of such an answer; the stream loop tolerates a malformed
// frame by skipping it, so the reply arrived empty with no error to explain it.
func TestMistralStreamReadsThinkingChunksFromContent(t *testing.T) {
	frames := []string{
		// The first frame carries the role and an empty content.
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		// Thinking, as a nested text-chunk list.
		`{"choices":[{"index":0,"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"Sieben mal "}]}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"drei ist einundzwanzig."}]}]},"finish_reason":null}]}`,
		// A text chunk in one and the same list.
		`{"choices":[{"index":0,"delta":{"content":[{"type":"text","text":"21"}]},"finish_reason":null}]}`,
		// A chunk type this app does not know, whose own `content` is a list —
		// with a text part beside it that must survive anyway. Decoding the
		// list as one typed slice fails on the citation and takes the "!" down
		// with it: the same whole-frame loss this type exists to prevent, one
		// level further in.
		`{"choices":[{"index":0,"delta":{"content":[{"type":"citation","content":[{"type":"text","text":"Quelle"}]},{"type":"text","text":"!"}]},"finish_reason":null}]}`,
		// The finish frame, with usage — Mistral sends it on the same frame that
		// carries finish_reason, and only stream_options.include_usage makes it
		// send it at all.
		`{"choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":9,"total_tokens":20}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
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

	events, err := newMistralProvider("mistral", "Mistral", srv.URL, "test-key").
		Chat(context.Background(), ChatRequest{
			Model:    "mistral-small-latest",
			Messages: []Message{{Role: RoleUser, Content: "Was ist 7*3?"}},
		})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var (
		content   strings.Builder
		reasoning strings.Builder
		usage     *Usage
		stats     *GenerationStats
	)
	for ev := range events {
		switch ev.Type {
		case EventDelta:
			content.WriteString(ev.Text)
		case EventReasoning:
			reasoning.WriteString(ev.Text)
		case EventUsage:
			usage = ev.Usage
		case EventDone:
			stats = ev.Stats
		case EventError:
			t.Fatalf("stream error: %s", ev.Error)
		}
	}

	if got, want := content.String(), "21!"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if got, want := reasoning.String(), "Sieben mal drei ist einundzwanzig."; got != want {
		t.Errorf("reasoning = %q, want %q", got, want)
	}
	// The measured prompt_tokens is what the context meter needs; without the
	// usage frame it could only ever show an estimate.
	if usage == nil || usage.PromptTokens != 11 || usage.CompletionTokens != 9 {
		t.Errorf("usage = %+v, want the numbers the stream reported", usage)
	}
	if stats == nil || stats.StopReason != "stop" {
		t.Errorf("stats = %+v, want the reported finish reason", stats)
	}
}

// TestMistralStreamKeepsPlainStringContent guards the other direction of the
// same change: the delta field now decodes a list, and it must still decode the
// plain string every other backend sends — including a frame that carries the
// thinking and the answer text together, where the order the user sees has to
// survive.
func TestMistralStreamKeepsPlainStringContent(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"content":"Hallo"}}]}`,
		`{"choices":[{"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"kurz überlegt"}]},{"type":"text","text":", Welt"}]}}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	events, err := newMistralProvider("mistral", "Mistral", srv.URL, "k").
		Chat(context.Background(), ChatRequest{Model: "ministral-3b-latest"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// The order of the emitted events is the contract: a single frame that
	// carries both kinds must emit the thinking first, so the reasoning panel
	// does not end up holding text the answer should have shown.
	var order []StreamEventType
	var content, reasoning strings.Builder
	for ev := range events {
		switch ev.Type {
		case EventReasoning:
			order = append(order, ev.Type)
			reasoning.WriteString(ev.Text)
		case EventDelta:
			order = append(order, ev.Type)
			content.WriteString(ev.Text)
		case EventError:
			t.Fatalf("stream error: %s", ev.Error)
		}
	}
	if got, want := content.String(), "Hallo, Welt"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if got, want := reasoning.String(), "kurz überlegt"; got != want {
		t.Errorf("reasoning = %q, want %q", got, want)
	}
	want := []StreamEventType{EventDelta, EventReasoning, EventDelta}
	if len(order) != len(want) {
		t.Fatalf("event order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("event order = %v, want %v", order, want)
		}
	}
}

// TestMistralBaseURLCompletionOnlyTouchesABareHost covers the address the vendor
// documents as the server (https://api.mistral.ai) against the one every route
// lives under (/v1). Exactly one shape is completed — a bare host with no path —
// because that is the mistake the vendor's own documentation sets up. A base that
// already carries a path is the user's own choice: appending /v1 to a gateway
// prefix would turn a working /openai into /openai/v1 and break an address that
// was right.
func TestMistralBaseURLCompletionOnlyTouchesABareHost(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", MistralDefaultBaseURL},
		{"https://api.mistral.ai", MistralDefaultBaseURL},
		{"https://api.mistral.ai/", MistralDefaultBaseURL},
		{MistralDefaultBaseURL, MistralDefaultBaseURL},
		{"http://proxy.lan:8080/mistral", "http://proxy.lan:8080/mistral"},
		{"https://gateway.example/v1/openai", "https://gateway.example/v1/openai"},
	}
	for _, tc := range cases {
		p := newMistralProvider("mistral", "Mistral", tc.in, "k")
		if p.baseURL != tc.want {
			t.Errorf("base URL %q became %q, want %q", tc.in, p.baseURL, tc.want)
		}
		if got, want := p.chatPaths[0], tc.want+"/chat/completions"; got != want {
			t.Errorf("chat path for %q = %q, want %q", tc.in, got, want)
		}
		if got, want := p.modelsPath, tc.want+"/models"; got != want {
			t.Errorf("models path for %q = %q, want %q", tc.in, got, want)
		}
	}
}

// TestMistralWithoutAKeyIsNotAttempted pins the deliberate refusal. The vendor
// answers an unauthenticated request with HTTP 401 and {"detail":"Invalid API
// Key"}, which says nothing about where a key belongs, so nothing is sent and the
// missing credential is named instead. The server fails the test if it is reached.
func TestMistralWithoutAKeyIsNotAttempted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the API was called without a key")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"detail":"Invalid API Key"}`)
	}))
	defer srv.Close()

	p := newMistralProvider("mistral", "Mistral", srv.URL, "")

	if _, err := p.ListModels(context.Background()); !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("ListModels error = %v, want ErrMissingAPIKey", err)
	}
	// The settings badge reports Health, so it has to carry both facts: the
	// backend is unusable, and why.
	if err := p.Health(context.Background()); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("Health error = %v, want ErrUnavailable wrapping ErrMissingAPIKey", err)
	}
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "ministral-3b-latest"}); !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("Chat error = %v, want ErrMissingAPIKey", err)
	}
}

// TestMistralStreamsAFragmentedToolCall keeps the tool claim honest: the listing
// says function_calling, and the provider's answer to that is that a tool call
// arriving as fragments is reassembled into one call the engine can execute.
func TestMistralStreamsAFragmentedToolCall(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"T4yroDKcT","type":"function","function":{"name":"get_","arguments":"{\"city\":\"Ber"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"weather","arguments":"lin\"}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	events, err := newMistralProvider("mistral", "Mistral", srv.URL, "k").Chat(context.Background(), ChatRequest{
		Model:    "ministral-3b-latest",
		Messages: []Message{{Role: RoleUser, Content: "Wetter in Berlin?"}},
		Tools: []ToolDefinition{{
			Name:   "get_weather",
			Schema: map[string]any{"type": "object"},
		}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var calls []ToolCall
	for ev := range events {
		switch ev.Type {
		case EventToolCall:
			calls = append(calls, *ev.ToolCall)
		case EventError:
			t.Fatalf("stream error: %s", ev.Error)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %+v, want exactly one reassembled call", calls)
	}
	if calls[0].Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", calls[0].Name)
	}
	if !strings.Contains(calls[0].Arguments, "Berlin") {
		t.Errorf("tool arguments = %q, want the city", calls[0].Arguments)
	}
	if calls[0].ID != "T4yroDKcT" {
		t.Errorf("tool id = %q, want the id the stream sent", calls[0].ID)
	}
}

// TestRegistryBuildsMistralFromConfig covers the wiring: the kind selects the
// provider, an entry with no base URL still points at the vendor host, and a
// disabled entry contributes nothing to the catalogue.
func TestRegistryBuildsMistralFromConfig(t *testing.T) {
	r := NewRegistry([]BackendConfig{{
		ID: "mistral", Name: "Mistral", Kind: "mistral", BaseURL: "", Enabled: true,
	}})
	p, ok := r.Provider("mistral")
	if !ok {
		t.Fatal("the mistral kind did not build a provider")
	}
	compat, ok := p.(*openAICompatProvider)
	if !ok {
		t.Fatalf("provider is %T, want the OpenAI-compatible implementation", p)
	}
	if compat.baseURL != MistralDefaultBaseURL {
		t.Errorf("base URL = %q, want %q", compat.baseURL, MistralDefaultBaseURL)
	}
	if !compat.requiresKey {
		t.Error("the Mistral provider does not require a key")
	}
	if got := compat.modelsFlavour; got != flavourMistral {
		t.Errorf("models flavour = %v, want the Mistral listing", got)
	}

	off := NewRegistry([]BackendConfig{{
		ID: "mistral", Name: "Mistral", Kind: "mistral", Enabled: false,
	}})
	if _, ok := off.Provider("mistral"); ok {
		t.Error("a disabled Mistral entry still built a provider")
	}
}

// TestShippedMistralBackendIsOffByDefault pins a decision rather than a
// mechanic: Mistral is metered and remote, so it must not be probed or chosen on
// a fresh installation. A user who wants it enables it and pastes a key.
func TestShippedMistralBackendIsOffByDefault(t *testing.T) {
	var mistral *BackendConfig
	for i, c := range DefaultBackendConfigs() {
		if c.Kind == "mistral" {
			mistral = &DefaultBackendConfigs()[i]
			if c.Enabled {
				t.Error("the shipped Mistral backend is enabled by default")
			}
			if c.BaseURL != MistralDefaultBaseURL {
				t.Errorf("shipped base URL = %q, want %q", c.BaseURL, MistralDefaultBaseURL)
			}
		}
	}
	if mistral == nil {
		t.Fatal("Mistral is not among the shipped backends")
	}
}
