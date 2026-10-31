package images

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"strings"

	// Decoders for the formats a picture in a chat can arrive in. PNG/JPEG/GIF are
	// registered by images.go already; WebP and BMP are not in the standard library and
	// are the two a WebUI or an upload adds. WebP has no encoder here, so a WebP input
	// is enlarged and written back as PNG — the bytes are what matter, not the container.
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// Methods and targets for a resize, as the panel and the bridge spell them.
const (
	MethodLanczos = "lanczos"
	MethodESRGAN  = "esrgan"

	TargetFHD    = "fhd"
	TargetUHD    = "uhd"
	TargetCustom = "custom"
)

// Long sides the presets name. They are the same numbers the picture card has always
// offered for a hires redraw, so "FHD" means one thing in the whole app.
const (
	FHDLongSide = 1920
	UHDLongSide = 3840
)

// Enlarging is bounded by memory rather than by a model's latent grid. 64 MP is four
// times a 4K frame and about four and a half times a 4K square; an 8K NRGBA is 132 MB
// before the resampler's own intermediate, which is already a large allocation for a
// desktop app and a poor one to make twice by accident.
const (
	MaxUpscaleLongSide = 8192
	MaxUpscalePixels   = int64(64_000_000)
)

// ErrUpscaleDisabled marks a request for the neural method while the switch is off.
// It is its own error because the remedy is a setting, not an installation.
var ErrUpscaleDisabled = errors.New("Real-ESRGAN ist nicht eingeschaltet — in den Einstellungen aktivieren")

// ErrUpscalerUnavailable marks an enabled neural path with no usable program.
var ErrUpscalerUnavailable = errors.New("Real-ESRGAN ist nicht eingerichtet — in den Einstellungen herunterladen oder den Pfad wählen")

// ErrUpscalerNoModels marks a program that is installed but cannot do the work: the weights
// its chosen model needs are missing. It is its own error because the remedy is a different
// download rather than a path — the sibling repository `xinntao/Real-ESRGAN-ncnn-vulkan`
// publishes a 3.6 MB Linux zip under v0.2.0 (3,654,741 B) that holds the program and no
// `models/` at all, while the parent repository's 20220424 archive (46.9 MB) is the one that
// carries `models/realesrgan-x4plus.bin`. Two different repositories both call something
// v0.2.0, which is the trap this message is worded around.
var ErrUpscalerNoModels = errors.New("Real-ESRGAN hat keine Modellgewichte — das Archiv mit models-Ordner verwenden (die kleine 3,6-MB-Ausgabe des ncnn-vulkan-Repos enthält nur das Programm)")

// UpscaleRequest is what the panel asks for. Width and Height are not fields: v1 sizes
// by long side, because an exact box on a different aspect ratio would have to choose
// between cropping and letterboxing, and that is a second decision the panel does not
// ask yet.
type UpscaleRequest struct {
	// Target is fhd, uhd or custom.
	Target string `json:"target"`
	// LongSide is the typed size for TargetCustom, ignored for the presets.
	LongSide int `json:"longSide"`
	// Method is lanczos or esrgan, empty for the app's own default.
	Method string `json:"method"`
	// Model overrides the configured Real-ESRGAN model for one picture.
	Model string `json:"model"`
	// Sharpen is the unsharp amount after a classical resize, 0 to 1, zero for none.
	Sharpen float64 `json:"sharpen"`
}

// UpscaleOptions is the machine's side of the decision: whether the neural path is on,
// where its program lives, and which weights it loads. It is separate from the request
// because none of it is a property of one picture.
type UpscaleOptions struct {
	Enabled bool
	Binary  string
	Model   string
	Tile    int
	// OnPhase is called once, with PhaseUpscaling, when the neural program is about to be
	// started. It is a callback rather than a field on the result because the wait it
	// describes has begun by then and ends after this function returns: a report carried
	// out with the answer would be a progress row for work that is already done.
	//
	// It is called after every refusal — the switch, the program's path — so a request that
	// cannot run never leaves a row behind, and never for the classical pass, which is local
	// arithmetic that a row would flicker over.
	OnPhase func(phase string)
	// Lang is the resolved language the refusals on this path are worded in
	// (config.ModelLanguage). The enlarge panel is the one place a person reads them, and a
	// German sentence under an English panel is the bug the rest of the app already fixed.
	Lang string
}

// UpscaleResult is one enlarged picture and the facts the recipe records.
type UpscaleResult struct {
	Image                     Image
	Width, Height             int
	SourceWidth, SourceHeight int
	Scale                     float64
	Method, Model             string
	DurationMs                int64
}

// Target is where a picture should end up, in pixels.
type Target struct{ Width, Height int }

