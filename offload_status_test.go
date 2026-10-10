package main

import (
	"path/filepath"
	"runtime"
	"testing"

	"kichat/internal/config"
	"kichat/internal/ingest"
	rt "kichat/internal/runtime"
	"kichat/internal/secret"
	"kichat/internal/store"
)

// The panel's answer to "is this model actually running on the GPU?" is the
// server's own, and three things have to hold for it to arrive: the app asks for the
// level whose output names a device and a layer count, the supervisor consumes those
// lines as they are written instead of reading the log tail afterwards, and the row
// the UI polls carries the numbers. So the test starts a managed server for real — a
// process, a readiness poll, the bound status call — and reads the row.
//
// The stand-in launcher answers the placement lines only when it was asked for the
// trace level (see runFakeLlamaServer), which is what makes the first half a check of
// the app rather than of the launcher.
func TestAManagedStartReportsWhereTheModelLayersWent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is a shell script")
	}
	root := t.TempDir()
	writeFakeBuild(t, root, "b11535-cpu")

	settings := config.Default()
	settings.Runtime.Tag = "b11535-cpu"
	settings.Runtime.Models = map[string]config.ManagedModel{
		"llamacpp": {
			Enabled: true, ModelPath: writeModelFile(t, root), CtxSize: 4096,
			Offload: config.OffloadAll,
		},
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a := newCleanupTestApp(t, settings, root)
	a.store, a.extractor = st, ingest.New()

	if err := a.StartManagedModel("llamacpp"); err != nil {
		t.Fatalf("StartManagedModel: %v", err)
	}
	defer func() { _ = a.StopManagedModel("llamacpp") }()
	waitForRunning(t, a)

	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	row := status.Models[0]
	if row.OffloadLayers != 34 || row.OffloadTotal != 34 || row.OffloadDevice != "Test0" {
		t.Errorf("the row reports %d/%d on %q, want 34/34 on Test0",
			row.OffloadLayers, row.OffloadTotal, row.OffloadDevice)
	}
	// The values the picker is filled from, so a mode this app can pass cannot go
	// missing from the UI without this failing.
	for _, mode := range []string{config.OffloadAuto, config.OffloadAll, config.OffloadOff} {
		found := false
		for _, offered := range status.OffloadModes {
			if offered == mode {
				found = true
			}
		}
		if !found {
			t.Errorf("the picker is not offered %q: %v", mode, status.OffloadModes)
		}
	}

	// Stopped, the reading goes with the process: a row that names a device for a
	// model that is no longer loaded is worse than no reading at all.
	if err := a.StopManagedModel("llamacpp"); err != nil {
		t.Fatalf("StopManagedModel: %v", err)
	}
	status, err = a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus after the stop: %v", err)
	}
	if row := status.Models[0]; row.OffloadTotal != 0 || row.OffloadDevice != "" {
		t.Errorf("a stopped server still reports a placement: %+v", row)
	}
}

// The picker's value has to survive the trip over the bridge and be stored as one of
// the modes, and a value nothing here knows must be repaired rather than passed on to
// `--gpu-layers`, where llama-server would refuse to start at all.
func TestTheOffloadModeSurvivesTheBridge(t *testing.T) {
	// A real store, because saving a model writes the settings row — the temp file
	// keeps the user's own data directory out of it.
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a := newCleanupTestApp(t, config.Default(), t.TempDir())
	a.store, a.extractor = st, ingest.New()

	mm := config.ManagedModel{Enabled: true, ModelPath: "/models/a.gguf", Offload: config.OffloadOff}
	if err := a.SetManagedModel("llamacpp", mm); err != nil {
		t.Fatalf("SetManagedModel: %v", err)
	}
	saved, gotErr := a.GetSettings()
	if gotErr != nil {
		t.Fatalf("GetSettings: %v", gotErr)
	}
	if got := saved.Runtime.Models["llamacpp"].Offload; got != config.OffloadOff {
		t.Errorf("stored offload = %q, want %q", got, config.OffloadOff)
	}

	// A value nothing here knows is not passed on to `--gpu-layers` — see
	// TestGpuLayersSpellsTheModesTheWayLlamaCppDoes — and it does not survive the
	// next start either: the settings are decoded over a populated default and
	// normalised, which is the same path a blob written by an older version takes.
	// A hand-edited settings row is the way such a value gets in.
	mm.Offload = "gpu"
	if err := a.SetManagedModel("llamacpp", mm); err != nil {
		t.Fatalf("SetManagedModel with an unknown mode: %v", err)
	}
	secrets, err := secret.NewIsolated(filepath.Join(t.TempDir(), "secrets"))
	if err != nil {
		t.Fatalf("NewIsolated: %v", err)
	}
	loaded, err := config.Load(st, secrets)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got := loaded.Runtime.Models["llamacpp"].Offload; got != config.OffloadAuto {
		t.Errorf("an unknown offload came back from the store as %q, want %q", got, config.OffloadAuto)
	}
}

