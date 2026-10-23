package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// groqListing is the shape /openai/v1/models answers with, cut down from the
// measured listing: the same field names, the same kinds of value, and – the part
// that shapes this provider – entries that are not chat models at all.
const groqListing = `{"object":"list","data":[
	{"id":"whisper-large-v3","object":"model","created":1728413088,"owned_by":"OpenAI","active":true,"context_window":448,"public_apps":null,"name":"Whisper Large V3","input_modalities":["audio"],"output_modalities":["transcription"],"context_length":448,"max_output_length":448},
	{"id":"meta-llama/llama-prompt-guard-2-22m","object":"model","created":1747931134,"owned_by":"Meta","active":true,"context_window":512,"public_apps":null,"name":"Llama Prompt Guard 2 22M","input_modalities":["text"],"output_modalities":["text"],"supported_sampling_parameters":["temperature","top_p"],"supported_features":["json_mode"],"max_completion_tokens":512},
	{"id":"qwen/qwen3.8-27b","object":"model","created":1781169594,"owned_by":"Alibaba Cloud","active":true,"context_window":131072,"public_apps":null,"name":"Qwen3.8 27B","input_modalities":["text","image"],"output_modalities":["text"],"supported_features":["tools","json_mode","reasoning"],"max_completion_tokens":16384},
	{"id":"openai/gpt-oss-20b","object":"model","created":1754407957,"owned_by":"OpenAI","active":true,"context_window":131072,"public_apps":null,"model_card":"https://openai.com/index/gpt-oss-model-card","hugging_face_id":"openai/gpt-oss-20b","name":"GPT OSS 20B","input_modalities":["text"],"output_modalities":["text"],"supported_features":["tools","json_mode","structured_outputs","reasoning"],"max_completion_tokens":65536},
	{"id":"canopylabs/orpheus-v1-english","object":"model","created":1743530097,"owned_by":"Canopy Labs","active":true,"context_window":4000,"public_apps":null,"name":"Canopy Labs Orpheus V1 English","input_modalities":["text"],"output_modalities":["speech"],"max_completion_tokens":50000,"pricing":{"prompt":"0.000022","completion":"0"}},
	{"id":"allam-2-7b","object":"model","created":1738892122,"owned_by":"SDAIA","active":true,"context_window":4096,"public_apps":null,"name":"ALLaM 2.0 7B","input_modalities":["text"],"output_modalities":["text"],"supported_features":["json_mode"],"max_completion_tokens":4096},
	{"id":"some-retired-model","object":"model","created":1738892122,"owned_by":"OpenAI","active":false,"context_window":8192,"input_modalities":["text"],"output_modalities":["text"],"supported_features":["tools"]}
]}`

