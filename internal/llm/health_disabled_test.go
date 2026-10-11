package llm

import (
	"context"
	"errors"
	"testing"
)

// TestHealthNamesADisabledBackendWithoutProbingIt is the bug the panel showed as
// "nicht erreichbar" for a backend that had simply never been switched on.
//
// A disabled backend builds no provider, so the health list used to leave it out
// entirely — and the settings screen reads a missing row as a failed probe. A
// user who pasted an API key into the Mistral entry and pressed refresh saw the
// entry stay "unreachable", because the probe it was reporting had never been
// made. The two states are different and have to read differently: "off" is a
// choice the user can undo, "unreachable" is a result.
func TestHealthNamesADisabledBackendWithoutProbingIt(t *testing.T) {
	reg := NewRegistry([]BackendConfig{
		// A dead local port: enabled, so it is probed and must report a failure.
		{ID: "lmstudio", Name: "LM Studio", Kind: "lmstudio", BaseURL: "http://127.0.0.1:1", Enabled: true},
		{ID: "mistral", Name: "Mistral", Kind: "mistral", BaseURL: MistralDefaultBaseURL, Enabled: false},
	})

	rows := reg.Health(context.Background())
	if len(rows) != 2 {
		t.Fatalf("Health returned %d rows, want one per configured backend: %+v", len(rows), rows)
	}

	var enabled, disabled *HealthStatus
	for i := range rows {
		switch rows[i].ID {
		case "lmstudio":
			enabled = &rows[i]
		case "mistral":
			disabled = &rows[i]
		}
	}
	if disabled == nil {
		t.Fatal("no row for the disabled backend: the panel has nothing to explain it with and renders it as unreachable")
	}
	if disabled.Enabled {
		t.Error("Enabled = true on a backend that is switched off")
	}
	if disabled.OK {
		t.Error("OK = true although nothing was contacted")
	}
	if disabled.Error != "" {
		t.Errorf("Error = %q, want empty: nothing was sent, so nothing failed", disabled.Error)
	}
	if disabled.Models != 0 {
		t.Errorf("Models = %d, want 0 for a backend that was not listed", disabled.Models)
	}
	if disabled.Name != "Mistral" || disabled.BaseURL != MistralDefaultBaseURL {
		t.Errorf("row = %+v, want the configured name and address so the panel can label it", disabled)
	}

	// The enabled row is still probed, and still reports what happened.
	if enabled == nil {
		t.Fatal("no row for the enabled backend")
	}
	if !enabled.Enabled {
		t.Error("Enabled = false on a backend that is switched on")
	}
	if enabled.OK {
		t.Error("OK = true although the address is a dead port")
	}
	if enabled.Error == "" {
		t.Error("no Error for an enabled backend that could not be reached")
	}
}

// TestADisabledBackendIsNotReportedAsAMissingKey is the same distinction against
// the message the user actually saw. Without a key, a *probed* hosted backend
// answers ErrMissingAPIKey — so if the disabled row carried that text the panel
// would be telling the user to fix a credential that was not the problem.
func TestADisabledBackendIsNotReportedAsAMissingKey(t *testing.T) {
	// The contrast first: the same configuration, enabled and without a key, does
	// report the credential.
	probed := newMistralProvider("mistral", "Mistral", MistralDefaultBaseURL, "")
	if err := probed.Health(context.Background()); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("an enabled Mistral without a key = %v, want ErrMissingAPIKey", err)
	}

	reg := NewRegistry([]BackendConfig{
		{ID: "mistral", Name: "Mistral", Kind: "mistral", BaseURL: MistralDefaultBaseURL, Enabled: false},
	})
	rows := reg.Health(context.Background())
	if len(rows) != 1 {
		t.Fatalf("Health returned %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].Error != "" {
		t.Errorf("disabled row carries %q; \"switch this on\" is the message, not a failed probe", rows[0].Error)
	}
}
