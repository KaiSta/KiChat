package runtime

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAssetNameMatchesThePublishedNames pins the mapping against the names
// llama.cpp actually publishes. Read off release b11528, which carried 36 assets:
// Linux is "ubuntu", Windows says "win-cpu" where the others imply the CPU build,
// macOS carries no accelerator, and the extension follows the asset rather than
// the host — every desktop Unix build is .tar.gz, Windows is .zip.
func TestAssetNameMatchesThePublishedNames(t *testing.T) {
	cases := []struct {
		tag, goos, goarch, accel string
		want                     string
	}{
		{"b11528", "linux", "amd64", AcceleratorCPU, "llama-b11528-bin-ubuntu-x64.tar.gz"},
		{"b11528", "linux", "arm64", AcceleratorCPU, "llama-b11528-bin-ubuntu-arm64.tar.gz"},
		{"b11528", "linux", "amd64", AcceleratorVulkan, "llama-b11528-bin-ubuntu-vulkan-x64.tar.gz"},
		{"b11528", "linux", "arm64", AcceleratorVulkan, "llama-b11528-bin-ubuntu-vulkan-arm64.tar.gz"},
		{"b11528", "darwin", "arm64", AcceleratorCPU, "llama-b11528-bin-macos-arm64.tar.gz"},
		{"b11528", "darwin", "amd64", AcceleratorCPU, "llama-b11528-bin-macos-x64.tar.gz"},
		{"b11528", "windows", "amd64", AcceleratorCPU, "llama-b11528-bin-win-cpu-x64.zip"},
		{"b11528", "windows", "amd64", AcceleratorVulkan, "llama-b11528-bin-win-vulkan-x64.zip"},
	}
	for _, tc := range cases {
		got, err := LlamaCPP.AssetName(tc.tag, tc.goos, tc.goarch, tc.accel)
		if err != nil {
			t.Errorf("%s/%s/%s: %v", tc.goos, tc.goarch, tc.accel, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s/%s/%s = %q, want %q", tc.goos, tc.goarch, tc.accel, got, tc.want)
		}
	}
}

// TestAssetNameRefusesWhatIsNotPublished: a platform or accelerator with no build
// has to fail rather than fall back to one the user did not ask for — a Vulkan
// build on macOS would be a silent substitution for something that does not exist.
func TestAssetNameRefusesWhatIsNotPublished(t *testing.T) {
	for _, tc := range []struct{ goos, goarch, accel string }{
		{"darwin", "arm64", AcceleratorVulkan},
		{"linux", "riscv64", AcceleratorCPU},
		{"windows", "386", AcceleratorCPU},
		{"freebsd", "amd64", AcceleratorCPU},
	} {
		if got, err := LlamaCPP.AssetName("b11528", tc.goos, tc.goarch, tc.accel); err == nil {
			t.Errorf("%s/%s/%s = %q, want a refusal", tc.goos, tc.goarch, tc.accel, got)
		}
	}
}

// TestLatestSkipsReleasesWithoutOurBuild covers the shape that made GitHub's
// "latest" endpoint unusable: the newest release is a versioned one carrying no
// binaries at all, and the archives live on the per-build releases behind it. The
// scan has to walk past the first and answer with the second.
func TestLatestSkipsReleasesWithoutOurBuild(t *testing.T) {
	const body = `[
	  {"tag_name":"v0.6.0","assets":[{"name":"nightly-tag.txt","size":7}]},
	  {"tag_name":"b11528","assets":[
	     {"name":"llama-b11528-ui.tar.gz","size":1},
	     {"name":"llama-b11528-bin-ubuntu-x64.tar.gz","size":17829692,
	      "digest":"sha256:688161d06a4138a09ef9a09c830316c89c1923941bac9f341af7ffe1ce68c0f3",
	      "browser_download_url":"https://example.invalid/a.tar.gz"}]}
	]`
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	inst := &Installer{Dir: t.TempDir(), APIBase: srv.URL, Client: srv.Client()}
	asset, err := inst.Latest(context.Background(), LlamaCPP, "linux", "amd64", AcceleratorCPU)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if asset.Tag != "b11528" || asset.Name != "llama-b11528-bin-ubuntu-x64.tar.gz" {
		t.Errorf("asset = %+v, want the per-build archive", asset)
	}
	if asset.Digest == "" || asset.Size != 17829692 {
		t.Errorf("asset = %+v, want its digest and size carried through", asset)
	}
	// GitHub refuses requests without a User-Agent, so it has to be sent.
	if gotUA == "" {
		t.Error("no User-Agent was sent")
	}
}

// TestLatestReportsAMissingBuild: a platform with no published build is an
// ordinary answer, not a panic or a silent empty result.
func TestLatestReportsAMissingBuild(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"b11528","assets":[{"name":"llama-b11528-ui.tar.gz"}]}]`)
	}))
	defer srv.Close()

	inst := &Installer{Dir: t.TempDir(), APIBase: srv.URL, Client: srv.Client()}
	if _, err := inst.Latest(context.Background(), LlamaCPP, "linux", "riscv64", AcceleratorCPU); err == nil {
		t.Error("want an error for a platform with no build")
	}
}

// tarGz builds a small tar.gz fixture and returns the file it wrote.
func tarGz(t *testing.T, entries map[string]string, links map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range links {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Linkname: target,
			Typeflag: tar.TypeSymlink,
			Mode:     0o777,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// readFile is the bytes of a fixture, for the cases that serve or digest one.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestInstallUnpacksTheWholeArchive is what the layout demands: the launcher is
// 17 KB beside a 7.6 MB implementation library and ten symlinks, and it needs all
// of them, so an install that extracted only `llama-server` would produce a
// launcher that cannot start.
func TestInstallUnpacksTheWholeArchive(t *testing.T) {
	archive := readFile(t, tarGz(t, map[string]string{
		"llama-b1/llama-server":            "#!/bin/sh\ntrue\n",
		"llama-b1/libllama-server-impl.so": "impl",
		"llama-b1/libllama.so.0.6.0":       "lib",
	}, map[string]string{"llama-b1/libllama.so.0": "libllama.so.0.6.0"}))
	digest := sha256.Sum256(archive)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	dir := t.TempDir()
	inst := &Installer{Dir: dir, APIBase: srv.URL, Client: srv.Client()}
	asset := Asset{
		Tag: "b1", Name: "llama-b1-bin-ubuntu-x64.tar.gz", URL: srv.URL + "/a.tar.gz",
		Size: int64(len(archive)), Digest: "sha256:" + hex.EncodeToString(digest[:]),
	}
	binary, err := inst.Install(context.Background(), asset)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	for _, want := range []string{"llama-server", "libllama-server-impl.so", "libllama.so.0.6.0", "libllama.so.0"} {
		if _, err := os.Lstat(filepath.Join(dir, "b1", want)); err != nil {
			t.Errorf("%s was not unpacked: %v", want, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(dir, "b1", "libllama.so.0")); err != nil || target != "libllama.so.0.6.0" {
		t.Errorf("symlink = %q, %v; want it preserved", target, err)
	}
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the launcher is not executable: %v", info.Mode())
	}
	// The archive itself is not kept: it is tens to hundreds of megabytes and
	// nothing reads it again.
	if _, err := os.Stat(filepath.Join(dir, asset.Name+".part")); !os.IsNotExist(err) {
		t.Errorf("the downloaded archive was left behind: %v", err)
	}
	// Find reports the build that was just unpacked.
	if tag, found := inst.Find(LlamaCPP); tag != "b1" || found != binary {
		t.Errorf("Find = %q, %q; want the installed build", tag, found)
	}
}

// TestInstallRefusesAWrongDigest is the guard on a download that arrives damaged
// or replaced: GitHub reports a sha256 per asset, and a mismatch has to stop the
// install rather than leave a broken build in place.
func TestInstallRefusesAWrongDigest(t *testing.T) {
	archive := readFile(t, tarGz(t, map[string]string{"llama-b1/llama-server": "x"}, nil))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	dir := t.TempDir()
	inst := &Installer{Dir: dir, APIBase: srv.URL, Client: srv.Client()}
	asset := Asset{
		Tag: "b1", Name: "llama-b1-bin-ubuntu-x64.tar.gz", URL: srv.URL + "/a.tar.gz",
		Size: int64(len(archive)),
		// The digest of something else entirely.
		Digest: "sha256:" + strings.Repeat("ab", 32),
	}
	if _, err := inst.Install(context.Background(), asset); err == nil {
		t.Fatal("a wrong digest was accepted")
	}
	if tag, _ := inst.Find(LlamaCPP); tag != "" {
		t.Errorf("a failed install left build %q installed", tag)
	}
	if _, err := os.Stat(filepath.Join(dir, "b1")); !os.IsNotExist(err) {
		t.Error("a failed install left a directory behind")
	}
}

// TestInstallRefusesATruncatedDownload: a body that ends early still ends
// cleanly, so the size the API reported is what catches it.
func TestInstallRefusesATruncatedDownload(t *testing.T) {
	archive := readFile(t, tarGz(t, map[string]string{"llama-b1/llama-server": "x"}, nil))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive[:len(archive)/2])
	}))
	defer srv.Close()

	inst := &Installer{Dir: t.TempDir(), APIBase: srv.URL, Client: srv.Client()}
	asset := Asset{Tag: "b1", Name: "llama-b1.tar.gz", URL: srv.URL + "/a.tar.gz", Size: int64(len(archive))}
	if _, err := inst.Install(context.Background(), asset); err == nil {
		t.Fatal("a truncated download was installed")
	}
}

// TestExtractRefusesEntriesOutsideTheDestination is the zip-slip guard. An archive
// is untrusted input, and an entry named with `..` or an absolute path would
// otherwise be written wherever it likes.
func TestExtractRefusesEntriesOutsideTheDestination(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "target")
	dst := t.TempDir()
	archive := tarGz(t, map[string]string{
		"ok.txt":                       "fine",
		"../" + filepath.Base(outside): "escaped",
	}, nil)

	err := extract(archive, formatTarGz, dst)
	if err == nil {
		t.Fatal("an entry escaping the destination was extracted")
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Errorf("the escaping entry was written to %s", outside)
	}
}

// TestExtractReadsZipAsWell: Windows builds ship .zip, and the format follows the
// asset rather than the host, so a Linux install can be handed one.
func TestExtractReadsZipAsWell(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range map[string]string{"llama-b1/llama-server.exe": "MZ", "llama-b1/ggml.dll": "dll"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if err := extract(path, formatZip, dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "llama-b1", "llama-server.exe")); err != nil {
		t.Errorf("zip entry not extracted: %v", err)
	}
}

// TestSplitArgsKeepsQuotedPathsTogether: a model or LoRA adapter whose path
// contains a space is ordinary on Windows, and splitting on whitespace alone would
// hand the server two arguments neither of which is a file.
func TestSplitArgsKeepsQuotedPathsTogether(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  --n-gpu-layers 99  ", []string{"--n-gpu-layers", "99"}},
		{`--lora "/home/me/my adapter.gguf"`, []string{"--lora", "/home/me/my adapter.gguf"}},
		{`--chat-template-file '/opt/a b/c.jinja'`, []string{"--chat-template-file", "/opt/a b/c.jinja"}},
		{`--alias ""`, []string{"--alias", ""}},
		{`--alias a\ b`, []string{"--alias", "a b"}},
	}
	for _, tc := range cases {
		got := SplitArgs(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("SplitArgs(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("SplitArgs(%q) = %q, want %q", tc.in, got, tc.want)
				break
			}
		}
	}
}

// fakeServer writes a shell script that records its arguments and then behaves
// like a server that stays up. Tests are skipped on Windows, where no shell is
// guaranteed.
func fakeServer(t *testing.T, body string) (binary, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args.txt")
	binary = filepath.Join(dir, "llama-server")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\n" + body
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, argsFile
}

func writeModel(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(path, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// waitFor polls until cond holds, so a test never depends on a sleep being long
// enough.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// stateRecorder collects state changes without racing the monitor goroutine.
type stateRecorder struct {
	mu     sync.Mutex
	states []string
}

func (r *stateRecorder) add(st Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, st.State)
}

func (r *stateRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	// The recorder holds references to nothing else, so a copy is enough to read
	// it outside the lock.
	out := make([]string, len(r.states))
	copy(out, r.states)
	return out
}

// TestServerStartProbeAndStop is the whole lifecycle of a managed server: the
// arguments it is started with, the readiness that turns "starting" into
// "running", and a stop that leaves nothing running.
func TestServerStartProbeAndStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	binary, argsFile := fakeServer(t, "echo 'loading model'\nsleep 30\n")
	model := writeModel(t)

	srv := NewServer("test")
	srv.readyTimeout = 10 * time.Second
	rec := &stateRecorder{}
	srv.SetStateHandler(rec.add)
	// Readiness is answered here rather than by a real model load; the process
	// itself is real, which is what this test is about.
	srv.probe = func(context.Context, string) error { return nil }

	if err := srv.Start(Options{Binary: binary, Model: model, Ctx: 4096}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return srv.Status().State == StateRunning }) {
		t.Fatalf("server never became ready: %+v", srv.Status())
	}

	st := srv.Status()
	if st.PID == 0 || st.Port == 0 || !strings.HasPrefix(st.URL, "http://127.0.0.1:") {
		t.Errorf("status = %+v, want a pid, a port and a loopback URL", st)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the process recorded no arguments: %v", err)
	}
	got := strings.Fields(string(args))
	for _, want := range []string{"-m", model, "-c", "4096", "--port", fmt.Sprint(st.Port), "--host", "127.0.0.1"} {
		if !contains(got, want) {
			t.Errorf("arguments %q do not contain %q", got, want)
		}
	}

	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st := srv.Status(); st.State != StateStopped || st.PID != 0 {
		t.Errorf("after Stop: %+v, want a stopped server with no pid", st)
	}
	// A second stop is not an error: the UI may well press it twice.
	if err := srv.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if states := rec.all(); !contains(states, StateStarting) || !contains(states, StateRunning) {
		t.Errorf("states = %v, want starting then running", states)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestServerReportsAFailedStart covers the interesting failure: the process starts
// and dies, which is what a bad quantisation or a missing file looks like. The
// state has to become failed, and the server's own last words have to be in the
// message — "exit status 1" alone tells the user nothing.
func TestServerReportsAFailedStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	binary, _ := fakeServer(t, "echo 'error: unknown model architecture'\nexit 3\n")
	model := writeModel(t)

	srv := NewServer("test")
	srv.readyTimeout = 10 * time.Second
	srv.probe = func(context.Context, string) error { return fmt.Errorf("not ready") }

	if err := srv.Start(Options{Binary: binary, Model: model}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return srv.Status().State == StateFailed }) {
		t.Fatalf("state = %q, want failed: %+v", srv.Status().State, srv.Status())
	}
	st := srv.Status()
	if !strings.Contains(st.Error, "unknown model architecture") {
		t.Errorf("error = %q, want the server's own last output in it", st.Error)
	}
	if st.PID != 0 {
		t.Errorf("a failed server still reports PID %d", st.PID)
	}
}

// TestServerRefusesWhatItCannotStart: both paths are checked before a process is
// created, so the user is told which one is wrong.
func TestServerRefusesWhatItCannotStart(t *testing.T) {
	srv := NewServer("test")
	if err := srv.Start(Options{Model: writeModel(t)}); err == nil {
		t.Error("want an error when no binary is configured")
	}
	binary, _ := fakeServer(t, "sleep 1\n")
	if err := srv.Start(Options{Binary: binary}); err == nil {
		t.Error("want an error when no model is configured")
	}
	if err := srv.Start(Options{Binary: binary, Model: filepath.Join(t.TempDir(), "missing.gguf")}); err == nil {
		t.Error("want an error for a model that does not exist")
	}
	if err := srv.Start(Options{Binary: binary, Model: t.TempDir()}); err == nil {
		t.Error("want an error for a directory instead of a model file")
	}
	if srv.Status().State != StateStopped {
		t.Errorf("state = %q after refused starts, want stopped", srv.Status().State)
	}
}

// TestFreePortHandsOutSomethingBindable: the port is released before llama-server
// takes it, so it has to at least be a usable number.
func TestFreePortHandsOutSomethingBindable(t *testing.T) {
	port, err := FreePort()
	if err != nil {
		t.Fatalf("FreePort: %v", err)
	}
	if port < 1024 || port > 65535 {
		t.Errorf("port = %d, want a usable one", port)
	}
}

// TestRingKeepsTheTailAndNotTheWholeLog: a server that logs for hours must not
// grow the app's memory with its output, but the last lines are what explain a
// failure.
func TestRingKeepsTheTailAndNotTheWholeLog(t *testing.T) {
	r := newRing(3)
	w := r.writer()
	for _, line := range []string{"one", "two", "three", "four"} {
		if _, err := w.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	got := strings.Join(r.tail(10), ",")
	if got != "two,three,four" {
		t.Errorf("tail = %q, want the last three lines", got)
	}
}

// TestArgsCarryFlashAttentionAndCacheTypes pins the command line the new fields
// produce. The defaults are the server's own, and passing them anyway would be a
// flag llama.cpp reads differently from its default in some builds — so "auto"
// and f16 must not appear, while an explicit choice must.
func TestArgsCarryFlashAttentionAndCacheTypes(t *testing.T) {
	base := Options{Model: "/m.gguf", Host: "127.0.0.1", Port: 8080}

	// The defaults add nothing: an empty or "auto" flash attention and an f16
	// cache are exactly what llama-server would pick on its own.
	for _, o := range []Options{
		base,
		func() Options { o := base; o.FlashAttention = "auto"; return o }(),
		func() Options { o := base; o.CacheTypeK, o.CacheTypeV = CacheTypeF16, CacheTypeF16; return o }(),
	} {
		line := strings.Join(o.args(), " ")
		for _, flag := range []string{"--flash-attn", "--cache-type-k", "--cache-type-v"} {
			if strings.Contains(line, flag) {
				t.Errorf("args = %q, want no %s for the server's own default", line, flag)
			}
		}
	}

	full := base
	full.FlashAttention = FlashAttentionOn
	full.CacheTypeK, full.CacheTypeV = "q8_0", "q4_0"
	want := "-m /m.gguf --host 127.0.0.1 --port 8080 --flash-attn on --cache-type-k q8_0 --cache-type-v q4_0"
	if got := strings.Join(full.args(), " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}

	// The user's own arguments stay last, so they can still override ours.
	override := full
	override.Extra = []string{"--flash-attn", "off"}
	if got := override.args(); got[len(got)-2] != "--flash-attn" || got[len(got)-1] != "off" {
		t.Errorf("the user's arguments are not last: %q", got)
	}
}

// TestLauncherFindsANamedBuild: the app runs the build it recorded rather than
// merely the newest one on disk, so the installer has to be able to hand back a
// named build's launcher — and answer false for one that is not there, which is
// what lets a deleted build fall back to Find.
func TestLauncherFindsANamedBuild(t *testing.T) {
	dir := t.TempDir()
	inst := NewInstaller(dir)

	if _, ok := inst.Launcher(""); ok {
		t.Error("an empty name answered with a launcher")
	}
	if _, ok := inst.Launcher("b11528-cpu"); ok {
		t.Error("a build that was never installed answered with a launcher")
	}

	if err := os.MkdirAll(filepath.Join(dir, "b11528-cpu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.BinaryPath("b11528-cpu"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := inst.Launcher("b11528-cpu")
	if !ok || got != inst.BinaryPath("b11528-cpu") {
		t.Errorf("Launcher = %q, %v; want the launcher of the named build", got, ok)
	}
}
