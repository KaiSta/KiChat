package main

import (
	"encoding/json"
	"strings"
	"testing"

	"kichat/internal/config"
)

// TestTheImageCredentialIsASecretLikeEveryOther mirrors the backend-key rule for
// the image server, because it is the same failure one section over: a "user:pass"
// pasted into the panel must reach the secret store, must not reach the settings
// row, and must survive the *next* save — the one where the field is blank again
// because the bridge never hands a secret back.
func TestTheImageCredentialIsASecretLikeEveryOther(t *testing.T) {
	a := newSettingsTestApp(t, config.Default(), t.TempDir())

	// The user's first click: switch it on, name the address, paste the credential.
	draft := readSettings(t, a)
	draft.Images.Enabled = true
	draft.Images.BaseURL = "http://127.0.0.1:7861"
	draft.Images.APIKey = "user:secretpass"
	if _, err := a.SaveSettings(draft); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if v, ok := a.secrets.Get(config.ImageAPIKeyName); !ok || v != "user:secretpass" {
		t.Fatalf("stored credential = %q (present %v), want the typed one", v, ok)
	}

	// The address is not a secret, and a feature whose address did not survive the
	// save is a feature that is unreachable on the next start.
	loaded, err := config.Load(a.store, a.secrets)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if !loaded.Images.Enabled {
		t.Error("the feature did not stay switched on across a reload")
	}
	if loaded.Images.BaseURL != "http://127.0.0.1:7861" {
		t.Errorf("baseUrl after a reload = %q, want the saved address", loaded.Images.BaseURL)
	}
	if loaded.Images.APIKey != "user:secretpass" {
		t.Errorf("credential after a reload = %q, want the one the save stored", loaded.Images.APIKey)
	}

	// The row itself must be clean. This is the check that matters: the app keeps
	// working either way, because Load shadows the row from the secret store, so a
	// secret in the blob is invisible until the file is read by someone else.
	var blob config.Settings
	found, err := a.store.GetSetting("settings", &blob)
	if err != nil || !found {
		t.Fatalf("read the settings row: found=%v err=%v", found, err)
	}
	if blob.Images.APIKey != "" {
		t.Error("the settings row carries the image credential; secrets live in the secret store")
	}

	// The second save: the panel re-reads sanitised settings first, so the field is
	// empty, and an unrelated edit must not clear the stored credential.
	next := readSettings(t, a)
	if next.Images.APIKey != "" {
		t.Fatal("the sanitised settings carry the credential; they must not")
	}
	next.Images.Steps = 30
	if _, err := a.SaveSettings(next); err != nil {
		t.Fatalf("second SaveSettings: %v", err)
	}
	if v, ok := a.secrets.Get(config.ImageAPIKeyName); !ok || v != "user:secretpass" {
		t.Errorf("an unrelated save cleared the credential: %q (present %v)", v, ok)
	}
	if next, err := config.Load(a.store, a.secrets); err != nil {
		t.Fatalf("config.Load: %v", err)
	} else if next.Images.Steps != 30 {
		t.Errorf("steps = %d, want the saved 30", next.Images.Steps)
	}
}

// TestSecretStatusReportsTheImageCredential is what lets the panel tell a
// configured server from an untouched one. As with the backend keys, only the
// boolean may cross the bridge.
func TestSecretStatusReportsTheImageCredential(t *testing.T) {
	a := newSettingsTestApp(t, config.Default(), t.TempDir())

	status, err := a.GetSecretStatus()
	if err != nil {
		t.Fatalf("GetSecretStatus: %v", err)
	}
	if status.ImageKeySet {
		t.Error("a server with no credential is reported as having one")
	}

	if err := a.secrets.Set(config.ImageAPIKeyName, "user:secretpass"); err != nil {
		t.Fatalf("store credential: %v", err)
	}
	status, err = a.GetSecretStatus()
	if err != nil {
		t.Fatalf("GetSecretStatus: %v", err)
	}
	if !status.ImageKeySet {
		t.Error("a stored credential is not reported, so the panel shows a blank field on a configured server")
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "secretpass") {
		t.Fatal("the credential crossed the bridge")
	}
}

// TestGetImageStatusNeverSendsNullSlices is the rule this codebase has learned
// twice, in two places: a nil Go slice marshals to JSON `null`, not `[]`, and the
// panel iterates both lists. A fresh install with the feature off is exactly the
// shape that produced the last crash — so the empty case is the one that has to be
// asserted, not the populated one.
//
// It is also the "off is not a failure" rule: nothing was contacted, so nothing
// may be reported as broken.
func TestGetImageStatusNeverSendsNullSlices(t *testing.T) {
	a := newSettingsTestApp(t, config.Default(), t.TempDir())

	status, err := a.GetImageStatus()
	if err != nil {
		t.Fatalf("GetImageStatus: %v", err)
	}
	if status.Enabled {
		t.Error("image generation is reported as enabled on a fresh install")
	}
	if status.OK || status.Error != "" {
		t.Errorf("a switched-off feature reported a probe result: %+v", status)
	}
	if status.KeySet {
		t.Error("a fresh install is reported as having an image credential")
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"models":[]`, `"samplers":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the JSON is missing %s, so the panel receives null and iterates it: %s", want, raw)
		}
	}
}

// TestImageGenerationOffLeavesTheToolOut is the consequence the whole feature hangs
// on: with no server configured the model must not be offered generate_image at
// all. A tool the model can see but never use costs a wasted tool round every time
// it tries, and the failure would look like a broken model rather than a setting.
func TestImageGenerationOffLeavesTheToolOut(t *testing.T) {
	a := newSettingsTestApp(t, config.Default(), t.TempDir())
	a.rebuildServices(config.Default())

	toolService := a.toolSvc
	if toolService == nil {
		t.Fatal("no tool service was built")
	}
	if toolService.Images != nil {
		t.Error("a provider was built for a feature that is switched off; nothing may be contacted")
	}
	if _, err := toolService.Execute(t.Context(), "chat-1", "generate_image", `{"prompt":"x"}`); err == nil {
		t.Error("generate_image answered with no server configured")
	}
}
