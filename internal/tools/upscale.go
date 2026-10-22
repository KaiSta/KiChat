package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"kichat/internal/images"
	"kichat/internal/ingest"
	"kichat/internal/store"
	"kichat/internal/uimsg"
)

// Upscale enlarges one stored picture and files the copy beside it.
//
// This is not a generation and does not go through a provider: the classical method is
// bytes in, bytes out, and the neural one is a separate program. That is what lets it
// work on an uploaded photograph, which has no recipe and no prompt, and on a machine
// with no image server running at all.
//
// The source is never modified. An enlargement is a new attachment on the same message,
// so the two can be compared and the original stays exactly as it was.
func (s *Service) Upscale(ctx context.Context, attachmentID string, req images.UpscaleRequest) (store.Attachment, error) {
	if s.Store == nil {
		return store.Attachment{}, uimsg.Detailf("tools.noStore", "no storage is available, so the enlargement cannot be filed")
	}
	source, err := s.Store.GetAttachment(attachmentID)
	if err != nil {
		return store.Attachment{}, err
	}
	if source.Kind != string(ingest.KindImage) && !strings.HasPrefix(strings.ToLower(source.Mime), "image/") {
		return store.Attachment{}, uimsg.Detailf("tools.notAnImage",
			fmt.Sprintf("%s is not an image", source.Filename), source.Filename)
	}
	if source.DocumentID == "" {
		return store.Attachment{}, uimsg.Detailf("tools.emptyImage",
			fmt.Sprintf("the image of %s is empty", source.Filename), source.Filename)
	}
	document, err := s.Store.GetDocument(source.DocumentID)
	if err != nil {
		return store.Attachment{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(document.Text))
	if err != nil {
		return store.Attachment{}, uimsg.Detailf("tools.imageUnreadable",
			fmt.Sprintf("the image of %s could not be read: %v", source.Filename, err), source.Filename)
	}

	cfg := s.imageSettings()
	options := images.UpscaleOptions{
		Enabled: cfg.UpscalerEnabled,
		Binary:  cfg.ESRGANBinary,
		Model:   cfg.ESRGANModel,
		Tile:    cfg.ESRGANTile,
	}
	if s.Progress != nil {
		// Reported under the *source* attachment, which is what the card the user pressed
		// knows itself by: the same id a redraw registers under, so Stop finds this one
		// where it looks. The chat comes from the row rather than from the caller, because
		// only here is it known.
		options.OnPhase = func(phase string) {
			s.Progress(source.ID, source.ChatID, images.Progress{Phase: phase})
		}
	}
	started := time.Now()
	result, err := images.Upscale(ctx, raw, req, options)
	if err != nil {
		return store.Attachment{}, err
	}
	result.DurationMs = time.Since(started).Milliseconds()

	derivation := &store.Derivation{
		SourceAttachmentID: source.ID,
		SourceWidth:        result.SourceWidth,
		SourceHeight:       result.SourceHeight,
		Method:             result.Method,
		Model:              result.Model,
		Scale:              result.Scale,
		Sharpen:            req.Sharpen,
		DurationMs:         result.DurationMs,
	}
	used, err := s.usedFilenames(source.ChatID)
	if err != nil {
		return store.Attachment{}, err
	}
	name := uniqueName(used, upscaleFilename(source.Filename, result.Width, result.Image.Ext))
	return s.storeGeneratedImage(source.ChatID, source.MessageID, name, result.Image, nil, derivation)
}

// upscaleFilename marks the copy with the long side it was enlarged to, so two
// enlargements of one picture are distinguishable in a message and in a save dialog.
func upscaleFilename(name string, longSide int, ext string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	base = strings.TrimSpace(base)
	if base == "" {
		base = "bild"
	}
	return fmt.Sprintf("%s-%dw.%s", base, longSide, ext)
}

// UpscalerStatus is what the Settings group and the Enlarge panel need to know before
// offering the neural method.
type UpscalerStatus struct {
	Enabled   bool     `json:"enabled"`
	Available bool     `json:"available"`
	Path      string   `json:"path"`
	Models    []string `json:"models"`
	Error     string   `json:"error,omitempty"`
}

// UpscalerStatus reports whether the neural path can run. It does not start the program:
// a path check is cheap and a real probe costs a model load, which is why TestUpscaler
// exists separately and is a button rather than something a settings page does on open.
func (s *Service) UpscalerStatus() UpscalerStatus {
	cfg := s.imageSettings()
	status := UpscalerStatus{
		Enabled: cfg.UpscalerEnabled,
		Models:  images.ESRGANModels,
	}
	program, err := images.FindESRGAN(cfg.ESRGANBinary)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Path = program
	status.Available = true
	return status
}

// TestUpscaler runs one tiny picture through the program, which is the only check that
// covers the Vulkan driver as well as the file.
func (s *Service) TestUpscaler(ctx context.Context) (string, error) {
	if !s.imageSettings().UpscalerEnabled {
		return "", images.ErrUpscaleDisabled
	}
	return images.ProbeESRGAN(ctx, s.imageSettings().ESRGANBinary)
}
