package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"kichat/internal/config"
	rt "kichat/internal/runtime"
)

// The comparison an update marker rests on, at the cases a tag can take: a newer
// build, the same one, an older one, and the digit boundary where a textual
// comparison would offer an update to a user who is already past it.
func TestNewerBuildComparesTheNumberNotTheText(t *testing.T) {
	cases := []struct {
		latest, installed string
		want              bool
	}{
		{"b11541", "b11535", true},
		{"b11535", "b11535", false},
		{"b11535", "b11541", false},
		// "b1000" < "b999" as text, which would hide a real update.
		{"b1000", "b999", true},
		// Nothing installed is not an update: there is no build to replace.
		{"b11541", "", false},
		// A tag this app cannot order is reported as different, so an unusual
		// tag does not silently disable the check.
		{"v0.6.0", "b11535", true},
		{"v0.6.0", "v0.6.0", false},
	}
	for _, tc := range cases {
		if got := newerBuild(tc.latest, tc.installed); got != tc.want {
			t.Errorf("newerBuild(%q, %q) = %v, want %v", tc.latest, tc.installed, got, tc.want)
		}
	}
}

// An installed build is named by its directory, and the directory carries the
// family and the suite as well as the tag (`b11535`, `b11535-vulkan`,
// `b11535-vulkan-10.0`). The comparison is against the build, so the extras have to
// come off — otherwise every update would look like a different build twice over.
func TestBuildTagOfStripsTheFamilyAndSuite(t *testing.T) {
	cases := map[string]string{
		"b11535":             "b11535",
		"b11535-vulkan":      "b11535",
		"b11535-vulkan-10.0": "b11535",
		"":                   "",
	}
	for dir, want := range cases {
		if got := buildTagOf(dir); got != want {
			t.Errorf("buildTagOf(%q) = %q, want %q", dir, got, want)
		}
	}
}

// The whole path the panel depends on, through the real bound method: what is on
// disk, what llama.cpp publishes, and what the status reports afterwards. The
// release listing is served by a test server, so nothing here reaches GitHub.
func TestCheckRuntimeUpdateFindsANewerBuild(t *testing.T) {
	dir := t.TempDir()
	installFakeBuild(t, dir, "b11535-cpu")

	srv := serveReleases(t, "b11541", 17829692)
	defer srv.Close()

	a := newUpdateTestApp(t, dir, srv)
	reading, err := a.CheckLlamaRuntimeUpdate()
	if err != nil {
		t.Fatalf("CheckLlamaRuntimeUpdate: %v", err)
	}
	if !reading.Checked || reading.Installed != "b11535" || reading.Latest != "b11541" {
		t.Errorf("reading = %+v, want b11535 against the published b11541", reading)
	}
	if !reading.Available {
		t.Error("a published b11541 over an installed b11535 is not offered as an update")
	}
	if reading.Size != 17829692 {
		t.Errorf("size = %d, want the 17829692 the release reported", reading.Size)
	}
	if reading.Error != "" {
		t.Errorf("reading carries an error although the lookup answered: %q", reading.Error)
	}

	// The panel does not read the return value: it polls the status, which has to
	// carry the same answer or the marker would never appear.
	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if !status.Update.Available || status.Update.Latest != "b11541" {
		t.Errorf("status.update = %+v, want the lookup the check just made", status.Update)
	}
	if raw, err := json.Marshal(status); err != nil {
		t.Errorf("marshal: %v", err)
	} else if !strings.Contains(string(raw), `"update":{`) {
		t.Errorf("the update reading did not reach the panel as an object: %s", raw)
	}

	// And once the update is on disk, the same check has to stop offering it —
	// otherwise the marker stays after the fix, which is how a warning trains a
	// user to ignore it.
	installFakeBuild(t, dir, "b11541-cpu")
	after, err := a.CheckLlamaRuntimeUpdate()
	if err != nil {
		t.Fatalf("CheckLlamaRuntimeUpdate after the update: %v", err)
	}
	if after.Available {
		t.Errorf("b11541 is still offered over an installed b11541: %+v", after)
	}
	if after.Installed != "b11541" || after.Latest != "b11541" {
		t.Errorf("reading after the update = %+v, want both sides at b11541", after)
	}
}

