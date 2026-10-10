package main

import (
	"path/filepath"
	"testing"

	"kichat/internal/config"
	"kichat/internal/ingest"
	"kichat/internal/secret"
	"kichat/internal/store"
)

// A download writes the build it fetched into the settings from its own goroutine, so
// the Settings view and the store hold a snapshot that predates it. Saving that
// snapshot must not put the replaced build back: without this, toggling the theme or
// an unrelated Save after an update made the next start run the old llama.cpp while
// the panel showed the new one, and the same save could strand a fork install under a
// reverted upstream source.
func TestASaveDoesNotUndoAnInstallItDoesNotKnowAbout(t *testing.T) {
	root := t.TempDir()
	makeBuild(t, root, "b11541-cpu")
	makeBuild(t, root, "prism-b10770-6684606-cpu")

	settings := config.Default()
	settings.Runtime.Tag = "b11541-cpu"
	a := newSettingsTestApp(t, settings, root)

	// What the view read before the download ran.
	stale := a.Settings()

	// The install's own write: the user selected the fork and fetched its build.
	a.settingsMu.Lock()
	a.settings.Runtime.Distribution = config.DistPrism
	a.settings.Runtime.Tag = "prism-b10770-6684606-cpu"
	a.settingsMu.Unlock()

	if _, err := a.SaveSettings(stale); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	got := a.Settings().Runtime
	if got.Tag != "prism-b10770-6684606-cpu" {
		t.Errorf("tag after a stale save = %q, want the build the install recorded", got.Tag)
	}
	if got.Distribution != config.DistPrism {
		t.Errorf("distribution after a stale save = %q, want the source the install recorded", got.Distribution)
	}
	// And the start path has to keep finding it: that is what the tag and the source
	// are for.
	status, err := a.GetRuntimeStatus()
	if err != nil {
		t.Fatalf("GetRuntimeStatus: %v", err)
	}
	if !status.Installed || status.Tag != "prism-b10770-6684606-cpu" {
		t.Errorf("status = %+v, want the installed fork build to survive the save", status)
	}
}

// A draft that already carries the recorded tag is current, so the source and the
// family the pickers show are the user's own choice and the save has to keep them.
// That is how switching the source works, and the check above must not swallow it.
func TestASaveOfACurrentDraftKeepsThePickedSource(t *testing.T) {
	root := t.TempDir()
	makeBuild(t, root, "b11541-cpu")
	makeBuild(t, root, "prism-b10770-6684606-cpu")

	settings := config.Default()
	settings.Runtime.Distribution = config.DistPrism
	settings.Runtime.Tag = "prism-b10770-6684606-cpu"
	a := newSettingsTestApp(t, settings, root)

	// Current draft: the tag the installer recorded, but the user has moved the source
	// picker back to upstream.
	draft := a.Settings()
	draft.Runtime.Distribution = config.DistLlamaCPP

	if _, err := a.SaveSettings(draft); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if got := a.Settings().Runtime.Distribution; got != config.DistLlamaCPP {
		t.Errorf("distribution = %q, want the source the picker was set to", got)
	}
}

// newSettingsTestApp is an App rooted at a runtime directory, with a real store and an
// isolated secret store, so SaveSettings runs its whole path without touching the real
// ~/.config/kichat or the user's keyring.
func newSettingsTestApp(t *testing.T, settings config.Settings, runtimeDir string) *App {
	t.Helper()
	a := newCleanupTestApp(t, settings, runtimeDir)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sec, err := secret.NewIsolated(t.TempDir())
	if err != nil {
		t.Fatalf("secret store: %v", err)
	}
	a.store, a.extractor, a.secrets = st, ingest.New(), sec
	return a
}
