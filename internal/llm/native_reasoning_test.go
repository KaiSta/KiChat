package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// reasoningFixture stands in for an LM Studio whose v1 namespace reports a
// reasoning configuration for model "m". The shapes are the ones measured
// against a local server: `allowed_options: ["off","on"]` for a reasoning model,
// no reasoning block at all for one that cannot reason, and a 400 carrying
// `param: "reasoning"` when the setting is sent to a model that has no such
// configuration.
type reasoningFixture struct {
	server *httptest.Server

	// options is what the listing reports for "m"; nil means no reasoning block.
	options []string
	// nativeStatus, when >= 400, is what the native chat route answers with.
	nativeStatus int
	// noV1 removes the v1 namespace entirely, as an older build does.
	noV1 bool

	mu          sync.Mutex
	modelsCalls int
	nativeCalls int
	compatCalls int
	nativeBody  map[string]any
	compatBody  map[string]any
}

func newReasoningFixture(t *testing.T, options []string) *reasoningFixture {
	t.Helper()
	f := &reasoningFixture{options: options}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models":
			f.mu.Lock()
			f.modelsCalls++
			missing := f.noV1
			f.mu.Unlock()
			if missing {
				http.NotFound(w, r)
				return
			}
			caps := map[string]any{}
			if f.options != nil {
				caps["reasoning"] = map[string]any{"allowed_options": f.options, "default": "on"}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{{"key": "m", "capabilities": caps}},
			})

		case "/api/v1/chat":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.nativeCalls++
			f.nativeBody = body
			status := f.nativeStatus
			if f.noV1 {
				status = http.StatusNotFound
			}
			f.mu.Unlock()
			if status >= 400 {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":{"message":"The model \"m\" does not expose reasoning configuration.",`+
					`"type":"invalid_request","param":"reasoning","code":"invalid_value"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, frame := range []string{
				`{"type":"chat.start","model_instance_id":"m"}`,
				`{"type":"prompt_processing.start"}`,
				`{"type":"reasoning.delta","content":"Thinking Process"}`,
				`{"type":"message.start"}`,
				`{"type":"message.delta","content":"Gedächtnis"}`,
				`{"type":"message.delta","content":"."}`,
				`{"type":"message.end"}`,
				`{"type":"chat.end","result":{"model_instance_id":"m",` +
					`"output":[{"type":"reasoning","content":"Thinking Process"},` +
					`{"type":"message","content":"Gedächtnis."}],` +
					`"stats":{"input_tokens":58,"total_output_tokens":16,"reasoning_output_tokens":0,` +
					`"tokens_per_second":2.5,"time_to_first_token_seconds":0.5}}}`,
			} {
				fmt.Fprintf(w, "event: ignored\ndata: %s\n\n", frame)
			}

		case "/api/v0/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.compatCalls++
			f.compatBody = body
			f.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"compat"}}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *reasoningFixture) counts() (models, native, compat int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.modelsCalls, f.nativeCalls, f.compatCalls
}

func (f *reasoningFixture) nativeRequestBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nativeBody
}

func (f *reasoningFixture) compatRequestBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.compatBody
}

// drain collects a stream, reporting the text, the reasoning and the numbers.
func drain(t *testing.T, events <-chan StreamEvent) (text, reasoning string, usage *Usage, stats *GenerationStats) {
	t.Helper()
	var body, thought strings.Builder
	for ev := range events {
		switch ev.Type {
		case EventDelta:
			body.WriteString(ev.Text)
		case EventReasoning:
			thought.WriteString(ev.Text)
		case EventUsage:
			usage = ev.Usage
		case EventDone:
			stats = ev.Stats
		case EventError:
			t.Fatalf("stream error: %s", ev.Error)
		}
	}
	return body.String(), thought.String(), usage, stats
}

// summaryRequest is the request compaction makes: no tools, a system prompt and
// one user turn, with thinking explicitly switched off.
func summaryRequest(off bool) ChatRequest {
	return ChatRequest{
		Model: "m",
		Messages: []Message{
			{Role: RoleSystem, Content: "system"},
			{Role: RoleUser, Content: "transcript"},
		},
		Temperature: 0.2,
		MaxTokens:   64,
		Think:       &off,
	}
}

