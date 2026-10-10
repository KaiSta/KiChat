package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kichat/internal/config"
	rt "kichat/internal/runtime"
)

// The bridge does not send Go values, it sends JSON, and a nil slice becomes
// `null` there rather than `[]`. The managed panel reads these arrays — it calls
// `.find` and `.length` on them — so an idle answer with a nil slice took the whole
// Settings view down with it: on a fresh install nothing is configured, so
// `Models` stayed nil, `status.models` arrived as null and
// `status.models.find(...)` threw, which the error boundary caught as
// "TypeError: Cannot read properties of null (reading 'find')" over an otherwise
// blank window. `chat.UnresolvedVars` set the same trap before.
//
// The assertions are on the marshalled JSON rather than on the Go slices, because
// the JSON is what the panel actually sees; `len(x) == 0` passes for a nil slice.
func TestIdleRuntimeStatusSendsArraysNotNull(t *testing.T) {
	cases := []struct {
		name             string
		settings         config.Settings
		goos, goarch     string
		wantModelArray   bool
		wantVariantArray bool
	}{
		{
			// The machine that crashed: nothing configured, a platform with
			// every family published.
			name: "nothing configured", settings: config.Settings{},
			goos: "linux", goarch: "amd64",
			wantModelArray: true, wantVariantArray: true,
		},
		{
			// No published build at all: the family list has to be an empty
			// array too, not null.
			name: "platform with no build", settings: config.Settings{},
			goos: "freebsd", goarch: "amd64",
			wantModelArray: true, wantVariantArray: true,
		},
		{
			name: "one configured backend",
			settings: config.Settings{Runtime: config.RuntimeSettings{Models: map[string]config.ManagedModel{
				"llamacpp": {Enabled: true, ModelPath: "/models/a.gguf", CtxSize: 4096},
			}}},
			goos: "linux", goarch: "amd64",
			wantModelArray: true, wantVariantArray: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := runtimeStatusBase(tc.settings, tc.goos, tc.goarch)
			raw, err := json.Marshal(status)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			body := string(raw)
			if tc.wantModelArray && strings.Contains(body, `"models":null`) {
				t.Errorf("models arrived as null, which the panel reads as an array: %s", body)
			}
			if tc.wantVariantArray && strings.Contains(body, `"variants":null`) {
				t.Errorf("variants arrived as null, which the picker reads as an array: %s", body)
			}
			if !strings.Contains(body, `"models":[`) || !strings.Contains(body, `"variants":[`) {
				t.Errorf("expected both arrays to be present: %s", body)
			}
		})
	}
}