// TestGroqListingOffersOnlyWhatCanAnswer is the provider's whole reason for having
// a mapper of its own.
//
// Groq publishes, per model, the context window, the modalities and the features
// the vendor supports — so nothing here has to be claimed on a model's behalf and
// nothing has to be corrected by hand. The listing also mixes in models that are
// not chat models: measured, `whisper-large-v3` answers a chat request with HTTP
// 400 "does not support chat completions", and its `output_modalities` of
// `["transcription"]` is the vendor saying so in advance.
func TestGroqListingOffersOnlyWhatCanAnswer(t *testing.T) {
	models := groqList(t, groqListing)
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}

	// The two entries that cannot answer are dropped, and nothing else is.
	if _, ok := byID["whisper-large-v3"]; ok {
		t.Error("the Whisper transcription model was offered as a chat model")
	}
	if _, ok := byID["canopylabs/orpheus-v1-english"]; ok {
		t.Error("the text-to-speech model was offered as a chat model")
	}
	// A model the account cannot call at all is dropped too: `active` false is
	// the vendor's own answer, not a guess about the id.
	if _, ok := byID["some-retired-model"]; ok {
		t.Error("an inactive entry was offered")
	}
	if len(byID) != 4 {
		t.Fatalf("listed %d models, want the 4 chat models: %+v", len(byID), models)
	}
	// The prompt guard is a classifier, and it stays. Nothing in the listing says
	// what it is — its modalities are text to text, `active` is true and its
	// window is a real one — so hiding it would be a rule about its name. What
	// makes the row a trap is not in the listing either: measured, a call without
	// `stream` answers HTTP 200 with a score, and `stream: true` is refused with
	// HTTP 400 "text classification models do not support streaming". A row that
	// can only fail is worth less than the honesty of the mapping, and the live
	// test asserts that refusal so this stays a measurement rather than a memory.
	if _, ok := byID["meta-llama/llama-prompt-guard-2-22m"]; !ok {
		t.Error("a model whose modalities and features say it can chat was dropped")
	}

	oss := byID["openai/gpt-oss-20b"]
	if oss.Publisher != "OpenAI" || oss.State != StateLoaded || oss.Type != "llm" {
		t.Errorf("gpt-oss-20b = %+v, want publisher OpenAI, loaded, llm", oss)
	}
	// The window is published per model, which is the difference from
	// build.nvidia.com: no override is needed before this model can be planned
	// against, and the number must not be zeroed or invented.
	if oss.ContextLimit != 131072 || oss.MaxContext != 131072 {
		t.Errorf("gpt-oss-20b windows = %d/%d, want 131072/131072", oss.ContextLimit, oss.MaxContext)
	}
	if !oss.Has(CapTools) || !oss.Has(CapThink) || oss.Has(CapVision) {
		t.Errorf("gpt-oss-20b capabilities = %v, want tools and thinking, no vision", oss.Capabilities)
	}

	// Vision is read from the input modalities, not guessed from the id.
	qwen := byID["qwen/qwen3.8-27b"]
	if !qwen.Has(CapVision) || qwen.Type != "vlm" {
		t.Errorf("qwen = type %q caps %v, want vlm with vision", qwen.Type, qwen.Capabilities)
	}

	// A model whose features are json_mode alone claims no tools and no
	// thinking: on this API the listing is the authority, unlike NVIDIA's, where
	// the absence of any capability field is why tools are claimed for every row.
	allam := byID["allam-2-7b"]
	if allam.Has(CapTools) || allam.Has(CapThink) || allam.Has(CapVision) {
		t.Errorf("allam-2-7b capabilities = %v, want none of tools/thinking/vision", allam.Capabilities)
	}
	if allam.ContextLimit != 4096 {
		t.Errorf("allam-2-7b window = %d, want 4096", allam.ContextLimit)
	}
}

// TestGroqListingKeepsWhatTheListingDoesNotDescribe guards the other direction:
// only a present claim may drop a model or take a capability away. A listing that
// stopped publishing modalities must not empty the picker, the same rule the
// llama.cpp provider follows for a `false` it cannot tell from a field that was
// never sent.
func TestGroqListingKeepsWhatTheListingDoesNotDescribe(t *testing.T) {
	const bare = `{"object":"list","data":[{"id":"bare-model","owned_by":"OpenAI"}]}`
	models := groqList(t, bare)
	if len(models) != 1 {
		t.Fatalf("listed %d models, want the one entry kept: %+v", len(models), models)
	}
	m := models[0]
	if m.State != StateLoaded || m.ContextLimit != 0 || m.MaxContext != 0 {
		t.Errorf("entry = %+v, want a ready model with no invented window", m)
	}
	if len(m.Capabilities) != 0 {
		t.Errorf("capabilities = %v, want none: nothing was published to claim", m.Capabilities)
	}
}

// TestGroqBaseURLCompletionFollowsThisVendorsPath pins the one address trap of this
// backend. Every other hosted kind here completes a bare host with `/v1`; this API
// lives under `/openai/v1`, so the same rule would build `api.groq.com/v1`, which
// does not exist. A base URL that already carries a path is the user's own choice
// and is left alone.
func TestGroqBaseURLCompletionFollowsThisVendorsPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", GroqDefaultBaseURL},
		{"bare host", "https://api.groq.com", "https://api.groq.com/openai/v1"},
		{"bare host with slash", "https://api.groq.com/", "https://api.groq.com/openai/v1"},
		{"documented address", GroqDefaultBaseURL, GroqDefaultBaseURL},
		{"a gateway of the user's own", "https://gateway.lan/groq", "https://gateway.lan/groq"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newGroqProvider("groq", "Groq", tc.in, "k")
			if p.baseURL != tc.want {
				t.Errorf("base URL = %q, want %q", p.baseURL, tc.want)
			}
			if want := tc.want + "/chat/completions"; p.chatPaths[0] != want {
				t.Errorf("chat path = %q, want %q", p.chatPaths[0], want)
			}
		})
	}
}

