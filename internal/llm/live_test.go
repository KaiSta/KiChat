package llm

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveLMStudio exercises the provider against a real LM Studio server.
// It is skipped unless KICHAT_LIVE=1, so the normal test suite stays hermetic.
//
// The assertion that matters is Usage.PromptTokens != 0. LM Studio only
// includes `usage` in a *streamed* response when the request carries
// `stream_options.include_usage`, so this test fails loudly if that field ever
// disappears from the request body — and a missing prompt_tokens is what would
// silently leave the context meter unable to show a measured value.
func TestLiveLMStudio(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to run against a live backend")
	}
	base := os.Getenv("KICHAT_LMSTUDIO")
	if base == "" {
		base = "http://localhost:1234"
	}
	p := newLMStudioProvider(base, "")

	// Generous: a cold LM Studio JIT-loads the model on the first request.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := p.Health(ctx); err != nil {
		t.Fatalf("health: %v", err)
	}
	models, err := p.ListModels(ctx)
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	// Prefer an already-loaded chat model; otherwise pick one and let LM Studio
	// just-in-time load it, which is exactly what the app relies on.
	chosen := pickLiveModel(models, os.Getenv("KICHAT_LMSTUDIO_MODEL"))
	if chosen.ID == "" {
		t.Fatalf("no usable chat model among %d models", len(models))
	}
	if chosen.State != StateLoaded {
		t.Logf("model %q is %s; LM Studio will JIT-load it", chosen.ID, chosen.State)
	}
	t.Logf("model=%s type=%s caps=%v loadedCtx=%d maxCtx=%d",
		chosen.ID, chosen.Type, chosen.Capabilities, chosen.ContextLimit, chosen.MaxContext)

	stream, err := p.Chat(ctx, ChatRequest{
		Model:    chosen.ID,
		Messages: []Message{{Role: "system", Content: "Antworte extrem knapp."}, {Role: "user", Content: "Nenne in einem Satz die Hauptstadt von Frankreich."}},
		// Generous: reasoning models spend the first slice of the budget on
		// reasoning_content, and a tiny cap yields stop_reason=length with no
		// visible text at all.
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	var (
		text, reasoning string
		usage           *Usage
		stats           *GenerationStats
		modelInfo       *ModelInfo
		events          int
		firstDelta      time.Time
		started         = time.Now()
	)
	for ev := range stream {
		events++
		switch ev.Type {
		case EventDelta:
			if firstDelta.IsZero() {
				firstDelta = time.Now()
			}
			text += ev.Text
		case EventReasoning:
			reasoning += ev.Text
		case EventUsage:
			usage = ev.Usage
		case EventModelInfo:
			modelInfo = ev.ModelInfo
		case EventError:
			t.Fatalf("stream error: %s", ev.Error)
		}
		// The done event carries stats.
		if ev.Type == EventDone {
			stats = ev.Stats
		}
	}

	wall := time.Since(started)
	ttft := "n/a (no visible content delta)"
	if !firstDelta.IsZero() {
		ttft = firstDelta.Sub(started).Round(time.Millisecond).String()
	}
	t.Logf("events=%d wall=%s ttft=%s", events, wall.Round(time.Millisecond), ttft)
	t.Logf("content=%q", text)
	if reasoning != "" {
		t.Logf("reasoning=%d chars (first 120: %q)", len(reasoning), firstHead(reasoning, 120))
	}
	if modelInfo != nil {
		t.Logf("model_info: arch=%s quant=%s context=%d", modelInfo.Arch, modelInfo.Quantization, modelInfo.ContextLimit)
	}

	if usage == nil {
		t.Fatalf("no usage event: stream_options.include_usage is not reaching LM Studio")
	}
	t.Logf("usage: prompt=%d completion=%d total=%d cached=%d",
		usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, usage.CachedPromptTokens)
	if usage.PromptTokens <= 0 {
		t.Errorf("prompt_tokens=%d, want a measured value > 0 (context meter depends on it)", usage.PromptTokens)
	}
	if usage.CompletionTokens <= 0 {
		t.Errorf("completion_tokens=%d, want > 0", usage.CompletionTokens)
	}

	if stats == nil {
		t.Logf("no stats event; the engine falls back to estimation (Source=%q)", SourceEstimated)
	} else {
		t.Logf("stats: tok/s=%.2f source=%s ttft=%dms gen=%dms stop=%q",
			stats.TokensPerSecond, stats.Source, stats.TimeToFirstTokenMs, stats.GenerationMs, stats.StopReason)
		if stats.TokensPerSecond <= 0 && usage.CompletionTokens > 0 {
			t.Logf("backend did not report tok/s; engine estimates from wall time (%.2f tok/s)",
				float64(usage.CompletionTokens)/wall.Seconds())
		}
	}
	if text == "" {
		t.Errorf("no assistant text received")
	}
}

// pickLiveModel returns the requested model id when present, else the smallest
// loaded chat model, else the smallest chat model overall. Embedding models are
// never eligible.
func pickLiveModel(models []ModelInfo, want string) ModelInfo {
	if want != "" {
		for _, m := range models {
			if m.ID == want {
				return m
			}
		}
	}
	chat := func(m ModelInfo) bool {
		return m.Type == "llm" || m.Type == "vlm" || m.Type == ""
	}
	var loaded, fallback ModelInfo
	score := func(m ModelInfo) int {
		if m.Type == "llm" {
			return 0
		}
		return 1
	}
	for _, m := range models {
		if !chat(m) {
			continue
		}
		if m.State == StateLoaded && (loaded.ID == "" || score(m) < score(loaded)) {
			loaded = m
		}
		if fallback.ID == "" || score(m) < score(fallback) {
			fallback = m
		}
	}
	if loaded.ID != "" {
		return loaded
	}
	return fallback
}

func firstHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
