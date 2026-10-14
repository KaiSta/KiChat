// Package config holds user settings, their defaults, and the shipped system
// prompt presets.
package config

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"kichat/internal/images"
	"kichat/internal/llm"
	"kichat/internal/secret"
	"kichat/internal/store"
)

// settingsKey is the single row the whole settings blob lives under, so adding
// a field never requires a migration.
const settingsKey = "settings"

// Settings is the complete user configuration.
type Settings struct {
	Language            string                       `json:"language"`
	Location            string                       `json:"location"`
	Theme               string                       `json:"theme"`
	Backends            []llm.BackendConfig          `json:"backends"`
	ModelOverrides      map[string]llm.ModelOverride `json:"modelOverrides"`
	Search              SearchSettings               `json:"search"`
	Compact             CompactSettings              `json:"compact"`
	Sampler             SamplerSettings              `json:"sampler"`
	DefaultSystemPrompt string                       `json:"defaultSystemPrompt"`
	PromptPresets       []PromptPreset               `json:"promptPresets"`
	AutoTitle           bool                         `json:"autoTitle"`
	ToolIterations      int                          `json:"toolIterations"`
	ToolTokenBudget     int                          `json:"toolTokenBudget"`
	NativeTools         bool                         `json:"nativeTools"`
	// Runtime is the llama.cpp that KiChat runs itself, as opposed to the one a
	// user started and pointed a backend at. Every field here describes a
	// capability the app provides rather than a server it contacts, which is why
	// none of it lives on BackendConfig.
	Runtime RuntimeSettings `json:"runtime"`
	// HiddenModels lists models an availability check found this account cannot
	// call, keyed "backend/model" the way ModelOverrides is. It is stored rather
	// than guessed because the check that produces it costs one request per model
	// of a backend — eighty for build.nvidia.com — so re-deriving it on every
	// start would spend those requests to learn what the last sweep already knew.
	//
	// It is separate from ModelOverrides because the two are opposites: an
	// override corrects what a model *is*, and this records that the model could
	// not be reached at all.
	HiddenModels []string `json:"hiddenModels,omitempty"`
	// Images configures the picture generation the model can ask for, which is a
	// server the user runs rather than one of the chat backends above: an image
	// server has no conversation, no context window and no token accounting, so
	// almost nothing about a BackendConfig applies to it.
	Images ImageSettings `json:"images"`
	// ImageProfiles are the named picture configurations the Images tab offers as
	// a dropdown, one per checkpoint or per pipeline a user switches between.
	//
	// They sit next to Images rather than inside it because a profile *is* an
	// ImageSettings: nesting the type in itself would make the profile a user
	// picks and the configuration it restores two shapes able to drift apart. The
	// list is the same idea the system-prompt presets follow — a named set of saved
	// configurations, stored whole, so a field added later needs no migration.
	ImageProfiles []ImageProfile `json:"imageProfiles,omitempty"`
}

// ImageProfile is a named, saved picture configuration: what the Images tab
// holds, under a name the user can pick again.
//
// The credential is deliberately *not* part of one. A profile describes how a
// picture is drawn — a checkpoint or a pipeline's parts, a sampler, a step count,
// a size, a negative prompt — while the address and its "user:pass" belong to the
// server the user runs. That server is the thing every profile talks to, so a
// per-profile secret would mean one keyring entry per profile for a field most
// setups leave empty; the panel therefore keeps the credential with the feature
// and every profile applies under it. settings.normalise() clears the field and
// api.sanitize() blanks it again, so a hand-written blob cannot smuggle one in.
type ImageProfile struct {
	Name     string        `json:"name"`
	Settings ImageSettings `json:"settings"`
}