// TestGroqRequestIsTheSharedOpenAIOne pins that this backend needs no request
// shape of its own: the deprecated `max_tokens` it still accepts is what the
// shared builder sends, `seed` is the vendor's own name for a pinned seed, and the
// usage frame the context meter needs is asked for explicitly.
func TestGroqRequestIsTheSharedOpenAIOne(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := newGroqProvider("groq", "Groq", srv.URL+"/openai/v1", "test-key")
	seed := 42
	stream, err := p.Chat(context.Background(), ChatRequest{
		Model:       "openai/gpt-oss-20b",
		Messages:    []Message{{Role: RoleUser, Content: "hi"}},
		MaxTokens:   64,
		Temperature: 0.2,
		Seed:        &seed,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for range stream {
	}
	if gotPath != "/openai/v1/chat/completions" {
		t.Errorf("path = %q, want the documented base plus /chat/completions", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want the bearer token", gotAuth)
	}
	for _, want := range []string{`"max_tokens":64`, `"seed":42`, `"include_usage":true`, `"stream":true`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body %s does not carry %s", gotBody, want)
		}
	}
}

// TestGroqNeverSeesAnImageOnAnAssistantTurn is a different fault from the same
// chat: a picture a tool generated is an inline attachment of the assistant row
// that asked for it, and that row's content is empty because it only made the
// call. The shared builder therefore sent `content: [{"type":"image_url",…}]` on
// an assistant message — a shape this vendor's schema does not have, refused with
// HTTP 400, "'messages.6.content.0.type' : value is not one of the allowed values
// ['text']". An assistant turn is a string plus its tool calls; a user turn keeps
// its image parts.
func TestGroqNeverSeesAnImageOnAnAssistantTurn(t *testing.T) {
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M8AAAMBAQDJ/pLvAAAAAElFTkSuQmCC"
	p := newGroqProvider("groq", "Groq", srv.URL+"/openai/v1", "test-key")
	stream, err := p.Chat(context.Background(), ChatRequest{
		Model: "qwen/qwen3.8-27b",
		Messages: []Message{
			{Role: RoleUser, Content: "zeichne einen Hund"},
			{Role: RoleAssistant, Images: []string{png},
				ToolCalls: []ToolCall{{ID: "call-1", Name: "generate_image", Arguments: "{}"}}},
			{Role: RoleTool, Name: "generate_image", ToolCallID: "call-1", Content: "Bild erstellt"},
			{Role: RoleUser, Content: "Was ist auf dem Bild?", Images: []string{png}},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for range stream {
	}

	if len(body.Messages) != 4 {
		t.Fatalf("request carried %d messages, want 4", len(body.Messages))
	}
	var assistant string
	if err := json.Unmarshal(body.Messages[1].Content, &assistant); err != nil {
		t.Fatalf("assistant content is not a string: %s", body.Messages[1].Content)
	}
	if strings.Contains(string(body.Messages[1].Content), "image_url") {
		t.Errorf("the assistant turn carried an image part: %s", body.Messages[1].Content)
	}
	var parts []map[string]any
	if err := json.Unmarshal(body.Messages[3].Content, &parts); err != nil {
		t.Fatalf("a user turn's picture was not sent as parts: %s", body.Messages[3].Content)
	}
	if len(parts) != 2 || parts[0]["type"] != "text" || parts[1]["type"] != "image_url" {
		t.Errorf("user parts = %v, want text then image_url", parts)
	}
}

// TestGroqWithoutAKeyIsReportedAsSuch keeps a new user's first screen readable:
// the vendor's 401 body is the same for a missing key as for a wrong one, so the
// missing case is answered locally instead.
func TestGroqWithoutAKeyIsReportedAsSuch(t *testing.T) {
	p := newGroqProvider("groq", "Groq", "", "")
	if _, err := p.ListModels(context.Background()); !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("ListModels error = %v, want ErrMissingAPIKey", err)
	}
	if err := p.Health(context.Background()); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("Health error = %v, want ErrUnavailable wrapping ErrMissingAPIKey", err)
	}
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "openai/gpt-oss-20b"}); !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("Chat error = %v, want ErrMissingAPIKey", err)
	}
}

