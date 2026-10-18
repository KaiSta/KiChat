package images

import (
	"path/filepath"
	"strings"
)

// Pipeline ids. Empty means "infer from the model file", which is the default and the only
// value an installation that predates this field carries.
const (
	PipelineAuto           = ""
	PipelineGeneric        = "generic"
	PipelineFlux3Klein     = "flux3-klein"
	PipelineFlux3KleinBase = "flux3-klein-base"
)

// Text encoder arrangements a pipeline names: EncoderLLM is a language model passed as
// `--llm` (FLUX 3, Qwen-Image). EncoderAny means the profile says nothing, which is what the
// generic one does — an arrangement this app does not model is named by its own flags in
// Settings rather than by a value here.
const (
	EncoderAny = ""
	EncoderLLM = "llm"
)

// Captioning modes, stored in ImageSettings.Captioning.
//
// CaptioningPipeline is the empty value: the profile decides, so a checkpoint that was
// trained on captions gets one without the user finding a switch. CaptioningOff is the
// explicit no — which is a different statement from "nobody chose", and the only one that
// can override a profile.
const (
	CaptioningPipeline = ""
	CaptioningOff      = "off"
	CaptioningAuto     = "auto"
	CaptioningModel    = "model"
)

// Where a resolved profile came from. The source is reported because the difference is
// exactly what a user needs when a profile misfires: a name that no longer contains
// `klein` still resolves from the weights, and a file whose header cannot be read still
// resolves from its name — and those two are not the same claim.
const (
	// SourceChosen is a value the user picked in Settings. Nothing identified the model
	// is the empty string, which is the generic profile's own zero value.
	SourceChosen = "chosen"
	// SourceWeights is the tensor layout inside the diffusion file itself.
	SourceWeights = "weights"
	// SourceName is the file name, used only when the weights cannot be read.
	SourceName = "name"
)

// Pipeline is what a checkpoint says about how it wants to be drawn: the numbers it was
// trained at, the parts it needs, and the knobs it does not have.
//
// It exists so a FLUX 3 draw is right the moment the diffusion file is chosen, instead of
// asking a model to know that a distilled 16-step checkpoint wants CFG 1.0 and no negative
// prompt. The facts here are the ones stable-diffusion.cpp documents for these files (its
// docs/flux3.md), not this app's taste.
type Pipeline struct {
	ID   string
	Name string
	// Source is where this value's profile came from — see the constants above. It is
	// not configuration and is never stored.
	Source string
	// Encoder is the text-encoding arrangement; RequiresVAE says the pipeline has no
	// single checkpoint at all. Both are read by Health, which can then name the missing
	// part instead of failing at the first draw.
	Encoder     string
	RequiresVAE bool
	// Steps, CFG and Sampler are the values a request is filled with when nobody chose
	// one. Zero means the pipeline has no opinion and the build's own default applies.
	Steps   int
	CFG     float64
	Sampler string
	// SupportsNegative is false where the model is guidance-free (distilled FLUX 3 at
	// CFG 1.0): a negative prompt there is sent, recorded and ignored, which looks like a
	// broken setting rather than one the model does not have.
	SupportsNegative bool
	// SupportsHires is false where a second pass has never been measured on that
	// checkpoint. It is not a statement that the pass is impossible — the distilled FLUX 3
	// profile was written that way and measurement turned it true (IMAGES.md §9.5) — but
	// drawing one and recording it as an enlargement would claim a result nobody checked.
	SupportsHires bool
	// Captioning is the default for ImageSettings.Captioning when that field is empty.
	// It is the mode, not a boolean: the profile says a caption is needed, the user says
	// who writes it.
	Captioning string
	// Tensors identify the pipeline inside the weights file: every prefix must name at
	// least one tensor, and NotTensors names the prefixes that must not appear. This is
	// the authority when the header can be read, because it is the only thing a rename and
	// a re-quantisation leave alone (IMAGES.md §9.5 has the measurements).
	Tensors    []string
	NotTensors []string
	// Markers identify the pipeline in a file name, all of which must appear. They are the
	// fallback for a file whose header cannot be read at all — weights on another machine,
	// a truncated download — not a second opinion when the header was read.
	Markers []string
}

