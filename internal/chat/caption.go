package chat

import (
	"context"
	"errors"

	"kichat/internal/images"
	"kichat/internal/llm"
)

// Caption asks the model that is already loaded for the caption a caption-trained image
// model draws from.
//
// It exists because FLUX 3 is not drawn from a request but from a caption, and the model
// trained to write that caption is an 18.8 GB prompt upsampler KiChat will not ship. BFL
// says the upsampler is replaceable — "Any other LLM can do the job with the system
// prompts" — so the model that already called generate_image does it, on the turn's own
// backend. See internal/images/caption.go for the prompt itself and its provenance.
//
// It goes through Complete, the non-streaming path compaction and titling use, for two
// reasons that matter here: the reasoning channel never reaches the caller, so a thinking
// model's analysis cannot end up in `-p`, and nothing is emitted to the window, so a
// 350-word caption does not appear in the transcript or get replayed on later turns.
//
// Why this is safe in the middle of a turn: tool calls run *after* the provider's stream has
// been consumed — generate.go calls runTool once streamErr has been handled — so this is a
// sequential request, not a nested one. A single-slot llama-server answers it instead of
// deadlocking behind a stream that is waiting for the tool that is making this call.
func Caption(ctx context.Context, p llm.Provider, model string, req images.CaptionRequest) (images.CaptionResult, error) {
	if p == nil {
		return images.CaptionResult{}, errNoCaptionBackend
	}
	// Greedy is zero *and* meant. BFL's own recipe decodes greedily, and a zero that
	// nobody meant is "use the backend's default" everywhere else in this app — Ollama
	// drops the key entirely, which would leave the modelfile's temperature in force and
	// make two captions of one request differ for no visible reason.
	off := false
	text, _, err := Complete(ctx, p, llm.ChatRequest{
		Model: model,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: images.CaptionSystemPrompt()},
			{Role: llm.RoleUser, Content: images.CaptionMessage(req)},
		},
		Temperature:    0,
		TemperatureSet: true,
		MaxTokens:      images.CaptionMaxTokens,
		Think:          &off,
	})
	if err != nil {
		return images.CaptionResult{}, err
	}
	cleaned, err := images.CleanCaption(text)
	if err != nil {
		return images.CaptionResult{}, err
	}
	return images.CaptionResult{Text: cleaned, Model: model}, nil
}

// errNoCaptionBackend is what a caption request answers when the turn has no provider to
// ask. It is its own error rather than a generic one because the tool's reaction is
// specific: draw the request as typed and say the caption did not happen.
var errNoCaptionBackend = errors.New("kein Backend für die Bildbeschreibung verfügbar")
