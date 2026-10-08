package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// llamaCppListing is what a direct llama-server answers on /v1/models. It is one
// entry, the model named by its file path, and it carries `meta` — including
// `n_ctx_train`, which is the only context number this route can supply. The
// shape is the one documented under "GET /v1/models".
const llamaCppListing = `{"object":"list","data":[{"id":"/models/Qwen3-4B-Q4_K_M.gguf",
	"object":"model","created":1757141666,"owned_by":"llamacpp",
	"meta":{"vocab_type":2,"n_vocab":151936,"n_ctx_train":32768,"n_embd":2560,
	        "n_params":4022468096,"size":2497475008},
	"architecture":{"input_modalities":["text"],"output_modalities":["text"]}}]}`

// llamaCppPropsBody is the /props response a direct server gives, trimmed to the
// fields the app reads. `n_ctx` here is deliberately *lower* than the listing's
// `n_ctx_train`: that is the whole point of reading /props — a server started
// with `-c 8192` reports 8192 here while its listing still reports the trained
// 32768, and the loaded one is the number a prompt has to fit in.
const llamaCppPropsBody = `{
	"default_generation_settings":{"n_ctx":8192,"temperature":0.8,"n_predict":-1},
	"total_slots":1,
	"model_path":"/models/Qwen3-4B-Q4_K_M.gguf",
	"chat_template":"{%- if enable_thinking is defined and enable_thinking %}<|think|>{%- endif %}",
	"chat_template_caps":{"supports_tool_calls":true,"supports_parallel_tool_calls":true,"supports_system_role":true},
	"modalities":{"vision":false},
	"build_info":"b1234-abcdef",
	"is_sleeping":false
}`

// llamaCppServer wires a fake llama-server that answers the two routes a direct
// server answers, and counts how often /props was asked so a test can pin the
// caching. A propsStatus other than 200 answers with that status instead of the
// body.
func llamaCppServer(t *testing.T, listing, props string, propsStatus int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var propsHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, listing)
		case "/props":
			propsHits.Add(1)
			if propsStatus != http.StatusOK {
				w.WriteHeader(propsStatus)
				fmt.Fprint(w, `{"error":{"message":"not ready"}}`)
				return
			}
			fmt.Fprint(w, props)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &propsHits
}

// llamaCppRecorder keeps the request body of the one chat call a test makes,
// guarded so the test can read it after the stream has finished.
type llamaCppRecorder struct {
	mu   sync.Mutex
	body map[string]any
}

func (r *llamaCppRecorder) set(body map[string]any) {
	r.mu.Lock()
	r.body = body
	r.mu.Unlock()
}

// Body returns the decoded request the server received.
func (r *llamaCppRecorder) Body() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body
}

