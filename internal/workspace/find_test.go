package workspace

import (
	"reflect"
	"testing"
)

// The two shapes a model writes: a bare name at any depth, and a path anchored to the project
// root. Getting that wrong is the difference between "no files" and "the whole tree".
func TestFindMatchesNamesAndPaths(t *testing.T) {
	root, _ := project(t, map[string]string{
		"main.go":                  "package main\n",
		"internal/app/app.go":      "package app\n",
		"internal/app/app_test.go": "package app\n",
		"internal/store/store.go":  "package store\n",
		"docs/readme.md":           "docs\n",
		"README.md":                "readme\n",
	})

	cases := []struct {
		pattern string
		want    []string
	}{
		{"*.md", []string{"README.md", "docs/readme.md"}},
		{"**/*.go", []string{
			"internal/app/app.go", "internal/app/app_test.go",
			"internal/store/store.go", "main.go",
		}},
		{"internal/*.go", nil},
		{"internal/*/*.go", []string{
			"internal/app/app.go", "internal/app/app_test.go", "internal/store/store.go",
		}},
		{"internal/**/*_test.go", []string{"internal/app/app_test.go"}},
		{"main.go", []string{"main.go"}},
	}
	for _, tc := range cases {
		got, err := root.Find(tc.pattern, 0)
		if err != nil {
			t.Errorf("%q: %v", tc.pattern, err)
			continue
		}
		if !reflect.DeepEqual(got.Paths, tc.want) {
			t.Errorf("%q = %v, want %v", tc.pattern, got.Paths, tc.want)
		}
	}
}

// What a search skips, a find skips: a list of generated files is not the answer to "where is
// this". The fixed list covers node_modules, the project's own .gitignore the rest.
func TestFindSkipsBuildOutputAndIgnoredFiles(t *testing.T) {
	root, _ := project(t, map[string]string{
		".gitignore":          "generated\n",
		"generated/api.go":    "package api\n",
		"node_modules/x/y.js": "x\n",
		"real.go":             "package real\n",
	})

	got, err := root.Find("**/*", 0)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	want := []string{".gitignore", "real.go"}
	if !reflect.DeepEqual(got.Paths, want) {
		t.Errorf("paths = %v, want %v", got.Paths, want)
	}
	if got.Ignored == 0 {
		t.Error("Ignored = 0, but two folders were excluded")
	}
}

// A cap that does not say it was reached reads as the whole tree, which is the one thing a
// file list must not do.
func TestFindReportsATruncatedList(t *testing.T) {
	files := map[string]string{}
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go"} {
		files[name] = "package x\n"
	}
	root, _ := project(t, files)

	got, err := root.Find("*.go", 2)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(got.Paths) != 2 || !got.Truncated {
		t.Errorf("paths = %v truncated = %v, want two paths and a truncation", got.Paths, got.Truncated)
	}
}
