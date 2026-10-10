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
// comparison would offer an update to a user who is already past it. The shape of a
// tag belongs to the source it comes from, which is why the comparison is asked of
// one — see internal/runtime/distribution_test.go for the fork's own cases.
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
		if got := rt.LlamaCPP.NewerBuild(tc.latest, tc.installed); got != tc.want {
			t.Errorf("NewerBuild(%q, %q) = %v, want %v", tc.latest, tc.installed, got, tc.want)
		}
	}
}

// An installed build is named by its directory, and the directory carries the
// family and the suite as well as the tag (`b11541`, `b11541-vulkan`,
// `b11541-vulkan-10.0`, and for the fork `prism-b10770-6684606-cuda-13.3`). The
// comparison is against the build, so the extras have to come off — otherwise every
// update would look like a different build twice over.
func TestBuildTagOfStripsTheFamilyAndSuite(t *testing.T) {
	cases := []struct {
		dist rt.Distribution
		dir  string
		want string
	}{
		{rt.LlamaCPP, "b11541", "b11541"},
		{rt.LlamaCPP, "b11541-vulkan", "b11541"},
		{rt.LlamaCPP, "b11541-vulkan-10.0", "b11541"},
		{rt.LlamaCPP, "", ""},
		// The fork's tag carries its own commit, and a directory of a source that
		// was not asked about reads as no tag at all.
		{rt.PrismML, "prism-b10770-6684606-cuda-13.3", "prism-b10770-6684606"},
		{rt.LlamaCPP, "prism-b10770-6684606-cpu", ""},
		{rt.PrismML, "b11541-cpu", ""},
	}
	for _, tc := range cases {
		if got := tc.dist.TagOfDir(tc.dir); got != tc.want {
			t.Errorf("%s.TagOfDir(%q) = %q, want %q", tc.dist.ID, tc.dir, got, tc.want)
		}
	}
}

// The whole path for the fork, through the real bound method: a fork build on disk,
// a newer fork release published, and the two tags read with the fork's own shape.
// This is the case that used to lie — with upstream's tag shape applied to a
// `prism-b10770-6684606-cpu` directory the name reduced to `prism`, which compared as
// "different" against every published tag, so the installed build was offered as an
// update over itself on every poll.
func TestCheckRuntimeUpdateReadsTheSelectedSource(t *testing.T) {
	dir := t.TempDir()
	installFakeBuild(t, dir, "prism-b10770-6684606-cpu")

	srv := serveReleases(t, rt.PrismML, "prism-b10771-abcdef0", 17800388)
	defer srv.Close()

	a := newUpdateTestApp(t, dir, srv)
	a.settings.Runtime.Distribution = config.DistPrism

	reading, err := a.CheckLlamaRuntimeUpdate()
	if err != nil {
		t.Fatalf("CheckLlamaRuntimeUpdate: %v", err)
	}
	if reading.Distribution != config.DistPrism {
		t.Errorf("reading is about %q, want the source the settings name", reading.Distribution)
	}
	if reading.Installed != "prism-b10770-6684606" || reading.Latest != "prism-b10771-abcdef0" {
		t.Errorf("reading = %+v, want the installed fork build against the published one", reading)
	}
	if !reading.Available {
		t.Error("a newer fork build was not offered to a fork installation")
	}

	// The same reading for the *installed* build has to be the quiet one: that is
	// the assertion the old helper failed.
	srv2 := serveReleases(t, rt.PrismML, "prism-b10770-6684606", 17800388)
	defer srv2.Close()
	a2 := newUpdateTestApp(t, dir, srv2)
	a2.settings.Runtime.Distribution = config.DistPrism
	same, err := a2.CheckLlamaRuntimeUpdate()
	if err != nil {
		t.Fatalf("CheckLlamaRuntimeUpdate: %v", err)
	}
	if same.Available {
		t.Errorf("the installed fork build was offered as an update over itself: %+v", same)
	}
	if same.Installed != "prism-b10770-6684606" {
		t.Errorf("installed = %q, want the tag the directory was made from", same.Installed)
	}
}

// A fork build on disk is not something an upstream lookup may compare against: the
// installed side of that comparison has to come out empty, so the marker is quiet
// rather than offering upstream's newest over a build it would replace with a
// different llama.cpp.
func TestAForkBuildOnDiskIsNotAnUpstreamInstallation(t *testing.T) {
	dir := t.TempDir()
	installFakeBuild(t, dir, "prism-b10770-6684606-cpu")

	srv := serveReleases(t, rt.LlamaCPP, "b11541", 17829692)
	defer srv.Close()

	a := newUpdateTestApp(t, dir, srv)
	reading, err := a.CheckLlamaRuntimeUpdate()
	if err != nil {
		t.Fatalf("CheckLlamaRuntimeUpdate: %v", err)
	}
	if reading.Installed != "" {
		t.Errorf("installed = %q, want nothing: the build on disk is not upstream's", reading.Installed)
	}
	if reading.Available {
		t.Errorf("an update was offered over a build of the other source: %+v", reading)
	}

	// And the status the panel reads says the same: no upstream launcher is
	// installed, so the card offers the download rather than a start button.
	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if status.Installed {
		t.Errorf("status reports an installed launcher although only a fork build is on disk: %+v", status)
	}
}

// The whole path the panel depends on, through the real bound method: what is on
// disk, what llama.cpp publishes, and what the status reports afterwards. The
// release listing is served by a test server, so nothing here reaches GitHub.
func TestCheckRuntimeUpdateFindsANewerBuild(t *testing.T) {
	dir := t.TempDir()
	installFakeBuild(t, dir, "b11535-cpu")

	srv := serveReleases(t, rt.LlamaCPP, "b11541", 17829692)
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
	srv := serveReleases(t, rt.LlamaCPP, "b11541", 17829692)
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

// serveReleases answers like GitHub's release list: one release of one source,
// carrying the CPU build for whatever platform this test runs on, so the fixture does
// not assume Linux.
func serveReleases(t *testing.T, dist rt.Distribution, tag string, size int64) *httptest.Server {
	t.Helper()
	name, err := dist.AssetName(tag, goruntime.GOOS, goruntime.GOARCH, rt.AcceleratorCPU)
	if err != nil {
		t.Fatalf("AssetName: %v", err)
	}
	body := fmt.Sprintf(`[{"tag_name":%q,"assets":[{"name":%q,"size":%d,
	  "browser_download_url":"https://example.invalid/%s"}]}]`, tag, name, size, name)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
}
