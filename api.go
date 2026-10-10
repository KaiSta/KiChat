package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"kichat/internal/chat"
	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/ingest"
	"kichat/internal/llm"
	"kichat/internal/research"
	rt "kichat/internal/runtime"
	"kichat/internal/secret"
	"kichat/internal/store"
	"kichat/internal/version"
)

// --- settings --------------------------------------------------------------

// GetSettings returns the current settings with every secret blanked: the Brave
// key and each backend's API key are mirrored only as shape/booleans and never
// cross the bridge.
func (a *App) GetSettings() (config.Settings, error) {
	if err := a.ensure(); err != nil {
		return config.Settings{}, err
	}
	return a.sanitize(a.Settings()), nil
}

// SaveSettings persists settings and rebuilds the services that depend on them.
//
// It returns what was stored instead of nothing because the built-in system
// prompts follow the interface language, and only the backend knows which of the
// prompts in the payload are still untouched: handing the caller its own draft
// back would leave the prompt textarea showing the previous language until the
// next start.
//
// Backend API keys are secrets. Each backend's key is stored under its own name
// in the secret store, the same way the Brave key is, and neither the blob nor
// the bridge ever carries the secret. SaveSettings commits the draft's keys to
// the secret store and reads the real stored values back into the snapshot the
// registry is built from, so a key typed in Settings reaches the provider. The
// copy it persists is sanitised; the keys stay only in memory and in the store.
func (a *App) SaveSettings(s config.Settings) (config.Settings, error) {
	if err := a.ensure(); err != nil {
		return config.Settings{}, err
	}
	// The draft only ever carries a boolean about the Brave key (GetSettings
	// blanks the secret), so an empty BraveAPIKey means "leave the stored key
	// alone".
	s.Search.BraveAPIKey = a.Settings().Search.BraveAPIKey
	// Moving the untouched prompts to the new language, and repairing an empty
	// default prompt, happen here as well as on load: clearing the textarea and
	// switching the language in the same save must not leave either undone.
	s.LocalisePrompts()
	// A save is also where "look again" happens: the launcher may have been
	// replaced since its version was read.
	a.forgetVersions()
	// A Save must not hand a running managed model back an address it no longer
	// listens on; see adoptManagedAddresses.
	s = a.adoptManagedAddresses(s)
	// Commit each backend key through the secret store, then reconcile the
	// snapshot from what is actually stored so the registry sees the real key on
	// the next rebuild rather than the draft's blank.
	s, err := a.reconcileBackendKeys(s)
	if err != nil {
		return s, err
	}
	// Persist the sanitised copy: the secrets live in the secret store, never in
	// the settings blob. The in-memory snapshot keeps the real keys so the
	// rebuild below can authenticate.
	if err := a.sanitize(s).Save(a.store); err != nil {
		return s, err
	}
	a.settingsMu.Lock()
	a.settings = s
	a.settingsMu.Unlock()
	a.rebuildServices(s)
	return a.sanitize(s), nil
}

// reconcileBackendKeys commits each backend's draft key to the secret store and
// reads the real stored value back into the snapshot. A non-empty draft key
// overwrites what is stored, the same way SetBackendAPIKey does; an empty one
// leaves the stored key alone — it never clears a key the user set earlier, and
// it never rewrites over one with a blank.
func (a *App) reconcileBackendKeys(s config.Settings) (config.Settings, error) {
	for i := range s.Backends {
		b := &s.Backends[i]
		name := llm.BackendAPIKeyName(b.ID)
		if want := strings.TrimSpace(b.APIKey); want != "" {
			if err := a.secrets.Set(name, want); err != nil {
				return s, err
			}
			b.APIKey = want
			continue
		}
		// Empty draft: keep whatever is on disk, which may be nothing.
		if stored, ok := a.secrets.Get(name); ok {
			b.APIKey = stored
		} else {
			b.APIKey = ""
		}
	}
	return s, nil
}

// sanitize returns the copy that the bridge is allowed to hand back and that the
// settings blob is allowed to store. Secrets never cross it. The backend slice is
// copied before it is blanked, so the caller's snapshot — the one the registry is
// built from — keeps its real keys.
func (a *App) sanitize(s config.Settings) config.Settings {
	backends := make([]llm.BackendConfig, len(s.Backends))
	copy(backends, s.Backends)
	for i := range backends {
		backends[i].APIKey = ""
	}
	s.Backends = backends
	s.Search.BraveAPIKey = ""
	return s
}

// SecretStatus reports how secrets are stored.
type SecretStatus struct {
	Backend     string `json:"backend"`
	Keyring     bool   `json:"keyring"`
	BraveKeySet bool   `json:"braveKeySet"`
	Warning     string `json:"warning,omitempty"`
}

