package llm

import (
	"encoding/json"
	"strings"
)

// NVIDIA hosts the build.nvidia.com catalogue behind an OpenAI-compatible NIM
// API, so chat, streaming, tool calls and usage all go through the shared path.
// What is specific to it is the address, the fact that it cannot answer without
// a key, and a model listing that is sparse in a way no other backend's is.
//
// The shapes below were read off the live service rather than assumed, and the
// last two are the ones that shape this provider:
//
//   - GET /v1/models answers 80 entries, all ids distinct, and every entry
//     carries exactly four fields: `id`, `object`, `created` and `owned_by`.
//     There is no context window, no modality list and no capability object, so
//     nothing in the listing says which models can hold a conversation. `created`
//     is the same placeholder (735790403) on all 80, i.e. it is not a date.
//   - GET /v1/models/<id> is not a richer route: it answers the same four fields,
//     so no per-model metadata can be fetched either.
//   - the listing is the **global catalogue, not this account's entitlement**.
//     Measured with one request per model at `max_tokens: 1`: 45 of the 80
//     answer HTTP 404 with `application/problem+json` and the body
//     {"status":404,"title":"Not Found","detail":"Function '<uuid>': Not found
//     for account '<id>'"}. Those models are listed for everyone and callable
//     only by whoever is entitled to them, which is a fact about the account and
//     not about the model — a second key can reach a different subset.
//   - 9 answer the bare-text `404 page not found`, i.e. no chat route exists at
//     all: the embed/retrieve models (`nvidia/nv-embedqa-*`, `nvidia/embed-qa-4`,
//     `snowflake/arctic-embed-l`, `nvidia/nvclip`). `bigcode/starcoder2-15b` is
//     in that set too, so the group is "the chat route is absent", not "the
//     embeddings"; it cannot be described by a rule about the id.
//   - of the 80, 16 answered 200, 6 never answered within 90 seconds, and the
//     remaining 4 answered 500, 502 or 400.
//
// The consequence is the design: **no model is filtered out of the catalogue.**
// Mistral could drop what cannot chat because its listing publishes
// `capabilities.completion_chat` per model; this one publishes nothing, and the
// models that fail are refused per account. A filter written here would be a
// guess about entitlement — it would hide a working model from the account that
// is entitled to it — so the row is offered and a model the account cannot call
// fails on send with the vendor's own message. The cost is a picker longer than
// the list of models that will answer; the alternative is a picker that is
// silently wrong for someone else.
//
// That is a statement about this provider only. The app offers a way out of the
// cost that does not guess: App.StartModelCheck asks every model once (probe.go)
// and keeps the ones this account cannot call out of the picker, so that filter
// is evidence from this key rather than a claim about anyone else's.
//
// Measured on the live API: chat completions stream OpenAI-style SSE,
// `stream_options.include_usage` is honoured (a final frame with `choices: []`
// carrying prompt_tokens/completion_tokens), reasoning arrives in **both**
// `reasoning` and `reasoning_content` with identical text (the shared delta
// parser prefers `reasoning` and so does not double it), tool calls arrive as
// `tool_calls` in the usual shape with `finish_reason: "tool_calls"`, and a
// request that runs out of budget ends with `finish_reason: "length"` and a null
// `content` — a reasoning model spending its whole cap on thinking. A bad key
// answers HTTP 401.

// NvidiaDefaultBaseURL is the address the vendor documents for the hosted
// catalogue. The `/v1` prefix is part of it: every route of this API lives under
// it, and the self-hosted NIM container (which listens on :8000) is a different
// deployment that the user points at by editing this field.
const NvidiaDefaultBaseURL = "https://integrate.api.nvidia.com/v1"

