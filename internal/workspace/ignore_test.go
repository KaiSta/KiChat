package workspace

import (
	"path/filepath"
	"testing"
)

// The reason this matcher exists: a project's own build output is named in its .gitignore and
// nowhere else, and a search that reads those files spends its budget before it reaches the
// source. The fixture is the shape that bit: a generated folder with the needle in it, and the
// real files next to the ignore file.
func TestSearchSkipsWhatGitignoreExcludes(t *testing.T) {
	root, _ := project(t, map[string]string{
		".gitignore":           "dist/\n*.log\n",
		"dist/bundle.js":       "needle\n",
		"notes.log":            "needle\n",
		"internal/app/app.go":  "package app // needle\n",
		"internal/gen/code.go": "package gen // needle\n",
	})

	got, err := root.Search("needle", 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	want := []string{"internal/app/app.go", "internal/gen/code.go"}
	if len(got.Hits) != len(want) {
		t.Fatalf("hits = %+v, want %v", got.Hits, want)
	}
	for i := range want {
		if got.Hits[i].Path != want[i] {
			t.Errorf("hit %d = %q, want %q", i, got.Hits[i].Path, want[i])
		}
	}
	if got.Ignored != 2 {
		t.Errorf("Ignored = %d, want the two ignored files", got.Ignored)
	}
}

// A nested .gitignore is applied after its parent's and the last matching rule wins, which is
// what makes a re-include possible at all — as long as the parent excludes the *contents* and
// not the folder, which is git's own rule.
func TestANestedGitignoreCanReinclude(t *testing.T) {
	root, dir := project(t, map[string]string{
		".gitignore":           "generated/*\n",
		"generated/a.txt":      "here\n",
		"generated/b.txt":      "here\n",
		"generated/.gitignore": "!b.txt\n",
	})

	tree := newIgnoreTree(dir)
	if !tree.ignored(filepath.Join(dir, "generated", "a.txt"), false) {
		t.Error("a.txt is not ignored, but the parent rule excludes it")
	}
	if tree.ignored(filepath.Join(dir, "generated", "b.txt"), false) {
		t.Error("b.txt is ignored, but the nested .gitignore re-includes it")
	}
	if tree.ignored(filepath.Join(dir, "generated"), true) {
		t.Error("the generated folder itself is ignored, but only its contents are")
	}
	_ = root
}

// The other half of that rule, and the one a naive matcher gets wrong: an excluded folder
// hides everything inside it, so no nested .gitignore can bring a file back.
func TestAnExcludedFolderHidesItsContents(t *testing.T) {
	_, dir := project(t, map[string]string{
		".gitignore":           "generated/\n",
		"generated/b.txt":      "here\n",
		"generated/.gitignore": "!b.txt\n",
	})

	tree := newIgnoreTree(dir)
	if !tree.ignored(filepath.Join(dir, "generated"), true) {
		t.Fatal("the generated folder is not ignored")
	}
	if !tree.ignored(filepath.Join(dir, "generated", "b.txt"), false) {
		t.Error("b.txt is not ignored, but its folder is and git cannot re-include it")
	}
}

// The shapes of a pattern a name list cannot express: anchored to the ignore file's own
// directory, matched at any depth, matched across directories with `**`, and the bracket
// classes git borrows from the shell.
func TestGitignorePatternShapes(t *testing.T) {
	cases := []struct {
		rule  string
		rel   string
		isDir bool
		want  bool
	}{
		{"/build", "build", true, true},
		{"/build", "pkg/build", true, false},
		{"*.log", "a/b/c.log", false, true},
		{"*.log", "a/b/c.txt", false, false},
		{"**/gen", "a/b/gen", true, true},
		{"a/**/z.txt", "a/z.txt", false, true},
		{"a/**/z.txt", "a/b/c/z.txt", false, true},
		{"tmp/", "tmp", true, true},
		{"?.txt", "a.txt", false, true},
		{"?.txt", "ab.txt", false, false},
		{"[ab].go", "a.go", false, true},
		{"[ab].go", "c.go", false, false},
		{"[!ab].go", "c.go", false, true},
		{"[!ab].go", "a.go", false, false},
	}
	for _, tc := range cases {
		rule, ok := parseIgnoreLine(tc.rule)
		if !ok {
			t.Errorf("%q did not parse", tc.rule)
			continue
		}
		got := rule.re.MatchString(tc.rel) && (!rule.dirOnly || tc.isDir)
		if got != tc.want {
			t.Errorf("%q against %q (dir=%v) = %v, want %v", tc.rule, tc.rel, tc.isDir, got, tc.want)
		}
	}
}

// `tmp/` means a folder and not a file of the same name, and the two are told apart by the
// walk rather than by the pattern alone.
func TestADirectoryOnlyRuleIgnoresOnlyDirectories(t *testing.T) {
	rule, ok := parseIgnoreLine("tmp/")
	if !ok {
		t.Fatal("tmp/ did not parse")
	}
	if !rule.dirOnly {
		t.Error("tmp/ is not marked as a directory-only rule")
	}
}

// A comment, a blank line and a bare `!` or `/` are not rules. Reading them as one would make
// the walk skip a file named `#` or nothing at all.
func TestGitignoreIgnoresNonRules(t *testing.T) {
	for _, line := range []string{"", "   ", "# a comment", "!", "/", "  # indented comment"} {
		if rule, ok := parseIgnoreLine(line); ok {
			t.Errorf("%q parsed as a rule: %+v", line, rule)
		}
	}
}