// TestNativeRouteSwitchesReasoningOffForASummariser pins the fix for the reported
// failure: a reasoning model spent every token of its output budget thinking and
// wrote no summary at all. On LM Studio the only way to switch that off is the
// native v1 route's `reasoning` field, so a summary must go there — with the
// model's own token accounting, which is where the reasoning share becomes
// visible.
func TestNativeRouteSwitchesReasoningOffForASummariser(t *testing.T) {
	f := newReasoningFixture(t, []string{"off", "on"})
	p := newLMStudioProvider(f.server.URL, "")

	events, err := p.Chat(context.Background(), summaryRequest(false))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, reasoning, usage, stats := drain(t, events)

	models, native, compat := f.counts()
	if models != 1 || native != 1 || compat != 0 {
		t.Fatalf("listing=%d native=%d compat=%d, want the native route used once",
			models, native, compat)
	}
	body := f.nativeRequestBody()
	if body == nil {
		t.Fatal("the native route received no body")
	}
	if got := body["reasoning"]; got != "off" {
		t.Errorf("reasoning = %v, want \"off\"", got)
	}
	if got, ok := body["store"].(bool); !ok || got {
		t.Errorf("store = %v, want the boolean false so a summary is not kept as a chat", body["store"])
	}
	if got := body["max_output_tokens"]; got != float64(64) {
		t.Errorf("max_output_tokens = %v, want the 64 the caller asked for", got)
	}
	if got := body["system_prompt"]; got != "system" {
		t.Errorf("system_prompt = %v, want the system message", got)
	}
	if _, ok := body["enable_thinking"]; ok {
		t.Error("the native route must not carry enable_thinking, which belongs to the compat path")
	}
	if _, ok := body["messages"]; ok {
		t.Error("the native route takes `input`, not `messages`")
	}
	input, ok := body["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %v, want a single part", body["input"])
	}
	if part, _ := input[0].(map[string]any); part["type"] != "text" || part["content"] != "transcript" {
		t.Errorf("input[0] = %v, want a text part holding the transcript", input[0])
	}

	if text != "Gedächtnis." {
		t.Errorf("text = %q, want the summary", text)
	}
	if reasoning != "Thinking Process" {
		t.Errorf("reasoning = %q, want the reasoning deltas passed through", reasoning)
	}
	if usage == nil {
		t.Fatal("no usage event")
	}
	if usage.ReasoningTokens != 0 {
		t.Errorf("reasoning tokens = %d, want 0 — that is the whole point of the route",
			usage.ReasoningTokens)
	}
	if usage.PromptTokens != 58 || usage.CompletionTokens != 16 {
		t.Errorf("usage = %+v, want the reported 58 in / 16 out", usage)
	}
	if stats == nil {
		t.Fatal("no done event")
	}
	if stats.Source != SourceLMStudio {
		t.Errorf("stats source = %q, want LM Studio's own numbers", stats.Source)
	}
	if stats.TokensPerSecond != 2.5 || stats.TimeToFirstTokenMs != 500 {
		t.Errorf("stats = %+v, want the reported speed and time to first token", stats)
	}
	if stats.StopReason == "length" {
		t.Errorf("stop reason = %q, but 16 tokens of 64 is not an exhausted budget", stats.StopReason)
	}
}

// TestNativeRouteReportsAnExhaustedBudgetAsLength pins the derived stop reason:
// this route sends no finish_reason, and without it a summariser that ran out of
// room looks like one that merely wrote something short.
func TestNativeRouteReportsAnExhaustedBudgetAsLength(t *testing.T) {
	f := newReasoningFixture(t, []string{"off"})
	p := newLMStudioProvider(f.server.URL, "")

	req := summaryRequest(false)
	req.MaxTokens = 16 // exactly what the fixture reports producing
	events, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, _, _, stats := drain(t, events)
	if stats == nil || stats.StopReason != "length" {
		t.Fatalf("stop reason = %+v, want \"length\" for a request that hit its cap", stats)
	}
}

// TestNativeRouteIsSkippedWhenReasoningCannotBeSwitchedOff pins the gate. LM
// Studio answers `reasoning` on a model without a reasoning configuration with
// HTTP 400 "does not expose reasoning configuration", so the field may only ever
// be sent where the listing says it is accepted.
func TestNativeRouteIsSkippedWhenReasoningCannotBeSwitchedOff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options []string
	}{
		{"only on is offered", []string{"on"}},
		{"the options are empty", []string{}},
		{"no reasoning block at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReasoningFixture(t, tc.options)
			p := newLMStudioProvider(f.server.URL, "")

			events, err := p.Chat(context.Background(), summaryRequest(false))
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			text, _, _, _ := drain(t, events)

			models, native, compat := f.counts()
			if native != 0 || compat != 1 {
				t.Fatalf("native=%d compat=%d, want the compat path only", native, compat)
			}
			if models != 1 {
				t.Errorf("the listing was read %d times, want once", models)
			}
			if got := f.compatRequestBody()["enable_thinking"]; got != false {
				t.Errorf("enable_thinking = %v, want false on the compat path", got)
			}
			if text != "compat" {
				t.Errorf("text = %q, want the compat answer", text)
			}
		})
	}
}

