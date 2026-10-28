package main

// A project's changed files, and the undo over them.
//
// The record itself is tested where it is written (internal/tools/snapshot_test.go); this file
// tests what the window gets: a list that shows a diff, and a revert that restores the file it
// names — or refuses, in the two cases where restoring would be the wrong thing to do.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kichat/internal/store"
	"kichat/internal/workspace"
)

// filesTestApp is an app with a real store, a project chat on a fresh folder, and no engine:
// the panel and the revert never need a model, and a test that built one would be testing the
// provider registry instead.
func filesTestApp(t *testing.T) (*App, *store.Store, string) {
	t.Helper()
	a, st := terminalTestApp(t, func(string) error { return nil })
	folder := t.TempDir()
	terminalTestChat(t, st, "p1", store.ModeCoding, folder)
	return a, st, folder
}

// writeDisk puts a file in the project folder the way the person's editor would.
func writeDisk(t *testing.T, folder, name, text string) {
	t.Helper()
	path := filepath.Join(folder, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readDisk is what the assertions are about: the file, not the record of it.
func readDisk(t *testing.T, folder, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(folder, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// recordChange writes the snapshot a tool would have written: the old text, and the digest of
// what the app left on disk.
func recordChange(t *testing.T, st *store.Store, path, before, after string, existed bool) {
	t.Helper()
	if err := st.PutFileSnapshot(store.FileSnapshot{
		ChatID: "p1", Path: path, BeforeText: before, Existed: existed,
		AfterSHA: workspace.Digest(after), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record the change: %v", err)
	}
}

// TestARevertPutsTheFileBack is the promise of the whole feature: after it, the file is what it
// was before the chat touched it, and the row that offered it is gone.
func TestARevertPutsTheFileBack(t *testing.T) {
	a, st, folder := filesTestApp(t)
	writeDisk(t, folder, "main.go", "package changed\n")
	recordChange(t, st, "main.go", "package mine\n", "package changed\n", true)

	if err := a.RevertFileChange("p1", "main.go"); err != nil {
		t.Fatalf("RevertFileChange: %v", err)
	}
	if got := readDisk(t, folder, "main.go"); got != "package mine\n" {
		t.Errorf("the file reads %q, want the text from before the change", got)
	}
	changes, err := a.ProjectFileChanges("p1")
	if err != nil {
		t.Fatalf("ProjectFileChanges: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("%d changes are still listed after a revert", len(changes))
	}
	// A second press on a row the first one restored is not an error: the window may still be
	// showing the list it was handed a moment ago.
	if err := a.RevertFileChange("p1", "main.go"); err != nil {
		t.Errorf("a repeat revert failed: %v", err)
	}
}

// TestARevertOfACreateRemovesTheFile is why the record keeps `existed`: the folder goes back to
// the state it was in, and an empty file is not that state.
func TestARevertOfACreateRemovesTheFile(t *testing.T) {
	a, st, folder := filesTestApp(t)
	writeDisk(t, folder, "docs/new.md", "# New\n")
	recordChange(t, st, "docs/new.md", "", "# New\n", false)

	if err := a.RevertFileChange("p1", "docs/new.md"); err != nil {
		t.Fatalf("RevertFileChange: %v", err)
	}
	if _, err := os.Stat(filepath.Join(folder, "docs/new.md")); !os.IsNotExist(err) {
		t.Errorf("the created file is still there (stat err = %v)", err)
	}
	// The folder stays: it may hold work this chat did not put there.
	if _, err := os.Stat(filepath.Join(folder, "docs")); err != nil {
		t.Errorf("the folder the write created was removed: %v", err)
	}
}

// TestARevertRefusesAFileEditedAfterwards is the one refusal that protects the user's own work.
// A revert is a write, and overwriting an edit made in another editor is exactly the accident
// the digest exists to prevent.
func TestARevertRefusesAFileEditedAfterwards(t *testing.T) {
	a, st, folder := filesTestApp(t)
	writeDisk(t, folder, "main.go", "package mine\nbut edited by hand\n")
	recordChange(t, st, "main.go", "package mine\n", "package changed\n", true)

	if code := codeOf(t, a.RevertFileChange("p1", "main.go")); code != "project.revertConflict" {
		t.Errorf("code = %q, want project.revertConflict", code)
	}
	if got := readDisk(t, folder, "main.go"); !strings.Contains(got, "edited by hand") {
		t.Errorf("the hand edit was overwritten: %q", got)
	}
	// The row survives, because the change it describes is still on disk.
	if changes, _ := a.ProjectFileChanges("p1"); len(changes) != 1 {
		t.Errorf("%d changes are listed, want the one still undoable in principle", len(changes))
	}
}

// TestTheListShowsADiffAndSaysWhetherItCanBeUndone pins what the panel reads: the two sides,
// the counts, and the state of the button — including the file that is gone from the disk,
// which undo can put back.
func TestTheListShowsADiffAndSaysWhetherItCanBeUndone(t *testing.T) {
	a, st, folder := filesTestApp(t)
	writeDisk(t, folder, "changed.go", "one\ntwo\n")
	writeDisk(t, folder, "gone.go", "")
	if err := os.Remove(filepath.Join(folder, "gone.go")); err != nil {
		t.Fatal(err)
	}
	writeDisk(t, folder, "created.go", "package new\n")
	recordChange(t, st, "changed.go", "one\n", "one\ntwo\n", true)
	recordChange(t, st, "gone.go", "alpha\nbeta\n", "", true)
	recordChange(t, st, "created.go", "", "package new\n", false)

	changes, err := a.ProjectFileChanges("p1")
	if err != nil {
		t.Fatalf("ProjectFileChanges: %v", err)
	}
	byPath := map[string]FileChange{}
	for _, change := range changes {
		byPath[change.Path] = change
	}

	changed, ok := byPath["changed.go"]
	if !ok {
		t.Fatalf("the changed file is not listed: %+v", changes)
	}
	if changed.Operation != "changed" || !changed.CanRevert || changed.Blocked != "" {
		t.Errorf("changed.go = %+v, want a revertable change", changed)
	}
	if !strings.Contains(changed.Before, "one") || !strings.Contains(changed.After, "two") {
		t.Errorf("the diff does not carry both sides: %+v", changed)
	}
	if changed.Added != 2 || changed.Removed != 1 {
		t.Errorf("line counts = +%d −%d, want +2 −1", changed.Added, changed.Removed)
	}

	if gone := byPath["gone.go"]; !gone.CanRevert {
		t.Errorf("a file missing from the disk should be restorable: %+v", gone)
	}
	created, ok := byPath["created.go"]
	if !ok || created.Operation != "created" {
		t.Errorf("created.go = %+v, want operation \"created\"", created)
	}
	if created.Added != 1 || created.Removed != 0 {
		t.Errorf("created.go counts = +%d −%d, want +1 −0", created.Added, created.Removed)
	}
}

// TestAFileThatIsNotTextHasNoUndo keeps the panel from offering a button that could only fail:
// nothing was stored for a binary, and the record says so rather than showing an empty diff and
// a live undo.
func TestAFileThatIsNotTextHasNoUndo(t *testing.T) {
	a, st, folder := filesTestApp(t)
	if err := os.WriteFile(filepath.Join(folder, "logo.bin"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	recordChange(t, st, "logo.bin", "", "\x00\x01\x02", true)

	changes, err := a.ProjectFileChanges("p1")
	if err != nil {
		t.Fatalf("ProjectFileChanges: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("%d changes listed, want 1", len(changes))
	}
	if changes[0].CanRevert {
		t.Error("a binary file offers a revert the app cannot perform")
	}
	if changes[0].Blocked != revertBlockedBinary {
		t.Errorf("blocked = %q, want %q", changes[0].Blocked, revertBlockedBinary)
	}
}

// TestAnEmptyListIsNotNil is the trap AGENTS.md names: a nil Go slice marshals to JSON null, and
// the window maps over this list. A project nobody has changed must send [], not nothing.
func TestAnEmptyListIsNotNil(t *testing.T) {
	a, _, _ := filesTestApp(t)
	changes, err := a.ProjectFileChanges("p1")
	if err != nil {
		t.Fatalf("ProjectFileChanges: %v", err)
	}
	if changes == nil {
		t.Error("an unchanged project answered with a nil slice, which reaches the window as null")
	}
	if len(changes) != 0 {
		t.Errorf("%d changes in an untouched project", len(changes))
	}
}
