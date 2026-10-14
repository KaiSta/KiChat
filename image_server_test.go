package main

import (
	"encoding/base64"
	"encoding/json"
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
	"kichat/internal/images"
	rt "kichat/internal/runtime"
	"kichat/internal/store"
	"kichat/internal/tools"
)

// The third kind of picture backend, from the outside: the app starts `sd-server` out of
// the same folder and the same model set the one-shot kind uses, waits for it to load,
// and then draws through it — and it must be the *app* doing that, because the provider
// alone cannot show that the folder resolved, that the listener flags were passed, that
// the port the panel shows is the one the process answers on, or that a picture drawn
// this way becomes an ordinary attachment with a recipe.

// fakeSDServerEnv turns this test binary into a stand-in for `sd-server`, the way
// fakeServerEnv does for llama-server: the app needs a process that really answers on the
// port it was given, and nothing outside the Go toolchain should have to be installed for
// that.
const fakeSDServerEnv = "KICHAT_TEST_FAKE_SD_SERVER"

// runFakeSDServer answers `-h` the way the real binary does — the option table is the only
// document stable-diffusion.cpp publishes, and `--listen-port` has to be in it or the app
// refuses the build rather than let it listen on its own default — and otherwise serves the
// A1111-compatible routes this client uses.
func runFakeSDServer() {
	args := os.Args[1:]
	port := 0
	for i, arg := range args {
		if arg == "--listen-port" && i+1 < len(args) {
			port, _ = strconv.Atoi(args[i+1])
		}
	}
	if port == 0 {
		// The usage probe. It is the same text for both of the archive's programs, as
		// far as this test is concerned: the app asks whether the build it is about to
		// start knows the listener flag, and answers by looking for exactly that.
		fmt.Println("usage: sd-server [options]")
		fmt.Println("  --listen-ip <ip>      ip address to listen on")
		fmt.Println("  --listen-port <port>  port to listen on")
		fmt.Println("  --diffusion-model <path>  path to the diffusion model")
		return
	}
	// A leak guard, the same one the llama-server stand-in carries: this process is a
	// child of the test binary, and a test that dies without its stop would leave it
	// listening on a loopback port for as long as the machine is up.
	time.AfterFunc(time.Minute, func() { os.Exit(0) })

	mux := http.NewServeMux()
	// The readiness route. sd-server documents no /health, and this is the route the app
	// probes: a server that answers it has a model loaded, which is the question a start
	// has to answer.
	mux.HandleFunc("/sdapi/v1/sd-models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]string{{"title": "fake.safetensors", "model_name": "fake"}})
	})
	mux.HandleFunc("/sdapi/v1/samplers", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]string{{"name": "Euler"}})
	})
	mux.HandleFunc("/sdapi/v1/txt2img", func(w http.ResponseWriter, _ *http.Request) {
		// `info` is where this client reads the seed the server really drew with, so
		// the stand-in reports one that differs from anything it was asked for: a
		// recipe that carried the requested seed instead would look identical in a
		// test that never checked which one was recorded. The shape is A1111's — a
		// JSON object in a field — and the number is one this app never sends.
		writeJSON(w, map[string]any{
			"images": []string{base64.StdEncoding.EncodeToString(cliPicture)},
			"info":   map[string]any{"seed": 4242, "sd_model_checkpoint": "fake.safetensors"},
		})
	})
	mux.HandleFunc("/sdapi/v1/progress", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"progress": 1.0, "state": map[string]int{"sampling_step": 1, "sampling_steps": 1}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	_ = http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", port), mux)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// fakeSDServerFolder lays out what a user points KiChat at: a folder holding `sd-server`
// beside the picture the stand-in answers with, since the fake reads that file from the
// program's own directory.
func fakeSDServerFolder(t *testing.T) (folder, program string) {
	t.Helper()
	folder = t.TempDir()
	program = filepath.Join(folder, "sd-server")
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot re-execute the test binary: %v", err)
	}
	script := "#!/bin/sh\nexport " + fakeSDServerEnv + "=1\nexec " + shellQuote(self) + " \"$@\"\n"
	if err := os.WriteFile(program, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake sd-server: %v", err)
	}
	return folder, program
}

