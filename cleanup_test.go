package main

import (
	"os"
	"path/filepath"
	"testing"

	"kichat/internal/config"
	rt "kichat/internal/runtime"
)

// The guard that matters most: the build KiChat would start cannot be deleted. It is
// the difference between a cleanup and a model that no longer starts, and the reason
// the list marks that line as "in use" instead of leaving the user to work it out.
func TestRemoveLlamaBuildProtectsTheBuildInUse(t *testing.T) {
	dir := t.TempDir()
	makeBuild(t, dir, "b11535-cpu")
	makeBuild(t, dir, "b11541-cpu")

	settings := config.Default()
	settings.Runtime.Tag = "b11541-cpu"
	a := newCleanupTestApp(t, settings, dir)

	builds, err := a.ListLlamaBuilds()
	if err != nil {
		t.Fatalf("ListLlamaBuilds: %v", err)
	}
	if len(builds) != 2 {
		t.Fatalf("got %d builds, want both: %+v", len(builds), builds)
	}
	var inUse string
	for _, build := range builds {
		if build.InUse {
			inUse = build.Dir
		}
	}
	if inUse != "b11541-cpu" {
		t.Errorf("the build in use is %q, want the recorded b11541-cpu: %+v", inUse, builds)
	}

	if err := a.RemoveLlamaBuild("b11541-cpu"); err == nil {
		t.Error("the build in use was deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "b11541-cpu", "llama-server")); err != nil {
		t.Errorf("the build in use is gone: %v", err)
	}

	// The one it does not run, on the other hand, is exactly what the list is for.
	if err := a.RemoveLlamaBuild("b11535-cpu"); err != nil {
		t.Fatalf("RemoveLlamaBuild on an old build: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b11535-cpu")); !os.IsNotExist(err) {
		t.Errorf("the old build was not removed: %v", err)
	}
}

// One press, and what it keeps is what this app would start — never a guess. The
// second press has nothing left to do, which is what makes the button honest about
// being finished.
func TestRemoveOldLlamaBuildsKeepsTheOneInUse(t *testing.T) {
	dir := t.TempDir()
	makeBuild(t, dir, "b11528-cpu")
	makeBuild(t, dir, "b11535-cpu")
	makeBuild(t, dir, "b11541-cpu")

	settings := config.Default()
	settings.Runtime.Tag = "b11541-cpu"
	a := newCleanupTestApp(t, settings, dir)

	removed, err := a.RemoveOldLlamaBuilds()
	if err != nil {
		t.Fatalf("RemoveOldLlamaBuilds: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d builds, want the two that are not in use", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "b11541-cpu", "llama-server")); err != nil {
		t.Errorf("the build in use did not survive: %v", err)
	}
	builds, err := a.ListLlamaBuilds()
	if err != nil {
		t.Fatalf("ListLlamaBuilds: %v", err)
	}
	if len(builds) != 1 || builds[0].Dir != "b11541-cpu" || !builds[0].InUse {
		t.Errorf("after the cleanup the list is %+v, want only the build in use", builds)
	}
	again, err := a.RemoveOldLlamaBuilds()
	if err != nil || again != 0 {
		t.Errorf("second cleanup removed %d builds (err %v), want nothing left to do", again, err)
	}
}

// A launcher the user supplied is not one of these builds, so it protects none of
// them: nothing on disk is in use, and everything the installer wrote is removable.
func TestRemoveOldLlamaBuildsWithACustomLauncher(t *testing.T) {
	dir := t.TempDir()
	makeBuild(t, dir, "b11535-cpu")
	makeBuild(t, dir, "b11541-cpu")

	settings := config.Default()
	settings.Runtime.BinaryPath = filepath.Join(t.TempDir(), "llama-server")
	settings.Runtime.Tag = "b11541-cpu"
	a := newCleanupTestApp(t, settings, dir)

	builds, err := a.ListLlamaBuilds()
	if err != nil {
		t.Fatalf("ListLlamaBuilds: %v", err)
	}
	for _, build := range builds {
		if build.InUse {
			t.Errorf("%s is marked in use although the launcher is the user's own", build.Dir)
		}
	}
	removed, err := a.RemoveOldLlamaBuilds()
	if err != nil {
		t.Fatalf("RemoveOldLlamaBuilds: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d builds, want both", removed)
	}
}

// Which directory a launcher belongs to, which is how "in use" and "running" are
// decided. A path outside the runtime directory belongs to no build here — the user's
// own launcher, or a hard link somewhere else — and a launcher one level deeper than
// usual still belongs to the build above it.
func TestBuildDirOfIsTheBuildALauncherBelongsTo(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "home", "me", ".config", "kichat", "runtime")
	cases := []struct{ binary, want string }{
		{filepath.Join(root, "b11541-cpu", "llama-server"), "b11541-cpu"},
		{filepath.Join(root, "b11541-vulkan-10.0", "llama-server"), "b11541-vulkan-10.0"},
		// An archive whose wrapper could not be unwrapped.
		{filepath.Join(root, "b11541-cpu", "llama-b11541", "llama-server"), "b11541-cpu"},
		{"", ""},
		{filepath.Join(root, "llama-server"), ""},
		{filepath.Join(string(filepath.Separator), "usr", "bin", "llama-server"), ""},
		{filepath.Join(root, "..", "models", "llama-server"), ""},
	}
	for _, c := range cases {
		if got := buildDirOf(root, c.binary); got != c.want {
			t.Errorf("buildDirOf(%q) = %q, want %q", c.binary, got, c.want)
		}
	}
}

// newCleanupTestApp is an App whose installer is rooted in dir, with the one-time
// startup marked done so no test touches the real ~/.config/kichat.
func newCleanupTestApp(t *testing.T, settings config.Settings, runtimeDir string) *App {
	t.Helper()
	a := &App{settings: settings, installer: rt.NewInstaller(runtimeDir)}
	// EventsEmit needs the Wails runtime's own context, which a test does not have, and it
	// calls log.Fatalf rather than returning an error when given anything else. Silencing
	// emit here is what lets a test set a.ctx — needed for a bound call — without the next
	// event killing the test binary. A test that wants to observe events can replace this.
	a.emitFn = func(string, any) {}
	a.initOnce.Do(func() {})
	return a
}

// makeBuild puts a launcher into the runtime directory under a build's own name,
// which is what Find and the settings tag resolve against.
func makeBuild(t *testing.T, runtimeDir, buildDir string) {
	t.Helper()
	dir := filepath.Join(runtimeDir, buildDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "llama-server"), []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
