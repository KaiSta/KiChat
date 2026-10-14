package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"kichat/internal/config"
	"kichat/internal/ingest"
	"kichat/internal/store"
)

// Starting a local model from the chat's picker is one call, and it has to leave the
// settings in the state the Settings panel would leave them in: the entry names the
// file, every other field is kept, and `enabled` is not quietly turned on — loading a
// model on every app start is a decision the checkbox makes, not one a start makes on
// the user's behalf.
//
// This drives a real managed start with the stand-in launcher, like the other runtime
// tests, so what is asserted is the same path the panel's own Start button takes.
func TestStartingFromThePickerNamesTheFileWithoutEnablingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is a shell script")
	}
	root := t.TempDir()
	writeFakeBuild(t, root, "b11535-cpu")
	model := writeModelFile(t, root)

	settings := config.Default()
	settings.Runtime.Tag = "b11535-cpu"
	// Nothing configured at all: this is a fresh install, which is exactly the state
	// the feature exists for — there is no entry to edit in Settings yet.
	if len(settings.Runtime.Models) != 0 {
		t.Fatalf("the fixture starts with %d managed entries, want none", len(settings.Runtime.Models))
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a := newCleanupTestApp(t, settings, root)
	a.store, a.extractor = st, ingest.New()

	// What the picker sends: the defaults the panel shows for a new entry, with the
	// file it was asked for.
	entry := config.ManagedModel{ModelPath: model, CtxSize: 8192}
	if err := a.StartManagedModelWith("llamacpp", entry); err != nil {
		t.Fatalf("StartManagedModelWith: %v", err)
	}
	defer func() { _ = a.StopManagedModel("llamacpp") }()
	waitForRunning(t, a)

	stored, err := a.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	mm, ok := stored.Runtime.Models["llamacpp"]
	if !ok {
		t.Fatal("the started file was not stored: the next start would load nothing")
	}
	if mm.ModelPath != model {
		t.Errorf("stored model = %q, want the file the picker started (%q)", mm.ModelPath, model)
	}
	if mm.CtxSize != 8192 {
		t.Errorf("stored ctx size = %d, want the one the picker sent", mm.CtxSize)
	}
	if mm.Enabled {
		t.Error("a start from the picker switched the entry on: loading this model at every app start is the checkbox's decision")
	}

	// The model the entry runs is the one the row reports, which is what the picker
	// marks as in use while the server is up.
	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if len(status.Models) != 1 || status.Models[0].LoadedModel != model {
		t.Errorf("rows = %+v, want one naming %q as loaded", status.Models, model)
	}
}

// A second pick restarts the server onto the other file: one call has to be enough to
// switch a running model, because that is what a click in the picker means.
func TestASecondPickRestartsOntoTheOtherFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is a shell script")
	}
	root := t.TempDir()
	writeFakeBuild(t, root, "b11535-cpu")
	first := writeModelFile(t, root)
	second := writeModelFile(t, t.TempDir())

	settings := config.Default()
	settings.Runtime.Tag = "b11535-cpu"
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a := newCleanupTestApp(t, settings, root)
	a.store, a.extractor = st, ingest.New()

	entry := config.ManagedModel{ModelPath: first, CtxSize: 8192}
	if err := a.StartManagedModelWith("llamacpp", entry); err != nil {
		t.Fatalf("first StartManagedModelWith: %v", err)
	}
	defer func() { _ = a.StopManagedModel("llamacpp") }()
	waitForRunning(t, a)

	entry.ModelPath = second
	if err := a.StartManagedModelWith("llamacpp", entry); err != nil {
		t.Fatalf("second StartManagedModelWith: %v", err)
	}
	waitForRunning(t, a)

	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if len(status.Models) != 1 {
		t.Fatalf("got %d managed rows, want one: %+v", len(status.Models), status.Models)
	}
	if got := status.Models[0].LoadedModel; got != second {
		t.Errorf("loaded model = %q, want the file picked second (%q)", got, second)
	}
	if got := status.Models[0].ModelPath; got != second {
		t.Errorf("stored model = %q, want %q", got, second)
	}
}

// The entry has to be one this app runs. A picker that started llama-server for
// `lmstudio` would leave a stray process listening on a port nothing chats to, and the
// mistake would be silent — so the kind is refused by name, before anything is written.
func TestOnlyTheEntryKiChatRunsCanBeStartedThisWay(t *testing.T) {
	model := writeModelFile(t, t.TempDir())
	a := newCleanupTestApp(t, config.Default(), t.TempDir())

	err := a.StartManagedModelWith("lmstudio", config.ManagedModel{ModelPath: model, CtxSize: 8192})
	if err == nil {
		t.Fatal("starting a model on a backend KiChat only talks to was accepted")
	}
	if !strings.Contains(err.Error(), "LM Studio") {
		t.Errorf("the refusal reads %q, want the entry named so the row can say which one", err)
	}

	// A file that is not there is refused as well, and nothing about the entry is left
	// behind: a stored path that does not exist fails every later start too, including
	// the automatic one at launch.
	gone := filepath.Join(t.TempDir(), "not-there.gguf")
	if err := a.StartManagedModelWith("llamacpp", config.ManagedModel{ModelPath: gone, CtxSize: 8192}); err == nil {
		t.Fatal("a start on a file that is not there was accepted")
	}
	saved, err := a.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if mm, ok := saved.Runtime.Models["llamacpp"]; ok && mm.ModelPath != "" {
		t.Errorf("the failed start stored %q, want nothing stored", mm.ModelPath)
	}
	if srv := a.existingServer("llamacpp"); srv != nil && srv.Status().State == "running" {
		t.Error("a refused start left a server running")
	}
}