// ImageSettings configures text-to-image generation.
//
// Every generation field is deliberately omittable, and an empty value means
// "leave it to the server". A user who runs AUTOMATIC1111 has already chosen a
// checkpoint, a sampler, a step count and a picture size in its own settings, and
// a request that filled those in with this app's guesses would quietly replace
// their setup with something they never asked for. So an empty string or a zero
// number is left out of the request entirely, and the server's own configured
// default applies — which is also why the box in Settings is labelled as a hint
// rather than as a value.
type ImageSettings struct {
	Enabled bool `json:"enabled"`
	// BaseURL is the image server's address, e.g. http://127.0.0.1:7860 for
	// AUTOMATIC1111. It is empty until the feature is switched on, so nothing is
	// contacted on a machine that never asked for image generation.
	BaseURL string `json:"baseUrl"`
	// APIKey is the "user:pass" pair a WebUI started with `--api-auth` accepts as
	// HTTP basic auth. Like every other credential it is not part of the settings
	// blob: SaveSettings commits it to the secret store and the bridge only ever
	// sees whether one is stored.
	APIKey string `json:"apiKey"`
	// Model names a checkpoint to switch to for a request. Empty uses whatever the
	// server already has loaded.
	Model string `json:"model"`
	// Sampler is the sampler name to use, empty for the server's own.
	Sampler string `json:"sampler"`
	// Steps is the sampling step count, 0 for the server's own.
	Steps int `json:"steps"`
	// Width and Height are the picture size, 0 for the server's own.
	Width  int `json:"width"`
	Height int `json:"height"`
	// NegativePrompt is what the picture should avoid. Empty sends none, which
	// matters: an empty string sent explicitly would replace a negative prompt the
	// user configured in their WebUI, while omitting the field leaves it in force.
	NegativePrompt string `json:"negativePrompt"`
	// CfgScale is the guidance scale, 0 for the model's and the backend's own. It is a
	// float because the scale is one: 7.0 is what A1111 and sd-cli are shipped with,
	// and a distilled or flow-matching model is run at 1.0. Nothing here is "a default"
	// in the sense the empty size and step count used to be — every shared generation
	// field is the model's per picture, and one filled in here is a value the model
	// cannot overrule (see internal/tools/image.go).
	CfgScale float64 `json:"cfgScale"`
	// SaveFolder is where "Save as…" starts. It is remembered after a successful
	// save because a user who exports pictures to a folder wants the next one to
	// offer the same place, and re-picking it every time is the sort of friction
	// that makes a feature unused.
	SaveFolder string `json:"saveFolder"`

	// Kind is which backend draws. Empty is the server kind, which is what every
	// installation that predates this field is, so an old settings blob keeps
	// working without a migration — the same rule the other fields follow.
	Kind string `json:"kind"`
	// StartWithApp asks KiChat to start the managed server (ImageKindSD) whenever the
	// app starts. It is off unless the user asks, and deliberately not implied by
	// choosing the kind: a loaded model holds the VRAM it was loaded into for as long
	// as it runs, so "KiChat can run this for me" and "KiChat starts this every time I
	// open the window" are two statements, and only the second one costs memory on a
	// machine whose user is about to do something else. Started by hand is the other
	// way, and nothing else starts it: an app that brought a multi-gigabyte model up
	// without being asked would be spending the user's memory on a choice they never
	// made.
	//
	// It says nothing about *stopping*: a server that was started stays up until the
	// user stops it or the app exits, because the point of it is that the model is
	// already loaded when the next picture is asked for.
	StartWithApp bool `json:"startWithApp"`

	// The CLI kind's own fields. They are named after the flags they become, because
	// that is the only thing a user can check them against: there is no server to ask
	// what it offers, and stable-diffusion.cpp's own README tells users to run `-h`
	// rather than publishing a stable list.
	//
	// CLIBinary is the executable run once per picture: `sd-cli`, or a wrapper script
	// that sets up an environment first. Absolute, because a Settings field is filled
	// in by hand and "which directory was the app started from" is not something a
	// user can see.
	CLIBinary string `json:"cliBinary"`
	// CLIFolder is the directory the program and its libraries live in — what a
	// `sd-cli` release archive unpacked (or the folder above it). It is the *primary*
	// way to supply stable-diffusion.cpp: the executable is found inside it
	// (images.FindProgram) instead of being typed, so the only path a user has to get
	// right is the one they downloaded into.
	//
	// A set folder does not fall back to CLIBinary when the lookup finds nothing.
	// Pointing at a place is a statement about where the program is, and drawing with
	// an older typed path instead would run a program nobody chose.
	//
	// Which accelerator build that folder holds is deliberately *not* a setting. It is
	// read off the folder's own backend libraries (images.AcceleratorOf), which is the
	// only thing that answers the question once the archive's name is gone — so a
	// stored copy could only ever disagree with reality, and the panel would have two
	// answers to print. The picker in the panel drives which archive the user is told
	// to fetch; the reading says which one they actually have.
	CLIFolder string `json:"cliFolder"`
	// CLIModel is a single-file checkpoint, passed as `-m`.
	CLIModel string `json:"cliModel"`
	// CLIDiffusionModel, CLIVAE and CLILLM are the standalone parts of a pipeline newer
	// than SD1.x — Qwen-Image, Flux, SD3 — passed as `--diffusion-model`, `--vae` and
	// `--llm`. A pipeline that names its parts has no single checkpoint, and passing a
	// `-m` as well would hand the binary two models to load.
	CLIDiffusionModel string `json:"cliDiffusionModel"`
	CLIVAE            string `json:"cliVae"`
	CLILLM            string `json:"cliLlm"`
	// CLIClipL, CLIClipG and CLIT5XXL are the separate text encoders some pipelines
	// ask for. The flags have underscores and so do these names, so one can be read
	// off the other.
	CLIClipL string `json:"cliClipL"`
	CLIClipG string `json:"cliClipG"`
	CLIT5XXL string `json:"cliT5xxl"`
	// CLIOffloadToCPU passes `--offload-to-cpu`: the sampling runs on the host CPU
	// while the weights stay where they were loaded. It is a *mode* a user flips per
	// model rather than a value — Flux on a card too small to hold it is the case it
	// was asked for — which is why it is a switch here and not only a line in
	// CLIExtraArgs. It is passed as a bare flag, spelled the way a working command line
	// spells it; a build that wants `--offload-to-cpu 1` can be spelled that way in the
	// extra arguments instead.
	//
	// It is deliberately not part of a picture's recipe: it changes where the work
	// happens, not what the picture is, so it is read from settings on every draw the
	// way the step count is, rather than stored with the picture.
	CLIOffloadToCPU bool `json:"cliOffloadToCpu"`
	// CLIExtraArgs is the rest of the user's command line as one string, split by
	// runtime.SplitArgs before it is used. It is placed before the arguments KiChat
	// builds, so a field with its own control — the prompt, the size, the seed, the
	// step count — wins over a flag typed twice, while anything KiChat has no control
	// for still reaches the binary.
	//
	// It exists because that binary documents that its options change between builds.
	// Modelling every flag here would mean shipping a new KiChat whenever
	// stable-diffusion.cpp renamed one, and a flag this app does not know — the
	// `--sigmas` schedule, `--diffusion-fa`, a `-v` log level — is exactly what a turbo
	// model needs.
	CLIExtraArgs string `json:"cliExtraArgs"`
}

