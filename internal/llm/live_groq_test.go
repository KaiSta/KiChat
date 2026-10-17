package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveGroq exercises the provider against the real api.groq.com API. It is
// skipped unless KICHAT_LIVE=1 and KICHAT_GROQ_KEY are both set, so the normal
// suite stays hermetic and no metered request is ever made by accident.
//
//	KICHAT_LIVE=1 KICHAT_GROQ_KEY=<key> go test ./internal/llm/ -run TestLiveGroq -v
//
// KICHAT_GROQ_KEY is passed in the environment on purpose: a key belongs nowhere
// near a tracked file.
//
//   - KICHAT_GROQ_BASE overrides the address.
//   - KICHAT_GROQ_MODEL picks the model to drive. It defaults to
//     `openai/gpt-oss-20b`, which publishes tools and reasoning and answered the
//     probe.
//   - KICHAT_GROQ_VISION_MODEL adds the image check: this API accepts image input
//     as a data URL, and measured, it refuses a 1x1 PNG with `invalid image data`
//     while accepting a 128x128 one. Unset, the check is skipped rather than
//     sending an image to a model that cannot read it.
//
// The budget matters here in a way it does not for the other hosted backends:
// this account's token-per-minute allowance is small (measured 8000, reported by
// every response's x-ratelimit-* headers), and one 128x128 PNG costs about 1300
// prompt tokens on its own. Every request below is therefore short, and the image
// check is opt-in.
func TestLiveGroq(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to run against a live backend")
	}
	key := os.Getenv("KICHAT_GROQ_KEY")
	if key == "" {
		t.Skip("set KICHAT_GROQ_KEY to run against the live api.groq.com API")
	}
	p := newGroqProvider("groq", "Groq", os.Getenv("KICHAT_GROQ_BASE"), key)

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
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	t.Logf("catalogue: %d chat models", len(models))

	// The filter's evidence, checked against the live catalogue: these are the
	// entries the listing itself says are not chat models (output modalities of
	// `transcription` and `speech`), and asking one to chat is refused with HTTP
	// 400 "does not support chat completions". If one of them is offered, the
	// filter has stopped reading what the vendor publishes.
	for _, notChat := range []string{
		"whisper-large-v3",
		"whisper-large-v3-turbo",
		"canopylabs/orpheus-arabic-saudi",
		"canopylabs/orpheus-v1-english",
	} {
		if _, ok := byID[notChat]; ok {
			t.Errorf("%s is not a chat model (the API refuses chat completions for it) but was offered", notChat)
		}
	}

	// The classification entries are the other half of that decision, and the one
	// that costs a user something: the listing offers them (text to text, active,
	// a real window) and the provider keeps them, because nothing the listing
	// publishes says what they are — measured, asking one to chat without
	// `stream` answers HTTP 200 with a score, and only `stream: true` is refused,
	// with HTTP 400 "text classification models do not support streaming". A
	// streaming-only client therefore has two rows in the picker that can only
	// fail, and the vendor's sentence is what the user sees. Asserted so the
	// documentation of that gap cannot go stale.
	classifiers := 0
	for _, id := range []string{"meta-llama/llama-prompt-guard-2-22m", "meta-llama/llama-prompt-guard-2-86m"} {
		m, ok := byID[id]
		if !ok {
			continue
		}
		classifiers++
		if m.State != StateLoaded {
			t.Errorf("%s = %+v, want it offered as a ready row like any other", id, m)
		}
		if m.ContextLimit <= 0 {
			t.Errorf("%s window = %d, want the published one", id, m.ContextLimit)
		}
		// The exact call the chat engine makes for this row.
		_, err := p.Chat(ctx, ChatRequest{
			Model:     id,
			Messages:  []Message{{Role: RoleUser, Content: "hi"}},
			MaxTokens: 16,
		})
		var he *HTTPError
		if !errors.As(err, &he) || he.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: streaming chat answered %v, want HTTP 400 — if this model now streams, the row stopped being a trap and the docs should say so", id, err)
			continue
		}
		if !strings.Contains(he.Body, "do not support streaming") {
			t.Errorf("%s refusal = %q, want the vendor's \"do not support streaming\"", id, strings.TrimSpace(he.Body))
		}
		t.Logf("classification row %s: window=%d caps=%v -> %d %s",
			id, m.ContextLimit, m.Capabilities, he.StatusCode, strings.TrimSpace(he.Body))
	}
	if classifiers == 0 {
		t.Error("no text-classification entry was listed; the keep-decision this provider documents no longer has a live subject")
	}

	want := os.Getenv("KICHAT_GROQ_MODEL")
	if want == "" {
		want = "openai/gpt-oss-20b"
	}
	chosen, ok := byID[want]
	if !ok {
		t.Fatalf("model %q is not in the catalogue", want)
	}
	if chosen.State != StateLoaded {
		t.Errorf("state = %q, want %q: a hosted model is ready without a load", chosen.State, StateLoaded)
	}
	if chosen.Publisher == "" {
		t.Errorf("model %q reports no publisher, so owned_by was not mapped", chosen.ID)
	}
	// The opposite assertion to NVIDIA's, and the fact worth having: this listing
	// publishes each model's window, so the context meter works without an
	// override. A zero here would mean the provider stopped reading it.
	if chosen.ContextLimit <= 0 {
		t.Errorf("window = %d, want the published context_window", chosen.ContextLimit)
	}
	t.Logf("model=%s publisher=%s ctx=%d caps=%v type=%s state=%s",
		chosen.ID, chosen.Publisher, chosen.ContextLimit, chosen.Capabilities, chosen.Type, chosen.State)

	// A short answer, with the usage frame the context meter needs. The budget is
	// modest because this model thinks before it answers and the account's token
	// allowance is the tight resource here.
	stream, err := p.Chat(ctx, ChatRequest{
		Model:       chosen.ID,
		Messages:    []Message{{Role: RoleUser, Content: "Reply with exactly: OK"}},
		MaxTokens:   512,
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
	// Reasoning is reported, not asserted: whether a model thinks on a given turn
	// is the model's choice, and the claim in the listing is what the provider
	// acts on. That it arrives in `reasoning` at all is asserted by the delta
	// count being readable here.
	t.Logf("content=%q reasoning=%d chars usage=%+v stop=%v",
		strings.TrimSpace(content.String()), reasoning.Len(), usage, stats)
	if content.Len() == 0 && reasoning.Len() == 0 {
		t.Error("the stream produced neither an answer nor reasoning")
	}
	if usage == nil || usage.PromptTokens == 0 {
		t.Errorf("usage = %+v, want a measured prompt_tokens from stream_options.include_usage", usage)
	}
	if stats == nil || stats.StopReason == "" {
		t.Errorf("stats = %+v, want the finish reason the API reported", stats)
	}

	// The per-request budget, read off an ordinary answer rather than out of a
	// refusal. Every response of this API carries `x-ratelimit-limit-tokens` — the
	// tokens-per-minute allowance of the model that answered — and reading it is what
	// lets the *second* oversized turn of a session be folded instead of sent. A zero
	// here means the header stopped being sent, arrived under another name, or is not
	// visible on a streamed response, and the app is back to learning the limit only
	// from a refusal. Reported rather than asserted-with-a-number, because the value
	// is the account's allowance and not a constant.
	ceil := p.RequestCeiling(chosen.ID)
	if ceil.Tokens <= 0 {
		t.Errorf("no per-request ceiling was learned from a successful answer: %s was not read",
			rateLimitTokensHeader)
	} else {
		t.Logf("ceiling from the answer's own response: %d tokens per request (%s), window %d",
			ceil.Tokens, ceil.Source, chosen.ContextLimit)
	}
	if ceil.Source != "" && ceil.Source != CeilingFromHeader {
		t.Errorf("ceiling source = %q, want %q after an ordinary answer", ceil.Source, CeilingFromHeader)
	}

	// And the minute the same response described, which is what a turn is paced
	// against: what is left of the allowance and how long that lasts. The two names
	// and the duration format (`7.66s`, `2m59.56s`) are what the documentation shows,
	// and this is the one place they are read against a real response — so a renamed
	// header or a reset sent as something other than a duration fails here rather than
	// quietly restoring the old behaviour of learning the budget from a refusal.
	// Reported rather than asserted with numbers: the allowance is the account's.
	if budget, ok := p.budgetFor(chosen.ID); !ok {
		t.Errorf("no budget was read from a successful answer: %s / %s were not read",
			rateLimitRemainingTokensHeader, rateLimitResetTokensHeader)
	} else {
		t.Logf("minute from the answer's own response: %d of %d tokens left, resetting in %s",
			budget.Remaining, budget.Limit, budget.Reset)
		if budget.Limit > 0 && budget.Remaining > budget.Limit {
			t.Errorf("remaining %d > limit %d, so one of the two was misread",
				budget.Remaining, budget.Limit)
		}
		if budget.Limit > 0 && ceil.Tokens > 0 && budget.Limit != ceil.Tokens {
			// One header family, one allowance: the tokens-per-minute limit is also the
			// largest single request, and two readings of it disagreeing means one of
			// them is wrong — which would make the meter and the fold disagree too.
			t.Errorf("limit-tokens read as %d for the ceiling and %d for the minute: one header, two answers",
				ceil.Tokens, budget.Limit)
		}
	}

	// The tool claim, which on this API comes from the model's own
	// `supported_features`. It is only asserted for a model that claims it.
	if chosen.Has(CapTools) {
		toolStream, err := p.Chat(ctx, ChatRequest{
			Model:     chosen.ID,
			Messages:  []Message{{Role: RoleUser, Content: "What is the weather in Berlin? You must call get_weather."}},
			MaxTokens: 1024,
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
			t.Fatalf("no tool call streamed, but the listing claims tools for %s", chosen.ID)
		}
		if calls[0].Name != "get_weather" {
			t.Errorf("tool name = %q, want get_weather", calls[0].Name)
		}
		if !strings.Contains(calls[0].Arguments, "Berlin") {
			t.Errorf("tool arguments = %q, want the city", calls[0].Arguments)
		}
		t.Logf("tool call=%s arguments=%s", calls[0].Name, calls[0].Arguments)
	}

	// The image route, opt-in because of its cost and because it needs a model
	// that reads images. The picture is the 128x128 red PNG the measurement used:
	// a 1x1 one is refused with "invalid image data", so a tiny fixture would fail
	// here for the wrong reason.
	if vision := os.Getenv("KICHAT_GROQ_VISION_MODEL"); vision != "" {
		vm, ok := byID[vision]
		if !ok {
			t.Fatalf("vision model %q is not in the catalogue", vision)
		}
		if !vm.Has(CapVision) {
			t.Errorf("model %q claims no vision, but the listing marks it an image input", vision)
		}
		red := liveRedPNG(t)
		vStream, err := p.Chat(ctx, ChatRequest{
			Model:     vision,
			MaxTokens: 64,
			Messages: []Message{{
				Role:    RoleUser,
				Content: "What colour is this image? Answer with one word.",
				Images:  []string{red},
			}},
		})
		if err != nil {
			t.Fatalf("vision chat: %v", err)
		}
		var answer strings.Builder
		for ev := range vStream {
			switch ev.Type {
			case EventDelta:
				answer.WriteString(ev.Text)
			case EventError:
				t.Fatalf("vision stream error: %s", ev.Error)
			}
		}
		t.Logf("vision model=%s answer=%q", vision, strings.TrimSpace(answer.String()))
		if answer.Len() == 0 {
			t.Errorf("the image request produced no answer at all")
		}
	}

	// A wrong key is a key, not a throttle: one request, no tokens, and the
	// vendor's own code in the message the user sees.
	bad := newGroqProvider("groq", "Groq", os.Getenv("KICHAT_GROQ_BASE"), "gsk_not-a-real-key")
	_, err = bad.ListModels(ctx)
	var he *HTTPError
	if err == nil {
		t.Fatal("a request with a fake key was accepted")
	}
	if !errors.As(err, &he) || he.StatusCode != 401 {
		t.Fatalf("a fake key answered %v, want HTTP 401", err)
	}
	if !strings.Contains(he.Body, "invalid_api_key") {
		t.Errorf("refusal body = %q, want the vendor's code kept", he.Body)
	}
	t.Logf("bad key: %d %s", he.StatusCode, strings.TrimSpace(he.Body))
}

// TestLiveGroqOversizedRequestIsRecognised asks the one question the offline fixture
// cannot answer: whether the real envelope for a request that is too big for the
// account is the shape the classifier reads. Every fact the fixture rests on came from
// published bug reports rather than from a request made here, so this is the measurement
// that would turn the fixture into a mistake if it were wrong.
//
// The request it sends is deliberately oversized — about 38k tokens against a free-tier
// allowance of 8k — because a request bigger than the whole per-minute budget is the one
// thing waiting cannot fix, which is the distinction being measured. One request is sent
// and no retry follows; whether a *refused* request is counted against the budget is not
// something the vendor documents, so treat this as up to one minute's allowance spent.
// An account whose allowance exceeds the probe has nothing to measure, and says so
// instead of failing.
func TestLiveGroqOversizedRequestIsRecognised(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to run against a live backend")
	}
	key := os.Getenv("KICHAT_GROQ_KEY")
	if key == "" {
		t.Skip("set KICHAT_GROQ_KEY to run against the live api.groq.com API")
	}
	model := os.Getenv("KICHAT_GROQ_MODEL")
	if model == "" {
		model = "openai/gpt-oss-20b"
	}
	p := newGroqProvider("groq", "Groq", os.Getenv("KICHAT_GROQ_BASE"), key)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	filler := strings.Repeat("Der Verlauf ist lang. ", 6000)
	_, err := p.Chat(ctx, ChatRequest{
		Model:     model,
		Messages:  []Message{{Role: RoleUser, Content: "Fasse das zusammen: " + filler}},
		MaxTokens: 16,
	})
	if err == nil {
		t.Skip("this account accepted a ~38k-token request, so its allowance is larger " +
			"than the probe and there is nothing to measure")
	}
	too, ok := RequestTooLarge(err)
	if !ok {
		t.Fatalf("an oversized request answered %v, which is not the shape the classifier reads", err)
	}
	kept := p.RequestCeiling(model)
	t.Logf("oversized request refused: status %v, limit %d, requested %d, ceiling kept %d (%s)",
		err, too.Limit, too.Requested, kept.Tokens, kept.Source)

	// A model with a small window answers this with its context-window refusal
	// instead, which is classified the same way and states no numbers. Both are facts;
	// what is asserted is that neither comes back as a throttle.
	if too.Limit == 0 {
		t.Log("the refusal stated no limit, which is this model's window rather than its " +
			"budget: the TPM shape was not exercised")
		return
	}
	if too.Requested <= 0 {
		t.Errorf("the refusal named a limit of %d but no requested figure: %v", too.Limit, err)
	}
	// The kept ceiling is the tighter of the refusal's number and whatever the same
	// response's budget header said, so it is asserted as never looser than the limit
	// the refusal named — an equality here would be a claim about which of the two
	// statements the vendor happens to send for this account's current tier.
	if kept.Tokens <= 0 || kept.Tokens > too.Limit {
		t.Errorf("ceiling kept = %d (%s), want a positive number no larger than the %d the refusal named",
			kept.Tokens, kept.Source, too.Limit)
	}
	if retryable(err) {
		t.Error("the oversized request would be retried, which spends the budget it exceeds")
	}
}

// TestLiveGroqShippedEntryReachesTheAPI checks the wiring rather than the mapper: the
// entry `DefaultBackendConfigs` ships, switched on and given a key, has to produce a
// provider through `NewRegistry` that lists models and answers — which is what a user
// who pastes a key into the panel and ticks the box actually gets.
func TestLiveGroqShippedEntryReachesTheAPI(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to run against a live backend")
	}
	key := os.Getenv("KICHAT_GROQ_KEY")
	if key == "" {
		t.Skip("set KICHAT_GROQ_KEY to run against the live api.groq.com API")
	}
	var shipped BackendConfig
	for _, c := range DefaultBackendConfigs() {
		if c.ID == "groq" {
			shipped = c
		}
	}
	if shipped.Kind == "" {
		t.Fatal("Groq is not among the shipped backends")
	}
	// As the panel leaves it after a key is pasted and the box is ticked. The
	// address is the shipped one on purpose: this is the address the app would
	// really call, so the base-URL completion is not what is being tested here.
	shipped.Enabled = true
	shipped.APIKey = key

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	reg := NewRegistry([]BackendConfig{shipped})
	p, ok := reg.Provider("groq")
	if !ok {
		t.Fatal("the shipped entry built no provider: Reconfigure has no case for that kind")
	}
	models, err := p.ListModels(ctx)
	if err != nil {
		t.Fatalf("the shipped address did not answer: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("the shipped entry listed nothing, so the picker would be empty")
	}
	// The model to drive is picked the way a user would have to: not
	// `models[0]`, because measured, the listing leads with a
	// text-classification entry that refuses `stream: true`. `KICHAT_GROQ_MODEL`
	// wins, then the model the rest of this file defaults to, then the first row
	// whose published features claim tools — on this API that claim comes from the
	// vendor and is only made for a model that answers chat completions.
	want := os.Getenv("KICHAT_GROQ_MODEL")
	if want == "" {
		want = "openai/gpt-oss-20b"
	}
	chosen := -1
	for i, m := range models {
		if m.ID == want {
			chosen = i
			break
		}
	}
	if chosen < 0 {
		for i, m := range models {
			if m.Has(CapTools) {
				chosen = i
				break
			}
		}
	}
	if chosen < 0 {
		t.Fatalf("no model in the listing claims tools, so there is nothing to drive: %+v", models)
	}
	// One real answer through the wiring, not just a listing: this is the path the
	// chat engine takes when the user picks a Groq model.
	stream, err := p.Chat(ctx, ChatRequest{
		Model:     models[chosen].ID,
		Messages:  []Message{{Role: RoleUser, Content: "Reply with exactly: OK"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("chat through the registry: %v", err)
	}
	var answer strings.Builder
	for ev := range stream {
		switch ev.Type {
		case EventDelta, EventReasoning:
			answer.WriteString(ev.Text)
		case EventError:
			t.Fatalf("stream error through the registry: %s", ev.Error)
		}
	}
	if answer.Len() == 0 {
		t.Error("the shipped entry streamed no answer")
	}
	t.Logf("shipped entry: %s (%s) listed %d chat models, drove %s, answered %q",
		shipped.Name, shipped.BaseURL, len(models), models[chosen].ID, strings.TrimSpace(answer.String()))
}

// liveRedPNG draws the 128x128 red picture the image check sends, base64-encoded
// the way a Message carries it.
//
// The size is the point: measured on this API, a 1x1 PNG is refused with HTTP 400
// `invalid image data` while a 128x128 one is read and answered correctly. A
// fixture written for another backend's tests would therefore fail here for a
// reason that has nothing to do with this provider.
func liveRedPNG(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode the test picture: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