// llamaCppChatServer answers /props, /v1/models and /v1/chat/completions. The sse
// argument is one JSON frame per line, framed as data: events the way llama.cpp
// streams them. The listing is served too, so a test can assert the /v1 base both
// chats and discovers models.
func llamaCppChatServer(t *testing.T, props, sse string) (*httptest.Server, *llamaCppRecorder) {
	t.Helper()
	rec := &llamaCppRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/props":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, props)
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, llamaCppListing)
		case "/v1/chat/completions":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			rec.set(body)
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			for _, frame := range strings.Split(strings.TrimSpace(sse), "\n") {
				fmt.Fprintf(w, "data: %s\n\n", frame)
				if flusher != nil {
					flusher.Flush()
				}
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// drainChat runs a chat request to completion and returns the done event's stats
// together with the usage the stream reported.
func drainChat(t *testing.T, p Provider, req ChatRequest) (*GenerationStats, *Usage) {
	t.Helper()
	events, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var (
		stats *GenerationStats
		usage *Usage
	)
	for ev := range events {
		switch ev.Type {
		case EventUsage:
			usage = ev.Usage
		case EventDone:
			stats = ev.Stats
		case EventError:
			t.Fatalf("stream error: %s", ev.Error)
		}
	}
	return stats, usage
}

// TestLlamaCppListingReportsTheLoadedWindowNotTheTrainedOne is the reason this
// provider exists. A model trained for 32768 tokens and loaded with `-c 8192`
// has to be planned against 8192: reporting the trained window would let the
// context grow past what the server allocated, and llama.cpp resolves that by
// dropping the *front* of the prompt — the system prompt — which is the failure
// the user never sees coming.
func TestLlamaCppListingReportsTheLoadedWindowNotTheTrainedOne(t *testing.T) {
	srv, propsHits := llamaCppServer(t, llamaCppListing, llamaCppPropsBody, http.StatusOK)

	models, err := newLlamaCppProvider("llamacpp", "llama.cpp", srv.URL, "").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("listed %d models, want the single one a direct server serves: %+v", len(models), models)
	}
	m := models[0]

	if m.ContextLimit != 8192 {
		t.Errorf("ContextLimit = %d, want the 8192 the server is loaded with", m.ContextLimit)
	}
	if m.MaxContext != 32768 {
		t.Errorf("MaxContext = %d, want the 32768 the model was trained for", m.MaxContext)
	}
	// A direct server is started with `-m` and does not answer before the weights
	// are resident, so the model is loaded by definition. Reporting it as pending
	// would grey the row out and keep it out of the default-model choice.
	if m.State != StateLoaded {
		t.Errorf("State = %q, want %q", m.State, StateLoaded)
	}
	if m.Backend != "llamacpp" {
		t.Errorf("Backend = %q, want the provider's own id", m.Backend)
	}
	// The caps the server reports are what the app acts on.
	if !m.Has(CapTools) {
		t.Errorf("does not claim tools: %v", m.Capabilities)
	}
	// The template names enable_thinking, so the toggle is real for this model.
	if !m.Has(CapThink) {
		t.Errorf("does not claim thinking although the template names it: %v", m.Capabilities)
	}
	if m.Type != "llm" || m.Has(CapVision) {
		t.Errorf("type/caps = %q/%v, want a text-only model", m.Type, m.Capabilities)
	}
	if got := propsHits.Load(); got != 1 {
		t.Errorf("/props asked %d times for one listing, want 1", got)
	}
}

// TestLlamaCppUnknownTemplateCapsStillOfferTools guards the difference between
// "the server said no" and "the server did not say".
//
// llama.cpp documents `chat_template_caps` as possibly `{}` — a template it could
// not analyse. Decoding that into a struct would hand the app `false` for
// `supports_tool_calls`, and the app answers a missing tool claim by never using
// the native tool protocol at all. So an absent key must not read as a denial:
// guessing "yes" costs a prose answer when it is wrong, guessing "no" silently
// costs a working model its tools.
func TestLlamaCppUnknownTemplateCapsStillOfferTools(t *testing.T) {
	props := `{"default_generation_settings":{"n_ctx":4096},"chat_template":"","chat_template_caps":{}}`
	srv, _ := llamaCppServer(t, llamaCppListing, props, http.StatusOK)

	models, err := newLlamaCppProvider("llamacpp", "llama.cpp", srv.URL, "").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if !models[0].Has(CapTools) {
		t.Errorf("an empty chat_template_caps withdrew tools: %v", models[0].Capabilities)
	}
	// An empty template cannot be shown to read the parameter, so the toggle is
	// not claimed for it.
	if models[0].Has(CapThink) {
		t.Errorf("claimed thinking for a template that never names the parameter: %v", models[0].Capabilities)
	}
}

// TestLlamaCppExplicitNoToolsIsHonoured is the other half: a template the server
// did analyse and found wanting is respected, because that is a statement about
// the model rather than a gap in the report.
func TestLlamaCppExplicitNoToolsIsHonoured(t *testing.T) {
	props := `{"default_generation_settings":{"n_ctx":4096},"chat_template":"",
		"chat_template_caps":{"supports_tool_calls":false}}`
	srv, _ := llamaCppServer(t, llamaCppListing, props, http.StatusOK)

	models, err := newLlamaCppProvider("llamacpp", "llama.cpp", srv.URL, "").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if models[0].Has(CapTools) {
		t.Errorf("claimed tools against an explicit false: %v", models[0].Capabilities)
	}
}

