package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	rt "kichat/internal/runtime"
)

// The build the panel names and the build an update is weighed against are the build a
// start would run — the recorded one — and not merely the newest directory of the
// source. Reading Find here while the start path ran the recorded tag was exactly the
// disagreement this pins: the header showed the build on disk, the start ran the older
// one the settings still named, and the update that older build needed could never be
// offered because the comparison had already been made against the newer one.
func TestThePanelAndTheUpdateCheckNameTheBuildAStartWouldRun(t *testing.T) {
	dir := t.TempDir()
	installFakeBuild(t, dir, "b11535-cpu")
	installFakeBuild(t, dir, "b11541-cpu")
	// Make the newer directory the newest of the two, so Find would pick it on its own.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "b11541-cpu", "llama-server"), later, later); err != nil {
		t.Fatal(err)
	}

	srv := serveReleases(t, rt.LlamaCPP, "b11541", 1000)
	defer srv.Close()
	a := newUpdateTestApp(t, dir, srv)
	// The install record names the older build, as a reverted save once did.
	a.settings.Runtime.Tag = "b11535-cpu"

	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if status.Tag != "b11535-cpu" {
		t.Errorf("status.tag = %q, want the recorded build a start would run", status.Tag)
	}

	reading, err := a.CheckLlamaRuntimeUpdate()
	if err != nil {
		t.Fatalf("CheckLlamaRuntimeUpdate: %v", err)
	}
	if reading.Installed != "b11535" {
		t.Errorf("installed = %q, want the recorded build", reading.Installed)
	}
	if !reading.Available {
		t.Errorf("the recorded build is behind %s but no update was offered: %+v", reading.Latest, reading)
	}
}
