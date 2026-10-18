package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"kichat/internal/config"
	"kichat/internal/images"
)

// TestGenerateImageDrawsTheCaption is the feature in one test: the model's short request
// is sent to the turn's own model for a caption, the caption is what the backend draws
// from, and the recipe records both the request and who wrote the caption. Without the
// last part a card cannot answer "why does this look nothing like my prompt?".
func TestGenerateImageDrawsTheCaption(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{
		Captioning: config.ImageCaptioningAuto,
	})

	const caption = "A red fox stands in deep snow at golden hour, breath visible in the cold air."
	var asked images.CaptionRequest
	service.Caption = func(_ context.Context, req images.CaptionRequest) (images.CaptionResult, error) {
		asked = req
		return images.CaptionResult{Text: caption, Model: "qwen3-8b"}, nil
	}
	var phases []string
	service.Progress = func(_, _ string, p images.Progress) {
		if p.Phase != "" {
			phases = append(phases, p.Phase)
		}
	}

	out, err := service.Execute(WithCallingMessage(context.Background(), "msg-1"), chatID,
		ToolGenerateImage, `{"prompt":"a fox","width":640,"height":480}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if asked.Prompt != "a fox" || asked.Width != 640 || asked.Height != 480 {
		t.Errorf("the caption asked for %+v; it has to see the request and the canvas", asked)
	}
	if provider.got.Caption != caption {
		t.Errorf("the backend drew %q, want the caption", provider.got.Caption)
	}
	if provider.got.EffectivePrompt() != caption {
		t.Errorf("EffectivePrompt = %q, want the caption", provider.got.EffectivePrompt())
	}
	if provider.got.Prompt != "a fox" {
		t.Errorf("the request was replaced rather than kept beside the caption: %q", provider.got.Prompt)
	}
	// The wait before the first step is the model's, and a window that says nothing there
	// is indistinguishable from a hang.
	if len(phases) != 1 || phases[0] != images.PhaseCaptioning {
		t.Errorf("phases = %v, want one %q report before the draw", phases, images.PhaseCaptioning)
	}
	if !strings.Contains(out, "Bildbeschreibung") {
		t.Errorf("the tool result does not mention the caption:\n%s", out)
	}

	attachments, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(attachments) != 1 || attachments[0].Generation == nil {
		t.Fatalf("attachments = %d, want one with a recipe", len(attachments))
	}
	recipe := attachments[0].Generation
	if recipe.Prompt != caption {
		t.Errorf("recipe prompt = %q, want the caption a redraw has to replay", recipe.Prompt)
	}
	if recipe.SourcePrompt != "a fox" {
		t.Errorf("recipe source = %q, want what the user asked for", recipe.SourcePrompt)
	}
	if recipe.CaptionModel != "qwen3-8b" {
		t.Errorf("recipe caption model = %q, want the model that wrote it", recipe.CaptionModel)
	}
}

// TestGenerateImageSkipsACaptionThatIsAlreadyOne: captioning a caption expands an
// expansion, so a request that is already the length BFL's prompt asks for is drawn as
// typed and no second model is asked.
func TestGenerateImageSkipsACaptionThatIsAlreadyOne(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, _, chatID := newImageService(t, provider, config.ImageSettings{
		Captioning: config.ImageCaptioningAuto,
	})
	called := false
	service.Caption = func(context.Context, images.CaptionRequest) (images.CaptionResult, error) {
		called = true
		return images.CaptionResult{Text: "should not be used"}, nil
	}

	long := strings.TrimSpace(strings.Repeat("detail ", images.CaptionMinWords))
	if _, err := service.Execute(context.Background(), chatID, ToolGenerateImage,
		fmt.Sprintf(`{"prompt":%q}`, long)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if called {
		t.Error("a caption-length prompt was captioned again")
	}
	if provider.got.Caption != "" {
		t.Errorf("caption = %q, want none", provider.got.Caption)
	}
}

// TestGenerateImageDrawsWhenTheCaptionFails: the user asked for a picture, and a second
// model's stumble is not a reason to refuse it. The picture is drawn from the request as
// typed, the result says the caption did not happen, and the recipe records no caption
// model — which is how a weak picture can later be explained rather than guessed at.
func TestGenerateImageDrawsWhenTheCaptionFails(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{
		Captioning: config.ImageCaptioningAuto,
	})
	service.Caption = func(context.Context, images.CaptionRequest) (images.CaptionResult, error) {
		return images.CaptionResult{}, errors.New("das Modell hat nur gedacht")
	}

	out, err := service.Execute(context.Background(), chatID, ToolGenerateImage, `{"prompt":"a fox"}`)
	if err != nil {
		t.Fatalf("a failed caption failed the draw: %v", err)
	}
	if provider.got.Caption != "" || provider.got.EffectivePrompt() != "a fox" {
		t.Errorf("drew %q, want the request as typed", provider.got.EffectivePrompt())
	}
	if !strings.Contains(out, "fehlgeschlagen") {
		t.Errorf("the tool result does not say the caption failed:\n%s", out)
	}
	attachments, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	recipe := attachments[0].Generation
	if recipe.CaptionModel != "" || recipe.SourcePrompt != "" {
		t.Errorf("recipe claims a caption that never happened: %+v", recipe)
	}
	if recipe.Prompt != "a fox" {
		t.Errorf("recipe prompt = %q, want the request", recipe.Prompt)
	}
}

// TestCaptioningOffSendsTheRequestAsTyped is the no-change guarantee for every pipeline
// that was not trained on captions: the switch is off by default, and nothing about the
// draw or its recipe moves.
func TestCaptioningOffSendsTheRequestAsTyped(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{})
	service.Caption = func(context.Context, images.CaptionRequest) (images.CaptionResult, error) {
		t.Error("a caption was requested with the switch off")
		return images.CaptionResult{Text: "unused"}, nil
	}

	if _, err := service.Execute(context.Background(), chatID, ToolGenerateImage, `{"prompt":"a fox"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.got.Caption != "" {
		t.Errorf("caption = %q, want none", provider.got.Caption)
	}
	attachments, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	recipe := attachments[0].Generation
	if recipe.Prompt != "a fox" || recipe.SourcePrompt != "" || recipe.CaptionModel != "" {
		t.Errorf("the recipe changed for a draw with no caption: %+v", recipe)
	}
}