// Image backend kinds, as stored in ImageSettings.Kind.
const (
	// ImageKindServer is an HTTP server behind the /sdapi/v1/txt2img route that the
	// user runs themselves: AUTOMATIC1111, its Forge fork, or a stable-diffusion.cpp
	// server they started by hand.
	ImageKindServer = "server"
	// ImageKindCLI is a local binary run once per picture — stable-diffusion.cpp's
	// `sd-cli` — which needs no daemon and reaches flags the compatibility route does
	// not expose, at the cost of loading the model on every draw.
	ImageKindCLI = "cli"
	// ImageKindSD is the *same* local build, run as a server: stable-diffusion.cpp's
	// `sd-server`, started and stopped by KiChat, from the folder and with the model
	// flags the CLI kind already configures. It exists because the CLI kind pays a
	// model load per picture — minutes on a CPU for a q5 diffusion model — while a
	// server pays it once and then spends only the sampling per picture. The price is
	// the opposite one: a process, a port, and the VRAM a loaded model holds until it
	// is stopped.
	ImageKindSD = "sdserver"
)

// IsImageCLI reports whether the one-shot program is selected. An empty or
// unrecognised kind is the server backend, so a blob written by hand cannot leave the
// app with no backend at all.
func IsImageCLI(kind string) bool { return kind == ImageKindCLI }

// IsImageSD reports whether the build is run as a server KiChat starts and stops.
func IsImageSD(kind string) bool { return kind == ImageKindSD }

// DefaultImageBaseURL is AUTOMATIC1111's own default listen address, and is
// also what Forge and stable-diffusion.cpp's sd-server use when left alone. It is
// offered rather than probed: nothing is contacted until the feature is enabled.
// The address is declared in internal/images, which is where the client that dials
// it lives; this is the name the settings layer and its tests read it from. One
// string, one home: the address that is offered and the address that is dialled
// cannot drift apart.
const DefaultImageBaseURL = images.DefaultBaseURL

// ImageAPIKeyName is the secret-store key the image server's credential lives
// under. It is spelled like the backend keys so GetSecretStatus can report it in
// the same table.
const ImageAPIKeyName = "images"

// Image bounds. They exist to stop a hand-edited settings blob — or a model that
// passes a number it invented — from asking for a 20000-pixel picture, which a
// local machine would spend minutes and tens of gigabytes on rather than refuse.
//
// The values are declared in internal/images and only *named* here, because the
// providers are what a bound protects and what enforces it now (see
// images.Request.Normalised): a second copy in this package is how the two ends
// came apart before, with a size rounded by one backend and passed through by the
// other. The settings layer keeps the names because this is where the panel and
// the settings tests read them from.

const (
	// MaxImageDimension is the largest side a request may ask for.
	MaxImageDimension = images.MaxDimension
	// MinImageDimension is the smallest size worth drawing.
	MinImageDimension = images.MinDimension
	// MaxImageSteps bounds the sampling step count.
	MaxImageSteps = images.MaxSteps
	// MinImageCfgScale and MaxImageCfgScale bound the guidance scale, which is a float
	// and therefore has its own clamp below.
	MinImageCfgScale = images.MinCfgScale
	MaxImageCfgScale = images.MaxCfgScale
)

// NormaliseImageClamps repairs the numeric image fields in place, reporting
// whether anything changed. A value outside the bounds is clamped rather than
// rejected: the settings blob is edited by hand often enough that throwing an
// error would leave the field unopenable, and a clamped number is still a picture.
func NormaliseImageClamps(s *ImageSettings) bool {
	changed := false
	clamp := func(value, min, max int) int {
		if value <= 0 {
			return 0 // 0 is "the server's own", not a number to raise.
		}
		if value < min {
			return min
		}
		if value > max {
			return max
		}
		return value
	}
	if next := clamp(s.Width, MinImageDimension, MaxImageDimension); next != s.Width {
		s.Width = next
		changed = true
	}
	if next := clamp(s.Height, MinImageDimension, MaxImageDimension); next != s.Height {
		s.Height = next
		changed = true
	}
	if next := clamp(s.Steps, 1, MaxImageSteps); next != s.Steps {
		s.Steps = next
		changed = true
	}
	// The guidance scale is the one float among these, and it is read the same way: zero is
	// "the model decides", a negative or an unreadable number says the same thing as an
	// emptied box, and only a scale somebody actually chose is clamped to the range — below
	// the floor it is raised, above the ceiling capped. Not-a-number would pass every
	// comparison untouched and reach a JSON payload that cannot marshal one, so it folds to
	// zero here as well as in images.Request.Normalised, which is where a request that never
	// came through this function is bounded.
	cfg := s.CfgScale
	switch {
	case math.IsNaN(cfg) || cfg <= 0:
		cfg = 0
	case cfg < MinImageCfgScale:
		cfg = MinImageCfgScale
	case cfg > MaxImageCfgScale:
		cfg = MaxImageCfgScale
	}
	if cfg != s.CfgScale {
		s.CfgScale = cfg
		changed = true
	}
	// Three kinds have a spelling. The HTTP server is the empty string, which is what
	// every installation predating the field already carries; the two local lifetimes are
	// stored under the names of the programs they run — `cli` for sd-cli and `sdserver`
	// for sd-server. Everything else, `server` written out in full included, folds to the
	// empty value, so a typo in a hand-edited blob degrades to the backend that used to
	// work rather than to no backend at all, and the selector keeps one value per option.
	//
	// The managed server is *kept* rather than folded away: it is a process this app
	// starts and stops, so losing the name on the next load would silently swap the user's
	// own sd-server for whatever answers on the HTTP address.
	kind := strings.TrimSpace(s.Kind)
	if kind != ImageKindCLI && kind != ImageKindSD {
		kind = ""
	}
	if kind != s.Kind {
		s.Kind = kind
		changed = true
	}
	return changed
}

