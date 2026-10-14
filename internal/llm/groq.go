package llm

import (
	"encoding/json"
	"strings"
)

// Groq is a hosted, OpenAI-compatible service, so chat, streaming, tool calls
// and usage all go through the shared path. What is specific to it is the
// address, the fact that it cannot answer without a key, and a model listing
// that is the richest of the hosted backends: it states each model's context
// window, its modalities and which features it supports.
//
// The shapes below were read off the live service rather than assumed:
//
//   - GET /openai/v1/models answers 11 entries for the account measured, and the
//     fields are `id`, `object`, `created`, `owned_by`, `active`, `context_window`,
//     `max_completion_tokens`, `context_length`, `max_output_length`, `name`,
//     `hugging_face_id`, `input_modalities`, `output_modalities`,
//     `supported_features`, `supported_sampling_parameters`, `model_card` and
//     `pricing`. `context_window`/`max_completion_tokens` are the numbers
//     `context_length`/`max_output_length` repeat, so one pair is read.
//   - the listing holds models that are not chat models at all: `whisper-large-v3`
//     and `whisper-large-v3-turbo` (input `audio`, output `transcription`) and
//     `canopylabs/orpheus-*` (output `speech`). Asking one to chat is refused with
//     HTTP 400 and `"The model \`whisper-large-v3\` does not support chat
//     completions"`, so dropping them is a reading of the API's own answer rather
//     than a guess about the ids — `output_modalities` is what says it.
//   - `meta-llama/llama-prompt-guard-2-22m` and `-86m` are the harder case, and the
//     one row this mapper deliberately keeps although it cannot be used: they are
//     text-classification models whose listing entry looks like any other chat
//     model (`active` true, text to text, a real `context_window` of 512), and
//     the vendor publishes no field that separates them. Measured, a call without
//     `stream` answers HTTP 200 with a score, and `stream: true` is refused with
//     HTTP 400 `"text classification models do not support streaming"` — so a
//     streaming-only client has two rows that can only fail, and the vendor's own
//     sentence is what the user is shown. Hiding them would take a rule about
//     their ids, which is the guess this mapping refuses to make (see §5 of
//     GROQ.md).
//   - `supported_features` is per model and honest: `openai/gpt-oss-20b` publishes
//     `["tools","json_mode","structured_outputs","reasoning"]`, `allam-2-7b`
//     publishes `["json_mode"]` alone. So unlike build.nvidia.com — whose listing
//     says nothing and whose provider therefore claims tools for every model —
//     the capability claims here are taken from the vendor's own answer.
//   - a streamed chat answers SSE in OpenAI's shape, and `stream_options.include_usage`
//     is honoured: the frame carrying `finish_reason` arrives with `usage`
//     (`prompt_tokens`, `completion_tokens`, `total_tokens`, plus `*_time` fields of
//     its own and `completion_tokens_details.reasoning_tokens`), and an identical
//     frame with `choices: []` follows it.
//   - reasoning streams in `reasoning` beside a `channel: "analysis"` marker;
//     answer deltas carry `content` alone. `finish_reason` is `"stop"` for a
//     finished answer and `"tool_calls"` for tool calls, which arrive fragmented
//     in the usual `tool_calls` shape.
//   - `seed` is the vendor's own name and is honoured: a pinned value comes back
//     in the response's `"seed"` field (and in `x_groq.seed`).
//   - the deprecated `max_tokens` is still accepted, which is why the shared
//     request body needs no field of its own here.
//   - `reasoning_effort` exists but has **no off value**: `"none"` is refused with
//     HTTP 400 and "`reasoning_effort` must be one of `low`, `medium`, or `high`".
//     The provider therefore does not claim to be able to switch thinking off —
//     compaction's fallback (adapting its own output budget) is what applies.
//   - image input works as an `image_url` data URL, but a 1x1 PNG is refused with
//     `invalid image data` where a 128x128 one is accepted, so the small fixtures
//     other tests use are not a valid check of this route.
//   - refusals carry a small JSON envelope with a `code`:
//     `{"error":{"message":"Invalid API Key","type":"invalid_request_error",
//     "code":"invalid_api_key"}}` with HTTP 401, for a missing key as much as for a
//     wrong one.
//   - every response carries `x-ratelimit-limit-requests`, `-limit-tokens`,
//     `-remaining-*` and `-reset-*` headers: the budget the account is on, which the
//     live test has to respect rather than spend.

// GroqDefaultBaseURL is the address the vendor documents. `/openai/v1` is part of
// it — the API is OpenAI-compatible but is not served from the host root.
const GroqDefaultBaseURL = "https://api.groq.com/openai/v1"

