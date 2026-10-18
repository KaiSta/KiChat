package images

import (
	"bytes"
	"context"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"
)

// Live: a real FLUX 3 Klein draw through the CLI backend, on the machine that has the
// checkpoint. Skipped unless KICHAT_LIVE=1 and the whole set is named, so the normal suite
// stays offline and never loads 6 GB of weights.
//
//	KICHAT_LIVE=1 KICHAT_SD_CLI=~/sdMaster/buildVulkan/sd-cli \
//	  KICHAT_SD_DIFFUSION_MODEL=~/sdMaster/models/flux3/flux-3-image-klein-UD-Q4_K_XL.gguf \
//	  KICHAT_SD_LLM=~/sdMaster/models/flux3/Qwen3VL-4B-Instruct-Q4_K_M.gguf \
//	  KICHAT_SD_VAE=~/sdMaster/models/flux2/flux2-vae.safetensors \
//	  go test ./internal/images/ -run TestLiveFlux3 -v
//
// Three draws, about a minute: one plain 512², then the same seed twice with and without a
// negative prompt. What it settles is the profile — that the numbers come from the file in
// front of it, that they are the ones the checkpoint draws well at, and that the two things
// the profile removes are the ones that do nothing.
func TestLiveFlux3KleinProfile(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to draw with a real sd-cli")
	}
	binary := strings.TrimSpace(os.Getenv("KICHAT_SD_CLI"))
	files := ModelFiles{
		DiffusionModel: strings.TrimSpace(os.Getenv("KICHAT_SD_DIFFUSION_MODEL")),
		LLM:            strings.TrimSpace(os.Getenv("KICHAT_SD_LLM")),
		VAE:            strings.TrimSpace(os.Getenv("KICHAT_SD_VAE")),
	}
	if binary == "" || files.DiffusionModel == "" || files.LLM == "" || files.VAE == "" {
		t.Skip("set KICHAT_SD_CLI, KICHAT_SD_DIFFUSION_MODEL, KICHAT_SD_LLM and KICHAT_SD_VAE")
	}

	// 1. The profile comes from the file, whatever it is called. This is the whole point of
	// reading the header: the path below is the user's own, and it need not say `klein`.
	pipeline := PipelineFor(PipelineAuto, files.DiffusionModel)
	t.Logf("%s → %s from %q (%d steps, CFG %v, sampler %q)",
		files.DiffusionModel, pipeline.ID, pipeline.Source, pipeline.Steps, pipeline.CFG, pipeline.Sampler)
	if pipeline.ID != PipelineFlux3Klein || pipeline.Source != SourceWeights {
		t.Fatalf("the configured checkpoint resolved to %q from %q, want the distilled profile from its weights",
			pipeline.ID, pipeline.Source)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// 2. Health points at the missing part rather than at the first draw failing after a
	// multi-gigabyte load: without the VAE the binary cannot start at all.
	withoutVAE := files
	withoutVAE.VAE = ""
	if err := NewCLI("images", "sd-cli", binary, withoutVAE, Options{Pipeline: pipeline}).Health(ctx); err == nil {
		t.Error("Health accepted a Klein set with no VAE")
	} else {
		t.Logf("Health without a VAE: %v", err)
	}

	client := NewCLI("images", "sd-cli", binary, files, Options{Pipeline: pipeline})
	if err := client.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}

	prompt := "A photograph of a tabby cat sitting on a wooden windowsill, morning light from the left, shallow depth of field, warm dust in the air"
	seed := int64(4242)

	// 3. One draw at the profile's numbers.
	req, _ := pipeline.Adjusted(Request{Prompt: prompt, Width: 512, Height: 512, Seed: &seed})
	start := time.Now()
	reports, last := 0, Progress{}
	result, err := client.GenerateWithProgress(ctx, req, func(p Progress) {
		reports++
		last = p
	})
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	if len(result.Images) == 0 {
		t.Fatal("the draw returned no image")
	}
	width, height, ok := Size(result.Images[0].Data)
	if !ok || width != 512 || height != 512 {
		t.Errorf("drew %dx%d, want 512x512", width, height)
	}
	t.Logf("512² at %d steps, CFG %v in %s (%d progress reports, last step %d of %d, seed %d)",
		req.Steps, req.CfgScale, time.Since(start).Round(time.Millisecond), reports, last.Step, last.Steps, result.Seed)
	if reports == 0 {
		t.Error("not one progress report: the step counter this app parses has moved")
	}

	// 4. The negative prompt the profile removes is inert at CFG 1.0 — which is why removing
	// it is a decision and not a loss. Two draws, same seed, one flag apart.
	withNegative := req
	withNegative.NegativePrompt = "blurry, extra fingers, watermark"
	plain, err := client.Generate(ctx, req)
	if err != nil {
		t.Fatalf("draw without a negative prompt: %v", err)
	}
	negated, err := client.Generate(ctx, withNegative)
	if err != nil {
		t.Fatalf("draw with a negative prompt: %v", err)
	}
	if !bytes.Equal(pixelsOf(t, plain.Images[0].Data), pixelsOf(t, negated.Images[0].Data)) {
		t.Error("a negative prompt changed the picture at CFG 1.0 — the profile is dropping one that does something")
	}
}

// pixelsOf is a picture's raw RGB, for comparing two draws a flag apart.
func pixelsOf(t *testing.T, data []byte) []byte {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	bounds := img.Bounds()
	out := make([]byte, 0, bounds.Dx()*bounds.Dy()*3)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			out = append(out, byte(r>>8), byte(g>>8), byte(b>>8))
		}
	}
	return out
}
