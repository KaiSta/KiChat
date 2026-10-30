package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kichat/internal/uimsg"
)

// gitRun runs one git command in the folder with the host's configuration cleared, so a
// `commit.gpgsign`, an `init.defaultBranch` or an alias cannot change what a test sees.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// gitProject is a project whose folder is a repository with one committed file, and a confirmer
// that says yes — so a test can be about whether a command runs rather than about the gate.
func gitProject(t *testing.T) (*Service, *asked, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	conf := &asked{}
	service, _, folder := writeService(t, nil, conf)
	gitRun(t, folder, "init", "-q", "-b", "main")
	gitRun(t, folder, "config", "user.email", "test@example.com")
	gitRun(t, folder, "config", "user.name", "Test")
	writeProjectFile(t, folder, "tracked.go", "package main\n")
	gitRun(t, folder, "add", ".")
	gitRun(t, folder, "commit", "-q", "-m", "first")
	return service, conf, folder
}

func writeProjectFile(t *testing.T, folder, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(folder, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readProjectFile(t *testing.T, folder, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(folder, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestDestructiveGitLinesAreRecognised pins which forms the guard covers and what scope each one
// gets. The scope is the part that matters: a refusal is only honest if it counted the paths the
// command would actually touch.
func TestDestructiveGitLinesAreRecognised(t *testing.T) {
	cases := []struct {
		line  string
		specs []string
	}{
		// checkout: the explicit-paths form, the dot form, and the forced rewrite.
		{"git checkout -- .", []string{"."}},
		{"git checkout -- a.go b.go", []string{"a.go", "b.go"}},
		{"git checkout HEAD -- .", []string{"."}},
		{"git checkout .", []string{"."}},
		{"git checkout ./internal", []string{"./internal"}},
		{"git checkout -f", nil},
		{"git checkout -f main", nil},
		// restore: the working copy, which `--staged` alone is not.
		{"git restore .", []string{"."}},
		{"git restore --worktree .", []string{"."}},
		{"git restore --staged --worktree .", []string{"."}},
		// reset: only --hard throws text away.
		{"git reset --hard", nil},
		{"git reset --hard HEAD~1", nil},
		// clean: -f without -n, and what -x/-X add.
		{"git clean -fd", nil},
		{"git clean -fdx build/", []string{"build/"}},
		{"git clean -fX", nil},
		// One segment of a compound line is enough, and global options are skipped.
		{"git add . && git checkout -- .", []string{"."}},
		{"git --no-pager reset --hard", nil},
	}
	for _, tc := range cases {
		discard, ok := parseDestructiveGit(tc.line)
		if !ok {
			t.Errorf("%q was not recognised as destructive", tc.line)
			continue
		}
		if !slices.Equal(discard.specs, tc.specs) {
			t.Errorf("%q has scope %v, want %v", tc.line, discard.specs, tc.specs)
		}
	}
}

// TestHarmlessGitLinesAreNotGuarded is the other half: a guard that fires on `git status` is a
// guard people turn off. Each entry is something the model does routinely in a dirty tree.
func TestHarmlessGitLinesAreNotGuarded(t *testing.T) {
	for _, line := range []string{
		"git status",
		"git diff",
		"git add .",
		"git commit -m work",
		"git stash",
		"git checkout main",
		"git checkout -b feature",
		"git switch main",
		"git restore --staged .",
		"git reset --soft HEAD~1",
		"git reset --mixed HEAD~1",
		"git clean -n",
		"git clean -nd",
		"echo git reset --hard",
		"go test ./...",
		// A git that runs somewhere else is not measurable against this folder.
		"git -C ../other reset --hard",
		// The pathspecs would be relative to another directory.
		"cd internal && git reset --hard",
		// The words are not the command.
		`bash -c "git reset --hard"`,
	} {
		if discard, ok := parseDestructiveGit(line); ok {
			t.Errorf("%q was treated as destructive (%s, scope %v)", line, discard.verb, discard.specs)
		}
	}
}

// TestADestructiveGitCommandIsRefusedBeforeItIsAsked is the promise of the guard: the person is
// not shown a card for a line the app will not run, and the work is still there afterwards.
func TestADestructiveGitCommandIsRefusedBeforeItIsAsked(t *testing.T) {
	service, conf, folder := gitProject(t)
	edited := "package main\n\nfunc main() {}\n"
	writeProjectFile(t, folder, "tracked.go", edited)

	_, err := service.Execute(context.Background(), "project-1", ToolRunCommand,
		`{"command":`+jsonString("git checkout -- .")+`}`)
	if code := refusalCode(t, err); code != "tools.destructiveGit" {
		t.Fatalf("refusal code = %q, want tools.destructiveGit", code)
	}
	if conf.calls != 0 {
		t.Errorf("the change was confirmed %d times, want the refusal to come first", conf.calls)
	}
	if got := readProjectFile(t, folder, "tracked.go"); got != edited {
		t.Errorf("the file changed although the command was refused:\n%s", got)
	}
	// The sentence the model gets has to name a way forward, not just a failure.
	sentence := uimsg.Sentence(err)
	for _, want := range []string{"tracked.go", "commit or stash"} {
		if !strings.Contains(sentence, want) {
			t.Errorf("the refusal does not mention %q: %s", want, sentence)
		}
	}
}

// TestACleanTreeDoesNotBlockTheSameCommand keeps the guard out of the way: on a clean tree
// `git checkout -- .` has nothing to destroy, so it runs like any other line.
func TestACleanTreeDoesNotBlockTheSameCommand(t *testing.T) {
	service, conf, _ := gitProject(t)

	out, err := service.Execute(context.Background(), "project-1", ToolRunCommand,
		`{"command":`+jsonString("git checkout -- .")+`}`)
	if err != nil {
		t.Fatalf("a clean tree was refused: %v", err)
	}
	if conf.calls != 1 {
		t.Errorf("confirmation calls = %d, want 1", conf.calls)
	}
	if !strings.Contains(out, "Exit-Code 0") {
		t.Errorf("the command did not run:\n%s", out)
	}
}

// TestThePathspecIsTheScope pins the narrow form: a destructive command aimed at a clean path is
// allowed while another file is dirty, and the same command aimed at the dirty one is refused.
func TestThePathspecIsTheScope(t *testing.T) {
	service, conf, folder := gitProject(t)
	writeProjectFile(t, folder, "other.go", "package main\n")
	gitRun(t, folder, "add", "other.go")
	gitRun(t, folder, "commit", "-q", "-m", "second")
	writeProjectFile(t, folder, "tracked.go", "package main\n// modified\n")

	if _, err := service.Execute(context.Background(), "project-1", ToolRunCommand,
		`{"command":`+jsonString("git checkout -- other.go")+`}`); err != nil {
		t.Fatalf("a clean pathspec was refused: %v", err)
	}
	if conf.calls != 1 {
		t.Fatalf("confirmation calls = %d, want the clean path to have been asked about", conf.calls)
	}

	_, err := service.Execute(context.Background(), "project-1", ToolRunCommand,
		`{"command":`+jsonString("git checkout -- tracked.go")+`}`)
	if code := refusalCode(t, err); code != "tools.destructiveGit" {
		t.Fatalf("refusal code = %q, want tools.destructiveGit", code)
	}
	if conf.calls != 1 {
		t.Errorf("the dirty path was confirmed: calls = %d, want 1", conf.calls)
	}
}

// TestCleanAndResetCountDifferentWork keeps the three commands apart. Untracked files are what
// `clean` removes and `reset --hard` leaves alone; tracked modifications are the reverse.
func TestCleanAndResetCountDifferentWork(t *testing.T) {
	service, conf, folder := gitProject(t)
	writeProjectFile(t, folder, "untracked.go", "package main\n")

	// A dry run deletes nothing and is always allowed.
	if _, err := service.Execute(context.Background(), "project-1", ToolRunCommand,
		`{"command":`+jsonString("git clean -n")+`}`); err != nil {
		t.Fatalf("git clean -n was refused: %v", err)
	}
	// reset --hard does not touch untracked files, and nothing tracked is dirty.
	if _, err := service.Execute(context.Background(), "project-1", ToolRunCommand,
		`{"command":`+jsonString("git reset --hard")+`}`); err != nil {
		t.Fatalf("git reset --hard was refused with only an untracked file: %v", err)
	}
	if conf.calls != 2 {
		t.Fatalf("confirmation calls = %d, want the two harmless lines", conf.calls)
	}

	_, err := service.Execute(context.Background(), "project-1", ToolRunCommand,
		`{"command":`+jsonString("git clean -fd")+`}`)
	if code := refusalCode(t, err); code != "tools.destructiveGit" {
		t.Fatalf("refusal code = %q, want tools.destructiveGit", code)
	}
	if _, statErr := os.Stat(filepath.Join(folder, "untracked.go")); statErr != nil {
		t.Errorf("the untracked file was removed although the command was refused: %v", statErr)
	}
}

// TestAnEditedLineIsCheckedLikeANewOne closes the hole the editable card would otherwise open: a
// harmless line that the person rewrites into a destructive one has to face the same refusal.
func TestAnEditedLineIsCheckedLikeANewOne(t *testing.T) {
	service, _, folder := gitProject(t)
	service.Confirm = editingConfirm{line: "git checkout -- ."}.confirm
	edited := "package main\n// uncommitted\n"
	writeProjectFile(t, folder, "tracked.go", edited)

	_, err := service.Execute(context.Background(), "project-1", ToolRunCommand,
		`{"command":`+jsonString("echo harmless")+`}`)
	if code := refusalCode(t, err); code != "tools.destructiveGit" {
		t.Fatalf("refusal code = %q, want tools.destructiveGit", code)
	}
	if got := readProjectFile(t, folder, "tracked.go"); got != edited {
		t.Errorf("the edited line destroyed the work anyway:\n%s", got)
	}
}
