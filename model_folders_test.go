package main

import (
	"os"
	"path/filepath"
	"testing"

	"kichat/internal/config"
	rt "kichat/internal/runtime"
)

// customFolder finds one of the folders the row offers, by path.
func customFolder(folders []rt.ModelFolder, path string) (rt.ModelFolder, bool) {
	for _, f := range folders {
		if f.Path == path {
			return f, true
		}
	}
	return rt.ModelFolder{}, false
}

// TestPickingASecondFolderRemembersTheFirst is the whole point of the list: a user
// with a library on each of two drives switches between them by clicking a chip, not
// by walking the dialog every time. One remembered folder could not do that — the
// folder being switched away from was forgotten the moment it stopped being the one
// in use — so what is pinned here is that a save keeps the previous folder rather
// than replacing it.
func TestPickingASecondFolderRemembersTheFirst(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "library-one")
	second := filepath.Join(root, "library-two")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("make %s: %v", dir, err)
		}
	}
	a := newSettingsTestApp(t, config.Default(), t.TempDir())

	// The first pick, as the panel sends it: the folder, and the list it heads.
	draft, err := a.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	draft.Runtime.ModelDir = first
	draft.Runtime.ModelDirs = []string{first}
	if _, err := a.SaveSettings(draft); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	// The second pick. The list carries both because the panel keeps it — and the
	// saved blob is what the row is rendered from on the next start.
	draft, err = a.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	draft.Runtime.ModelDir = second
	draft.Runtime.ModelDirs = []string{second, first}
	if _, err := a.SaveSettings(draft); err != nil {
		t.Fatalf("second SaveSettings: %v", err)
	}

	folders := a.ModelFolders()
	positions := map[string]int{}
	for i, f := range folders {
		positions[f.Path] = i
	}
	for _, dir := range []string{first, second} {
		f, ok := customFolder(folders, dir)
		if !ok {
			t.Fatalf("folder %s is not offered at all: %+v", dir, folders)
		}
		if f.Kind != rt.FolderCustom {
			t.Errorf("folder %s kind = %q, want the picked kind", dir, f.Kind)
		}
		if !f.Exists {
			t.Errorf("folder %s reported as missing, want it there", dir)
		}
	}
	if positions[second] > positions[first] {
		t.Errorf("the folders are offered %+v, want the one picked last first", folders)
	}
}

// TestTheFolderInUseIsOfferedEvenWhenTheListIsEmpty covers the two ways a folder
// reaches the row without the list: a store written before the list existed, and a
// path typed straight into the text box. Both end up at the head of the list, because
// a folder in use that has no chip is a folder that can only be switched away from
// through the dialog — which is the walk this row exists to save.
func TestTheFolderInUseIsOfferedEvenWhenTheListIsEmpty(t *testing.T) {
	dir := t.TempDir()
	settings := config.Default()
	settings.Runtime.ModelDir = dir
	a := newSettingsTestApp(t, settings, t.TempDir())

	if _, ok := customFolder(a.ModelFolders(), dir); !ok {
		t.Errorf("the folder in use is not offered: %+v", a.ModelFolders())
	}

	// A save stores it in the list as well as in the field, so the next session's
	// list — and not just the fallback above — carries it.
	draft, err := a.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	draft.Runtime.ModelDirs = nil
	stored, err := a.SaveSettings(draft)
	if err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if len(stored.Runtime.ModelDirs) != 1 || stored.Runtime.ModelDirs[0] != dir {
		t.Errorf("stored folders = %v, want the folder in use alone", stored.Runtime.ModelDirs)
	}
}

// TestARememberedFolderThatIsGoneIsStillNamed: the row is a list of places models
// may be, and a drive that is not mounted is one of them. Dropping the entry would
// lose the user's choice the moment a removable disk was unplugged; reporting it as
// missing is what lets the panel hide the chip until the drive is back instead of
// offering a click that can only return "that folder is not there".
func TestARememberedFolderThatIsGoneIsStillNamed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "unmounted")
	settings := config.Default()
	settings.Runtime.ModelDirs = []string{dir}
	a := newSettingsTestApp(t, settings, t.TempDir())

	f, ok := customFolder(a.ModelFolders(), dir)
	if !ok {
		t.Fatalf("the remembered folder was dropped: %+v", a.ModelFolders())
	}
	if f.Exists {
		t.Errorf("folder %s reported as there, want the missing reading", dir)
	}
}

// TestAnEmptyFieldUsesTheRememberedLibrary: an empty field means "the folder you
// would use", and that has to be the one the row offers first. The panel's
// placeholder names the same folder, so falling back to LM Studio's directory while
// the box showed another path would list the wrong models under the right name.
func TestAnEmptyFieldUsesTheRememberedLibrary(t *testing.T) {
	library := t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	settings := config.Default()
	settings.Runtime.ModelDirs = []string{gone, library}
	a := newSettingsTestApp(t, settings, t.TempDir())

	if got := a.defaultModelDir(); got != library {
		t.Errorf("fallback folder = %q, want the first remembered folder that is there (%q)", got, library)
	}
}