// The bound method the panel actually calls, on the state a fresh install is in:
// default settings, no managed model configured, no build downloaded. This is the
// exact answer that crashed Settings → Backends, so it is asserted through the real
// method rather than only on the helper that builds it.
func TestGetRuntimeStatusOnAFreshInstall(t *testing.T) {
	a := &App{
		settings:  config.Default(),
		installer: rt.NewInstaller(t.TempDir()),
	}
	// Every bound method calls ensure() first, and it would initialise the real
	// ~/.config/kichat. Marking the one-time step done keeps this test off the
	// user's data directory, which a test must never touch.
	a.initOnce.Do(func() {})

	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if status.Installed {
		t.Errorf("a fresh install reports an installed launcher: %+v", status)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	if strings.Contains(body, "null") {
		t.Errorf("the answer reached the panel with a null array in it: %s", body)
	}
	if len(status.Models) != 0 {
		t.Errorf("a fresh install has no managed model, got %+v", status.Models)
	}
}

// The panel's size warning is a comparison of two numbers, and both have to reach
// it from the backend: the model file's size and the free memory the launcher's own
// devices report, in bytes. This walks the real bound method over a stub launcher
// and a real file, so the two cannot come out in different units — the MiB against
// bytes mistake would make a 2 GiB model look small next to a card with 600 MiB
// left, which is the one error that would silently delete the warning.
func TestRuntimeStatusMeasuresTheModelAgainstTheDeviceMemory(t *testing.T) {
	dir := t.TempDir()
	launcher := filepath.Join(dir, "llama-server")
	// A card with 600 MiB free out of 20464 MiB: another process is holding the
	// rest. That is the state the warning is for, and it is a state the model
	// cannot be blamed for.
	script := "#!/bin/sh\n" +
		"[ \"$1\" = \"--list-devices\" ] || exit 3\n" +
		"printf 'Available devices:\\n  Test0: A Card (20464 MiB, 600 MiB free)\\n'\n"
	if err := os.WriteFile(launcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, "weights.gguf")
	// Truncated rather than written: the size is all that is read, and 2 GiB of
	// zeros in a temp dir would be a test that costs disk space for nothing.
	if err := os.WriteFile(model, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(model, 2<<30); err != nil {
		t.Fatal(err)
	}

	settings := config.Default()
	settings.Runtime.BinaryPath = launcher
	settings.Runtime.Models = map[string]config.ManagedModel{
		"llamacpp": {Enabled: true, ModelPath: model},
	}
	a := &App{settings: settings, installer: rt.NewInstaller(t.TempDir())}
	a.initOnce.Do(func() {})

	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if want := int64(600) << 20; status.DeviceFreeBytes != want {
		t.Errorf("deviceFreeBytes = %d, want %d — the 600 MiB the device reported, in bytes", status.DeviceFreeBytes, want)
	}
	if len(status.Models) != 1 {
		t.Fatalf("got %d rows, want the one configured backend: %+v", len(status.Models), status.Models)
	}
	if got := status.Models[0].ModelBytes; got != 2<<30 {
		t.Errorf("modelBytes = %d, want the 2 GiB file on disk", got)
	}
	// The comparison itself, at the sizes above: this is what the panel paints
	// red. It is asserted here because the arithmetic is the behaviour, not the
	// layout.
	if status.Models[0].ModelBytes <= status.DeviceFreeBytes {
		t.Error("a 2 GiB model did not come out larger than 600 MiB of free device memory")
	}
}

// A path that is not there is 0 bytes rather than a failure: the panel reads 0 as
// "unknown" and offers no verdict, which is also what a row with no model
// configured looks like.
func TestRuntimeStatusLeavesAnUnreadableModelUnmeasured(t *testing.T) {
	settings := config.Default()
	settings.Runtime.Models = map[string]config.ManagedModel{
		"llamacpp": {Enabled: true, ModelPath: filepath.Join(t.TempDir(), "gone.gguf")},
	}
	a := &App{settings: settings, installer: rt.NewInstaller(t.TempDir())}
	a.initOnce.Do(func() {})

	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if len(status.Models) != 1 || status.Models[0].ModelBytes != 0 {
		t.Errorf("rows = %+v, want one row with no size for a file that is not there", status.Models)
	}
}

// What the rows say is behaviour, not shape, so it is pinned too: a configured
// backend appears exactly once, with its stored fields, stopped until a server
// says otherwise.
func TestRuntimeStatusListsConfiguredBackendsOnce(t *testing.T) {
	settings := config.Settings{Runtime: config.RuntimeSettings{Models: map[string]config.ManagedModel{
		"zulu":     {ModelPath: "/z.gguf"},
		"llamacpp": {Enabled: true, ModelPath: "/a.gguf", CtxSize: 4096, ExtraArgs: "--n-gpu-layers 99"},
	}}}
	status := runtimeStatusBase(settings, "linux", "amd64")
	if len(status.Models) != 2 {
		t.Fatalf("got %d rows, want one per configured backend: %+v", len(status.Models), status.Models)
	}
	// Sorted, so two status calls that found the same thing do not reshuffle the
	// panel.
	if status.Models[0].BackendID != "llamacpp" || status.Models[1].BackendID != "zulu" {
		t.Errorf("rows are not in a stable order: %+v", status.Models)
	}
	row := status.Models[0]
	if !row.Enabled || row.ModelPath != "/a.gguf" || row.CtxSize != 4096 || row.ExtraArgs != "--n-gpu-layers 99" {
		t.Errorf("stored fields were not carried through: %+v", row)
	}
	if row.State != "stopped" {
		t.Errorf("state = %q before any server was asked, want stopped", row.State)
	}
}
