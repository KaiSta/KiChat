package images

import (
	"context"
	"errors"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveESRGAN runs the neural tier against the real portable program. It is skipped
// unless both variables are set, so the ordinary suite never downloads, starts, or spends
// GPU seconds on anything:
//
//	KICHAT_LIVE=1 KICHAT_ESRGAN=/path/to/realesrgan-ncnn-vulkan go test ./internal/images/ -run TestLiveESRGAN -v
//
// Three claims are checked, and each of them is one the offline tests can only fake with a
// stand-in program:
//
//   - the program starts at all on this machine, which is a Vulkan question and not a file
//     question. The released build is Vulkan-only and has no CPU switch, so a machine with
//     no driver fails here rather than quietly resampling.
//   - the weights load. The published archives differ in a way that is invisible from the
//     exit code: the small `Real-ESRGAN-ncnn-vulkan` v0.2.0 archive contains no `models/`
//     folder, and the program then reports the failure in its log, writes an all-black
//     picture, and exits 0. That is what TestANeuralRunWithoutWeightsIsAFailure reproduces
//     with a stub; here it is the real thing, and ErrUpscalerNoModels is the assertion.
//   - the answer really is four times the input on the long side, from the model rather
//     than from the resampler, which is what `-s 4` and the four-times check together say.
//
// The picture is deliberately tiny — 64² in, 256² out — because what is being verified is
// the wiring, not the art. A 512² source costs about two seconds on the GPU measured here
// and about twenty on a software Vulkan device, and both of those belong in a benchmark
// rather than in a test that must stay quick.
func TestLiveESRGAN(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to run against a real Real-ESRGAN binary")
	}
	program := os.Getenv("KICHAT_ESRGAN")
	if program == "" {
		t.Skip("set KICHAT_ESRGAN=/path/to/realesrgan-ncnn-vulkan to run this test")
	}
	if _, err := os.Stat(program); err != nil {
		t.Fatalf("KICHAT_ESRGAN=%s: %v", program, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// The program is usable, which for this build means the driver came up.
	if _, err := ProbeESRGAN(ctx, program); err != nil {
		t.Fatalf("the program did not run: %v", err)
	}
	// The weights are beside it, or nothing below means anything.
	if _, err := os.Stat(filepath.Join(filepath.Dir(program), "models", ESRGANDefaultModel+".bin")); err != nil {
		t.Fatalf("no %s weights beside %s: %v", ESRGANDefaultModel, program, err)
	}

	source := checkerboard(64, 32)
	start := time.Now()
	result, err := Upscale(ctx, encodePNG(t, source), UpscaleRequest{
		Target: TargetCustom, LongSide: 256, Method: MethodESRGAN,
	}, UpscaleOptions{Enabled: true, Binary: program})
	took := time.Since(start)
	if errors.Is(err, ErrUpscalerNoModels) {
		t.Fatalf("the program ran but loaded no weights: %v", err)
	}
	if err != nil {
		t.Fatalf("neural upscale: %v", err)
	}
	if result.Width != 256 || result.Height != 128 {
		t.Errorf("result is %dx%d, want 256x128", result.Width, result.Height)
	}
	if result.Model != ESRGANDefaultModel {
		t.Errorf("model = %q, want %q", result.Model, ESRGANDefaultModel)
	}
	// The model answered rather than the resampler: the classical pass alone would already
	// produce the right size, so the evidence is that the run happened at all and said what
	// it used.
	t.Logf("real Real-ESRGAN: 64² → %d×%d in %s (model %s)", result.Width, result.Height, took.Round(time.Millisecond), result.Model)
}

// checkerboard is a source with edges in both directions, so a wrong scale or a transposed
// output is visible rather than merely soft.
func checkerboard(width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if (x/4+y/4)%2 == 0 {
				img.SetRGBA(x, y, color.RGBA{R: 230, G: 30, B: 30, A: 255})
				continue
			}
			img.SetRGBA(x, y, color.RGBA{R: 20, G: 60, B: 230, A: 255})
		}
	}
	return img
}
