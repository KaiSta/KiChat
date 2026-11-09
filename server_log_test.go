package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kichat/internal/config"
)

// A managed server's whole output goes to a file in the data directory, and a backend
// id cannot move it out of there: the id reaches this from saved settings, so a
// separator or a `..` in it would put the file somewhere the app does not own.
func TestAManagedServerKeepsItsOutputInTheDataDirectory(t *testing.T) {
	a := &App{ctx: context.Background()}
	a.dataDir = t.TempDir()

	path := a.serverLogPath("llama", "../../etc/passwd")
	if filepath.Dir(path) != a.dataDir {
		t.Errorf("path = %q, want it directly inside %q", path, a.dataDir)
	}
	if name := filepath.Base(path); strings.ContainsAny(name, `/\`) {
		t.Errorf("file name = %q, want no separators in it", name)
	}

	srv := a.serverFor("b1", config.Default())
	want := filepath.Join(a.dataDir, "llama-b1.log")
	if got := srv.LogPath(); got != want {
		t.Errorf("the server's log is %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("the log file was not created: %v", err)
	}
}