// RuntimeSettings configures the llama.cpp binaries KiChat installs and the
// servers it starts.
//
// It is deliberately one section rather than fields on a backend: the binaries
// and the download are shared, while which model to run is per backend entry.
// ManagedModels is keyed by backend id for that reason.
type RuntimeSettings struct {
	// Distribution names which llama.cpp the app installs and runs managed models
	// with: upstream's releases, or PrismML's fork, which is the build that loads a
	// ternary model such as Bonsai 2. It is one setting for the whole runtime
	// rather than one per model because there is one runtime directory and one
	// installed launcher: the two sources publish different releases under
	// different tag schemes, so "which build is on disk" is a question about the
	// source as much as about the family. Empty means upstream.
	Distribution string `json:"distribution"`
	// Tag is the llama.cpp build whose binaries were installed, empty when none
	// is. It is the directory name in the runtime folder, so it is also how an
	// install is found again after a restart — and its name carries the source it
	// came from, so a recorded build of the other source is recognised as one this
	// app would not start for the selected distribution.
	Tag string `json:"tag"`
	// BinaryPath overrides the downloaded binary with one the user installed
	// themselves — a distro package, a build with an accelerator this app does
	// not fetch, or a copy on a network share.
	BinaryPath string `json:"binaryPath"`
	// Accelerator chooses which published build to fetch.
	Accelerator string `json:"accelerator"`
	// ReadySeconds is how long a started server may take to load its model before
	// it is given up on. Loading a large model from a slow disk is minutes, not
	// seconds, so this is generous by default and the wait is reported.
	ReadySeconds int `json:"readySeconds"`
	// ModelDir is the folder the model picker looks in. It is remembered rather
	// than derived because a user with a second drive would otherwise have to
	// find the folder again in every session; detection fills it when nothing was
	// chosen, and it is never used to start anything on its own.
	ModelDir string `json:"modelDir"`
	// Models is one entry per backend KiChat starts itself.
	Models map[string]ManagedModel `json:"models"`
}

// ManagedModel is one model KiChat runs on its own.
type ManagedModel struct {
	// Enabled starts this model when the app starts, and is what makes the
	// backend reachable without the user running anything.
	Enabled bool `json:"enabled"`
	// ModelPath is the GGUF file to load.
	ModelPath string `json:"modelPath"`
	// CtxSize is the context window to load it with; 0 lets the server choose,
	// which is llama.cpp's own `-c` default of "loaded from model".
	CtxSize int `json:"ctxSize"`
	// FlashAttention selects llama.cpp's flash attention, which speeds up long
	// contexts and shrinks the attention memory: "auto" (the server's own
	// default), "on" or "off". Empty is normalised to "auto".
	FlashAttention string `json:"flashAttention"`
	// CacheTypeK and CacheTypeV quantise the KV cache, trading precision for
	// memory: a q8_0 cache is roughly half an f16 one, q4_0 roughly a quarter.
	// They are separate because K tolerates more compression than V. "f16" is
	// llama.cpp's own default and is passed through as such.
	CacheTypeK string `json:"cacheTypeK"`
	CacheTypeV string `json:"cacheTypeV"`
	// Offload selects how much of the model runs on the GPU, llama.cpp's
	// `--gpu-layers`. "auto" is the server's own default and the right answer for
	// everything that fits: llama.cpp places as many layers in the device memory
	// as it can (`--fit on` with a 1024 MiB margin), so nothing has to be counted
	// here. "all" forces every layer onto the device; "off" runs the model on the
	// CPU, which is what a GPU busy with something else needs — and what a model
	// whose KV cache does not fit beside its own weights needs as well.
	// Empty is normalised to "auto".
	Offload string `json:"offload"`
	// ExtraArgs is the rest of the command line, for the flags this app has no
	// field for (--n-gpu-layers 24, a chat template).
	ExtraArgs string `json:"extraArgs"`
}

// The llama.cpp release sources the app can install from. Like the family names
// below, they are spelled here rather than imported from internal/runtime so the
// settings package stays free of it, and the two lists are pinned against each other
// by a test.
const (
	// DistLlamaCPP is upstream ggml-org/llama.cpp, the default.
	DistLlamaCPP = "llamacpp"
	// DistPrism is PrismML's fork, the build that can load a ternary model.
	DistPrism = "prism"
)

// Distributions is every source a settings blob may name, in the order a picker
// offers them: the default first.
func Distributions() []string {
	return []string{DistLlamaCPP, DistPrism}
}

// IsDistribution reports whether a value names a source this app knows. An
// unrecognised value is repaired to the default rather than passed on: a download
// URL built from it would ask for an archive that does not exist.
func IsDistribution(value string) bool {
	for _, d := range Distributions() {
		if d == value {
			return true
		}
	}
	return false
}

// The build families the installer can fetch. They are spelled here rather than
// imported from internal/runtime so the settings package stays free of it; the
// two lists are pinned against each other by a test.
const (
	AccelCPU      = "cpu"
	AccelVulkan   = "vulkan"
	AccelROCm     = "rocm"
	AccelCUDA     = "cuda"
	AccelSYCL     = "sycl"
	AccelOpenVINO = "openvino"
	AccelOpenCL   = "opencl"
)

// Accelerators is every family a settings blob may name. Which of them a given
// platform publishes is a question for the installer, not for the stored value:
// settings travel between machines.
func Accelerators() []string {
	return []string{AccelCPU, AccelVulkan, AccelROCm, AccelCUDA, AccelSYCL, AccelOpenVINO, AccelOpenCL}
}

// Flash attention modes. "auto" is llama.cpp's own default and is the value a
// missing or unrecognised field falls back to.
const (
	FlashAuto = "auto"
	FlashOn   = "on"
	FlashOff  = "off"
)