// newNvidiaProvider builds the NVIDIA provider. An empty base URL falls back to
// the vendor host, so a settings blob with the entry half-filled still works.
func newNvidiaProvider(id, name, baseURL, apiKey string) *openAICompatProvider {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = NvidiaDefaultBaseURL
	} else if isBareHost(baseURL) {
		// The vendor's own console shows the bare host as the address while
		// every route lives under /v1, so that one shape is completed. A base
		// URL that already carries a path is the user's own choice and is left
		// exactly as it is — appending /v1 to a gateway prefix would turn a
		// working /openai into /openai/v1.
		baseURL += "/v1"
	}
	if id == "" {
		id = "nvidia"
	}
	if name == "" {
		name = "NVIDIA"
	}
	return &openAICompatProvider{
		id:          id,
		name:        name,
		baseURL:     baseURL,
		apiKey:      apiKey,
		requiresKey: true,
		headers:     map[string]string{},
		// One path: this API has a single chat route, and a second guess would
		// only cost a 404. It is not DeepSeek's case, where two routes are both
		// documented.
		chatPaths:        []string{baseURL + "/chat/completions"},
		modelsPath:       baseURL + "/models",
		openAIModelsPath: baseURL + "/models",
		modelsFlavour:    flavourNvidia,
	}
}

// nvidiaModelEntry is one entry of NVIDIA's /models listing. It is the sparsest
// listing this app reads — four fields, of which two are read: `object` and
// `created` carry no information (the latter is the same placeholder value on
// every entry), so they are not decoded.
type nvidiaModelEntry struct {
	ID string `json:"id"`
	// OwnedBy is the publisher namespace (`meta`, `nvidia`, `z-ai`, …). It is
	// also the first half of the id, which is what makes the id a fully
	// qualified model name rather than a bare one.
	OwnedBy string `json:"owned_by"`
}

// fromNvidiaEntry maps one listing entry. There is no second return value: this
// listing says nothing about what a model can do, so unlike Mistral's there is
// no entry to drop here.
func (p *openAICompatProvider) fromNvidiaEntry(m nvidiaModelEntry) ModelInfo {
	info := ModelInfo{
		ID:        m.ID,
		Backend:   p.id,
		Publisher: m.OwnedBy,
		Type:      "llm",
		// Hosted: the model answers as soon as the request is authenticated, so
		// it must not be reported as awaiting a load. The picker greys those out
		// and the default-model choice skips them.
		State: StateLoaded,
		// The context window is not published by any route of this API — not on
		// the listing and not on the per-model route — so it is left at zero
		// rather than guessed. Zero is the state the app already handles for a
		// plain OpenAI-compatible server: the per-model override in Settings is
		// where the number comes from.
		ContextLimit: 0,
	}
	// Tools are claimed for every model, the same way DeepSeek's are and for the
	// same reason: the app only uses the native tool protocol when a model
	// claims it, so leaving the claim off would silently cost every model here
	// its tools. The live test is what keeps the claim honest — it asserts a
	// real tool call streams from a model this listing offered.
	//
	// Thinking and vision are **not** claimed. Per-model capability is simply
	// not in this listing, and the catalogue contains both vision models and
	// models that never reason; claiming either for all of them would be wrong
	// for most. A model that reasons still streams it — the delta parser reads
	// `reasoning`/`reasoning_content` regardless of the claim; the claim only
	// decides the badges and whether compaction may try to switch thinking off.
	info.Capabilities = append(info.Capabilities, CapTools)
	return info
}

// mapNvidiaEntries maps the listing entry by entry. Nothing is grouped or
// dropped: the ids are unique (measured: 80 of 80) and there is no alias field
// to collapse on, so the vendor's own list is what the picker shows.
func (p *openAICompatProvider) mapNvidiaEntries(raw []json.RawMessage) []ModelInfo {
	out := make([]ModelInfo, 0, len(raw))
	for _, entry := range raw {
		var m nvidiaModelEntry
		if err := json.Unmarshal(entry, &m); err != nil {
			// An entry that cannot be read is skipped rather than failing the
			// listing, the same way one dead backend does not hide a live one.
			continue
		}
		if m.ID == "" {
			continue
		}
		out = append(out, p.fromNvidiaEntry(m))
	}
	return out
}
