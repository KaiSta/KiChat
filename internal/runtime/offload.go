package runtime

import (
	"regexp"
	"strconv"
	"strings"
)

// The two explicit `--gpu-layers` modes this app passes. "auto" — the server's own
// default, as many layers as fit in the device memory — is not passed at all, the
// same way an "auto" flash attention is not. Spelled here as well as in the
// settings package so neither has to import the other; a test pins the two lists
// together.
const (
	OffloadAll = "all"
	OffloadOff = "off"
)

// TraceVerbosity is llama.cpp's `-lv 4`, the level at which a server says which
// device it picked and how many layers it put there. The default (3, info) prints
// neither — measured on both release sources: a load of a 9B model at the default
// level produced 24–28 lines and not one of them about a device, while the same
// load at `-lv 4` printed `using device Vulkan0 …` and
// `load_tensors: offloaded 34/34 layers to GPU`.
const TraceVerbosity = 4

// gpuLayers is the value to pass as `--gpu-layers`, empty when the server's own
// default is wanted.
//
// The app's word for a fully CPU-side model is `off`, because that is what it
// means to a user; llama.cpp's own spelling is a count of zero, and that is what
// goes on the command line. `auto` is the server's default (`--fit on`, a 1024 MiB
// margin), so it is left off rather than passed as a word a build might read
// differently.
func gpuLayers(offload string) string {
	switch strings.TrimSpace(offload) {
	case OffloadAll:
		return "all"
	case OffloadOff:
		return "0"
	}
	return ""
}

// Offload is llama.cpp's own answer about where a model's layers went, read out of
// the lines it prints while it loads one:
//
//	llama_prepare_model_devices: using device Vulkan0 (…) (0000:0c:00.0) - 18678 MiB free
//	load_tensors: offloaded 34/34 layers to GPU
//
// The two belong together, because the second one alone is not evidence of a GPU.
// Measured on prism-b10770-6684606:
//
//   - its CPU-only build, which has no accelerator at all and lists nothing but the
//     host CPU, still logs `offloaded 65/65 layers to GPU` for a ternary model —
//     llama.cpp's wording covers the compute buffers whatever device they are in —
//     and names no device, because `llama_prepare_model_devices` never runs there;
//   - a GPU build started with `--device none` is the same: every layer "offloaded",
//     no device line;
//   - a GPU build started with `--gpu-layers 0` is the opposite case: it does name
//     `Vulkan0`, and the layer line reads `0/65`.
//
// So the *device line* is what distinguishes a placed model from a CPU one, and a
// reading is about a GPU only when it has both.
//
// What the reading cannot resolve: a machine whose layers are split across several
// cards of one backend prints a `using device` line *per card* — the free memory in
// each line is that card's — while the layer line stays a single summary of the whole
// model. The id kept here is then one of the devices the layers are on, and the count
// is about the model rather than about that card. Reasoned from the log's shape, not
// measured: this machine has one card per backend. It is still the honest half of the
// answer, because the absence of a device line means something else entirely — a build
// that named no device at all, i.e. a model on the CPU.
//
// Reported rather than predicted. This app can see a model's size and the free
// device memory and warns when the first outgrows the second, but whether
// llama.cpp placed the layers on the device is a different question — and one that
// a `-ngl auto` start answers only in these two lines.
type Offload struct {
	// Layers and Total are the numbers from that line, "34/34". Both are 0 when
	// nothing was reported.
	Layers int `json:"layers"`
	Total  int `json:"total"`
	// Device is the id the server named for the placement, e.g. "Vulkan0". It is
	// what ties the reading to a row of the device list the panel shows.
	Device string `json:"device,omitempty"`
}

// Reported reports whether a placement was read at all.
//
// Total is what decides, not Layers: "0/34" is an answer — llama.cpp kept every
// layer on the CPU — while an empty reading is not one. A launcher started without
// the trace level prints neither line, and "not reported" must not look like
// "nothing offloaded".
func (o Offload) Reported() bool { return o.Total > 0 }

// OnDevice reports whether layers went to a named device, which is the difference
// between a model that runs on the GPU and one that only could.
//
// A named device is required, not just a layer count: a CPU-only build reports
// every layer as offloaded and names no device, and reading that as "on the GPU"
// would be the silent CPU fallback this app exists to make visible. See Offload.
func (o Offload) OnDevice() bool { return o.Layers > 0 && o.Device != "" }

var (
	// Measured on b11541 and on the fork's prism-b10770, both at `-lv 4`:
	//
	//	load_tensors: offloading 32 repeating layers to GPU
	//	load_tensors: offloaded 34/34 layers to GPU
	//
	// The summary is the line that is always there and always about the whole
	// model, which is why it is the one parsed.
	offloadSummary = regexp.MustCompile(`\boffloaded (\d+)/(\d+) layers to GPU\b`)
	// The device line carries the id, the card name and the free memory:
	//
	//	llama_prepare_model_devices: using device Vulkan0 (AMD Radeon RX 7900 XT (RADV NAVI31)) (0000:0c:00.0) - 18678 MiB free
	//
	// Only the id is taken: the card name and its memory are already a row of the
	// device list, and this app does not rebuild a sentence about hardware it did
	// not detect.
	offloadDevice = regexp.MustCompile(`\busing device ([A-Za-z0-9_.:-]+)`)
)

// parseOffloadLine folds one line of a server's log into a placement reading.
//
// The two lines arrive apart — the device is named first, the layer count at the
// end of loading — so the reading is carried forward and updated rather than
// rebuilt, and a line that is about neither leaves it alone. A device line still
// counts when nothing is offloaded: a `--gpu-layers 0` run names the device it
// *would* have used, and that is precisely what makes "off on a machine that has a
// card" readable.
func parseOffloadLine(line string, current Offload) Offload {
	if match := offloadSummary.FindStringSubmatch(line); match != nil {
		layers, errLayers := strconv.Atoi(match[1])
		total, errTotal := strconv.Atoi(match[2])
		if errLayers == nil && errTotal == nil {
			current.Layers, current.Total = layers, total
		}
	}
	if match := offloadDevice.FindStringSubmatch(line); match != nil {
		current.Device = match[1]
	}
	return current
}
