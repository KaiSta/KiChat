package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/ingest"
	"kichat/internal/llm"
	rt "kichat/internal/runtime"
	"kichat/internal/secret"
	"kichat/internal/store"
)

// TestLiveProjectorIsWhatMakesAVisionModelSee is the two-row table in LLAMACPP.md §3.12,
// taken through the app's own supervisor rather than at a hand-started server: the same
// weights, the same 64x64 PNG, one flag apart under the same build. It pins the whole chain
// a user's image travels — the folder beside the model is read, `--mmproj` reaches the
// process, the server reports a vision modality, the provider turns that into the `vision`
// capability the chat badges, and a turn carrying an image is *answered*.
//
//	KICHAT_LIVE=1 KICHAT_VISION_MODEL=~/…/Qwen3-VL-8B-Instruct-Q4_K_M.gguf \
//	    go test ./ -run TestLiveProjectorIsWhatMakesAVisionModelSee -v
//
// The projector-less row cannot be had by pointing the app at a folder that holds a
// projector, because finding it is now the app's job: that row starts a symlink to the same
// weights out of an otherwise empty folder instead, which is the identical file loaded
// without its second half. Two starts load five gigabytes each, so there is no default and
// the variable is required.
//
// Optional: KICHAT_VISION_TAG pins the installed build to start (otherwise the newest one
// of the selected source), KICHAT_RUNTIME_DIR the folder builds are installed in.
func TestLiveProjectorIsWhatMakesAVisionModelSee(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") == "" {
		t.Skip("set KICHAT_LIVE=1 to run a test that loads a real model")
	}
	model := visionModelPath(t)

	pair := rt.MatchProjector(model)
	if pair == "" {
		t.Fatalf("nothing is paired with %s — the folder holds %v, and a projector beside the model is what this test needs",
			model, rt.ProjectorsBeside(model))
	}

	t.Run("with the projector the app pairs itself", func(t *testing.T) {
		a, row, argv := startRealModel(t, model)
		if row.MMProj != pair || !row.MMProjDetected {
			t.Errorf("the row reports %q (detected=%v), want the projector beside the model, %q",
				row.MMProj, row.MMProjDetected, pair)
		}
		// The argument list of the process that is really running, not the Options it was
		// built from: the offline tests cover this half with a stand-in launcher, and it is
		// worth having once against a launcher that loads a real model.
		if runtime.GOOS == "linux" && !hasFlagValue(argv, "--mmproj", pair) {
			t.Errorf("the running server's command line is %v, want --mmproj %s", argv, pair)
		}
		if props := propsAt(t, row.URL); !props.Vision {
			t.Errorf("/props reports %+v, want a vision modality with the projector loaded", props)
		}
		if status, body := imageTurn(t, row.URL); status != http.StatusOK {
			t.Errorf("an image turn answered HTTP %d: %s", status, body)
		} else {
			t.Logf("an image turn answered %s", strings.TrimSpace(body))
		}
		assertAdvertisedVision(t, a, model)
	})

	t.Run("without it", func(t *testing.T) {
		_, row, argv := startRealModel(t, soloWeights(t, model))
		for _, arg := range argv {
			if arg == "--mmproj" {
				t.Errorf("a model with no projector beside it was given the flag: %v", argv)
			}
		}
		if props := propsAt(t, row.URL); props.Vision {
			t.Errorf("/props reports %+v, want no vision modality without the file", props)
		}
		status, body := imageTurn(t, row.URL)
		if status == http.StatusOK {
			t.Errorf("an image turn was answered though nothing can read it: %s", body)
		}
		// The server's own words, which is what the transcript shows when this app has no
		// projector to pass: a 500 from llama.cpp rather than a sentence from KiChat.
		if !strings.Contains(body, "image input is not supported") {
			t.Errorf("the refusal is %q, want the server's own \"image input is not supported\"", body)
		}
	})
}

// startRealModel starts one managed model on this machine's own installed launcher and
// returns the app, the row the panel reads, and the argument list of the running process.
func startRealModel(t *testing.T, modelPath string) (*App, ManagedStatus, []string) {
	t.Helper()
	settings := config.Default()
	settings.Runtime.Models = map[string]config.ManagedModel{
		"llamacpp": {
			Enabled: true, ModelPath: modelPath, CtxSize: 4096,
			// What a first start in the panel would use. `all` would ask for a 5 GB model
			// and a 1 GB projector on the card at once, which is a different measurement.
			Offload: config.OffloadAuto,
		},
	}
	if tag := strings.TrimSpace(os.Getenv("KICHAT_VISION_TAG")); tag != "" {
		settings.Runtime.Tag = tag
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	a := newCleanupTestApp(t, settings, realRuntimeDir(t))
	a.store, a.extractor = st, ingest.New()
	// The keyring is a per-user global resource and a save here must not reach it.
	if secrets, err := secret.NewIsolated(filepath.Join(t.TempDir(), "secrets")); err == nil {
		a.secrets = secrets
	}

	started := time.Now()
	if err := a.StartManagedModel("llamacpp"); err != nil {
		t.Fatalf("StartManagedModel: %v", err)
	}
	t.Cleanup(func() { _ = a.StopManagedModel("llamacpp") })
	row := waitForLiveModel(t, a)
	t.Logf("load: %s — %s of weights, %d/%d layers on %q, projector %s",
		time.Since(started).Round(time.Second), humanBytes(row.ModelBytes),
		row.OffloadLayers, row.OffloadTotal, row.OffloadDevice, filepath.Base(row.MMProj))

	srv := a.existingServer("llamacpp")
	if srv == nil {
		t.Fatal("the started model has no supervisor")
	}
	if runtime.GOOS != "linux" {
		return a, row, nil
	}
	return a, row, cmdlineOf(t, srv.Status().PID)
}

// waitForLiveModel waits for a real model to be loaded. The 20 second deadline the
// stand-in launcher is given is a joke for five gigabytes of weights, so the wait is
// longer here and reports the row it gave up on.
func waitForLiveModel(t *testing.T, a *App) ManagedStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		status, err := a.GetRuntimeStatus()
		if err != nil {
			t.Fatalf("GetRuntimeStatus: %v", err)
		}
		if len(status.Models) != 1 {
			t.Fatalf("got %d managed rows, want one: %+v", len(status.Models), status.Models)
		}
		switch status.Models[0].State {
		case rt.StateRunning:
			return status.Models[0]
		case rt.StateFailed:
			t.Fatalf("the server failed instead of becoming ready: %+v", status.Models[0])
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never reported ready: %+v", status.Models[0])
		}
		time.Sleep(time.Second)
	}
}

