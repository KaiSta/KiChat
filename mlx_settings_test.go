package main

import (
	"testing"

	"kichat/internal/config"
)

// The installed mlx-lm version is the installer's record, not a field any control
// edits. A settings draft read before the pip run finishes therefore carries the
// version from before it — or none — and saving any unrelated setting must not put
// that back. This is the same rule Runtime.Tag already follows, and the same bug:
// the panel would say "unknown" about an environment that is there.
func TestASaveDoesNotPutBackTheOldMLXVersion(t *testing.T) {
	settings := config.Default()
	settings.Runtime.MLX.Version = "0.30.1"
	a := newSettingsTestApp(t, settings, t.TempDir())

	// The draft the panel has been holding since before the install: no version,
	// because the install had not written one when the view was opened.
	draft := a.Settings()
	draft.Runtime.MLX.Version = ""
	draft.Language = config.LangGerman

	if _, err := a.SaveSettings(draft); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	if got := a.Settings().Runtime.MLX.Version; got != "0.30.1" {
		t.Errorf("stored mlx-lm version = %q, want the installed 0.30.1", got)
	}
	stored, err := config.Load(a.store, a.secrets)
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	if got := stored.Runtime.MLX.Version; got != "0.30.1" {
		t.Errorf("persisted mlx-lm version = %q, want 0.30.1", got)
	}

	// A path the user typed is theirs, and must survive the same save: it is the
	// one half of this section the panel does edit.
	draft = a.Settings()
	draft.Runtime.MLX.ServerPath = "/opt/homebrew/bin/mlx_lm.server"
	if _, err := a.SaveSettings(draft); err != nil {
		t.Fatalf("SaveSettings with a server path: %v", err)
	}
	if got := a.Settings().Runtime.MLX.ServerPath; got != "/opt/homebrew/bin/mlx_lm.server" {
		t.Errorf("server path = %q, want the draft's value", got)
	}
	if got := a.Settings().Runtime.MLX.Version; got != "0.30.1" {
		t.Errorf("version after a path save = %q, want 0.30.1", got)
	}
}