// A device a model was *not* placed on must not reach the panel as one.
//
// The `off` mode is the measured shape of that: `--gpu-layers 0` on the fork's Vulkan
// build still prints `using device Vulkan0 …`, and the layer line then reads `0/65`.
// The panel's only question is whether a device is named, so the answer has to be
// decided before the field crosses the bridge — a plain "send the id, let the UI
// compare" would put the CPU-only case (every layer "offloaded", no device named)
// one comparison away from claiming an accelerator the machine does not have.
func TestAZeroLayerPlacementDoesNotNameADevice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is a shell script")
	}
	root := t.TempDir()
	writeFakeBuild(t, root, "b11535-cpu")

	settings := config.Default()
	settings.Runtime.Tag = "b11535-cpu"
	settings.Runtime.Models = map[string]config.ManagedModel{
		"llamacpp": {
			Enabled: true, ModelPath: writeModelFile(t, root), CtxSize: 4096,
			Offload: config.OffloadOff,
		},
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a := newCleanupTestApp(t, settings, root)
	a.store, a.extractor = st, ingest.New()

	if err := a.StartManagedModel("llamacpp"); err != nil {
		t.Fatalf("StartManagedModel: %v", err)
	}
	defer func() { _ = a.StopManagedModel("llamacpp") }()
	waitForRunning(t, a)

	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	row := status.Models[0]
	if row.OffloadTotal != 34 || row.OffloadLayers != 0 {
		t.Errorf("reading = %d/%d, want the 0/34 the server printed", row.OffloadLayers, row.OffloadTotal)
	}
	if row.OffloadDevice != "" {
		t.Errorf("offloadDevice = %q, want it empty: no layer is on that device", row.OffloadDevice)
	}
}

// A launcher the user supplied is started without the trace level, because `-lv` is
// a flag an older binary may not know and an unknown argument stops a server rather
// than a report. The consequence is that this app has no placement to show there —
// which is the honest answer, and the reason the row's empty reading means "not
// reported" rather than "nothing offloaded".
func TestACustomLauncherIsNotAskedForTheTraceLevel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is a shell script")
	}
	// The same stand-in, but outside the runtime directory and named in BinaryPath —
	// which is the only thing that tells the app it did not install this launcher.
	custom := t.TempDir()
	writeFakeBuild(t, custom, "custom")

	settings := config.Default()
	settings.Runtime.BinaryPath = filepath.Join(custom, "custom", "llama-server")
	settings.Runtime.Models = map[string]config.ManagedModel{
		"llamacpp": {Enabled: true, ModelPath: writeModelFile(t, t.TempDir()), CtxSize: 4096},
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a := newCleanupTestApp(t, settings, t.TempDir())
	a.store, a.extractor = st, ingest.New()

	if err := a.StartManagedModel("llamacpp"); err != nil {
		t.Fatalf("StartManagedModel: %v", err)
	}
	defer func() { _ = a.StopManagedModel("llamacpp") }()
	waitForRunning(t, a)

	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if row := status.Models[0]; row.OffloadTotal != 0 || row.OffloadLayers != 0 {
		t.Errorf("a launcher the app did not install reported a placement: %+v", row)
	}
	if row := status.Models[0]; row.State != rt.StateRunning {
		t.Errorf("the custom launcher did not start: %+v", row)
	}
}
