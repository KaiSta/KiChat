package llm

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveMistral exercises the provider against the real service. It is skipped
// unless KICHAT_LIVE=1 and KICHAT_MISTRAL_KEY are both set, so the normal suite
// stays hermetic and no metered request is ever made by accident.
//
// KICHAT_MISTRAL_KEY is passed in the environment on purpose: a key belongs
// nowhere near a tracked file.
//
//	KICHAT_LIVE=1 KICHAT_MISTRAL_KEY=<key> go test ./internal/llm/ -run TestLiveMistral -v
//
// Four claims are checked, each of which the offline tests can only fake:
//
//   - the listing is not a chat catalogue. Mistral publishes its embedding,
//     moderation, OCR, speech and transcription models in the same array, and the
//     filter is asserted from both sides: every listed model reports a context
//     window, and the embedding ids the API does publish are absent from what the
//     provider offers.
//   - `stream_options.include_usage` is honoured, so the context meter gets a
//     measured prompt_tokens instead of an estimate.
//   - a tool call streams, which is what the provider's CapTools claim rests on.
//   - a reasoning model sends its thinking as chunks inside `content` rather than
//     in a field of the delta — the shape deltaContent exists to decode. This one
//     needs a model the account may call, so a refusal is reported as an unchecked
//     claim rather than a failure: measured on the free tier, `ministral-*` answers
//     while `mistral-small-latest` is refused with HTTP 429 and a Labs model with
//     HTTP 403, both naming the gate in the vendor's own envelope.
func TestLiveMistral(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to run against a live backend")
	}
	key := os.Getenv("KICHAT_MISTRAL_KEY")
	if key == "" {
		t.Skip("set KICHAT_MISTRAL_KEY to run against the live Mistral API")
	}
	base := os.Getenv("KICHAT_MISTRAL_BASE")
	p := newMistralProvider("mistral", "Mistral", base, key)

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

	// The filter, asserted from both sides. Every model offered has to carry the
	// metadata the planning needs, and the models that cannot hold a conversation
	// have to be missing even though the API publishes them.
	for _, m := range models {
		if m.ContextLimit <= 0 {
			t.Errorf("model %q reports no context window; the listing mapping is wrong", m.ID)
		}
		if m.State != StateLoaded {
			t.Errorf("model %q is %q, want %q for a hosted model", m.ID, m.State, StateLoaded)
		}
	}
	raw := liveModelIDs(ctx, t, p)
	t.Logf("listed %d chat models out of %d published", len(models), len(raw))
	if len(raw) <= len(models) {
		t.Errorf("the listing filtered nothing, but this API publishes non-chat models: %d offered, %d published", len(models), len(raw))
	}
	for _, id := range raw {
		if strings.Contains(id, "embed") || strings.Contains(id, "moderation") {
			if offered(models, id) {
				t.Errorf("%q was offered as a chat model", id)
			}
		}
	}

	chosen := liveMistralChatModel(t, models)
	t.Logf("chosen model=%s ctx=%d type=%s caps=%v", chosen.ID, chosen.ContextLimit, chosen.Type, chosen.Capabilities)

	// include_usage: the measured prompt_tokens is the whole reason the request
	// asks for it, because without it the context meter can only ever estimate.
	stream, err := p.Chat(ctx, ChatRequest{
		Model:       chosen.ID,
		Messages:    []Message{{Role: RoleUser, Content: "Reply with exactly: OK"}},
		MaxTokens:   64,
		Temperature: 0.2,
	})
	if err != nil {
		liveMistralChatRefusal(t, chosen.ID, err)
		return
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
	if usage == nil || usage.PromptTokens == 0 {
		t.Errorf("usage = %+v, want a measured prompt_tokens", usage)
	}
	t.Logf("content=%q reasoning=%d chars usage=%+v stats=%+v", strings.TrimSpace(content.String()), reasoning.Len(), usage, stats)

	// The tool claim the listing mapping encodes. One small call, and the model is
	// told the tool is the only way to answer.
	toolStream, err := p.Chat(ctx, ChatRequest{
		Model:     chosen.ID,
		Messages:  []Message{{Role: RoleUser, Content: "What is the weather in Berlin? You must call get_weather."}},
		MaxTokens: 512,
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
		liveMistralChatRefusal(t, chosen.ID, err)
		return
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
		t.Fatalf("no tool call streamed from %q, but the provider claims CapTools for it", chosen.ID)
	}
	if calls[0].Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", calls[0].Name)
	}
	if !strings.Contains(calls[0].Arguments, "Berlin") {
		t.Errorf("tool arguments = %q, want the city", calls[0].Arguments)
	}
	t.Logf("tool call=%s arguments=%s", calls[0].Name, calls[0].Arguments)

	liveMistralThinkingChunks(ctx, t, p, models)
}

