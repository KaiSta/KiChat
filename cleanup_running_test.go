package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/ingest"
	rt "kichat/internal/runtime"
	"kichat/internal/store"
)

// fakeServerEnv turns this test binary into a stand-in for llama-server, and
// fakeBuildEnv tells it which build it is: the version probe is run without a working
// directory of its own, so the build number cannot be read from the path there.
const (
	fakeServerEnv = "KICHAT_TEST_FAKE_LLAMA_SERVER"
	fakeBuildEnv  = "KICHAT_TEST_FAKE_LLAMA_BUILD"
)

// TestMain gives the tests a launcher they can start for real.
//
// The guard that refuses to delete the build a running server was started from cannot
// be exercised with a launcher that exits: readiness is the server's own /health, so
// "running" has to be reached for real, and the supervisor holds the launcher path it
// started. Instead of reaching for python or netcat — which a machine may not have —
// the test binary plays the server: the script the tests write re-executes this same
// binary with fakeServerEnv set, and it then answers /health, --version and
// --list-devices the way llama.cpp does. The process is real, the readiness poll is
// real, and nothing outside the Go toolchain has to be installed for it.
func TestMain(m *testing.M) {
	if os.Getenv(fakeServerEnv) == "1" {
		runFakeLlamaServer()
		return
	}
	// The same trick for the other managed server: see image_server_test.go. One
	// test binary plays both stand-ins, chosen by which variable is set.
	if os.Getenv(fakeSDServerEnv) == "1" {
		runFakeSDServer()
		return
	}
	os.Exit(m.Run())
}

