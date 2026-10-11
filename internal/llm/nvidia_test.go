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

// nvidiaListing is the shape /v1/models answers with, byte for byte: four fields
// per entry and no more. `created` is the same placeholder on every entry in the
// real listing (735790403), which is why the provider does not read it.
const nvidiaListing = `{"object":"list","data":[
	{"id":"openai/gpt-oss-20b","object":"model","created":735790403,"owned_by":"openai"},
	{"id":"meta/llama-3.2-11b-vision-instruct","object":"model","created":735790403,"owned_by":"meta"},
	{"id":"nvidia/nv-embedqa-mistral-7b-v2","object":"model","created":735790403,"owned_by":"nvidia"}
]}`

// TestNvidiaListingCarriesWhatThePlanningNeeds pins what the sparse listing does
// and does not say.
//
// The listing reports no context window, so `ContextLimit` stays zero — the state
// the app already handles for a plain OpenAI-compatible server, where the
// per-model override in Settings is where the number comes from. It reports no
// capability object either, so tools are claimed for every model (the same
// unconditional claim DeepSeek makes, on the same reasoning: the app only uses
// the native tool protocol for a model that claims it) while thinking and vision
// are not claimed at all.
func TestNvidiaListingCarriesWhatThePlanningNeeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, nvidiaListing)
	}))
	defer srv.Close()

	models, err := newNvidiaProvider("nvidia", "NVIDIA", srv.URL, "test-key").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if len(byID) != 3 {
		t.Fatalf("listed %d models, want 3: %+v", len(byID), models)
	}

	chat, ok := byID["openai/gpt-oss-20b"]
	if !ok {
		t.Fatalf("the chat model is missing from the listing: %+v", models)
	}
	if chat.Backend != "nvidia" || chat.Publisher != "openai" {
		t.Errorf("backend/publisher = %q/%q, want nvidia/openai", chat.Backend, chat.Publisher)
	}
	// A hosted model answers as soon as the request is authenticated, so it must
	// not be reported as awaiting a load: the picker greys those out and the
	// default-model choice skips them.
	if chat.State != StateLoaded {
		t.Errorf("state = %q, want %q", chat.State, StateLoaded)
	}
	if !chat.Has(CapTools) {
		t.Errorf("no tool claim: %v", chat.Capabilities)
	}
	// The listing says nothing about the window, so nothing may be invented for
	// it. A made-up number would be planned against as if it were measured.
	if chat.ContextLimit != 0 || chat.MaxContext != 0 {
		t.Errorf("windows = %d/%d, want 0/0 (the listing does not report one)",
			chat.ContextLimit, chat.MaxContext)
	}
	// The catalogue mixes vision models with text-only ones and publishes no way
	// to tell them apart, so neither may be claimed: claiming vision for
	// everything would send images to a model that cannot read them, and claiming
	// it for nothing would hide the attachment path from the models that can.
	if chat.Has(CapVision) || chat.Has(CapThink) {
		t.Errorf("capabilities = %v, want tools only — this listing has no capability field",
			chat.Capabilities)
	}
	vision := byID["meta/llama-3.2-11b-vision-instruct"]
	if vision.Has(CapVision) || vision.Type != "llm" {
		t.Errorf("the vision model claims %v / type %q, want neither", vision.Capabilities, vision.Type)
	}
}

// TestNvidiaListingKeepsModelsTheAccountMayNotReach is the decision this provider
// makes and Mistral's does not, pinned so it cannot be quietly reversed.
//
// Measured on the live catalogue: 45 of its 80 entries answer HTTP 404 with
// {"status":404,...,"detail":"Function '…': Not found for account '…'"} — they are
// listed for everyone and callable only by the account entitled to them, and a
// different key reaches a different subset. Nothing in the listing separates
// those from the rest, so a filter written here could only be a guess, and the
// guess would hide a working model from the account that owns it. The embedding
// entry below is therefore offered, not dropped. Mistral's listing can drop what
// cannot chat because it publishes `capabilities.completion_chat`; this one
// publishes no such field.
func TestNvidiaListingKeepsModelsTheAccountMayNotReach(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, nvidiaListing)
	}))
	defer srv.Close()

	models, err := newNvidiaProvider("nvidia", "NVIDIA", srv.URL, "k").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	found := false
	for _, m := range models {
		if m.ID == "nvidia/nv-embedqa-mistral-7b-v2" {
			found = true
			// It is offered under its publisher and as a ready model, like any
			// other row; its unsuitability is the vendor's to report at send
			// time, not this app's to assume.
			if m.Publisher != "nvidia" || m.State != StateLoaded {
				t.Errorf("entry = %+v, want it offered like any other", m)
			}
		}
	}
	if !found {
		t.Fatalf("a catalogue entry was filtered out: %+v", models)
	}
}