// genericPipeline is today's behaviour: no numbers, no restrictions, no caption. Every
// field left at zero is what an installation predating this feature had.
var genericPipeline = Pipeline{
	ID:               PipelineGeneric,
	Name:             "Generic",
	SupportsNegative: true,
	SupportsHires:    true,
}

// flux3Pipelines is ordered: the first match wins, so `-base` is tested before its own name
// would otherwise match the distilled entry.
var flux3Pipelines = []Pipeline{
	{
		ID:               PipelineFlux3KleinBase,
		Name:             "FLUX 3 Image Klein Base",
		Encoder:          EncoderLLM,
		RequiresVAE:      true,
		Steps:            20,
		CFG:              3.5,
		Sampler:          "euler",
		SupportsNegative: true,
		// A second pass on a 20-step base model at CFG 3.5 has not been measured — no Base
		// checkpoint exists on the machine the distilled one was measured on — so it stays
		// refused rather than assumed to behave like a different checkpoint's.
		SupportsHires: false,
		Captioning:    CaptioningAuto,
		// The architecture's own tensor names, and the one tensor the distilled checkpoint
		// adds; see IMAGES.md §9.5 for the files this was measured on.
		Tensors:    []string{"content_mode_blocks.", "txt_mode_blocks."},
		NotTensors: []string{"ref_time_in."},
		Markers:    []string{"klein", "base"},
	},
	{
		ID:          PipelineFlux3Klein,
		Name:        "FLUX 3 Image Klein",
		Encoder:     EncoderLLM,
		RequiresVAE: true,
		// Distilled: 16 steps at CFG 1.0, from the project's own guide.
		Steps:   16,
		CFG:     1.0,
		Sampler: "euler",
		// Guidance-free at 1.0, and the README says not to use it without a caption —
		// which is the whole reason this profile exists.
		SupportsNegative: false,
		// Measured on the real checkpoint on 2026-10-18: a 1.5× second pass at denoise 0.4
		// draws a clean 768² from a 512² base in 18.4 s, against 7.3 s for the base draw
		// alone and 13.3 s for sampling at 768² directly (IMAGES.md §9.5). The pass was
		// unmeasured when this profile was written, which is the only reason it used to be
		// refused — and refusing a second pass is a silent loss for a user who asked for one.
		SupportsHires: true,
		Captioning:    CaptioningAuto,
		Tensors:       []string{"content_mode_blocks.", "txt_mode_blocks.", "ref_time_in."},
		Markers:       []string{"klein"},
	},
}

// PipelineFor resolves the pipeline for a configuration: an explicit id wins, then the
// tensor layout in the diffusion model's own header, then its file name, and anything
// unrecognised is the generic pipeline rather than an error — a model nobody has a profile
// for still draws.
//
// The header comes first because a file name is a label somebody typed: it survives
// download renames, folder names and quantiser suffixes by luck, not by contract, and a GGUF
// that lost the `klein` in its name used to draw at the build's defaults with no caption.
// Reading the header costs a few kilobytes, and it is the same file the draw will load.
//
// A name is used only when the header cannot be read — a path that does not exist here, a
// truncated download, a format this package does not parse. That is a weaker claim, and
// Source says which one was made, so a wrong one is visible in the panel instead of
// mysterious.
func PipelineFor(id, diffusionModel string) Pipeline {
	switch strings.TrimSpace(id) {
	case PipelineGeneric:
		return withSource(genericPipeline, SourceChosen)
	case PipelineFlux3Klein:
		return withSource(flux3Pipelines[1], SourceChosen)
	case PipelineFlux3KleinBase:
		return withSource(flux3Pipelines[0], SourceChosen)
	case PipelineAuto:
	default:
		// A hand-edited value, folded the way the settings fold it: the generic
		// pipeline is what this app did before profiles existed.
		return genericPipeline
	}

	if p, ok := pipelineFromWeights(diffusionModel); ok {
		return p
	}
	if p, ok := pipelineFromName(diffusionModel); ok {
		return p
	}
	return genericPipeline
}

