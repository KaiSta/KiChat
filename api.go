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
	"kichat/internal/images"
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
	// The record the draft is merged against. The draft only ever carries a boolean
	// about the Brave key (GetSettings blanks the secret), and it never carries the
	// installed build at all, so both are read from here.
	recorded := a.Settings()
	// An empty BraveAPIKey means "leave the stored key alone".
	s.Search.BraveAPIKey = recorded.Search.BraveAPIKey
	// The image server's credential follows the same rule as a backend key: a
	// non-empty draft overwrites what is stored, and an empty one leaves it alone.
	// "Empty" therefore cannot clear it, which is the right trade — a save that
	// does not touch the field must not silently undo a configured credential.
	if want := strings.TrimSpace(s.Images.APIKey); want != "" {
		if err := a.secrets.Set(config.ImageAPIKeyName, want); err != nil {
			return s, err
		}
		s.Images.APIKey = want
	} else if stored, ok := a.secrets.Get(config.ImageAPIKeyName); ok {
		s.Images.APIKey = stored
	} else {
		s.Images.APIKey = ""
	}
	// The installed build is the installer's record, and no control in the panel edits
	// the tag. A download writes it from its own goroutine, after the bound call that
	// started it returned, so a draft the view read before that still names the build
	// the install replaced. Two tags that differ therefore say the draft predates the
	// install — which also means its source and family are the ones the install
	// superseded and not a choice the user just made — so the recorded runtime wins
	// whole. Without this, saving an unrelated setting put the replaced build back (the
	// next start ran the old llama.cpp while the panel showed the new one) or stranded
	// a fork install under a reverted upstream source. A draft whose tag already matches
	// is current, and its source and family are the pickers' own.
	if strings.TrimSpace(s.Runtime.Tag) != strings.TrimSpace(recorded.Runtime.Tag) {
		s.Runtime.Distribution = recorded.Runtime.Distribution
		s.Runtime.Accelerator = recorded.Runtime.Accelerator
	}
	s.Runtime.Tag = recorded.Runtime.Tag
	// The availability blacklist is written by the model check, not by any control
	// in the panel, so no draft ever carries it. A draft the view read before the
	// last sweep would otherwise put every hidden model back into the picker on the
	// next unrelated save.
	s.HiddenModels = recorded.HiddenModels
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
	// The image server's "user:pass" is a credential like the others, so it never
	// reaches the settings blob or the bridge.
	s.Images.APIKey = ""
	return s
}