// Fit is the arithmetic every tier shares, kept separate from the pixels so it can be
// tested without decoding anything.
//
// The long side is the number a preset names, and the short side follows the source's
// aspect ratio. A picture already larger than the target is reduced: the same operation
// with a scale below one, which is what makes "FHD" mean FHD rather than "at least FHD".
func Fit(srcW, srcH, longSide int) Target {
	if srcW <= 0 || srcH <= 0 || longSide <= 0 {
		return Target{}
	}
	if longSide > MaxUpscaleLongSide {
		longSide = MaxUpscaleLongSide
	}
	long, short := srcW, srcH
	if srcH > srcW {
		long, short = srcH, srcW
	}
	shortSide := int(math.Round(float64(short) * float64(longSide) / float64(long)))
	if shortSide < 1 {
		shortSide = 1
	}
	target := Target{Width: longSide, Height: shortSide}
	if srcH > srcW {
		target = Target{Width: shortSide, Height: longSide}
	}
	// The pixel cap is a second bound: an 8K long side on a square would be 67 MP,
	// just past it. Shrinking both sides together keeps the aspect ratio.
	if pixels := int64(target.Width) * int64(target.Height); pixels > MaxUpscalePixels {
		shrink := math.Sqrt(float64(MaxUpscalePixels) / float64(pixels))
		target.Width = max(1, int(math.Floor(float64(target.Width)*shrink)))
		target.Height = max(1, int(math.Floor(float64(target.Height)*shrink)))
	}
	return target
}

// TargetFor resolves a request against the source's own size.
func TargetFor(kind string, longSide, srcW, srcH int, lang string) (Target, error) {
	var long int
	switch strings.TrimSpace(kind) {
	case TargetFHD:
		long = FHDLongSide
	case TargetUHD:
		long = UHDLongSide
	case TargetCustom:
		long = longSide
		if long <= 0 {
			return Target{}, errors.New(text(lang, "die Zielgröße fehlt", "the target size is missing"))
		}
	default:
		return Target{}, fmt.Errorf(text(lang, "unbekanntes Ziel %q", "unknown target %q"), kind)
	}
	target := Fit(srcW, srcH, long)
	if target.Width <= 0 || target.Height <= 0 {
		return Target{}, errors.New(text(lang,
			"die Bildgröße ist unbekannt", "the picture's size is unknown"))
	}
	return target, nil
}

// Upscale enlarges one picture by the requested method.
//
// The classical path runs entirely here; the neural path hands the pixels to a
// portable program and then lands the result on the exact target with the same
// resampler. Both therefore return the size that was asked for, not the size a model
// happens to produce.
func Upscale(ctx context.Context, src []byte, req UpscaleRequest, opts UpscaleOptions) (UpscaleResult, error) {
	img, format, err := decodeForUpscale(src)
	if err != nil {
		return UpscaleResult{}, fmt.Errorf(text(opts.Lang,
			"das Bild konnte nicht gelesen werden: %w",
			"the picture could not be read: %w"), err)
	}
	bounds := img.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	target, err := TargetFor(req.Target, req.LongSide, srcW, srcH, opts.Lang)
	if err != nil {
		return UpscaleResult{}, err
	}

	method := strings.TrimSpace(req.Method)
	if method == "" {
		method = MethodLanczos
	}
	var out *image.RGBA
	model := ""
	switch method {
	case MethodLanczos:
		out = resampleTo(img, target)
		if req.Sharpen > 0 {
			unsharp(out, math.Min(1, req.Sharpen))
		}
	case MethodESRGAN:
		if !opts.Enabled {
			return UpscaleResult{}, ErrUpscaleDisabled
		}
		program, err := FindESRGAN(opts.Binary)
		if err != nil {
			return UpscaleResult{}, err
		}
		if opts.OnPhase != nil {
			opts.OnPhase(PhaseUpscaling)
		}
		up := ESRGAN{Binary: program, Model: strings.TrimSpace(req.Model), Tile: opts.Tile}
		if up.Model == "" {
			up.Model = strings.TrimSpace(opts.Model)
		}
		out, err = up.Enlarge(ctx, img, target, opts.Lang)
		if err != nil {
			return UpscaleResult{}, err
		}
		model = up.ModelName()
	default:
		return UpscaleResult{}, fmt.Errorf("unbekanntes Verfahren %q", method)
	}

	encoded, err := encodeLike(out, format)
	if err != nil {
		return UpscaleResult{}, err
	}
	scale := float64(out.Bounds().Dx()) / float64(srcW)
	return UpscaleResult{
		Image:        encoded,
		Width:        out.Bounds().Dx(),
		Height:       out.Bounds().Dy(),
		SourceWidth:  srcW,
		SourceHeight: srcH,
		Scale:        scale,
		Method:       method,
		Model:        model,
	}, nil
}

// decodeForUpscale reads the bytes and normalises a JPEG's EXIF orientation.
//
// The browser applies EXIF rotation when it paints an image; Go's decoder does not.
// Without this step an uploaded portrait would be enlarged sideways — the one bug in
// this file whose output looks like a different picture rather than a soft one.
func decodeForUpscale(data []byte) (image.Image, string, error) {
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	if format == "jpeg" {
		if orientation := jpegOrientation(data); orientation > 1 {
			img = orient(img, orientation)
		}
	}
	return img, format, nil
}

// encodeLike writes the enlarged picture in the source's own container where possible.
// PNG stays PNG, JPEG stays JPEG; WebP, GIF and BMP become PNG because this build has
// no WebP encoder and a palette or animation would not survive either.
func encodeLike(img *image.RGBA, sourceFormat string) (Image, error) {
	var buf bytes.Buffer
	switch sourceFormat {
	case "jpeg":
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
			return Image{}, err
		}
		return Image{Data: buf.Bytes(), Mime: MimeJPEG, Ext: "jpg"}, nil
	default:
		if err := png.Encode(&buf, img); err != nil {
			return Image{}, err
		}
		return Image{Data: buf.Bytes(), Mime: MimePNG, Ext: "png"}, nil
	}
}