// TestRegistryBuildsGroqFromConfig covers the wiring: the kind selects the provider,
// an entry with no base URL still points at the vendor host, and a disabled entry
// contributes nothing to the catalogue.
//
// It also pins what this provider must *not* have borrowed, which is the part with
// teeth here. `seed` is this vendor's own field name, so no `seedField` override may
// appear; and `reasoning_effort` has no off value on this API (measured: `"none"`
// is refused with HTTP 400), so a `reasoningEffortOff` or `thinkingToggle` set here
// would make every compaction that tries to silence thinking fail at the API rather
// than fall back to adapting its own output budget.
func TestRegistryBuildsGroqFromConfig(t *testing.T) {
	r := NewRegistry([]BackendConfig{{
		ID: "groq", Name: "Groq", Kind: "groq", BaseURL: "", Enabled: true,
	}})
	p, ok := r.Provider("groq")
	if !ok {
		t.Fatal("the groq kind did not build a provider")
	}
	compat, ok := p.(*openAICompatProvider)
	if !ok {
		t.Fatalf("provider is %T, want the OpenAI-compatible implementation", p)
	}
	if compat.baseURL != GroqDefaultBaseURL {
		t.Errorf("base URL = %q, want %q", compat.baseURL, GroqDefaultBaseURL)
	}
	if !compat.requiresKey {
		t.Error("the Groq provider does not require a key")
	}
	if got := compat.modelsFlavour; got != flavourGroq {
		t.Errorf("models flavour = %v, want the Groq listing", got)
	}
	if compat.seedField != "" || compat.reasoningEffortOff || compat.thinkingToggle || compat.chatTemplateKwargs {
		t.Errorf("the provider took another vendor's request names: %+v", compat)
	}

	off := NewRegistry([]BackendConfig{{
		ID: "groq", Name: "Groq", Kind: "groq", Enabled: false,
	}})
	if _, ok := off.Provider("groq"); ok {
		t.Error("a disabled Groq entry built a provider")
	}
}

// TestShippedGroqBackendIsOffByDefault is the settings half of the same rule
// DeepSeek, Mistral and NVIDIA follow: the entry exists so a key can be pasted into
// it, and it ships switched off so nothing probes a metered API — or, because a
// hosted model answers without a load, wins the loaded-model race and becomes the
// default for new chats without being asked.
func TestShippedGroqBackendIsOffByDefault(t *testing.T) {
	var got *BackendConfig
	for i, c := range DefaultBackendConfigs() {
		if c.Kind == "groq" {
			got = &DefaultBackendConfigs()[i]
			if c.Enabled {
				t.Error("the shipped Groq backend is enabled by default")
			}
		}
	}
	if got == nil {
		t.Fatal("no Groq entry ships, so there is nowhere to paste a key")
	}
	// The address is the part that is easy to get wrong for this vendor: every
	// other shipped host here is a bare origin plus /v1, and this one is not.
	if got.BaseURL != GroqDefaultBaseURL {
		t.Errorf("shipped address = %q, want %q", got.BaseURL, GroqDefaultBaseURL)
	}
	if got.Name != "Groq" {
		t.Errorf("shipped name = %q, want Groq", got.Name)
	}
}

