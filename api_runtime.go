package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"kichat/internal/config"
	"kichat/internal/llm"
	rt "kichat/internal/runtime"
	"kichat/internal/uimsg"
)

// ManagedStatus is one model KiChat runs itself, as the UI reads it.
type ManagedStatus struct {
	BackendID string `json:"backendId"`
	// Kind is the backend's kind, so a row can be read without looking the entry up
	// again in a settings snapshot that may have moved on.
	Kind      string `json:"kind,omitempty"`
	Enabled   bool   `json:"enabled"`
	ModelPath string `json:"modelPath"`
	// LoadedModel is the GGUF the running server was started with — what is
	// answering, as opposed to what is selected — and it is empty when nothing is
	// running. The two are deliberately separate fields: picking another file in the
	// folder listing moves ModelPath at once, while the process in memory keeps
	// serving the model it loaded until it is started again, and a listing that
	// marked the selection as "in use" would be claiming a switch that has not
	// happened. A stopped server and a failed start both report nothing here, even
	// though the supervisor still has the path in its status.
	LoadedModel string `json:"loadedModel,omitempty"`
	CtxSize     int    `json:"ctxSize"`
	ExtraArgs   string `json:"extraArgs"`
	// MMProj is the multimodal projector a start would load, empty for a text-only
	// model, and MMProjDetected says the app found it beside the model rather than
	// being pointed at it. The panel needs the difference: a detected file is a path
	// nobody typed, so it has to be *shown* as the reason images work, while a typed
	// one is already in the field above it.
	MMProj         string   `json:"mmproj,omitempty"`
	MMProjDetected bool     `json:"mmprojDetected,omitempty"`
	State          string   `json:"state"`
	Port           int      `json:"port"`
	URL            string   `json:"url"`
	Error          string   `json:"error,omitempty"`
	Log            []string `json:"log,omitempty"`
	// ModelBytes is the size of the GGUF on disk, 0 when it cannot be read. The
	// panel measures it against DeviceFreeBytes, because it is the one part of a
	// model's memory use this app can know without loading it — the weights have to
	// fit before the KV cache is even considered.
	ModelBytes int64 `json:"modelBytes"`
	// OffloadLayers and OffloadTotal are the numbers the running server printed
	// about the model's layers, "34/34" — the answer to "is this model actually on
	// the GPU?" as opposed to the guess the free-memory warning makes. Both are 0
	// when the server said nothing, which is what a launcher started without the
	// trace level does: an empty reading is "not reported", never "nothing
	// offloaded".
	//
	// OffloadDevice is the card those layers are on, and it is empty whenever they
	// are not on one — including the CPU-only build, which reports every layer as
	// offloaded and names no device. So the panel's rule is this single field: a
	// name means the GPU, no name means the CPU. The panel phrases the sentence —
	// numbers and an id cross the bridge, never a translated string.
	OffloadLayers int    `json:"offloadLayers"`
	OffloadTotal  int    `json:"offloadTotal"`
	OffloadDevice string `json:"offloadDevice,omitempty"`
}

