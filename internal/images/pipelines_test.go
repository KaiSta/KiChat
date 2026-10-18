package images

import (
	"path/filepath"
	"testing"
)

// TestPipelineIsInferredFromTheDiffusionFileName pins the fallback: a path whose weights
// cannot be read — one that does not exist here, a truncated download — is still recognised
// by the marker BFL publishes. It is deliberately narrow, so a wrong guess costs a caption
// rather than a set of numbers that do not fit the model.
func TestPipelineIsInferredFromTheDiffusionFileName(t *testing.T) {
	cases := []struct {
		name string
		file string
		want string
	}{
		{"the published name", "/models/diffusion_models/flux-3-image-klein.safetensors", PipelineFlux3Klein},
		{"a quantised copy", "/models/flux-3-image-klein-Q4_K_M.gguf", PipelineFlux3Klein},
		{"base before distilled", "/models/flux-3-image-klein-base.safetensors", PipelineFlux3KleinBase},
		{"base in a quantised copy", "/m/flux-3-image-klein-base-Q8_0.gguf", PipelineFlux3KleinBase},
		{"another family", "/models/qwen-image-2.1-Q5_K_M.gguf", PipelineGeneric},
		{"an older flux", "/models/flux1-dev-Q8_0.gguf", PipelineGeneric},
		{"nothing configured", "", PipelineGeneric},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PipelineFor(PipelineAuto, tc.file).ID; got != tc.want {
				t.Errorf("PipelineFor(auto, %q) = %q, want %q", tc.file, got, tc.want)
			}
		})
	}
}

// TestThePipelineComesFromTheTensorsNotTheFileName is the point of reading the header: a
// file whose name says nothing, or says the wrong thing, is still the model it is.
func TestThePipelineComesFromTheTensorsNotTheFileName(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		file    string
		tensors []string
		want    string
		source  string
	}{
		{"a renamed distilled checkpoint", "weights.gguf", flux3KleinTensors, PipelineFlux3Klein, SourceWeights},
		{"a distilled checkpoint named base", "flux-3-image-klein-base-Q4_K_M.gguf", flux3KleinTensors, PipelineFlux3Klein, SourceWeights},
		{"a base checkpoint named distilled", "flux-3-image-klein-Q8_0.gguf", flux3BaseTensors, PipelineFlux3KleinBase, SourceWeights},
		{"a base checkpoint named nothing", "f3-image-small.gguf", flux3BaseTensors, PipelineFlux3KleinBase, SourceWeights},
		// The header outranks the name in both directions: a FLUX 1 file renamed to look
		// like Klein must not acquire Klein's 16 steps or its caption.
		{"another family named klein", "flux-3-image-klein.gguf", flux1Tensors, PipelineGeneric, SourceWeights},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			writeGGUF(t, path, map[string]string{"general.architecture": "flux"}, tc.tensors)
			got := PipelineFor(PipelineAuto, path)
			if got.ID != tc.want {
				t.Errorf("%s resolved to %q, want %q", filepath.Base(path), got.ID, tc.want)
			}
			if got.Source != tc.source {
				t.Errorf("%s resolved from %q, want %q", filepath.Base(path), got.Source, tc.source)
			}
		})
	}

	// The same answer from the other format, with a name that carries no marker at all.
	safetensorsPath := filepath.Join(dir, "model.safetensors")
	writeSafetensors(t, safetensorsPath, map[string]string{"model": "f3-image-small"}, flux3KleinTensors)
	if got := PipelineFor(PipelineAuto, safetensorsPath); got.ID != PipelineFlux3Klein || got.Source != SourceWeights {
		t.Errorf("a renamed safetensors checkpoint resolved to %q from %q", got.ID, got.Source)
	}
}

// TestARenamedKleinDrawsTheSameWay pins what the identification is for: the numbers and the
// caption, not a label. A distilled checkpoint called `weights.gguf` draws at 16 steps and
// CFG 1.0 and gets a caption, which is what the file name used to have to say.
func TestARenamedKleinDrawsTheSameWay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weights.gguf")
	writeGGUF(t, path, nil, flux3KleinTensors)

	pipeline := PipelineFor(PipelineAuto, path)
	req, adjusted := pipeline.Adjusted(Request{})
	if req.Steps != 16 || req.CfgScale != 1.0 || req.Sampler != "euler" {
		t.Errorf("a renamed Klein drew %d steps at CFG %v with %q", req.Steps, req.CfgScale, req.Sampler)
	}
	if adjusted.DroppedHires || adjusted.DroppedNegative {
		t.Errorf("an empty request had something dropped: %+v", adjusted)
	}
	if got := pipeline.CaptioningMode(CaptioningPipeline); got != CaptioningAuto {
		t.Errorf("captioning = %q, want a caption from the profile", got)
	}
}