// KV cache types, spelled as llama.cpp's `--cache-type-k/-v` accepts them. F16 is
// the server's own default; the rest trade precision for memory.
const (
	CacheTypeF16    = "f16"
	CacheTypeBF16   = "bf16"
	CacheTypeQ8_0   = "q8_0"
	CacheTypeQ5_1   = "q5_1"
	CacheTypeQ5_0   = "q5_0"
	CacheTypeQ4_1   = "q4_1"
	CacheTypeQ4_0   = "q4_0"
	CacheTypeIQ4_NL = "iq4_nl"
	CacheTypeF32    = "f32"
)

// FlashAttentionModes is every value the flash-attention field may hold, in the
// order a picker should offer them: the server's own choice first.
func FlashAttentionModes() []string {
	return []string{FlashAuto, FlashOn, FlashOff}
}

// CacheTypes is every KV cache type, roughly from most to least precise, with
// F16 — the server default — first.
func CacheTypes() []string {
	return []string{
		CacheTypeF16, CacheTypeBF16, CacheTypeQ8_0, CacheTypeQ5_1, CacheTypeQ5_0,
		CacheTypeQ4_1, CacheTypeQ4_0, CacheTypeIQ4_NL, CacheTypeF32,
	}
}

// IsFlashAttention reports whether a value names a flash-attention mode.
func IsFlashAttention(value string) bool {
	for _, v := range FlashAttentionModes() {
		if v == value {
			return true
		}
	}
	return false
}

// IsCacheType reports whether a value names a KV cache type.
func IsCacheType(value string) bool {
	for _, v := range CacheTypes() {
		if v == value {
			return true
		}
	}
	return false
}

// normaliseFlashAttention maps an absent or unrecognised value onto the server's
// own default. An empty field is what an installation that predates this setting
// carries, and it must mean "auto" rather than a flag the server did not ask for.
func normaliseFlashAttention(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !IsFlashAttention(value) {
		return FlashAuto
	}
	return value
}

// normaliseCacheType maps an absent or unrecognised cache type onto F16, the
// server's own default. A stored type this build does not know would be passed to
// `--cache-type-k/-v` and rejected at startup, so it is repaired here instead.
func normaliseCacheType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !IsCacheType(value) {
		return CacheTypeF16
	}
	return value
}

// GPU offload modes. "auto" is llama.cpp's own default — as many layers as fit in
// the device memory — and is what a missing or unrecognised field falls back to.
// The words are this app's: llama.cpp itself takes a layer count, `auto` or `all`,
// so `off` is the app's name for `--gpu-layers 0`.
const (
	OffloadAuto = "auto"
	OffloadAll  = "all"
	OffloadOff  = "off"
)

// OffloadModes is every value the offload field may hold, in the order a picker
// should offer them: the server's own choice first, the two explicit ones after
// it. An exact layer count is deliberately not among them — a number only one
// model needs stays a further argument (`--n-gpu-layers 24`), which is where the
// flags this app has no field for belong.
func OffloadModes() []string {
	return []string{OffloadAuto, OffloadAll, OffloadOff}
}

// IsOffload reports whether a value names an offload mode.
func IsOffload(value string) bool {
	for _, v := range OffloadModes() {
		if v == value {
			return true
		}
	}
	return false
}

// normaliseOffload maps an absent or unrecognised value onto the server's own
// default, for the same reason as the cache types: a word nothing here knows would
// otherwise reach `--gpu-layers` and be rejected at startup, which reads as a model
// that suddenly stopped loading rather than as a setting.
func normaliseOffload(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !IsOffload(value) {
		return OffloadAuto
	}
	return value
}

// Defaults for the managed runtime.
const (
	DefaultReadySeconds = 180
	// MinCtxSize is the smallest window worth starting a model with: below it a
	// chat with a system prompt and a scraped page cannot fit.
	MinCtxSize = 2048
)

// SearchSettings configures web search and scraping.
type SearchSettings struct {
	ProviderOrder []string `json:"providerOrder"`
	BraveAPIKey   string   `json:"braveApiKey"`
	Country       string   `json:"country"`
	SearchLang    string   `json:"searchLang"`
	ResultCount   int      `json:"resultCount"`
	Freshness     string   `json:"freshness"`
	ExtraSnippets bool     `json:"extraSnippets"`
	CacheMinutes  int      `json:"cacheMinutes"`

	// UseLLMContext prefers Brave's LLM-context route, which returns whole page
	// chunks instead of headlines, so the model can read sources without a
	// second scrape round trip. It degrades to a plain search when unavailable.
	UseLLMContext bool `json:"useLlmContext"`
	// LLMContextTokens is the token budget requested from that route.
	LLMContextTokens int `json:"llmContextTokens"`

	ScrapeRespectRobots     bool  `json:"scrapeRespectRobots"`
	ScrapeMaxBytes          int64 `json:"scrapeMaxBytes"`
	ScrapeTimeoutSeconds    int   `json:"scrapeTimeoutSeconds"`
	ScrapeAllowPrivateHosts bool  `json:"scrapeAllowPrivateHosts"`
	ScrapeCacheHours        int   `json:"scrapeCacheHours"`
}

// CompactSettings configures automatic compaction.
type CompactSettings struct {
	Enabled            bool    `json:"enabled"`
	Threshold          float64 `json:"threshold"`
	KeepRecentTurns    int     `json:"keepRecentTurns"`
	ReserveForResponse int     `json:"reserveForResponse"`
	SummaryTemperature float64 `json:"summaryTemperature"`
	MaxSummaryTokens   int     `json:"maxSummaryTokens"`
	// SummaryModel runs the summarisation on a model other than the chat's own.
	// A reasoning model can spend the whole summary budget thinking before it
	// writes a word — measured on a local reasoning model: every one of the 48
	// output tokens went to reasoning and not a single message was produced — and
	// not every backend lets thinking be switched off. A small instruction model
	// sidesteps the question. Empty means the chat's own model, which is the
	// previous behaviour and the right default: it is already loaded.
	SummaryModel llm.Ref `json:"summaryModel"`
}