// soloWeights puts the same weights alone in an empty folder, so the app finds nothing to
// pair them with. A link rather than a copy: the identical file is what makes the two rows
// comparable, and nobody copies five gigabytes to prove it.
func soloWeights(t *testing.T, model string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), filepath.Base(model))
	if err := os.Symlink(model, path); err != nil {
		t.Skipf("cannot link the weights into an empty folder: %v", err)
	}
	return path
}

// visionModelPath is the file the test drives, from the environment.
func visionModelPath(t *testing.T) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("KICHAT_VISION_MODEL"))
	if path == "" {
		t.Skip("set KICHAT_VISION_MODEL to a multimodal GGUF that has its projector beside it")
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		t.Skipf("KICHAT_VISION_MODEL=%s is not a file", path)
	}
	return path
}

// realRuntimeDir is where this machine's llama.cpp builds are installed — the app's own
// directory, so the test starts the launcher the user starts.
func realRuntimeDir(t *testing.T) string {
	t.Helper()
	if dir := strings.TrimSpace(os.Getenv("KICHAT_RUNTIME_DIR")); dir != "" {
		return dir
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("no user config directory: %v", err)
	}
	dir := filepath.Join(configDir, "kichat", "runtime")
	if !rt.IsDir(dir) {
		t.Skipf("no llama.cpp runtime is installed in %s — install one in Settings → Backends", dir)
	}
	return dir
}

// cmdlineOf is the argument list a running process was started with, which is the only
// place a dropped flag can be seen.
func cmdlineOf(t *testing.T, pid int) []string {
	t.Helper()
	if pid == 0 {
		t.Fatal("the running server reports no pid")
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		t.Fatalf("read the command line of pid %d: %v", pid, err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
}

// propReading is the part of /props this feature is about.
type propReading struct {
	Vision bool `json:"vision"`
	Video  bool `json:"video"`
	Audio  bool `json:"audio"`
}

func propsAt(t *testing.T, baseURL string) propReading {
	t.Helper()
	var out struct {
		Modalities propReading `json:"modalities"`
	}
	getJSON(t, baseURL+"/props", &out)
	t.Logf("/props modalities: %+v", out.Modalities)
	return out.Modalities
}

func getJSON(t *testing.T, url string, into any) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s answered HTTP %d: %s", url, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode %s: %v — body %s", url, err, raw)
	}
}

// imageTurn sends one real picture as an ordinary OpenAI-shaped image part and returns the
// status and the body. One token is asked for: the projector is exercised while the prompt
// is processed, and the model's answer is not what is being measured.
func imageTurn(t *testing.T, baseURL string) (int, string) {
	t.Helper()
	payload := map[string]any{
		"model":      "kichat-live-check",
		"max_tokens": 1,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "Answer with one word: what is in this picture?"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64," + pngBase64(t),
				}},
			},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Post(baseURL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	t.Logf("image turn: HTTP %d in %s", resp.StatusCode, time.Since(started).Round(time.Millisecond))
	return resp.StatusCode, string(raw)
}

// pngBase64 is a real 64x64 picture rather than a byte string with a PNG header: the
// decoder on the other side has to see something for the turn to be a measurement of
// vision at all.
func pngBase64(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 4), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode the picture: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// assertAdvertisedVision is the app's own answer rather than the server's: the listing the
// chat picker badges, built by the provider out of /props. A vision model the app lists as
// text-only is the same bug as a projector that is never passed, one layer up.
func assertAdvertisedVision(t *testing.T, a *App, modelPath string) {
	t.Helper()
	models, err := a.ListModels(true)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	for _, m := range models {
		if !strings.EqualFold(filepath.Base(m.ID), filepath.Base(modelPath)) {
			continue
		}
		for _, cap := range m.Capabilities {
			if cap == llm.CapVision {
				return
			}
		}
		t.Errorf("the listing offers %s as %q with %v, want the %q capability /props reports",
			filepath.Base(m.ID), m.Type, m.Capabilities, llm.CapVision)
		return
	}
	t.Errorf("the listing has no row for %s: %+v", filepath.Base(modelPath), models)
}

// humanBytes is for the log line: the numbers behind a model load are read in gigabytes.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, exp := float64(n), 0
	for value >= unit && exp < 4 {
		value /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", value, "KMGT"[exp-1])
}
