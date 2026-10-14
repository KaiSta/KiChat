package main

import (
	"path/filepath"
	"runtime"
	"testing"

	"kichat/internal/config"
	"kichat/internal/ingest"
	"kichat/internal/store"
)

// A running llama-server is not reconfigured by picking another file: the model in
// memory stays loaded until the server is started again. The panel needs to be able
// to say which of those two a file is — the one being served, or the one selected —
// and this test drives a real managed start to pin that the row carries the served
// one.
//
// The three states matter, and each is checked: idle (nothing is loaded), running
// (the file the start was given), and after a re-pick (the selection has moved, the
// loaded file has not). The stop is the one that keeps the field honest — the
// supervisor keeps the model in its status once the process is gone, so a row that
// carried it regardless would mark a file as "in use" on a server that is not
// answering at all.
func TestTheRowNamesTheFileTheRunningServerLoaded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is a shell script")
	}
	root := t.TempDir()
	writeFakeBuild(t, root, "b11535-cpu")
	first := writeModelFile(t, root)

	settings := config.Default()
	settings.Runtime.Tag = "b11535-cpu"
	settings.Runtime.Models = map[string]config.ManagedModel{
		"llamacpp": {Enabled: true, ModelPath: first, CtxSize: 4096},
	}
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

	// Idle: nothing is loaded, so the listing has nothing to mark. An empty field is
	// the answer here, not the selected path — a stopped row that named a model would
	// put an "in use" badge on a file nothing is serving.
	if got := row(t).LoadedModel; got != "" {
		t.Errorf("a stopped server names %q as loaded, want nothing", got)
	}

	if err := a.StartManagedModel("llamacpp"); err != nil {
		t.Fatalf("StartManagedModel: %v", err)
	}
	defer func() { _ = a.StopManagedModel("llamacpp") }()
	waitForRunning(t, a)
	if got := row(t).LoadedModel; got != first {
		t.Errorf("loaded model = %q, want the file the start was given (%q)", got, first)
	}

	// The pair the panel has to tell apart. Saving another model does not restart
	// anything — that is why a started model is not torn down by every save — so the
	// stored path moves first, and the loaded one still names the file in memory.
	second := writeModelFile(t, t.TempDir())
	if err := a.SetManagedModel("llamacpp", config.ManagedModel{Enabled: true, ModelPath: second, CtxSize: 4096}); err != nil {
		t.Fatalf("SetManagedModel: %v", err)
	}
	after := row(t)
	if after.ModelPath != second {
		t.Errorf("stored model path = %q, want the newly saved %q", after.ModelPath, second)
	}
	if after.LoadedModel != first {
		t.Errorf("loaded model = %q, want the file the running server still serves (%q)", after.LoadedModel, first)
	}

	// And the mark goes with the process.
	if err := a.StopManagedModel("llamacpp"); err != nil {
		t.Fatalf("StopManagedModel: %v", err)
	}
	if got := row(t).LoadedModel; got != "" {
		t.Errorf("a stopped server still names %q as loaded", got)
	}
}
