package main

import (
	"encoding/json"
	"strings"
	"testing"

	"kichat/internal/config"
	"kichat/internal/images"
	"kichat/internal/llm"
)

// hostedSettings is the saved shape of a user who switched the picture backend to the
// hosted kind and left everything else alone — which is what the panel writes, so it is
// what the bridge has to answer for.
func hostedSettings(t *testing.T) config.Settings {
	t.Helper()
	settings := config.Default()
	settings.Images.Enabled = true
	settings.Images.Kind = config.ImageKindNIM
	return settings
}

// TestTheHostedKindUsesTheBackendsKey is the whole point of the kind: a user who already
// pasted an NVIDIA token in the Backends tab gets pictures without a second credential,
// and the one they *did* paste for a WebUI is not silently sent as a bearer token.
func TestTheHostedKindUsesTheBackendsKey(t *testing.T) {
	settings := hostedSettings(t)
	a := newSettingsTestApp(t, settings, t.TempDir())
	a.ctx = t.Context()

	// The WebUI credential, which must not be a candidate: it is a "user:pass" pair, and
	// sending it to a bearer-token endpoint is a 401 from a setup that looks configured.
	if err := a.secrets.Set(config.ImageAPIKeyName, "user:secretpass"); err != nil {
		t.Fatalf("store the WebUI credential: %v", err)
	}
	if key := a.imageAPIKey(settings); key != "" {
		t.Fatalf("the hosted kind drew with the WebUI credential (%q)", key)
	}
	a.rebuildServices(settings)
	if a.currentImageProvider() != nil {
		t.Error("a provider was built with no NVIDIA key, so the model is offered a tool that can only fail")
	}

	status, err := a.GetImageStatus()
	if err != nil {
		t.Fatalf("GetImageStatus: %v", err)
	}
	if status.KeySet {
		t.Error("a key was reported before the Backends tab had one")
	}
	if status.BaseURL != images.NIMBaseURL {
		t.Errorf("baseURL = %q, want the vendor's own address (%q) rather than the stored WebUI one", status.BaseURL, images.NIMBaseURL)
	}
	// Nothing runs here, so a program or folder in the report would be a field about a
	// file that will never be started.
	if status.Program != "" || status.Binary != "" || status.Folder != "" {
		t.Errorf("the hosted kind reported a local program: program=%q binary=%q folder=%q", status.Program, status.Binary, status.Folder)
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

	// Now the same token the chat backend uses, under the name the Backends tab stores it by.
	if err := a.secrets.Set(llm.BackendAPIKeyName("nvidia"), "nvapi-test-key"); err != nil {
		t.Fatalf("store the NVIDIA key: %v", err)
	}
	if key := a.imageAPIKey(settings); key != "nvapi-test-key" {
		t.Fatalf("imageAPIKey = %q, want the key from the Backends tab", key)
	}
	a.rebuildServices(settings)
	if a.currentImageProvider() == nil {
		t.Fatal("no provider with a stored NVIDIA key, so the picture tool is missing from a configured setup")
	}
	status, err = a.GetImageStatus()
	if err != nil {
		t.Fatalf("GetImageStatus: %v", err)
	}
	if !status.KeySet {
		t.Error("the borrowed key is not reported, so the panel tells the user to add one they already have")
	}
}

// TestTheHostedKindFindsTheNvidiaRowByKind covers the row a user added themselves: the key
// is stored under that row's own id, so a lookup by the literal id "nvidia" would miss it and
// report a configured setup as keyless. A key belonging to a *different* backend is not a
// candidate either — that is the mistake this rule exists to prevent.
func TestTheHostedKindFindsTheNvidiaRowByKind(t *testing.T) {
	settings := hostedSettings(t)
	settings.Backends = []llm.BackendConfig{
		{ID: "groq-fast", Name: "Groq", Kind: "groq", Enabled: true},
		{ID: "nvidia-work", Name: "NVIDIA (work)", Kind: "nvidia", Enabled: true},
	}
	a := newSettingsTestApp(t, settings, t.TempDir())
	if err := a.secrets.Set(llm.BackendAPIKeyName("groq-fast"), "gsk-not-this-one"); err != nil {
		t.Fatalf("store the Groq key: %v", err)
	}
	if key := a.imageAPIKey(settings); key != "" {
		t.Fatalf("the picture backend drew with another vendor's key (%q)", key)
	}
	if err := a.secrets.Set(llm.BackendAPIKeyName("nvidia-work"), "nvapi-work"); err != nil {
		t.Fatalf("store the NVIDIA key: %v", err)
	}
	if key := a.imageAPIKey(settings); key != "nvapi-work" {
		t.Errorf("imageAPIKey = %q, want the key stored under the row's own id", key)
	}
}

// TestTheHostedKindIsOffUntilItIsChosen keeps the "nothing leaves the machine" promise:
// the hosted kind is a switch, not a fallback, and nothing about it may be reached by an
// installation that never selected it.
func TestTheHostedKindIsOffUntilItIsChosen(t *testing.T) {
	settings := config.Default()
	if got := settings.Images.Kind; got != "" {
		t.Fatalf("a fresh install selects %q; the default is the server the user runs", got)
	}
	if settings.Images.NIMModel != images.NIMModelSchnell {
		t.Errorf("default hosted model = %q, want %q", settings.Images.NIMModel, images.NIMModelSchnell)
	}
	a := newSettingsTestApp(t, settings, t.TempDir())
	a.ctx = t.Context()
	if err := a.secrets.Set(llm.BackendAPIKeyName("nvidia"), "nvapi-test-key"); err != nil {
		t.Fatalf("store the NVIDIA key: %v", err)
	}
	// A key alone must not switch anything on: with the feature enabled and the kind left
	// at the server, the provider is the WebUI client.
	settings.Images.Enabled = true
	a.rebuildServices(settings)
	provider := a.currentImageProvider()
	if provider == nil {
		t.Fatal("the HTTP server kind stopped building a provider")
	}
	if _, hosted := provider.(*images.NIM); hosted {
		t.Error("a stored NVIDIA key switched the hosted kind on by itself")
	}
}