// liveMistralChatModel picks the model the turn runs on: the one named in
// KICHAT_MISTRAL_MODEL when it is set, otherwise the cheapest entry the listing
// offers. The order matters on a metered account — the listing is sorted with the
// smallest models first for exactly that reason.
func liveMistralChatModel(t *testing.T, models []ModelInfo) ModelInfo {
	t.Helper()
	if want := os.Getenv("KICHAT_MISTRAL_MODEL"); want != "" {
		for _, m := range models {
			if m.ID == want {
				return m
			}
		}
		t.Fatalf("KICHAT_MISTRAL_MODEL=%q is not among the models the API listed", want)
	}
	// A model the platform gates behind a payment plan must not be the one the
	// test spends its budget failing on, so an explicitly cheap family is
	// preferred before anything else.
	for _, prefix := range []string{"ministral-3b", "ministral-8b", "voice", "mistral-small"} {
		for _, m := range models {
			if strings.HasPrefix(m.ID, prefix) {
				return m
			}
		}
	}
	return models[0]
}

// liveMistralReasoningModel names the model used for the reasoning check, so a
// machine whose account can reach a reasoning model can exercise that shape.
func liveMistralReasoningModel(models []ModelInfo) (ModelInfo, bool) {
	if want := os.Getenv("KICHAT_MISTRAL_REASONING_MODEL"); want != "" {
		for _, m := range models {
			if m.ID == want {
				return m, true
			}
		}
		return ModelInfo{}, false
	}
	for _, m := range models {
		if m.Has(CapThink) && !strings.HasPrefix(m.ID, "labs-") {
			return m, true
		}
	}
	return ModelInfo{}, false
}

// liveMistralThinkingChunks checks the reason this provider has its own delta
// handling: a reasoning model here streams its thinking as typed chunks inside
// `content`, so a reply whose text arrives that way would be empty without the
// decoding in deltaContent. It is the one claim in this test that needs a model
// the account may call, so an unreachable one is reported rather than failed.
func liveMistralThinkingChunks(ctx context.Context, t *testing.T, p *openAICompatProvider, models []ModelInfo) {
	t.Helper()
	model, ok := liveMistralReasoningModel(models)
	if !ok {
		t.Log("no reasoning model in the listing; the thinking-chunk shape was not checked")
		return
	}
	stream, err := p.Chat(ctx, ChatRequest{
		Model:     model.ID,
		Messages:  []Message{{Role: RoleUser, Content: "What is 17*23? Think it through."}},
		MaxTokens: 1024,
	})
	if err != nil {
		// A tier gate is not a fact about the integration. It is reported with
		// the vendor's own wording so the gap is visible and named.
		var he *HTTPError
		if errors.As(err, &he) && (he.StatusCode == http.StatusTooManyRequests || he.StatusCode == http.StatusForbidden) {
			t.Logf("thinking chunks not checked: %s is refused for this account (HTTP %d: %s)", model.ID, he.StatusCode, he.Body)
			return
		}
		t.Fatalf("reasoning chat against %q: %v", model.ID, err)
	}
	var content, reasoning strings.Builder
	for ev := range stream {
		switch ev.Type {
		case EventDelta:
			content.WriteString(ev.Text)
		case EventReasoning:
			reasoning.WriteString(ev.Text)
		case EventError:
			t.Fatalf("reasoning stream error: %s", ev.Error)
		}
	}
	if reasoning.Len() == 0 {
		t.Errorf("%q advertised reasoning but streamed no thinking content", model.ID)
	}
	if content.Len() == 0 {
		t.Errorf("%q streamed thinking but no answer text, which is what a mistyped content shape looks like", model.ID)
	}
	t.Logf("reasoning model=%s thinking=%d chars answer=%q", model.ID, reasoning.Len(), strings.TrimSpace(content.String()))
}

