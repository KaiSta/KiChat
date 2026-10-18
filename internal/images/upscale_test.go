package images

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFitPreservesAspectAndBounds(t *testing.T) {
	cases := []struct {
		name             string
		srcW, srcH, long int
		wantW, wantH     int
		why              string
	}{
		{"landscape to FHD", 3000, 2000, 1920, 1920, 1280, "the long side is the preset and the short side follows"},
		{"portrait to FHD", 2000, 3000, 1920, 1280, 1920, "a portrait keeps its orientation"},
		{"square to FHD", 512, 512, 1920, 1920, 1920, "a square stays square"},
		{"already the target", 1920, 1080, 1920, 1920, 1080, "a picture at the target is not enlarged"},
		{"larger than the target", 3840, 2160, 1920, 1920, 1080, "FHD means FHD, not at least FHD"},
		{"odd source", 801, 601, 1920, 1920, 1441, "the short side is rounded, not truncated"},
		{"long side capped", 100, 50, 9000, MaxUpscaleLongSide, MaxUpscaleLongSide / 2, "the cap bounds memory"},
		{"pixel cap", 8192, 8192, 8192, 8000, 8000, "a square at the cap is shrunk to 64 MP"},
		{"nothing to compute", 0, 100, 1920, 0, 0, "a missing dimension is not a request"},
	}
	for _, c := range cases {
		got := Fit(c.srcW, c.srcH, c.long)
		if got.Width != c.wantW || got.Height != c.wantH {
			t.Errorf("%s: Fit(%d,%d,%d) = %dx%d, want %dx%d — %s",
				c.name, c.srcW, c.srcH, c.long, got.Width, got.Height, c.wantW, c.wantH, c.why)
		}
	}
}

