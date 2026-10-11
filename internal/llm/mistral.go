package llm

import (
	"encoding/json"
	"net/url"
	"strings"
)

// Mistral is a hosted, OpenAI-compatible service, so chat, streaming and tool
// calls all go through the shared path. What is specific to it is the address,
// the model listing, and two things the listing says that no other backend's
// does: which models can hold a conversation at all, and which of those can call
// functions, read images or reason.
//
// The shapes below were read off the live service rather than assumed, and all
// four of them matter:
//
//   - GET /v1/models returns 46 entries, and they are not a chat catalogue: the
//     embedding, moderation, OCR, speech and transcription models sit in the same
//     array, each with `capabilities.completion_chat: false`. `mistral-ocr-*`
//     even reports `function_calling: true`, so the chat flag and not the tool
//     flag is what separates a model that can answer from one that cannot.
//   - a model's own `capabilities` object is the truth about it, so a vision
//     model is not offered as text-only and a model without function calling
//     does not claim tools — unlike DeepSeek, where the listing is silent and
//     every model is assumed to call tools.
//   - the ids are not models. Of the 27 chat-capable entries, only **8 distinct
//     models** are behind them: `mistral-medium-latest` alone is published under
//     nine ids (mistral-medium, mistral-medium-3-5, mistral-medium-3.5,
//     mistral-medium-3, mistral-medium-2604, mistral-vibe-cli-latest,
//     mistral-vibe-cli-with-tools, magistral-medium-latest) and
//     `magistral-small-latest` is the same model as `mistral-small-latest`. Each
//     of the eight groups agreed on context window and on every capability flag,
//     which is what makes the shared `name` field usable as the identity. Listing
//     them all would put 27 rows in the picker for 8 models, so the aliases are
//     collapsed to one row per model.
//   - a reasoning model does not stream its thinking in a field of the delta:
//     `choices[].delta.content` is a **list of typed chunks** whose thinking rides
//     as {"type":"thinking","thinking":[{"type":"text","text":…}]}. That is what
//     deltaContent in openai.go decodes; a string-typed field would have made
//     every frame of such an answer fail to decode and vanish.
//
// Measured on the live API: `stream_options.include_usage` is honoured (the final
// frame carries prompt_tokens/completion_tokens), tool calls stream as fragmented
// `tool_calls`, a bad key answers HTTP 401 with `{"detail":"Invalid API Key"}`,
// and a throttled request answers HTTP 429 with
// `{"object":"error","type":"rate_limited","code":"1300"}`. The last one is a
// per-model gate as much as a rate: on the free tier `ministral-*` answers while
// `mistral-small-latest` is refused outright, and a Labs model answers HTTP 403
// naming that gate instead.

// MistralDefaultBaseURL is the address the vendor documents. The /v1 prefix is
// part of it: every route of this API lives under it.
const MistralDefaultBaseURL = "https://api.mistral.ai/v1"

// newMistralProvider builds the Mistral provider. An empty base URL falls back to
// the vendor host, so a settings blob with the entry half-filled still works.
func newMistralProvider(id, name, baseURL, apiKey string) *openAICompatProvider {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = MistralDefaultBaseURL
	} else if isBareHost(baseURL) {
		// The vendor's own documentation shows the bare host as the server
		// address while every route lives under /v1, so that one shape is
		// completed. A base URL that already carries a path is taken as the
		// user's own choice and left exactly as it is: appending /v1 to a
		// gateway prefix would turn a working /openai into /openai/v1.
		baseURL += "/v1"
	}
	if id == "" {
		id = "mistral"
	}
	if name == "" {
		name = "Mistral"
	}
	return &openAICompatProvider{
		id:          id,
		name:        name,
		baseURL:     baseURL,
		apiKey:      apiKey,
		requiresKey: true,
		headers:     map[string]string{},
		// One path: unlike DeepSeek, whose two routes are both documented, this
		// API has a single chat route and a wrong guess would only cost a 404.
		chatPaths:        []string{baseURL + "/chat/completions"},
		modelsPath:       baseURL + "/models",
		openAIModelsPath: baseURL + "/models",
		modelsFlavour:    flavourMistral,
	}
}

