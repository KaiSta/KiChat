package llm

import "strings"

// MLXDefaultBaseURL is where mlx_lm.server listens when nobody says otherwise:
// `--host` defaults to 127.0.0.1 and `--port` to 8080 (mlx_lm/SERVER.md). It is
// only a default for a server the user runs, exactly like llama.cpp's 9931 — a
// managed start chooses a free port and writes the address it landed on into the
// entry.
const MLXDefaultBaseURL = "http://127.0.0.1:8080"

// newMLXProvider builds the provider for Apple's mlx_lm.server.
//
// The wire protocol is OpenAI's, so the streaming, tool-call and usage handling
// are the shared implementation. What this constructor decides is the handful of
// things that differ, and each one was read out of mlx_lm/server.py rather than
// guessed:
//
//   - The listing is the plain `/v1/models` shape: `{id, object, created}`, where
//     `id` is a Hugging Face repo id or the path the server was started with.
//     It carries no context window and no capabilities, so ListModels maps it to
//     an id and a state and nothing else. The window a chat is planned against
//     therefore comes from the user's per-model override, never from here — an
//     invented window would truncate a prompt the server would have accepted.
//   - `stream_options.include_usage` is supported, and the usage frame carries
//     `prompt_tokens_details.cached_tokens`; the shared request already asks for
//     it.
//   - Thinking arrives in a `reasoning` delta, which the shared delta reader
//     already maps. Tool calls arrive in the ordinary `tool_calls` chunks with an
//     `index`, and `finish_reason` becomes `tool_calls` once the template's own
//     parser accepts one.
//   - A model that fails to load is answered with HTTP 404 and the Python
//     exception's text — not a 404 from a missing route. Callers must classify
//     that body; the shared provider falls back to its second chat path on any
//     404, which costs one wasted request and nothing else.
//   - `/health` is 200 as soon as the generation thread is alive, *before* the
//     weights are loaded. A managed start therefore treats "running" as "the
//     process answers", and the first request pays the load.
//
// There is no `/props` namespace and no native route: unlike llama.cpp and LM
// Studio, there is nothing to enrich the listing with.
func newMLXProvider(id, name, baseURL, apiKey string) *openAICompatProvider {
	baseURL = strings.TrimRight(baseURL, "/")
	chatPaths := []string{baseURL + "/v1/chat/completions", baseURL + "/chat/completions"}
	modelsPath := baseURL + "/v1/models"
	// A base URL that already names /v1 must not produce /v1/v1/chat/completions
	// as the first attempt: that request is certain to 404 on the first message of
	// every session, with the fallback quietly hiding it afterwards (the same trap
	// llama.cpp's provider records).
	if strings.HasSuffix(baseURL, "/v1") {
		chatPaths = []string{baseURL + "/chat/completions"}
		modelsPath = baseURL + "/models"
	}
	if strings.TrimSpace(id) == "" {
		id = "mlx"
	}
	if strings.TrimSpace(name) == "" {
		name = "MLX"
	}
	return &openAICompatProvider{
		id:               id,
		name:             name,
		baseURL:          baseURL,
		apiKey:           apiKey,
		headers:          map[string]string{},
		chatPaths:        chatPaths,
		modelsPath:       "", // no metadata namespace to prefer
		openAIModelsPath: modelsPath,
	}
}

// ManagedKind reports whether KiChat starts the server for this backend kind
// itself. The card, the start path and the chat's local-model picker all ask this
// one question, so a kind cannot be managed in one place and merely addressable in
// another.
func ManagedKind(kind string) bool {
	return kind == "llamacpp" || kind == "mlx"
}