// GetSecretStatus reports where the Brave key is kept.
func (a *App) GetSecretStatus() (SecretStatus, error) {
	if err := a.ensure(); err != nil {
		return SecretStatus{}, err
	}
	status := SecretStatus{
		Backend: a.secrets.Backend(),
		Keyring: a.secrets.KeyringAvailable(),
	}
	_, status.BraveKeySet = a.secrets.Get("brave")
	if !status.Keyring {
		status.Warning = "Kein Schlüsselbund verfügbar (libsecret fehlt). " +
			"Der Key wird lokal verschlüsselt gespeichert — das schützt vor versehentlicher " +
			"Offenlegung, aber nicht gegen einen Angreifer mit Zugriff auf dieses Benutzerkonto."
	}
	return status, nil
}

// AppInfo is the running build's identity, shown in Settings so a user can tell
// which version they are on and what to quote in a bug report.
type AppInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// GetAppInfo returns the build identity.
//
// Unlike every other bound method it does not call ensure(): the version has to
// answer even when initialisation failed, because that is exactly the moment
// someone wants to know what they are running.
func (a *App) GetAppInfo() AppInfo {
	return AppInfo{
		Version:   version.Version,
		Commit:    version.Commit,
		Date:      version.Date,
		GoVersion: goruntime.Version(),
		Platform:  goruntime.GOOS + "/" + goruntime.GOARCH,
	}
}

// setBackendAPIKey stores one backend's key in the secret store and rebuilds the
// registry so the change takes effect at once. The secret store owns the key, so
// it never reaches the settings blob or the bridge.
func (a *App) setBackendAPIKey(backendID, key string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	if err := a.secrets.Set(llm.BackendAPIKeyName(backendID), key); err != nil {
		return err
	}
	a.settingsMu.Lock()
	for i := range a.settings.Backends {
		if a.settings.Backends[i].ID == backendID {
			a.settings.Backends[i].APIKey = key
		}
	}
	a.settingsMu.Unlock()
	a.rebuildServices(a.Settings())
	return nil
}

// SetBackendAPIKey stores an API key for one backend. Empty clears it.
func (a *App) SetBackendAPIKey(backendID, apiKey string) error {
	return a.setBackendAPIKey(backendID, apiKey)
}

// ManagedStatus is one model KiChat runs itself, as the UI reads it.
type ManagedStatus struct {
	BackendID string   `json:"backendId"`
	Enabled   bool     `json:"enabled"`
	ModelPath string   `json:"modelPath"`
	CtxSize   int      `json:"ctxSize"`
	ExtraArgs string   `json:"extraArgs"`
	State     string   `json:"state"`
	Port      int      `json:"port"`
	URL       string   `json:"url"`
	Error     string   `json:"error,omitempty"`
	Log       []string `json:"log,omitempty"`
	// ModelBytes is the size of the GGUF on disk, 0 when it cannot be read. The
	// panel measures it against DeviceFreeBytes, because it is the one part of a
	// model's memory use this app can know without loading it — the weights have to
	// fit before the KV cache is even considered.
	ModelBytes int64 `json:"modelBytes"`
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
	// Variants are the build families this platform actually publishes, in the
	// order to offer them. The picker is filled from here rather than from a list
	// of its own, so it cannot offer a build that does not exist — ROCm and SYCL
	// are x64 only, and macOS publishes a single Metal-enabled build per arch.
	Variants []string `json:"variants"`
	// FlashAttentionModes and CacheTypes are the values a managed model may carry
	// for `--flash-attn` and `--cache-type-k/-v`. The pickers are filled from here
	// for the same reason as Variants: a value the backend cannot pass must not be
	// offered.
	FlashAttentionModes []string `json:"flashAttentionModes"`
	CacheTypes          []string `json:"cacheTypes"`
	// Update is the last look at llama.cpp's releases, if one has been made. It is
	// reported rather than asked for here: see RuntimeUpdate.
	Update RuntimeUpdate `json:"update"`
	// RestartPending means a build was downloaded during this session and no managed
	// model has been started since, so a server that is running now is still the old
	// build. It is what makes the "restart the model" line survive the update it
	// refers to; a managed start clears it.
	RestartPending bool `json:"restartPending"`
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

	// The family the app installs for: a draft the picker has not saved is not a
	// build that is on disk to compare against. installAccelerator falls back for an
	// empty or unknown value.
	family := installAccelerator("", settings.Runtime.Accelerator)
	installed := a.installedBuildTag()

	ctx, cancel := context.WithTimeout(context.Background(), updateLookupTimeout)
	defer cancel()
	asset, err := a.installer.Latest(ctx, goruntime.GOOS, goruntime.GOARCH, family)
	if err != nil {
		reading := RuntimeUpdate{Checked: true, Family: family, Installed: installed, Error: err.Error()}
		a.setUpdate(reading)
		return reading, nil
	}
	reading := updateReadingOf(asset, installed)
	a.setUpdate(reading)
	return reading, nil
}