// isBareHost reports a URL that names a scheme and a host and nothing else.
func isBareHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Host != "" && (u.Path == "" || u.Path == "/")
}

// mapMistralEntries maps the whole listing: the models that cannot hold a
// conversation are dropped, and the aliases of one model are collapsed into a
// single row.
func (p *openAICompatProvider) mapMistralEntries(raw []json.RawMessage) []ModelInfo {
	out := make([]ModelInfo, 0, len(raw))
	// canonical name -> index in out. The name is the model's identity, and the
	// listing's own `aliases` array says the same thing about the ids that share
	// it — every one of them reaches the same model.
	at := map[string]int{}
	for _, entry := range raw {
		var m mistralModelEntry
		if err := json.Unmarshal(entry, &m); err != nil {
			continue
		}
		info, ok := p.fromMistralEntry(m)
		if !ok {
			continue
		}
		key := m.Name
		if key == "" {
			// No canonical name to group by; the id is the only identity there
			// is, so the entry stands on its own.
			key = m.ID
		}
		if i, seen := at[key]; seen {
			out[i] = preferredMistralID(out[i], info)
			continue
		}
		at[key] = len(out)
		out = append(out, info)
	}
	return out
}

// preferredMistralID picks which of two aliases of one model to offer. The id
// that follows the vendor's rolling release wins, and the shortest of those when
// a family has several (`mistral-small-latest` over `magistral-small-latest`,
// `codestral-latest` over `mistral-code-fim-latest`). With no rolling alias at
// all the first one the listing named is kept, so the order stays the vendor's.
func preferredMistralID(current, candidate ModelInfo) ModelInfo {
	curLatest := strings.HasSuffix(current.ID, "-latest")
	candLatest := strings.HasSuffix(candidate.ID, "-latest")
	switch {
	case candLatest && !curLatest:
		return candidate
	case candLatest && curLatest && len(candidate.ID) < len(current.ID):
		return candidate
	default:
		return current
	}
}

// mistralModelEntry is one entry of Mistral's /models listing. It is the richest
// of the three listings this app reads: the context window and the capabilities
// are both reported per model.
type mistralModelEntry struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
	// Name is the model's canonical name, shared by every alias of it. It is the
	// identity the collapsing groups by, in preference to the id.
	Name   string `json:"name"`
	MaxCtx int    `json:"max_context_length"`
	// Capabilities is the per-model answer this provider needs. A field that is
	// absent reads as false, which is the safe direction for each of them.
	Capabilities struct {
		CompletionChat  bool `json:"completion_chat"`
		FunctionCalling bool `json:"function_calling"`
		Reasoning       bool `json:"reasoning"`
		Vision          bool `json:"vision"`
	} `json:"capabilities"`
}

// fromMistralEntry maps one listing entry. The second return value is false for
// a model that cannot answer a conversation, so the caller can leave it out of
// the catalogue entirely rather than offering a picker row that fails on send.
func (p *openAICompatProvider) fromMistralEntry(m mistralModelEntry) (ModelInfo, bool) {
	if !m.Capabilities.CompletionChat {
		return ModelInfo{}, false
	}
	info := ModelInfo{
		ID:           m.ID,
		Backend:      p.id,
		Publisher:    m.OwnedBy,
		Type:         "llm",
		ContextLimit: m.MaxCtx,
		MaxContext:   m.MaxCtx,
		// Hosted: the model answers as soon as the request is authenticated.
		// Reporting it "not loaded" the way a local model waiting for VRAM is
		// reported would grey out the picker and keep it out of the default-model
		// choice for no reason.
		State: StateLoaded,
	}
	if m.Capabilities.Vision {
		info.Type = "vlm"
		info.Capabilities = append(info.Capabilities, CapVision)
	}
	if m.Capabilities.FunctionCalling {
		info.Capabilities = append(info.Capabilities, CapTools)
	}
	if m.Capabilities.Reasoning {
		info.Capabilities = append(info.Capabilities, CapThink)
	}
	return info, true
}
