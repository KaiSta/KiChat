package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"kichat/internal/config"
	"kichat/internal/llm"
)

// mistralListingForTest is the shape /v1/models answers with, trimmed to what the
// mapping needs: one conversation model, plus an entry that is not one.
const mistralListingForTest = `{"object":"list","data":[
	{"id":"mistral-small-latest","object":"model","owned_by":"mistralai","name":"mistral-small-2603",
	 "capabilities":{"completion_chat":true,"function_calling":true,"reasoning":true,"vision":true},
	 "max_context_length":262144,"type":"base"},
	{"id":"mistral-embed-2312","object":"model","owned_by":"mistralai",
	 "capabilities":{"completion_chat":false,"function_calling":false,"reasoning":false,"vision":false},
	 "max_context_length":8192,"type":"base"}
]}`

// fakeMistral answers a listing only for the key it was built with, and counts the
// requests it saw, so a test can tell "the entry was never contacted" from "it was
// contacted and refused".
func fakeMistral(t *testing.T, wantKey string, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(hits, 1)
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantKey {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"detail":"Invalid API Key"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, mistralListingForTest)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mistralRow returns the index of the shipped Mistral entry, pointed at the fake
// server so the test never reaches the vendor.
func mistralBackends(t *testing.T, baseURL string) []llm.BackendConfig {
	t.Helper()
	settings := config.Default()
	found := false
	for i := range settings.Backends {
		if settings.Backends[i].ID == "mistral" {
			settings.Backends[i].BaseURL = baseURL
			found = true
		}
	}
	if !found {
		t.Fatal("the shipped settings have no Mistral entry")
	}
	return settings.Backends
}

func findHealth(t *testing.T, rows []llm.HealthStatus, id string) llm.HealthStatus {
	t.Helper()
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("no health row for %q: the panel has nothing to render for it and falls back to a failure", id)
	return llm.HealthStatus{}
}

func findBackend(t *testing.T, backends []llm.BackendConfig, id string) int {
	t.Helper()
	for i, b := range backends {
		if b.ID == id {
			return i
		}
	}
	t.Fatalf("no backend %q", id)
	return -1
}

// TestAKeyTypedIntoASwitchedOffEntryIsStoredAndStillOff is the report this fix
// comes from, played through the two bound methods the panel calls.
//
// A user pastes their Mistral key into the shipped entry and presses
// "Aktualisieren". The save stores the key — that part always worked — but the
// entry ships switched off, so no provider is built for it and no probe is sent.
// The health report has to say exactly that: an entry that is off was never
// asked, and calling it unreachable sent the user to repair an address nothing
// had contacted.
func TestAKeyTypedIntoASwitchedOffEntryIsStoredAndStillOff(t *testing.T) {
	var hits int32
	srv := fakeMistral(t, "sk-typed", &hits)

	settings := config.Default()
	settings.Backends = mistralBackends(t, srv.URL)
	a := newSettingsTestApp(t, settings, t.TempDir())

	// What the view holds: the key typed into the row, the checkbox untouched.
	draft := readSettings(t, a)
	row := findBackend(t, draft.Backends, "mistral")
	if draft.Backends[row].Enabled {
		t.Fatal("the shipped Mistral entry is enabled; this test is about the off state")
	}

	if _, err := a.SaveSettings(draftPayloadWithKey(t, a, "mistral", "sk-typed")); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	// The key reached the secret store, which is what the user's click does.
	if v, ok := a.secrets.Get(llm.BackendAPIKeyName("mistral")); !ok || v != "sk-typed" {
		t.Fatalf("stored key = %q (present %v), want the typed one", v, ok)
	}

	health, err := a.GetHealth()
	if err != nil {
		t.Fatalf("GetHealth: %v", err)
	}
	got := findHealth(t, health, "mistral")
	if got.Enabled {
		t.Error("the health row claims a switched-off entry is on")
	}
	if got.OK || got.Error != "" {
		t.Errorf("a switched-off entry reported a probe result: %+v", got)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("a switched-off entry was contacted %d times: the box is what asks it", n)
	}
}

// TestTheSecondClickUsesTheKeyTheFirstOneStored continues that flow. The panel
// re-reads sanitized settings after every save, so the key field is blank again —
// the stored key, not the draft, is what the probe has to use. Ticking the box
// and pressing "Aktualisieren" is the whole fix from the user's side, and it only
// works if the key survives the round trip.
func TestTheSecondClickUsesTheKeyTheFirstOneStored(t *testing.T) {
	var hits int32
	srv := fakeMistral(t, "sk-typed", &hits)

	settings := config.Default()
	settings.Backends = mistralBackends(t, srv.URL)
	a := newSettingsTestApp(t, settings, t.TempDir())

	if _, err := a.SaveSettings(draftPayloadWithKey(t, a, "mistral", "sk-typed")); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	// The second click: the box is ticked, the key field is empty because the
	// bridge never hands a secret back.
	next := readSettings(t, a)
	row := findBackend(t, next.Backends, "mistral")
	if next.Backends[row].APIKey != "" {
		t.Fatal("the sanitized settings carry a secret; they must not")
	}
	next.Backends[row].Enabled = true

	if _, err := a.SaveSettings(next); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	health, err := a.GetHealth()
	if err != nil {
		t.Fatalf("GetHealth: %v", err)
	}
	got := findHealth(t, health, "mistral")
	if !got.Enabled {
		t.Fatalf("the enabled entry is still reported as off: %+v", got)
	}
	if !got.OK || got.Models == 0 {
		// The fake server refuses without the key, so this also says the stored
		// key reached the request rather than a blank one.
		t.Fatalf("the enabled entry is not reachable: %+v", got)
	}
	if n := atomic.LoadInt32(&hits); n == 0 {
		t.Error("the enabled entry was never contacted")
	}
}

// TestAnEnabledEntryWithoutAKeySaysSo keeps the two failures apart: a backend
// that really was asked and had nothing to authenticate with is a failure with a
// reason, not the "off" state above.
func TestAnEnabledEntryWithoutAKeySaysSo(t *testing.T) {
	var hits int32
	srv := fakeMistral(t, "sk-typed", &hits)

	settings := config.Default()
	settings.Backends = mistralBackends(t, srv.URL)
	row := findBackend(t, settings.Backends, "mistral")
	settings.Backends[row].Enabled = true
	a := newSettingsTestApp(t, settings, t.TempDir())
	// init() builds the services at startup; this test goes straight to the probe,
	// so it is the one place that has to say so.
	a.rebuildServices(settings)

	health, err := a.GetHealth()
	if err != nil {
		t.Fatalf("GetHealth: %v", err)
	}
	got := findHealth(t, health, "mistral")
	if !got.Enabled || got.OK {
		t.Fatalf("an enabled entry without a key reported %+v, want a failure", got)
	}
	if got.Error == "" {
		t.Error("the failure carries no reason for the panel to show")
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("a request went out without a key: %d", n)
	}
}

// TestSecretStatusNamesTheBackendsWithAStoredKey is what lets the panel tell a
// configured entry from an untouched one. The key itself must not cross the
// bridge: the settings blob is sanitized, so a boolean is the only thing the view
// is allowed to know.
func TestSecretStatusNamesTheBackendsWithAStoredKey(t *testing.T) {
	settings := config.Default()
	a := newSettingsTestApp(t, settings, t.TempDir())

	if err := a.secrets.Set(llm.BackendAPIKeyName("mistral"), "sk-mistral-secret"); err != nil {
		t.Fatalf("store key: %v", err)
	}

	status, err := a.GetSecretStatus()
	if err != nil {
		t.Fatalf("GetSecretStatus: %v", err)
	}
	if !status.BackendKeys["mistral"] {
		t.Errorf("backendKeys = %+v, want mistral reported as having a key", status.BackendKeys)
	}
	if status.BackendKeys["deepseek"] {
		t.Errorf("backendKeys claims a key for a backend that has none: %+v", status.BackendKeys)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "sk-mistral-secret") {
		t.Fatal("the secret crossed the bridge")
	}
}

// readSettings is the bridge call the panel makes when it opens or re-reads its
// draft: settings with every secret blanked.
func readSettings(t *testing.T, a *App) config.Settings {
	t.Helper()
	s, err := a.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	return s
}

// draftPayloadWithKey is the panel's save: the settings as it read them, with the
// given backend's key field filled in by the user.
func draftPayloadWithKey(t *testing.T, a *App, id, key string) config.Settings {
	t.Helper()
	draft := readSettings(t, a)
	row := findBackend(t, draft.Backends, id)
	draft.Backends[row].APIKey = key
	return draft
}
