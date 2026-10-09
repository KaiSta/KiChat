package runtime

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestParseVersionOutput pins the shapes a launcher answers with. The measured one
// comes first: `version: 0.6.0-dev (build 11534, commit 50e3e3e48)`, and the whole
// tail is kept because the build and commit are what tell two installations of the
// same "0.6.0-dev" apart.
func TestParseVersionOutput(t *testing.T) {
	cases := map[string]string{
		"version: 0.6.0-dev (build 11534, commit 50e3e3e48)": "0.6.0-dev (build 11534, commit 50e3e3e48)",
		// Warnings come first in a real run, so the version is searched for rather
		// than read off the first line.
		"load_backend: loaded CPU backend\nversion: 0.6.0-dev (build 11535, commit 50e3e3e48)\n": "0.6.0-dev (build 11535, commit 50e3e3e48)",
		"llama-server version 0.6.0\n": "0.6.0",
		// A line that names only a build is kept whole: it is already the most
		// specific thing the launcher said.
		"build 11534\n": "build 11534",
		"":              "",
		"just some unrelated line without a number": "",
	}
	for output, want := range cases {
		if got := parseVersionOutput(output); got != want {
			t.Errorf("parseVersionOutput(%q) = %q, want %q", output, got, want)
		}
	}
}

// fakeLauncher writes an executable that prints what it is told to.
func fakeLauncher(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a shell script is not a launcher on Windows")
	}
	path := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBinaryVersionReadsTheLauncher runs the probe for real against a scripted
// launcher, so the argument and the output handling are exercised and not only the
// parser.
func TestBinaryVersionReadsTheLauncher(t *testing.T) {
	binary := fakeLauncher(t, `echo "version: 0.6.0-dev (build 11534, commit 50e3e3e48)"`)
	got, err := BinaryVersion(context.Background(), binary)
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.6.0-dev (build 11534, commit 50e3e3e48)" {
		t.Fatalf("version = %q", got)
	}

	// A build that reports on stderr is how it answers in some environments.
	binary = fakeLauncher(t, `echo "version: 0.6.0-dev" >&2`)
	if got, err := BinaryVersion(context.Background(), binary); err != nil || got != "0.6.0-dev" {
		t.Fatalf("stderr version: %q %v", got, err)
	}

	// A build that answers and then exits non-zero still answered.
	binary = fakeLauncher(t, `echo "version: 1.2.3"; exit 3`)
	if got, err := BinaryVersion(context.Background(), binary); err != nil || got != "1.2.3" {
		t.Fatalf("non-zero exit: %q %v", got, err)
	}
}

// TestBinaryVersionReportsWhatItCannotRead: a missing launcher and one that says
// nothing are different failures, and neither may come back as an empty version
// that the UI would render as if the binary were fine.
func TestBinaryVersionReportsWhatItCannotRead(t *testing.T) {
	if _, err := BinaryVersion(context.Background(), filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("a missing launcher must be an error")
	}
	binary := fakeLauncher(t, `echo "nothing useful here"`)
	if _, err := BinaryVersion(context.Background(), binary); err == nil {
		t.Fatal("a launcher that reports no version must be an error")
	}
	if _, err := BinaryVersion(context.Background(), ""); err == nil {
		t.Fatal("an empty path must be an error")
	}
}

// TestBinaryVersionRespectsItsDeadline: a launcher that hangs must not hold a status
// call open. Loading a broken shared library is how that happens in practice.
func TestBinaryVersionRespectsItsDeadline(t *testing.T) {
	binary := fakeLauncher(t, `sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := BinaryVersion(ctx, binary)
	if err == nil {
		t.Fatal("a hanging launcher must be reported")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the probe ran for %s despite its deadline", elapsed)
	}
	if !strings.Contains(err.Error(), "Version von") {
		t.Fatalf("unhelpful error: %v", err)
	}
}