// pipelineFromWeights identifies the model from the tensor table in its own header. The
// second return value is false only when the header could not be read at all: a header that
// was read and matched nothing is a definite answer of "no profile", which is what keeps a
// FLUX 1 checkpoint renamed to `klein` from being drawn as one.
func pipelineFromWeights(path string) (Pipeline, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Pipeline{}, false
	}
	header, err := inspectModel(path)
	if err != nil || len(header.Tensors) == 0 {
		return Pipeline{}, false
	}
	for _, p := range flux3Pipelines {
		if p.matchesTensors(header.Tensors) {
			return withSource(p, SourceWeights), true
		}
	}
	return withSource(genericPipeline, SourceWeights), true
}

// pipelineFromName is the fallback: the old, deliberately narrow match on the name BFL
// publishes, and nothing else.
func pipelineFromName(path string) (Pipeline, bool) {
	name := strings.ToLower(filepath.Base(strings.TrimSpace(path)))
	if name == "" || name == "." {
		return Pipeline{}, false
	}
	for _, p := range flux3Pipelines {
		if p.matches(name) {
			return withSource(p, SourceName), true
		}
	}
	return Pipeline{}, false
}

// matchesTensors reports whether a tensor table identifies this pipeline: every required
// prefix has to appear and none of the excluded ones may.
func (p Pipeline) matchesTensors(names []string) bool {
	if len(p.Tensors) == 0 {
		return false
	}
	for _, prefix := range p.Tensors {
		if !hasTensorPrefix(names, prefix) {
			return false
		}
	}
	for _, prefix := range p.NotTensors {
		if hasTensorPrefix(names, prefix) {
			return false
		}
	}
	return true
}

func hasTensorPrefix(names []string, prefix string) bool {
	for _, name := range names {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// withSource stamps where a profile came from without touching the table itself.
func withSource(p Pipeline, source string) Pipeline {
	p.Source = source
	return p
}

// matches reports whether every marker appears in a lowercased file name.
func (p Pipeline) matches(name string) bool {
	if len(p.Markers) == 0 {
		return false
	}
	for _, marker := range p.Markers {
		if !strings.Contains(name, marker) {
			return false
		}
	}
	return true
}

// CaptioningMode is the caption setting in force: the profile's default when nobody chose,
// the configured value otherwise. An explicit "off" is how a user overrides a profile that
// wants a caption.
func (p Pipeline) CaptioningMode(configured string) string {
	if configured != CaptioningPipeline {
		return configured
	}
	return p.Captioning
}

// Adjustments reports what a profile removed from a request, so a caller can say it out
// loud rather than leaving a setting that appears to work.
type Adjustments struct {
	DroppedNegative bool
	DroppedHires    bool
}

// Adjusted fills a request's unset values from the profile and removes what the pipeline
// cannot use.
//
// Only zeros are filled, which is the same rule Settings follows for every other field: a
// number somebody chose — the model per picture, or the user once — is an instruction, and a
// profile that overwrote it would be a default pretending to be a decision.
//
// What it removes is removed rather than refused, because the switch that asks for it is
// global and a user who had a second pass on for another model would otherwise be unable to
// draw at all. The negative prompt and the second pass are reported back so the tool result
// and the picture's card can say they did not happen.
func (p Pipeline) Adjusted(req Request) (Request, Adjustments) {
	if req.Steps <= 0 && p.Steps > 0 {
		req.Steps = p.Steps
	}
	if req.CfgScale <= 0 && p.CFG > 0 {
		req.CfgScale = p.CFG
	}
	if strings.TrimSpace(req.Sampler) == "" && p.Sampler != "" {
		req.Sampler = p.Sampler
	}
	var adj Adjustments
	if !p.SupportsNegative && strings.TrimSpace(req.NegativePrompt) != "" {
		req.NegativePrompt = ""
		adj.DroppedNegative = true
	}
	if !p.SupportsHires && req.Hires.Enabled {
		req.Hires = Hires{}
		adj.DroppedHires = true
	}
	return req, adj
}
