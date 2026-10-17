package images

import "math"

// This file is the one place a request's numbers are bounded, and the bounds themselves.
//
// Three callers build a Request — the tool taking a model's arguments, the redraw taking a
// recipe from the UI, and the settings button — and two providers send one. Before this
// file, each caller clamped what it happened to think of and each provider clamped what it
// happened to remember: the WebUI client rounded the size while the CLI client passed it
// through, the step count was bounded on the redraw path but not on the tool's, and a
// negative step count reached the wire as `"steps": -7` because a JSON `omitempty` does not
// omit a negative. Measured in each case before this was written:
//
//	cli   width=20000 height=20000 steps=100000 -> [... "--steps" "100000" "-W" "20000" "-H" "20000"]
//	cli   width=63    height=63    steps=-7      -> [... "-W" "63" "-H" "63"]        (not a multiple of 8)
//	tool  width=20000 steps=100000 reached the provider unchanged, and was recorded in the
//	      recipe as asked for rather than as drawn
//
// The harm is that a local server does not refuse these numbers, it attempts them: 20000
// pixels a side is a latent of tens of gigabytes, and 100000 steps is hours of sampling on
// a machine the user is sitting in front of. The app's own tool schema advertises 64–2048
// and 1–150, so a model that reads it is fine and one that invents a number is not — and
// small local models, which is who this app is for, invent numbers.
//
// The rule is therefore one function rather than a check per caller: every caller that
// builds a request normalises it *before* the values are recorded anywhere, and both
// providers normalise again at the point of sending. The first is what makes a recipe
// describe the picture that was actually drawn; the second is what keeps a future caller
// from reopening the hole. Normalised is idempotent, so the double application costs
// nothing but is not an accident.

// The bounds every backend in this package answers to.
const (
	// MinDimension is the smallest side worth drawing: below it there is no latent grid to
	// speak of, and the servers themselves round up to their own minimum.
	MinDimension = 64
	// MaxDimension is the largest side a request may ask for.
	MaxDimension = 2048
	// DimensionStep is the multiple of 8 every diffusion model's latent grid needs.
	DimensionStep = 8
	// MaxSteps bounds the sampling step count. More is slower, not automatically better.
	MaxSteps = 150
	// MaxHiresScale is the largest enlargement the second pass may ask for.
	//
	// The first pass is already bounded by MaxDimension a side, so 2× is what
	// makes MaxOutputDimension 4096 — and the number is a bound rather than a
	// taste: an enlargement beyond 2× is two doublings, and the detail the second
	// pass paints stops being detail and becomes a picture of the first one's
	// artefacts. MinHiresScale is the smallest enlargement worth a second pass at
	// all: below 1.25 the extra sampling buys a few pixels and costs the whole
	// second pass.
	MaxHiresScale = 2.0
	MinHiresScale = 1.25
	// MinHiresDenoise and MaxHiresDenoise bound how far the second pass may
	// depart from the picture it is enlarging.
	//
	// The range is this app's own, and it is narrow on purpose. 0.2 is where an
	// enlargement is essentially the first picture with its edges re-sampled;
	// 0.6 is where the second pass starts composing rather than refining, and an
	// enlarged picture that no longer contains the first one is the failure mode
	// a user sees as "it drew something else". A1111's own default is 0.7 for
	// `denoising_strength` in hires mode, which is why the ceiling is named
	// rather than adopted.
	MinHiresDenoise = 0.2
	MaxHiresDenoise = 0.6
	// DefaultHiresScale and DefaultHiresDenoise are what an enabled second pass
	// uses when nobody chose a number: 1.5× at 0.4 is an enlargement that adds
	// detail without repainting the composition.
	DefaultHiresScale   = 1.5
	DefaultHiresDenoise = 0.4
	// MaxOutputDimension is the largest side a picture may *end up* at, which is
	// the first pass's bound doubled. It is separate from MaxDimension because
	// MaxDimension is a statement about one sampling pass: a hires draw runs two
	// of them, each inside the bound, and the result is allowed to be larger.
	MaxOutputDimension = MaxDimension * 2
	// MinCfgScale and MaxCfgScale bound the guidance scale.
	//
	// The range is this app's own rather than a vendor's, and it brackets what the three
	// backends actually document: 7.0 is the default A1111 and sd-cli both ship, and 1.0
	// is where a distilled or flow-matching model is run — the repo's own working Flux
	// line is `--cfg-scale 1.0 --steps 4`, and at 1.0 the guidance term is inert rather
	// than wrong, which is why the floor is 1 and not 7. The top is the point past which
	// a picture stops getting better and starts getting *harder*: saturated contrast,
	// burnt highlights, and the same GPU seconds spent on it either way. A bound is what
	// keeps a model that invented 250 from being believed.
	MinCfgScale = 1.0
	MaxCfgScale = 30.0
)