// TestTheFileNameIsOnlyTheFallback: when the weights cannot be read at all, the name is the
// only evidence left — and the resolution says so, so the panel can print which claim it is.
func TestTheFileNameIsOnlyTheFallback(t *testing.T) {
	dir := t.TempDir()
	absent := filepath.Join(dir, "flux-3-image-klein.safetensors")
	if got := PipelineFor(PipelineAuto, absent); got.ID != PipelineFlux3Klein || got.Source != SourceName {
		t.Errorf("a missing file resolved to %q from %q", got.ID, got.Source)
	}

	garbage := filepath.Join(dir, "flux-3-image-klein-base.gguf")
	writeTestFile(t, garbage, []byte("not a model at all"))
	if got := PipelineFor(PipelineAuto, garbage); got.ID != PipelineFlux3KleinBase || got.Source != SourceName {
		t.Errorf("an unreadable file resolved to %q from %q", got.ID, got.Source)
	}

	truncated := filepath.Join(dir, "flux-3-image-klein.gguf")
	writeTestFile(t, truncated, []byte("GGUF\x03\x00\x00\x00\x09"))
	if got := PipelineFor(PipelineAuto, truncated); got.ID != PipelineFlux3Klein || got.Source != SourceName {
		t.Errorf("a truncated header resolved to %q from %q", got.ID, got.Source)
	}
}

// TestAnExplicitPipelineBeatsTheFileName is the override for a renamed file: a user who
// says which model this is must not be overruled by a string in the path — or by the
// header, which is why the explicit branch returns before anything is read.
func TestAnExplicitPipelineBeatsTheFileName(t *testing.T) {
	kleinPath := filepath.Join(t.TempDir(), "weights.gguf")
	writeGGUF(t, kleinPath, nil, flux3KleinTensors)
	if got := PipelineFor(PipelineGeneric, kleinPath).ID; got != PipelineGeneric {
		t.Errorf("an explicit generic profile was overruled by the header: %q", got)
	}
	if got := PipelineFor(PipelineFlux3KleinBase, kleinPath); got.ID != PipelineFlux3KleinBase || got.Source != SourceChosen {
		t.Errorf("an explicit base profile resolved to %q from %q", got.ID, got.Source)
	}
	if got := PipelineFor("nonsense", kleinPath).ID; got != PipelineGeneric {
		t.Errorf("an unknown profile resolved to %q, want the generic one", got)
	}
}

// TestTheKleinProfileIsTheNumberItsOwnGuidePrints is the point of the table: the numbers a
// user would otherwise have to know, taken from stable-diffusion.cpp's docs/flux3.md rather
// than chosen here.
func TestTheKleinProfileIsTheNumberItsOwnGuidePrints(t *testing.T) {
	klein := PipelineFor(PipelineFlux3Klein, "")
	if klein.Steps != 16 || klein.CFG != 1.0 || klein.Sampler != "euler" {
		t.Errorf("klein = %d steps, CFG %v, sampler %q", klein.Steps, klein.CFG, klein.Sampler)
	}
	if klein.SupportsNegative {
		t.Error("the distilled model is guidance-free; a negative prompt there is a setting that does nothing")
	}
	// The second pass was refused while it was unmeasured and is allowed now that it has
	// been drawn: 512² → 768² at denoise 0.4 in 18.4 s, against 7.3 s for the base draw
	// (IMAGES.md §9.5). Refusing it is a silent loss for a user who asked for one.
	if !klein.SupportsHires {
		t.Error("the distilled profile now has a measured second pass and must let it through")
	}
	if klein.Captioning != CaptioningAuto {
		t.Error("klein was trained on captions; its profile has to ask for one")
	}
	if klein.Encoder != EncoderLLM || !klein.RequiresVAE {
		t.Error("klein needs --llm, --vae and its diffusion model; Health names whichever is missing")
	}

	base := PipelineFor(PipelineFlux3KleinBase, "")
	if base.Steps != 20 || base.CFG != 3.5 {
		t.Errorf("base = %d steps at CFG %v, want the guide's 20 / 3.5", base.Steps, base.CFG)
	}
	if !base.SupportsNegative {
		t.Error("the base model is not guidance-free, so a negative prompt is meaningful there")
	}
	// Base keeps the refusal: no Base checkpoint was measured, so the honest answer is
	// still "nobody checked" rather than "like the other one".
	if base.SupportsHires {
		t.Error("a second pass on the base checkpoint has not been measured and must not be claimed")
	}
}

