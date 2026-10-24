package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"kichat/internal/backup"
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
	"kichat/internal/tools"
	"kichat/internal/uimsg"
)

// Event names live in internal/events so the emitters and the frontend's
// subscriber list cannot drift apart; see that package for why.

// App is the bound Wails application object.
type App struct {
	ctx context.Context

	initOnce sync.Once
	initErr  error

	store     *store.Store
	registry  *llm.Registry
	engine    *chat.Engine
	toolSvc   *tools.Service
	chain     *research.Chain
	scraper   *research.Scraper
	extractor *ingest.Extractor
	secrets   *secret.Store
	// imageProvider is the picture server generate_image draws through, nil when
	// image generation is switched off. It is rebuilt with the other services and
	// read under the same lock, for the same reason: a settings save replaces it
	// while a bound call may be using it.
	imageProvider images.Provider

	// servicesMu guards the six services above that a settings save replaces
	// wholesale: registry, chain, scraper, toolSvc, engine and imageProvider. A bound method can be
	// running on another goroutine while that replacement happens, so reading the
	// field as it was being assigned was a data race; every read goes through one of
	// the accessors below and every write happens inside rebuildServices.
	//
	// Replacing the engine is safe for a reply in flight only because the turn state
	// is not part of it — see turns.
	servicesMu sync.RWMutex

	// turns is the turn state every engine this app builds shares, so a rebuild
	// cannot orphan a generation that is already running. See chat.TurnState.
	turns *chat.TurnState

	settingsMu sync.RWMutex
	settings   config.Settings

	// pendingBackup is the archive OpenBackup decrypted and BackupImport writes. It is
	// held between the two calls because the user chooses from it in between, and it is
	// dropped as soon as the import finishes: a decrypted history should not outlive
	// the sheet it was opened for.
	backupMu      sync.Mutex
	pendingBackup *backup.Archive

	dataDir string

	// installer fetches llama-server; servers holds one supervisor per backend
	// entry that KiChat runs itself. The two are separate because the binaries
	// are shared by every managed entry.
	installer *rt.Installer

	// upscalerInstaller fetches Real-ESRGAN into a directory of its own, not into
	// the runtime directory: ListLlamaBuilds walks that one and would list the
	// upscaler as a llama.cpp build that cannot be started. It is the same
	// Installer because what it does — verify a digest, unpack, make the archive's
	// launcher executable, move it into place — is the same job; only the asset's
	// tagged directory and its launcher's name differ.
	upscalerInstaller *rt.Installer
	// upscalerBuild is the pinned archive for one platform. It is a field rather
	// than a direct call so a test can serve a small fixture archive instead of
	// 46 MB from GitHub; nothing but a test ever replaces it.
	upscalerBuild func(goos, goarch string) (images.UpscalerBuild, error)
	// upscalerMu guards the one download and the progress the panel polls through
	// GetUpscalerInstall. It is separate from installMu so a llama.cpp download and
	// an upscaler download cannot overwrite each other's phase or error.
	upscalerMu    sync.Mutex
	upscalerBusy  bool
	upscalerPhase string
	upscalerDone  int64
	upscalerTotal int64
	upscalerErr   string

	serversMu sync.Mutex
	servers   map[string]*rt.Server

	// imageSpec is the command line the managed picture server was last started
	// with, empty when nothing was started. It is what tells a settings change that
	// swapped the model from one that did not, and it is compared as a whole rather
	// than field by field: the argv is the entire input of a start, so a second list
	// of "the fields that matter" is a list that can drift from the one that builds
	// the line.
	imageMu   sync.Mutex
	imageSpec string

	// versions caches what each llama-server reported about itself. The status is
	// polled while a model loads, so asking the binary again on every call would
	// start a process every 1.2 seconds for an answer that does not change; a path
	// is probed once and kept until a save says the binary may have been replaced.
	versionMu sync.Mutex
	versions  map[string]string

	// deviceCache holds the other per-binary reading, for the same reason: which
	// compute devices a launcher can use is a question for a process, and the
	// answer does not change while the app runs.
	deviceMu    sync.Mutex
	deviceCache map[string]deviceReading

	// update is the last look at llama.cpp's releases. It is cached because it is
	// the one question in the runtime panel that costs a request to a rate-limited
	// API: the panel would otherwise ask it again on every poll, and the answer
	// changes once a day.
	updateMu sync.Mutex
	update   RuntimeUpdate

	// install tracks a download in progress. It is read by the UI through a
	// bound method rather than pushed, because a download outlives the call that
	// asked for it and polling cannot miss the end of one.
	installMu    sync.Mutex
	installBusy  bool
	installPhase string
	installDone  int64
	installTotal int64
	installErr   string
	// check tracks a model availability sweep. Like a download it is read by the
	// UI through a bound method rather than pushed: it outlives the call that asked
	// for it, and polling cannot miss the end of one. checkHidden is how many
	// models the sweep that just finished put on the blacklist.
	checkMu      sync.Mutex
	checkBackend string
	checkBusy    bool
	checkDone    int
	checkTotal   int
	checkErr     string
	checkReason  string
	checkHidden  int

	// draws is every image draw in flight, keyed by the attachment it was redrawn from,
	// so a Stop can end the wait on the picture the user is looking at. Each entry
	// carries a token as well as the cancel function: a func value cannot be compared in
	// Go, so the token is what tells a finishing draw whether the entry is still its own.
	// Without it, a second draw of the same picture that started while the first was
	// unwinding would have its entry deleted by the first one's cleanup, and Stop would
	// then find nothing to cancel while a picture was still being drawn.
	drawsMu   sync.Mutex
	draws     map[string]inflightDraw
	drawToken uint64

	// drawSlot is held for the length of every draw this app starts, whichever path started
	// it. One picture at a time is a property of the machine rather than of one path into the
	// feature: every backend here draws on the same GPU, and the CLI and managed kinds pay a
	// model load per draw, so a second draw beside a first is either a queue on the server or
	// two copies of one checkpoint in memory. A TryLock rather than a queue, because the
	// second caller wants an answer it can act on ("one is already running") rather than a
	// picture minutes later that it cannot tell apart from the first.
	drawSlot sync.Mutex

	// installCompleted records that a build was downloaded during this session.
	// A server that was already running keeps the binary it was started from, so
	// without it the panel's "restart the model" line would disappear exactly when
	// the update landed — the moment the restart is owed. A managed start clears
	// it, because that server is running the new build.
	installCompleted bool

	// emitFn, when set, replaces the Wails runtime call in emit. It exists for tests:
	// EventsEmit given a context that is not the one from the lifecycle hooks calls
	// log.Fatalf, which no test can recover from — and a draw now announces itself the
	// moment it starts, so a test that sets a.ctx to run a bound call would otherwise
	// die as soon as a picture began. A test helper installs a no-op.
	emitFn func(name string, payload any)

	// logfFn, when set, replaces the Wails logger in logInfo. It is here for the same
	// reason emitFn is: the runtime's logger calls log.Fatalf when the context did not
	// come from the Wails lifecycle hooks, and a test that runs a bound call sets a.ctx
	// itself. A test helper installs a no-op.
	logfFn func(format string, args ...any)
}