// updateReadingOf is one release lookup as the panel shows it: the two tags, what the
// download would cost, and whether the published build is newer than the one on disk.
// The install path calls it too, where the two tags are equal by construction — which
// is what makes an update marker disappear the moment the update is installed.
func updateReadingOf(asset rt.Asset, installed string) RuntimeUpdate {
	size := asset.Size
	if asset.Companion != nil {
		size += asset.Companion.Size
	}
	return RuntimeUpdate{
		Checked:   true,
		Family:    asset.Variant,
		Installed: installed,
		Latest:    asset.Tag,
		Size:      size,
		Available: newerBuild(asset.Tag, installed),
	}
}

// installedBuildTag is the build tag of the launcher on disk, which is the side of
// the comparison the panel names as installed. It is the same directory
// GetRuntimeStatus reports as the installed tag.
func (a *App) installedBuildTag() string {
	tag, _ := a.installer.Find()
	return buildTagOf(tag)
}

// buildTagOf reduces an installation directory name to the build it belongs to.
// rt.Asset.Dir names directories `b11535`, `b11535-vulkan` and
// `b11535-vulkan-10.0`, and a build tag carries no `-` of its own.
func buildTagOf(dir string) string {
	if i := strings.IndexByte(dir, '-'); i >= 0 {
		return dir[:i]
	}
	return dir
}

// newerBuild reports whether the published tag is newer than the installed one.
//
// The two are compared as numbers and not as text: llama.cpp's tags are `b11534`
// today, and `b1000` sorts below `b999` as text the day the counter passes a digit
// boundary — an update offered to a user who is already past it. A tag that carries
// no number falls back to "different means newer": the panel prints both tags, so the
// user sees what they would get, and refusing an update over an unusual tag would
// hide the real ones.
func newerBuild(latest, installed string) bool {
	if installed == "" {
		return false
	}
	l, lok := buildNumber(latest)
	i, iok := buildNumber(installed)
	if lok && iok {
		return l > i
	}
	return latest != installed
}

