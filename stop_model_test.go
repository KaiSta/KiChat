package main

import (
	"path/filepath"
	"runtime"
	"testing"

	"kichat/internal/config"
	"kichat/internal/ingest"
	rt "kichat/internal/runtime"
	"kichat/internal/store"
)

// The chat's picker can start a local model, so it has to be able to release one: a
// managed server holds the whole file in memory, and the panel whose click put it there
// is where giving that memory back belongs. The button is a thin call into the binding
// Settings already uses, so what this pins is not the plumbing but the two things its
// usefulness rests on — a stop writes nothing, so the picker's own start is one click
// away again, and it is safe to press on a server that is not there, because the state
// the button renders from is a reading that may be older than the process.
func TestStoppingFromThePickerKeepsTheEntryStartable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is a shell script")
	}
	root := t.TempDir()
	writeFakeBuild(t, root, "b11535-cpu")
	model := writeModelFile(t, root)

	settings := config.Default()
	settings.Runtime.Tag = "b11535-cpu"
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a := newCleanupTestApp(t, settings, root)
	a.store, a.extractor = st, ingest.New()

	row := func(t *testing.T) ManagedStatus {
		t.Helper()
		status, err := a.GetRuntimeStatus()
		if err != nil {
			t.Fatalf("GetRuntimeStatus: %v", err)
		}
		if len(status.Models) != 1 {
			t.Fatalf("got %d managed rows, want one: %+v", len(status.Models), status.Models)
		}
		return status.Models[0]
	}

	// The picker's own start first, so the stop is pressed on exactly the state a click
	// in the chat produces.
	if err := a.StartManagedModelWith("llamacpp", config.ManagedModel{ModelPath: model, CtxSize: 8192}); err != nil {
		t.Fatalf("StartManagedModelWith: %v", err)
	}
	defer func() { _ = a.StopManagedModel("llamacpp") }()
	waitForRunning(t, a)
	if got := row(t).LoadedModel; got != model {
		t.Fatalf("loaded model = %q, want the file that was started (%q)", got, model)
	}

	if err := a.StopManagedModel("llamacpp"); err != nil {
		t.Fatalf("StopManagedModel: %v", err)
	}
	after := row(t)
	if after.State == rt.StateRunning {
		t.Errorf("the row still reports %q after a stop", after.State)
	}
	// The mark has to go with the process. A stopped server that still named its file
	// would put "in use" on a row for a model nothing is serving, and the Stop button
	// would be offered again for a server that is already down.
	if after.LoadedModel != "" {
		t.Errorf("a stopped server still names %q as loaded, want nothing", after.LoadedModel)
	}
	if after.Port != 0 || after.URL != "" {
		t.Errorf("a stopped server still reports %d / %q, want no port and no address", after.Port, after.URL)
	}

	// Nothing was written: the entry keeps the file, which is the whole reason the
	// stopped model can be started again from the picker without naming it a second
	// time. Switching the entry off — the `enabled` flag — is a Settings decision about
	// the next launch, and a stop is not that decision.
	stored, err := a.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	mm, ok := stored.Runtime.Models["llamacpp"]
	if !ok {
		t.Fatal("the stop dropped the entry: the stopped model would no longer be startable")
	}
	if mm.ModelPath != model {
		t.Errorf("stored model = %q, want the file the start stored (%q)", mm.ModelPath, model)
	}
	if mm.Enabled {
		t.Error("the stop switched the entry on")
	}

	// Pressing Stop twice is a click with nothing left to do, and a stale status can
	// offer it: it must not be an error the section then shows under the list.
	if err := a.StopManagedModel("llamacpp"); err != nil {
		t.Errorf("a second stop reported %v, want the no-op a stale button needs", err)
	}

	// And the entry it wrote is still startable through the panel's own Start, which
	// takes no entry at all — the file has to have survived the stop for that to work.
	if err := a.StartManagedModel("llamacpp"); err != nil {
		t.Fatalf("StartManagedModel after a stop: %v", err)
	}
	waitForRunning(t, a)
	if got := row(t).LoadedModel; got != model {
		t.Errorf("after restarting, loaded model = %q, want %q", got, model)
	}
}

// A stop with nothing to stop is the ordinary case on a row whose reading is older than
// the process — an entry that was never started, a backend id that is not there at all.
// It has to answer without an error and without touching anything: the state line the
// picker renders is what offers the button, and a stale one is not a failure to report.
func TestStoppingSomethingThatIsNotRunningIsNotAnError(t *testing.T) {
	root := t.TempDir()
	model := writeModelFile(t, root)

	settings := config.Default()
	settings.Runtime.Models = map[string]config.ManagedModel{
		"llamacpp": {Enabled: true, ModelPath: model, CtxSize: 4096},
	}
	a := newCleanupTestApp(t, settings, root)

	for _, id := range []string{"llamacpp", "nope", ""} {
		if err := a.StopManagedModel(id); err != nil {
			t.Errorf("StopManagedModel(%q) on a stopped server reported %v, want nil", id, err)
		}
	}

	stored, err := a.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	mm, ok := stored.Runtime.Models["llamacpp"]
	if !ok || mm.ModelPath != model || !mm.Enabled {
		t.Errorf("a stop that had nothing to stop changed the entry: %+v (present %v)", mm, ok)
	}
}
