package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/images"
	"kichat/internal/store"
)

// newUpscaleTestApp is the default app — generation off, no provider — with one uploaded
// picture on one chat. The default is the point: enlarging an attachment must not need an
// image backend, and an uploaded file has no recipe, so it is the picture the redraw path
// cannot serve at all.
func newUpscaleTestApp(t *testing.T) (*App, string, []byte) {
	t.Helper()
	settings := config.Default()
	if settings.Images.Enabled {
		t.Fatal("the default settings draw pictures, so this test is not exercising the off state")
	}
	return newUpscaleTestAppWith(t, settings)
}

// newUpscaleTestAppWith is the same fixture under given settings, for the tests that need
// the neural path switched on. Generation stays off either way: an enlargement must not
// need an image server, and the neural method must not need one quietly.
func newUpscaleTestAppWith(t *testing.T, settings config.Settings) (*App, string, []byte) {
	t.Helper()
	a := newSettingsTestApp(t, settings, t.TempDir())
	a.ctx = context.Background()
	a.rebuildServices(settings)

	const chatID = "chat-upscale"
	if err := a.store.CreateChat(store.Chat{ID: chatID, Title: "Bilder", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	source := image.NewNRGBA(image.Rect(0, 0, 32, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			source.SetNRGBA(x, y, color.NRGBA{R: 40, G: 90, B: 200, A: 255})
		}
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, source); err != nil {
		t.Fatal(err)
	}
	if err := a.store.CreateDocument(store.Document{
		ID: "doc-src", AttachmentID: "src", ChatID: chatID, Title: "photo.png",
		Text: base64.StdEncoding.EncodeToString(raw.Bytes()), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.CreateAttachment(store.Attachment{
		ID: "src", ChatID: chatID, MessageID: "msg-1", Kind: "image",
		Filename: "photo.png", Mime: "image/png", SizeBytes: int64(raw.Len()),
		Placement: store.PlacementInline, DocumentID: "doc-src", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return a, chatID, raw.Bytes()
}

// TestUpscaleAttachmentWorksWithGenerationOff is the promise the feature was added for:
// enlarging a picture is a picture operation, not a draw. The default image settings have
// generation off and no provider at all, and the call still has to work — including on an
// uploaded file, which has no recipe to redraw.
func TestUpscaleAttachmentWorksWithGenerationOff(t *testing.T) {
	a, _, sourceBytes := newUpscaleTestApp(t)
	// The view holds the message's attachment list, so the new picture has to be
	// announced or it exists and stays invisible until a reload.
	var emitted []string
	a.emitFn = func(name string, _ any) { emitted = append(emitted, name) }

	made, err := a.UpscaleAttachment("src", images.UpscaleRequest{
		Target:   images.TargetCustom,
		LongSide: 96,
		Method:   images.MethodLanczos,
	})
	if err != nil {
		t.Fatalf("UpscaleAttachment: %v", err)
	}
	if made.ID == "src" {
		t.Fatal("the source was replaced instead of copied")
	}
	if made.MessageID != "msg-1" {
		t.Errorf("the copy is on message %q, want the source's message", made.MessageID)
	}
	if made.Generation != nil {
		t.Errorf("the copy reports a recipe: %+v", made.Generation)
	}
	if made.Derivation == nil || made.Derivation.SourceAttachmentID != "src" {
		t.Fatalf("the copy reports derivation %+v", made.Derivation)
	}
	if made.Derivation.Method != images.MethodLanczos || made.Derivation.Scale != 3 {
		t.Errorf("derivation = %+v, want lanczos at 3x", made.Derivation)
	}
	if made.Derivation.SourceWidth != 32 || made.Derivation.SourceHeight != 24 {
		t.Errorf("derivation records the source as %d×%d, want 32×24",
			made.Derivation.SourceWidth, made.Derivation.SourceHeight)
	}
	if made.Filename != "photo-96w.png" {
		t.Errorf("filename = %q", made.Filename)
	}
	if !slices.Contains(emitted, events.Attachments) {
		t.Errorf("the copy was filed without telling the window: %v", emitted)
	}

	// The source is still there, exactly as it was.
	sourceDoc, err := a.store.GetDocument("doc-src")
	if err != nil {
		t.Fatal(err)
	}
	if sourceDoc.Text != base64.StdEncoding.EncodeToString(sourceBytes) {
		t.Error("the source bytes changed")
	}

	// And the neural path is refused by the switch before any program is looked for.
	if _, err := a.UpscaleAttachment("src", images.UpscaleRequest{
		Target: images.TargetCustom, LongSide: 64, Method: images.MethodESRGAN,
	}); !errors.Is(err, images.ErrUpscaleDisabled) {
		t.Fatalf("neural err = %v, want ErrUpscaleDisabled", err)
	}
}

// stubUpscalerProgram is a stand-in for realesrgan-ncnn-vulkan that never finishes on its
// own: it copies its input to its output, but only after a minute. It is what makes the
// stopped state reachable — the state the progress row and its Stop exist for — without a
// Vulkan card and without a test that waits out the timeout it is trying not to reach.
func stubUpscalerProgram(t *testing.T) string {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("the stub program is a shell script")
	}
	script := "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    -i) in=$2; shift 2;;\n" +
		"    -o) out=$2; shift 2;;\n" +
		"    *) shift;;\n" +
		"  esac\n" +
		"done\n" +
		"sleep 60\n" +
		"cp \"$in\" \"$out\"\n"
	path := filepath.Join(t.TempDir(), "realesrgan-ncnn-vulkan")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAStoppedUpscaleEndsTheWaitAndLeavesNothingBehind is the whole feature as the user
// meets it: a progress row appears for the method that waits, Stop ends it there rather
// than at the fifteen-minute bound, the row is taken off the screen by the same release
// every draw uses, and a stopped enlargement files no picture.
func TestAStoppedUpscaleEndsTheWaitAndLeavesNothingBehind(t *testing.T) {
	settings := config.Default()
	settings.Images.UpscalerEnabled = true
	settings.Images.ESRGANBinary = stubUpscalerProgram(t)
	a, chatID, _ := newUpscaleTestAppWith(t, settings)

	// The window's view of it, as events: the reports in order, and everything else for
	// the assertions about what was and was not announced.
	progress := make(chan map[string]any, 8)
	others := make(chan string, 8)
	a.emitFn = func(name string, payload any) {
		if name != events.ImageProgress {
			others <- name
			return
		}
		if m, ok := payload.(map[string]any); ok {
			progress <- m
		}
	}

	finished := make(chan error, 1)
	go func() {
		_, err := a.UpscaleAttachment("src", images.UpscaleRequest{
			Target: images.TargetCustom, LongSide: 96, Method: images.MethodESRGAN,
		})
		finished <- err
	}()

	var first map[string]any
	select {
	case first = <-progress:
	case <-time.After(10 * time.Second):
		t.Fatal("no progress was reported, so there is no row and no Stop to press")
	}
	if first["drawId"] != "src" {
		t.Errorf("the row is keyed %v, want the source attachment's own id", first["drawId"])
	}
	if first["chatId"] != chatID {
		t.Errorf("the row is scoped to chat %v, want %q", first["chatId"], chatID)
	}
	if first["phase"] != images.PhaseUpscaling {
		t.Errorf("phase = %v, want %q", first["phase"], images.PhaseUpscaling)
	}
	if first["done"] != false {
		t.Errorf("the first report already says it is over: %v", first["done"])
	}

	if result := a.StopImageDraw("src"); !result.WasDrawing {
		t.Fatalf("Stop found nothing to stop: %+v", result)
	}

	select {
	case err := <-finished:
		if !errors.Is(err, images.ErrCancelled) {
			t.Fatalf("a stopped upscale returned %v, want ErrCancelled", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the stopped upscale never returned")
	}

	// The row has to be taken off the screen by the release, which is the only event that
	// means a wait is over: a row that outlives its upscale is a Stop button for nothing.
	var cleared bool
	deadline := time.After(5 * time.Second)
	for !cleared {
		select {
		case report := <-progress:
			if report["drawId"] == "src" && report["done"] == true {
				cleared = true
			}
		case <-deadline:
			t.Fatal("no done report arrived, so the row would stay on screen")
		}
	}

	attachments, err := a.store.ListAttachments(chatID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 1 {
		t.Errorf("a stopped upscale filed %d extra attachment(s)", len(attachments)-1)
	}
	select {
	case name := <-others:
		t.Errorf("a stopped upscale announced %s", name)
	default:
	}
}

// TestAnUpscaleTakesTheSameSlotAsADraw pins the shared rule rather than a second one: a
// 4K encode does not use the image server, but it does use memory and the window has one
// Stop, so it is the same slot. Refusing has to happen before the slot is taken and
// before anything is read — a refusal that started work would be a second picture running.
func TestAnUpscaleTakesTheSameSlotAsADraw(t *testing.T) {
	a, _, _ := newUpscaleTestApp(t)
	release, ok := a.tryDraw()
	if !ok {
		t.Fatal("the slot was already taken in a fresh app")
	}
	defer release()

	if _, err := a.UpscaleAttachment("src", images.UpscaleRequest{
		Target: images.TargetFHD, Method: images.MethodLanczos,
	}); !errors.Is(err, images.ErrDrawInProgress) {
		t.Fatalf("err = %v, want ErrDrawInProgress", err)
	}
}