// TestNvidiaListingSkipsEntriesItCannotRead keeps one malformed entry from
// hiding the rest, the same way one dead backend does not hide a live one. The
// id itself is what the list is for, so an entry without one is dropped rather
// than offered as a nameless row.
func TestNvidiaListingSkipsEntriesItCannotRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":42,"owned_by":"x"},{"owned_by":"nobody"},{"id":"fine/model","owned_by":"fine"}]}`)
	}))
	defer srv.Close()

	models, err := newNvidiaProvider("nvidia", "NVIDIA", srv.URL, "k").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "fine/model" {
		t.Fatalf("listing = %+v, want only the readable, named entry", models)
	}
}

// TestNvidiaStreamsReasoningWithoutDoublingIt pins the one shape on this API that
// is a trap rather than a difference: a reasoning model sends its thinking
// **twice in the same delta**, in `reasoning` and in `reasoning_content`, with
// identical text. The shared delta parser prefers `reasoning`, so the panel shows
// the words once. If it ever concatenated the fields instead, every reasoning
// model here would double its own thinking in the UI — and the answer text, which
// is what the user reads, would still look correct.
func TestNvidiaStreamsReasoningWithoutDoublingIt(t *testing.T) {
	frames := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning":"Denke nach.","reasoning_content":"Denke nach."},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning":" Zwei plus zwei.","reasoning_content":" Zwei plus zwei."},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Vier"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"."},"finish_reason":"stop"}]}`,
		// The usage frame: its `choices` array is empty, which is what this API
		// sends when include_usage is asked for.
		`{"choices":[],"usage":{"prompt_tokens":71,"completion_tokens":40,"total_tokens":111}}`,
	}
	srv := sseServer(t, "/v1/chat/completions", frames)
	defer srv.Close()

	events, err := newNvidiaProvider("nvidia", "NVIDIA", srv.URL, "k").Chat(context.Background(), ChatRequest{
		Model:    "openai/gpt-oss-20b",
		Messages: []Message{{Role: RoleUser, Content: "Was ist 2+2?"}},
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

	if got, want := content.String(), "Vier."; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if got, want := reasoning.String(), "Denke nach. Zwei plus zwei."; got != want {
		t.Errorf("reasoning = %q, want %q — the two fields carry the same text and must not be added together", got, want)
	}
	// The measured prompt_tokens is what the context meter needs; without the
	// usage frame it could only ever show an estimate.
	if usage == nil || usage.PromptTokens != 71 || usage.CompletionTokens != 40 {
		t.Errorf("usage = %+v, want the numbers the stream reported", usage)
	}
	if stats == nil || stats.StopReason != "stop" {
		t.Errorf("stats = %+v, want the reported finish reason", stats)
	}
}

// TestNvidiaStreamsAToolCall keeps the unconditional tool claim honest: the
// listing says nothing about function calling, so the claim rests on this shape
// being handled. The frames are the live one, which for this catalogue arrived
// as a single complete call rather than in fragments — the reassembly path is
// pinned against fragmentation anyway, because a model that streams its arguments
// in pieces would otherwise lose the call silently.
func TestNvidiaStreamsAToolCall(t *testing.T) {
	frames := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"4cf35b97","type":"function","function":{"name":"get_","arguments":"{\"city\":\"Ber"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"weather","arguments":"lin\"}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	srv := sseServer(t, "/v1/chat/completions", frames)
	defer srv.Close()

	events, err := newNvidiaProvider("nvidia", "NVIDIA", srv.URL, "k").Chat(context.Background(), ChatRequest{
		Model:    "z-ai/glm-5.3-flash",
		Messages: []Message{{Role: RoleUser, Content: "Wetter in Berlin?"}},
		Tools:    []ToolDefinition{{Name: "get_weather", Schema: map[string]any{"type": "object"}}},
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
}

// TestNvidiaAccountRefusalKeepsTheVendorDetail is the failure a user of this
// backend will meet first, so what reaches them is pinned.
//
// Most of the catalogue answers 404 with the vendor's own problem+json naming the
// account, not the model. That body is the only thing that tells this apart from
// a vanished model or a wrong address, and HTTPError carries it to the toast
// verbatim — the status alone would read as "the backend is broken".
func TestNvidiaAccountRefusalKeepsTheVendorDetail(t *testing.T) {
	const body = `{"status":404,"title":"Not Found","detail":"Function '04174188': Not found for account 'OCCn-x'"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	_, err := newNvidiaProvider("nvidia", "NVIDIA", srv.URL, "k").Chat(context.Background(), ChatRequest{
		Model:    "google/gemma-2b",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("a 404 was reported as a successful request")
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("error is %T (%v), want *HTTPError", err, err)
	}
	if he.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", he.StatusCode)
	}
	if !strings.Contains(he.Body, "Not found for account") {
		t.Errorf("body = %q, want the vendor's own wording kept", he.Body)
	}
}

// TestNvidiaBaseURLCompletionOnlyTouchesABareHost is the same rule as Mistral's,
// for the same reason: the vendor's console shows the bare host while every route
// lives under /v1, so that one shape is completed — and a base URL that already
// carries a path is the user's own choice, left exactly as it is so that appending
// /v1 to a gateway prefix cannot turn a working /openai into /openai/v1.
func TestNvidiaBaseURLCompletionOnlyTouchesABareHost(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", NvidiaDefaultBaseURL},
		{"https://integrate.api.nvidia.com", NvidiaDefaultBaseURL},
		{"https://integrate.api.nvidia.com/", NvidiaDefaultBaseURL},
		{NvidiaDefaultBaseURL, NvidiaDefaultBaseURL},
		// A self-hosted NIM container: the address the vendor documents for it
		// is its own, and this app must not "correct" it.
		{"http://localhost:8000/v1", "http://localhost:8000/v1"},
		{"http://proxy.lan:8080/nvidia", "http://proxy.lan:8080/nvidia"},
	}
	for _, tc := range cases {
		p := newNvidiaProvider("nvidia", "NVIDIA", tc.in, "k")
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

// TestNvidiaWithoutAKeyIsNotAttempted pins the deliberate refusal, the same one
// DeepSeek and Mistral make: nothing is sent without a credential, so the user is
// told what is missing instead of being handed the vendor's 401 body. The server
// fails the test if it is reached.
func TestNvidiaWithoutAKeyIsNotAttempted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the API was called without a key")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()

	p := newNvidiaProvider("nvidia", "NVIDIA", srv.URL, "")

	if _, err := p.ListModels(context.Background()); !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("ListModels error = %v, want ErrMissingAPIKey", err)
	}
	// The settings badge reports Health, so it has to carry both facts: the
	// backend is unusable, and why.
	if err := p.Health(context.Background()); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("Health error = %v, want ErrUnavailable wrapping ErrMissingAPIKey", err)
	}
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "openai/gpt-oss-20b"}); !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("Chat error = %v, want ErrMissingAPIKey", err)
	}
}

// TestRegistryBuildsNvidiaFromConfig covers the wiring: the kind selects the
// provider, an entry with no base URL still points at the vendor host, and a
// disabled entry contributes nothing to the catalogue.
func TestRegistryBuildsNvidiaFromConfig(t *testing.T) {
	r := NewRegistry([]BackendConfig{{
		ID: "nvidia", Name: "NVIDIA", Kind: "nvidia", BaseURL: "", Enabled: true,
	}})
	p, ok := r.Provider("nvidia")
	if !ok {
		t.Fatal("the nvidia kind did not build a provider")
	}
	compat, ok := p.(*openAICompatProvider)
	if !ok {
		t.Fatalf("provider is %T, want the OpenAI-compatible implementation", p)
	}
	if compat.baseURL != NvidiaDefaultBaseURL {
		t.Errorf("base URL = %q, want %q", compat.baseURL, NvidiaDefaultBaseURL)
	}
	if !compat.requiresKey {
		t.Error("the NVIDIA provider does not require a key")
	}
	if got := compat.modelsFlavour; got != flavourNvidia {
		t.Errorf("models flavour = %v, want the NVIDIA listing", got)
	}
	// This vendor's address and fields are its own; the shared builder's seed
	// name and thinking toggle must not have been borrowed by accident.
	if compat.seedField != "" || compat.reasoningEffortOff || compat.thinkingToggle || compat.chatTemplateKwargs {
		t.Errorf("the provider took another vendor's request names: %+v", compat)
	}

	off := NewRegistry([]BackendConfig{{
		ID: "nvidia", Name: "NVIDIA", Kind: "nvidia", Enabled: false,
	}})
	if _, ok := off.Provider("nvidia"); ok {
		t.Error("a disabled NVIDIA entry built a provider")
	}
}

// TestDefaultBackendsShipNvidiaOff is the settings half of the same rule DeepSeek
// and Mistral follow: the entry exists so a key can be pasted into it, and it
// ships switched off so nothing probes a metered API — or wins the loaded-model
// race for new chats — without being asked.
func TestDefaultBackendsShipNvidiaOff(t *testing.T) {
	var got *BackendConfig
	for _, c := range DefaultBackendConfigs() {
		if c.Kind == "nvidia" {
			got = &c
		}
	}
	if got == nil {
		t.Fatal("no NVIDIA entry ships, so there is nowhere to paste a key")
	}
	if got.Enabled {
		t.Error("the shipped NVIDIA entry is enabled")
	}
	if got.BaseURL != NvidiaDefaultBaseURL {
		t.Errorf("shipped address = %q, want %q", got.BaseURL, NvidiaDefaultBaseURL)
	}
	if got.Name != "NVIDIA" {
		t.Errorf("shipped name = %q, want NVIDIA", got.Name)
	}
}

// sseServer serves the given frames as an SSE stream on one path.
func sseServer(t *testing.T, path string, frames []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
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
	return srv
}