// TestLlamaCppThinkingToggleIsNestedInTemplateKwargs pins llama.cpp's shape for
// "do not think". It has no `enable_thinking` field of its own: the parameter is
// handed to the model's own chat template through `chat_template_kwargs`. Sending
// the LM Studio shape instead would be a field the server does not define, and
// the whole point of the toggle is that compaction can ask a reasoning model to
// stop spending its output budget on private thinking.
func TestLlamaCppThinkingToggleIsNestedInTemplateKwargs(t *testing.T) {
	srv, rec := llamaCppChatServer(t, llamaCppPropsBody, `{"choices":[{"delta":{"content":"ok"}}]}`)
	p := newLlamaCppProvider("llamacpp", "llama.cpp", srv.URL, "")

	no := false
	drainChat(t, p, ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Think:    &no,
	})

	body := rec.Body()
	kwargs, ok := body["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("chat_template_kwargs missing from the request: %v", body)
	}
	if v, ok := kwargs[llamaCppThinkingParam].(bool); !ok || v {
		t.Errorf("chat_template_kwargs = %v, want %s false", kwargs, llamaCppThinkingParam)
	}
	if _, present := body["enable_thinking"]; present {
		t.Errorf("a top-level enable_thinking leaked into a llama.cpp request: %v", body)
	}
}

// TestLlamaCppThinkingToggleIsOmittedWhenTheTemplateCannotReadIt is the guard
// that keeps the toggle from breaking models. llama.cpp validates the parameters
// a request hands to the template, so a model whose template never names
// `enable_thinking` would get an error instead of an answer. Nothing is sent in
// that case, and the turn goes through normally.
func TestLlamaCppThinkingToggleIsOmittedWhenTheTemplateCannotReadIt(t *testing.T) {
	props := `{"default_generation_settings":{"n_ctx":4096},
		"chat_template":"{% for m in messages %}{{ m.content }}{% endfor %}",
		"chat_template_caps":{"supports_tool_calls":true}}`
	srv, rec := llamaCppChatServer(t, props, `{"choices":[{"delta":{"content":"ok"}}]}`)
	p := newLlamaCppProvider("llamacpp", "llama.cpp", srv.URL, "")

	no := false
	drainChat(t, p, ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Think:    &no,
	})

	body := rec.Body()
	if _, present := body["chat_template_kwargs"]; present {
		t.Errorf("sent a template parameter the template never reads: %v", body)
	}
	if _, present := body["enable_thinking"]; present {
		t.Errorf("sent enable_thinking to llama.cpp: %v", body)
	}
	// The capability is still advertised, so compaction keeps its option; the
	// provider just declines to send what this template cannot use.
	if !p.CanToggleThinking() {
		t.Error("llama.cpp does not report the ability to toggle thinking")
	}
}

// llamaCppTimingsFrame is the final frame of a llama.cpp stream: no choices, the
// standard `usage` object, and the `timings` object the server measures itself.
// The numbers are the ones llama.cpp's documentation prints, including the
// awkward `prompt_n + cache_n + predicted_n` arithmetic that makes the two
// objects consistent: 1 + 236 + 35 tokens were in context.
const llamaCppTimingsFrame = `{"choices":[],` +
	`"usage":{"prompt_tokens":44,"completion_tokens":48,"total_tokens":92,` +
	`"prompt_tokens_details":{"cached_tokens":236}},` +
	`"timings":{"cache_n":236,"prompt_n":1,"prompt_ms":30.958,"prompt_per_token_ms":30.958,` +
	`"prompt_per_second":32.301828283480845,"predicted_n":35,"predicted_ms":661.064,` +
	`"predicted_per_token_ms":18.887542857142858,"predicted_per_second":52.94494935437416}}`

// TestLlamaCppTimingsReplaceTheEstimateWithTheServersOwnNumbers: the server
// times its own decode loop, so the badge can name llama.cpp rather than admit to
// an estimate. The number has to be the server's, not a recomputation — a client
// that derived its own tokens/second from the text length would land somewhere
// else entirely and would be wrong for the same reason the estimate is.
func TestLlamaCppTimingsReplaceTheEstimateWithTheServersOwnNumbers(t *testing.T) {
	srv, _ := llamaCppChatServer(t, llamaCppPropsBody, `{"choices":[{"delta":{"content":"Hallo"}}]}
{"choices":[{"delta":{"content":" Welt"}}]}
`+llamaCppTimingsFrame)

	stats, usage := drainChat(t, newLlamaCppProvider("llamacpp", "llama.cpp", srv.URL, ""), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})

	if stats == nil {
		t.Fatal("no stats were reported")
	}
	if stats.Source != SourceLlamaCpp {
		t.Errorf("stats source = %q, want %q", stats.Source, SourceLlamaCpp)
	}
	if math.Abs(stats.TokensPerSecond-52.94494935437416) > 1e-9 {
		t.Errorf("tokens/second = %v, want the 52.94… the server measured", stats.TokensPerSecond)
	}
	// The decode loop timed itself; the client's stopwatch would include the wait
	// for the first token.
	if stats.GenerationMs != 661 {
		t.Errorf("generation ms = %d, want the 661 the server measured", stats.GenerationMs)
	}
	// The standard usage object still arrives and is still what the context meter
	// reads, including the cached prompt tokens the server reports separately.
	if usage == nil || usage.PromptTokens != 44 || usage.CompletionTokens != 48 {
		t.Fatalf("usage = %+v, want the token counts the stream reported", usage)
	}
	if usage.CachedPromptTokens != 236 {
		t.Errorf("cached prompt tokens = %d, want the 236 the server re-used", usage.CachedPromptTokens)
	}
}

