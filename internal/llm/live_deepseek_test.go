package llm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveDeepSeek exercises the provider against the real service. It is
// skipped unless KICHAT_LIVE=1 and KICHAT_DEEPSEEK_KEY are both set, so the
// normal suite stays hermetic and no metered request is ever made by accident.
//
// KICHAT_DEEPSEEK_KEY is passed in the environment on purpose: a key belongs
// nowhere near a tracked file.
//
//	KICHAT_LIVE=1 KICHAT_DEEPSEEK_KEY=<key> go test ./internal/llm/ -run TestLiveDeepSeek -v
//
// Three claims are checked, each of which the offline tests can only fake:
// the listing reports a real context window, `stream_options.include_usage` is
// honoured so the context meter gets a measured prompt_tokens, and a tool call
// streams — which is what the provider's CapTools claim rests on.
func TestLiveDeepSeek(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to run against a live backend")
	}
	key := os.Getenv("KICHAT_DEEPSEEK_KEY")
	if key == "" {
		t.Skip("set KICHAT_DEEPSEEK_KEY to run against the live DeepSeek API")
	}
	p := newDeepSeekProvider("deepseek", "DeepSeek", os.Getenv("KICHAT_DEEPSEEK_BASE"), key)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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
	chosen := models[0]
	if want := os.Getenv("KICHAT_DEEPSEEK_MODEL"); want != "" {
		for _, m := range models {
			if m.ID == want {
				chosen = m
			}
		}
	}
	if chosen.ContextLimit <= 0 {
		t.Fatalf("model %q reports no context window; the listing mapping is wrong", chosen.ID)
	}
	t.Logf("model=%s ctx=%d type=%s caps=%v state=%s", chosen.ID, chosen.ContextLimit, chosen.Type, chosen.Capabilities, chosen.State)
	if !chosen.Has(CapTools) {
		t.Errorf("model %q does not claim tools", chosen.ID)
	}

	// A reasoning model spends the first part of the budget on reasoning_content
	// before any answer token, so a small cap returns finish_reason=length with
	// no content at all. The budget is generous for that reason, not because the
	// answer is expected to be long.
	stream, err := p.Chat(ctx, ChatRequest{
		Model:       chosen.ID,
		Messages:    []Message{{Role: RoleUser, Content: "Reply with exactly: OK"}},
		MaxTokens:   2048,
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
		t.Errorf("usage = %+v, want a measured prompt_tokens", usage)
	}
	if stats == nil || stats.StopReason == "" {
		t.Logf("stats = %+v (a stop reason is not guaranteed by this API)", stats)
	}
	t.Logf("content=%q reasoning=%d chars usage=%+v", strings.TrimSpace(content.String()), reasoning.Len(), usage)

	// The tool claim the listing mapping encodes. One small call, and the model
	// is told the tool is the only way to answer.
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