// buildNumber reads the number a build tag carries (`b11541` becomes 11541).
func buildNumber(tag string) (int, bool) {
	digits := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(tag), "b"), "B")
	if digits == "" {
		return 0, false
	}
	n := 0
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
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
		return fmt.Errorf("%s läuft gerade — Modell zuerst stoppen", name)
	}
	if inUse[name] {
		return fmt.Errorf("%s ist der Build, den KiChat startet — erst aktualisieren oder einen anderen Build wählen", name)
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
	status := RuntimeStatus{
		Accelerator:         settings.Runtime.Accelerator,
		ReadySeconds:        settings.Runtime.ReadySeconds,
		CustomBinary:        settings.Runtime.BinaryPath,
		Variants:            []string{},
		FlashAttentionModes: config.FlashAttentionModes(),
		CacheTypes:          config.CacheTypes(),
		Devices:             []string{},
		DevicesMissingLibs:  []string{},
		Models:              []ManagedStatus{},
	}
	if variants := rt.PublishedAccelerators(goos, goarch); len(variants) > 0 {
		status.Variants = variants
	}
	for _, id := range sortedRuntimeIDs(settings) {
		mm := settings.Runtime.Models[id]
		status.Models = append(status.Models, ManagedStatus{
			BackendID: id, Enabled: mm.Enabled, ModelPath: mm.ModelPath,
			CtxSize: mm.CtxSize, ExtraArgs: mm.ExtraArgs, State: rt.StateStopped,
		})
	}
	return status
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
	if tag, binary := a.installer.Find(); tag != "" {
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

// InstallLlamaRuntime fetches llama-server for one build family. It returns as
// soon as the download is under way — the archive is tens to hundreds of megabytes,
// and a bound method that blocked for minutes would freeze the window that called
// it. GetRuntimeStatus reports the progress and the outcome.
//
// The family is a parameter rather than read from the stored settings because the
// picker above the button edits a draft that has not been saved yet. Reading the
// stored value here made "pick Vulkan, press Download" fetch the previous build,
// which is exactly the bug where a user picks a GPU build and the model still runs
// on the CPU.
func (a *App) InstallLlamaRuntime(accelerator string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	a.installMu.Lock()
	if a.installBusy {
		a.installMu.Unlock()
		return errors.New("ein Download läuft bereits")
	}
	a.installBusy, a.installPhase, a.installErr, a.installDone, a.installTotal = true, "lookup", "", 0, 0
	a.installMu.Unlock()
	go a.installRuntime(installAccelerator(accelerator, a.Settings().Runtime.Accelerator))
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

// installRuntime is the download itself, on its own goroutine.
func (a *App) installRuntime(accelerator string) {
	asset, err := a.installer.Latest(a.ctx, goruntime.GOOS, goruntime.GOARCH, accelerator)
	if err != nil {
		a.finishInstall("", err)
		return
	}
	binary, err := a.installer.Install(a.ctx, asset)
	if err != nil {
		a.finishInstall("", err)
		return
	}

	// The build that was installed is written down, so the next start finds it
	// through the settings rather than by picking whatever is newest. The folder
	// name carries the family, since two families are two installations. The
	// accelerator is written down with it: it now names the build that is really on
	// disk, which is what the picker has to show.
	a.settingsMu.Lock()
	a.settings.Runtime.Tag = asset.Dir()
	a.settings.Runtime.Accelerator = asset.Variant
	updated := a.settings
	a.settingsMu.Unlock()
	if err := a.sanitize(updated).Save(a.store); err != nil {
		a.finishInstall(asset.Dir(), fmt.Errorf("Installation speichern: %w", err))
		return
	}
	runtime.LogInfof(a.ctx, "llama-server %s (%s) installiert: %s", asset.Dir(), asset.Variant, binary)
	// What was just fetched is the newest published build by definition, so the
	// cached reading is replaced rather than left saying "update available" about
	// the build that is now on disk and starting.
	a.setUpdate(updateReadingOf(asset, buildTagOf(asset.Dir())))
	a.rebuildServices(updated)
	a.finishInstall(asset.Dir(), nil)
}

// recordInstallProgress is the installer's progress callback.
func (a *App) recordInstallProgress(p rt.Progress) {
	a.installMu.Lock()
	a.installPhase = p.Phase
	a.installDone, a.installTotal = p.Done, p.Total
	a.installMu.Unlock()
}

func (a *App) finishInstall(tag string, err error) {
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
func (a *App) ModelFolders() []rt.ModelFolder {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	folders := rt.LMStudioFolders(home)
	if chosen := strings.TrimSpace(a.Settings().Runtime.ModelDir); chosen != "" {
		// The folder the user picked is offered first: it is the one they asked
		// for, whatever LM Studio does with its own.
		folders = append([]rt.ModelFolder{{Path: chosen, Kind: rt.FolderCustom, Exists: rt.IsDir(chosen)}}, folders...)
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
// place LM Studio keeps models that is actually there.
func (a *App) defaultModelDir() string {
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
		return errors.New("kein Backend angegeben")
	}
	mm.ModelPath = strings.TrimSpace(mm.ModelPath)
	mm.ExtraArgs = strings.TrimSpace(mm.ExtraArgs)
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
	updated := a.settings
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

// StopManagedModel stops it.
func (a *App) StopManagedModel(backendID string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	srv := a.existingServer(backendID)
	if srv == nil {
		return nil
	}
	return srv.Stop()
}

// SetBraveKey stores the Brave Search API key. It is kept separate from
// setBackendAPIKey because Brave is a search provider, not a chat backend: its key
// lives under the plain "brave" name the rest of the app reads, not under a
// backend-qualified one.
func (a *App) SetBraveKey(key string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	if err := a.secrets.Set("brave", key); err != nil {
		return err
	}
	a.settingsMu.Lock()
	a.settings.Search.BraveAPIKey = key
	a.settingsMu.Unlock()
	a.rebuildServices(a.Settings())
	return nil
}

// TestBraveKey validates a key with a single live query.
func (a *App) TestBraveKey(key string) (research.SearchOutcome, error) {
	if err := a.ensure(); err != nil {
		return research.SearchOutcome{}, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		if stored, ok := a.secrets.Get("brave"); ok {
			key = stored
		}
	}
	if key == "" {
		return research.SearchOutcome{}, errors.New("kein Brave API-Key angegeben")
	}
	provider := research.NewBraveProvider(key, "")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	results, err := provider.Search(ctx, research.SearchRequest{
		Query: "test", Count: 1, Country: "DE", SearchLang: "de",
	})
	if err != nil {
		return research.SearchOutcome{}, err
	}
	return research.SearchOutcome{Results: results, Provider: "brave"}, nil
}

// --- backends and models ---------------------------------------------------

// GetHealth probes every configured backend.
func (a *App) GetHealth() ([]llm.HealthStatus, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	return a.registry.Health(ctx), nil
}

// ListModels returns the merged cross-backend model catalogue.
func (a *App) ListModels(forceRefresh bool) ([]llm.ModelInfo, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if forceRefresh {
		a.registry.Invalidate()
	}
	return a.visibleModels(ctx), nil
}

// SetModelOverride stores a per-model context window or capability correction.
func (a *App) SetModelOverride(backend, model string, ov llm.ModelOverride) error {
	if err := a.ensure(); err != nil {
		return err
	}
	a.settingsMu.Lock()
	if a.settings.ModelOverrides == nil {
		a.settings.ModelOverrides = map[string]llm.ModelOverride{}
	}
	key := backend + "/" + model
	if ov.ContextLimit == 0 && len(ov.Capabilities) == 0 {
		delete(a.settings.ModelOverrides, key)
	} else {
		a.settings.ModelOverrides[key] = ov
	}
	settings := a.settings
	a.settingsMu.Unlock()

	if err := a.store.PutModelOverride(backend, model, ov); err != nil {
		return err
	}
	a.registry.Invalidate()
	return settings.Save(a.store)
}

// --- prompt presets --------------------------------------------------------

// PromptVariable describes a template variable available in system prompts.
//
// Key is the i18n key the settings view translates and Description is the
// untranslated fallback, the same pair a built-in preset name uses: the list sits
// directly under the prompt editor, so leaving it in one language made the screen
// read as English above and German below.
type PromptVariable struct {
	Name        string `json:"name"`
	Key         string `json:"key"`
	Description string `json:"description"`
}

// GetPromptVariables lists the supported template variables.
func (a *App) GetPromptVariables() []PromptVariable {
	return []PromptVariable{
		{Name: "datetime", Key: "promptvar.datetime", Description: "Aktuelles Datum, Uhrzeit und Wochentag"},
		{Name: "date", Key: "promptvar.date", Description: "Nur das Datum (TT.MM.JJJJ)"},
		{Name: "time", Key: "promptvar.time", Description: "Nur die Uhrzeit (HH:MM)"},
		{Name: "weekday", Key: "promptvar.weekday", Description: "Wochentag ausgeschrieben"},
		{Name: "location", Key: "promptvar.location", Description: "Der in den Einstellungen hinterlegte Standort"},
		{Name: "model", Key: "promptvar.model", Description: "Das aktuell gewählte Modell"},
		{Name: "backend", Key: "promptvar.backend", Description: "Das aktuelle Backend (LM Studio, Ollama, …)"},
		{Name: "context_limit", Key: "promptvar.context_limit", Description: "Größe des Kontextfensters in Tokens"},
		{Name: "context_used", Key: "promptvar.context_used", Description: "Zuletzt gemessene Kontextbelegung"},
		{Name: "tool_list", Key: "promptvar.tool_list", Description: "Liste der verfügbaren Tools"},
	}
}

// PreviewSystemPrompt renders a prompt template with live values.
func (a *App) PreviewSystemPrompt(tmpl string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	settings := a.Settings()
	models := a.visibleModels(context.Background())
	model := ""
	if len(models) > 0 {
		model = models[0].ID
	}
	return chat.PreviewPrompt(tmpl, settings.Location, model), nil
}

// UnresolvedPromptVars reports template variables that do not exist.
func (a *App) UnresolvedPromptVars(tmpl string) []string {
	return chat.UnresolvedVars(tmpl)
}

// --- compaction ------------------------------------------------------------

// CompactRequest is the manual compaction payload.
type CompactRequest struct {
	ChatID  string  `json:"chatId"`
	Focus   string  `json:"focus"`
	Target  float64 `json:"target"`
	FromSeq int     `json:"fromSeq"`
	ToSeq   int     `json:"toSeq"`
}

// CompactNow runs a manual compaction. It returns what the run did rather than
// only an error, because "there was nothing to fold" is a normal answer to a
// manual request and the UI has to be able to say so.
func (a *App) CompactNow(req CompactRequest) (chat.CompactResult, error) {
	if err := a.ensure(); err != nil {
		return chat.CompactResult{}, err
	}
	// A compaction reads a large transcript and writes a summary; on a local model
	// running on CPU that is minutes of work, and five of them was not enough — the
	// run was still producing tokens when the deadline cut it off, reported as
	// "context deadline exceeded". The output is bounded (see compactWith), so this
	// is a backstop against a stuck backend, not a budget the work has to fit in.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	result, err := a.engine.Compact(ctx, req.ChatID, chat.CompactOptions{
		Force:   true,
		Focus:   req.Focus,
		Target:  req.Target,
		FromSeq: req.FromSeq,
		ToSeq:   req.ToSeq,
	})
	if err != nil {
		return chat.CompactResult{}, err
	}
	if result.Created {
		a.emit(events.ChatsChanged, map[string]any{})
	}
	return result, nil
}

// ListCompactions returns a chat's compaction history.
func (a *App) ListCompactions(chatID string) ([]store.Compaction, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	return a.store.ListCompactions(chatID)
}

// DeleteCompaction removes a compaction, restoring the folded messages to the
// live context. The original messages were never deleted, so this is complete.
//
// Any compaction that was summarised on top of this one is dropped with it; see
// DeleteCompactionsFrom for why that is not optional. The measurement goes too,
// because the prompt has changed shape and the old count no longer describes it.
func (a *App) DeleteCompaction(chatID, compactionID string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	compactions, err := a.store.ListCompactions(chatID)
	if err != nil {
		return err
	}
	target, found := 0, false
	for _, c := range compactions {
		if c.ID == compactionID {
			target, found = c.ToSeq, true
			break
		}
	}
	if !found {
		return fmt.Errorf("Kompaktierung %q existiert nicht mehr", compactionID)
	}
	if err := a.store.DeleteCompactionsFrom(chatID, target); err != nil {
		return err
	}
	a.engine.InvalidateMeasurement(chatID)
	return nil
}

// --- attachments and documents --------------------------------------------

// AttachmentInput describes a file to add.
type AttachmentInput struct {
	ChatID    string `json:"chatId"`
	Path      string `json:"path"`
	Placement string `json:"placement"`
}

// AutoIndexThreshold is the token count above which an attachment is indexed
// rather than inlined.
const AutoIndexThreshold = 8000

// AddAttachment reads a file, extracts its text, and attaches it to a chat.
func (a *App) AddAttachment(in AttachmentInput) (store.Attachment, error) {
	if err := a.ensure(); err != nil {
		return store.Attachment{}, err
	}
	if strings.TrimSpace(in.Path) == "" {
		return store.Attachment{}, errors.New("kein Dateipfad angegeben")
	}
	info, err := os.Stat(in.Path)
	if err != nil {
		return store.Attachment{}, fmt.Errorf("Datei nicht lesbar: %w", err)
	}
	if info.IsDir() {
		return store.Attachment{}, errors.New("Ordner können nicht angehängt werden")
	}
	const maxSize = 64 << 20
	if info.Size() > maxSize {
		return store.Attachment{}, fmt.Errorf("Datei ist zu groß (%d MB, Maximum %d MB)",
			info.Size()>>20, int64(maxSize)>>20)
	}
	data, err := os.ReadFile(in.Path)
	if err != nil {
		return store.Attachment{}, fmt.Errorf("Datei nicht lesbar: %w", err)
	}

	result, err := a.extractor.Ingest(filepath.Base(in.Path), "", data)
	if err != nil {
		return store.Attachment{}, err
	}

	placement := in.Placement
	if placement == "" {
		if result.Tokens > AutoIndexThreshold {
			placement = store.PlacementIndexed
		} else {
			placement = store.PlacementInline
		}
	}

	attachment := store.Attachment{
		ID:            newID(),
		ChatID:        in.ChatID,
		Kind:          string(result.Kind),
		Filename:      filepath.Base(in.Path),
		Mime:          result.Mime,
		SizeBytes:     info.Size(),
		Pages:         result.Pages,
		TokenEstimate: result.Tokens,
		Placement:     placement,
		CreatedAt:     time.Now(),
	}
	if len(result.Warnings) > 0 {
		attachment.Warning = strings.Join(result.Warnings, " ")
	}

	// Images carry their payload rather than text, so a vision model can receive
	// them; everything else stores the extracted text.
	documentText := result.Text
	if result.Kind == ingest.KindImage {
		documentText = base64.StdEncoding.EncodeToString(data)
		attachment.TokenEstimate = 1024
	}

	document := store.Document{
		ID:           newID(),
		AttachmentID: attachment.ID,
		ChatID:       in.ChatID,
		Title:        filepath.Base(in.Path),
		Text:         documentText,
		TokenCount:   attachment.TokenEstimate,
		PageCount:    result.Pages,
		CreatedAt:    time.Now(),
	}
	if err := a.store.CreateDocument(document); err != nil {
		return store.Attachment{}, err
	}
	attachment.DocumentID = document.ID

	if placement == store.PlacementIndexed && result.Kind != ingest.KindImage {
		chunks := ingest.ChunkDocument(result.Text, ingest.DefaultChunkSpec())
		out := make([]store.Chunk, 0, len(chunks))
		for _, c := range chunks {
			out = append(out, store.Chunk{
				DocumentID: document.ID,
				ChatID:     in.ChatID,
				Ordinal:    c.Ordinal,
				Heading:    c.Heading,
				Page:       c.Page,
				Text:       c.Text,
				Tokens:     c.Tokens,
			})
		}
		if err := a.store.InsertChunks(out); err != nil {
			// Indexing is what makes the attachment reachable through
			// search_documents. Staying "indexed" after it failed would leave the
			// text in the database but invisible to the tool, so the placement falls
			// back to inline: the content still reaches the model, and the warning
			// says why it is not searchable.
			placement = store.PlacementInline
			attachment.Placement = placement
			attachment.Warning = strings.TrimSpace(attachment.Warning +
				" Indizierung fehlgeschlagen: " + err.Error())
		}
	}

	if err := a.store.CreateAttachment(attachment); err != nil {
		// The document — the whole extracted text, and the parent of its chunks — is
		// written first because the attachment references it. Losing the attachment
		// must not leave that text behind with nothing pointing at it: it is the
		// largest row in the database, and DeleteChat and DeleteAttachment both
		// exist to stop exactly this leak.
		if cleanupErr := a.store.DeleteDocument(document.ID); cleanupErr != nil {
			return store.Attachment{}, fmt.Errorf("%w (Dokument %s konnte nicht aufgeräumt werden: %v)",
				err, document.ID, cleanupErr)
		}
		return store.Attachment{}, err
	}
	a.emit(events.Attachments, map[string]any{"chatId": in.ChatID})
	return attachment, nil
}

// RemoveAttachment deletes an attachment and its extracted text.
func (a *App) RemoveAttachment(attachmentID string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	attachment, err := a.store.GetAttachment(attachmentID)
	if err != nil {
		return err
	}
	if err := a.store.DeleteAttachment(attachmentID); err != nil {
		return err
	}
	a.emit(events.Attachments, map[string]any{"chatId": attachment.ChatID})
	return nil
}

// SetAttachmentPlacement switches an attachment between inline and indexed.
func (a *App) SetAttachmentPlacement(attachmentID, placement string) (store.Attachment, error) {
	if err := a.ensure(); err != nil {
		return store.Attachment{}, err
	}
	attachment, err := a.store.GetAttachment(attachmentID)
	if err != nil {
		return store.Attachment{}, err
	}
	// An image's document text is the raw base64 of the file, so indexing it would
	// fill the search index with encoded bytes. AddAttachment never indexes one;
	// the switch has to refuse it for the same reason.
	if placement == store.PlacementIndexed && attachment.Kind == string(ingest.KindImage) {
		return store.Attachment{}, errors.New("Bilder werden nicht indiziert, sondern direkt übergeben")
	}
	attachment.Placement = placement
	if placement == store.PlacementIndexed {
		document, err := a.store.GetDocument(attachment.DocumentID)
		if err != nil {
			// Marking the attachment indexed without its text would leave it
			// unreachable through search_documents while the UI showed it as
			// searchable.
			return store.Attachment{}, fmt.Errorf("Dokument des Anhangs lesen: %w", err)
		}
		existing, _ := a.store.ListChunks(document.ID)
		if len(existing) == 0 {
			chunks := ingest.ChunkDocument(document.Text, ingest.DefaultChunkSpec())
			out := make([]store.Chunk, 0, len(chunks))
			for _, c := range chunks {
				out = append(out, store.Chunk{
					DocumentID: document.ID, ChatID: attachment.ChatID,
					Ordinal: c.Ordinal, Heading: c.Heading, Page: c.Page,
					Text: c.Text, Tokens: c.Tokens,
				})
			}
			if err := a.store.InsertChunks(out); err != nil {
				return store.Attachment{}, err
			}
		}
	}
	if err := a.store.UpdateAttachment(attachment); err != nil {
		return store.Attachment{}, err
	}
	a.emit(events.Attachments, map[string]any{"chatId": attachment.ChatID})
	return attachment, nil
}

// PickFiles opens a native file dialog and returns the chosen paths.
func (a *App) PickFiles() ([]string, error) {
	if a.ctx == nil {
		return nil, errors.New("App ist noch nicht bereit")
	}
	return runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Dateien zum Chat hinzufügen",
	})
}

// GetDocument returns a document including its full text.
func (a *App) GetDocument(documentID string) (store.Document, error) {
	if err := a.ensure(); err != nil {
		return store.Document{}, err
	}
	return a.store.GetDocument(documentID)
}

// ListChunks returns the indexed chunks of a document.
func (a *App) ListChunks(documentID string) ([]store.Chunk, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	return a.store.ListChunks(documentID)
}

// --- search and scraping ---------------------------------------------------

// Search runs a live web search, bypassing nothing but the provider chain.
func (a *App) Search(query string) (research.SearchOutcome, error) {
	if err := a.ensure(); err != nil {
		return research.SearchOutcome{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	outcome, err := a.chain.Search(ctx, query, research.SearchRequest{})
	if err != nil {
		return research.SearchOutcome{}, err
	}
	return outcome, nil
}

// Scrape fetches a URL and returns clean Markdown.
func (a *App) Scrape(url string, maxTokens int) (research.ScrapeResult, error) {
	if err := a.ensure(); err != nil {
		return research.ScrapeResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if maxTokens <= 0 {
		maxTokens = 12000
	}
	return a.scraper.Scrape(ctx, url, maxTokens)
}

// ScrapeIntoChat scrapes a page and attaches it to a chat as an indexed or
// inline document, which is how "read this page" becomes permanent context.
func (a *App) ScrapeIntoChat(chatID, pageURL string, maxTokens int) (store.Attachment, error) {
	if err := a.ensure(); err != nil {
		return store.Attachment{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if maxTokens <= 0 {
		maxTokens = 12000
	}
	result, err := a.scraper.Scrape(ctx, pageURL, maxTokens)
	if err != nil {
		return store.Attachment{}, err
	}

	title := result.Title
	if title == "" {
		title = result.FinalURL
	}
	attachment := store.Attachment{
		ID:            newID(),
		ChatID:        chatID,
		Kind:          "web",
		Filename:      title,
		Mime:          "text/markdown",
		SizeBytes:     int64(len(result.Markdown)),
		TokenEstimate: result.Tokens,
		Placement:     store.PlacementInline,
		CreatedAt:     time.Now(),
	}
	if result.Warning != "" {
		attachment.Warning = result.Warning
	}
	document := store.Document{
		ID:           newID(),
		AttachmentID: attachment.ID,
		ChatID:       chatID,
		Title:        title,
		Text:         result.Markdown,
		TokenCount:   result.Tokens,
		SourceURL:    result.FinalURL,
		CreatedAt:    time.Now(),
	}
	if err := a.store.CreateDocument(document); err != nil {
		return store.Attachment{}, err
	}
	attachment.DocumentID = document.ID
	if err := a.store.CreateAttachment(attachment); err != nil {
		return store.Attachment{}, err
	}
	a.emit(events.Attachments, map[string]any{"chatId": chatID})
	return attachment, nil
}

// SearchStatus reports provider availability and quota usage.
type SearchStatus struct {
	Providers     map[string]bool `json:"providers"`
	Order         []string        `json:"order"`
	BraveQueries  int64           `json:"braveQueriesThisMonth"`
	BraveQuotaEst int64           `json:"braveQuotaEstimate"`
	CacheMinutes  int             `json:"cacheMinutes"`
	SecretBackend string          `json:"secretBackend"`
}

// GetSearchStatus reports the search configuration state.
func (a *App) GetSearchStatus() (SearchStatus, error) {
	if err := a.ensure(); err != nil {
		return SearchStatus{}, err
	}
	settings := a.Settings()
	return SearchStatus{
		Providers:    a.chain.ProviderAvailability(),
		Order:        settings.Search.ProviderOrder,
		BraveQueries: a.chain.BraveQueriesThisMonth(),
		// Brave's metered plan gives $5 of free credit per month at
		// $5 per 1,000 requests, so about a thousand queries.
		BraveQuotaEst: 1000,
		CacheMinutes:  settings.Search.CacheMinutes,
		SecretBackend: a.secrets.Backend(),
	}, nil
}

// OpenExternal opens a URL in the user's browser.
func (a *App) OpenExternal(target string) {
	if a.ctx == nil {
		return
	}
	runtime.BrowserOpenURL(a.ctx, target)
}

// RedactSecrets strips stored secrets from arbitrary text.
func (a *App) RedactSecrets(text string) string {
	if a.secrets == nil {
		return text
	}
	var values []string
	if v, ok := a.secrets.Get("brave"); ok {
		values = append(values, v)
	}
	// Backend API keys are secrets too, so they are redacted alongside the Brave
	// key rather than being left to leak into a log or a toast.
	for _, b := range a.Settings().Backends {
		if v, ok := a.secrets.Get(llm.BackendAPIKeyName(b.ID)); ok {
			values = append(values, v)
		}
	}
	return secret.Redact(text, values...)
}
