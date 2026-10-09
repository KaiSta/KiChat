package main

import (
	"encoding/json"
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