// MaxPicturesPerDraw bounds how many pictures one draw may file into a chat.
//
// A request asks for exactly one (`n: 1` in the payload, see sdRequest.N), but a build that
// ignores that key, a proxy in front of one, or a WebUI script can answer with a batch, and
// every picture in one answer becomes an attachment with a base64 document behind it. Four is
// a batch a person might have configured on purpose; past that the answer is not a draw
// anybody asked for, and filing it would fill a chat and the database with pictures nobody
// chose. What is not kept is reported rather than dropped in silence, so the model can say so
// too.
const MaxPicturesPerDraw = 4

// Kept returns the pictures to file from one answer, at most MaxPicturesPerDraw of them.
// Both callers that store a result loop over this rather than over Result.Images, which is
// what keeps the bound from being a rule one of them remembers.
func (r Result) Kept() []Image {
	if len(r.Images) <= MaxPicturesPerDraw {
		return r.Images
	}
	return r.Images[:MaxPicturesPerDraw]
}

// Hires is the second sampling pass: the first draws the picture at the requested size, and
// the second enlarges that picture and repaints it at a low denoise so the extra pixels carry
// generated detail rather than interpolated blur.
//
// It is a separate type from the request's own numbers because it is a separate decision with
// its own bounds, and because both providers spell it differently — the WebUI in one payload
// and the local program on its command line — while the arithmetic that turns a base size
// into an output size has to be the same one or a recipe would describe a picture nobody
// drew. Enabled false is the whole of "one pass": the other two fields are then unused, and
// a zero value here is an ordinary draw.
type Hires struct {
	Enabled bool
	// Scale is how much larger the output is than the first pass, per side.
	Scale float64
	// Denoise is how far the second pass may depart from the picture it enlarges.
	Denoise float64
}

// Normalised returns the second pass with its numbers inside their bounds, reading zero the
// way every other generation field here does: as "nobody chose one", which gets the default
// rather than the minimum.
//
// A disabled pass normalises to the zero value, so "off" has one spelling. That matters
// downstream: the recipe stores this type only when it ran, and a disabled-but-configured
// pass would otherwise be indistinguishable from an enabled one to anything that asked
// `Scale != 0`.
func (h Hires) Normalised() Hires {
	if !h.Enabled {
		return Hires{}
	}
	scale := h.Scale
	switch {
	case math.IsNaN(scale) || scale <= 0:
		scale = DefaultHiresScale
	case scale < MinHiresScale:
		scale = MinHiresScale
	case scale > MaxHiresScale:
		scale = MaxHiresScale
	}
	denoise := h.Denoise
	switch {
	case math.IsNaN(denoise) || denoise <= 0:
		denoise = DefaultHiresDenoise
	case denoise < MinHiresDenoise:
		denoise = MinHiresDenoise
	case denoise > MaxHiresDenoise:
		denoise = MaxHiresDenoise
	}
	return Hires{Enabled: true, Scale: scale, Denoise: denoise}
}

// OutputSize is the size the picture will *end up* at, which is what the recipe records and
// what the panel's result line reads.
//
// It answers the normalised requested size when the pass is off, and that size again when
// either side is unknown: an empty size is the server's own, and a server's own size is not
// something this app can multiply. The aspect ratio is preserved by scaling both sides by the
// same factor,
// and the result is aligned *up* to the latent grid — see roundToStep below. The backends are
// asked for this exact size (`hr_resize_x`/`hr_resize_y`, `--hires-width`/`--hires-height`)
// whenever their own options allow it, so the number the recipe records and the one
// verifyHiresOutput compares against are the same number the server was told to produce.
func (r Request) OutputSize() (int, int) {
	r = r.Normalised()
	if !r.Hires.Enabled || r.Width <= 0 || r.Height <= 0 {
		return r.Width, r.Height
	}
	width := roundToStep(int(math.Round(float64(r.Width) * r.Hires.Scale)))
	height := roundToStep(int(math.Round(float64(r.Height) * r.Hires.Scale)))
	// A normalised base and scale cannot reach past the cap: 2048 a side at 2× is exactly
	// MaxOutputDimension. This block is the backstop for the day one of those two bounds is
	// raised without the other, rather than a path an ordinary request takes, and it keeps
	// the aspect ratio by shrinking both sides together.
	if width > MaxOutputDimension || height > MaxOutputDimension {
		shrink := math.Min(
			float64(MaxOutputDimension)/float64(width),
			float64(MaxOutputDimension)/float64(height))
		width = roundToStep(int(float64(width) * shrink))
		height = roundToStep(int(float64(height) * shrink))
	}
	if width > MaxOutputDimension {
		width = MaxOutputDimension
	}
	if height > MaxOutputDimension {
		height = MaxOutputDimension
	}
	return width, height
}