// runFakeLlamaServer answers the three questions the app asks a launcher, and serves
// /health when it was started as a server.
func runFakeLlamaServer() {
	args := os.Args[1:]
	port := 0
	for i, arg := range args {
		if arg == "--port" && i+1 < len(args) {
			port, _ = strconv.Atoi(args[i+1])
		}
	}
	if port == 0 {
		// It was probed rather than started: `llama-server --version` for the panel's
		// version line, or `--list-devices` for the memory warning. A launcher of one
		// build reports that build's number, so the fake reports the same one — the
		// app reads both and the two have to agree.
		switch {
		case hasArg(args, "--list-devices"):
			fmt.Println("Available devices:")
			fmt.Println("  Test0: A Card (20464 MiB, 18418 MiB free)")
		default:
			fmt.Printf("version: 0.6.0-dev (build %s, commit deadbeef0)\n", fakeBuildNumber())
		}
		return
	}
	// What a real server says about the placement, and only at the level it is asked
	// for. Measured on b11541 and on the fork's prism-b10770: at the default level
	// (3, info) a model load prints nothing about a device, while at `-lv 4` it
	// prints both of these. A stand-in that printed them unconditionally would
	// answer a question the real server only answers when it is asked, and the test
	// that checks the app asks would then pass for the wrong reason.
	if hasArgValue(args, "-lv", "4") {
		// A `--gpu-layers 0` run, which is what the app's `off` mode sends, names the
		// device it would have used and then reports no layer on it — measured on the
		// fork's Vulkan build. That shape is what the app must not read as a GPU.
		layers := "34"
		if hasArgValue(args, "--gpu-layers", "0") {
			layers = "0"
		}
		fmt.Println("0.00.479.028 I llama_prepare_model_devices: using device Test0 (A Card) (0000:0c:00.0) - 18418 MiB free")
		fmt.Printf("0.00.878.017 I load_tensors: offloaded %s/34 layers to GPU\n", layers)
	}

	// A leak guard: the launcher is a child of the test process, so a test that dies
	// without its deferred stop — a panic, a killed run — would otherwise leave this
	// stand-in listening on a loopback port for as long as the machine is up. No test
	// here runs a server for a minute.
	time.AfterFunc(time.Minute, func() { os.Exit(0) })

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	// Anything else is refused rather than answered: a chat against this stand-in is
	// not what any test here does, and a 404 says so louder than an empty 200.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	_ = http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", port), mux)
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// hasArgValue reports whether a flag was passed with exactly that value, which is how
// a stand-in decides what the real server would have printed.
func hasArgValue(args []string, flag, value string) bool {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// fakeBuildNumber is the build the launcher script belongs to, as the script recorded
// it when the test wrote it.
func fakeBuildNumber() string {
	if build := os.Getenv(fakeBuildEnv); build != "" {
		return build
	}
	return "0"
}

// The guard that a fake launcher could not prove: a build a managed server is answering
// from must not be deleted, even when it is no longer the build the settings record.
//
// That is the state a build chosen in the picker leaves behind — the settings name one
// build while the loaded model is running on another — and it is the one case where "in
// use" is not enough to protect a directory: the server holds those files open, so on
// Windows the delete would not even succeed, and on Linux it would leave a model that
// cannot answer its next request. So the running build is refused by name, and the
// one-press cleanup has to skip it rather than take the model down with it.
func TestARunningServerProtectsTheBuildItWasStartedFrom(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is a shell script")
	}
	root := t.TempDir()
	// The build the model is started from, and the newest on disk — which is what the
	// panel's version line describes, so it has to name this one.
	writeFakeBuild(t, root, "b11535-cpu")
	// The one the settings end up naming: an older build, picked by hand. Its launcher
	// is deliberately older than the other's, because Find, the version row and the
	// default the settings fall back to all read "newest" from the modification time.
	makeBuild(t, root, "b11528-cpu")
	older := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "b11528-cpu", "llama-server"), older, older); err != nil {
		t.Fatal(err)
	}

	settings := config.Default()
	settings.Runtime.Tag = "b11535-cpu"
	settings.Runtime.Models = map[string]config.ManagedModel{
		"llamacpp": {Enabled: true, ModelPath: writeModelFile(t, root), CtxSize: 4096},
	}
	// A real store and extractor, because a running server reaches the code that
	// writes its address into the backend entry: that path saves the settings, and a
	// test that skipped it would be testing a state the app is never in. The database
	// is a temp file, so the user's own data directory stays untouched.
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
	// A failed assertion must not leave a process behind.
	defer func() { _ = a.StopManagedModel("llamacpp") }()

	if version := waitForRunning(t, a); !strings.Contains(version, "11535") {
		t.Errorf("version = %q, want the version probe to name the build the launcher was started from (11535)", version)
	}

	// The picker: the settings now record the other build, which is exactly what makes
	// the running one a build only the running guard can save.
	a.settingsMu.Lock()
	a.settings.Runtime.Tag = "b11528-cpu"
	a.settingsMu.Unlock()

	builds, err := a.ListLlamaBuilds()
	if err != nil {
		t.Fatalf("ListLlamaBuilds: %v", err)
	}
	running := rowFor(t, builds, "b11535-cpu")
	if !running.Running {
		t.Errorf("the build the server was started from is not marked running: %+v", builds)
	}
	if running.InUse {
		t.Fatalf("the test needs the running build to differ from the recorded one: %+v", builds)
	}
	if inUse := rowFor(t, builds, "b11528-cpu"); !inUse.InUse || inUse.Running {
		t.Errorf("the recorded build is marked %+v, want in use and not running: %+v", inUse, builds)
	}

	// Each protected line refuses for its own reason, and the running one is checked
	// first: it is the refusal that a deletion attempt would otherwise have to be told
	// about by a broken chat.
	err = a.RemoveLlamaBuild("b11535-cpu")
	if err == nil {
		t.Fatal("the build a running server was started from was deleted")
	}
	if !strings.Contains(err.Error(), "läuft") {
		t.Errorf("refusal = %q, want it to say the build is running", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "b11535-cpu", "llama-server")); statErr != nil {
		t.Fatalf("the running build is gone from disk: %v", statErr)
	}
	if err := a.RemoveLlamaBuild("b11528-cpu"); err == nil {
		t.Error("the build in use was deleted")
	} else if !strings.Contains(err.Error(), "startet") {
		t.Errorf("refusal = %q, want it to say KiChat starts that build", err)
	}

	// One press on the cleanup, with both kinds of protected build among the
	// candidates: it has to skip both rather than break the model or the settings.
	removed, err := a.RemoveOldLlamaBuilds()
	if err != nil {
		t.Fatalf("RemoveOldLlamaBuilds: %v", err)
	}
	if removed != 0 {
		t.Errorf("the cleanup removed %d builds, want none — both are protected", removed)
	}
	for _, dir := range []string{"b11535-cpu", "b11528-cpu"} {
		if _, statErr := os.Stat(filepath.Join(root, dir, "llama-server")); statErr != nil {
			t.Fatalf("the cleanup deleted %s: %v", dir, statErr)
		}
	}

	// Stopped, it is an ordinary old build again: the protection was the server, not
	// the directory.
	if err := a.StopManagedModel("llamacpp"); err != nil {
		t.Fatalf("StopManagedModel: %v", err)
	}
	if err := a.RemoveLlamaBuild("b11535-cpu"); err != nil {
		t.Fatalf("RemoveLlamaBuild after the stop: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "b11535-cpu")); !os.IsNotExist(statErr) {
		t.Errorf("the stopped build was not removed: %v", statErr)
	}
	// And the recorded build is still there, because it is still the one KiChat
	// would start.
	if _, statErr := os.Stat(filepath.Join(root, "b11528-cpu", "llama-server")); statErr != nil {
		t.Errorf("the build in use did not survive: %v", statErr)
	}
}