// SamplerSettings are the global generation defaults.
type SamplerSettings struct {
	Temperature float64 `json:"temperature"`
	TopP        float64 `json:"topP"`
	MaxTokens   int     `json:"maxTokens"`
	NumCtx      int     `json:"numCtx"`
}

// PromptPreset is a named, saved system prompt.
type PromptPreset struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	Built  bool   `json:"builtIn"`
}

// DefaultLocation is the location substituted into {{location}} by default.
const DefaultLocation = "Karlsdorf-Neuthard, Landkreis Karlsruhe, Baden-Württemberg, Deutschland"

// Supported UI languages. English is the default; anything else in a stored
// settings blob is treated as unrecognised rather than trusted.
const (
	LangEnglish = "en"
	LangGerman  = "de"
	LangChinese = "zh"
)

// Default returns the shipped configuration.
func Default() Settings {
	return Settings{
		Language:       LangEnglish,
		Location:       DefaultLocation,
		Theme:          "dark",
		Backends:       llm.DefaultBackendConfigs(),
		ModelOverrides: map[string]llm.ModelOverride{},
		Runtime: RuntimeSettings{
			Distribution: DistLlamaCPP,
			Accelerator:  AccelCPU,
			ReadySeconds: DefaultReadySeconds,
			Models:       map[string]ManagedModel{},
		},
		Search: SearchSettings{
			ProviderOrder:        []string{"brave", "duckduckgo"},
			Country:              "DE",
			SearchLang:           "de",
			ResultCount:          8,
			ExtraSnippets:        true,
			CacheMinutes:         60,
			UseLLMContext:        true,
			LLMContextTokens:     8192, // Brave's own default; range 1024-32768
			ScrapeRespectRobots:  true,
			ScrapeMaxBytes:       5 << 20,
			ScrapeTimeoutSeconds: 20,
			ScrapeCacheHours:     168,
		},
		Compact: CompactSettings{
			Enabled:            true,
			Threshold:          0.80,
			KeepRecentTurns:    6,
			ReserveForResponse: 1024,
			SummaryTemperature: 0.2,
			MaxSummaryTokens:   1200,
		},
		Sampler: SamplerSettings{
			Temperature: 0.7,
			TopP:        0.9,
			MaxTokens:   0,
			NumCtx:      0,
		},
		DefaultSystemPrompt: PromptBody(PresetResearch, LangEnglish),
		PromptPresets:       DefaultPresets(LangEnglish),
		AutoTitle:           true,
		ToolIterations:      6,
		ToolTokenBudget:     25000,
		NativeTools:         true,
		// Image generation ships switched off. It is a second server the user has
		// to be running — a WebUI is a several-gigabyte install of its own — so
		// offering an address and probing it would report a failure for a feature
		// nobody asked for yet, which is the same reason the hosted chat backends
		// ship disabled.
		Images: ImageSettings{BaseURL: DefaultImageBaseURL},
	}
}

// Built-in preset identifiers. They are stable because a chat stores the one it
// was created with, and because the frontend translates a built-in's name by
// looking up "preset.<id>" — the Name field is only the fallback for a preset
// whose id has no translation yet.
const (
	PresetResearch   = "research"
	PresetSmallLocal = "small-local"
	PresetAssistant  = "assistant"
	PresetCoding     = "coding"
	PresetBlank      = "blank"
)

// DefaultPresets are the system prompts available out of the box, written in
// one UI language. The research prompt that shipped first stays first and stays
// the default; the small-model preset sits next to it because a local setup
// often starts on a small model. The names are the untranslated fallback — the
// settings view looks up "preset.<id>" first — so they do not follow lang.
func DefaultPresets(lang string) []PromptPreset {
	return []PromptPreset{
		{ID: PresetResearch, Name: "Research (default)", Prompt: PromptBody(PresetResearch, lang), Built: true},
		{ID: PresetSmallLocal, Name: "Small local models", Prompt: PromptBody(PresetSmallLocal, lang), Built: true},
		{ID: PresetAssistant, Name: "Neutral assistant", Prompt: PromptBody(PresetAssistant, lang), Built: true},
		{ID: PresetCoding, Name: "Code & engineering", Prompt: PromptBody(PresetCoding, lang), Built: true},
		{ID: PresetBlank, Name: "Empty", Prompt: PromptBody(PresetBlank, lang), Built: true},
	}
}

// Load reads the settings blob and folds the secrets back in from the secret
// store: the Brave key and each backend's API key, which the blob deliberately
// does not carry. Without this the keys a user typed in Settings would be gone on
// the next start. Load only reads the secret store; a missing key is not an error,
// so a first run with nothing stored still loads cleanly.
func Load(st *store.Store, ss *secret.Store) (Settings, error) {
	s := Default()
	found, err := st.GetSetting(settingsKey, &s)
	if err != nil {
		return s, err
	}
	if !found {
		return s, nil
	}
	if ss != nil {
		// The Brave key is a search-provider secret stored under its plain name;
		// each backend key is qualified with its backend id.
		if v, ok := ss.Get("brave"); ok {
			s.Search.BraveAPIKey = v
		}
		for i := range s.Backends {
			if v, ok := ss.Get(llm.BackendAPIKeyName(s.Backends[i].ID)); ok {
				s.Backends[i].APIKey = v
			}
		}
		// The image server's credential is a secret like the others, so it is read
		// back here rather than from the blob.
		if v, ok := ss.Get(ImageAPIKeyName); ok {
			s.Images.APIKey = v
		}
	}
	s.normalise()
	return s, nil
}

