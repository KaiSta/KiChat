package llm

import (
	"errors"
	"strings"
)

// DeepSeek is a hosted, OpenAI-compatible service, so the chat and streaming
// path is the shared one. What is specific to it is the base URL, the model
// listing — which reports a real context window where the plain OpenAI listing
// reports none — and the fact that it cannot answer without a key.
//
// The shapes below were read off the live service rather than assumed:
// POST /chat/completions (and /v1/chat/completions) stream OpenAI-style SSE,
// reasoning arrives in `reasoning_content`, tool calls arrive as fragmented
// `tool_calls`, and `stream_options.include_usage` is honoured, which is what
// lets the context meter show a measured value instead of an estimate.

// DeepSeekDefaultBaseURL is the host the vendor documents for the OpenAI-
// compatible API. /v1 is an accepted alias of the same API rather than a
// versioned path, so it is used as the fallback chat path.
const DeepSeekDefaultBaseURL = "https://api.deepseek.com"

// ErrMissingAPIKey marks a backend that cannot answer without a credential.
// It is reported instead of attempting the request because the vendor's 401
// body is long, says nothing about where the key belongs, and would otherwise
// be the first thing a new user sees.
var ErrMissingAPIKey = errors.New("no API key configured")

// newDeepSeekProvider builds the DeepSeek provider. An empty base URL falls back
// to the vendor host, so a settings blob with the entry half-filled still works.
func newDeepSeekProvider(id, name, baseURL, apiKey string) *openAICompatProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DeepSeekDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if id == "" {
		id = "deepseek"
	}
	if name == "" {
		name = "DeepSeek"
	}
	return &openAICompatProvider{
		id:          id,
		name:        name,
		baseURL:     baseURL,
		apiKey:      apiKey,
		requiresKey: true,
		headers:     map[string]string{},
		// The documented path first, the OpenAI-SDK-shaped one as the fallback:
		// both answer today, and a 404 retires the first permanently.
		chatPaths:        []string{baseURL + "/chat/completions", baseURL + "/v1/chat/completions"},
		modelsPath:       baseURL + "/models",
		openAIModelsPath: baseURL + "/models",
		modelsFlavour:    flavourDeepSeek,
	}
}

// deepSeekModelEntry is one entry of DeepSeek's /models listing. Unlike the
// lowest-common-denominator OpenAI listing it carries the context window, so a
// DeepSeek model needs no manual override before it can be planned against.
type deepSeekModelEntry struct {
	ID              string   `json:"id"`
	OwnedBy         string   `json:"owned_by"`
	ContextWindow   int      `json:"context_window"`
	InputModalities []string `json:"input_modalities"`
	// Effort is present on models that reason. Its presence, not its level, is
	// what matters here.
	Effort *struct {
		SupportedLevels []string `json:"supported_levels"`
		DefaultLevel    string   `json:"default_level"`
	} `json:"effort"`
}

func (p *openAICompatProvider) fromDeepSeekEntry(m deepSeekModelEntry) ModelInfo {
	info := ModelInfo{
		ID:           m.ID,
		Backend:      p.id,
		Publisher:    m.OwnedBy,
		Type:         "llm",
		ContextLimit: m.ContextWindow,
		MaxContext:   m.ContextWindow,
		// A hosted model has no load step: it answers as soon as the request is
		// authenticated. Reporting it "not loaded" the way a local model waiting
		// for VRAM is reported would grey out the picker and keep the model out
		// of the default-model choice for no reason.
		State: StateLoaded,
	}
	for _, modality := range m.InputModalities {
		if modality == "image" {
			info.Type = "vlm"
			info.Capabilities = append(info.Capabilities, CapVision)
			break
		}
	}
	// The listing does not advertise tool calling, and the app uses the native
	// protocol only when a model claims it — a model left without the claim would
	// lose tools rather than fall back to the text protocol quietly. Verified
	// against both shipped models, which stream fragmented `tool_calls`.
	info.Capabilities = append(info.Capabilities, CapTools)
	if m.Effort != nil {
		// Reasoning streams by default here, so this is a fact about the endpoint
		// rather than a setting the user turned on.
		info.Capabilities = append(info.Capabilities, CapThink)
	}
	return info
}