// RuntimeStatus is everything the UI needs about the llama.cpp KiChat would run:
// whether a launcher is available, where it came from, and what each managed entry
// is doing.
type RuntimeStatus struct {
	Installed bool   `json:"installed"`
	Tag       string `json:"tag,omitempty"`
	Binary    string `json:"binary,omitempty"`
	// Version is what the launcher reports about itself, which is the only thing
	// that describes a binary the user supplied instead of the one the installer
	// fetched — and the only way to tell two builds of "0.6.0-dev" apart.
	Version string `json:"version,omitempty"`
	// CustomBinary is the path a user gave instead of the installed build.
	CustomBinary string `json:"customBinary,omitempty"`
	// Devices are the compute devices the launcher in use reports, e.g.
	// "Vulkan0: AMD Radeon RX 7900 XT (RADV NAVI31) (20464 MiB, 18418 MiB free)".
	// An installed build with an empty list sees no accelerator and will run on the
	// CPU — which is what makes a silent fallback visible instead of a model that
	// just feels slow.
	Devices []string `json:"devices"`
	// DevicesError says why the list could not be read at all. It is kept apart
	// from an empty list because "uses no GPU" and "did not answer" call for
	// different things from a user.
	DevicesError string `json:"devicesError,omitempty"`
	// DevicesMissingLibs names the shared objects the launcher's accelerator
	// backend needs and this machine does not have, e.g. "libamdhip64.so.7" for a
	// build made for ROCm 10 on a machine with ROCm 5.7. An empty device list beside
	// these is not a mystery: the backend cannot be loaded at all, and that is why
	// no device is reported and the model runs on the CPU. Only filled when the
	// device list came back empty.
	DevicesMissingLibs []string `json:"devicesMissingLibs"`
	// DeviceFreeBytes is the free memory the devices reported, added up across
	// them because llama.cpp splits a model over several devices. 0 means unknown:
	// either there is no accelerator, or the build named its devices without their
	// memory. A model file larger than this cannot be placed on the GPUs whole.
	DeviceFreeBytes int64  `json:"deviceFreeBytes"`
	Accelerator     string `json:"accelerator"`
	ReadySeconds    int    `json:"readySeconds"`
	// Busy, Phase, Done and Total describe an install in progress. The UI polls
	// this rather than listening for an event: a download outlives the call that
	// started it, and polling cannot miss its end.
	Busy  bool   `json:"busy"`
	Phase string `json:"phase,omitempty"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
	Error string `json:"error,omitempty"`
	// Distribution is the release source the app installs from, and Distributions
	// the ones it can offer: upstream llama.cpp, and PrismML's fork, which is the
	// build that loads a ternary model. Distributions is never empty — the picker
	// would have nothing to offer.
	Distribution  string   `json:"distribution"`
	Distributions []string `json:"distributions"`
	// Variants are the build families the selected source actually publishes for
	// this platform, in the order to offer them. The picker is filled from here
	// rather than from a list of its own, so it cannot offer a build that does not
	// exist — ROCm and SYCL are x64 only, macOS publishes a single Metal-enabled
	// build per arch, and the fork publishes neither an OpenVINO nor an Adreno one.
	Variants []string `json:"variants"`
	// FlashAttentionModes and CacheTypes are the values a managed model may carry
	// for `--flash-attn` and `--cache-type-k/-v`. The pickers are filled from here
	// for the same reason as Variants: a value the backend cannot pass must not be
	// offered.
	FlashAttentionModes []string `json:"flashAttentionModes"`
	CacheTypes          []string `json:"cacheTypes"`
	// OffloadModes are the values a managed model may carry for `--gpu-layers`,
	// filled from here for the same reason: the picker offers what this app can
	// pass. The words are the app's own — llama.cpp takes a count, `auto` or `all`,
	// so "off" is how KiChat spells `--gpu-layers 0`.
	OffloadModes []string `json:"offloadModes"`
	// Update is the last look at llama.cpp's releases, if one has been made. It is
	// reported rather than asked for here: see RuntimeUpdate.
	Update RuntimeUpdate `json:"update"`
	// RestartPending means a build was downloaded during this session and no managed
	// model has been started since, so a server that is running now is still the old
	// build. It is what makes the "restart the model" line survive the update it
	// refers to; a managed start clears it.
	RestartPending bool `json:"restartPending"`
	// MLX is the mlx-lm environment KiChat installs on Apple silicon. It is a
	// second, independent install with its own fields, because a pip run and a
	// llama.cpp download must not overwrite each other's phase or error.
	MLXSupported bool   `json:"mlxSupported"`
	MLXInstalled bool   `json:"mlxInstalled"`
	MLXVersion   string `json:"mlxVersion,omitempty"`
	// MLXPython is the interpreter inside the environment the app made.
	MLXPython string `json:"mlxPython,omitempty"`
	// MLXServer is a launcher the user supplied instead.
	MLXServer string `json:"mlxServer,omitempty"`
	MLXBusy   bool   `json:"mlxBusy"`
	MLXPhase  string `json:"mlxPhase,omitempty"`
	MLXError  string `json:"mlxError,omitempty"`
	// Models carries one entry per id in the settings, so a row with no model
	// configured still appears with the state to explain it.
	Models []ManagedStatus `json:"models"`
}

// RuntimeUpdate is the answer to "is there a newer llama.cpp build?", as the panel
// reads it.
//
// It is a type of its own rather than more fields on RuntimeStatus because it is the
// only question in that panel that costs a request to llama.cpp's release list — an
// unauthenticated API that allows 60 requests an hour per address — while the status
// is polled every 1.2 s while a download or a model load is under way. So the lookup
// happens when it is asked for, its answer is cached on the App, and
// GetRuntimeStatus only reports what that lookup found.
type RuntimeUpdate struct {
	// Checked is false until a lookup has finished. "up to date" and "not asked"
	// are different statements, and only one of them is a claim about the build on
	// disk.
	Checked bool `json:"checked"`
	// Available marks the newest published build as newer than the one on disk.
	Available bool `json:"available"`
	// Family is the build family the lookup was made for, so a cached reading is
	// not shown as an answer about a family the user has since switched to.
	Family string `json:"family,omitempty"`
	// Distribution is the source the lookup was made for. A reading taken from
	// upstream's releases says nothing about the newest fork build, so a switch of
	// the source has to discard it the same way a family switch does.
	Distribution string `json:"distribution,omitempty"`
	// Installed and Latest are the build tags being compared (`b11535`, `b11541`) —
	// the two halves of the "b11535 → b11541" the panel prints. Installed is empty
	// when there is no managed build to compare against.
	Installed string `json:"installed,omitempty"`
	Latest    string `json:"latest,omitempty"`
	// Size is what an update would download, both archives when the family
	// publishes a companion runtime one. 0 when the release did not report it.
	Size int64 `json:"size"`
	// Error is why no newest build could be established — no network, a rate limit,
	// no build for this machine. It is kept apart from Available=false because
	// "current" is a claim this app must not make when it could not ask.
	Error string `json:"error,omitempty"`
}

// updateLookupTimeout bounds a release lookup. The installer's own client allows 30
// minutes because downloading hundreds of megabytes needs it; a JSON listing that
// has not answered in ten seconds is not worth the wait, and the panel awaits this
// call.
const updateLookupTimeout = 10 * time.Second

// CheckLlamaRuntimeUpdate asks llama.cpp's releases for the newest build of the
// family this app installs, and reports whether it is newer than the one on disk.
// The answer is cached and GetRuntimeStatus reports the cached copy: the panel polls
// that status while a model loads, and spending a request out of a 60-an-hour budget
// on every poll would be answering a question whose answer changes once a day.
//
// It blocks until the lookup is done — a few hundred milliseconds when GitHub
// answers, bounded by updateLookupTimeout when it does not. A lookup that failed is
// reported in the reading rather than as an error, because it is a row in the panel
// ("could not ask") and not a failure of the call.
func (a *App) CheckLlamaRuntimeUpdate() (RuntimeUpdate, error) {
	if err := a.ensure(); err != nil {
		return RuntimeUpdate{}, err
	}
	settings := a.Settings()
	if strings.TrimSpace(settings.Runtime.BinaryPath) != "" {
		// The launcher in use is the user's own build. llama.cpp's releases say
		// nothing about it, and offering to replace a binary the user pointed at
		// would be an offer to overwrite it.
		a.setUpdate(RuntimeUpdate{})
		return RuntimeUpdate{}, nil
	}

	// The source and family the app installs for: a draft the picker has not saved
	// is not a build that is on disk to compare against. Both fall back for an empty
	// or unknown value.
	dist := distributionFor(settings)
	family := installAccelerator("", settings.Runtime.Accelerator)
	installed := a.installedBuildTag(settings)

	// Bounded by the app's own context, not a fresh one: a lookup that is still in
	// flight when the window closes is answered by the store's own shutdown rather
	// than left to outlive it. Tests call this on an App whose ctx is nil.
	parent := a.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, updateLookupTimeout)
	defer cancel()
	asset, err := a.installer.Latest(ctx, dist, goruntime.GOOS, goruntime.GOARCH, family)
	if err != nil {
		reading := RuntimeUpdate{Checked: true, Family: family, Distribution: dist.ID, Installed: installed, Error: err.Error()}
		a.setUpdate(reading)
		return reading, nil
	}
	reading := updateReadingOf(dist, asset, installed)
	a.setUpdate(reading)
	return reading, nil
}

// updateReadingOf is one release lookup as the panel shows it: the two tags, what the
// download would cost, and whether the published build is newer than the one on disk.
// The install path calls it too, where the two tags are equal by construction — which
// is what makes an update marker disappear the moment the update is installed.
//
// The comparison belongs to the source: both sides are that source's tags, read with
// its own tag shape. Reading a fork tag with upstream's shape made an installed fork
// build look older than itself, because the name reduced to the prefix `prism` — an
// update offered on every poll, for the build already on disk.
func updateReadingOf(dist rt.Distribution, asset rt.Asset, installed string) RuntimeUpdate {
	size := asset.Size
	if asset.Companion != nil {
		size += asset.Companion.Size
	}
	return RuntimeUpdate{
		Checked:      true,
		Family:       asset.Variant,
		Distribution: dist.ID,
		Installed:    installed,
		Latest:       asset.Tag,
		Size:         size,
		Available:    dist.NewerBuild(asset.Tag, installed),
	}
}

// installedBuildTag is the build tag of the launcher the app would start, which is the
// side of the comparison the panel names as installed. It goes through managedLauncher
// for the same reason GetRuntimeStatus does: the build an update is weighed against has
// to be the build a start would run, not merely the newest directory of the source —
// otherwise a recorded build behind a newer directory is offered no update, and the
// comparison is about a llama.cpp the app does not use. The name is read with the shape
// of the source it was asked about, so a directory of the other source reduces to no tag
// at all and a fork build is never compared against upstream's releases.
func (a *App) installedBuildTag(settings config.Settings) string {
	dist := distributionFor(settings)
	tag, _ := a.managedLauncher(settings)
	return dist.TagOfDir(tag)
}

// InstalledBuild is one llama.cpp build on disk, as the panel reads it.
//
// It is listed rather than counted because a build is not small and an update leaves
// the one it replaces: a CPU build is 17.8 MB, a CUDA one 593 MB with its runtime
// bundle, and the sizes are what tell a user which line is worth a second look.
type InstalledBuild struct {
	// Dir is the directory name and the tag the settings would record for it
	// (`b11541-vulkan-10.0`).
	Dir string `json:"dir"`
	// Size is what the directory occupies in bytes.
	Size int64 `json:"size"`
	// InstalledAt is when the build landed.
	InstalledAt time.Time `json:"installedAt"`
	// InUse marks the build this app would start a model with — the one the settings
	// record, or the newest one when nothing is recorded. It is the line that must
	// not be deleted.
	InUse bool `json:"inUse"`
	// Running marks a build a managed server was started from and has not stopped
	// since. It is kept apart from InUse because it is the other way a deletion
	// breaks something: the server holds those files open, so on Windows the delete
	// would not even succeed.
	Running bool `json:"running"`
	// Incomplete marks what an interrupted install left behind — a staging directory
	// that is always safe to remove and is otherwise invisible.
	Incomplete bool `json:"incomplete"`
}

// ListLlamaBuilds lists the llama.cpp builds on disk with what each occupies, so an
// old one can be seen before it is removed.
func (a *App) ListLlamaBuilds() ([]InstalledBuild, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	inUse, running := a.buildDirsInUse(a.Settings())
	builds := a.installer.Builds()
	out := make([]InstalledBuild, 0, len(builds))
	for _, build := range builds {
		out = append(out, InstalledBuild{
			Dir: build.Dir, Size: build.SizeBytes, InstalledAt: build.InstalledAt,
			InUse: inUse[build.Dir], Running: running[build.Dir],
			Incomplete: build.Incomplete,
		})
	}
	return out, nil
}

// RemoveLlamaBuild deletes one installed build.
//
// The build in use and the build a running server was started from are refused, with
// the reason: removing the first leaves a model that cannot start, and removing the
// second takes the files out from under a server that is answering.
func (a *App) RemoveLlamaBuild(dir string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	name := strings.TrimSpace(dir)
	inUse, running := a.buildDirsInUse(a.Settings())
	if running[name] {
		return uimsg.Errorf("runtime.modelRunning", name)
	}
	if inUse[name] {
		return uimsg.Errorf("runtime.buildInUse", name)
	}
	if err := a.installer.RemoveBuild(name); err != nil {
		return err
	}
	if a.ctx != nil {
		runtime.LogInfof(a.ctx, "llama.cpp-Build %s entfernt", name)
	}
	return nil
}

// RemoveOldLlamaBuilds deletes every build except the one in use, and answers how
// many it removed.
//
// It is the list's one-press version, and what it keeps is exactly what this app
// would start — so "which of these do I still need" is never left to be guessed. A
// running server does not make its build expendable: that one is refused by name and
// the rest go.
func (a *App) RemoveOldLlamaBuilds() (int, error) {
	if err := a.ensure(); err != nil {
		return 0, err
	}
	inUse, running := a.buildDirsInUse(a.Settings())
	removed := 0
	var failure error
	for _, build := range a.installer.Builds() {
		if inUse[build.Dir] || running[build.Dir] {
			continue
		}
		if err := a.installer.RemoveBuild(build.Dir); err != nil {
			if failure == nil {
				failure = err
			}
			continue
		}
		removed++
	}
	if a.ctx != nil && removed > 0 {
		runtime.LogInfof(a.ctx, "%d alte llama.cpp-Builds entfernt", removed)
	}
	if failure != nil {
		// The count is lost with the error, and the panel re-reads the list anyway —
		// so what matters is that the reason is not swallowed.
		return removed, fmt.Errorf("%d entfernt, dann: %w", removed, failure)
	}
	return removed, nil
}

// buildDirsInUse names the build directories a removal has to leave alone: the one
// this app would start, and any a managed server is running from.
//
// A launcher the user supplied protects nothing here. It lives outside the runtime
// directory, and a path that is not a build says nothing about which builds exist.
func (a *App) buildDirsInUse(settings config.Settings) (inUse, running map[string]bool) {
	inUse, running = map[string]bool{}, map[string]bool{}
	if binary, err := a.llamaServerPath(settings); err == nil {
		if dir := buildDirOf(a.installer.Dir, binary); dir != "" {
			inUse[dir] = true
		}
	}
	a.serversMu.Lock()
	defer a.serversMu.Unlock()
	for _, srv := range a.servers {
		state := srv.Status().State
		// A stopped server says nothing: what it ran last is not a reason to keep
		// hundreds of megabytes.
		if state != rt.StateStarting && state != rt.StateRunning {
			continue
		}
		if dir := buildDirOf(a.installer.Dir, srv.Binary()); dir != "" {
			running[dir] = true
		}
	}
	return inUse, running
}

// buildDirOf is the runtime build directory a launcher lives in, empty for a path
// outside root. A build is a direct child of the runtime directory, so anything else
// — the user's own launcher, the models folder — is not a build this list manages.
func buildDirOf(root, binary string) string {
	if strings.TrimSpace(binary) == "" {
		return ""
	}
	rel, err := filepath.Rel(root, filepath.Dir(binary))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	// An archive whose wrapper could not be unwrapped leaves the launcher one level
	// deeper, and that build is the directory above it.
	if i := strings.IndexRune(rel, filepath.Separator); i >= 0 {
		rel = rel[:i]
	}
	return rel
}

// runtimeStatusBase is the part of the status that comes from the stored settings,
// before anything live is filled in: the build families this platform publishes,
// and one row per configured backend id.
//
// It is a function rather than inline code so a test can pin the shape of an idle
// answer, because that shape is a trap: a nil slice marshals to JSON `null`, not
// `[]`, and the panel reads these as arrays. On a fresh install nothing is
// configured, so `Models` stayed nil, `status.models` arrived as null and
// `status.models.find(...)` threw — taking the whole Settings view down to the
// error boundary with "Cannot read properties of null (reading 'find')". That is
// the same trap `chat.UnresolvedVars` set. The slices here are never nil.
func runtimeStatusBase(settings config.Settings, goos, goarch string) RuntimeStatus {
	dist := distributionFor(settings)
	status := RuntimeStatus{
		Accelerator:         settings.Runtime.Accelerator,
		Distribution:        dist.ID,
		Distributions:       distributionIDs(),
		ReadySeconds:        settings.Runtime.ReadySeconds,
		CustomBinary:        settings.Runtime.BinaryPath,
		Variants:            []string{},
		FlashAttentionModes: config.FlashAttentionModes(),
		CacheTypes:          config.CacheTypes(),
		OffloadModes:        config.OffloadModes(),
		Devices:             []string{},
		DevicesMissingLibs:  []string{},
		Models:              []ManagedStatus{},
	}
	// The families of the selected source, not of both: the fork publishes five of
	// upstream's six, and a picker offering OpenVINO under a PrismML label would be
	// offering a download the source does not have.
	if variants := dist.PublishedAccelerators(goos, goarch); len(variants) > 0 {
		status.Variants = variants
	}
	for _, id := range sortedRuntimeIDs(settings) {
		mm := settings.Runtime.Models[id]
		kind := backendKind(settings, id)
		// Projector pairing is a llama.cpp question: an MLX model is a folder, has no
		// mmproj, and scanning its snapshot for one would be a second wrong answer on
		// a row that has no field to show it.
		projector, detected := "", false
		if kind != "mlx" {
			projector, detected = projectorReading(mm)
		}
		status.Models = append(status.Models, ManagedStatus{
			BackendID: id, Kind: kind, Enabled: mm.Enabled, ModelPath: mm.ModelPath,
			CtxSize: mm.CtxSize, ExtraArgs: mm.ExtraArgs, State: rt.StateStopped,
			MMProj: projector, MMProjDetected: detected,
		})
	}
	return status
}

// distributionFor resolves the release source the settings name. An unrecognised
// value is upstream, which is what config's own repair does with it; this is the
// second line of defence for a blob that never went through that.
func distributionFor(settings config.Settings) rt.Distribution {
	return resolvedDistribution(settings.Runtime.Distribution)
}

// resolvedDistribution turns a stored or picked source id into a source, falling
// back to upstream for anything this app does not know.
func resolvedDistribution(id string) rt.Distribution {
	if dist, ok := rt.DistributionFor(id); ok {
		return dist
	}
	return rt.DefaultDistribution()
}

// distributionIDs is the sources the panel offers, never empty: a picker with no
// options would leave the stored source unchangeable.
func distributionIDs() []string {
	dists := rt.Distributions()
	ids := make([]string, 0, len(dists))
	for _, d := range dists {
		ids = append(ids, d.ID)
	}
	return ids
}

// deviceTexts is the device entries as the panel shows them: the line the server
// printed, which already carries the id, the card name and its memory. Rebuilding
// that sentence from the parsed fields would be this app inventing a description
// of hardware it did not detect.
func deviceTexts(devices []rt.Device) []string {
	out := make([]string, 0, len(devices))
	for _, d := range devices {
		out = append(out, d.Raw)
	}
	return out
}

// modelFileSize is the size of a GGUF on disk, and 0 when there is nothing to
// measure. 0 is also what an unreadable path gives, which is the honest answer for
// it: a file this app cannot stat is not one it should guess about, and the panel
// reads 0 as "no verdict" rather than as an empty model.
func modelFileSize(path string) int64 {
	if strings.TrimSpace(path) == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return 0
	}
	return info.Size()
}

// sortedRuntimeIDs lists the configured managed backends in a stable order, so the
// rows of the panel do not reshuffle between two status calls that found the same
// thing.
func sortedRuntimeIDs(settings config.Settings) []string {
	ids := make([]string, 0, len(settings.Runtime.Models))
	for id := range settings.Runtime.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// GetRuntimeStatus reports the managed llama.cpp runtime.
func (a *App) GetRuntimeStatus() (RuntimeStatus, error) {
	if err := a.ensure(); err != nil {
		return RuntimeStatus{}, err
	}
	settings := a.Settings()
	status := runtimeStatusBase(settings, goruntime.GOOS, goruntime.GOARCH)
	// Only a build of the selected source counts as installed, and the one named is
	// the build a start would run rather than merely the newest on disk: with a fork
	// build beside an upstream one, reporting the other source's build here would name
	// a launcher the app refuses to start, and reporting a newer directory than the
	// recorded build would name a llama.cpp the app does not use. managedLauncher is
	// the single answer to that question; see it for why.
	if tag, binary := a.managedLauncher(settings); tag != "" {
		status.Installed, status.Tag, status.Binary = true, tag, binary
	}
	if status.CustomBinary != "" {
		// A path the user gave counts as installed even when nothing was
		// downloaded: that is the whole point of the field.
		status.Installed = true
	}
	// The launcher in use, whichever of the two it is: a custom path replaces the
	// installed build rather than sitting beside it, so it is the one that answers.
	if status.Installed {
		binary := status.Binary
		if status.CustomBinary != "" {
			binary = status.CustomBinary
		}
		status.Version = a.versionFor(binary)
		// Which compute devices this build can use. A caller that sees an empty
		// list next to an installed launcher is looking at a model that will run
		// on the CPU, whether the build has no accelerator or its runtime is
		// missing.
		reading := a.devicesFor(binary)
		status.Devices, status.DevicesError = deviceTexts(reading.devices), reading.err
		status.DeviceFreeBytes = int64(rt.FreeDeviceMemoryMiB(reading.devices)) << 20
		if reading.missingLibs != nil {
			status.DevicesMissingLibs = reading.missingLibs
		}
	}

	// The last release lookup, reported rather than asked for: see RuntimeUpdate.
	// A user-supplied launcher is left out of it entirely — a build nobody here
	// installed is not one this app can offer to update.
	if status.CustomBinary == "" {
		status.Update = a.cachedUpdate()
	}

	// The weights have to fit before anything else about the model does, so the
	// file's size is measured against the free device memory — the one number this
	// app can read without loading a model, and one that understates the footprint
	// rather than overstating it, which is the cautious direction for a warning. It
	// is measured whether or not a device reported memory: a 0 in either number
	// means "unknown" to the panel, and a size that is only read when a GPU is
	// present would be unknown exactly on the machines that have none.
	for i := range status.Models {
		status.Models[i].ModelBytes = modelFileSize(status.Models[i].ModelPath)
	}

	a.installMu.Lock()
	status.Busy, status.Phase, status.Done, status.Total, status.Error =
		a.installBusy, a.installPhase, a.installDone, a.installTotal, a.installErr
	// A launcher the user supplied is not the one an install replaced, so there is
	// nothing to restart for in that case.
	status.RestartPending = a.installCompleted && status.CustomBinary == ""
	a.installMu.Unlock()

	// The MLX environment is the other install this app can own. It is reported on
	// every status call rather than only when its card is open, because the card is
	// what reads it and the card polls this method.
	status.MLXSupported = rt.MLXSupported(goruntime.GOOS, goruntime.GOARCH)
	status.MLXVersion = settings.Runtime.MLX.Version
	status.MLXServer = settings.Runtime.MLX.ServerPath
	if strings.TrimSpace(status.MLXServer) != "" {
		// The recorded version describes the environment this app made. A launcher
		// the user supplied is a different program, and this app has no verified way
		// to ask it (there is no `--version` contract to rely on), so saying nothing
		// is the honest answer rather than showing another environment's number.
		status.MLXVersion = ""
	}
	if python, ok := rt.MLXVenvPython(rt.MLXVenvDir(a.dataDir)); ok {
		status.MLXPython = python
	}
	// A supplied launcher counts only when it is really there: the field is a path the
	// user typed, and a typo must not make the card offer a Start that cannot work.
	if server := strings.TrimSpace(status.MLXServer); server != "" {
		if info, err := os.Stat(server); err == nil && !info.IsDir() {
			status.MLXInstalled = true
		}
	} else if status.MLXPython != "" {
		status.MLXInstalled = true
	}
	a.mlxMu.Lock()
	status.MLXBusy, status.MLXPhase, status.MLXError = a.mlxBusy, a.mlxPhase, a.mlxErr
	a.mlxMu.Unlock()

	// The live state is filled into the rows the base already laid out, rather
	// than appended a second time.
	for i := range status.Models {
		srv := a.existingServer(status.Models[i].BackendID)
		if srv == nil {
			continue
		}
		st := srv.Status()
		status.Models[i].State = st.State
		status.Models[i].Port = st.Port
		status.Models[i].URL = st.URL
		status.Models[i].Error = st.Error
		status.Models[i].Log = st.Log
		// Which file the running process is serving. The state is part of the answer
		// rather than an extra field for the panel to combine: the supervisor keeps the
		// model in its status after the process is gone — a stop and a failed start
		// both leave it there — so passing the path on regardless would put an "in use"
		// mark on a server that is not answering at all.
		if st.State == rt.StateRunning || st.State == rt.StateStarting {
			status.Models[i].LoadedModel = st.Model
		}
		// Only a reading that exists is passed on, and the device only when layers
		// really are on it — both decided by the runtime, not by the panel. A CPU-only
		// build reports every layer offloaded while naming no device at all, and a
		// `--gpu-layers 0` run names one while reporting none on it, so sending the id
		// regardless would put the decision in the UI, one comparison away from
		// claiming an accelerator that is not there.
		if st.Offload.Reported() {
			status.Models[i].OffloadLayers = st.Offload.Layers
			status.Models[i].OffloadTotal = st.Offload.Total
			if st.Offload.OnDevice() {
				status.Models[i].OffloadDevice = st.Offload.Device
			}
		}
	}
	return status, nil
}

// existingServer returns the supervisor of one backend, or nil. It is read under
// the lock because a status call runs while a server may be starting.
func (a *App) existingServer(backendID string) *rt.Server {
	a.serversMu.Lock()
	defer a.serversMu.Unlock()
	return a.servers[backendID]
}

// InstallLlamaRuntime fetches llama-server for one build family of one release
// source. It returns as soon as the download is under way — the archive is tens to
// hundreds of megabytes, and a bound method that blocked for minutes would freeze the
// window that called it. GetRuntimeStatus reports the progress and the outcome.
//
// Both are parameters rather than read from the stored settings because the pickers
// above the button edit a draft that has not been saved yet. Reading the stored value
// here made "pick Vulkan, press Download" fetch the previous build, which is exactly
// the bug where a user picks a GPU build and the model still runs on the CPU; with a
// source in the same draft it would be worse, because the wrong source's archive is a
// different llama.cpp that cannot load the model the source was chosen for.
func (a *App) InstallLlamaRuntime(accelerator, source string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	a.installMu.Lock()
	if a.installBusy {
		a.installMu.Unlock()
		return uimsg.Errorf("download.running")
	}
	a.installBusy, a.installPhase, a.installErr, a.installDone, a.installTotal = true, "lookup", "", 0, 0
	a.installMu.Unlock()
	stored := a.Settings()
	dist := resolvedDistribution(installDistribution(source, stored.Runtime.Distribution))
	go a.installRuntime(dist, installAccelerator(accelerator, stored.Runtime.Accelerator))
	return nil
}

// installAccelerator picks the family to fetch: the one the caller asked for when
// it names a family this app knows, otherwise what is stored. An empty or unknown
// answer must not become a download URL of its own, so it falls back.
func installAccelerator(picked, stored string) string {
	picked = strings.TrimSpace(picked)
	if config.IsAccelerator(picked) {
		return picked
	}
	return stored
}

// installDistribution picks the source to fetch from, by the same rule and for the
// same reason as the family above.
func installDistribution(picked, stored string) string {
	picked = strings.TrimSpace(picked)
	if config.IsDistribution(picked) {
		return picked
	}
	return stored
}

// InstallMLXRuntime creates the mlx-lm environment KiChat runs, or upgrades the
// package in the one it made before. It returns as soon as the work is under way:
// pip takes minutes, and GetRuntimeStatus reports the phase and the outcome.
//
// This is a separate environment from anything the user has: no system package is
// touched, no administrator password is asked for, and removing the directory is a
// complete uninstall.
func (a *App) InstallMLXRuntime() error {
	if err := a.ensure(); err != nil {
		return err
	}
	if !rt.MLXSupported(goruntime.GOOS, goruntime.GOARCH) {
		return uimsg.Errorf("runtime.mlxPlatform")
	}
	a.mlxMu.Lock()
	if a.mlxBusy {
		a.mlxMu.Unlock()
		return uimsg.Errorf("download.running")
	}
	a.mlxBusy, a.mlxPhase, a.mlxErr = true, "python", ""
	a.mlxMu.Unlock()
	go a.installMLX()
	return nil
}

// setMLXPhase is the installer's progress callback.
func (a *App) setMLXPhase(phase string) {
	a.mlxMu.Lock()
	a.mlxPhase = phase
	a.mlxMu.Unlock()
}

// installMLX is the install itself, on its own goroutine.
//
// The interpreter is looked for among the names a Mac actually has, and one older
// than the package supports is passed over rather than tried: pip would spend a
// minute resolving wheels and then fail with a version error whose cause is the
// interpreter, not the package.
func (a *App) installMLX() {
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()

	python := ""
	for _, candidate := range rt.MLXPythonCandidates() {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		major, minor, err := rt.MLXPythonVersion(ctx, path)
		if err == nil && (major > 3 || (major == 3 && minor >= 11)) {
			python = path
			break
		}
	}
	if python == "" {
		a.finishMLX(uimsg.Errorf("runtime.mlxPythonMissing", "python3"))
		return
	}

	version, err := rt.MLXInstall(ctx, python, rt.MLXVenvDir(a.dataDir), a.setMLXPhase)
	if err != nil {
		a.finishMLX(err)
		return
	}
	a.settingsMu.Lock()
	a.settings.Runtime.MLX.Version = version
	updated := a.settings.Clone()
	a.settingsMu.Unlock()
	if err := a.sanitize(updated).Save(a.store); err != nil {
		a.finishMLX(err)
		return
	}
	runtime.LogInfof(a.ctx, "mlx-lm %s installiert: %s", version, rt.MLXVenvDir(a.dataDir))
	a.finishMLX(nil)
}

// finishMLX ends an install, successfully or not.
func (a *App) finishMLX(err error) {
	a.mlxMu.Lock()
	a.mlxBusy = false
	if err != nil {
		a.mlxErr = err.Error()
		a.mlxPhase = ""
	} else {
		a.mlxErr = ""
		a.mlxPhase = "done"
	}
	a.mlxMu.Unlock()
}

// PickMLXModelFolder asks for an MLX snapshot directory: a folder holding
// config.json and the weights, which is what `mlx_lm.server --model` takes.
func (a *App) PickMLXModelFolder(title string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: title})
}

// installRuntime is the download itself, on its own goroutine.
func (a *App) installRuntime(dist rt.Distribution, accelerator string) {
	asset, err := a.installer.Latest(a.ctx, dist, goruntime.GOOS, goruntime.GOARCH, accelerator)
	if err != nil {
		a.finishInstall(err)
		return
	}
	binary, err := a.installer.Install(a.ctx, asset)
	if err != nil {
		a.finishInstall(err)
		return
	}

	// The build that was installed is written down, so the next start finds it
	// through the settings rather than by picking whatever is newest. The folder
	// name carries the family, since two families are two installations. The
	// accelerator is written down with it: it now names the build that is really on
	// disk, which is what the picker has to show. So is the source, because the
	// installed directory can only be found again by the source whose tag shape
	// reads its name.
	a.settingsMu.Lock()
	a.settings.Runtime.Distribution = dist.ID
	a.settings.Runtime.Tag = asset.Dir()
	a.settings.Runtime.Accelerator = asset.Variant
	// A clone, not the struct: what follows marshals this value, and the maps inside it
	// are written by other goroutines under settingsMu (config.Settings.Clone).
	updated := a.settings.Clone()
	a.settingsMu.Unlock()
	if err := a.sanitize(updated).Save(a.store); err != nil {
		a.finishInstall(fmt.Errorf("Installation speichern: %w", err))
		return
	}
	runtime.LogInfof(a.ctx, "%s llama-server %s (%s) installiert: %s", dist.ID, asset.Dir(), asset.Variant, binary)
	// What was just fetched is the newest published build by definition, so the
	// cached reading is replaced rather than left saying "update available" about
	// the build that is now on disk and starting.
	a.setUpdate(updateReadingOf(dist, asset, dist.TagOfDir(asset.Dir())))
	a.rebuildServices(updated)
	a.finishInstall(nil)
}

// recordInstallProgress is the installer's progress callback.
func (a *App) recordInstallProgress(p rt.Progress) {
	a.installMu.Lock()
	a.installPhase = p.Phase
	a.installDone, a.installTotal = p.Done, p.Total
	a.installMu.Unlock()
}

// finishInstall ends a download, successfully or not. The tag the caller knew is not
// part of the record — the settings carry it, and this only closes the progress the UI
// polls.
func (a *App) finishInstall(err error) {
	a.installMu.Lock()
	a.installBusy = false
	if err != nil {
		a.installErr = err.Error()
		a.installPhase = ""
	} else {
		a.installErr = ""
		a.installPhase = "done"
		a.installCompleted = true
	}
	a.installMu.Unlock()
}

// installCompletedDuringSession reports whether a build was downloaded in this
// session, which is what a running server has to be restarted for.
func (a *App) installCompletedDuringSession() bool {
	a.installMu.Lock()
	defer a.installMu.Unlock()
	return a.installCompleted
}

// clearInstallCompleted is what a successful managed start calls: the server that
// just came up runs the launcher that is on disk now.
func (a *App) clearInstallCompleted() {
	a.installMu.Lock()
	a.installCompleted = false
	a.installMu.Unlock()
}

// PickManagedModelFile asks for the GGUF to run. The title is passed in rather
// than chosen here, so it follows the UI language like every other string.
func (a *App) PickManagedModelFile(title string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: title,
		Filters: []runtime.FileFilter{
			{DisplayName: "GGUF", Pattern: "*.gguf"},
		},
	})
}

// ModelFolders lists the folders a model may already be in, most likely first,
// each saying whether it is there. A user who already has LM Studio has models on
// disk, and re-downloading a 5 GB file to run it in KiChat instead would be an
// absurd answer to that.
//
// The folders the user has picked come first, in the order they were picked: they
// are the ones asked for by name, whatever LM Studio does with its own, and there is
// more than one of them because one drive rarely holds everything. A folder that is
// only in `modelDir` — a blob written before this list existed, or a path typed
// straight into the text box — is offered too, so no stored settings loses the folder
// it names. A folder the user picked that is also one of LM Studio's stays one entry,
// under the name the user gave it.
func (a *App) ModelFolders() []rt.ModelFolder {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	settings := a.Settings()
	dirs := append([]string{}, settings.Runtime.ModelDirs...)
	if chosen := strings.TrimSpace(settings.Runtime.ModelDir); chosen != "" {
		seen := false
		for _, dir := range dirs {
			seen = seen || strings.TrimSpace(dir) == chosen
		}
		if !seen {
			dirs = append([]string{chosen}, dirs...)
		}
	}
	folders := make([]rt.ModelFolder, 0, len(dirs)+4)
	listed := make(map[string]bool, len(dirs)+4)
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" || listed[dir] {
			continue
		}
		listed[dir] = true
		folders = append(folders, rt.ModelFolder{Path: dir, Kind: rt.FolderCustom, Exists: rt.IsDir(dir)})
	}
	for _, folder := range rt.LMStudioFolders(home) {
		if listed[folder.Path] {
			continue
		}
		listed[folder.Path] = true
		folders = append(folders, folder)
	}
	return folders
}

// ScanModelFolder lists the GGUF files in one folder. An empty dir means "the one
// the user chose, or LM Studio's if they never chose one", which is what the
// panel asks for when it opens.
func (a *App) ScanModelFolder(dir string) (rt.ModelFolderScan, error) {
	if err := a.ensure(); err != nil {
		return rt.ModelFolderScan{Files: []rt.ModelFile{}}, err
	}
	if strings.TrimSpace(dir) == "" {
		dir = a.defaultModelDir()
	}
	if dir == "" {
		// A scan with no files is an empty list, not a null one: the panel reads
		// `files.length` the moment the answer arrives.
		return rt.ModelFolderScan{Files: []rt.ModelFile{}}, nil
	}
	return rt.ScanModelFolder(dir)
}

// PickModelFolder asks for a folder to look for models in.
func (a *App) PickModelFolder(title string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: title})
}

// defaultModelDir is the folder to look in when nothing was chosen: the first
// remembered folder that is actually there, and failing that the first place LM
// Studio keeps models.
//
// A remembered folder wins because the row offers it: it is the head of the chips,
// and it is what the empty field's placeholder names. Falling back to LM Studio's
// directory instead would make the panel say one folder and scan another, which is
// the one way an empty field can lie about which models are being listed.
func (a *App) defaultModelDir() string {
	settings := a.Settings()
	for _, dir := range settings.Runtime.ModelDirs {
		if rt.IsDir(dir) {
			return dir
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, folder := range rt.LMStudioFolders(home) {
		if folder.Exists {
			return folder.Path
		}
	}
	return ""
}

// SetManagedModel saves what one backend should run. An empty model path switches
// the entry off rather than leaving a half-configured server behind.
func (a *App) SetManagedModel(backendID string, mm config.ManagedModel) error {
	if err := a.ensure(); err != nil {
		return err
	}
	backendID = strings.TrimSpace(backendID)
	if backendID == "" {
		return uimsg.Errorf("model.noBackend")
	}
	mm.ModelPath = strings.TrimSpace(mm.ModelPath)
	mm.MMProj = strings.TrimSpace(mm.MMProj)
	mm.ExtraArgs = strings.TrimSpace(mm.ExtraArgs)
	// Repaired here as well as on load: a start builds its command line from this
	// value, and a width the server refuses is a model that exits rather than a
	// setting that looks odd.
	mm.MLX = config.NormaliseMLX(mm.MLX)
	if mm.CtxSize != 0 && mm.CtxSize < config.MinCtxSize {
		mm.CtxSize = config.MinCtxSize
	}
	if mm.ModelPath == "" {
		mm.Enabled = false
	}

	// A model that was just switched off has to stop running: leaving it up would
	// keep a multi-gigabyte model in memory with no row that offers to stop it.
	if !mm.Enabled {
		if srv := a.existingServer(backendID); srv != nil {
			if err := srv.Stop(); err != nil {
				return err
			}
		}
	}

	a.settingsMu.Lock()
	if a.settings.Runtime.Models == nil {
		a.settings.Runtime.Models = map[string]config.ManagedModel{}
	}
	a.settings.Runtime.Models[backendID] = mm
	// Cloned under the lock, because everything below reads or marshals it outside: a
	// shallow copy here is the reported race — this map write against the settings save a
	// managed server's state callback runs, which marshals the same map.
	updated := a.settings.Clone()
	a.settingsMu.Unlock()

	if err := a.sanitize(updated).Save(a.store); err != nil {
		return err
	}
	a.rebuildServices(updated)
	// Saving a model does not start it. It used to, and that turned every edited
	// field — the context size, the extra arguments — into a restart of a model
	// that may have taken minutes to load. Starting is its own call, so the UI
	// says when it wants one.
	return nil
}

// StartManagedModel starts the managed server of one backend.
func (a *App) StartManagedModel(backendID string) error {
	return a.startManagedModel(backendID)
}

// StartManagedModelWith starts the managed server of one backend on the entry it is
// given, which is how the chat's model picker starts a local model: a GGUF that is
// already on disk should be one click from the conversation, and walking to
// Settings → Backends to name the file there and then coming back to choose the model
// is the walk this exists to save.
//
// The entry is the stored one with another `modelPath` — the context size, the flash
// attention, the KV cache types, the offload mode and the extra arguments are the
// user's configuration of that entry and a pick made elsewhere says nothing about them
// — or, for an entry that does not exist yet, the defaults the panel shows for a new
// one, so a first start from the picker lands in the state a first start from Settings
// does. `enabled` is taken as given, and the picker passes what is stored: an enabled
// entry is loaded on every app start, which is a decision the checkbox makes, not one a
// start should make on the user's behalf.
//
// Two things are checked before anything is written, because both are cheap here and
// expensive later: the entry must be of the kind KiChat runs itself — any other kind is
// addressed over HTTP and has no command line to build — and the file must exist, since
// a stored path that is not there fails every start after this one, including the
// automatic one at launch.
func (a *App) StartManagedModelWith(backendID string, mm config.ManagedModel) error {
	if err := a.ensure(); err != nil {
		return err
	}
	backendID = strings.TrimSpace(backendID)
	mm.ModelPath = strings.TrimSpace(mm.ModelPath)
	if backendID == "" || mm.ModelPath == "" {
		return uimsg.Errorf("model.noneSelected")
	}
	var entry llm.BackendConfig
	found := false
	for _, b := range a.Settings().Backends {
		if b.ID == backendID {
			entry, found = b, true
			break
		}
	}
	if !found {
		return fmt.Errorf("unbekanntes Backend: %s", backendID)
	}
	// The kinds whose server this app starts, stops and configures itself:
	// llama.cpp everywhere, and MLX on Apple silicon.
	if !llm.ManagedKind(entry.Kind) {
		return uimsg.Errorf("runtime.notManaged", entry.Name)
	}
	// The two shapes differ: a llama.cpp model is a file, an MLX model is the
	// snapshot directory holding config.json and the weights. Checking the shape
	// here is what keeps a GGUF out of an MLX start and a folder out of a
	// llama-server one, each of which fails later and less clearly.
	if info, err := os.Stat(mm.ModelPath); err != nil || (entry.Kind == "mlx" && !info.IsDir()) || (entry.Kind != "mlx" && info.IsDir()) {
		return uimsg.Errorf("model.notFound", mm.ModelPath)
	}

	// Through SetManagedModel rather than around it, so the one place that writes a
	// managed entry — its clamps, its sanitising save, the service rebuild that gives
	// the entry its supervisor — stays one place.
	if err := a.SetManagedModel(backendID, mm); err != nil {
		return err
	}
	return a.startManagedModel(backendID)
}

// StartImageServer starts the managed picture server, or restarts it when it is already up.
//
// There is one call rather than a start and a restart because sd-server loads one pipeline at
// startup and has no documented way to replace it: a different model is a different process,
// so pressing start on a running server means "load this now" either way.
func (a *App) StartImageServer() error {
	return a.startImageServer()
}

// StopImageServer stops it. The model's memory is released here and nowhere else, which is
// why the panel's row keeps a Stop button in front of the user rather than an idle timer
// making that decision for them.
func (a *App) StopImageServer() error {
	return a.stopImageServer()
}

// StopManagedModel stops it.
func (a *App) StopManagedModel(backendID string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	srv := a.existingServer(backendID)
	if srv == nil {
		return nil
	}
	if err := srv.Stop(); err != nil {
		return err
	}
	// A row that is no longer served must stop claiming the window it was loaded
	// with; the next listing reads nothing and the model reads as stopped.
	a.invalidateModels()
	return nil
}