// Save writes the settings blob. Secrets are not part of it — SaveSettings
// commits the keys to the secret store and hands this a sanitised copy — so the
// blob only ever carries the non-secret shape of the configuration.
func (s Settings) Save(st *store.Store) error {
	return st.PutSetting(settingsKey, s)
}

// normalise repairs values that would otherwise break the app, e.g. a settings
// file from an older version or a hand-edited config with an empty backend list.
func (s *Settings) normalise() {
	switch s.Language {
	case LangEnglish, LangGerman, LangChinese:
	default:
		s.Language = LangEnglish
	}
	if s.Location == "" {
		s.Location = DefaultLocation
	}
	if len(s.Backends) == 0 {
		s.Backends = llm.DefaultBackendConfigs()
	}
	if strings.TrimSpace(s.Images.BaseURL) == "" {
		s.Images.BaseURL = DefaultImageBaseURL
	}
	// A crawled or hand-edited blob can carry a size or a step count no local
	// machine should be asked for; clamping here means the request builder never
	// has to trust the stored number.
	NormaliseImageClamps(&s.Images)
	// Each profile goes through the same clamp, because applying one writes these
	// numbers into the live fields: a profile that kept a 20000-pixel size would
	// re-arm the request the clamp above exists to prevent, once per pick. An
	// unnamed profile is dropped — the panel addresses one by its name, so nothing
	// could ever select it — and so is a repeated name, which would leave an
	// "update this profile" click with two candidates. The credential is cleared
	// for the reason it is never stored: profiles are configurations.
	profiles := make([]ImageProfile, 0, len(s.ImageProfiles))
	seenProfiles := map[string]bool{}
	for _, p := range s.ImageProfiles {
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" || seenProfiles[p.Name] {
			continue
		}
		seenProfiles[p.Name] = true
		NormaliseImageClamps(&p.Settings)
		p.Settings.APIKey = ""
		profiles = append(profiles, p)
	}
	if len(profiles) == 0 {
		profiles = nil
	}
	s.ImageProfiles = profiles
	// Ensure the shipped backends always exist, even if the user removed them
	// from an older settings blob. This is also how a backend shipped in a later
	// release — DeepSeek, then Mistral, then NVIDIA — reaches an installation
	// that predates it.
	seen := map[string]bool{}
	for _, b := range s.Backends {
		seen[b.ID] = true
	}
	for _, def := range llm.DefaultBackendConfigs() {
		if !seen[def.ID] {
			s.Backends = append(s.Backends, def)
		}
	}
	for i := range s.Backends {
		if s.Backends[i].BaseURL == "" {
			switch s.Backends[i].Kind {
			case "lmstudio":
				s.Backends[i].BaseURL = "http://127.0.0.1:1234"
			case "ollama":
				s.Backends[i].BaseURL = "http://127.0.0.1:11434"
			case "deepseek":
				// Restored here as well as in the provider, so the settings field
				// shows the address the app would actually use.
				s.Backends[i].BaseURL = llm.DeepSeekDefaultBaseURL
			case "mistral":
				// Same reason. The /v1 prefix is part of the address, and the
				// provider completes it for a base URL saved without it.
				s.Backends[i].BaseURL = llm.MistralDefaultBaseURL
			case "nvidia":
				// Same reason again: /v1 is part of the vendor's address, and a
				// self-hosted NIM container on another port is the user's edit to
				// this field rather than a different kind.
				s.Backends[i].BaseURL = llm.NvidiaDefaultBaseURL
			case "llamacpp":
				// Same reason as DeepSeek: 9931 is llama-server's documented
				// default, and an empty field would otherwise hide it.
				s.Backends[i].BaseURL = llm.LlamaCppDefaultBaseURL
			}
		}
		if s.Backends[i].Name == "" {
			s.Backends[i].Name = s.Backends[i].ID
		}
	}
	if s.ModelOverrides == nil {
		s.ModelOverrides = map[string]llm.ModelOverride{}
	}
	// The blacklist is a set, and the picker counts it: a duplicate or a blank
	// key would make that count disagree with what is actually hidden.
	if len(s.HiddenModels) > 0 {
		kept := make([]string, 0, len(s.HiddenModels))
		seenHidden := map[string]bool{}
		for _, key := range s.HiddenModels {
			key = strings.TrimSpace(key)
			if key == "" || seenHidden[key] {
				continue
			}
			seenHidden[key] = true
			kept = append(kept, key)
		}
		if len(kept) == 0 {
			kept = nil
		}
		s.HiddenModels = kept
	}
	if !IsDistribution(s.Runtime.Distribution) {
		// An empty field is what every installation that predates this setting
		// carries, and it has to mean upstream rather than something else.
		s.Runtime.Distribution = DistLlamaCPP
	}
	if !IsAccelerator(s.Runtime.Accelerator) {
		// Anything unrecognised falls back to the build that needs no GPU
		// toolkit: shipping a stored value straight into a download URL would
		// make the installer ask for an archive that may not exist.
		s.Runtime.Accelerator = AccelCPU
	}
	if s.Runtime.ReadySeconds <= 0 {
		s.Runtime.ReadySeconds = DefaultReadySeconds
	}
	if s.Runtime.ReadySeconds < 15 {
		s.Runtime.ReadySeconds = 15
	}
	if s.Runtime.ReadySeconds > 1800 {
		s.Runtime.ReadySeconds = 1800
	}
	// A named folder is kept even when it is not there right now: a drive that is
	// not mounted during one session is still the folder the user means, and
	// clearing it would quietly undo their choice.
	s.Runtime.ModelDir = strings.TrimSpace(s.Runtime.ModelDir)
	if s.Runtime.Models == nil {
		s.Runtime.Models = map[string]ManagedModel{}
	}
	for id, mm := range s.Runtime.Models {
		// A window below the minimum is not a smaller budget, it is a model that
		// cannot hold a conversation; 0 keeps its own meaning of "let the server
		// choose", which is a valid choice rather than a missing one.
		if mm.CtxSize != 0 && mm.CtxSize < MinCtxSize {
			mm.CtxSize = MinCtxSize
		}
		mm.FlashAttention = normaliseFlashAttention(mm.FlashAttention)
		mm.CacheTypeK = normaliseCacheType(mm.CacheTypeK)
		mm.CacheTypeV = normaliseCacheType(mm.CacheTypeV)
		mm.Offload = normaliseOffload(mm.Offload)
		s.Runtime.Models[id] = mm
	}
	if len(s.Search.ProviderOrder) == 0 {
		s.Search.ProviderOrder = []string{"brave", "duckduckgo"}
	}
	if s.Search.ResultCount <= 0 {
		s.Search.ResultCount = 8
	}
	if s.Search.CacheMinutes <= 0 {
		s.Search.CacheMinutes = 60
	}
	// Ranges come from the Brave LLM-context reference; values outside them are
	// rejected by the API, so they are clamped rather than sent and failed.
	if s.Search.LLMContextTokens <= 0 {
		s.Search.LLMContextTokens = 8192
	}
	if s.Search.LLMContextTokens < 1024 {
		s.Search.LLMContextTokens = 1024
	}
	if s.Search.LLMContextTokens > 32768 {
		s.Search.LLMContextTokens = 32768
	}
	if s.Search.ScrapeMaxBytes <= 0 {
		s.Search.ScrapeMaxBytes = 5 << 20
	}
	if s.Search.ScrapeTimeoutSeconds <= 0 {
		s.Search.ScrapeTimeoutSeconds = 20
	}
	if s.Search.ScrapeCacheHours <= 0 {
		s.Search.ScrapeCacheHours = 168
	}
	if s.Compact.Threshold <= 0 || s.Compact.Threshold >= 1 {
		s.Compact.Threshold = 0.80
	}
	if s.Compact.KeepRecentTurns < 0 {
		s.Compact.KeepRecentTurns = 6
	}
	if s.Compact.ReserveForResponse <= 0 {
		s.Compact.ReserveForResponse = 1024
	}
	if s.Compact.MaxSummaryTokens <= 0 {
		s.Compact.MaxSummaryTokens = 1200
	}
	// A half-filled summary model — a backend without a model, or the reverse —
	// would look configured and then fail every compaction, so it counts as
	// unset and the chat's own model is used, as before.
	if s.Compact.SummaryModel.Backend == "" || s.Compact.SummaryModel.Model == "" {
		s.Compact.SummaryModel = llm.Ref{}
	}
	if s.Sampler.Temperature <= 0 {
		s.Sampler.Temperature = 0.7
	}
	if s.Sampler.TopP <= 0 {
		s.Sampler.TopP = 0.9
	}
	if s.ToolIterations <= 0 {
		s.ToolIterations = 6
	}
	if s.ToolTokenBudget <= 0 {
		s.ToolTokenBudget = 25000
	}
	if len(s.PromptPresets) == 0 {
		s.PromptPresets = DefaultPresets(s.Language)
	}
	// A settings blob stores the whole preset list, so a preset shipped in a
	// later version would never reach an existing installation. Append the ones
	// that are missing without touching the others, the same way the shipped
	// backends are re-added above: an edited or removed built-in keeps its edited
	// text, it is only the absence that is repaired.
	{
		have := map[string]bool{}
		for _, p := range s.PromptPresets {
			have[p.ID] = true
		}
		for _, def := range DefaultPresets(s.Language) {
			if !have[def.ID] {
				s.PromptPresets = append(s.PromptPresets, def)
			}
		}
	}
	// Last, so that a preset appended just above is left alone and only the
	// ones already stored are moved to the interface language.
	s.LocalisePrompts()
}

