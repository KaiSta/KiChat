package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"kichat/internal/config"
	"kichat/internal/ingest"
	rt "kichat/internal/runtime"
	"kichat/internal/store"
)

// The projector is the half of a vision model that llama.cpp will not load on its own:
// a server started with only the language model answers every image with 500 "image
// input is not supported", measured on a real Qwen3-VL-8B through the app's own
// supervisor. So the app pairs the two files itself, and these tests are about the part
// that cannot be seen from the settings screen: that the pairing reaches the *command
// line* of the process that is really started, and that a model with no projector beside
// it is not handed a flag at all.

// managedTestApp builds the app a managed start runs on: a stand-in launcher in the
// runtime directory, the model configured as the user configured it, and a store for the
// settings row the save path writes.
func managedTestApp(t *testing.T, mm config.ManagedModel) *App {
	t.Helper()
	root := t.TempDir()
	writeFakeBuild(t, root, "b11535-cpu")

	mm.Enabled = true
	if mm.CtxSize == 0 {
		mm.CtxSize = 4096
	}
	settings := config.Default()
	settings.Runtime.Tag = "b11535-cpu"
	settings.Runtime.Models = map[string]config.ManagedModel{"llamacpp": mm}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := newCleanupTestApp(t, settings, root)
	a.store, a.extractor = st, ingest.New()
	return a
}

// startManagedForArgv starts one managed model and returns its status row together with
// the argument list of the process that is actually running.
//
// The command line is read from /proc rather than from the Options the app built, because
// the bug this covers is a field that is resolved, passed and then dropped on the way to
// exec — which is exactly the kind of mistake a test of the options struct cannot see.
func startManagedForArgv(t *testing.T, mm config.ManagedModel) (ManagedStatus, []string) {
	t.Helper()
	a := managedTestApp(t, mm)

	if err := a.StartManagedModel("llamacpp"); err != nil {
		t.Fatalf("StartManagedModel: %v", err)
	}
	t.Cleanup(func() { _ = a.StopManagedModel("llamacpp") })
	waitForRunning(t, a)

	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	srv := a.existingServer("llamacpp")
	if srv == nil {
		t.Fatal("the started model has no supervisor")
	}
	pid := srv.Status().PID
	if pid == 0 {
		t.Fatalf("the running server reports no pid: %+v", srv.Status())
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		t.Fatalf("read the command line of pid %d: %v", pid, err)
	}
	return status.Models[0], strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
}

// TestAManagedStartLoadsTheProjectorBesideTheModel is the whole feature end to end: the
// user picks the language model, and the app finds the projector in the same folder, says
// so in the row, and starts llama-server with it.
func TestAManagedStartLoadsTheProjectorBesideTheModel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the command line is read from /proc")
	}
	models := t.TempDir()
	model := writeModelFile(t, models)
	// LM Studio's own spelling, beside the model it belongs to.
	projector := filepath.Join(models, "mmproj-weights-F16.gguf")
	if err := os.WriteFile(projector, []byte("GGUF"), 0o644); err != nil {
		t.Fatal(err)
	}

	row, argv := startManagedForArgv(t, config.ManagedModel{ModelPath: model})

	if !hasFlagValue(argv, "--mmproj", projector) {
		t.Errorf("the running server's command line is %v, want --mmproj %s", argv, projector)
	}
	if row.MMProj != projector || !row.MMProjDetected {
		t.Errorf("the row reports %q (detected=%v), want the projector beside the model %q",
			row.MMProj, row.MMProjDetected, projector)
	}
}

// TestAStartWithoutAProjectorSendsNoFlag: a text-only model stays text-only. An empty
// `--mmproj` is a path llama.cpp would try to open, so "not found" has to mean the flag is
// absent — and the row has to say so, which is what the panel reads as "this model cannot
// see".
func TestAStartWithoutAProjectorSendsNoFlag(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the command line is read from /proc")
	}
	models := t.TempDir()
	model := writeModelFile(t, models)

	row, argv := startManagedForArgv(t, config.ManagedModel{ModelPath: model})

	for _, arg := range argv {
		if arg == "--mmproj" {
			t.Errorf("a model with no projector beside it was given the flag: %v", argv)
		}
	}
	if row.MMProj != "" || row.MMProjDetected {
		t.Errorf("the row reports %q (detected=%v), want no projector", row.MMProj, row.MMProjDetected)
	}
}

// TestATypedProjectorIsUsedAndIsNotReportedAsDetected: a projector somewhere else is what
// the second field is for — the two halves are not always downloaded together — and the row
// must not claim the app found it on its own, because that is the badge a user reads as
// "nothing to do here".
func TestATypedProjectorIsUsedAndIsNotReportedAsDetected(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the command line is read from /proc")
	}
	models := t.TempDir()
	model := writeModelFile(t, models)
	elsewhere := t.TempDir()
	typed := filepath.Join(elsewhere, "mmproj-weights-BF16.gguf")
	if err := os.WriteFile(typed, []byte("GGUF"), 0o644); err != nil {
		t.Fatal(err)
	}

	row, argv := startManagedForArgv(t, config.ManagedModel{ModelPath: model, MMProj: typed})

	if !hasFlagValue(argv, "--mmproj", typed) {
		t.Errorf("the running server's command line is %v, want --mmproj %s", argv, typed)
	}
	if row.MMProj != typed || row.MMProjDetected {
		t.Errorf("the row reports %q (detected=%v), want the typed projector %q reported as typed",
			row.MMProj, row.MMProjDetected, typed)
	}
}

// TestAMissingProjectorIsRefusedBeforeTheModelLoads: a typed path that is gone fails here,
// naming the file, rather than at llama.cpp — which would load five gigabytes of weights
// first and then die on the projector, leaving the row to report the server's own words
// about a file the user cannot see in the panel.
func TestAMissingProjectorIsRefusedBeforeTheModelLoads(t *testing.T) {
	models := t.TempDir()
	model := writeModelFile(t, models)
	missing := filepath.Join(models, "mmproj-gone.gguf")
	a := managedTestApp(t, config.ManagedModel{ModelPath: model, MMProj: missing})

	err := a.StartManagedModel("llamacpp")
	if err == nil {
		t.Fatal("a start with a projector that is not there was allowed")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("the error is %q, want it to name %s", err, missing)
	}
	status, statusErr := a.GetRuntimeStatus()
	if statusErr != nil {
		t.Fatalf("GetRuntimeStatus: %v", statusErr)
	}
	if state := status.Models[0].State; state != rt.StateStopped {
		t.Errorf("the model is %q after a refused start, want it stopped", state)
	}
}

// hasFlagValue reports whether a flag is followed by this value in a real argv.
func hasFlagValue(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}