// SecretStatus reports how secrets are stored.
type SecretStatus struct {
	Backend     string `json:"backend"`
	Keyring     bool   `json:"keyring"`
	BraveKeySet bool   `json:"braveKeySet"`
	// BackendKeys names the backends that have a key stored, one boolean per
	// configured entry and never a value. The settings blob is sanitized, so this
	// is the only way the panel can tell a configured entry from an untouched one.
	// It needs that to explain the state this was added for: a key pasted into a
	// shipped entry that is switched off is stored by the save, but no provider is
	// built for it and no probe is ever sent, and the panel could otherwise only
	// show a blank key field on a backend that is already configured.
	BackendKeys map[string]bool `json:"backendKeys,omitempty"`
	// ImageKeySet reports whether the image server has a credential stored. The
	// panel needs it for the same reason it needs BackendKeys: the blob is
	// sanitised, so a blank field cannot be told apart from a configured one.
	ImageKeySet bool   `json:"imageKeySet"`
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
	_, status.ImageKeySet = a.secrets.Get(config.ImageAPIKeyName)
	// Every configured entry, switched on or not: an entry that is off is exactly
	// the one whose key has to stay visible.
	status.BackendKeys = map[string]bool{}
	for _, b := range a.Settings().Backends {
		_, status.BackendKeys[b.ID] = a.secrets.Get(llm.BackendAPIKeyName(b.ID))
	}
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
		status.Models = append(status.Models, ManagedStatus{
			BackendID: id, Enabled: mm.Enabled, ModelPath: mm.ModelPath,
			CtxSize: mm.CtxSize, ExtraArgs: mm.ExtraArgs, State: rt.StateStopped,
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
		return errors.New("ein Download läuft bereits")
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
	updated := a.settings
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

// PickImageFile asks for one of the CLI backend's paths: the binary, or one of the model
// files a pipeline is made of.
//
// One binding for both rather than two, because the difference is only the filter: a model
// file is `*.gguf`/`*.safetensors`, while the executable has no extension at all on Linux and
// `.exe` on Windows, so an empty pattern list — "all files" — is the only filter that shows
// either. Getting that wrong hides the very file the user was told to pick.
//
// Patterns reach Wails as one semicolon-separated string, which is its own format rather than
// glob alternation, and the title comes from the UI so it follows the interface language.
// The dialog opens in the model folder the app already knows about: the Qwen-Image triple is
// the shape this backend exists for, and it is downloaded to one folder.
func (a *App) PickImageFile(title string, patterns []string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	opts := runtime.OpenDialogOptions{Title: title, DefaultDirectory: a.defaultModelDir()}
	if len(patterns) > 0 {
		opts.Filters = []runtime.FileFilter{{DisplayName: "Modelldateien", Pattern: strings.Join(patterns, ";")}}
	}
	return runtime.OpenFileDialog(a.ctx, opts)
}

// PickImageFolder asks for the folder a stable-diffusion.cpp release unpacked into.
//
// A folder rather than a file, because that is the shape a download arrives in: the
// archive holds the program, its backend libraries and its DLLs or `.so` files
// together, and asking a user to find `sd-cli` inside it is asking them to open
// folders until they see a name with no extension. What the app does with the answer
// is find the program itself (images.FindProgram), including the single root directory
// an archive lays down one level below.
//
// The choice is checked before it is returned: a folder with no program in it is a
// mistake the user can still correct, and saying so here is one sentence instead of a
// red badge on the next reload. An empty answer is a cancelled dialog, which is
// neither success nor failure and is left for the caller to ignore.
func (a *App) PickImageFolder(title string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: title})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(dir) == "" {
		return "", nil
	}
	if _, err := images.FindProgram(dir); err != nil {
		return "", err
	}
	return dir, nil
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
	return a.currentRegistry().Health(ctx), nil
}

// ListModels returns the merged cross-backend model catalogue.
func (a *App) ListModels(forceRefresh bool) ([]llm.ModelInfo, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if forceRefresh {
		a.currentRegistry().Invalidate()
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
	a.currentRegistry().Invalidate()
	// Sanitised, because this is the one save that writes the *in-memory* snapshot
	// rather than a draft: no control in the panel edits a model override through
	// the settings form, so the snapshot is all this call has. SaveSettings can
	// persist its draft as it stands precisely because a draft never carries a key;
	// this one carries every key the app holds, and writing it as it stood put all
	// of them into the settings row.
	return a.sanitize(settings).Save(a.store)
}

// ModelCheckStatus is a model availability sweep as the picker reads it.
type ModelCheckStatus struct {
	Backend string `json:"backend,omitempty"`
	Busy    bool   `json:"busy"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	// Hidden is how many models the sweep that just finished put on the
	// blacklist, and HiddenTotal is the whole blacklist across every backend. The
	// picker shows the second: a user who switched to another backend would
	// otherwise see no sign that a list is shorter than the catalogue says.
	Hidden      int `json:"hidden"`
	HiddenTotal int `json:"hiddenTotal"`
	// Error is why a sweep could not run, or why it declined to act. It is a
	// reading rather than a failed call, because it is a row in the picker.
	Error string `json:"error,omitempty"`
	// Reason names the failure when the UI has its own sentence for it. Error is
	// the backend's own text and is passed through untranslated — that is what
	// every other Go error in this app does — but the one verdict the sweep itself
	// reaches has to be translatable, or a Chinese or English window would show a
	// German sentence in a row that stays on screen.
	Reason string `json:"reason,omitempty"`
}

// ReasonAllRefused is the sweep's own verdict that it declined to act: every
// model it reached was refused, which is what a wrong address or a key without
// entitlement looks like from here. The UI has a sentence of its own for it.
const ReasonAllRefused = "all-refused"

// modelCheckTimeout bounds a whole sweep. It is generous because it is the outer
// bound only: the per-model timeout is what a single hanging model costs, and six
// of NVIDIA's 80 models hang rather than answer, so a tight total would cut the
// sweep short and leave the models after it unasked.
const modelCheckTimeout = 10 * time.Minute

// StartModelCheck asks every model of one backend to answer one token, so the
// ones this account cannot call can be kept out of the picker.
//
// It returns as soon as the sweep is running rather than when it finishes. The
// sweep for build.nvidia.com's 80 models takes about a minute, and a bound call
// that blocked for it would look like a hang with nothing to show; the UI follows
// it by polling GetModelCheckStatus, the same way it follows a runtime download.
func (a *App) StartModelCheck(backend string) (ModelCheckStatus, error) {
	if err := a.ensure(); err != nil {
		return ModelCheckStatus{}, err
	}
	a.checkMu.Lock()
	if a.checkBusy {
		// A second click while one is running is the same request, not a new one:
		// two sweeps at once would double the requests and race on the result.
		status := a.checkStatusLocked()
		a.checkMu.Unlock()
		return status, nil
	}
	a.checkMu.Unlock()

	provider, ok := a.currentRegistry().Provider(backend)
	if !ok {
		return ModelCheckStatus{}, fmt.Errorf("Backend %q ist nicht konfiguriert oder ausgeschaltet", backend)
	}

	// The sweep outlives the call that started it, so it cannot borrow the call's
	// context: Wails cancels that the moment this returns. The app's own context
	// bounds it instead, which is what makes a sweep stop when the window closes.
	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, modelCheckTimeout)

	// Reachability is settled before the sweep rather than during it. A backend
	// that answers nothing at all refuses every model it is asked about, and that
	// reading is indistinguishable from a catalogue this account may not call — so
	// it would blacklist the whole list because a server was down.
	if err := provider.Health(ctx); err != nil {
		cancel()
		return ModelCheckStatus{}, fmt.Errorf("Backend %q ist nicht erreichbar: %w", backend, err)
	}

	// The listing is asked of the provider directly rather than read from the
	// visible catalogue: a re-check has to see the models the last sweep hid, or a
	// model wrongly kept out could never come back.
	models, err := provider.ListModels(ctx)
	if err != nil {
		cancel()
		return ModelCheckStatus{}, fmt.Errorf("Modellliste von %q: %w", backend, err)
	}
	if len(models) == 0 {
		cancel()
		return ModelCheckStatus{}, fmt.Errorf("Backend %q meldet keine Modelle", backend)
	}

	a.checkMu.Lock()
	a.checkBackend, a.checkBusy, a.checkErr, a.checkReason = backend, true, "", ""
	a.checkDone, a.checkTotal, a.checkHidden = 0, len(models), 0
	status := a.checkStatusLocked()
	a.checkMu.Unlock()

	go func() {
		defer cancel()
		probes := llm.ProbeModels(ctx, provider, models, llm.ProbeOptions{}, func(llm.ModelProbe) {
			a.checkMu.Lock()
			a.checkDone++
			a.checkMu.Unlock()
		})
		hidden, reason, err := a.applyModelProbes(backend, probes)
		a.checkMu.Lock()
		a.checkBusy = false
		a.checkReason = reason
		if err != nil {
			a.checkErr = err.Error()
		} else {
			a.checkHidden = hidden
		}
		a.checkMu.Unlock()
	}()
	return status, nil
}

// GetModelCheckStatus reports the sweep in progress, or the last one's result.
// The picker polls it while a sweep runs and once after.
func (a *App) GetModelCheckStatus() ModelCheckStatus {
	settings := a.Settings()
	a.checkMu.Lock()
	defer a.checkMu.Unlock()
	status := a.checkStatusLocked()
	status.HiddenTotal = len(settings.HiddenModels)
	return status
}

// checkStatusLocked reads the sweep state; the caller holds checkMu.
func (a *App) checkStatusLocked() ModelCheckStatus {
	return ModelCheckStatus{
		Backend: a.checkBackend,
		Busy:    a.checkBusy,
		Done:    a.checkDone,
		Total:   a.checkTotal,
		Hidden:  a.checkHidden,
		Error:   a.checkErr,
		Reason:  a.checkReason,
	}
}

// applyModelProbes writes a sweep's verdicts into the blacklist and returns how
// many models of that backend it hides.
//
// Two rules keep it from being destructive on weak evidence. A sweep in which
// every model was refused is discarded rather than applied — that is what a wrong
// address or a key without entitlement looks like from here, and committing it
// would empty the picker. And a model the sweep could not judge (a timeout, a
// throttle) keeps the verdict it already had, so only a sweep that really reached
// a model can add it to the list or take it off.
func (a *App) applyModelProbes(backend string, probes []llm.ModelProbe) (int, string, error) {
	reached := map[string]bool{}
	refused := map[string]bool{}
	for _, p := range probes {
		if p.Model == "" || p.Kind == "" {
			continue // never asked: the sweep was stopped before it got here
		}
		reached[p.Model] = true
		if p.Refused() {
			refused[p.Model] = true
		}
	}
	switch {
	case len(reached) == 0:
		return 0, "", errors.New("kein Modell konnte geprüft werden")
	case len(refused) == len(reached):
		return 0, ReasonAllRefused, fmt.Errorf("alle %d Modelle wurden abgelehnt — das deutet auf Adresse oder Schlüssel hin, nicht auf die Liste; es wurde nichts ausgeblendet", len(reached))
	}

	settings := a.Settings()
	prefix := backend + "/"
	out := make([]string, 0, len(settings.HiddenModels)+len(refused))
	seen := map[string]bool{}
	add := func(key string) {
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, key)
	}
	for _, key := range settings.HiddenModels {
		model, ok := strings.CutPrefix(key, prefix)
		if !ok {
			add(key) // another backend's entry, untouched
			continue
		}
		if refused[model] || !reached[model] {
			// Either still refused, or a model this sweep could not judge — in
			// which case the earlier verdict stands rather than being dropped by a
			// timeout that says nothing about the model.
			add(key)
		}
	}
	for _, p := range probes {
		if p.Refused() {
			add(prefix + p.Model)
		}
	}

	a.settingsMu.Lock()
	a.settings.HiddenModels = out
	updated := a.settings
	a.settingsMu.Unlock()
	// Sanitised, the way SaveSettings persists: the in-memory snapshot keeps the
	// real backend keys so the registry can authenticate, and the blob must not.
	if err := a.sanitize(updated).Save(a.store); err != nil {
		return 0, "", err
	}

	// Hiding is a statement about the picker, so a chat already pinned to one of
	// these models keeps working and keeps its trigger label; only the listing
	// changes.
	hidden := 0
	for _, key := range out {
		if strings.HasPrefix(key, prefix) {
			hidden++
		}
	}
	return hidden, "", nil
}

// ClearHiddenModels forgets the blacklist, for one backend or — with an empty id
// — for every backend.
//
// A sweep measures one moment: an account can be upgraded, and NVIDIA adds
// capacity behind a model that 404s today. A verdict therefore has to be
// reversible from the UI rather than by hand-editing the settings blob.
func (a *App) ClearHiddenModels(backend string) (ModelCheckStatus, error) {
	if err := a.ensure(); err != nil {
		return ModelCheckStatus{}, err
	}
	settings := a.Settings()
	kept := make([]string, 0, len(settings.HiddenModels))
	for _, key := range settings.HiddenModels {
		if backend == "" || !strings.HasPrefix(key, backend+"/") {
			kept = append(kept, key)
		}
	}

	a.settingsMu.Lock()
	a.settings.HiddenModels = kept
	a.settingsMu.Unlock()
	a.checkMu.Lock()
	a.checkBackend, a.checkHidden, a.checkErr, a.checkReason = "", 0, "", ""
	a.checkMu.Unlock()

	// Sanitised for the same reason as in applyModelProbes: the blob carries the
	// blacklist, never a key.
	if err := a.sanitize(a.Settings()).Save(a.store); err != nil {
		return ModelCheckStatus{}, err
	}
	return a.GetModelCheckStatus(), nil
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
	result, err := a.currentEngine().Compact(ctx, req.ChatID, chat.CompactOptions{
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
	a.currentEngine().InvalidateMeasurement(chatID)
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

// --- image generation -------------------------------------------------------

// ImageStatus is the picture backend as the Settings panel reads it.
type ImageStatus struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"baseURL"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	// Models and Samplers are never nil. A nil Go slice marshals to JSON null,
	// and the panel iterates both — the empty case is exactly what a fresh
	// WebUI with no checkpoint installed answers with, so it is the one that
	// must not arrive as null.
	Models   []string `json:"models"`
	Samplers []string `json:"samplers"`
	KeySet   bool     `json:"keySet"`
	// Kind is which backend draws: "" for the HTTP server, "cli" for a local binary.
	// The panel needs it to render the right controls, and it is the *stored* kind
	// rather than anything derived, so a hand-edited blob shows what it says.
	Kind string `json:"kind"`
	// Binary is the CLI kind's executable, empty for the server kind. It is reported
	// separately from BaseURL rather than reusing one field: a path and a URL are
	// checked, opened and copied differently, and a single field meaning one in each
	// kind ends up in a log line as a broken link.
	//
	// It is the *resolved* program — the one a draw would run — not necessarily what
	// was typed: a configured program folder supplies it (images.ResolveProgram), and
	// the panel showing the file that will really be started is the point of resolving
	// it here rather than making the view re-do the search.
	Binary string `json:"binary"`
	// Folder is the CLI kind's program folder, empty when the program is named by path
	// instead. Reported so the panel can say which of the two is supplying the
	// executable, because they are not equivalent: a set folder wins, and one that
	// holds no program is a failure rather than a fallback.
	Folder string `json:"folder"`
	// Build is what the program folder *is*, read off its backend libraries:
	// "vulkan", "cuda", "rocm", "metal", or empty for the plain CPU build. It is a
	// reading rather than the stored CLIBuild, which is what the user meant to
	// download — and the two disagreeing is exactly what is worth showing.
	Build string `json:"build"`
	// Builds are the builds the project publishes for this machine (images.SDBuilds),
	// so the panel can offer only what can really be downloaded and name the archive to
	// look for. Never nil, because the panel iterates it.
	Builds []images.SDBuild `json:"builds"`
	// ReleasePage is where those archives live. It travels in the status rather than
	// being written into the interface a second time: it is the project's address, and
	// a copy of it in the frontend is a copy that goes stale on its own.
	ReleasePage string `json:"releasePage"`
	// Files are the CLI kind's model paths, in the order they reach the binary. Both
	// this and Samplers exist so the panel can show what will really be loaded
	// without re-deriving it from the settings, and neither is ever nil.
	Files []string `json:"files"`
}

// imageStatusFiles lists the model paths the CLI backend will pass, in argument order,
// skipping the empty ones. An empty field is not listed: it is omitted from the command
// line, and a blank row in the panel would suggest the binary is being handed something.
func imageStatusFiles(s config.ImageSettings) []string {
	out := []string{}
	for _, path := range (images.ModelFiles{
		Model:          s.CLIModel,
		DiffusionModel: s.CLIDiffusionModel,
		VAE:            s.CLIVAE,
		LLM:            s.CLILLM,
		ClipL:          s.CLIClipL,
		ClipG:          s.CLIClipG,
		T5XXL:          s.CLIT5XXL,
	}).Paths() {
		if trimmed := strings.TrimSpace(path); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// GetImageStatus reports the image server's state: whether the feature is on,
// whether the address answers, and what it offers.
//
// A switched-off feature reports no error at all. Nothing was contacted, so
// nothing failed, and saying otherwise would put a red row in front of a user who
// never asked for image generation — which is the mistake the backends panel
// already made once with a disabled entry.
func (a *App) GetImageStatus() (ImageStatus, error) {
	if err := a.ensure(); err != nil {
		return ImageStatus{}, err
	}
	settings := a.Settings()
	status := ImageStatus{
		Enabled:  settings.Images.Enabled,
		BaseURL:  settings.Images.BaseURL,
		Models:   []string{},
		Samplers: []string{},
		Kind:     settings.Images.Kind,
		Binary:   settings.Images.CLIBinary,
		Folder:   settings.Images.CLIFolder,
		Files:    imageStatusFiles(settings.Images),
		Builds:   images.SDBuilds(goruntime.GOOS, goruntime.GOARCH),

		ReleasePage: images.ReleasePage,
	}
	// What the program folder really holds, when there is one to read. The program is
	// resolved exactly as a draw resolves it, so the panel reports the build the next
	// picture would use rather than the one the folder is named after; a folder that
	// yields no program is left to Health, which is where the reason belongs.
	if binary, err := images.ResolveProgram(settings.Images.CLIFolder, settings.Images.CLIBinary); err == nil {
		status.Binary = binary
		status.Build = images.AcceleratorOf(filepath.Dir(binary))
	}
	_, status.KeySet = a.secrets.Get(config.ImageAPIKeyName)

	provider := a.currentImageProvider()
	if provider == nil {
		return status, nil
	}
	// The same bound as a backend health probe: a WebUI that is up answers in
	// milliseconds, and one that is not must not hold the settings view open.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := provider.Health(ctx); err != nil {
		status.Error = err.Error()
		return status, nil
	}
	status.OK = true
	if models, err := provider.Models(ctx); err == nil {
		for _, m := range models {
			status.Models = append(status.Models, m.ID)
		}
	}
	if samplers, err := provider.Samplers(ctx); err == nil {
		status.Samplers = append(status.Samplers, samplers...)
	}
	return status, nil
}

// TestImageBackend runs one generation and discards the result, so a user can
// find out that their WebUI is reachable and has a model loaded without spending
// a chat turn on it.
//
// The bound is the backend's own, exactly as it is for a redraw: the CLI kind pays a model
// load before its first step, and a fixed five minutes would report a working setup as a
// failure on a machine that needed six.
func (a *App) TestImageBackend() (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	provider := a.currentImageProvider()
	if provider == nil {
		return "", images.ErrNotConfigured
	}
	settings := a.Settings()
	ctx, cancel := context.WithTimeout(context.Background(), imageDrawTimeout(settings))
	defer cancel()
	// The size is the user's own when they chose one, and a small square otherwise:
	// a setup that only works at its trained size would otherwise fail a test that
	// real draws pass. Bounded by the same call every other request goes through.
	dimension := func(value int) int {
		if value > 0 {
			return value
		}
		return 512
	}
	result, err := provider.Generate(ctx, images.Request{
		Prompt:         "a simple color test pattern",
		NegativePrompt: settings.Images.NegativePrompt,
		Width:          dimension(settings.Images.Width),
		Height:         dimension(settings.Images.Height),
		Steps:          settings.Images.Steps,
		Sampler:        settings.Images.Sampler,
		Model:          settings.Images.Model,
	}.Normalised())
	if err != nil {
		return "", err
	}
	report := fmt.Sprintf("%d Bild(er) erzeugt (%s)", len(result.Images), provider.Name())
	if result.Model != "" {
		report += ", Modell " + result.Model
	}
	return report, nil
}

// RegenerateImage draws a picture from the recipe it is handed and returns the new
// attachment.
//
// This is the only path into image generation that does not go through a chat model: a
// recipe is data, not a request in words, so nothing has to be asked of the model to use it.
// The panel sends the whole recipe every time, which is what gives it three controls out of
// one call — a repeat passes the stored recipe back untouched, a variation passes it with
// the seed cleared, and the editor passes the fields the user changed. A zero field means
// "the server's own", so an emptied box in the editor means what it looks like it means.
//
// The picture is filed onto the message it was redrawn from, which is what puts it beside
// the original, and the attachments event is emitted because the view holds that message's
// attachment list — without it the new picture would exist and stay invisible until a manual
// reload.
func (a *App) RegenerateImage(attachmentID string, recipe store.GenerationRecipe) (store.Attachment, error) {
	if err := a.ensure(); err != nil {
		return store.Attachment{}, err
	}
	service := a.currentToolService()
	if service == nil {
		return store.Attachment{}, images.ErrNotConfigured
	}
	if a.ctx == nil {
		return store.Attachment{}, errors.New("App ist noch nicht bereit")
	}
	// Generous, because a local backend drawing a large picture takes minutes on a CPU, and
	// present at all so a wedged process cannot hold the click open indefinitely. How
	// generous is the backend's own answer: a server holds its model in memory and spends
	// the bound on sampling, while the CLI kind loads one from disk first. A user who will
	// not wait that long presses Stop, which cancels this context — that, rather than the
	// bound, is the control for a draw that is merely slow.
	ctx, cancel := context.WithTimeout(a.ctx, imageDrawTimeout(a.Settings()))
	defer cancel()
	// The chat is read here rather than inside the service so the progress events can say
	// which chat they belong to, the way every other event does; one indexed row read is a
	// smaller price than a window that has to guess.
	chatID := ""
	if source, err := a.store.GetAttachment(attachmentID); err == nil {
		chatID = source.ChatID
	}
	release := a.registerDraw(attachmentID, chatID, cancel)
	defer release()

	attachment, err := service.Regenerate(ctx, attachmentID, recipe)
	if err != nil {
		// A stopped draw is not a failure and must not travel as one. The context is the
		// only thing that can have cancelled this call, so a cancelled context is what
		// tells the user's own click from a server that broke — and the *parent* being
		// gone is the app closing rather than a stop, which is left to read as what it is.
		if errors.Is(ctx.Err(), context.Canceled) && a.ctx.Err() == nil {
			return store.Attachment{}, images.ErrCancelled
		}
		return store.Attachment{}, err
	}
	a.emit(events.Attachments, map[string]any{"chatId": attachment.ChatID})
	return attachment, nil
}

// StopDrawResult is what stopping an image draw achieved.
//
// Three facts rather than a bare bool, because they are different sentences and the UI
// has to be able to pick the right one: a draw was running and is now over (say so),
// nothing was running (say nothing — this is a second click, or a draw that finished
// between the render and the click), and whether the *server* was also told to stop.
type StopDrawResult struct {
	// WasDrawing is true when a draw was in flight and is now over.
	WasDrawing bool `json:"wasDrawing"`
	// Interrupted is true when the image server was asked to abandon the work behind it.
	// False is not a failure: stable-diffusion.cpp has no route for it.
	Interrupted bool `json:"interrupted"`
	// InterruptError says why the server could not be asked. It is reported rather than
	// dropped so a server that has the route can be told from one that does not — but it
	// is never a failure *of the stop*: the wait it was pressed to end is already over.
	InterruptError string `json:"interruptError,omitempty"`
}

// reportDrawProgress turns one report from a running draw into an event the window can
// paint.
//
// The preview travels as a data: URL for the same reason a stored picture does — the
// webview loads from an embedded assets handler, and a path would be blocked by the same
// origin rule — and because the bytes are already in hand: a second bridge round trip to
// fetch what this call has just read would be a request per step.
//
// Called from the drawing goroutine, so it only touches the emitter, which is safe from
// any goroutine.
func (a *App) reportDrawProgress(id, chatID string, progress images.Progress) {
	if id == "" {
		return
	}
	// Noted so the release knows there is a row on screen to clear. A report for an id
	// nothing is registered under is still emitted: the window keys its rows by draw id, so
	// the worst it can do is update one nothing else will touch.
	a.drawsMu.Lock()
	if draw, ok := a.draws[id]; ok {
		draw.reported = true
		a.draws[id] = draw
	}
	a.drawsMu.Unlock()
	a.emit(events.ImageProgress, drawProgressPayload(id, chatID, progress, false))
}

// drawProgressPayload decides the shape of one `image:progress` event, in one place because
// two calls emit it — a report and the end of a draw.
//
// The keys are the interface the window reads, and a renamed key is not a compile error on
// either side of the bridge: it is a progress row that silently stops appearing while the
// draw itself keeps working. That is what the test in image_progress_test.go pins.
//
// `done` is part of every payload rather than a second event name, so a listener that misses
// one cannot be left holding a progress row for a draw that is over — which is a Stop button
// for something that is not running.
func drawProgressPayload(id, chatID string, progress images.Progress, done bool) map[string]any {
	payload := map[string]any{
		"drawId": id,
		"chatId": chatID,
		"step":   progress.Step,
		"steps":  progress.Steps,
		"done":   done,
	}
	if len(progress.Preview) > 0 && progress.PreviewMime != "" {
		payload["preview"] = "data:" + progress.PreviewMime + ";base64," +
			base64.StdEncoding.EncodeToString(progress.Preview)
	}
	return payload
}

// StopImageDraw ends the wait on a picture being drawn and asks the server to abandon
// the work behind it.
//
// Both halves matter and neither replaces the other. Cancelling the request is what makes
// the click take effect at once — a 40-step draw on a CPU server is minutes, and the
// ten-minute bound on the call is not a promise the user should be held to. The interrupt
// is what stops the server spending those minutes on a picture nobody will ever see, and
// it exists on AUTOMATIC1111 and Forge and not on stable-diffusion.cpp, so it is attempted
// and never required.
//
// Nothing is stored either way: the picture only exists once the server answers with it,
// so a stopped draw leaves the transcript exactly as it was.
//
// drawID is whatever the view knows the draw by, and there are two of those: the attachment
// a redraw works on, and the assistant message a model-started draw belongs to (its tool
// call is rendered there). Passing an id nothing is registered under is not an error — it
// is the "nothing was running" answer a second click produces.
func (a *App) StopImageDraw(drawID string) StopDrawResult {
	result := StopDrawResult{}
	if a.ctx == nil {
		return result
	}
	a.drawsMu.Lock()
	draw, ok := a.draws[drawID]
	a.drawsMu.Unlock()
	if !ok {
		return result
	}
	result.WasDrawing = true
	draw.cancel()
	// A draw registered before the tool service existed — or by a path that never
	// filled the field — has nothing to ask. The wait it was pressed to end is over
	// either way, so this is the same answer as a backend that cannot be interrupted.
	if draw.service == nil {
		return result
	}
	// A fresh, short context on purpose: the draw's own context was cancelled by the line
	// above, and an interrupt sent on it would never leave the machine.
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	asked, err := draw.service.InterruptDraw(ctx)
	result.Interrupted = asked && err == nil
	if err != nil {
		result.InterruptError = err.Error()
	}
	return result
}

// registerDraw records a draw so Stop can find it, and returns the function that releases
// the registration.
//
// It is called from two places, and that is the point of it: RegenerateImage registers the
// attachment a redraw works on, and tools.Service registers the assistant message a
// model-started draw belongs to. Both ids are uuids, so one map holds both kinds without
// them colliding, and a single Stop path serves a picture that exists and one that does not
// yet.
//
// The release is the same token dance as before: it removes the entry only if it is still
// the one this call made, because a second draw of the same picture must keep its own entry
// — deleting it would leave that draw uncancellable while Stop reported nothing to stop.
// It also tells the window the draw is over, since there is no other event that means it: a
// progress row that outlives its draw is a Stop button for something that is not running.
//
// That last part is why the event is *conditional* on the draw having been reported on. A
// backend that cannot say anything while it works — or one whose only draw is a redraw the
// user is watching under the picture — leaves no row behind, and an event that fires for
// every silent draw is one the window has to handle for no reason at all.
func (a *App) registerDraw(id, chatID string, cancel context.CancelFunc) func() {
	if a.ctx == nil || id == "" {
		return func() {}
	}
	service := a.currentToolService()
	a.drawsMu.Lock()
	if a.draws == nil {
		a.draws = map[string]inflightDraw{}
	}
	// A draw already running for this id is cancelled first. Nothing in the UI can start two
	// (every control is disabled while one runs), so reaching this means a request arrived
	// from somewhere else, and the newest recipe is then the one the user is waiting for.
	if previous, ok := a.draws[id]; ok {
		previous.cancel()
	}
	a.drawToken++
	token := a.drawToken
	a.draws[id] = inflightDraw{token: token, cancel: cancel, service: service}
	a.drawsMu.Unlock()

	return func() {
		a.drawsMu.Lock()
		wasReported := false
		if draw, ok := a.draws[id]; ok && draw.token == token {
			wasReported = draw.reported
			delete(a.draws, id)
		}
		a.drawsMu.Unlock()
		if !wasReported {
			return
		}
		a.emit(events.ImageProgress, drawProgressPayload(id, chatID, images.Progress{}, true))
	}
}

// GetAttachmentImage returns an attachment's payload as a data: URL the webview
// can render.
//
// The bytes are already stored base64-encoded — that is the shape an image
// attachment's document has, and what the assembly turns back into an image part
// — so this re-encodes nothing. The data: URL rather than a file:// path is
// deliberate: the webview loads from an embedded assets handler, and a path would
// be blocked by the same origin rule that stops any other local file from being
// read.
func (a *App) GetAttachmentImage(attachmentID string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	attachment, err := a.store.GetAttachment(attachmentID)
	if err != nil {
		return "", err
	}
	if attachment.DocumentID == "" {
		return "", fmt.Errorf("Anhang %s hat kein gespeichertes Bild", attachment.Filename)
	}
	mime := strings.ToLower(attachment.Mime)
	if attachment.Kind != string(ingest.KindImage) && !strings.HasPrefix(mime, "image/") {
		return "", fmt.Errorf("%s ist kein Bild", attachment.Filename)
	}
	document, err := a.store.GetDocument(attachment.DocumentID)
	if err != nil {
		return "", err
	}
	payload := strings.TrimSpace(document.Text)
	if payload == "" {
		return "", fmt.Errorf("das Bild von %s ist leer", attachment.Filename)
	}
	if attachment.Mime == "" {
		// The renderer needs the type to paint anything at all.
		mime = "image/png"
	} else {
		mime = attachment.Mime
	}
	return "data:" + mime + ";base64," + payload, nil
}

// SaveAttachmentAs writes an attachment to a file the user picks, and returns the
// path it was written to — empty when the user cancelled.
//
// This is the only place a generated picture leaves the app's own storage, and it
// goes through the native save dialog rather than a folder setting so the choice
// is made per picture. The folder it was saved into is remembered as the next
// dialog's starting point, because re-navigating to the same place every time is
// what makes an export button annoying enough not to use.
func (a *App) SaveAttachmentAs(attachmentID string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	if a.ctx == nil {
		return "", errors.New("App ist noch nicht bereit")
	}
	attachment, err := a.store.GetAttachment(attachmentID)
	if err != nil {
		return "", err
	}
	document, err := a.store.GetDocument(attachment.DocumentID)
	if err != nil {
		return "", err
	}
	// Only an image is stored as base64; a text attachment's document holds its
	// extracted prose, and writing that out as if it were bytes would produce a
	// file of literal characters rather than the original document.
	if attachment.Kind != string(ingest.KindImage) {
		return "", fmt.Errorf("%s ist kein gespeichertes Bild", attachment.Filename)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(document.Text))
	if err != nil {
		return "", fmt.Errorf("das Bild konnte nicht dekodiert werden: %w", err)
	}

	filename := attachment.Filename
	if strings.TrimSpace(filename) == "" {
		filename = "bild.png"
	}
	// The filter follows the stored picture's own format, because that format is not
	// always PNG: a WebUI can be configured to answer with JPEG, and offering only
	// `*.png` for a JPEG makes the file look like it cannot be saved here.
	pattern, filterName := "*.png", "PNG-Bild"
	switch strings.ToLower(attachment.Mime) {
	case "image/jpeg":
		pattern, filterName = "*.jpg", "JPEG-Bild"
	case "image/webp":
		pattern, filterName = "*.webp", "WebP-Bild"
	case "image/gif":
		pattern, filterName = "*.gif", "GIF-Bild"
	}
	path, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:            "Bild speichern",
		DefaultFilename:  filename,
		DefaultDirectory: a.Settings().Images.SaveFolder,
		Filters: []runtime.FileFilter{
			{DisplayName: filterName, Pattern: pattern},
			{DisplayName: "Alle Dateien", Pattern: "*.*"},
		},
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(path) == "" {
		return "", nil // The user closed the dialog; not an error.
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("Bild konnte nicht gespeichert werden: %w", err)
	}
	// Recorded, because it is the whole difference between a picture its owner has a
	// file of and one that lives only in this database — and that is the question the
	// pictures panel asks (§10). A failure here is not a failed save: the file is on
	// disk and the note is a reading, so it is a warning rather than an error.
	if err := a.store.SetAttachmentSavedPath(attachment.ID, path); err != nil {
		a.emit(events.Warning, map[string]any{
			"warning": "Der Speicherort konnte nicht vermerkt werden: " + err.Error(),
		})
	}

	if dir := filepath.Dir(path); dir != "" && dir != a.Settings().Images.SaveFolder {
		a.settingsMu.Lock()
		a.settings.Images.SaveFolder = dir
		snapshot := a.settings
		a.settingsMu.Unlock()
		if err := a.sanitize(snapshot).Save(a.store); err != nil {
			// Remembering the folder is a convenience, and the picture is already on
			// disk: failing to write it must not read as a failed save.
			a.emit(events.Warning, map[string]any{
				"warning": "Der Zielordner konnte nicht gemerkt werden: " + err.Error(),
			})
		}
	}
	return path, nil
}

// PickSaveFolder opens a folder dialog, for the default export location.
func (a *App) PickSaveFolder() (string, error) {
	if a.ctx == nil {
		return "", errors.New("App ist noch nicht bereit")
	}
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "Ordner für gespeicherte Bilder",
		DefaultDirectory: a.Settings().Images.SaveFolder,
	})
}

// ImageCache is the app's own picture storage as the settings panel reads it.
//
// It answers the two questions a user asks in one breath — where did those pictures go,
// and what may I throw away — from a single walk of the attachment table, so the numbers
// cannot disagree with each other.
//
// **Only pictures KiChat drew count.** A photograph the user dropped into the composer is
// an image attachment too, and it is emphatically not a cache entry: the file it came from
// still exists, this app simply was never told where, and deleting it would take a picture
// out of a conversation to free nothing. The recipe is what tells the two apart — a drawn
// picture records how it was drawn, an attached one has nothing to record.
//
// Unsaved means no export was ever recorded for it, which is not quite the same as "there is
// no copy": a picture saved before that note existed reads exactly like one nobody ever
// saved. The panel says so rather than pretending otherwise, and nothing here deletes
// anything without the user pressing the button that does.
//
// No field is a nullable slice, but `UnsavedChats` is the count that makes the button's
// warning honest: it is how many conversations lose a picture.
type ImageCache struct {
	// Count and Bytes are every picture KiChat drew that its store still holds.
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
	// UnsavedCount and UnsavedBytes are the part of that no export records. They are the
	// only thing the clear button touches, and zero when every picture was saved.
	UnsavedCount int   `json:"unsavedCount"`
	UnsavedBytes int64 `json:"unsavedBytes"`
	// UnsavedChats is how many chats hold at least one of those pictures.
	UnsavedChats int `json:"unsavedChats"`
	// Folder is where the save dialog starts — the place a user already chose for their own
	// pictures, so the panel can point at it while explaining what "saved" means. It is the
	// stored setting, not a live reading: an empty one means the dialog has no default yet.
	Folder string `json:"folder"`
}

// encodedSize is how many bytes a picture's payload occupies in KiChat's own store.
//
// The document holds the payload base64-encoded, so the row is about a third larger
// than the picture itself. Reporting the decoded size would under-tell the space a
// clear actually frees, and "why is my KiChat data directory gigabytes?" is the one
// question this panel exists to answer — so it counts what is on disk, not what the
// file would be once decoded.
func encodedSize(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + 2) / 3 * 4
}

// drawnPictures returns every picture KiChat drew, newest last, in the order the store
// holds them.
//
// It is the one place the store's rows are turned into "a picture this app drew", so the
// status and the clear cannot disagree about what a candidate is: a recipe is present (so
// it is drawn, not attached) and the row is an image attachment.
func (a *App) drawnPictures() ([]store.Attachment, error) {
	all, err := a.store.AllAttachments()
	if err != nil {
		return nil, err
	}
	out := []store.Attachment{}
	for _, attachment := range all {
		if attachment.Generation == nil {
			continue
		}
		if attachment.Kind != string(ingest.KindImage) && !strings.HasPrefix(strings.ToLower(attachment.Mime), "image/") {
			continue
		}
		out = append(out, attachment)
	}
	return out, nil
}

// GetImageCache reports how much of KiChat's own storage is pictures it drew, and how much
// of that only KiChat holds.
func (a *App) GetImageCache() (ImageCache, error) {
	if err := a.ensure(); err != nil {
		return ImageCache{}, err
	}
	cache := ImageCache{Folder: a.Settings().Images.SaveFolder}
	pictures, err := a.drawnPictures()
	if err != nil {
		return ImageCache{}, err
	}
	chats := map[string]bool{}
	for _, picture := range pictures {
		cache.Count++
		cache.Bytes += encodedSize(picture.SizeBytes)
		if picture.SavedPath != "" {
			continue
		}
		cache.UnsavedCount++
		cache.UnsavedBytes += encodedSize(picture.SizeBytes)
		chats[picture.ChatID] = true
	}
	cache.UnsavedChats = len(chats)
	return cache, nil
}

// ImageCacheClear is what clearing the picture cache achieved.
//
// Two numbers rather than a bare count, because they are what the confirmation promised:
// the panel says "14 Bilder, 12 MB" before the click and has to be able to say what really
// went. A picture that could not be deleted is not counted and not silently ignored — the
// error travels, and whatever was removed before it stays removed, because each deletion
// is its own transaction.
type ImageCacheClear struct {
	Removed int   `json:"removed"`
	Bytes   int64 `json:"bytes"`
}

// ClearImageCache deletes every picture KiChat drew that has no recorded export.
//
// This is the one destructive thing in the image panel, and it is deliberately narrow: it
// touches neither an attached file (§ the drawnPictures rule) nor a picture its owner has a
// copy of, and it is never automatic. What it buys is the answer to "why is my KiChat data
// directory gigabytes?" — a session that draws pictures leaves every one of them in the
// store, and a user who never saves one has no other way to say they are finished with it.
//
// Each chat that lost something is told once, at the end, rather than per picture: the event
// makes the view re-read that chat, and one re-read per chat is what it costs either way.
func (a *App) ClearImageCache() (ImageCacheClear, error) {
	if err := a.ensure(); err != nil {
		return ImageCacheClear{}, err
	}
	pictures, err := a.drawnPictures()
	if err != nil {
		return ImageCacheClear{}, err
	}
	result := ImageCacheClear{}
	affected := map[string]bool{}
	for _, picture := range pictures {
		if picture.SavedPath != "" {
			continue
		}
		if err := a.store.DeleteAttachment(picture.ID); err != nil {
			return result, fmt.Errorf("Bild %s entfernen: %w", picture.Filename, err)
		}
		result.Removed++
		result.Bytes += encodedSize(picture.SizeBytes)
		if picture.ChatID != "" {
			affected[picture.ChatID] = true
		}
	}
	for chatID := range affected {
		a.emit(events.Attachments, map[string]any{"chatId": chatID})
	}
	if a.ctx != nil && result.Removed > 0 {
		runtime.LogInfof(a.ctx, "%d nicht gespeicherte Bilder entfernt", result.Removed)
	}
	return result, nil
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
	outcome, err := a.currentChain().Search(ctx, query, research.SearchRequest{})
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
	return a.currentScraper().Scrape(ctx, url, maxTokens)
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
	result, err := a.currentScraper().Scrape(ctx, pageURL, maxTokens)
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
		Providers:    a.currentChain().ProviderAvailability(),
		Order:        settings.Search.ProviderOrder,
		BraveQueries: a.currentChain().BraveQueriesThisMonth(),
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