// inflightDraw is one image draw this app is waiting for.
type inflightDraw struct {
	token  uint64
	cancel context.CancelFunc
	// reported is whether the window was told anything about this draw while it ran. It is
	// what makes the end-of-draw event conditional: the event exists to clear a progress row
	// off the screen, and a draw nobody reported on has no row to clear. Guarded by the
	// app's drawsMu rather than by a mutex of its own, because it is only ever read and
	// written under that one.
	reported bool
	// service is the tool service the draw was started on, kept so that Stop asks the
	// same provider that is drawing. A settings save mid-draw replaces the app's
	// toolSvc, and interrupting the replacement would ask a provider that never
	// received this request — while the one that did keeps drawing.
	service *tools.Service
}

// NewApp creates the application.
func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if err := a.ensure(); err != nil {
		runtime.LogErrorf(ctx, "Initialisierung fehlgeschlagen: %v", err)
	}
	a.startEnabledModels()
	// The picture server is off unless the user asked for it, so this does nothing on
	// most installs; see ImageSettings.StartWithApp for why it is not implied by
	// choosing the kind.
	a.startEnabledImages()
}

func (a *App) shutdown(ctx context.Context) {
	// Cancelling is not enough on its own: a turn that is between two writes would
	// go on to write to a database that is about to be closed. The wait is short
	// because it is a courtesy, not a guarantee — a tool call stuck in a syscall
	// cannot be waited out.
	if engine := a.currentEngine(); engine != nil {
		if !engine.StopAllAndWait(3 * time.Second) {
			runtime.LogWarningf(ctx, "laufende Generierungen wurden nach dem Abbruch nicht rechtzeitig beendet")
		}
	}
	// Stopping the managed servers is not optional: they are child processes, and
	// a llama-server left behind holds its model in memory with nothing left in
	// the UI to stop it.
	a.stopAllManaged()
	if a.store != nil {
		a.store.Close()
	}
}

// ensure performs one-time initialisation. Every bound method calls it, so the
// app works even if startup has not completed yet.
func (a *App) ensure() error {
	a.initOnce.Do(func() {
		a.initErr = a.init()
	})
	return a.initErr
}

func (a *App) init() error {
	dir, err := config.DataDir()
	if err != nil {
		// No printable config directory: keep the fallback beside the working
		// directory rather than refusing to start at all.
		dir = "kichat"
	}
	a.dataDir = dir
	// Owner-only: the directory holds the unencrypted database, the encrypted secret
	// fallback and the log. store.Open repairs the mode of an installation that
	// predates this, since MkdirAll leaves an existing directory alone.
	if err := os.MkdirAll(a.dataDir, 0o700); err != nil {
		return fmt.Errorf("Datenverzeichnis anlegen: %w", err)
	}

	st, err := store.Open(filepath.Join(a.dataDir, "kichat.db"))
	if err != nil {
		return err
	}
	a.store = st

	secrets, err := secret.New(filepath.Join(a.dataDir, "secrets"))
	if err != nil {
		return err
	}
	a.secrets = secrets

	settings, err := config.Load(st, secrets)
	if err != nil {
		// A corrupt settings blob should not stop the app from starting.
		settings = config.Default()
	}

	a.settingsMu.Lock()
	a.settings = settings
	a.settingsMu.Unlock()

	a.extractor = ingest.New()
	a.installer = rt.NewInstaller(filepath.Join(a.dataDir, "runtime"))
	a.installer.OnProgress = a.recordInstallProgress
	a.upscalerInstaller = rt.NewInstaller(filepath.Join(a.dataDir, "upscalers", "realesrgan-ncnn-vulkan"))
	a.upscalerInstaller.OnProgress = a.recordUpscalerProgress
	a.upscalerBuild = images.UpscalerBuildFor
	a.servers = map[string]*rt.Server{}
	a.turns = chat.NewTurnState()
	a.rebuildServices(settings)
	return nil
}

// startEnabledModels starts every managed model the user left switched on. It is
// what makes "the app runs the model" true across restarts: nothing else has to
// be started by hand.
func (a *App) startEnabledModels() {
	for id, mm := range a.Settings().Runtime.Models {
		if !mm.Enabled || mm.ModelPath == "" {
			continue
		}
		if err := a.startManagedModel(id); err != nil {
			// Not fatal to the app: the failure is on the entry's status, where
			// the user can read what to fix.
			runtime.LogErrorf(a.ctx, "Modell %s starten: %v", id, err)
		}
	}
}

// startEnabledImages starts the managed picture server if the user asked KiChat to.
//
// It is the only place that starts it without being asked, and it is a setting rather than a
// consequence of the kind: a resident model holds the memory it was loaded into, so an app
// that loaded one on every launch because a folder was configured would be spending the
// user's VRAM on a decision they never made.
func (a *App) startEnabledImages() {
	settings := a.Settings()
	if !settings.Images.Enabled || !config.IsImageSD(settings.Images.Kind) || !settings.Images.StartWithApp {
		return
	}
	if err := a.startImageServer(); err != nil {
		// Not fatal to the app, like a managed chat model that will not start: the
		// reason lands on the row the user can read and fix.
		runtime.LogErrorf(a.ctx, "Bildserver starten: %v", err)
	}
}

// startManagedModel starts the server of one managed backend.
func (a *App) startManagedModel(backendID string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	settings := a.Settings()
	mm, ok := settings.Runtime.Models[backendID]
	if !ok {
		return uimsg.Errorf("model.notConfigured", backendID)
	}
	binary, err := a.llamaServerPath(settings)
	if err != nil {
		return err
	}

	srv := a.serverFor(backendID, settings)
	// The projector is resolved here rather than at save time: it is a file on disk that
	// can be downloaded, moved or replaced after the model was picked, and what a start
	// loads has to be what is there now.
	projector, err := managedProjector(mm)
	if err != nil {
		return err
	}
	// Only the build this app installed is asked for its trace log. The placement
	// reading — which device, how many layers — is printed at that level and nowhere
	// else, but `-lv` is a flag an older binary a user points at may not know, and an
	// unknown argument stops the server rather than a report. A downloaded build is
	// days old and always has it; a path in BinaryPath is of unknown age.
	trace := strings.TrimSpace(settings.Runtime.BinaryPath) == ""
	err = srv.Start(rt.Options{
		Binary:         binary,
		Model:          mm.ModelPath,
		MMProj:         projector,
		Ctx:            mm.CtxSize,
		FlashAttention: mm.FlashAttention,
		CacheTypeK:     mm.CacheTypeK,
		CacheTypeV:     mm.CacheTypeV,
		Offload:        mm.Offload,
		Trace:          trace,
		Extra:          rt.SplitArgs(mm.ExtraArgs),
	})
	if err == nil {
		// The server that just came up runs the launcher that is on disk now, which
		// is what the panel's "restart the model" reminder is waiting for. A start
		// that failed leaves the reminder in place: that model is still on the old
		// build.
		a.clearInstallCompleted()
	}
	return err
}

