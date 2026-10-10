package main

import (
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"kichat/internal/config"
	"kichat/internal/ingest"
	rt "kichat/internal/runtime"
	"kichat/internal/store"
)

// TestTheSelectedSourceDecidesWhichBuildKiChatStarts walks the whole app-level path of
// the second distribution: which build the status calls installed, which launcher a
// managed start runs, and what a switch of the source does to a build already on disk.
//
// The two builds on disk are real launchers — the test binary standing in for
// llama-server — so the version the panel shows and the start are both measured rather
// than asserted from a table. What makes this test worth having is the pair of
// mistakes it is written against: reporting the other source's build as "installed"
// (the panel would offer to start a launcher the app refuses to run), and keeping the
// previous source's launcher after a switch (a user who selects the fork to run a
// ternary model would get upstream's llama.cpp, which cannot load it).
func TestTheSelectedSourceDecidesWhichBuildKiChatStarts(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("the launcher is a shell script")
	}
	root := t.TempDir()
	writeFakeBuild(t, root, "b11541-cpu")

	settings := config.Default()
	settings.Runtime.Tag = "b11541-cpu"
	settings.Runtime.Models = map[string]config.ManagedModel{
		"llamacpp": {Enabled: true, ModelPath: writeModelFile(t, root), CtxSize: 4096},
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a := newCleanupTestApp(t, settings, root)
	a.store, a.extractor = st, ingest.New()

	// Upstream selected, an upstream build recorded: everything is as it was before
	// there was a second source.
	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if !status.Installed || status.Tag != "b11541-cpu" {
		t.Errorf("status = %+v, want the upstream build it records", status)
	}
	if status.Distribution != config.DistLlamaCPP {
		t.Errorf("distribution = %q, want the default", status.Distribution)
	}
	if !hasString(status.Distributions, config.DistPrism) {
		t.Errorf("the panel is not told the fork may be selected: %v", status.Distributions)
	}
	if !hasString(status.Variants, "openvino") {
		t.Errorf("upstream's own family list is missing OpenVINO: %v", status.Variants)
	}

	// The switch, with the recorded tag left where it was — which is the state a save
	// of the picker alone produces, and with no fork build on disk yet. The upstream
	// launcher is not a build of the fork, so nothing is installed for it: the panel
	// says so, and a start is refused rather than quietly running the other llama.cpp.
	a.settingsMu.Lock()
	a.settings.Runtime.Distribution = config.DistPrism
	a.settingsMu.Unlock()
	status, err = a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if status.Installed {
		t.Errorf("an upstream build is reported as installed for the fork: %+v", status)
	}
	if status.Tag != "" {
		t.Errorf("tag = %q, want nothing: the build on disk belongs to the other source", status.Tag)
	}
	if hasString(status.Variants, "openvino") {
		t.Errorf("the fork offers a family it does not publish: %v", status.Variants)
	}
	if err := a.StartManagedModel("llamacpp"); err == nil {
		t.Fatal("a model was started on a build of the source that is not selected")
	}

	// With a fork build on disk — what an earlier install of it leaves behind — that
	// build is the one the app names and starts, whatever the recorded tag says. The
	// user selected the fork to run a ternary model; the launcher has to be the fork's,
	// and the version line is what proves which one it was.
	writeFakeBuild(t, root, "prism-b10770-6684606-cpu")
	status, err = a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if !status.Installed || status.Tag != "prism-b10770-6684606-cpu" {
		t.Fatalf("status = %+v, want the fork build that is on disk", status)
	}
	// The version line is the launcher's own, read from the build the source selected
	// — `prism-b10770-6684606-cpu` is build 10770 of the fork.
	if !strings.Contains(status.Version, "10770") {
		t.Errorf("version = %q, want the fork build's own number", status.Version)
	}

	if err := a.StartManagedModel("llamacpp"); err != nil {
		t.Fatalf("StartManagedModel: %v", err)
	}
	defer func() { _ = a.StopManagedModel("llamacpp") }()
	if version := waitForRunning(t, a); !strings.Contains(version, "10770") {
		t.Errorf("version = %q, want the fork's launcher to be the one that started", version)
	}

	// What the cleanup list makes of the two: the fork's build is in use and running,
	// the upstream one is neither — so switching the source leaves the previous build
	// removable rather than pinned forever.
	builds, err := a.ListLlamaBuilds()
	if err != nil {
		t.Fatalf("ListLlamaBuilds: %v", err)
	}
	if fork := rowFor(t, builds, "prism-b10770-6684606-cpu"); !fork.InUse || !fork.Running {
		t.Errorf("the fork build is %+v, want in use and running", fork)
	}
	if upstream := rowFor(t, builds, "b11541-cpu"); upstream.InUse || upstream.Running {
		t.Errorf("the upstream build is %+v, want neither in use nor running", upstream)
	}
	if err := a.RemoveLlamaBuild("b11541-cpu"); err != nil {
		t.Errorf("the build of the source that is no longer selected could not be removed: %v", err)
	}
}

// hasString is the list membership the assertions above read as a question.
func hasString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// The reading a lookup makes has to name the source it was made for: a cached reading
// from upstream's releases says nothing about the newest fork build, and the panel can
// only know to ask again if the reading itself says which source it is about.
func TestAnUpdateReadingNamesItsSource(t *testing.T) {
	dir := t.TempDir()
	installFakeBuild(t, dir, "b11541-cpu")
	srv := serveReleases(t, rt.LlamaCPP, "b11541", 17829692)
	defer srv.Close()

	a := newUpdateTestApp(t, dir, srv)
	reading, err := a.CheckLlamaRuntimeUpdate()
	if err != nil {
		t.Fatalf("CheckLlamaRuntimeUpdate: %v", err)
	}
	if reading.Distribution != config.DistLlamaCPP {
		t.Errorf("reading = %+v, want the source the settings name", reading)
	}
	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if status.Update.Distribution != status.Distribution {
		t.Errorf("status.update.distribution = %q and the selected source is %q, so the panel "+
			"cannot tell whether the reading is about the build it shows",
			status.Update.Distribution, status.Distribution)
	}
}