// TestNativeRouteIsSkippedForRequestsItCannotExpress pins that the narrow route
// is used only where it can carry the request whole. An assistant turn has no
// representation on it at all, so a summary that quietly dropped one would
// corrupt the memory it is writing.
func TestNativeRouteIsSkippedForRequestsItCannotExpress(t *testing.T) {
	withTools := summaryRequest(false)
	withTools.Tools = []ToolDefinition{{Name: "search"}}

	withAssistant := summaryRequest(false)
	withAssistant.Messages = append(withAssistant.Messages,
		Message{Role: RoleAssistant, Content: "eine Antwort"})

	withImage := summaryRequest(false)
	withImage.Messages = append(withImage.Messages,
		Message{Role: RoleUser, Content: "siehe Bild", Images: []string{"iVBORw0KGgo"}})

	seed := 7
	withSeed := summaryRequest(false)
	withSeed.Seed = &seed

	for _, tc := range []struct {
		name string
		req  ChatRequest
	}{
		{"tools are offered", withTools},
		{"an assistant turn is present", withAssistant},
		{"an image is attached", withImage},
		{"a seed is pinned", withSeed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReasoningFixture(t, []string{"off", "on"})
			p := newLMStudioProvider(f.server.URL, "")

			events, err := p.Chat(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			text, _, _, _ := drain(t, events)

			models, native, compat := f.counts()
			if native != 0 {
				t.Errorf("the native route was used %d times, want 0", native)
			}
			if compat != 1 {
				t.Errorf("compat = %d, want 1", compat)
			}
			if models != 0 {
				t.Errorf("the listing was read %d times, want 0: the route is ruled out first", models)
			}
			if text != "compat" {
				t.Errorf("text = %q", text)
			}
		})
	}
}

// TestNativeRouteRejectionFallsBackAndIsNotRepeated pins that the optimisation
// can never fail a compaction, and that a rejected setting is retired once
// rather than paid for on every later summary.
func TestNativeRouteRejectionFallsBackAndIsNotRepeated(t *testing.T) {
	f := newReasoningFixture(t, []string{"off", "on"})
	f.nativeStatus = 400
	p := newLMStudioProvider(f.server.URL, "")

	for i := 0; i < 2; i++ {
		events, err := p.Chat(context.Background(), summaryRequest(false))
		if err != nil {
			t.Fatalf("Chat %d: %v", i, err)
		}
		if text, _, _, _ := drain(t, events); text != "compat" {
			t.Fatalf("call %d: text = %q, want the compat answer", i, text)
		}
	}
	if _, native, compat := f.counts(); native != 1 || compat != 2 {
		t.Errorf("native=%d compat=%d, want the rejected route tried exactly once", native, compat)
	}

	// Re-reading the capabilities re-arms the check, which is how a listing that
	// has genuinely changed is picked up after a settings change or a refresh.
	p.InvalidateCache()
	events, err := p.Chat(context.Background(), summaryRequest(false))
	if err != nil {
		t.Fatalf("Chat after InvalidateCache: %v", err)
	}
	_, _, _, _ = drain(t, events)
	models, native, _ := f.counts()
	if native != 2 || models != 2 {
		t.Errorf("after InvalidateCache: native=%d listing=%d, want the check made again", native, models)
	}
}

// TestNativeRouteIsNotConsultedForAnOrdinaryTurn pins that the extra round trip
// is paid only where it buys something. A user's own turn has no reason to think
// about reasoning settings, and must not slow down for one.
func TestNativeRouteIsNotConsultedForAnOrdinaryTurn(t *testing.T) {
	f := newReasoningFixture(t, []string{"off", "on"})
	p := newLMStudioProvider(f.server.URL, "")

	events, err := p.Chat(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, _, _, _ = drain(t, events)

	models, native, compat := f.counts()
	if models != 0 || native != 0 || compat != 1 {
		t.Errorf("listing=%d native=%d compat=%d, want the compat path with no listing read",
			models, native, compat)
	}
}

// TestNativeRouteIsSkippedWhenV1IsMissing pins the older-build path: a 404 from
// the namespace is permanent, so it is not asked again on every summary.
func TestNativeRouteIsSkippedWhenV1IsMissing(t *testing.T) {
	f := newReasoningFixture(t, []string{"off"})
	f.noV1 = true
	p := newLMStudioProvider(f.server.URL, "")

	for i := 0; i < 2; i++ {
		events, err := p.Chat(context.Background(), summaryRequest(false))
		if err != nil {
			t.Fatalf("Chat %d: %v", i, err)
		}
		if text, _, _, _ := drain(t, events); text != "compat" {
			t.Fatalf("call %d: text = %q", i, text)
		}
	}
	models, native, compat := f.counts()
	if models != 1 {
		t.Errorf("the missing namespace was probed %d times, want once", models)
	}
	if native != 0 || compat != 2 {
		t.Errorf("native=%d compat=%d, want the compat path only", native, compat)
	}
}
