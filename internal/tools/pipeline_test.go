package tools

import (
	"context"
	"strings"
	"testing"

	"kichat/internal/config"
	"kichat/internal/images"
)

// TestTheProfileTurnsCaptioningOnByItself is the feature the profile exists for: choosing a
// FLUX 3 diffusion file is enough. Steps, guidance and sampler come from the profile, the
// caption is asked for, and the recipe records the numbers that were really drawn with
// rather than the empty fields that produced them.
func TestTheProfileTurnsCaptioningOnByItself(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{
		Pipeline:          config.ImagePipelineFlux3Klein,
		CLIDiffusionModel: "/models/flux-3-image-klein.safetensors",
	})
	service.Caption = func(context.Context, images.CaptionRequest) (images.CaptionResult, error) {
		return images.CaptionResult{Text: "A red fox stands in deep snow at golden hour.", Model: "qwen3-8b"}, nil
	}

	if _, err := service.Execute(context.Background(), chatID, ToolGenerateImage, `{"prompt":"a fox"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.got.Steps != 16 || provider.got.CfgScale != 1.0 || provider.got.Sampler != "euler" {
		t.Errorf("drew %d steps at CFG %v with %q; want the profile's 16 / 1.0 / euler",
			provider.got.Steps, provider.got.CfgScale, provider.got.Sampler)
	}
	if provider.got.Caption == "" {
		t.Error("a FLUX 3 profile did not caption; that is the weak path BFL warns about")
	}

	attachments, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	recipe := attachments[0].Generation
	if recipe.Steps != 16 || recipe.CfgScale != 1.0 {
		t.Errorf("recipe records %d steps at CFG %v, want what was drawn", recipe.Steps, recipe.CfgScale)
	}
}

// TestPickingTheKleinFileIsEnough is the whole feature in the user's own terms: nothing is
// selected but the diffusion model, and the draw is 16 steps at CFG 1.0 with a caption.
// Everything else — the profile id, the caption mode — is inference from that one path.
func TestPickingTheKleinFileIsEnough(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, _, chatID := newImageService(t, provider, config.ImageSettings{
		CLIDiffusionModel: "/home/user/models/flux-3-image-klein.safetensors",
	})
	service.Caption = func(context.Context, images.CaptionRequest) (images.CaptionResult, error) {
		return images.CaptionResult{Text: "A red fox stands in deep snow at golden hour.", Model: "qwen3-8b"}, nil
	}

	if _, err := service.Execute(context.Background(), chatID, ToolGenerateImage, `{"prompt":"a fox"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.got.Steps != 16 || provider.got.CfgScale != 1.0 {
		t.Errorf("drew %d steps at CFG %v; picking the Klein file alone should give 16 / 1.0",
			provider.got.Steps, provider.got.CfgScale)
	}
	if provider.got.Caption == "" {
		t.Error("picking the Klein file did not turn the caption on")
	}
}

// TestAnExplicitOffBeatsTheProfile: "off" is the only value that overrides a profile that
// wants a caption, which is what makes it a decision rather than a default.
func TestAnExplicitOffBeatsTheProfile(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, _, chatID := newImageService(t, provider, config.ImageSettings{
		Pipeline:   config.ImagePipelineFlux3Klein,
		Captioning: config.ImageCaptioningOff,
	})
	service.Caption = func(context.Context, images.CaptionRequest) (images.CaptionResult, error) {
		t.Error("a caption was requested with the switch explicitly off")
		return images.CaptionResult{Text: "unused"}, nil
	}

	if _, err := service.Execute(context.Background(), chatID, ToolGenerateImage, `{"prompt":"a fox"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.got.Caption != "" {
		t.Errorf("caption = %q, want none", provider.got.Caption)
	}
	if provider.got.Steps != 16 {
		t.Errorf("steps = %d; turning the caption off must not turn the profile off", provider.got.Steps)
	}
}

// TestASettingsValueBeatsTheProfile pins the direction: a number the user typed wins, which
// is the same rule every other image field follows.
func TestASettingsValueBeatsTheProfile(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, _, chatID := newImageService(t, provider, config.ImageSettings{
		Pipeline:   config.ImagePipelineFlux3Klein,
		Steps:      30,
		CfgScale:   7,
		Captioning: config.ImageCaptioningOff,
	})

	if _, err := service.Execute(context.Background(), chatID, ToolGenerateImage, `{"prompt":"a fox"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.got.Steps != 30 || provider.got.CfgScale != 7 {
		t.Errorf("drew %d steps at CFG %v, want the configured 30 / 7", provider.got.Steps, provider.got.CfgScale)
	}
}

// TestTheProfileDropsTheNegativePromptAndSaysSo: on a guidance-free model the field is sent,
// recorded and ignored, which reads as a broken setting. It is removed, and the tool result
// names the reason so the user can see the field is not in play.
func TestTheProfileDropsTheNegativePromptAndSaysSo(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{
		Pipeline:       config.ImagePipelineFlux3Klein,
		Captioning:     config.ImageCaptioningOff,
		NegativePrompt: "blurry, extra fingers",
	})

	out, err := service.Execute(context.Background(), chatID, ToolGenerateImage, `{"prompt":"a fox"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.got.NegativePrompt != "" {
		t.Errorf("negative prompt = %q, want it dropped", provider.got.NegativePrompt)
	}
	if !strings.Contains(out, "nicht unterstützt") {
		t.Errorf("the tool result does not say the negative prompt was dropped:\n%s", out)
	}
	attachments, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if recipe := attachments[0].Generation; recipe.NegativePrompt != "" {
		t.Errorf("the recipe records a negative prompt that was never sent: %q", recipe.NegativePrompt)
	}
}

// TestTheProfileDropsTheSecondPass: the base checkpoint's second pass has never been
// measured, so it is not drawn and the result says so. The switch is global — leaving it on
// for another model must not make these draws impossible. The distilled profile used to
// behave this way too; it does not any more, because its second pass was drawn (IMAGES.md
// §9.5) and TestTheMeasuredSecondPassIsDrawn pins that half.
func TestTheProfileDropsTheSecondPass(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{
		Pipeline:     config.ImagePipelineFlux3KleinBase,
		Captioning:   config.ImageCaptioningOff,
		HiresEnabled: true,
		HiresScale:   1.5,
		HiresDenoise: 0.4,
	})

	out, err := service.Execute(context.Background(), chatID, ToolGenerateImage, `{"prompt":"a fox"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.got.Hires.Enabled {
		t.Error("a second pass was drawn for a pipeline that has none measured")
	}
	if !strings.Contains(out, "Zweite Stufe") {
		t.Errorf("the tool result does not say the second pass was dropped:\n%s", out)
	}
	attachments, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if recipe := attachments[0].Generation; recipe.Hires {
		t.Error("the recipe claims a second pass that never ran")
	}
}

// TestTheMeasuredSecondPassIsDrawn is the other half of that decision: the distilled
// checkpoint's second pass was drawn on a real build — 512² → 768² at denoise 0.4, 18.4 s
// against 7.3 s for the base draw (IMAGES.md §9.5) — so a user who switched hires on gets
// one. The profile adds nothing and removes nothing here; only the numbers nobody chose are
// still filled.
func TestTheMeasuredSecondPassIsDrawn(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{
		Pipeline:     config.ImagePipelineFlux3Klein,
		Captioning:   config.ImageCaptioningOff,
		HiresEnabled: true,
		HiresScale:   1.5,
		HiresDenoise: 0.4,
	})

	out, err := service.Execute(context.Background(), chatID, ToolGenerateImage, `{"prompt":"a fox"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !provider.got.Hires.Enabled || provider.got.Hires.Scale != 1.5 || provider.got.Hires.Denoise != 0.4 {
		t.Errorf("hires = %+v, want the requested second pass", provider.got.Hires)
	}
	if provider.got.Steps != 16 || provider.got.CfgScale != 1.0 {
		t.Errorf("steps = %d, CFG = %v, want the profile's 16 at CFG 1.0", provider.got.Steps, provider.got.CfgScale)
	}
	if strings.Contains(out, "Zweite Stufe: für") {
		t.Errorf("the tool result still says the second pass was not drawn:\n%s", out)
	}
	attachments, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if recipe := attachments[0].Generation; !recipe.Hires {
		t.Error("the recipe does not record the second pass that ran")
	}
}

// TestAGenericPipelineLeavesTheDrawAlone is the no-change guarantee for every model without
// a profile: not one number is invented and no caption is asked for.
func TestAGenericPipelineLeavesTheDrawAlone(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, _, chatID := newImageService(t, provider, config.ImageSettings{
		Pipeline: config.ImagePipelineGeneric,
	})
	service.Caption = func(context.Context, images.CaptionRequest) (images.CaptionResult, error) {
		t.Error("a generic pipeline asked for a caption")
		return images.CaptionResult{Text: "unused"}, nil
	}

	if _, err := service.Execute(context.Background(), chatID, ToolGenerateImage, `{"prompt":"a fox"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.got.Steps != 0 || provider.got.CfgScale != 0 || provider.got.Sampler != "" {
		t.Errorf("the generic profile invented numbers: %+v", provider.got)
	}
	if provider.got.Caption != "" {
		t.Errorf("caption = %q, want none", provider.got.Caption)
	}
}