// managedProjector resolves the multimodal projector for a managed model: the file the
// user named, or the one sitting beside the model when they named none.
//
// A detected file is deliberately *not* written back into the settings. An empty field
// keeps meaning "whatever is beside the model", so a projector replaced by another quant
// — or a whole model folder moved with its projector — is picked up on the next start
// without anyone clearing a stale path first.
func managedProjector(mm config.ManagedModel) (string, error) {
	if named := strings.TrimSpace(mm.MMProj); named != "" {
		// Named but unusable. llama-server would load the whole language model first and
		// only then fail to open the projector, so the answer belongs here, where the
		// sentence can name what the user typed — and say which of the two things is
		// wrong with it, since a path that is there but is a folder sends the reader
		// looking for a file they can see.
		info, statErr := os.Stat(named)
		if statErr != nil {
			return "", uimsg.Errorf("runtime.mmprojMissing", named)
		}
		if info.IsDir() {
			return "", uimsg.Errorf("runtime.mmprojIsFolder", named)
		}
		return named, nil
	}
	return rt.MatchProjector(strings.TrimSpace(mm.ModelPath)), nil
}

// projectorReading is what a status row says about the projector: the file a start would
// load, and whether the app found it beside the model rather than being given it.
//
// An empty path is the honest answer for a text-only model, and it is one the panel says
// out loud — a model that cannot see is worth knowing before an image is attached to a
// turn, since the failure otherwise arrives as the server's own 500.
func projectorReading(mm config.ManagedModel) (path string, detected bool) {
	if named := strings.TrimSpace(mm.MMProj); named != "" {
		return named, false
	}
	found := rt.MatchProjector(strings.TrimSpace(mm.ModelPath))
	return found, found != ""
}

// llamaServerPath resolves the launcher: the user's own copy if they gave one,
// otherwise the build the installer fetched for the selected release source. Which
// build that is, is managedLauncher's question.
func (a *App) llamaServerPath(settings config.Settings) (string, error) {
	if p := strings.TrimSpace(settings.Runtime.BinaryPath); p != "" {
		return p, nil
	}
	if _, binary := a.managedLauncher(settings); binary != "" {
		return binary, nil
	}
	return "", uimsg.Errorf("runtime.notInstalled")
}

// managedLauncher resolves the build a managed start would run for the selected
// source, and is the one place that answers "which build is installed".
//
// The recorded build is preferred over the newest one. Install writes the tag of
// what it just fetched, and that is the build the picker shows; falling straight
// through to Find's newest would run a different family than the panel names the
// moment two families are installed. A tag whose directory is gone falls back, so
// a deleted build is not a dead end.
//
// A recorded tag of the *other* source does not count: upstream's `b11541-cpu` is
// not a build the fork's selection can run, and the two produce different GGUF
// support. Switching the source in the picker and saving therefore does not keep
// running the launcher the switch moved away from — it says no build is installed
// until one is, which is what the download button next to the picker is for.
//
// The status the panel reads and the update comparison go through here too, so the
// build the panel names, the build an update is weighed against, and the build a
// start runs cannot be three different answers. Reading Find's newest for the panel
// while a start ran the recorded tag was exactly that: when the two disagreed, the
// header showed the build that was on disk while the start ran the older one the
// settings still named — and an update the newer build already was could never be
// offered.
func (a *App) managedLauncher(settings config.Settings) (tag, binary string) {
	dist := distributionFor(settings)
	if recorded := strings.TrimSpace(settings.Runtime.Tag); dist.TagOfDir(recorded) != "" {
		if path, ok := a.installer.Launcher(recorded); ok {
			return recorded, path
		}
	}
	return a.installer.Find(dist)
}

// serverFor returns the supervisor of one backend, creating it if needed.
func (a *App) serverFor(backendID string, settings config.Settings) *rt.Server {
	a.serversMu.Lock()
	defer a.serversMu.Unlock()
	if a.servers == nil {
		a.servers = map[string]*rt.Server{}
	}
	srv, ok := a.servers[backendID]
	if !ok {
		srv = rt.NewServer(backendID)
		// The id is captured rather than read back from the status: the handler
		// outlives the settings snapshot it was created from.
		id := backendID
		srv.SetStateHandler(func(st rt.Status) { a.onManagedState(id, st) })
		a.servers[backendID] = srv
	}
	srv.SetReadyTimeout(time.Duration(settings.Runtime.ReadySeconds) * time.Second)
	return srv
}

// onManagedState follows a managed server's address into its backend entry.
//
// A managed server listens on a port chosen when it starts, so the backend the
// user set up cannot hold its address in advance: it has to be written when the
// server is up, and persisted, so the row in Settings and the chat's registry
// both name the server that is actually running. Nothing happens on the way down:
// the address stays as the last one, and the health check reports what it finds.
func (a *App) onManagedState(backendID string, st rt.Status) {
	if st.State != rt.StateRunning || st.URL == "" {
		return
	}
	a.settingsMu.Lock()
	changed := false
	for i := range a.settings.Backends {
		if a.settings.Backends[i].ID != backendID {
			continue
		}
		if a.settings.Backends[i].BaseURL != st.URL || !a.settings.Backends[i].Enabled {
			a.settings.Backends[i].BaseURL = st.URL
			a.settings.Backends[i].Enabled = true
			changed = true
		}
	}
	// Cloned under the lock, and it is the whole point: this function persists the
	// settings, which marshals the value, and marshalling a shallow copy would walk the
	// live maps while the next setter writes them — the managed-model map most of all,
	// since SetManagedModel is what a save does. See config.Settings.Clone.
	settings := a.settings.Clone()
	a.settingsMu.Unlock()
	if !changed {
		return
	}
	if err := a.sanitize(settings).Save(a.store); err != nil {
		runtime.LogErrorf(a.ctx, "Adresse des laufenden Servers speichern: %v", err)
	}
	a.rebuildServices(settings)
}

// versionFor returns the version a llama-server reports, asking it once per path.
//
// An empty answer means the binary could not be read — a damaged download, a
// launcher that hangs, a path that is no longer there. That is kept as "no answer"
// rather than raised: which version is installed is not something a user can act
// on, the reason goes to the log, and the panel says unknown.
func (a *App) versionFor(binary string) string {
	if strings.TrimSpace(binary) == "" {
		return ""
	}
	a.versionMu.Lock()
	version, known := a.versions[binary]
	a.versionMu.Unlock()
	if known {
		return version
	}

	// Long enough for a cold binary on a slow disk, short enough that a launcher
	// which cannot start does not hold a status call open.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	version, err := rt.BinaryVersion(ctx, binary)
	if err != nil {
		version = ""
		if a.ctx != nil {
			runtime.LogWarningf(a.ctx, "Version von %s nicht lesbar: %v", binary, err)
		}
	}

	a.versionMu.Lock()
	if a.versions == nil {
		a.versions = map[string]string{}
	}
	a.versions[binary] = version
	a.versionMu.Unlock()
	return version
}