// TestAdjustedFillsOnlyWhatNobodyChose is the same rule Settings follows: a number somebody
// chose is an instruction, and a profile that overwrote it would be a default pretending to
// be a decision.
func TestAdjustedFillsOnlyWhatNobodyChose(t *testing.T) {
	klein := PipelineFor(PipelineFlux3Klein, "")
	req, adj := klein.Adjusted(Request{
		Steps:          40,
		Sampler:        "dpm++2m",
		NegativePrompt: "blurry, extra fingers",
		Hires:          Hires{Enabled: true, Scale: 1.5, Denoise: 0.4},
	})

	if req.Steps != 40 {
		t.Errorf("steps = %d, want the chosen 40", req.Steps)
	}
	if req.Sampler != "dpm++2m" {
		t.Errorf("sampler = %q, want the chosen one", req.Sampler)
	}
	if req.CfgScale != 1.0 {
		t.Errorf("cfg = %v, want the profile's 1.0", req.CfgScale)
	}
	if req.NegativePrompt != "" || !adj.DroppedNegative {
		t.Errorf("negative = %q (dropped = %v), want it removed and reported", req.NegativePrompt, adj.DroppedNegative)
	}
	// The second pass is the user's, and this profile has measured one: it is passed through
	// with the scale and denoise that were asked for.
	if !req.Hires.Enabled || adj.DroppedHires {
		t.Errorf("hires = %+v (dropped = %v), want the measured second pass kept", req.Hires, adj.DroppedHires)
	}

	// The base profile is where a second pass is still removed, and it says so.
	base := PipelineFor(PipelineFlux3KleinBase, "")
	baseReq, baseAdj := base.Adjusted(Request{Hires: Hires{Enabled: true, Scale: 1.5, Denoise: 0.4}, NegativePrompt: "blurry"})
	if baseReq.Hires.Enabled || !baseAdj.DroppedHires {
		t.Errorf("base hires = %+v (dropped = %v), want it removed and reported", baseReq.Hires, baseAdj.DroppedHires)
	}
	if baseReq.NegativePrompt != "blurry" || baseAdj.DroppedNegative {
		t.Errorf("base negative = %q (dropped = %v), want it kept: the base model is not guidance-free",
			baseReq.NegativePrompt, baseAdj.DroppedNegative)
	}
}

// TestTheGenericProfileChangesNothing is the guarantee for every setup that predates
// profiles: no numbers are added and nothing is taken away.
func TestTheGenericProfileChangesNothing(t *testing.T) {
	req := Request{
		Steps:          28,
		Sampler:        "euler_a",
		CfgScale:       7,
		NegativePrompt: "blurry",
		Hires:          Hires{Enabled: true, Scale: 1.5, Denoise: 0.4},
	}
	got, adj := PipelineFor(PipelineGeneric, "").Adjusted(req)
	if got != req {
		t.Errorf("generic profile changed the request: %+v → %+v", req, got)
	}
	if adj.DroppedNegative || adj.DroppedHires {
		t.Errorf("generic profile dropped something: %+v", adj)
	}
}

// TestCaptioningModeFollowsTheProfileUnlessChosen: empty means the profile decides, "off" is
// the only value that overrides it, and the two caption writers stay alternatives.
func TestCaptioningModeFollowsTheProfileUnlessChosen(t *testing.T) {
	klein := PipelineFor(PipelineFlux3Klein, "")
	generic := PipelineFor(PipelineGeneric, "")

	if got := klein.CaptioningMode(CaptioningPipeline); got != CaptioningAuto {
		t.Errorf("klein with nothing chosen = %q, want auto", got)
	}
	if got := klein.CaptioningMode(CaptioningOff); got != CaptioningOff {
		t.Errorf("an explicit off was overridden: %q", got)
	}
	if got := klein.CaptioningMode(CaptioningModel); got != CaptioningModel {
		t.Errorf("an explicit model mode was overridden: %q", got)
	}
	if got := generic.CaptioningMode(CaptioningPipeline); got != CaptioningPipeline {
		t.Errorf("generic with nothing chosen = %q, want no caption", got)
	}
}