// newGroqProvider builds the Groq provider. An empty base URL falls back to the
// vendor host, so a settings blob with the entry half-filled still works.
func newGroqProvider(id, name, baseURL, apiKey string) *openAICompatProvider {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	switch {
	case baseURL == "":
		baseURL = GroqDefaultBaseURL
	case isBareHost(baseURL):
		// The vendor's console shows the host on its own, and this is the one
		// backend here whose path is not a bare `/v1`: completing a bare host
		// with `/v1` would build `https://api.groq.com/v1`, which 404s.
		baseURL += "/openai/v1"
	}
	if id == "" {
		id = "groq"
	}
	if name == "" {
		name = "Groq"
	}
	return &openAICompatProvider{
		id:          id,
		name:        name,
		baseURL:     baseURL,
		apiKey:      apiKey,
		requiresKey: true,
		headers:     map[string]string{},
		// One chat route: every route of this API lives under the documented
		// base, and a second guess would only cost a 404.
		chatPaths:        []string{baseURL + "/chat/completions"},
		modelsPath:       baseURL + "/models",
		openAIModelsPath: baseURL + "/models",
		modelsFlavour:    flavourGroq,
	}
}

// groqModelEntry is one entry of Groq's /models listing. The fields that are not
// decoded carry no decision: `object` is always "model", `created` is a real date
// the app has no use for, `public_apps` is null, `pricing` is per-token cents the
// panel does not show, and `model_card` is a documentation URL. `name` and
// `hugging_face_id` are display strings, and the id — the vendor's own fully
// qualified name — is what the app addresses a model by.
type groqModelEntry struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
	// Active is a pointer so that a missing field is not read as "false": a
	// listing that stopped publishing the field must not empty the picker.
	Active *bool `json:"active"`
	// ContextWindow is the model's window and MaxCompletionTokens the largest
	// answer it will produce. Both are published per model, which is why nothing
	// here has to be guessed or overridden by hand.
	ContextWindow       int      `json:"context_window"`
	MaxCompletionTokens int      `json:"max_completion_tokens"`
	InputModalities     []string `json:"input_modalities"`
	OutputModalities    []string `json:"output_modalities"`
	SupportedFeatures   []string `json:"supported_features"`
}

// fromGroqEntry maps one listing entry and says whether it can be offered.
//
// An entry is dropped for exactly two reasons, both of them the vendor's own
// answer rather than a rule about the id: the account cannot call it (`active`
// false), or it is not a chat model at all — its output modalities name no text,
// which is the `transcription` of the Whisper models and the `speech` of the TTS
// ones. Measured, asking one of those to chat is HTTP 400 "does not support chat
// completions", so the filter only spares the user a row that could only fail.
//
// Everything a model *claims* is read from the entry: tools and reasoning from
// `supported_features`, vision from an `image` input modality. Nothing is claimed
// on a model's behalf, which is the opposite of NVIDIA's listing — there the
// catalogue says nothing, so the provider claims tools for every row and lets a
// refusal answer for the account.
func (p *openAICompatProvider) fromGroqEntry(m groqModelEntry) (ModelInfo, bool) {
	if m.ID == "" {
		return ModelInfo{}, false
	}
	if m.Active != nil && !*m.Active {
		return ModelInfo{}, false
	}
	// A listing that stops publishing output modalities must not empty the
	// picker: only a present list that leaves text out is a verdict.
	if len(m.OutputModalities) > 0 && !hasModality(m.OutputModalities, "text") {
		return ModelInfo{}, false
	}
	info := ModelInfo{
		ID:        m.ID,
		Backend:   p.id,
		Publisher: m.OwnedBy,
		Type:      "llm",
		// Hosted: a model answers as soon as the request is authenticated, so
		// it must not be reported as awaiting a load.
		State: StateLoaded,
		// The window is published (measured: 131072 on openai/gpt-oss-20b), so
		// the context meter and the switch check work without an override.
		ContextLimit: m.ContextWindow,
		MaxContext:   m.ContextWindow,
	}
	if hasModality(m.InputModalities, "image") {
		info.Type = "vlm"
		info.Capabilities = append(info.Capabilities, CapVision)
	}
	for _, feature := range m.SupportedFeatures {
		switch feature {
		case "tools":
			info.Capabilities = append(info.Capabilities, CapTools)
		case "reasoning":
			// The model reasons by default on this API; the claim is what lets
			// the UI badge it and what compaction reads before it tries to
			// silence thinking. That attempt cannot succeed here — there is no
			// off value for `reasoning_effort` — which is why compaction's own
			// output budget is the fallback that matters.
			info.Capabilities = append(info.Capabilities, CapThink)
		}
	}
	return info, true
}

// hasModality reports whether a modality list names one. The comparison is by whole
// value, not by prefix: the vendor writes `text`, `image`, `audio`, `transcription`
// and `speech`, so `text` must not be found inside `text-to-speech` — a substring
// reading would call a speech-only model a chat model, which is the one verdict this
// filter exists to reach.
func hasModality(modalities []string, want string) bool {
	for _, m := range modalities {
		if strings.EqualFold(strings.TrimSpace(m), want) {
			return true
		}
	}
	return false
}

// mapGroqEntries maps the listing entry by entry, keeping the vendor's order.
func (p *openAICompatProvider) mapGroqEntries(raw []json.RawMessage) []ModelInfo {
	out := make([]ModelInfo, 0, len(raw))
	for _, entry := range raw {
		var m groqModelEntry
		if err := json.Unmarshal(entry, &m); err != nil {
			// An entry that cannot be read is skipped rather than failing the
			// listing, the same way one dead backend does not hide a live one.
			continue
		}
		if info, keep := p.fromGroqEntry(m); keep {
			out = append(out, info)
		}
	}
	return out
}