// Normalised returns the request with its numbers inside the bounds every backend here
// agrees on.
//
// Zero and negative both stay zero, because zero is this package's spelling of "the
// server's own": an emptied box in the recipe editor, a size the user never chose, a `-1`
// from the WebUI's own convention for "random". Raising such a value to the minimum would
// turn "you decide" into a number nobody asked for, and passing a negative through would
// hand the server a request it has to interpret.
func (r Request) Normalised() Request {
	r.Width = normaliseDimension(r.Width)
	r.Height = normaliseDimension(r.Height)
	r.Steps = normaliseSteps(r.Steps)
	r.CfgScale = normaliseCfgScale(r.CfgScale)
	r.Hires = r.Hires.Normalised()
	return r
}

// roundToStep rounds a size *up* to the next multiple of DimensionStep the latent grid
// needs, even when it is above MaxDimension: this is the output-size rounding, where the
// bound is MaxOutputDimension rather than the per-pass one.
//
// Up rather than to the nearest multiple, because the target has to be on the latent grid and
// because that is the direction a backend moves a scale-derived size when its exact target is
// unavailable. A1111's VAE encoder pads the edge before the stride-2 convolutions, and
// stable-diffusion.cpp computes round(base × scale) and then `align_image_size` moves it up to
// its spatial multiple. A recipe that predicted 648 for a 650-pixel request would disagree
// with the picture the server returns. The prediction is still only the app's 8-grid: sd.cpp
// rounds a target up again to a coarser multiple (64 for a UNet), which is why verification
// treats this number as a floor and not as the exact size.
func roundToStep(value int) int {
	return (value + DimensionStep - 1) / DimensionStep * DimensionStep
}

// normaliseDimension rounds a requested size to the multiple of 8 every diffusion model
// needs and keeps it inside the range a local machine can finish. The bound is applied
// before the rounding, so a size just over the maximum lands on the maximum rather than
// beyond it.
func normaliseDimension(value int) int {
	if value <= 0 {
		return 0 // omitted: the server's own default applies.
	}
	if value < MinDimension {
		value = MinDimension
	}
	if value > MaxDimension {
		value = MaxDimension
	}
	return (value + DimensionStep/2) / DimensionStep * DimensionStep
}

// normaliseSteps bounds the sampling step count, with the same reading of a missing value.
// A negative count is not clamped up to 1: it means the same thing as an empty box.
func normaliseSteps(value int) int {
	if value <= 0 {
		return 0
	}
	if value > MaxSteps {
		return MaxSteps
	}
	return value
}

// normaliseCfgScale bounds the guidance scale, reading a missing value the same way the two
// above do — zero is "the server's own" and stays zero rather than being raised to the
// minimum, which would turn an empty field into a number nobody chose.
//
// Not-a-number is read as "nobody made a decision" as well, and that is not paranoia:
// `strconv.ParseFloat` accepts "nan" and "inf", so a model that sends `"cfg_scale": "nan"`
// — small local models do emit numbers as strings, which is why the tool decodes loosely —
// would otherwise put a NaN into a JSON payload, and `encoding/json` refuses to *marshal*
// one at all. The whole request would fail on a field the user never filled in. Infinity
// needs no special case: it is above the maximum, so it lands on it.
func normaliseCfgScale(value float64) float64 {
	if math.IsNaN(value) {
		return 0
	}
	if value <= 0 {
		return 0
	}
	if value < MinCfgScale {
		return MinCfgScale
	}
	if value > MaxCfgScale {
		return MaxCfgScale
	}
	return value
}