// liveMistralChatRefusal reports a chat call the account is not allowed to make.
// The vendor answers those with HTTP 429 and a rate_limited envelope, or HTTP 403
// and a Labs-model envelope; neither is a defect in this provider, and neither is
// a reason to pretend the turn was verified.
func liveMistralChatRefusal(t *testing.T, model string, err error) {
	t.Helper()
	var he *HTTPError
	if errors.As(err, &he) && (he.StatusCode == http.StatusTooManyRequests || he.StatusCode == http.StatusForbidden) {
		// A skip is only honest while nothing has gone wrong yet: skipped and
		// failed at once would let this bury a claim that already did not hold,
		// and the run would read as "not checked" rather than as a defect.
		if t.Failed() {
			t.Logf("%s is refused for this account (HTTP %d: %s), so the remaining claims were not checked", model, he.StatusCode, he.Body)
			return
		}
		t.Skipf("%s is refused for this account (HTTP %d: %s) — the remaining claims were not checked", model, he.StatusCode, he.Body)
	}
	t.Fatalf("chat against %q: %v", model, err)
}

// liveModelIDs reads the raw listing, so the filter can be checked against what
// the API really publishes rather than against a copy of the fixture.
func liveModelIDs(ctx context.Context, t *testing.T, p *openAICompatProvider) []string {
	t.Helper()
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := doJSON(ctx, jsonClient, http.MethodGet, p.modelsPath, p.authHeaders(), nil, &resp); err != nil {
		t.Fatalf("raw listing: %v", err)
	}
	ids := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

func offered(models []ModelInfo, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// TestLiveMistralTemperatureAboveThePublishedMaximum settles a question the
// offline tests cannot. Mistral's request schema bounds `temperature` to
// `[0, 1.5]` while the app's own Settings screen offers up to `2`, and nothing
// in between clamps the value, so a user who drags the slider to the top can
// construct a body the vendor's schema forbids. Whether the deployed service
// enforces that bound decides whether this is a reachable 422 or harmless schema
// drift — and only a key can answer it.
//
// It reports the outcome rather than asserting one: either answer is a fact
// worth having, and inventing a failure for the expected one would be guessing
// in a test's clothing. One metered request, one token.
//
//	KICHAT_LIVE=1 KICHAT_MISTRAL_KEY=<key> go test ./internal/llm/ -run TestLiveMistralTemperature -v
func TestLiveMistralTemperatureAboveThePublishedMaximum(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to run against a live backend")
	}
	key := os.Getenv("KICHAT_MISTRAL_KEY")
	if key == "" {
		t.Skip("set KICHAT_MISTRAL_KEY to run against the live Mistral API")
	}
	p := newMistralProvider("mistral", "Mistral", os.Getenv("KICHAT_MISTRAL_BASE"), key)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	models, err := p.ListModels(ctx)
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	chosen := liveMistralChatModel(t, models)

	stream, err := p.Chat(ctx, ChatRequest{
		Model:       chosen.ID,
		Messages:    []Message{{Role: RoleUser, Content: "Reply with exactly: OK"}},
		MaxTokens:   1,
		Temperature: 1.8,
	})
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.StatusCode == http.StatusTooManyRequests || he.StatusCode == http.StatusForbidden) {
			t.Skipf("%s is refused for this account (HTTP %d: %s)", chosen.ID, he.StatusCode, he.Body)
		}
		if errors.As(err, &he) && (he.StatusCode == http.StatusBadRequest || he.StatusCode == http.StatusUnprocessableEntity) {
			t.Logf("temperature 1.8 is REJECTED for %s (HTTP %d: %s): the published maximum is enforced, Settings offers 2 and nothing clamps it — a reachable 422, so the UI bound has to come down to 1.5",
				chosen.ID, he.StatusCode, he.Body)
			return
		}
		t.Fatalf("chat against %q: %v", chosen.ID, err)
	}
	for ev := range stream {
		if ev.Type == EventError {
			t.Fatalf("stream error: %s", ev.Error)
		}
	}
	t.Logf("temperature 1.8 is ACCEPTED for %s: the published maximum of 1.5 is not enforced, so the Settings bound of 2 costs no rejected request (it may still be clamped silently)",
		chosen.ID)
}
