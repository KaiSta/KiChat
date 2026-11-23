package main

import (
	"encoding/json"
	"strings"
	"testing"

	"kichat/internal/config"
	"kichat/internal/llm"
)

func TestGatewayKeyStaysOutOfSettingsAndDiagnostics(t *testing.T) {
	a := newSettingsTestApp(t, config.Default(), t.TempDir())
	key, err := a.GetGatewayKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 64 {
		t.Fatal("connection key must have 256 bits")
	}
	again, err := a.GetGatewayKey()
	if err != nil || key != again {
		t.Fatal("key did not survive a second read")
	}
	if _, err = a.SaveSettings(a.Settings()); err != nil {
		t.Fatal(err)
	}
	s, err := a.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatal("key exposed in settings")
	}
	if got := a.RedactSecrets("Authorization: Bearer " + key); strings.Contains(got, key) {
		t.Fatal("key exposed in diagnostics")
	}
	loaded, err := config.Load(a.store, a.secrets)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Gateway != s.Gateway || loaded.Gateway.Enabled {
		t.Fatal("gateway settings did not round trip with default off")
	}
	status, err := a.GetGatewayStatus()
	if err != nil || status.Running || status.Last != nil {
		t.Fatalf("empty status: %+v %v", status, err)
	}
}

func TestGatewayRequiresAnEnabledBackendAndRejectsSelfRouting(t *testing.T) {
	s := config.Default()
	s.Gateway.Model = llm.Ref{Backend: "external", Model: "qwen"}
	s.Backends = []llm.BackendConfig{{ID: "external", Kind: "custom", BaseURL: "http://localhost:17841/v1", Enabled: true}}
	if validateGateway(s) == nil {
		t.Fatal("accepted recursive gateway")
	}
	s.Backends[0].BaseURL = "http://127.0.0.1:1234/v1"
	if err := validateGateway(s); err != nil {
		t.Fatal(err)
	}
	s.Backends[0].Enabled = false
	if validateGateway(s) == nil {
		t.Fatal("accepted disabled backend")
	}
}