// Asking failed is not the same answer as being up to date, and the panel must be
// able to tell them apart: a rate limit or an unreachable API would otherwise be
// reported as "current", which is a claim this app has no basis for.
func TestCheckRuntimeUpdateSeparatesFailureFromBeingCurrent(t *testing.T) {
	dir := t.TempDir()
	installFakeBuild(t, dir, "b11535-cpu")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	a := newUpdateTestApp(t, dir, srv)
	reading, err := a.CheckLlamaRuntimeUpdate()
	if err != nil {
		t.Fatalf("CheckLlamaRuntimeUpdate: %v", err)
	}
	if !reading.Checked {
		t.Error("a failed lookup left the reading as \"not asked\", so the panel cannot say it tried")
	}
	if reading.Error == "" {
		t.Error("a refused lookup reported no reason")
	}
	if reading.Available {
		t.Error("a lookup that could not ask still offered an update")
	}
	// The two tags are known even when the newest build is not: the installed one
	// is on disk, and the panel prints it either way.
	if reading.Installed != "b11535" {
		t.Errorf("installed = %q, want the build that is on disk", reading.Installed)
	}
}

// A launcher the user supplied is not one this app installed, so there is nothing
// to compare against a release: the reading stays empty and the panel keeps the row
// out of the way rather than offering to overwrite the user's own build.
func TestCheckRuntimeUpdateIgnoresACustomBinary(t *testing.T) {
	dir := t.TempDir()
	srv := serveReleases(t, "b11541", 17829692)
	defer srv.Close()

	a := newUpdateTestApp(t, dir, srv)
	a.settings.Runtime.BinaryPath = "/usr/bin/llama-server"

	reading, err := a.CheckLlamaRuntimeUpdate()
	if err != nil {
		t.Fatalf("CheckLlamaRuntimeUpdate: %v", err)
	}
	if reading.Checked {
		t.Errorf("a custom launcher was checked against llama.cpp's releases: %+v", reading)
	}
	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if status.Update.Checked {
		t.Errorf("status.update = %+v, want nothing for a custom launcher", status.Update)
	}
}

// newUpdateTestApp is an App with one installer rooted at dir and pointed at a test
// release server. The one-time initialisation is marked done so the test never
// touches the real ~/.config/kichat.
func newUpdateTestApp(t *testing.T, dir string, srv *httptest.Server) *App {
	t.Helper()
	a := &App{
		settings:  config.Default(),
		installer: &rt.Installer{Dir: dir, APIBase: srv.URL, Client: srv.Client()},
	}
	a.initOnce.Do(func() {})
	return a
}

// installFakeBuild puts a launcher into the runtime directory under the directory
// name an install would use, which is what Find reads and what the comparison is
// taken from. The script answers nothing: version and device probes fail, which is
// the state a build whose libraries are missing is in anyway.
func installFakeBuild(t *testing.T, runtimeDir, buildDir string) {
	t.Helper()
	path := filepath.Join(runtimeDir, buildDir, "llama-server")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// serveReleases answers like GitHub's release list: one release, carrying the CPU
// build for whatever platform this test runs on, so the fixture does not assume
// Linux.
func serveReleases(t *testing.T, tag string, size int64) *httptest.Server {
	t.Helper()
	name, err := rt.AssetName(tag, goruntime.GOOS, goruntime.GOARCH, rt.AcceleratorCPU)
	if err != nil {
		t.Fatalf("AssetName: %v", err)
	}
	body := fmt.Sprintf(`[{"tag_name":%q,"assets":[{"name":%q,"size":%d,
	  "browser_download_url":"https://example.invalid/%s"}]}]`, tag, name, size, name)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
}