// forgetVersions drops the cached version readings. Saving settings is what calls
// it: the binary may have been replaced, and the refresh control next to the
// backends is where a user says "look again".
//
// The device readings go with them: they are the same kind of answer about the
// same process, and a save is the moment the launcher may have become a different
// build — one with an accelerator where the last had none.
func (a *App) forgetVersions() {
	a.versionMu.Lock()
	a.versions = map[string]string{}
	a.versionMu.Unlock()
	a.deviceMu.Lock()
	a.deviceCache = map[string]deviceReading{}
	a.deviceMu.Unlock()
}

// cachedUpdate returns the last release lookup, and whether one was made at all.
func (a *App) cachedUpdate() RuntimeUpdate {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	return a.update
}

// setUpdate records one release lookup. An empty reading means "not checked", which
// is what a user-supplied launcher gets: there is nothing on disk this app installed
// to compare a published build against.
func (a *App) setUpdate(reading RuntimeUpdate) {
	a.updateMu.Lock()
	a.update = reading
	a.updateMu.Unlock()
}

// deviceReading is one cached `--list-devices` answer: the devices, why they could
// not be read, and — when there are none — the shared objects the build's own
// accelerator backend cannot resolve.
//
// The three are kept apart because "this build uses no GPU", "this build did not
// answer" and "this build cannot load its backend here" are different things to a
// user: the first is a working CPU setup, the second is a launcher to look at, and
// the third has a cause worth naming.
type deviceReading struct {
	devices []rt.Device
	// missingLibs is what the loader cannot give the accelerator backend, e.g.
	// "libamdhip64.so.7" for a ROCm 10 build on a ROCm 5.7 machine. It is read only
	// when the device list came back empty, which is the only case where it says
	// anything.
	missingLibs []string
	err         string
}

// devicesFor returns the compute devices a launcher reports, asking it once per
// path. Neither list is ever nil, because the panel reads them as arrays.
func (a *App) devicesFor(binary string) deviceReading {
	if strings.TrimSpace(binary) == "" {
		return deviceReading{devices: []rt.Device{}, missingLibs: []string{}}
	}
	a.deviceMu.Lock()
	reading, known := a.deviceCache[binary]
	a.deviceMu.Unlock()
	if known {
		return reading
	}

	// Long enough for a cold binary on a slow disk, short enough that a launcher
	// which cannot start does not hold a status call open.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	devices, err := rt.BinaryDevices(ctx, binary)
	reading = deviceReading{devices: devices, missingLibs: []string{}, err: ""}
	if err != nil {
		reading.devices, reading.err = []rt.Device{}, err.Error()
		if a.ctx != nil {
			runtime.LogWarningf(a.ctx, "Geräte von %s nicht lesbar: %v", binary, err)
		}
	} else if len(devices) == 0 {
		// A build that answered "no devices" has said everything it is going to
		// say: a backend that failed to load is not an error to llama.cpp, it just
		// is not there. The loader's own dependency check is where the reason is,
		// and it is what tells a ROCm 10 build apart from a ROCm 5.7 machine.
		reading.missingLibs = rt.MissingLibraries(ctx, filepath.Dir(binary))
		if a.ctx != nil && len(reading.missingLibs) > 0 {
			runtime.LogWarningf(a.ctx, "Backend von %s nicht ladbar: %s fehlen",
				binary, strings.Join(reading.missingLibs, ", "))
		}
	}

	a.deviceMu.Lock()
	if a.deviceCache == nil {
		a.deviceCache = map[string]deviceReading{}
	}
	a.deviceCache[binary] = reading
	a.deviceMu.Unlock()
	return reading
}

// adoptManagedAddresses restores the address of every backend whose model the app
// is running itself.
//
// A managed server picks its port when it starts, and it is the app that writes
// the address it settled on into the backend entry. A settings view opened before
// that start still holds the previous one, so its Save would put a dead address
// back and the running model would look unreachable until it was started again.
// While the app owns the server it owns the address, so the field is restored here
// rather than trusted from the caller. Nothing happens for a backend the app is not
// running, which is where a hand-started server's address belongs to the user.
func (a *App) adoptManagedAddresses(s config.Settings) config.Settings {
	a.serversMu.Lock()
	defer a.serversMu.Unlock()
	for i := range s.Backends {
		srv := a.servers[s.Backends[i].ID]
		if srv == nil {
			continue
		}
		st := srv.Status()
		if st.State != rt.StateRunning || st.URL == "" {
			continue
		}
		s.Backends[i].BaseURL = st.URL
		s.Backends[i].Enabled = true
	}
	return s
}

// stopAllManaged stops every server the app started.
func (a *App) stopAllManaged() {
	a.serversMu.Lock()
	servers := make([]*rt.Server, 0, len(a.servers))
	for _, srv := range a.servers {
		servers = append(servers, srv)
	}
	a.serversMu.Unlock()
	for _, srv := range servers {
		if err := srv.Stop(); err != nil {
			// The program's own name: this map holds the chat backends' llama-server
			// *and* the picture server, and "llama-server beenden" about a failed
			// sd-server would send a reader looking at the wrong process.
			runtime.LogErrorf(a.ctx, "%s beenden: %v", srv.Program(), err)
		}
	}
}

// managedStatus is one managed entry as the UI asks for it.
type managedStatus struct {
	BackendID string    `json:"backendId"`
	Running   bool      `json:"running"`
	Status    rt.Status `json:"status"`
	Model     string    `json:"model"`
}

