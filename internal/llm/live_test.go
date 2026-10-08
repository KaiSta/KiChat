package llm

import (
	"context"
	"os"
	"sort"
	"strings"
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

// TestLiveNativeReasoningOffIsTheRouteThatMakesASummaryPossible settles the one
// claim only a real server can: that asking LM Studio's native route for
// `reasoning: "off"` makes a reasoning model write an answer it would otherwise
// spend its whole output budget thinking past. Both halves are measured — the
// same prompt with the setting and without it — because "the summary is not
// empty" on its own would also hold for a model that never reasons.
//
// The strict assertions are on the reasoning-off half, which is the route the
// summariser now takes. The comparator is reported rather than asserted: whether
// a model thinks its way past a small cap is a property of that model, and the
// point of the test is to show the route works, not to require a particular
// model to be bad at budgeting.
func TestLiveNativeReasoningOffProducesASummary(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to run against a live backend")
	}
	base := os.Getenv("KICHAT_LMSTUDIO")
	if base == "" {
		base = "http://localhost:1234"
	}
	p := newLMStudioProvider(base, "")

	// A 4B model on a CPU measured ~0.3 tokens/s here, and a cold start JIT-loads
	// it, so this is deliberately not a quick test.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	modelID := liveReasoningModel(t, ctx, p)
	t.Logf("model=%s reasoning=%q", modelID, p.reasoningChoice[modelID])

	messages := []Message{
		{Role: RoleSystem, Content: "Du komprimierst einen Verlauf zu einem Gedächtnis. Antworte knapp."},
		{Role: RoleUser, Content: "<conversation>\nNutzer: Hauptstadt von Peru?\n" +
			"Assistent: Lima.\n</conversation>\n\nKomprimiere den Verlauf zu einem Gedächtnis."},
	}

	// Half one: the route the summariser takes.
	off := false
	stream, err := p.Chat(ctx, ChatRequest{Model: modelID, Messages: messages, MaxTokens: 64, Think: &off})
	if err != nil {
		t.Fatalf("reasoning-off chat: %v", err)
	}
	var text strings.Builder
	var usage *Usage
	var stats *GenerationStats
	for ev := range stream {
		switch ev.Type {
		case EventDelta:
			text.WriteString(ev.Text)
		case EventReasoning:
			// Expected to stay silent on this route; recorded for the log.
		case EventUsage:
			usage = ev.Usage
		case EventDone:
			stats = ev.Stats
		case EventError:
			t.Fatalf("reasoning-off stream error: %s", ev.Error)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		t.Fatalf("reasoning off produced no answer at all (usage=%+v stats=%+v)", usage, stats)
	}
	if usage == nil {
		t.Fatal("no usage from a route that reports reasoning_output_tokens")
	}
	if usage.ReasoningTokens != 0 {
		t.Errorf("reasoning tokens = %d, want 0 with the setting off", usage.ReasoningTokens)
	}
	if stats == nil || stats.Source != SourceLMStudio {
		t.Errorf("stats = %+v, want LM Studio's own numbers", stats)
	}
	t.Logf("reasoning off: %d in / %d out (%d reasoning), stop=%q, text=%q",
		usage.PromptTokens, usage.CompletionTokens, usage.ReasoningTokens,
		stats.StopReason, firstHead(text.String(), 200))

	// Half two: the same request as an ordinary (OpenAI-compatible) one, which is
	// what compaction did before and cannot switch thinking off.
	stream, err = p.Chat(ctx, ChatRequest{Model: modelID, Messages: messages, MaxTokens: 64})
	if err != nil {
		t.Fatalf("compat chat: %v", err)
	}
	var plain strings.Builder
	var plainReasoning strings.Builder
	var plainUsage *Usage
	var plainStats *GenerationStats
	for ev := range stream {
		switch ev.Type {
		case EventDelta:
			plain.WriteString(ev.Text)
		case EventReasoning:
			plainReasoning.WriteString(ev.Text)
		case EventUsage:
			plainUsage = ev.Usage
		case EventDone:
			plainStats = ev.Stats
		case EventError:
			t.Fatalf("compat stream error: %s", ev.Error)
		}
	}
	if plainUsage == nil || plainStats == nil {
		t.Fatalf("compat route reported no numbers (usage=%+v stats=%+v)", plainUsage, plainStats)
	}
	t.Logf("thinking left on: %d in / %d out (%d reasoning reported), stop=%q, text=%q, reasoning=%q",
		plainUsage.PromptTokens, plainUsage.CompletionTokens, plainUsage.ReasoningTokens,
		plainStats.StopReason, firstHead(plain.String(), 200), firstHead(plainReasoning.String(), 200))
	// A model that reasons to the last token and writes no answer is the reported
	// failure — visible here as reasoning without text. Either way something must
	// have come back, which is what makes this half a comparator rather than a
	// second thing that could fail for its own reasons.
	if strings.TrimSpace(plain.String()) == "" && strings.TrimSpace(plainReasoning.String()) == "" {
		t.Errorf("the ordinary request returned neither an answer nor reasoning: %+v", plainUsage)
	}
}

// liveReasoningModel picks a model that publishes a reasoning configuration,
// preferring one that is already loaded so the test does not pay for a load it
// does not need. It skips rather than fails when the server has none, because a
// machine with only non-reasoning models cannot answer the question this test
// asks.
func liveReasoningModel(t *testing.T, ctx context.Context, p *openAICompatProvider) string {
	t.Helper()
	p.loadReasoningChoices(ctx)
	if len(p.reasoningChoice) == 0 {
		t.Skip("no model on this LM Studio publishes a reasoning configuration")
	}
	models, err := p.ListModels(ctx)
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	ids := make([]string, 0, len(p.reasoningChoice))
	for id := range p.reasoningChoice {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, m := range models {
		if m.State != StateLoaded {
			continue
		}
		if _, ok := p.reasoningChoice[m.ID]; ok {
			return m.ID
		}
	}
	return ids[0]
}
