package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// project builds a folder with a few files and returns it opened.
func project(t *testing.T, files map[string]string) (Root, string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := Open(dir)
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	return root, dir
}

// TestAPathCannotLeaveTheFolder is the property the whole package exists for. The
// cases are the shapes a model actually writes: a traversal, an absolute path to
// somewhere else, and a sibling folder whose name starts the same way.
func TestAPathCannotLeaveTheFolder(t *testing.T) {
	root, dir := project(t, map[string]string{"main.go": "package main\n"})
	outside := filepath.Join(filepath.Dir(dir), "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"../elsewhere/secret.txt",
		"../../etc/passwd",
		"/etc/passwd",
		filepath.Join(outside, "secret.txt"),
		"main.go/../../elsewhere/secret.txt",
	} {
		if _, err := root.Resolve(path); !errors.Is(err, ErrOutside) {
			t.Errorf("Resolve(%q) = %v, want ErrOutside — the folder is the boundary", path, err)
		}
	}
}

// TestAPrefixIsNotContainment is the mistake a string comparison makes: a folder named
// "proj" is a prefix of "project", and only a path-relative comparison tells them
// apart.
func TestAPrefixIsNotContainment(t *testing.T) {
	parent := t.TempDir()
	rootDir := filepath.Join(parent, "proj")
	sibling := filepath.Join(parent, "project")
	for _, dir := range []string{rootDir, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root, err := Open(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.Resolve(filepath.Join(sibling, "file.txt")); !errors.Is(err, ErrOutside) {
		t.Fatalf("a sibling folder whose name starts with the project's was accepted: %v", err)
	}
}

// TestASymlinkOutOfTheFolderIsRefused covers the one escape a textual check cannot see:
// the link lives inside the folder, so the path it was asked for is inside the folder,
// and only resolving it shows where it goes.
func TestASymlinkOutOfTheFolderIsRefused(t *testing.T) {
	root, dir := project(t, map[string]string{"main.go": "package main\n"})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("out\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skipf("this machine cannot create symlinks: %v", err)
	}
	if _, err := root.Resolve("link/secret.txt"); !errors.Is(err, ErrOutside) {
		t.Fatalf("a symlink out of the folder was followed: %v", err)
	}
	// A link that stays inside is ordinary and has to keep working, or a project full
	// of them would be unreadable.
	if err := os.Symlink(filepath.Join(dir, "main.go"), filepath.Join(dir, "inside.go")); err != nil {
		t.Skipf("this machine cannot create symlinks: %v", err)
	}
	if _, err := root.Resolve("inside.go"); err != nil {
		t.Fatalf("a symlink inside the folder was refused: %v", err)
	}
}

func TestDanglingSymlinkCannotCreateOutsideProject(t *testing.T) {
	root, dir := project(t, nil)
	outside := filepath.Join(t.TempDir(), "not-yet-created.txt")
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := root.Write("link.txt", "content"); err == nil {
		t.Fatal("a dangling symlink was accepted for writing")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside file was created: %v", err)
	}
}

// TestAnAbsolutePathInsideTheFolderIsAccepted is the other half of the rule: a model
// that was shown an absolute path writes one back, and refusing it would cost a round
// for a path that is plainly inside.
func TestAnAbsolutePathInsideTheFolderIsAccepted(t *testing.T) {
	root, dir := project(t, map[string]string{"pkg/a.go": "package pkg\n"})
	got, err := root.Resolve(filepath.Join(dir, "pkg", "a.go"))
	if err != nil {
		t.Fatalf("an absolute path inside the folder was refused: %v", err)
	}
	if want := filepath.Join(dir, "pkg", "a.go"); got != want {
		t.Errorf("resolved to %q, want %q", got, want)
	}
	// The empty path and the folder's own root both mean the folder.
	for _, path := range []string{"", ".", "./"} {
		if got, err := root.Resolve(path); err != nil || got != dir {
			t.Errorf("Resolve(%q) = %q, %v; want the folder itself", path, got, err)
		}
	}
}

// TestResolveOpensTheFolderThroughItsOwnSymlinks pins the reason Open resolves once: a
// project reached through a symlinked parent (a moved home directory, /tmp on macOS)
// has to keep working, or every path inside it would compare a resolved child against
// an unresolved root.
func TestResolveOpensTheFolderThroughItsOwnSymlinks(t *testing.T) {
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this machine cannot create symlinks: %v", err)
	}
	root, err := Open(link)
	if err != nil {
		t.Fatalf("open a folder reached through a symlink: %v", err)
	}
	if _, err := root.Resolve("a.txt"); err != nil {
		t.Fatalf("a file inside a symlinked project was refused: %v", err)
	}
}

func TestSearchClipsAMatchNearTheEndOfALongLine(t *testing.T) {
	root, _ := project(t, map[string]string{"bundle.js": strings.Repeat("x", 400) + " needle"})
	matches, err := root.Search("needle", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches.Hits) != 1 || !strings.Contains(matches.Hits[0].Text, "needle") {
		t.Fatalf("matches = %+v, want the end-of-line match", matches)
	}
}

func TestSearchReportsWhenHitLimitCutsOneFile(t *testing.T) {
	root, _ := project(t, map[string]string{"a.txt": "needle one\nneedle two\nneedle three\n"})
	matches, err := root.Search("needle", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches.Hits) != 1 || !matches.Stopped {
		t.Fatalf("matches = %+v, want one hit and a stopped search", matches)
	}
}

func TestAgentsFileDoesNotFollowSymlinkOutsideProject(t *testing.T) {
	root, dir := project(t, map[string]string{"nested/file.go": "package a\n"})
	outside := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(outside, []byte("outside instructions"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{filepath.Join(dir, "AGENTS.md"), filepath.Join(dir, "nested", "AGENTS.md")} {
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	if _, _, err := root.AgentsFile(0); !errors.Is(err, ErrOutside) {
		t.Fatalf("root guidance error = %v, want ErrOutside", err)
	}
	if _, _, err := root.AgentsFileNear("nested/file.go", 0); !errors.Is(err, ErrOutside) {
		t.Fatalf("nested guidance error = %v, want ErrOutside", err)
	}
}

// TestReadReturnsAWindow is what the model gets to see: numbered, bounded, and told
// where the window it did not get starts.
func TestReadReturnsAWindow(t *testing.T) {
	body := "line one\nline two\nline three\nline four\nline five\nline six\n"
	root, _ := project(t, map[string]string{"notes.txt": body})

	whole, err := root.Read("notes.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if whole.Total != 6 || whole.Returned != 6 || whole.Truncated {
		t.Fatalf("a short file read as %+v, want six lines and no truncation", whole)
	}
	if !strings.Contains(whole.Text, "     1| line one") {
		t.Errorf("the window is not line-numbered:\n%s", whole.Text)
	}

	second, err := root.Read("notes.txt", 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if second.From != 3 || second.Returned != 2 || !second.Truncated {
		t.Fatalf("window from line 3 = %+v, want two lines starting at 3", second)
	}
	// A truncated window does not know how long the file is, and says so rather than
	// reporting the count it stopped at as if it were the length.
	if second.Total != 0 {
		t.Errorf("a truncated window reported a total of %d, want none", second.Total)
	}
	if strings.Contains(second.Text, "line two") {
		t.Errorf("the window starts before the offset:\n%s", second.Text)
	}
	if !strings.Contains(second.Text, "line three") || !strings.Contains(second.Text, "line four") {
		t.Errorf("the window lost a line it was asked for:\n%s", second.Text)
	}
}

// TestReadRefusesABinaryFile is the difference between "this file is not text" and a
// page of replacement characters the model would try to reason about.
func TestReadRefusesABinaryFile(t *testing.T) {
	root, dir := project(t, map[string]string{"keep.txt": "x\n"})
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0x7f, 'E', 'L', 'F', 0, 0, 1, 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := root.Read("blob.bin", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Binary || got.Text != "" {
		t.Fatalf("a binary file was handed back as text: %+v", got)
	}
}

// TestReadSaysWhenTheOffsetIsPastTheEnd keeps an empty window from looking like an
// empty file: the model is told how long the file is instead of being left to guess.
func TestReadSaysWhenTheOffsetIsPastTheEnd(t *testing.T) {
	root, _ := project(t, map[string]string{"three.txt": "a\nb\nc\n"})
	got, err := root.Read("three.txt", 40, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.Returned != 0 || got.Total != 3 || got.Truncated {
		t.Fatalf("an offset past the end read as %+v, want no lines and a total of 3", got)
	}
}

// TestListPutsFoldersFirst pins the order the model navigates by, and the omission
// count that keeps a large folder from becoming the whole answer.
func TestListPutsFoldersFirst(t *testing.T) {
	root, _ := project(t, map[string]string{
		"b.txt": "b\n", "a.txt": "a\n", "src/x.go": "package x\n", "zdir/y.go": "package z\n",
	})
	list, err := root.List("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if list.Total != 4 || len(list.Entries) != 4 || list.Omitted != 0 {
		t.Fatalf("listing = %+v, want four entries and none omitted", list)
	}
	if !list.Entries[0].Dir || !list.Entries[1].Dir {
		t.Fatalf("folders are not first: %+v", list.Entries)
	}
	if list.Entries[0].Name != "src" || list.Entries[1].Name != "zdir" {
		t.Errorf("folders are not alphabetical: %+v", list.Entries)
	}

	cut, err := root.List("", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(cut.Entries) != 2 || cut.Omitted != 2 {
		t.Fatalf("a listing capped at two returned %d entries and omitted %d", len(cut.Entries), cut.Omitted)
	}
}

// TestSearchSkipsVendoredCode is the reason the skip list exists: the same string is in
// the project and in a dependency, and the answer should be the project's.
func TestSearchSkipsVendoredCode(t *testing.T) {
	root, _ := project(t, map[string]string{
		"main.go":                   "func FindThing() {}\n",
		"node_modules/pkg/index.js": "function FindThing() {}\n",
		"src/deep/other.go":         "// FindThing is declared in main.go\n",
		".git/objects/findthing":    "FindThing\n",
	})

	got, err := root.Search("findthing", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) != 2 {
		t.Fatalf("search found %d hits, want the two outside node_modules and .git: %+v", len(got.Hits), got.Hits)
	}
	for _, hit := range got.Hits {
		if strings.Contains(hit.Path, "node_modules") || strings.HasPrefix(hit.Path, ".git/") {
			t.Errorf("hit in a skipped folder: %s:%d", hit.Path, hit.Line)
		}
		if !strings.Contains(strings.ToLower(hit.Text), "findthing") {
			t.Errorf("hit %s:%d does not contain the query: %q", hit.Path, hit.Line, hit.Text)
		}
	}
}

// TestSearchIsLiteralAndCaseInsensitive pins the two choices the comment argues for: a
// pattern is text, and a name written in prose is found whatever its case.
func TestSearchIsLiteralAndCaseInsensitive(t *testing.T) {
	root, _ := project(t, map[string]string{
		"re.go": "re := regexp.MustCompile(\"a.b\")\naXb\n",
	})
	got, err := root.Search("a.b", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) != 1 {
		t.Fatalf("a literal search matched %d lines, want only the one with a dot: %+v", len(got.Hits), got.Hits)
	}
	upper, err := root.Search("RE :=", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(upper.Hits) != 1 {
		t.Fatalf("a search ignored case: %+v", upper.Hits)
	}
}

// TestAHitIsAWindowAroundTheMatch is the file a search answers worst: one minified line, or a
// generated JSON, is a single line of megabytes. Returning it whole spends the answer's whole
// room on one hit and holds two hundred of them in memory; returning its first 300 bytes
// hides the text that made it a hit. So a hit is a window, and the match is in it.
func TestAHitIsAWindowAroundTheMatch(t *testing.T) {
	line := strings.Repeat("var a=1;", 900) + "needle" + strings.Repeat(";", 900)
	root, _ := project(t, map[string]string{"bundle.min.js": line + "\n"})

	found, err := root.Search("needle", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Hits) != 1 {
		t.Fatalf("hits = %+v, want the one line", found.Hits)
	}
	hit := found.Hits[0]
	if len(hit.Text) > MaxHitLine+len("……") {
		t.Errorf("a hit is %d bytes, more than the %d window", len(hit.Text), MaxHitLine)
	}
	if !strings.Contains(hit.Text, "needle") {
		t.Errorf("the window cut the match out of the hit: %q", hit.Text)
	}
	// The line number is what locates it precisely, so the window may be a fragment — but it has
	// to look like one rather than like the start of the line.
	if !strings.HasPrefix(hit.Text, "…") {
		t.Errorf("a window from the middle of a line does not say so: %q", hit.Text[:20])
	}
}

// TestAgentsFileReadsTheProjectGuidance is the file the whole project rule is about.
func TestAgentsFileReadsTheProjectGuidance(t *testing.T) {
	ForgetAgentDocs()
	root, dir := project(t, map[string]string{
		AgentsFileName: "Run `make check` before you claim a change works.\n",
	})
	doc, ok, err := root.AgentsFile(0)
	if err != nil || !ok {
		t.Fatalf("AgentsFile = %v, %v; want the file", ok, err)
	}
	if doc.Path != AgentsFileName || doc.Truncated {
		t.Errorf("doc = %+v, want the whole file under its own name", doc)
	}

	// An edit is picked up on the next read, which is the whole reason the file is read
	// per turn rather than once when the project was created.
	edited := "Run `make check` first.\n"
	if err := os.WriteFile(filepath.Join(dir, AgentsFileName), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	// A filesystem with a coarse clock can report the same modification time for both
	// writes, which is why the cache also keys on the size.
	again, _, err := root.AgentsFile(0)
	if err != nil {
		t.Fatal(err)
	}
	if again.Text != edited {
		t.Fatalf("the cache served the old file after an edit: %q", again.Text)
	}
}

// TestAgentsFileIsCapped cuts a file the caller's budget cannot hold, on a line
// boundary, and says it did.
func TestAgentsFileIsCapped(t *testing.T) {
	ForgetAgentDocs()
	root, _ := project(t, map[string]string{
		AgentsFileName: "first line of guidance\nsecond line of guidance\nthird line\n",
	})
	doc, ok, err := root.AgentsFile(28)
	if err != nil || !ok {
		t.Fatalf("AgentsFile = %v, %v", ok, err)
	}
	if !doc.Truncated {
		t.Fatalf("a file beyond the budget was not reported as truncated: %+v", doc)
	}
	if doc.Text != "first line of guidance" {
		t.Errorf("the cut is not on a line boundary: %q", doc.Text)
	}
}

// TestNoAgentsFileIsNotAnError is the empty folder the user asked to build in: there is
// no guidance to read, and that is a state rather than a failure.
func TestNoAgentsFileIsNotAnError(t *testing.T) {
	ForgetAgentDocs()
	root, _ := project(t, map[string]string{"main.go": "package main\n"})
	_, ok, err := root.AgentsFile(0)
	if err != nil || ok {
		t.Fatalf("AgentsFile = %v, %v; want no file and no error", ok, err)
	}
	// The zero Root is a chat with no folder, which is the one case that is an error:
	// nothing was opened, so there is nothing to read out of.
	if _, _, err := (Root{}).AgentsFile(0); !errors.Is(err, ErrNoRoot) {
		t.Fatalf("a Root that was never opened = %v, want ErrNoRoot", err)
	}
}