// rebuildServices (re)creates the objects that depend on settings.
//
// It runs under servicesMu because a bound method may be reading one of them on
// another goroutine — that is what makes it safe to reach them through the
// accessors below. The engine it installs shares a.turns with the one it replaces,
// so a reply that is streaming right now keeps its claim on its chat: Stop still
// finds it and a second message is still refused.
func (a *App) rebuildServices(settings config.Settings) {
	registry := llm.NewRegistry(settings.Backends)
	chain := research.NewChain(a.store, research.Options{
		ProviderOrder: settings.Search.ProviderOrder,
		BraveAPIKey:   settings.Search.BraveAPIKey,
		Country:       settings.Search.Country,
		SearchLang:    settings.Search.SearchLang,
		ResultCount:   settings.Search.ResultCount,
		ExtraSnippets: settings.Search.ExtraSnippets,
		CacheMinutes:  settings.Search.CacheMinutes,
	}, "")

	scraper := research.NewScraper(a.store, research.ScrapeOptions{
		MaxBytes:          settings.Search.ScrapeMaxBytes,
		Timeout:           time.Duration(settings.Search.ScrapeTimeoutSeconds) * time.Second,
		RespectRobots:     settings.Search.ScrapeRespectRobots,
		AllowPrivateHosts: settings.Search.ScrapeAllowPrivateHosts,
		CacheHours:        settings.Search.ScrapeCacheHours,
	}, a.extractor.PDFExtractorFunc())

	// The image backend is built from the same snapshot. Off means nil, and nil is
	// what removes generate_image from the model's tool list rather than leaving a
	// tool that can only ever fail.
	var imageProvider images.Provider
	if settings.Images.Enabled {
		imageProvider = a.newImageProvider(settings, a.runningImageServerURL())
	}
	toolService := tools.New(a.store, chain, scraper)
	toolService.Images = imageProvider
	toolService.Settings = a.Settings
	// A draw is watched and stoppable wherever it starts. The tool path registers itself here
	// (under the message the call belongs to, which is the row the view shows it on); a redraw
	// registers in RegenerateImage under its attachment. One registry, so one Stop works for
	// a picture that does not exist yet as well as for one that does.
	toolService.Draws = a.registerDraw
	toolService.Progress = a.reportDrawProgress
	toolService.Caption = a.captionImage
	toolService.TryDraw = a.tryDraw
	// A nil turns is only possible before init has run; NewEngineWithState makes its
	// own in that case, so a half-initialised app cannot panic here.
	engine := chat.NewEngineWithState(a.store, registry, toolService, a.Settings, a.emit, a.turns)

	a.servicesMu.Lock()
	a.registry, a.chain, a.scraper, a.toolSvc, a.engine = registry, chain, scraper, toolService, engine
	a.imageProvider = imageProvider
	a.servicesMu.Unlock()

	// Servers survive a settings save: rebuilding services must not restart a
	// model that is already loaded, it only updates the settings each one runs
	// with. So this creates the supervisors that are missing and touches nothing
	// else.
	for id, mm := range settings.Runtime.Models {
		if mm.Enabled || mm.ModelPath != "" {
			a.serverFor(id, settings)
		}
	}
}

// currentEngine is the engine in use, read under the lock that guards it.
func (a *App) currentEngine() *chat.Engine {
	a.servicesMu.RLock()
	defer a.servicesMu.RUnlock()
	return a.engine
}

// currentRegistry is the provider registry in use.
func (a *App) currentRegistry() *llm.Registry {
	a.servicesMu.RLock()
	defer a.servicesMu.RUnlock()
	return a.registry
}

// newImageProvider builds the backend the settings select.
//
// Three kinds, one settings section: the difference is where the work happens, not what the
// panel configures. The server kind contacts an HTTP endpoint the user runs, the two local
// kinds run their binary, and the hosted kind calls build.nvidia.com. Everything downstream —
// the tool, the recipe, the attachment — is written against images.Provider and cannot tell
// them apart, which is what makes this a switch rather than a second feature.
//
// The CLI fields are named one per flag and are passed straight through, empty ones omitted,
// so "which files does this pipeline load" is answered by the same panel that names them. The
// extra arguments go through rt.SplitArgs, the shell-free splitter the managed llama.cpp
// server already uses: this text is a command line the user typed, and the one thing the app
// must never do with it is hand it to a shell. CPU offload travels beside them as a typed
// switch rather than as text, because a user flips it per model and a checkbox says that
// better than remembering a flag.
func (a *App) newImageProvider(settings config.Settings, serverURL string) images.Provider {
	// The hosted kind answers with no local process at all, so its one prerequisite is a
	// key: without one the provider is nil and the tool disappears, which is the same
	// honest state a stopped managed server produces rather than a tool that can only fail.
	if config.IsImageNIM(settings.Images.Kind) {
		key := a.imageAPIKey(settings)
		if key == "" {
			return nil
		}
		return images.NewNIM("images", "NVIDIA NIM", images.NIMBaseURL, key, settings.Images.NIMModel)
	}
	if config.IsImageCLI(settings.Images.Kind) {
		return imageCLI(settings)
	}
	if config.IsImageSD(settings.Images.Kind) {
		// The server KiChat runs itself, while it runs. Stopped, there is no provider at
		// all — and that is the honest state rather than a technicality: a backend that
		// could only answer with "nothing is listening" would put a tool in front of the
		// model that costs a wasted round every time it is called. The panel says so where
		// the Start button is, and starting the server is what brings the tool back, with
		// no restart of the app: the services are rebuilt on every state change.
		//
		// The address is the running server's own rather than the stored BaseURL. A port
		// is picked when the process starts, and the one field that could hold a stale
		// address must not become a way to draw from somebody else's server —
		// 127.0.0.1:1234 is sd-server's own default *and* LM Studio's.
		if strings.TrimSpace(serverURL) == "" {
			return nil
		}
		return images.NewSDWebUI("images", "sd-server", serverURL, settings.Images.APIKey)
	}
	return images.NewSDWebUI("images", "Stable Diffusion WebUI",
		settings.Images.BaseURL, settings.Images.APIKey)
}