// TestLlamaCppAMissingPropsRouteIsRemembered: a build without /props answers 404
// on every listing, and asking again on each health refresh would be pure noise.
// The route is remembered as absent while the listing itself still succeeds —
// the models are what the caller asked for, and the context window falls back to
// the app's own conservative default rather than to the trained one, which would
// promise a window the server never allocated.
func TestLlamaCppAMissingPropsRouteIsRemembered(t *testing.T) {
	srv, propsHits := llamaCppServer(t, llamaCppListing, "", http.StatusNotFound)

	p := newLlamaCppProvider("llamacpp", "llama.cpp", srv.URL, "")
	for i := range 2 {
		models, err := p.ListModels(context.Background())
		if err != nil {
			t.Fatalf("listing %d: %v", i, err)
		}
		if len(models) != 1 {
			t.Fatalf("listing %d: %+v, want the model the listing reports", i, models)
		}
		if models[0].ContextLimit != 0 {
			t.Errorf("listing %d: ContextLimit = %d, want it unset without /props", i, models[0].ContextLimit)
		}
		// The ceiling still comes from the listing, which needs no /props.
		if models[0].MaxContext != 32768 {
			t.Errorf("listing %d: MaxContext = %d, want the trained 32768", i, models[0].MaxContext)
		}
	}
	if got := propsHits.Load(); got != 1 {
		t.Errorf("/props asked %d times over two listings, want the absence remembered after 1", got)
	}
}

// TestLlamaCppAFailedPropsReadIsRetriedOnTheNextListing: a server that is still
// loading its weights answers /props with an error, and that must not be
// remembered. Caching it would leave the model without a context window for the
// rest of the session even after the server came up — and the window is the one
// number this provider exists to supply.
func TestLlamaCppAFailedPropsReadIsRetriedOnTheNextListing(t *testing.T) {
	var (
		failing atomic.Bool
		hits    atomic.Int32
	)
	failing.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, llamaCppListing)
		case "/props":
			hits.Add(1)
			if failing.Load() {
				http.Error(w, `{"error":{"message":"model is loading"}}`, http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, llamaCppPropsBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	p := newLlamaCppProvider("llamacpp", "llama.cpp", srv.URL, "")
	first, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels while /props is failing: %v", err)
	}
	if first[0].ContextLimit != 0 {
		t.Errorf("ContextLimit = %d, want it unset while /props fails", first[0].ContextLimit)
	}
	if hits.Load() == 0 {
		t.Fatal("/props was never asked")
	}

	failing.Store(false)
	second, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels after /props recovered: %v", err)
	}
	if second[0].ContextLimit != 8192 {
		t.Errorf("ContextLimit = %d, want the 8192 the server now reports", second[0].ContextLimit)
	}
}

// capCount counts how often a capability is listed. A model that claims vision
// from both reports must still show it once: the picker renders the list.
func capCount(info ModelInfo, capability string) int {
	n := 0
	for _, c := range info.Capabilities {
		if c == capability {
			n++
		}
	}
	return n
}

