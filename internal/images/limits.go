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
	return r
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
