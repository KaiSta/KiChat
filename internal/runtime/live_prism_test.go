package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestLivePrismForkRunsAModel is the end-to-end check that the second distribution
// is a runtime rather than a table of names: the fork's own release list, its own
// tag scheme, its archive, its launcher, its ternary kernels, and a real model
// answering over the endpoints the app's llama.cpp provider reads.
//
// It is opt-in because it downloads ~18 MB, unpacks a build and loads real weights:
//
//	KICHAT_LIVE=1 go test ./internal/runtime/ -run TestLivePrism -v
//
// KICHAT_PRISM_MODEL names the GGUF to load; without it the smallest chat model in
// the LM Studio folders is used, and the test skips if there is none. Nothing here
// touches the real runtime directory or ~/.config/kichat: the installer is rooted in
// a temporary directory.
func TestLivePrismForkRunsAModel(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") == "" {
		t.Skip("KICHAT_LIVE is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	endpoint := PrismML.ReleasesAPI()
	asset, err := NewInstaller(t.TempDir()).Latest(ctx, PrismML, goruntime.GOOS, goruntime.GOARCH, AcceleratorCPU)
	if err != nil {
		t.Fatalf("the fork's release list could not be read from %s: %v", endpoint, err)
	}
	t.Logf("release %s: %s (%.1f MB, %s)", asset.Tag, asset.Name, float64(asset.Size)/(1<<20), asset.Suite)
	if !strings.HasPrefix(asset.Tag, "prism-") {
		t.Errorf("tag %q does not carry the fork's prefix — upstream's releases were read instead?", asset.Tag)
	}
	if !strings.Contains(asset.Name, "-prism-") {
		t.Errorf("archive %q is not a PrismML build", asset.Name)
	}
	if got := PrismML.TagOfDir(asset.Dir()); got != asset.Tag {
		t.Errorf("TagOfDir(%q) = %q, want the tag it was built from: the tag comparison an update rests on", asset.Dir(), got)
	}
	if PrismML.NewerBuild(asset.Tag, asset.Tag) {
		t.Error("the published build is offered as an update over itself")
	}

	inst := NewInstaller(t.TempDir())
	started := time.Now()
	binary, err := inst.Install(ctx, asset)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Logf("installed to %s in %s", binary, time.Since(started).Round(time.Second))
	if info, err := os.Stat(binary); err != nil || info.IsDir() {
		t.Fatalf("no launcher at %s: %v", binary, err)
	}

	version, err := BinaryVersion(ctx, binary)
	if err != nil {
		t.Fatalf("the installed launcher did not report a version: %v", err)
	}
	t.Logf("launcher: %s", version)
	if want, ok := buildNumberOf(version); ok {
		if got, _ := PrismML.buildNumber(asset.Tag); got != want {
			t.Errorf("the launcher is build %d and the tag says %d — the archive is not the release it claims", want, got)
		}
	} else {
		t.Errorf("no build number in %q", version)
	}

	// The ternary kernels are the reason this distribution exists: a build without
	// them would start, answer and produce nothing a ternary model can use, which is
	// the failure this test is here to catch rather than a later model load.
	kernels, err := forkKernels(filepath.Dir(binary))
	if err != nil {
		t.Fatalf("reading the installed build: %v", err)
	}
	if len(kernels) == 0 {
		t.Fatalf("no file of the installed build carries the fork's ternary types — %s is not the fork's launcher", binary)
	}
	t.Logf("ternary kernels present in: %s", strings.Join(kernels, ", "))

	model := livePrismModel(t)
	if model == "" {
		t.Skip("no GGUF to load — set KICHAT_PRISM_MODEL, or point the LM Studio folder at one")
	}
	t.Logf("model: %s", model)

	srv := NewServer("prism-live")
	srv.SetReadyTimeout(20 * time.Minute)
	if err := srv.Start(Options{Binary: binary, Model: model, Ctx: 4096}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := srv.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	loading := time.Now()
	if !waitFor(t, 20*time.Minute, func() bool { return srv.Status().State != StateStarting }) {
		t.Fatalf("the server never left \"starting\": %+v", srv.Status())
	}
	st := srv.Status()
	if st.State != StateRunning {
		t.Fatalf("the fork's server failed to load a model: %+v", st)
	}
	t.Logf("ready after %s on %s", time.Since(loading).Round(time.Second), st.URL)

	// What the app's provider reads: n_ctx is the window the model is loaded with,
	// and the chat template is what makes a chat possible at all.
	props := getJSON(t, st.URL+"/props")
	defaults, _ := props["default_generation_settings"].(map[string]any)
	if nCtx, _ := defaults["n_ctx"].(float64); nCtx <= 0 {
		t.Errorf("/props reports no context window: %v", props["default_generation_settings"])
	} else {
		t.Logf("/props: n_ctx=%v, chat_template=%v chars", nCtx, len(fmt.Sprint(props["chat_template"])))
	}
	if template, _ := props["chat_template"].(string); strings.TrimSpace(template) == "" {
		t.Error("/props carries no chat template, so a chat would have no framing")
	}

	// A real turn, which is the only thing that proves the runtime the app installs
	// is one the app can talk to.
	answer := postJSON(t, st.URL+"/v1/chat/completions", map[string]any{
		"messages":    []map[string]string{{"role": "user", "content": "Reply with the single word: ready"}},
		"max_tokens":  256,
		"temperature": 0,
	})
	choices, _ := answer["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choice in the answer: %v", answer)
	}
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	content, _ := message["content"].(string)
	// A reasoning model can spend a whole small budget on its thinking and return an
	// empty answer with finish_reason "length" — measured on this exact fixture:
	// 24 tokens went to reasoning_content and the single word never arrived. Both
	// fields are an answer, and KiChat's stream parser already reads both.
	reasoning, _ := message["reasoning_content"].(string)
	if strings.TrimSpace(content) == "" && strings.TrimSpace(reasoning) == "" {
		t.Errorf("the model answered nothing: %v", answer)
	} else {
		t.Logf("answer: %q (reasoning: %d chars)", strings.TrimSpace(content), len(reasoning))
	}
	if usage, ok := answer["usage"].(map[string]any); ok {
		t.Logf("usage: prompt=%v completion=%v", usage["prompt_tokens"], usage["completion_tokens"])
	}
	// The server names the build it was compiled from, and it has to be the release
	// that was installed: `b10770-6684606ac` for the tag `prism-b10770-6684606`.
	fingerprint, _ := answer["system_fingerprint"].(string)
	number, _ := PrismML.buildNumber(asset.Tag)
	commit := asset.Tag[strings.LastIndex(asset.Tag, "-")+1:]
	if !strings.Contains(fingerprint, fmt.Sprint(number)) || !strings.Contains(fingerprint, commit) {
		t.Errorf("system_fingerprint %q is not the installed release %s (build %d, commit %s)",
			fingerprint, asset.Tag, number, commit)
	} else {
		t.Logf("fingerprint: %s", fingerprint)
	}
}

// forkKernels lists the files of a build that carry the fork's ternary GGML types,
// `PQ2_0` and `PTQ1_0`. Measured on prism-b10770-6684606: PQ2_0 is in libllama and
// in every libggml-cpu-*.so, PTQ1_0 only in libllama — the kernels are compiled into
// the shared objects, so their absence is a build that would fail on a ternary file
// at load time rather than at install time.
func forkKernels(dir string) ([]string, error) {
	var found []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if bytes.Contains(data, []byte("PQ2_0")) || bytes.Contains(data, []byte("PTQ1_0")) {
			found = append(found, entry.Name())
		}
	}
	sort.Strings(found)
	return found, nil
}