// TestLlamaCppVisionFromTheListingsInputModalities covers the report a direct
// server makes on /v1/models: `architecture.input_modalities` gains "image" for a
// multimodal model, and a model that takes an image has to be typed as one or the
// attachment path stays shut.
func TestLlamaCppVisionFromTheListingsInputModalities(t *testing.T) {
	listing := `{"data":[{"id":"m","meta":{"n_ctx_train":8192},
		"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]}}]}`
	srv, _ := llamaCppServer(t, listing, llamaCppPropsBody, http.StatusOK)

	models, err := newLlamaCppProvider("llamacpp", "llama.cpp", srv.URL, "").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if models[0].Type != "vlm" || !models[0].Has(CapVision) {
		t.Errorf("type/caps = %q/%v, want a vision model", models[0].Type, models[0].Capabilities)
	}
	// /props reports vision as false here, and a disagreement must not add the
	// capability a second time.
	if n := capCount(models[0], CapVision); n != 1 {
		t.Errorf("vision listed %d times, want once: %v", n, models[0].Capabilities)
	}
}

// TestLlamaCppVisionFromPropsAlone covers the other report: /props carries
// `modalities.vision` as a plain boolean, and it is read as well, because a
// build that fills in one may leave the other unset.
func TestLlamaCppVisionFromPropsAlone(t *testing.T) {
	props := `{"default_generation_settings":{"n_ctx":8192},"modalities":{"vision":true},
		"chat_template_caps":{"supports_tool_calls":true}}`
	srv, _ := llamaCppServer(t, llamaCppListing, props, http.StatusOK)

	models, err := newLlamaCppProvider("llamacpp", "llama.cpp", srv.URL, "").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if models[0].Type != "vlm" || !models[0].Has(CapVision) {
		t.Errorf("type/caps = %q/%v, want a vision model", models[0].Type, models[0].Capabilities)
	}
	if n := capCount(models[0], CapVision); n != 1 {
		t.Errorf("vision listed %d times, want once: %v", n, models[0].Capabilities)
	}
	// The listing carried no architecture at all, and the window still arrives
	// from /props — the two reports are independent.
	if models[0].ContextLimit != 8192 {
		t.Errorf("ContextLimit = %d, want the 8192 from /props", models[0].ContextLimit)
	}
}

// replyFrom runs a chat and returns the text streamed back, so a route test can
// assert on what the user would actually see.
func replyFrom(t *testing.T, p Provider) string {
	t.Helper()
	events, err := p.Chat(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var reply strings.Builder
	for ev := range events {
		switch ev.Type {
		case EventDelta:
			reply.WriteString(ev.Text)
		case EventError:
			t.Fatalf("stream error: %s", ev.Error)
		}
	}
	return reply.String()
}

// TestLlamaCppAcceptsABaseURLThatAlreadyNamesV1: a user who pastes the address
// their proxy exposes, `http://host:8080/v1`, must get a working backend rather
// than one that asks for /v1/v1/chat/completions. The /v1 suffix is the endpoint
// root on one side and the server root on the other, and /props hangs off the
// server root.
func TestLlamaCppAcceptsABaseURLThatAlreadyNamesV1(t *testing.T) {
	srv, _ := llamaCppChatServer(t, llamaCppPropsBody, `{"choices":[{"delta":{"content":"ok"}}]}`)

	p := newLlamaCppProvider("llamacpp", "llama.cpp", srv.URL+"/v1", "")
	if got := replyFrom(t, p); got != "ok" {
		t.Errorf("reply = %q, want the answer from /v1/chat/completions", got)
	}

	// The listing and /props are derived from the same base, so the model still
	// arrives with its window rather than as a bare id.
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ContextLimit != 8192 {
		t.Errorf("models = %+v, want the listing with the /props window", models)
	}
}

// TestLlamaCppFallsBackToTheRootChatPath covers the other spelling: llama-server
// answers /chat/completions as well as the /v1 one, and a proxy that exposes only
// the plain route is common enough to be worth the second attempt. A 404 retires
// the first path for good, so the second request must not try it again.
func TestLlamaCppFallsBackToTheRootChatPath(t *testing.T) {
	var v1Hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			// A proxy that does not expose the /v1 spelling.
			v1Hits.Add(1)
			http.NotFound(w, r)
		case "/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
		case "/props":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, llamaCppPropsBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	p := newLlamaCppProvider("llamacpp", "llama.cpp", srv.URL, "")
	for i := range 2 {
		if got := replyFrom(t, p); got != "ok" {
			t.Fatalf("reply %d = %q, want the answer from the root path", i, got)
		}
	}
	if got := v1Hits.Load(); got != 1 {
		t.Errorf("/v1/chat/completions tried %d times over two chats, want once: a 404 must retire it", got)
	}
}
