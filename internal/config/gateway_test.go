package config

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"kichat/internal/store"
)

func TestOldSettingsKeepGatewayOffAndUsableDefaults(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.PutSetting("settings", json.RawMessage(`{"language":"en"}`)); err != nil {
		t.Fatal(err)
	}
	s, err := Load(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Gateway != DefaultGateway() || s.Gateway.Enabled {
		t.Fatalf("defaults: %+v", s.Gateway)
	}
	if got := NormaliseGateway(GatewaySettings{Port: 80, ContextLimit: -1, ReserveTokens: 999999}); got.Port != 17841 || got.ContextLimit != 16384 || got.ReserveTokens > got.ContextLimit/2 {
		t.Fatalf("invalid limits: %+v", got)
	}
}

func TestGatewayAliasSurvivesSaveAndLoad(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := Default()
	s.Gateway.ClientModel = "  qwen4b-q4  "
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := Load(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Gateway.ClientModel != "qwen4b-q4" {
		t.Fatalf("alias = %q", got.Gateway.ClientModel)
	}
	got.Gateway.ClientModel = "  "
	if err := got.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err = Load(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Gateway.ClientModel != "kichat" {
		t.Fatal("blank alias did not fall back to the default")
	}
}