// buildNumberOf reads the build number out of the line a launcher answers with
// (`0.2.0-dev (build 10770, commit 6684606ac)`).
func buildNumberOf(version string) (int, bool) {
	m := regexp.MustCompile(`build\s+(\d+)`).FindStringSubmatch(version)
	if m == nil {
		return 0, false
	}
	n := 0
	for _, r := range m[1] {
		n = n*10 + int(r-'0')
	}
	return n, true
}

// livePrismModel is the GGUF the live test loads: the one named in the environment,
// or the smallest chat model in the folders the app itself offers. Projectors are not
// candidates — a vision projector is a GGUF without a language model behind it.
func livePrismModel(t *testing.T) string {
	t.Helper()
	if named := strings.TrimSpace(os.Getenv("KICHAT_PRISM_MODEL")); named != "" {
		if _, err := os.Stat(named); err != nil {
			t.Fatalf("KICHAT_PRISM_MODEL=%s: %v", named, err)
		}
		return named
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, folder := range LMStudioFolders(home) {
		if !folder.Exists {
			continue
		}
		scan, err := ScanModelFolder(folder.Path)
		if err != nil {
			continue
		}
		candidates := make([]ModelFile, 0, len(scan.Files))
		for _, file := range scan.Files {
			if strings.Contains(strings.ToLower(file.Name), "mmproj") {
				continue
			}
			candidates = append(candidates, file)
		}
		if len(candidates) == 0 {
			continue
		}
		sort.Slice(candidates, func(a, b int) bool { return candidates[a].Size < candidates[b].Size })
		t.Logf("folder %s (%s) holds %d models; smallest is %s", folder.Path, folder.Kind, len(candidates), candidates[0].Name)
		return candidates[0].Path
	}
	return ""
}

// getJSON reads one JSON object from the server.
func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: HTTP %d: %s", url, resp.StatusCode, firstLine(string(body)))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return out
}

// postJSON sends one JSON body and reads the JSON answer back.
func postJSON(t *testing.T, url string, payload map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: HTTP %d: %s", url, resp.StatusCode, firstLine(string(answer)))
	}
	var out map[string]any
	if err := json.Unmarshal(answer, &out); err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