func TestTargetForPresets(t *testing.T) {
	cases := []struct {
		kind    string
		long    int
		wantW   int
		wantH   int
		wantErr bool
	}{
		{TargetFHD, 0, 1920, 1080, false},
		{TargetUHD, 0, 3840, 2160, false},
		{TargetCustom, 2560, 2560, 1440, false},
		{TargetCustom, 0, 0, 0, true},
		{"bigger", 0, 0, 0, true},
	}
	for _, c := range cases {
		got, err := TargetFor(c.kind, c.long, 3840, 2160)
		if c.wantErr {
			if err == nil {
				t.Errorf("TargetFor(%q) accepted an invalid target: %+v", c.kind, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("TargetFor(%q): %v", c.kind, err)
			continue
		}
		if got.Width != c.wantW || got.Height != c.wantH {
			t.Errorf("TargetFor(%q) = %dx%d, want %dx%d", c.kind, got.Width, got.Height, c.wantW, c.wantH)
		}
	}
}

func TestUpscaleEnlargesAndKeepsTheContainer(t *testing.T) {
	source := encodePNG(t, solid(4, 4, color.NRGBA{R: 200, G: 60, B: 30, A: 255}))
	result, err := Upscale(context.Background(), source, UpscaleRequest{
		Target:   TargetCustom,
		LongSide: 12,
		Method:   MethodLanczos,
	}, UpscaleOptions{})
	if err != nil {
		t.Fatalf("Upscale: %v", err)
	}
	if result.Width != 12 || result.Height != 12 {
		t.Errorf("upscaled to %dx%d, want 12x12", result.Width, result.Height)
	}
	if result.Image.Mime != MimePNG || result.Image.Ext != "png" {
		t.Errorf("a PNG became %s/%s", result.Image.Mime, result.Image.Ext)
	}
	img, err := png.Decode(bytes.NewReader(result.Image.Data))
	if err != nil {
		t.Fatalf("the result is not a PNG: %v", err)
	}
	if got := color.NRGBAModel.Convert(img.At(6, 6)).(color.NRGBA); got.R < 190 || got.G > 70 || got.B > 40 {
		t.Errorf("the enlarged middle drifted to %v", got)
	}
}

func TestUpscaleJPEGStaysJPEG(t *testing.T) {
	raw := solid(32, 16, color.NRGBA{R: 20, G: 120, B: 200, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, raw, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	result, err := Upscale(context.Background(), buf.Bytes(), UpscaleRequest{
		Target: TargetCustom, LongSide: 64, Method: MethodLanczos,
	}, UpscaleOptions{})
	if err != nil {
		t.Fatalf("Upscale: %v", err)
	}
	if result.Image.Mime != MimeJPEG || result.Image.Ext != "jpg" {
		t.Errorf("a JPEG became %s/%s", result.Image.Mime, result.Image.Ext)
	}
	if _, err := jpeg.Decode(bytes.NewReader(result.Image.Data)); err != nil {
		t.Errorf("the result is not a JPEG: %v", err)
	}
}

// TestUpscaleKeepsTransparencyOpaque is the alpha trap: resampling straight alpha pulls
// the colour under a transparent pixel into its neighbours and leaves a dark fringe.
func TestUpscaleKeepsTransparencyOpaque(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			if x == 0 || y == 0 {
				src.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 0})
				continue
			}
			src.SetNRGBA(x, y, color.NRGBA{R: 240, G: 40, B: 20, A: 255})
		}
	}
	result, err := Upscale(context.Background(), encodePNG(t, src), UpscaleRequest{
		Target: TargetCustom, LongSide: 16, Method: MethodLanczos,
	}, UpscaleOptions{})
	if err != nil {
		t.Fatalf("Upscale: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(result.Image.Data))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, a := img.At(8, 8).RGBA(); a == 0 {
		t.Error("the opaque middle became transparent")
	}
	// The corner is outside the opaque square; ringing may leave a trace, but a dark
	// premultiplied fringe would show up as a non-zero colour with near-zero alpha.
	r, g, b, a := img.At(0, 0).RGBA()
	if a < 0x1000 && (r > 0x2000 || g > 0x2000 || b > 0x2000) {
		t.Errorf("transparent corner carries colour %d/%d/%d at alpha %d", r, g, b, a)
	}
}

// stubESRGAN writes a stand-in for realesrgan-ncnn-vulkan: a program that reads the flags
// the client passes, optionally sleeps, and copies its input to its output.
//
// A stub is the only way the neural tier is testable without a Vulkan card, and it covers
// what matters about the client rather than the network: that the phase is reported once
// the program is really about to run, that a cancelled run kills the process instead of
// waiting out its timeout, and that the output is then resampled onto the exact target.
// Copying rather than enlarging is deliberate — the fit is the client's job, so a stub that
// produced a 4× picture would hide a client that forgot to resample.
func stubESRGAN(t *testing.T, sleepSeconds int) string {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("the stub program is a shell script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    -i) in=$2; shift 2;;\n" +
		"    -o) out=$2; shift 2;;\n" +
		"    *) shift;;\n" +
		"  esac\n" +
		"done\n"
	if sleepSeconds > 0 {
		// The pid is written beside the script rather than in the client's temp directory,
		// which is removed the moment the run ends: the test has to be able to look up a
		// process that is supposed to be gone, and "killed" is a different fact from "the
		// call returned" — the timeout would also return, five seconds later.
		script += fmt.Sprintf("echo $$ > %q\n", filepath.Join(dir, "pid")) +
			fmt.Sprintf("sleep %d\n", sleepSeconds)
	}
	script += "cp \"$in\" \"$out\"\n"
	path := filepath.Join(dir, "realesrgan-ncnn-vulkan")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestANeuralRunWithoutWeightsIsAFailure pins the trap the measurements found: a program that
// cannot load its weights is not a program that fails.
//
// Measured against the v0.2.0 archive, whose Linux zip is 3.6 MB and contains no `models/` at
// all: the program prints `fopen …realesrgan-x4plus.param failed` and
// `find_blob_index_by_name data failed`, writes an 880×880 PNG that is black at every sampled
// pixel, and exits **0**. Size and dimensions are exactly what a good run produces, so the log
// line is the only honest signal — and without this the app files the black picture as the
// enlargement.
func TestANeuralRunWithoutWeightsIsAFailure(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("the stub program is a shell script")
	}
	dir := t.TempDir()
	// A stand-in that behaves like the real one did: it exits 0, it writes a decodable picture
	// (the client's own input copied through, so nothing about the file is wrong), and the only
	// evidence of the failure is in its output.
	script := "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    -i) in=$2; shift 2;;\n" +
		"    -o) out=$2; shift 2;;\n" +
		"    *) shift;;\n" +
		"  esac\n" +
		"done\n" +
		"echo 'fopen /opt/upscaler/models/realesrgan-x4plus.param failed'\n" +
		"echo 'find_blob_index_by_name data failed'\n" +
		"cp \"$in\" \"$out\"\n"
	program := filepath.Join(dir, "realesrgan-ncnn-vulkan")
	if err := os.WriteFile(program, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Upscale(context.Background(), encodePNG(t, solid(8, 8, color.NRGBA{A: 255})), UpscaleRequest{
		Target: TargetCustom, LongSide: 32, Method: MethodESRGAN,
	}, UpscaleOptions{Enabled: true, Binary: program})
	if err == nil {
		t.Fatal("a run that loaded no weights was reported as a successful enlargement")
	}
	if !errors.Is(err, ErrUpscalerNoModels) {
		t.Errorf("error = %v, want ErrUpscalerNoModels", err)
	}
	// The program's own line has to travel with the error: without it the answer is
	// "something went wrong", and the line is what says the weights never loaded.
	if !strings.Contains(err.Error(), "fopen") && !strings.Contains(err.Error(), "find_blob_index_by_name") {
		t.Errorf("the error hides the program's own reason: %v", err)
	}
}

// TestAModelsFolderWithoutTheChosenModelIsRefused is the other half: when the archive did ship
// a `models/` folder but not the weights the panel is set to, the run is refused before the
// program starts, and the error names the file rather than the folder.
func TestAModelsFolderWithoutTheChosenModelIsRefused(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("the stub program is a shell script")
	}
	dir := t.TempDir()
	program := filepath.Join(dir, "realesrgan-ncnn-vulkan")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	models := filepath.Join(dir, "models")
	if err := os.MkdirAll(models, 0o755); err != nil {
		t.Fatal(err)
	}
	// The anime weights are there; x4plus is not.
	for _, name := range []string{"realesrgan-x4plus-anime.param", "realesrgan-x4plus-anime.bin"} {
		if err := os.WriteFile(filepath.Join(models, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := Upscale(context.Background(), encodePNG(t, solid(8, 8, color.NRGBA{A: 255})), UpscaleRequest{
		Target: TargetCustom, LongSide: 32, Method: MethodESRGAN, Model: ESRGANDefaultModel,
	}, UpscaleOptions{Enabled: true, Binary: program})
	if !errors.Is(err, ErrUpscalerNoModels) {
		t.Fatalf("err = %v, want ErrUpscalerNoModels", err)
	}
	// Either half of the pair is the right thing to name — what matters is that it names a
	// file of the model that was asked for, inside the folder that was searched.
	if !strings.Contains(err.Error(), filepath.Join(models, ESRGANDefaultModel)) {
		t.Errorf("the error does not name the missing file of %s: %v", ESRGANDefaultModel, err)
	}
}

// TestTheNeuralPhaseIsReportedOnlyWhenTheProgramStarts pins the row the Stop lives in: the
// callback fires once, for the neural method, after every refusal has been passed — so a
// request that cannot run never leaves a progress row behind, and the fast method never
// flashes one.
func TestTheNeuralPhaseIsReportedOnlyWhenTheProgramStarts(t *testing.T) {
	source := encodePNG(t, solid(8, 8, color.NRGBA{R: 10, G: 200, B: 120, A: 255}))
	stub := stubESRGAN(t, 0)

	var phases []string
	result, err := Upscale(context.Background(), source, UpscaleRequest{
		Target: TargetCustom, LongSide: 32, Method: MethodESRGAN,
	}, UpscaleOptions{
		Enabled: true,
		Binary:  stub,
		OnPhase: func(phase string) { phases = append(phases, phase) },
	})
	if err != nil {
		t.Fatalf("Upscale: %v", err)
	}
	if len(phases) != 1 || phases[0] != PhaseUpscaling {
		t.Errorf("phases = %v, want one %q", phases, PhaseUpscaling)
	}
	// And the 4× answer is landed on what was asked for: the stub copies, so this is the
	// client's own resampler doing the work.
	if result.Width != 32 || result.Height != 32 {
		t.Errorf("result is %dx%d, want 32x32", result.Width, result.Height)
	}

	phases = nil
	if _, err := Upscale(context.Background(), source, UpscaleRequest{
		Target: TargetCustom, LongSide: 32, Method: MethodLanczos,
	}, UpscaleOptions{Enabled: true, Binary: stub, OnPhase: func(phase string) { phases = append(phases, phase) }}); err != nil {
		t.Fatalf("classical Upscale: %v", err)
	}
	if len(phases) != 0 {
		t.Errorf("the classical pass reported phases %v, and a row for a second of work is a flicker", phases)
	}

	if _, err := Upscale(context.Background(), source, UpscaleRequest{
		Target: TargetCustom, LongSide: 32, Method: MethodESRGAN,
	}, UpscaleOptions{Enabled: false, Binary: stub, OnPhase: func(phase string) { phases = append(phases, phase) }}); !errors.Is(err, ErrUpscaleDisabled) {
		t.Fatalf("err = %v, want ErrUpscaleDisabled", err)
	}
	if len(phases) != 0 {
		t.Errorf("a refused request reported phases %v, so it would have left a row behind", phases)
	}
}

// TestAStoppedNeuralRunKillsTheProgramAndReturns: the wait is minutes on a small card, and
// cancelling has to end it there rather than at the timeout. The stub writes its pid, so the
// assertion is about the process and not only about how long the call took.
func TestAStoppedNeuralRunKillsTheProgramAndReturns(t *testing.T) {
	stub := stubESRGAN(t, 60)
	reported := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := Upscale(ctx, encodePNG(t, solid(8, 8, color.NRGBA{R: 255, A: 255})), UpscaleRequest{
			Target: TargetCustom, LongSide: 32, Method: MethodESRGAN,
		}, UpscaleOptions{
			Enabled: true,
			Binary:  stub,
			OnPhase: func(string) { once.Do(func() { close(reported) }) },
		})
		done <- err
	}()

	select {
	case <-reported:
	case <-time.After(10 * time.Second):
		t.Fatal("the neural run never reached the program")
	}
	pid := stubPid(t, stub)
	stop := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled run returned %v, want context.Canceled", err)
		}
		if elapsed := time.Since(stop); elapsed > 5*time.Second {
			t.Errorf("cancelling took %s, so the program was left to finish", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled run never returned, so the program outlived the Stop")
	}
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the program (pid %d) is still running after the run was stopped", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stubPid reads the pid the stub wrote, which is the process group leader the client is
// expected to have killed.
//
// It waits for the file: the phase is reported just *before* the program is started, so at
// that moment there is nothing to read yet.
func stubPid(t *testing.T, stub string) int {
	t.Helper()
	file := filepath.Join(filepath.Dir(stub), "pid")
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(file)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatalf("the stub wrote %q as a pid: %v", data, err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stub program never wrote its pid: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// BenchmarkResample512To1920 is the number the document refuses to guess at: how long the
// classical pass takes on a real 512² picture enlarged to FHD, which is the case the feature
// exists for. The doc quotes this machine's figure; a different machine's is one command
// away.
func BenchmarkResample512To1920(b *testing.B) {
	source := encodePNG(b, solid(512, 512, color.NRGBA{R: 90, G: 140, B: 210, A: 255}))
	req := UpscaleRequest{Target: TargetFHD, Method: MethodLanczos}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Upscale(context.Background(), source, req, UpscaleOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResample512To4K is the same at the expensive preset, because the cost is not
// linear in the long side and the doc quotes both.
func BenchmarkResample512To4K(b *testing.B) {
	source := encodePNG(b, solid(512, 512, color.NRGBA{R: 90, G: 140, B: 210, A: 255}))
	req := UpscaleRequest{Target: TargetUHD, Method: MethodLanczos}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Upscale(context.Background(), source, req, UpscaleOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestUpscaleRefusesTheNeuralPathWhileDisabled(t *testing.T) {
	_, err := Upscale(context.Background(), encodePNG(t, solid(4, 4, color.NRGBA{R: 255, G: 255, B: 255, A: 255})), UpscaleRequest{
		Target: TargetCustom, LongSide: 8, Method: MethodESRGAN,
	}, UpscaleOptions{Enabled: false})
	if err == nil {
		t.Fatal("the neural method ran with the switch off")
	}
	if !errors.Is(err, ErrUpscaleDisabled) {
		t.Errorf("error = %v, want ErrUpscaleDisabled", err)
	}
}

func TestJPEGOrientationIsReadAndApplied(t *testing.T) {
	raw := image.NewNRGBA(image.Rect(0, 0, 2, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 2; x++ {
			raw.SetNRGBA(x, y, color.NRGBA{R: uint8(40 * x), G: uint8(60 * y), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, raw, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	for orientation := 2; orientation <= 8; orientation++ {
		withExif := insertOrientation(buf.Bytes(), orientation)
		if got := jpegOrientation(withExif); got != orientation {
			t.Errorf("orientation %d read as %d", orientation, got)
			continue
		}
		img, format, err := decodeForUpscale(withExif)
		if err != nil || format != "jpeg" {
			t.Fatalf("decode orientation %d: %v (%s)", orientation, err, format)
		}
		w, h := img.Bounds().Dx(), img.Bounds().Dy()
		wantW, wantH := 2, 4
		if orientation >= 5 {
			wantW, wantH = 4, 2
		}
		if w != wantW || h != wantH {
			t.Errorf("orientation %d produced %dx%d, want %dx%d", orientation, w, h, wantW, wantH)
		}
	}
}

func solid(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// encodePNG takes a testing.TB so the benchmarks below can use the same encoder as the
// tests, rather than a second one that could drift.
func encodePNG(t testing.TB, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// insertOrientation puts a minimal EXIF APP1 segment directly after SOI. It is the
// smallest valid TIFF block that carries one SHORT tag, which is all the reader claims
// to understand.
func insertOrientation(jpegBytes []byte, orientation int) []byte {
	tiff := make([]byte, 0, 26)
	tiff = append(tiff, 'I', 'I')   // little endian
	tiff = append(tiff, 42, 0)      // magic
	tiff = append(tiff, 8, 0, 0, 0) // IFD0 at offset 8
	tiff = append(tiff, 1, 0)       // one entry
	tiff = append(tiff, 0x12, 0x01) // tag 0x0112
	tiff = append(tiff, 3, 0)       // SHORT
	tiff = append(tiff, 1, 0, 0, 0) // count 1
	tiff = append(tiff, byte(orientation), 0, 0, 0)
	tiff = append(tiff, 0, 0, 0, 0) // next IFD
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segment := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:], uint16(len(payload)+2))
	out := make([]byte, 0, len(jpegBytes)+len(segment)+len(payload))
	out = append(out, jpegBytes[:2]...)
	out = append(out, segment...)
	out = append(out, payload...)
	out = append(out, jpegBytes[2:]...)
	return out
}
