package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// What the cleanup list is allowed to see. A build is a directory with a launcher in
// it; the staging directory of an interrupted install is listed as incomplete; and
// anything else in the runtime directory — a file, a folder somebody put there — is
// left alone, because this package removes what it wrote and no more.
func TestBuildsListsOnlyWhatThisPackageInstalled(t *testing.T) {
	dir := t.TempDir()
	writeBuild(t, dir, "b11535-cpu", 1000)
	writeBuild(t, dir, "b11541-cpu", 2500)
	// The leftovers of an interrupted install: a staging directory with a partial
	// extraction in it, which no launcher has been moved into yet.
	if err := os.MkdirAll(filepath.Join(dir, "b11541-cuda-13.4.installing", "llama-b11541"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b11541-cuda-13.4.installing", "part.bin"), make([]byte, 500), 0o644); err != nil {
		t.Fatal(err)
	}
	// Not ours: a directory without a launcher, and a plain file.
	if err := os.MkdirAll(filepath.Join(dir, "my-notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Three deliberate ages, because the list is newest first and a modification
	// time is what says which build landed when. Without this the leftover —
	// created last — would sort above the build it interrupted.
	old, middle := time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "b11535-cpu", "llama-server"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "b11541-cuda-13.4.installing"), middle, middle); err != nil {
		t.Fatal(err)
	}

	inst := &Installer{Dir: dir}
	builds := inst.Builds()
	if len(builds) != 3 {
		t.Fatalf("got %d builds, want the two installs and the leftover: %+v", len(builds), builds)
	}
	if builds[0].Dir != "b11541-cpu" {
		t.Errorf("first entry = %q, want the newest build", builds[0].Dir)
	}
	if builds[0].SizeBytes != 2500 {
		t.Errorf("size = %d, want the 2500 bytes written into the build", builds[0].SizeBytes)
	}
	if builds[0].Incomplete {
		t.Error("a build with a launcher was reported as an interrupted install")
	}
	last := builds[len(builds)-1]
	if last.Dir != "b11535-cpu" {
		t.Errorf("last entry = %q, want the older build", last.Dir)
	}
	var leftover *Build
	for i := range builds {
		if builds[i].Dir == "b11541-cuda-13.4.installing" {
			leftover = &builds[i]
		}
	}
	if leftover == nil {
		t.Fatalf("the interrupted install is not listed: %+v", builds)
	}
	if !leftover.Incomplete || leftover.SizeBytes != 500 {
		t.Errorf("leftover = %+v, want it marked incomplete and measured", *leftover)
	}
	for _, build := range builds {
		if build.Dir == "my-notes" || build.Dir == "notes.txt" {
			t.Errorf("%q is not this app's to list", build.Dir)
		}
	}
}

// Deleting is the one destructive thing here, so what it refuses matters as much as
// what it deletes: a name that is not a directory name, and a directory this app did
// not write.
func TestRemoveBuildRefusesWhatItCannotAccountFor(t *testing.T) {
	dir := t.TempDir()
	writeBuild(t, dir, "b11535-cpu", 10)
	if err := os.MkdirAll(filepath.Join(dir, "my-notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "keep")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	inst := &Installer{Dir: dir}
	for _, name := range []string{
		"",
		"   ",
		"..",
		"../keep",
		filepath.Join("..", filepath.Base(filepath.Dir(outside)), "keep"),
		"b11535-cpu/llama-server",
		"my-notes",
		"nothing-here",
	} {
		if err := inst.RemoveBuild(name); err == nil {
			t.Errorf("RemoveBuild(%q) deleted something", name)
		}
	}
	for _, survivor := range []string{filepath.Join(dir, "b11535-cpu"), filepath.Join(dir, "my-notes"), outside} {
		if _, err := os.Stat(survivor); err != nil {
			t.Errorf("%s is gone after the refusals: %v", survivor, err)
		}
	}
}

// And what it does delete: one build, the one it was asked for, leaving the others
// where they are. The leftover of an interrupted install needs no launcher to be
// removable — it is precisely the thing nothing else will ever clean up.
func TestRemoveBuildDeletesOneBuildAndItsLeftovers(t *testing.T) {
	dir := t.TempDir()
	writeBuild(t, dir, "b11535-cpu", 10)
	writeBuild(t, dir, "b11541-cpu", 20)
	if err := os.MkdirAll(filepath.Join(dir, "b11541-cuda-13.4.installing"), 0o755); err != nil {
		t.Fatal(err)
	}

	inst := &Installer{Dir: dir}
	if err := inst.RemoveBuild("b11535-cpu"); err != nil {
		t.Fatalf("RemoveBuild: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b11535-cpu")); !os.IsNotExist(err) {
		t.Errorf("the build was asked for and is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b11541-cpu", "llama-server")); err != nil {
		t.Errorf("the other build did not survive: %v", err)
	}
	if err := inst.RemoveBuild("b11541-cuda-13.4.installing"); err != nil {
		t.Fatalf("RemoveBuild on a leftover: %v", err)
	}
	builds := inst.Builds()
	if len(builds) != 1 || builds[0].Dir != "b11541-cpu" {
		t.Errorf("after two removals the list is %+v, want only the untouched build", builds)
	}
}

// An interrupted install can be killed after its archive was flattened, or while a
// several-hundred-megabyte CUDA bundle unpacks, and then its staging directory holds a
// launcher. It is still not a build: it is the newest thing in the directory, so `Find`
// would pick it and a start would run a half-extracted build — upstream's is 17 MB, so
// the archive's own contents decide what is missing — and the cleanup list would stop
// calling the one directory nothing else cleans up a leftover.
func TestAStagingDirectoryWithALauncherIsStillAnInterruptedInstall(t *testing.T) {
	dir := t.TempDir()
	writeBuild(t, dir, "b11541-cpu", 1000)
	// The shape a crash leaves once the wrapper has been flattened: the launcher sits
	// in the staging directory itself, and it was written after the real build.
	staging := filepath.Join(dir, "b11541-cuda-13.4.installing")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(staging, launcherName())
	if err := os.WriteFile(launcher, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(launcher, later, later); err != nil {
		t.Fatal(err)
	}

	inst := &Installer{Dir: dir}
	if tag, binary := inst.Find(LlamaCPP); tag != "b11541-cpu" || binary == "" {
		t.Errorf("Find = %q, %q; want the installed build, never the staging directory", tag, binary)
	}

	var leftover *Build
	for _, build := range inst.Builds() {
		if build.Dir == "b11541-cuda-13.4.installing" {
			found := build
			leftover = &found
		}
	}
	if leftover == nil {
		t.Fatal("the staging directory is not listed at all, so it could never be cleaned up")
	}
	if !leftover.Incomplete {
		t.Errorf("staging directory listed as %+v, want it marked incomplete", *leftover)
	}
	// It is still removable: RemoveBuild refuses a directory holding no launcher, and
	// this one holds the interrupted install's own.
	if err := inst.RemoveBuild("b11541-cuda-13.4.installing"); err != nil {
		t.Errorf("RemoveBuild on the staging directory: %v", err)
	}
}

// The two scratch names an install uses besides the staging directory itself. A crash
// after flattenSingleRoot renamed the archive's inner root leaves a *complete* build
// under `<build>.installing.root` — launcher and all. Recognising only a name ending
// in `.installing` made Find pick exactly that as the newest build, so a start could
// run an unrenamed staging directory, and the cleanup list called it finished.
func TestTheRootAndRuntimeScratchDirectoriesAreLeftovers(t *testing.T) {
	dir := t.TempDir()
	writeBuild(t, dir, "b11541-cpu", 1000)
	// A crash between flattenSingleRoot's rename and its final one: the full build,
	// still under the `.root` name.
	writeBuild(t, dir, "b11541-cuda-13.4.installing.root", 1000)
	// A crash while a companion runtime unpacks: libraries, no launcher.
	runtimeDir := filepath.Join(dir, "b11541-cuda-13.4.installing.runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "libcudart.so.13"), make([]byte, 10), 0o644); err != nil {
		t.Fatal(err)
	}

	inst := &Installer{Dir: dir}
	if tag, _ := inst.Find(LlamaCPP); tag != "b11541-cpu" {
		t.Errorf("Find = %q, want the real build, never a scratch directory", tag)
	}

	incomplete := map[string]bool{}
	for _, build := range inst.Builds() {
		incomplete[build.Dir] = build.Incomplete
	}
	for _, name := range []string{"b11541-cuda-13.4.installing.root", "b11541-cuda-13.4.installing.runtime"} {
		if !incomplete[name] {
			t.Errorf("%s is not listed as an interrupted install: %v", name, incomplete)
		}
		// The cleanup is the only thing that ever removes these.
		if err := inst.RemoveBuild(name); err != nil {
			t.Errorf("RemoveBuild(%s): %v", name, err)
		}
	}
	builds := inst.Builds()
	if len(builds) != 1 || builds[0].Dir != "b11541-cpu" {
		t.Errorf("after the cleanup the list is %+v, want only the real build", builds)
	}
}

// writeBuild makes a directory that looks like an install: a launcher, and one more
// file whose size makes the measured footprint predictable.
func writeBuild(t *testing.T, root, name string, size int) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Empty on purpose: the launcher only has to exist for the directory to count
	// as a build, and leaving it at zero bytes makes the measured footprint exactly
	// the size the caller asked for.
	if err := os.WriteFile(filepath.Join(dir, "llama-server"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "libllama.so"), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}
