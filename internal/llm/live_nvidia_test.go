package llm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveNvidia exercises the provider against the real build.nvidia.com API.
// It is skipped unless KICHAT_LIVE=1 and KICHAT_NVIDIA_KEY are both set, so the
// normal suite stays hermetic and no metered request is ever made by accident.
//
//	KICHAT_LIVE=1 KICHAT_NVIDIA_KEY=<key> go test ./internal/llm/ -run TestLiveNvidia -v
//
// KICHAT_NVIDIA_KEY is passed in the environment on purpose: a key belongs
// nowhere near a tracked file.
//
//   - KICHAT_NVIDIA_BASE overrides the address (a self-hosted NIM container).
//   - KICHAT_NVIDIA_MODEL picks the model to drive. It defaults to a chat model
//     that answered the probe, because the catalogue is largely not callable:
//     see the next paragraph.
//
// Three claims are checked, each of which the offline tests can only fake: the
// listing maps the way the provider says it does, `stream_options.include_usage`
// is honoured so the context meter gets a measured prompt_tokens, and a real
// tool call streams — which is what the provider's unconditional CapTools claim
// rests on.
//
// One claim is checked in the *negative*, and it is the important one here. The
// catalogue lists models the account cannot invoke: measured, 45 of 80 answer
// HTTP 404 with the vendor's own "Not found for account" problem+json, and another
// nine answer the bare-text `404 page not found` because no chat route exists for
// them. That is why this test does not walk the whole listing — it would spend
// eighty requests mostly discovering refusals — and why the provider filters
// nothing: entitlement is per account, so a filter written from this test's key
// would be wrong for every other one. TestNvidiaListingKeepsModelsTheAccountMayNotReach
// pins that decision offline.
//
// The reported context window is asserted to be **zero**, not positive: no route
// of this API publishes one, so a number appearing here would mean the provider
// started inventing metadata to plan against.
func TestLiveNvidia(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to run against a live backend")
	}
	key := os.Getenv("KICHAT_NVIDIA_KEY")
	if key == "" {
		t.Skip("set KICHAT_NVIDIA_KEY to run against the live build.nvidia.com API")
	}
	p := newNvidiaProvider("nvidia", "NVIDIA", os.Getenv("KICHAT_NVIDIA_BASE"), key)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := p.Health(ctx); err != nil {
		t.Fatalf("health: %v", err)
	}
	models, err := p.ListModels(ctx)
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("the API listed no models")
	}
	t.Logf("catalogue: %d entries", len(models))

	want := os.Getenv("KICHAT_NVIDIA_MODEL")
	if want == "" {
		want = "openai/gpt-oss-20b"
	}
	chosen := ModelInfo{}
	for _, m := range models {
		if m.ID == want {
			chosen = m
		}
	}
	if chosen.ID == "" {
		t.Fatalf("model %q is not in the catalogue", want)
	}
	if chosen.State != StateLoaded {
		t.Errorf("state = %q, want %q: a hosted model is ready without a load", chosen.State, StateLoaded)
	}
	if !chosen.Has(CapTools) {
		t.Errorf("model %q does not claim tools", chosen.ID)
	}
	if chosen.Publisher == "" {
		t.Errorf("model %q reports no publisher, so owned_by was not mapped", chosen.ID)
	}
	if chosen.ContextLimit != 0 {
		t.Errorf("window = %d, want 0: no route of this API reports one, so a value here is invented",
			chosen.ContextLimit)
	}
	t.Logf("model=%s publisher=%s ctx=%d caps=%v state=%s",
		chosen.ID, chosen.Publisher, chosen.ContextLimit, chosen.Capabilities, chosen.State)

	// The budget is generous because these models think before they answer: the
	// first request measured on this API spent every token of a 40-token cap on
	// reasoning and returned a null content with finish_reason=length.
	stream, err := p.Chat(ctx, ChatRequest{
		Model:       chosen.ID,
		Messages:    []Message{{Role: RoleUser, Content: "Reply with exactly: OK"}},
		MaxTokens:   4096,
		Temperature: 0.2,
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	var (
		content   strings.Builder
		reasoning strings.Builder
		usage     *Usage
		stats     *GenerationStats
	)
	for ev := range stream {
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
	if content.Len() == 0 && reasoning.Len() == 0 {
		t.Error("the stream produced neither an answer nor reasoning")
	}
	// The measured prompt_tokens is the whole reason the request asks for usage:
	// without it the context meter can only ever show an estimate.
	if usage == nil || usage.PromptTokens == 0 {
		t.Errorf("usage = %+v, want a measured prompt_tokens from stream_options.include_usage", usage)
	}
	if stats == nil || stats.StopReason == "" {
		t.Errorf("stats = %+v, want the finish reason the API reported", stats)
	}
	t.Logf("content=%q reasoning=%d chars usage=%+v stop=%v",
		strings.TrimSpace(content.String()), reasoning.Len(), usage, stats)

	// The tool claim the listing mapping encodes. The model is told the tool is
	// the only way to answer.
	toolStream, err := p.Chat(ctx, ChatRequest{
		Model:     chosen.ID,
		Messages:  []Message{{Role: RoleUser, Content: "What is the weather in Berlin? You must call get_weather."}},
		MaxTokens: 4096,
		Tools: []ToolDefinition{{
			Name:        "get_weather",
			Description: "Get the current weather for a city",
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []string{"city"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("tool chat: %v", err)
	}
	var calls []ToolCall
	for ev := range toolStream {
		switch ev.Type {
		case EventToolCall:
			calls = append(calls, *ev.ToolCall)
		case EventError:
			t.Fatalf("tool stream error: %s", ev.Error)
		}
	}
	if len(calls) == 0 {
		t.Fatal("no tool call streamed, but the provider claims CapTools for this model")
	}
	if calls[0].Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", calls[0].Name)
	}
	if !strings.Contains(calls[0].Arguments, "Berlin") {
		t.Errorf("tool arguments = %q, want the city", calls[0].Arguments)
	}
	t.Logf("tool call=%s arguments=%s", calls[0].Name, calls[0].Arguments)
}
