package workspace

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo makes a repository in a temporary folder and returns it.
//
// The global and system git configuration is cleared so the host's `init.defaultBranch`,
// fpmonitor settings and aliases cannot decide what a test sees. A machine without git skips
// rather than fails: the feature is an addition to the prompt, and every other test in this
// package runs without it.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// entryFor finds a reported path, so an assertion does not depend on git's ordering.
func entryFor(t *testing.T, state GitState, path string) GitEntry {
	t.Helper()
	for _, entry := range state.Entries {
		if entry.Path == path {
			return entry
		}
	}
	t.Fatalf("no entry for %q in %+v", path, state.Entries)
	return GitEntry{}
}

// TestGitStateReportsTheWorkingTree pins what the prompt is given: the branch, every kind of
// pending change with the half of it that is staged, and the line counts of the tracked ones.
func TestGitStateReportsTheWorkingTree(t *testing.T) {
	dir := gitRepo(t)
	write(t, filepath.Join(dir, "main.go"), "package main\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-q", "-m", "first")

	// Three different shapes at once: an unstaged edit, a staged new file, an untracked file.
	write(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	write(t, filepath.Join(dir, "new.go"), "package main\n")
	runGit(t, dir, "add", "new.go")
	write(t, filepath.Join(dir, "notes.txt"), "not tracked\n")

	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := root.GitState()
	if err != nil {
		t.Fatalf("GitState: %v", err)
	}
	if state.Branch != "main" {
		t.Errorf("branch = %q, want main", state.Branch)
	}
	if state.Detached {
		t.Error("a checkout on a branch reported a detached HEAD")
	}
	if state.Total != 3 || len(state.Entries) != 3 {
		t.Fatalf("state = %+v, want three changes", state)
	}

	edited := entryFor(t, state, "main.go")
	if edited.Index != ' ' || edited.Worktree != 'M' {
		t.Errorf("main.go status = %q/%q, want an unstaged modification", edited.Index, edited.Worktree)
	}
	if !edited.Counted || edited.Added != 2 || edited.Deleted != 0 {
		t.Errorf("main.go counts = %+v, want +2 −0", edited)
	}

	added := entryFor(t, state, "new.go")
	if added.Index != 'A' || added.Worktree != ' ' {
		t.Errorf("new.go status = %q/%q, want a staged addition", added.Index, added.Worktree)
	}
	if !added.Counted || added.Added != 1 {
		t.Errorf("new.go counts = %+v, want +1", added)
	}

	untracked := entryFor(t, state, "notes.txt")
	if untracked.Index != '?' || untracked.Worktree != '?' {
		t.Errorf("notes.txt status = %q/%q, want untracked", untracked.Index, untracked.Worktree)
	}
	if untracked.Counted {
		t.Error("an untracked file was given a line diff it does not have")
	}
}

// TestGitStateIsSilentOutsideARepository keeps the block additive: a scratch folder, or a
// machine without git, is not an error to show anybody, it is simply no state.
func TestGitStateIsSilentOutsideARepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.GitState(); !errors.Is(err, ErrNoGit) {
		t.Fatalf("GitState in a plain folder = %v, want ErrNoGit", err)
	}
}

// TestGitStateStaysInsideASubdirectoryProject is the boundary the block would otherwise break.
// With `--porcelain` git reports paths from the repository root, so a project folder inside a
// larger repository would be handed names it cannot resolve — and names from the surrounding
// tree it was never allowed to read. Only the folder's own paths may survive, relative to it.
func TestGitStateStaysInsideASubdirectoryProject(t *testing.T) {
	parent := gitRepo(t)
	project := filepath.Join(parent, "project")
	write(t, filepath.Join(project, "inside.go"), "package project\n")
	// A path git would quote by default, which besides being unreadable would stop it from being
	// recognised as inside the folder at all.
	write(t, filepath.Join(project, "übung.go"), "package project\n")
	write(t, filepath.Join(parent, "sibling", "outside.go"), "package sibling\n")
	runGit(t, parent, "add", ".")
	runGit(t, parent, "commit", "-q", "-m", "first")

	write(t, filepath.Join(project, "inside.go"), "package project\n\nvar Inside = true\n")
	write(t, filepath.Join(project, "übung.go"), "package project\n\nvar Uebung = true\n")
	write(t, filepath.Join(parent, "sibling", "outside.go"), "package sibling\n\nvar Outside = true\n")

	root, err := Open(project)
	if err != nil {
		t.Fatal(err)
	}
	state, err := root.GitState()
	if err != nil {
		t.Fatalf("GitState: %v", err)
	}
	if state.Total != 2 || len(state.Entries) != 2 {
		t.Fatalf("state = %+v, want only the project's own changes", state.Entries)
	}
	for _, want := range []string{"inside.go", "übung.go"} {
		have := false
		for _, entry := range state.Entries {
			have = have || entry.Path == want
		}
		if !have {
			t.Errorf("path %q is missing from %+v, want it relative to the project folder", want, state.Entries)
		}
	}
	if state.Branch != "main" {
		t.Errorf("branch = %q, want the repository's branch", state.Branch)
	}
}

// TestGitStateCapsTheList keeps a dirty tree from spending the window on paths, while still
// reporting how much there was.
func TestGitStateCapsTheList(t *testing.T) {
	dir := gitRepo(t)
	// Tracked files rather than untracked ones: `--untracked-files=normal` collapses a new
	// directory to one entry, so untracked files would never reach the cap.
	total := MaxGitEntries + 5
	for i := 0; i < total; i++ {
		write(t, filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), "x\n")
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-q", "-m", "first")
	for i := 0; i < total; i++ {
		write(t, filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), "x\ny\n")
	}
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := root.GitState()
	if err != nil {
		t.Fatalf("GitState: %v", err)
	}
	if len(state.Entries) != MaxGitEntries {
		t.Errorf("entries = %d, want the %d cap", len(state.Entries), MaxGitEntries)
	}
	if state.Total != total {
		t.Errorf("total = %d, want every change counted even though the list is capped", state.Total)
	}
}