// sdModelSet is a diffusion pipeline on disk, small enough to write and real enough that a
// start reaches the process: the supervisor stats the model it is handed before it spawns
// anything, which is the check that turns a wrong path into a message rather than a load.
type sdModelSet struct {
	dir       string
	diffusion string
	vae       string
}

func writeSDModelSet(t *testing.T) sdModelSet {
	t.Helper()
	dir := t.TempDir()
	set := sdModelSet{
		dir:       dir,
		diffusion: filepath.Join(dir, "qwen-image-2.1-Q5_K_M.gguf"),
		vae:       filepath.Join(dir, "qwen_image_vae.safetensors"),
	}
	for _, path := range []string{set.diffusion, set.vae} {
		if err := os.WriteFile(path, []byte("GGUF"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return set
}

// sdServerSettings is the SD kind configured against that folder, with the model set the
// panel fills in for a pipeline of parts.
func sdServerSettings(folder string, set sdModelSet) config.Settings {
	settings := config.Default()
	settings.Images.Enabled = true
	settings.Images.Kind = config.ImageKindSD
	settings.Images.CLIFolder = folder
	settings.Images.CLIDiffusionModel = set.diffusion
	settings.Images.CLIVAE = set.vae
	settings.Images.Width, settings.Images.Height, settings.Images.Steps = 512, 512, 4
	settings.Images.Sampler = "euler"
	return settings
}

// waitForImageServer polls the status the panel polls until the managed server reports
// ready, and fails with the row if it never does. Starting is minutes of model loading in
// reality; here the stand-in answers as soon as it is listening.
func waitForImageServer(t *testing.T, a *App) ImageServerStatus {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, err := a.GetImageStatus()
		if err != nil {
			t.Fatalf("GetImageStatus: %v", err)
		}
		switch status.Server.State {
		case rt.StateRunning:
			return status.Server
		case rt.StateFailed:
			t.Fatalf("the server failed instead of becoming ready: %+v", status.Server)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never reported ready: %+v", status.Server)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForImageProvider polls until the services have been rebuilt around the running server,
// which happens one step after the state becomes `running` — the state change is what triggers
// the rebuild, so the two are not observable at the same instant.
func waitForImageProvider(t *testing.T, a *App) images.Provider {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if provider := a.currentImageProvider(); provider != nil {
			return provider
		}
		if time.Now().After(deadline) {
			t.Fatal("no provider was built for a running server")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTheServerKindStartsDrawsAndStops is the whole feature at the level a user meets it:
// the kind is chosen, the folder supplies the program, starting it produces a running
// server the panel can name, a picture is drawn *through that process*, and stopping it
// takes the provider away again.
func TestTheServerKindStartsDrawsAndStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script")
	}
	folder, program := fakeSDServerFolder(t)
	settings := sdServerSettings(folder, writeSDModelSet(t))

	a := newSettingsTestApp(t, settings, t.TempDir())
	a.ctx = t.Context()
	a.rebuildServices(settings)

	// Before the start there is no provider at all, which is what removes the tool:
	// a backend that could only answer "nothing is listening" would cost the model a
	// wasted round every time it was asked for a picture.
	if got := a.currentImageProvider(); got != nil {
		t.Fatalf("a provider was built before anything was started: %v", got.Name())
	}
	// And the row is a stopped one, with arrays rather than nulls: the panel iterates
	// the log it is given.
	before, err := a.GetImageStatus()
	if err != nil {
		t.Fatalf("GetImageStatus: %v", err)
	}
	if before.Server.State != rt.StateStopped || before.Server.Log == nil {
		t.Errorf("the idle row = %+v, want a stopped server with a log array", before.Server)
	}

	if err := a.StartImageServer(); err != nil {
		t.Fatalf("StartImageServer: %v", err)
	}
	defer func() { _ = a.StopImageServer() }()

	running := waitForImageServer(t, a)
	if running.PID == 0 || running.Port == 0 {
		t.Errorf("running row = %+v, want a pid and a port", running)
	}
	if want := fmt.Sprintf("http://127.0.0.1:%d", running.Port); running.URL != want {
		t.Errorf("url = %q, want %q — the panel must name the address the process answers on", running.URL, want)
	}
	// Loopback only. This is a local model with no authentication, and an endpoint on
	// every interface is a port on the network rather than a convenience.
	if !strings.HasPrefix(running.URL, "http://127.0.0.1:") {
		t.Errorf("the server was bound to %q, want the loopback address", running.URL)
	}
	if !strings.Contains(strings.Join(running.Log, "\n"), program) {
		t.Errorf("the log does not name the program that was started:\n%s", strings.Join(running.Log, "\n"))
	}

	// The provider is built from the *running* server, not from the stored address: a
	// stale BaseURL must never become a way to draw from somebody else's server, and
	// 1234 is sd-server's own default as well as LM Studio's.
	//
	// Waited for rather than read once, and that is not test paranoia: the state is
	// published from the supervisor's own goroutine and the provider is built by the
	// state handler that follows it, so a poll landing between the two sees a running
	// server whose services have not been rebuilt yet. The panel's next poll — 1.2 s
	// later — is what makes that invisible in the window.
	provider := waitForImageProvider(t, a)
	if provider.Name() != "sd-server" {
		t.Errorf("provider = %q, want the managed server's own name", provider.Name())
	}

	chatID := "chat-sd"
	if err := a.store.CreateChat(store.Chat{ID: chatID, Title: "Bilder", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	ctx := tools.WithCallingMessage(t.Context(), "msg-1")
	if _, err := a.currentToolService().Execute(ctx, chatID, tools.ToolGenerateImage,
		`{"prompt":"a serene mountain lake"}`); err != nil {
		t.Fatalf("generate_image: %v", err)
	}
	attachments, err := a.store.ListAttachments(chatID)
	if err != nil || len(attachments) != 1 {
		t.Fatalf("attachments = %d, err = %v", len(attachments), err)
	}
	recipe := attachments[0].Generation
	if recipe == nil {
		t.Fatal("the picture carries no recipe, so nothing could ever be redrawn")
	}
	// The seed the *server* reported, not the one this app sent: only the one that was
	// used makes the picture reproducible.
	if recipe.Seed != 4242 {
		t.Errorf("recipe seed = %d, want the 4242 the server reported", recipe.Seed)
	}
	if recipe.Steps != 4 || recipe.Width != 512 || recipe.Height != 512 || recipe.Sampler != "euler" {
		t.Errorf("recipe = %dx%d @%d %q, want the picked 512x512 @4 euler",
			recipe.Width, recipe.Height, recipe.Steps, recipe.Sampler)
	}

	// Stopping releases the model's memory and takes the tool away, both without a
	// restart of the app.
	if err := a.StopImageServer(); err != nil {
		t.Fatalf("StopImageServer: %v", err)
	}
	stopped, err := a.GetImageStatus()
	if err != nil {
		t.Fatalf("GetImageStatus: %v", err)
	}
	if stopped.Server.State != rt.StateStopped || stopped.Server.PID != 0 || stopped.Server.URL != "" {
		t.Errorf("after the stop the row = %+v, want a stopped server with nothing listening", stopped.Server)
	}
	// The other direction of the same ordering: the stop publishes `stopped` and *then*
	// rebuilds, so the provider may still be the running one for that instant.
	deadline := time.Now().Add(5 * time.Second)
	for a.currentImageProvider() != nil {
		if time.Now().After(deadline) {
			t.Fatalf("a provider survived the stop: %v", a.currentImageProvider().Name())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTheServerKindSurvivesASaveAndAReload is the round trip a fixture cannot cover: the
// settings blob is decoded over a fully populated default and then normalised, and the
// normaliser is where a kind no version knows is repaired back to the server backend. The
// managed kind has to come back out of the database as itself, or a user's own sd-server
// would be replaced on the next launch by whatever answers on the stored HTTP address —
// which is the confusion this kind exists to end.
func TestTheServerKindSurvivesASaveAndAReload(t *testing.T) {
	folder, _ := fakeSDServerFolder(t)
	settings := sdServerSettings(folder, writeSDModelSet(t))

	a := newSettingsTestApp(t, settings, t.TempDir())
	if _, err := a.SaveSettings(settings); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	loaded, err := config.Load(a.store, a.secrets)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Images.Kind != config.ImageKindSD {
		t.Errorf("kind after a reload = %q, want %q", loaded.Images.Kind, config.ImageKindSD)
	}
	if !config.IsImageSD(loaded.Images.Kind) {
		t.Error("the reloaded kind is not the managed server, so nothing would start it")
	}
	for name, got := range map[string]string{
		"folder":       loaded.Images.CLIFolder,
		"diffusion":    loaded.Images.CLIDiffusionModel,
		"vae":          loaded.Images.CLIVAE,
		"extra args":   loaded.Images.CLIExtraArgs,
		"startWithApp": strconv.FormatBool(loaded.Images.StartWithApp),
	} {
		t.Logf("%s: %s", name, got)
	}
	if loaded.Images.CLIFolder != folder || loaded.Images.CLIDiffusionModel != settings.Images.CLIDiffusionModel {
		t.Errorf("the model set did not survive the reload: %+v", loaded.Images)
	}

	// And the settings that would start it are read back by the app the same way, so this
	// is a test of the path the next launch takes rather than of the decoder alone.
	b := newSettingsTestApp(t, loaded, t.TempDir())
	b.ctx = t.Context()
	b.rebuildServices(loaded)
	if err := b.StartImageServer(); err != nil {
		t.Fatalf("StartImageServer after the reload: %v", err)
	}
	defer func() { _ = b.StopImageServer() }()
	waitForImageServer(t, b)
}

// TestSavingNeverRestartsTheServerButAsksForOne: a start may be minutes of loading, so an
// edited field must not be a restart. The panel is told instead, and the restart is a click
// — the same rule the managed chat models already follow.
func TestSavingNeverRestartsTheServerButAsksForOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script")
	}
	folder, _ := fakeSDServerFolder(t)
	set := writeSDModelSet(t)
	settings := sdServerSettings(folder, set)

	a := newSettingsTestApp(t, settings, t.TempDir())
	a.ctx = t.Context()
	a.rebuildServices(settings)

	if err := a.StartImageServer(); err != nil {
		t.Fatalf("StartImageServer: %v", err)
	}
	defer func() { _ = a.StopImageServer() }()
	waitForImageServer(t, a)

	if got := waitForImageServer(t, a).RestartNeeded; got {
		t.Error("a freshly started server was reported as needing a restart")
	}

	// The user edits the model set and saves. sd-server loads one pipeline at startup
	// and documents no way to replace it, so the running process is now loading
	// something the settings no longer describe — which is exactly what the row has to
	// say.
	edited := sdServerSettings(folder, set)
	edited.Images.CLIDiffusionModel = filepath.Join(set.dir, "qwen-image-3-Q5_K_M.gguf")
	if err := os.WriteFile(edited.Images.CLIDiffusionModel, []byte("GGUF"), 0o644); err != nil {
		t.Fatalf("write the other model: %v", err)
	}
	if _, err := a.SaveSettings(edited); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	after, err := a.GetImageStatus()
	if err != nil {
		t.Fatalf("GetImageStatus: %v", err)
	}
	if after.Server.State != rt.StateRunning {
		t.Fatalf("saving restarted or stopped the server: %+v", after.Server)
	}
	if !after.Server.RestartNeeded {
		t.Error("the row does not ask for a restart after the model set changed")
	}

	// The restart itself is one call and lands on the new configuration.
	if err := a.StartImageServer(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	restarted := waitForImageServer(t, a)
	if restarted.RestartNeeded {
		t.Error("the restarted server still reports the old configuration")
	}
	if !strings.Contains(strings.Join(restarted.Log, "\n"), "qwen-image-3") {
		t.Errorf("the restart did not load the edited model set:\n%s", strings.Join(restarted.Log, "\n"))
	}
}

// TestAServerLeftRunningUnderAnotherKindStaysStoppable is the state a save can leave behind.
//
// Switching the kind away from the managed server does *not* stop the process — a save never
// reloads a model that may have taken minutes to load — so the row in Settings is the only
// control left for releasing that memory. It is therefore reported for a kind that has no
// process of its own; a row that was only filled for `sdserver` would hide exactly the
// process it exists to stop, and several gigabytes of VRAM would be held by a window with no
// button for it.
func TestAServerLeftRunningUnderAnotherKindStaysStoppable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script")
	}
	folder, _ := fakeSDServerFolder(t)
	settings := sdServerSettings(folder, writeSDModelSet(t))

	a := newSettingsTestApp(t, settings, t.TempDir())
	a.ctx = t.Context()
	a.rebuildServices(settings)
	if err := a.StartImageServer(); err != nil {
		t.Fatalf("StartImageServer: %v", err)
	}
	defer func() { _ = a.StopImageServer() }()
	waitForImageServer(t, a)

	// The user picks the ordinary HTTP server and saves. Nothing stops sd-server.
	switched := settings
	switched.Images.Kind = config.ImageKindServer
	switched.Images.BaseURL = "http://127.0.0.1:7860"
	if _, err := a.SaveSettings(switched); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	status, err := a.GetImageStatus()
	if err != nil {
		t.Fatalf("GetImageStatus: %v", err)
	}
	if status.Server.State != rt.StateRunning || status.Server.URL == "" {
		t.Fatalf("the running server is not reported for the other kind, so nothing could stop it: %+v",
			status.Server)
	}
	// And the reading beside the row says why: the process is loading a model this kind's
	// settings no longer describe.
	if !status.Server.RestartNeeded {
		t.Error("a server whose kind was switched away is not reported as needing a restart")
	}

	// The one control that state leaves is the stop, and it still works.
	if err := a.StopImageServer(); err != nil {
		t.Fatalf("StopImageServer: %v", err)
	}
	after, err := a.GetImageStatus()
	if err != nil {
		t.Fatalf("GetImageStatus: %v", err)
	}
	if after.Server.State != rt.StateStopped || after.Server.PID != 0 {
		t.Errorf("after the stop the row = %+v, want the process gone", after.Server)
	}
	// The kind that took over is unaffected: stopping a process that belonged to another
	// selection must not take the selected backend's provider with it.
	if a.currentImageProvider() == nil {
		t.Error("the HTTP kind has no provider after the stray server was stopped")
	}
}

// TestStartWithAppIsOptInAndLeavesNothingBehind covers the switch the user asked for: the
// server is started at launch only when it is set, and the app exiting takes the process
// with it — a picture server left behind holds its model in memory with nothing left in the
// UI to stop it.
func TestStartWithAppIsOptInAndLeavesNothingBehind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script")
	}
	folder, _ := fakeSDServerFolder(t)
	set := writeSDModelSet(t)

	// Off, which is the default: the app starts and nothing is loaded.
	off := sdServerSettings(folder, set)
	a := newSettingsTestApp(t, off, t.TempDir())
	a.ctx = t.Context()
	a.rebuildServices(off)
	a.startEnabledImages()
	status, err := a.GetImageStatus()
	if err != nil {
		t.Fatalf("GetImageStatus: %v", err)
	}
	if status.Server.State != rt.StateStopped {
		t.Errorf("state = %q, want nothing started when the switch is off", status.Server.State)
	}

	// On: the app starts it, and exiting stops it.
	on := sdServerSettings(folder, set)
	on.Images.StartWithApp = true
	b := newSettingsTestApp(t, on, t.TempDir())
	b.ctx = t.Context()
	b.rebuildServices(on)
	b.startEnabledImages()
	running := waitForImageServer(t, b)
	if running.State != rt.StateRunning {
		t.Fatalf("the switch on did not start the server: %+v", running)
	}

	b.shutdown(t.Context())
	after, err := b.GetImageStatus()
	if err != nil {
		t.Fatalf("GetImageStatus after shutdown: %v", err)
	}
	if after.Server.State != rt.StateStopped || after.Server.PID != 0 {
		t.Errorf("after the app exited the row = %+v, want the process gone", after.Server)
	}
}