// imageAPIKey resolves the credential an image backend draws with.
//
// The hosted kind reads the *NVIDIA backend's* key and nothing else. Its field in the panel is
// the one the user already pasted in the Backends tab, so there is a single token to rotate;
// and the image section's own stored value is not a candidate, because it is the WebUI's
// "user:pass" — sending that pair as a bearer token would produce a 401 from a setup that
// looks configured. The local kinds read nothing here: their credential is that same pair.
func (a *App) imageAPIKey(settings config.Settings) string {
	if !config.IsImageNIM(settings.Images.Kind) {
		return strings.TrimSpace(settings.Images.APIKey)
	}
	if a.secrets == nil {
		return ""
	}
	// Found by *kind* rather than by the id "nvidia": the id is the user's to change, and a
	// row they added themselves (a second account, a self-hosted NIM container on another
	// port) is the same vendor and the same key format. The first row of that kind holding a
	// stored key is what a chat draw would use, so it is what a picture uses.
	for _, backend := range settings.Backends {
		if backend.Kind != "nvidia" {
			continue
		}
		if key, ok := a.secrets.Get(llm.BackendAPIKeyName(backend.ID)); ok {
			if trimmed := strings.TrimSpace(key); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// imageCLI builds the local-build client the *two* local kinds are described by: the
// one-shot program, and the server KiChat starts from the same folder and the same model
// set. One constructor for both, because everything but the lifetime is the same object —
// which is what makes the server's command line the model half of the CLI's own (see
// images.ServerSpec).
func imageCLI(settings config.Settings) *images.CLI {
	return images.NewCLI("images", "sd-cli", settings.Images.CLIBinary,
		images.ModelFiles{
			Model:          settings.Images.CLIModel,
			DiffusionModel: settings.Images.CLIDiffusionModel,
			VAE:            settings.Images.CLIVAE,
			LLM:            settings.Images.CLILLM,
			ClipL:          settings.Images.CLIClipL,
			ClipG:          settings.Images.CLIClipG,
			T5XXL:          settings.Images.CLIT5XXL,
		}, images.Options{
			// Where the program is, when the user pointed KiChat at the folder a
			// release archive unpacked into. The executable is found inside it, so
			// the path nobody has to type is the one nobody can get wrong.
			Folder: settings.Images.CLIFolder,
			// A mode, and the only one this app models itself: the rest of a moving
			// command line's surface stays in the user's own arguments.
			OffloadToCPU: settings.Images.CLIOffloadToCPU,
			Extra:        rt.SplitArgs(settings.Images.CLIExtraArgs),
			// A large second pass is exactly the draw whose latent a small card can
			// sample but not decode in one piece, so the tiling switches belong here
			// with the other load-time options rather than in the extra arguments.
			VaeTiling:   settings.Images.CLIVaeTiling,
			VaeTileSize: settings.Images.CLIVaeTileSize,
			// The profile of the configured model set, so Health can name a missing part by
			// role instead of failing on the first draw with the model already loaded.
			Pipeline: images.PipelineFor(settings.Images.Pipeline, settings.Images.CLIDiffusionModel),
		})
}

// --- the managed picture server ---------------------------------------------

// imageServerID keys the supervised picture server in a.servers. It is not a backend
// id, and nothing that iterates Backends sees it; the map is keyed by string only
// because one supervisor per thing is all the app ever needs.
const imageServerID = "images"

// captionImage asks the turn's own model for the caption a caption-trained image model
// draws from.
//
// The backend and model come from the tool context rather than from a field on the app,
// because two chats can be drawing at once and the service outlives a turn. The provider
// is resolved through the registry that is current at call time for the same reason the
// image backend is rebuilt on every settings change: a save takes effect on the next call,
// not on the next start.
//
// A failure here is never a failed draw — the tool falls back to the user's own request —
// so the errors are plain and meant to be read in a tool result.
func (a *App) captionImage(ctx context.Context, req images.CaptionRequest) (images.CaptionResult, error) {
	turn, ok := tools.TurnFrom(ctx)
	if !ok {
		return images.CaptionResult{}, uimsg.Errorf("image.noCaptionModel")
	}
	provider, ok := a.currentRegistry().Provider(turn.BackendID)
	if !ok {
		return images.CaptionResult{}, uimsg.Errorf("model.backendUnavailable", turn.BackendID)
	}
	return chat.Caption(ctx, provider, turn.ModelID, req)
}

// imageServerFor returns the supervisor of the managed picture server, creating it if
// this is the first time it is needed.
func (a *App) imageServerFor() *rt.Server {
	a.serversMu.Lock()
	defer a.serversMu.Unlock()
	if a.servers == nil {
		a.servers = map[string]*rt.Server{}
	}
	srv, ok := a.servers[imageServerID]
	if !ok {
		srv = rt.NewServer(imageServerID)
		srv.SetStateHandler(a.onImageServerState)
		a.servers[imageServerID] = srv
	}
	// A diffusion model plus its VAE and text encoder is several gigabytes, and the CLI
	// kind pays exactly that load on every single picture — which is why its own bound
	// is half an hour. A start is given the same patience for the same reason.
	srv.SetReadyTimeout(images.DefaultServerReadyTimeout)
	return srv
}

// runningImageServerURL is the address of the supervised server, empty when it is not
// running. It is read when the picture backend is built, so a draw can only ever reach a
// server this app is really running.
func (a *App) runningImageServerURL() string {
	srv := a.existingServer(imageServerID)
	if srv == nil {
		return ""
	}
	st := srv.Status()
	if st.State != rt.StateRunning {
		return ""
	}
	return st.URL
}

// onImageServerState rebuilds the services on every state change.
//
// That is what makes the *tool* follow the process: generate_image is offered only while a
// backend can answer it, so a stop takes the tool away and a start brings it back without a
// restart of the app.
//
// The address is deliberately *not* written into the settings, which is where this differs
// from the managed chat models' own handler. There the address belongs to the backend that
// server serves, so adopting it is exactly how the entry comes to name the running process;
// here ImageSettings.BaseURL is the address of the *other* picture backend, and adopting a
// chosen port into it would rewrite the user's WebUI address as a side effect of running a
// local program. Nothing here needs it: the provider is built from the running server's own
// URL (newImageProvider), which is also what keeps a stale address from ever being drawn
// from, and the panel's row shows that URL out of the process status.
func (a *App) onImageServerState(rt.Status) {
	a.rebuildServices(a.Settings())
}

// startImageServer starts the managed picture server, or restarts it when it is already
// running.
//
// Restarting is the same call because it has to be: sd-server loads one pipeline when it
// starts and documents no way to change it afterwards, so a different model is a different
// process. The app therefore never has to decide between "start" and "restart" — a start
// replaces whatever was running, which is what a user who pressed it meant either way.
func (a *App) startImageServer() error {
	if err := a.ensure(); err != nil {
		return err
	}
	settings := a.Settings()
	if !config.IsImageSD(settings.Images.Kind) {
		return uimsg.Errorf("image.notLocalServer")
	}
	port, err := rt.FreePort()
	if err != nil {
		return fmt.Errorf("freien Port finden: %w", err)
	}
	cli := imageCLI(settings)
	// The `-h` probe happens here, before a process is spawned for real: a build without
	// a server mode has to be refused rather than started on its own default port.
	ctx := a.ctx
	if ctx == nil {
		// A bound call before startup completed has no app context yet; the probe still
		// needs one, and a background context is the right parent for a question that is
		// not part of any user request.
		ctx = context.Background()
	}
	spec, err := cli.ServerSpec(ctx, port)
	if err != nil {
		return err
	}
	err = a.imageServerFor().Start(rt.Options{
		Binary:    spec.Program,
		Model:     spec.Model,
		Args:      spec.Args,
		ReadyPath: images.ServerReadyPath,
		Label:     "sd-server",
		Port:      port,
	})
	if err != nil {
		return err
	}
	a.imageMu.Lock()
	a.imageSpec = cli.ServerKey()
	a.imageMu.Unlock()
	return nil
}

// stopImageServer stops the managed picture server. It is safe when nothing runs.
func (a *App) stopImageServer() error {
	if err := a.ensure(); err != nil {
		return err
	}
	srv := a.existingServer(imageServerID)
	if srv == nil {
		return nil
	}
	return srv.Stop()
}

// imageServerStatus is the managed picture server as the Settings panel reads it.
func (a *App) imageServerStatus(settings config.Settings) ImageServerStatus {
	// A kind with no process of its own reports a stopped row with no address rather
	// than an empty one: the panel renders the same row in both cases, and "stopped" is
	// what a server that was never started is.
	out := ImageServerStatus{
		State:        rt.StateStopped,
		Log:          []string{},
		StartWithApp: settings.Images.StartWithApp,
	}
	srv := a.existingServer(imageServerID)
	if srv == nil {
		return out
	}
	st := srv.Status()
	out.State, out.URL, out.Port, out.PID, out.Error = st.State, st.URL, st.Port, st.PID, st.Error
	if st.Log != nil {
		out.Log = st.Log
	}
	out.RestartNeeded = a.imageServerRestartNeeded()
	return out
}

// imageServerRestartNeeded reports whether the running server was started from a different
// command line than the settings now describe.
//
// It is answered rather than acted on. Saving settings must not restart a server that may
// have spent minutes loading — the same rule the managed chat models follow — so the panel
// is told, and the restart is a click. A server that is not running needs nothing: the next
// start reads the settings anyway.
func (a *App) imageServerRestartNeeded() bool {
	srv := a.existingServer(imageServerID)
	if srv == nil || srv.Status().State != rt.StateRunning {
		return false
	}
	a.imageMu.Lock()
	started := a.imageSpec
	a.imageMu.Unlock()
	if started == "" {
		return false
	}
	settings := a.Settings()
	if !config.IsImageSD(settings.Images.Kind) {
		// The kind was switched away from the local server while it was running. That is
		// a restart's worth of difference in itself: the process is doing a job nothing
		// is asking it to do any more.
		return true
	}
	return started != imageCLI(settings).ServerKey()
}

// imageDrawTimeout bounds one draw by the backend that performs it and the picture it will
// produce.
//
// The two backends are minutes apart because the work is: a server that already holds its
// model in memory spends the whole bound on sampling, while the CLI backend pays a model load
// before the first step — a bound that would be generous for the first cuts the second off
// mid-load and reports it as a failure. Both are guards against a wedged process rather than
// a promise to the user: Stop is what ends a draw that is merely slow.
//
// Both also scale with the output, for the same reason: a picture large enough that the
// backend falls back to CPU sampling is hours of work rather than minutes, and a bound
// written for the card would kill it after the sampling had already been paid for. See
// images.DrawTimeout.
//
// The hosted kind is the exception to the scaling: its bound is about a *network* wait, not
// about arithmetic on this machine, and a request the vendor accepted answers in seconds or
// not at all (measured: one that returned no headers in 240 s). Three minutes is generous for
// the slow case and short enough that a stuck queue reports itself instead of holding the draw
// slot for ten.
func imageDrawTimeout(settings config.Settings, req images.Request) time.Duration {
	if config.IsImageNIM(settings.Images.Kind) {
		return 3 * time.Minute
	}
	if config.IsImageCLI(settings.Images.Kind) {
		return images.DrawTimeout(images.DefaultCLITimeout, req)
	}
	return images.DrawTimeout(10*time.Minute, req)
}

// currentImageProvider is the picture server in use, nil when image generation is
// switched off.
func (a *App) currentImageProvider() images.Provider {
	a.servicesMu.RLock()
	defer a.servicesMu.RUnlock()
	return a.imageProvider
}

// currentToolService is the tool service in use. A redraw goes through it rather than
// through the provider directly, because filing a picture as an attachment is the
// service's job and that is exactly what a redraw has to do with the result.
func (a *App) currentToolService() *tools.Service {
	a.servicesMu.RLock()
	defer a.servicesMu.RUnlock()
	return a.toolSvc
}

// currentChain is the search chain in use.
func (a *App) currentChain() *research.Chain {
	a.servicesMu.RLock()
	defer a.servicesMu.RUnlock()
	return a.chain
}

// currentScraper is the page scraper in use.
func (a *App) currentScraper() *research.Scraper {
	a.servicesMu.RLock()
	defer a.servicesMu.RUnlock()
	return a.scraper
}

// logInfo writes one line to the Wails log, or to whatever a test put in logfFn.
func (a *App) logInfo(format string, args ...any) {
	if a.logfFn != nil {
		a.logfFn(format, args...)
		return
	}
	runtime.LogInfof(a.ctx, format, args...)
}

func (a *App) emit(name string, payload any) {
	if a.ctx == nil {
		return
	}
	if a.emitFn != nil {
		a.emitFn(name, payload)
		return
	}
	runtime.EventsEmit(a.ctx, name, payload)
}

// Settings returns a snapshot of the current settings — a real one, sharing no map or
// slice with the live value (config.Settings.Clone). It has to be a copy of the contents
// and not merely of the struct: callers marshal it (the bridge, the settings blob), iterate
// its maps and build a registry from it, all on their own goroutines, while a setter under
// settingsMu writes the next entry.
func (a *App) Settings() config.Settings {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.settings.Clone()
}

// --- chat API --------------------------------------------------------------

// SendMessageRequest is the payload for SendMessage.
type SendMessageRequest struct {
	ChatID        string   `json:"chatId"`
	Text          string   `json:"text"`
	AttachmentIDs []string `json:"attachmentIds"`
	Backend       string   `json:"backend"`
	Model         string   `json:"model"`
	Regenerate    bool     `json:"regenerate"`
}

// SendMessage starts a generation.
func (a *App) SendMessage(req SendMessageRequest) error {
	if err := a.ensure(); err != nil {
		return err
	}
	if req.Regenerate {
		return a.Regenerate(req.ChatID, req.Backend, req.Model)
	}
	return a.currentEngine().Send(chat.TurnRequest{
		ChatID:        req.ChatID,
		Text:          req.Text,
		AttachmentIDs: req.AttachmentIDs,
		Backend:       req.Backend,
		Model:         req.Model,
	})
}

// Regenerate re-runs the last user turn, optionally on a different model.
func (a *App) Regenerate(chatID, backend, model string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	return a.currentEngine().Send(chat.TurnRequest{
		ChatID:     chatID,
		Backend:    backend,
		Model:      model,
		Regenerate: true,
	})
}

// StopGeneration cancels a running generation, keeping the partial reply.
func (a *App) StopGeneration(chatID string) bool {
	if err := a.ensure(); err != nil {
		return false
	}
	return a.currentEngine().Stop(chatID)
}

// ActiveGenerations lists chats currently generating.
func (a *App) ActiveGenerations() []string {
	if err := a.ensure(); err != nil {
		return nil
	}
	return a.currentEngine().ActiveChats()
}

// ListChats returns all chats, most recent first.
func (a *App) ListChats(includeArchived bool) ([]store.Chat, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	return a.store.ListChats(includeArchived)
}

// CreateChat makes a new chat, starting on the model the settings name and
// falling back to the first available one when nothing is configured.
func (a *App) CreateChat(backend, model string) (store.Chat, error) {
	if err := a.ensure(); err != nil {
		return store.Chat{}, err
	}
	settings := a.Settings()
	if backend == "" || model == "" {
		backend, model = a.defaultChatModel(settings.DefaultModel)
	}
	now := time.Now()
	newChat := store.Chat{
		ID:                 newID(),
		Title:              "",
		CreatedAt:          now,
		UpdatedAt:          now,
		Backend:            backend,
		Model:              model,
		SystemPrompt:       settings.DefaultSystemPrompt,
		SystemPromptPreset: config.PresetResearch,
		Temperature:        settings.Sampler.Temperature,
		TopP:               settings.Sampler.TopP,
		AutoCompact:        true,
		CompactThreshold:   settings.Compact.Threshold,
		KeepRecentTurns:    settings.Compact.KeepRecentTurns,
	}
	if err := a.store.CreateChat(newChat); err != nil {
		return store.Chat{}, err
	}
	a.emit(events.ChatsChanged, map[string]any{})
	return newChat, nil
}

// defaultChatModel picks the model a new chat opens on: the one the settings name
// if it is still in the catalogue, otherwise a loaded model, otherwise the first
// one listed.
//
// The configured default is checked against the catalogue rather than trusted,
// because a model can stop being there without the user touching this setting —
// its backend gets switched off, its server stops publishing it, or the
// availability check hides it. A chat created on a ref nothing serves would answer
// its first message with an error and put a name in the picker that is not in the
// list; the fallback is the behaviour every older install already had.
func (a *App) defaultChatModel(preferred llm.Ref) (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models := a.visibleModels(ctx)
	if preferred.Backend != "" && preferred.Model != "" {
		for _, m := range models {
			if m.Backend == preferred.Backend && m.ID == preferred.Model {
				return m.Backend, m.ID
			}
		}
	}
	for _, m := range models {
		if m.State == llm.StateLoaded && m.Type != "embeddings" {
			return m.Backend, m.ID
		}
	}
	for _, m := range models {
		if m.Type != "embeddings" {
			return m.Backend, m.ID
		}
	}
	return "", ""
}

// visibleModels is the merged catalogue with user overrides applied and
// embedding-only models filtered out of the picker.
//
// It also drops the blacklist the availability check wrote. That filtering
// happens here rather than in the provider on purpose: every listing path — the
// picker, the budget preview, the default model of a new chat — comes through
// this function, so one filter covers all of them, and a backend keeps reporting
// the models it really publishes. A chat that is already pinned to a hidden model
// still resolves it, because resolving goes to the registry; hiding is a
// statement about the picker, not a revocation.
func (a *App) visibleModels(ctx context.Context) []llm.ModelInfo {
	settings := a.Settings()
	hidden := make(map[string]bool, len(settings.HiddenModels))
	for _, key := range settings.HiddenModels {
		hidden[key] = true
	}
	models := llm.MergeOverrides(a.currentRegistry().ListModels(ctx, false), settings.ModelOverrides)
	out := make([]llm.ModelInfo, 0, len(models))
	for _, m := range models {
		if m.Type == "embeddings" {
			continue
		}
		if hidden[m.Backend+"/"+m.ID] {
			continue
		}
		// Fill in capability hints when a backend reports none, so the UI can
		// still show something useful instead of a blank badge row.
		if len(m.Capabilities) == 0 {
			m.Capabilities = inferCapabilities(m)
		}
		out = append(out, m)
	}
	return out
}

// inferCapabilities guesses capabilities when a backend does not report them.
// The guess only affects the badges and whether native tool calling is used; the
// text-protocol fallback still works either way.
func inferCapabilities(m llm.ModelInfo) []string {
	caps := []string{}
	if m.Type == "vlm" {
		caps = append(caps, llm.CapVision)
	}
	return caps
}

// GetChat loads one chat.
func (a *App) GetChat(chatID string) (store.Chat, error) {
	if err := a.ensure(); err != nil {
		return store.Chat{}, err
	}
	return a.store.GetChat(chatID)
}

// UpdateChat persists chat settings (model, system prompt, samplers, flags).
func (a *App) UpdateChat(c store.Chat) (store.Chat, error) {
	if err := a.ensure(); err != nil {
		return store.Chat{}, err
	}
	existing, err := a.store.GetChat(c.ID)
	if err != nil {
		return store.Chat{}, err
	}
	// A model or backend change invalidates the recorded prompt size, because a
	// different tokenizer makes the old count meaningless.
	if existing.Backend != c.Backend || existing.Model != c.Model {
		a.currentEngine().InvalidateMeasurement(c.ID)
	}
	if err := a.store.UpdateChat(c); err != nil {
		return store.Chat{}, err
	}
	a.emit(events.ChatsChanged, map[string]any{})
	return a.store.GetChat(c.ID)
}

// RenameChat sets a chat title.
func (a *App) RenameChat(chatID, title string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	if err := a.store.SetChatTitle(chatID, title); err != nil {
		return err
	}
	a.emit(events.ChatsChanged, map[string]any{})
	return nil
}

// DeleteChat removes a chat and its data.
func (a *App) DeleteChat(chatID string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	a.currentEngine().Stop(chatID)
	if err := a.store.DeleteChat(chatID); err != nil {
		return err
	}
	a.currentEngine().InvalidateMeasurement(chatID)
	a.emit(events.ChatsChanged, map[string]any{})
	return nil
}

// ChatState bundles everything the UI needs for one chat in a single call,
// which keeps the message list, budget, compactions and attachments consistent.
type ChatState struct {
	Chat        store.Chat         `json:"chat"`
	Messages    []store.Message    `json:"messages"`
	Compactions []store.Compaction `json:"compactions"`
	Attachments []store.Attachment `json:"attachments"`
	Documents   []store.Document   `json:"documents"`
	Budget      chat.ContextBudget `json:"budget"`
	Running     bool               `json:"running"`
}

// GetChatState returns the full state of a chat.
func (a *App) GetChatState(chatID string) (ChatState, error) {
	if err := a.ensure(); err != nil {
		return ChatState{}, err
	}
	c, err := a.store.GetChat(chatID)
	if err != nil {
		return ChatState{}, err
	}
	messages, err := a.store.ListMessages(chatID)
	if err != nil {
		return ChatState{}, err
	}
	compactions, err := a.store.ListCompactions(chatID)
	if err != nil {
		return ChatState{}, err
	}
	attachments, err := a.store.ListAttachments(chatID)
	if err != nil {
		return ChatState{}, err
	}
	documents, err := a.store.ListDocuments(chatID)
	if err != nil {
		return ChatState{}, err
	}
	budget, err := a.GetBudget(chatID, "", "")
	if err != nil {
		budget = chat.ContextBudget{}
	}
	return ChatState{
		Chat:        c,
		Messages:    messages,
		Compactions: compactions,
		Attachments: attachments,
		Documents:   documents,
		Budget:      budget,
		Running:     a.currentEngine().IsRunning(chatID),
	}, nil
}

// GetBudget computes the context budget for a chat, optionally for a different
// model than the one currently selected. That is what lets the model switcher
// warn about a smaller window before the user commits to the switch.
//
// The budget is computed from the catalogue already in hand, never by asking the
// backends: this is a preview that the UI awaits while a chat opens, and a listing
// request there could stall the window behind a slow one (see
// chat.Engine.PreviewBudget).
func (a *App) GetBudget(chatID, backend, model string) (chat.ContextBudget, error) {
	if err := a.ensure(); err != nil {
		return chat.ContextBudget{}, err
	}
	return a.currentEngine().PreviewBudget(context.Background(), chatID, backend, model)
}

// newID returns a fresh identifier.
func newID() string { return uuid.NewString() }