// TestGroqRefusalCarriesTheVendorsOwnWords: the envelope is small and has a code,
// which is what the message has to carry — a user reading "Invalid API Key" knows
// where to look, where a bare status code does not say whose fault it is.
func TestGroqRefusalCarriesTheVendorsOwnWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"Invalid API Key","type":"invalid_request_error","code":"invalid_api_key"}}`)
	}))
	defer srv.Close()

	_, err := newGroqProvider("groq", "Groq", srv.URL, "wrong").ListModels(context.Background())
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("error is %T (%v), want *HTTPError", err, err)
	}
	if he.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", he.StatusCode)
	}
	if !strings.Contains(he.Body, "invalid_api_key") {
		t.Errorf("body = %q, want the vendor's code kept", he.Body)
	}
}

// TestGroqLearnsTheCeilingFromTheResponsesOwnHeaders is the fact that turns the
// per-request ceiling from something only a refusal teaches into something an ordinary
// answer does.
//
// Every response of this API carries `x-ratelimit-limit-tokens` — the tokens-per-minute
// allowance of the model that answered — so the number that bounds one request is
// published before anything is refused. That is the difference between the first
// oversized request of a session being sent and folded afterwards and being folded
// instead, and it is read from a streaming response as much as from a JSON one.
func TestGroqLearnsTheCeilingFromTheResponsesOwnHeaders(t *testing.T) {
	var answer atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(rateLimitTokensHeader, "8000")
		if answer.Load() {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		fmt.Fprint(w, tokensRefusal(groqOversizedMessage))
	}))
	defer srv.Close()

	p := newGroqProvider("groq", "Groq", srv.URL+"/openai/v1", "test-key")

	// A refusal whose sentence names 10000 and whose header says 8000. The tighter is
	// kept: a plan that fits under both is what the next request needs, and nothing
	// tells the app which of the two statements the account is governed by.
	_, err := p.Chat(context.Background(), ChatRequest{
		Model:    "moonshotai/kimi-k2-instruct",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("Chat error = %v, want an oversized-request refusal", err)
	}
	if got := p.RequestCeiling("moonshotai/kimi-k2-instruct"); got.Tokens != 8000 || got.Source != CeilingFromHeader {
		t.Errorf("ceiling after a refused response = %+v, want the 8000 its header stated", got)
	}

	// And the answer that was never refused states it too, which is the case the fix
	// exists for: the second request of a session is planned against a limit the app
	// learned from a success.
	answer.Store(true)
	stream, err := p.Chat(context.Background(), ChatRequest{
		Model:    "openai/gpt-oss-20b",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for range stream {
	}
	got := p.RequestCeiling("openai/gpt-oss-20b")
	if got.Tokens != 8000 || got.Source != CeilingFromHeader {
		t.Errorf("ceiling after an ordinary answer = %+v, want the 8000 its header stated and %q", got, CeilingFromHeader)
	}
	// Per model, like every other ceiling: nothing was learned about a model nobody
	// asked.
	if other := p.RequestCeiling("qwen/qwen3.8-27b"); other.Tokens != 0 || other.Source != "" {
		t.Errorf("ceiling for a model that was never called = %+v, want none", other)
	}
}

// TestACeilingOnlyEverTightens pins the rule that makes two writers safe. A response
// header and a refusal both state one limit, and the smaller of the two becomes the
// plan, so neither order of arrival can loosen it — which is what keeps a budget
// header that turned out to be the account's rather than the model's from turning a
// refused request into a second refused request.
func TestACeilingOnlyEverTightens(t *testing.T) {
	const model = "openai/gpt-oss-20b"

	headerFirst := newGroqProvider("groq", "Groq", "", "k")
	headerFirst.noteRequestCeiling(model, Ceiling{Tokens: 8000, Source: CeilingFromHeader})
	headerFirst.noteRequestCeiling(model, Ceiling{Tokens: 10000, Source: CeilingFromRefusal})
	if got := headerFirst.RequestCeiling(model); got.Tokens != 8000 || got.Source != CeilingFromHeader {
		t.Errorf("after a looser statement = %+v, want the 8000 already known", got)
	}

	refusalFirst := newGroqProvider("groq", "Groq", "", "k")
	refusalFirst.noteRequestCeiling(model, Ceiling{Tokens: 10000, Source: CeilingFromRefusal})
	refusalFirst.noteRequestCeiling(model, Ceiling{Tokens: 8000, Source: CeilingFromHeader})
	if got := refusalFirst.RequestCeiling(model); got.Tokens != 8000 {
		t.Errorf("after the same two statements in the other order = %+v, want 8000", got)
	}

	// A number with no source is not evidence: the source says which of the two places
	// it came from, and an unattributed number is not something a vendor stated.
	unattributed := newGroqProvider("groq", "Groq", "", "k")
	unattributed.noteRequestCeiling(model, Ceiling{Tokens: 8000})
	if got := unattributed.RequestCeiling(model); got.Tokens != 0 {
		t.Errorf("ceiling from a statement with no source = %+v, want none", got)
	}
}

// TestTheBudgetHeaderIsReadForGroqOnly is the narrowness that keeps this honest. The
// header is a measurement of one vendor — Groq documents its rate-limit headers as
// tokens per minute, always, and it was a Groq refusal that showed a per-minute
// allowance bounding one request. Another service's rate-limit header is not evidence
// about the largest request it accepts, and folding a chat against a number nothing
// measured would compact conversations that fit.
func TestTheBudgetHeaderIsReadForGroqOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(rateLimitTokensHeader, "8000")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	// A server of the user's own, of another flavour, sending the same header.
	p := &openAICompatProvider{
		id:        "custom",
		name:      "Custom",
		baseURL:   srv.URL,
		chatPaths: []string{srv.URL + "/chat/completions"},
	}
	stream, err := p.Chat(context.Background(), ChatRequest{
		Model:    "some-model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for range stream {
	}
	if got := p.RequestCeiling("some-model"); got.Tokens != 0 {
		t.Errorf("ceiling = %+v, want none: one vendor's rate-limit header is not another's evidence", got)
	}
}

// groqList builds the provider against a listing served from a test server.
func groqList(t *testing.T, listing string) []ModelInfo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, listing)
	}))
	t.Cleanup(srv.Close)
	models, err := newGroqProvider("groq", "Groq", srv.URL+"/openai/v1", "test-key").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	return models
}
