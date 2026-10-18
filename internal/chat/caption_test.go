package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kichat/internal/images"
	"kichat/internal/llm"
)

// TestCaptionAsksTheTurnModelForBFLsCaption pins the four things the caption request has
// to get right at once: BFL's prompt as the system message, the canvas line above the
// request, greedy decoding that survives a backend which drops a zero, and a model name
// that comes back with the caption so the picture can record who wrote it.
func TestCaptionAsksTheTurnModelForBFLsCaption(t *testing.T) {
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "qwen3-8b"},
		replies: [][]llm.StreamEvent{{
			{Type: llm.EventDelta, Text: "Here is the caption:\n"},
			{Type: llm.EventDelta, Text: "A red fox in deep snow at golden hour."},
			{Type: llm.EventDone},
		}},
	}

	result, err := Caption(context.Background(), provider, "qwen3-8b", images.CaptionRequest{
		Prompt: "a fox", Width: 1024, Height: 768,
	})
	if err != nil {
		t.Fatalf("Caption: %v", err)
	}
	if result.Text != "A red fox in deep snow at golden hour." {
		t.Errorf("caption = %q; the preamble was not stripped", result.Text)
	}
	if result.Model != "qwen3-8b" {
		t.Errorf("model = %q, want the model that wrote it", result.Model)
	}

	req := provider.lastRequest()
	if req.Model != "qwen3-8b" {
		t.Errorf("asked %q, want the turn's model", req.Model)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want a system prompt and the request", len(req.Messages))
	}
	if req.Messages[0].Role != llm.RoleSystem || req.Messages[0].Content != images.CaptionSystemPrompt() {
		t.Error("the system message is not BFL's prompt verbatim")
	}
	user := req.Messages[1].Content
	for _, want := range []string{"TARGET CANVAS: 1024:768", "a fox"} {
		if !strings.Contains(user, want) {
			t.Errorf("the user message does not contain %q: %q", want, user)
		}
	}
	// Greedy is zero *and meant*: Ollama drops a temperature of zero unless the caller
	// says it chose it, and a sampled caption would differ between two draws of one
	// request for no visible reason.
	if req.Temperature != 0 || !req.TemperatureSet {
		t.Errorf("temperature = %v (set=%v), want an explicit greedy zero", req.Temperature, req.TemperatureSet)
	}
	if req.MaxTokens != images.CaptionMaxTokens {
		t.Errorf("max tokens = %d, want %d", req.MaxTokens, images.CaptionMaxTokens)
	}
	if req.Think == nil || *req.Think {
		t.Error("thinking was not switched off; a model that thinks its budget away returns no caption")
	}
}

// TestCaptionNeverSendsReasoning: only the text channel of a completion reaches `-p`. A
// reasoning model's analysis is not a caption, and putting it there would draw a picture
// of the model thinking.
func TestCaptionNeverSendsReasoning(t *testing.T) {
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "thinking-model"},
		replies: [][]llm.StreamEvent{{
			{Type: llm.EventReasoning, Text: "The user wants a fox. I should mention snow."},
			{Type: llm.EventDone},
		}},
	}

	_, err := Caption(context.Background(), provider, "thinking-model", images.CaptionRequest{Prompt: "a fox"})
	if !errors.Is(err, images.ErrEmptyCaption) {
		t.Fatalf("err = %v, want ErrEmptyCaption: a model that only reasoned wrote no caption", err)
	}
}

func TestCaptionRefusesToRunWithoutABackend(t *testing.T) {
	if _, err := Caption(context.Background(), nil, "m", images.CaptionRequest{Prompt: "a fox"}); err == nil {
		t.Fatal("a caption with no provider was attempted")
	}
}
