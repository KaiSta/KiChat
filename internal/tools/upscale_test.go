package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/images"
	"kichat/internal/store"
)

// TestUpscaleFilesADerivedCopy is the whole fast path in one test: the source is
// untouched, the copy is a second attachment on the same message, and its provenance
// says where it came from and how it was made.
func TestUpscaleFilesADerivedCopy(t *testing.T) {
	service, st, chatID := newImageService(t, nil, config.ImageSettings{UpscaleSharpen: 0.25})
	sourceID := seedPicture(t, st, chatID, 64, 48)
	before := documentText(t, st, sourceID)

	got, err := service.Upscale(context.Background(), sourceID, images.UpscaleRequest{
		Target:   images.TargetCustom,
		LongSide: 192,
		Method:   images.MethodLanczos,
		Sharpen:  0.25,
	})
	if err != nil {
		t.Fatalf("Upscale: %v", err)
	}
	if got.ID == sourceID {
		t.Fatal("the source was replaced instead of copied")
	}
	if got.MessageID != "msg-1" {
		t.Errorf("the copy is on message %q, want the source's message", got.MessageID)
	}
	if got.Generation != nil {
		t.Errorf("an enlarged copy reports a generation recipe: %+v", got.Generation)
	}
	if got.Derivation == nil {
		t.Fatal("the copy has no provenance")
	}
	if got.Derivation.SourceAttachmentID != sourceID {
		t.Errorf("provenance names %q, want %q", got.Derivation.SourceAttachmentID, sourceID)
	}
	if got.Derivation.Method != images.MethodLanczos {
		t.Errorf("method = %q", got.Derivation.Method)
	}
	if got.Derivation.Scale != 3 {
		t.Errorf("scale = %v, want 3", got.Derivation.Scale)
	}
	if got.Filename != "photo-192w.png" {
		t.Errorf("filename = %q, want the target in the name", got.Filename)
	}
	if after := documentText(t, st, sourceID); after != before {
		t.Error("the source picture's bytes changed")
	}

	width, height := pictureSize(t, st, got.ID)
	if width != 192 || height != 144 {
		t.Errorf("the copy is %dx%d, want 192x144", width, height)
	}
}

// TestUpscaleRefusesNeuralWhileSwitchedOff keeps the off state honest on the new path:
// the switch is checked before a binary is looked for, and nothing is filed.
func TestUpscaleRefusesNeuralWhileSwitchedOff(t *testing.T) {
	service, st, chatID := newImageService(t, nil, config.ImageSettings{})
	sourceID := seedPicture(t, st, chatID, 64, 48)
	before, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.Upscale(context.Background(), sourceID, images.UpscaleRequest{
		Target:   images.TargetCustom,
		LongSide: 128,
		Method:   images.MethodESRGAN,
	})
	if !errors.Is(err, images.ErrUpscaleDisabled) {
		t.Fatalf("err = %v, want ErrUpscaleDisabled", err)
	}
	after, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("a refused upscale filed %d attachment(s)", len(after)-len(before))
	}
}

func seedPicture(t *testing.T, st *store.Store, chatID string, width, height int) string {
	t.Helper()
	const id = "uploaded"
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 200, G: 80, B: 40, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDocument(store.Document{
		ID: "doc-1", AttachmentID: id, ChatID: chatID, Title: "photo.png",
		Text: base64.StdEncoding.EncodeToString(buf.Bytes()), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if err := st.CreateAttachment(store.Attachment{
		ID: id, ChatID: chatID, MessageID: "msg-1", Kind: "image",
		Filename: "photo.png", Mime: "image/png", SizeBytes: int64(buf.Len()),
		TokenEstimate: 1024, Placement: store.PlacementInline, DocumentID: "doc-1",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	return id
}

func documentText(t *testing.T, st *store.Store, attachmentID string) string {
	t.Helper()
	a, err := st.GetAttachment(attachmentID)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.GetDocument(a.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	return d.Text
}

func pictureSize(t *testing.T, st *store.Store, attachmentID string) (int, int) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(documentText(t, st, attachmentID))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}