// waitForRunning polls the status the panel polls until the stand-in server reports
// ready, and fails with the reason if it never does. It answers the version the panel
// would show, because that comes from the same call and is the fingerprint of the build
// the launcher belongs to.
func waitForRunning(t *testing.T, a *App) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		status, err := a.GetRuntimeStatus()
		if err != nil {
			t.Fatalf("GetRuntimeStatus: %v", err)
		}
		if len(status.Models) != 1 {
			t.Fatalf("got %d managed rows, want one: %+v", len(status.Models), status.Models)
		}
		if state := status.Models[0].State; state == rt.StateRunning {
			return status.Version
		} else if state == rt.StateFailed {
			t.Fatalf("the server failed instead of becoming ready: %+v", status.Models[0])
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never reported ready: %+v", status.Models[0])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func rowFor(t *testing.T, builds []InstalledBuild, dir string) InstalledBuild {
	t.Helper()
	for _, build := range builds {
		if build.Dir == dir {
			return build
		}
	}
	t.Fatalf("no build %q in %+v", dir, builds)
	return InstalledBuild{}
}

// writeFakeBuild puts a launcher into the runtime directory that re-executes this test
// binary as the stand-in server.
func writeFakeBuild(t *testing.T, runtimeDir, buildDir string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot re-execute the test binary: %v", err)
	}
	dir := filepath.Join(runtimeDir, buildDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexport " + fakeServerEnv + "=1\n		export " + fakeBuildEnv + "=" + digitsOf(buildDir) +
		"\nexec " + shellQuote(self) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "llama-server"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeModelFile is a file the supervisor will accept as a model: it stats it and
// nothing else, so a small one does.
func writeModelFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "weights.gguf")
	if err := os.WriteFile(path, []byte("GGUF"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// digitsOf is the digits in a build directory's name: `b11535-cpu` → `11535`, which is
// what a launcher of that build reports about itself.
func digitsOf(buildDir string) string {
	digits := ""
	for _, r := range buildDir {
		if r >= '0' && r <= '9' {
			digits += string(r)
			continue
		}
		if digits != "" {
			break
		}
	}
	if digits == "" {
		return "0"
	}
	return digits
}