// LocalisePrompts moves the built-in system prompts into the language the
// interface is set to. It is the whole of the "the prompts follow the app
// language" rule, and it runs both when settings are loaded and when they are
// saved, so switching the language in the settings view changes the prompt next
// to it instead of waiting for a restart.
//
// Only a prompt that is still byte-identical to a shipped body is replaced. A
// prompt the user edited — and one they added themselves — is left exactly as
// it is: rewriting it would silently throw away their work, and a half-German
// half-English prompt is worse than an untranslated one.
func (s *Settings) LocalisePrompts() {
	for i := range s.PromptPresets {
		p := &s.PromptPresets[i]
		if p.Built && isShippedPrompt(p.ID, p.Prompt) {
			p.Prompt = PromptBody(p.ID, s.Language)
		}
	}
	if s.DefaultSystemPrompt == "" || isShippedPrompt(PresetResearch, s.DefaultSystemPrompt) {
		s.DefaultSystemPrompt = PromptBody(PresetResearch, s.Language)
	}
}

// IsAccelerator reports whether a value names a build family this app knows.
func IsAccelerator(value string) bool {
	for _, a := range Accelerators() {
		if a == value {
			return true
		}
	}
	return false
}

// CurrentPeriod returns the "YYYY-MM" bucket used by the search quota counter.
func CurrentPeriod() string { return time.Now().Format("2006-01") }

// MarshalOverrides encodes a model override for storage.
func MarshalOverrides(o llm.ModelOverride) (string, error) {
	raw, err := json.Marshal(o)
	if err != nil {
		return "", fmt.Errorf("encode override: %w", err)
	}
	return string(raw), nil
}
