package config

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"kichat/internal/store"
)

// TestLoadKeepsDefaultsForFieldsAddedLater pins the contract the whole settings
// blob relies on: it is stored as a single JSON row and decoded over a fully
// populated default, so a field added in a later version keeps its default on an
// existing installation instead of silently arriving as false or zero. Without
// that behaviour a new option would do nothing until the user toggled it twice.
func TestLoadKeepsDefaultsForFieldsAddedLater(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// A blob written before the LLM-context option existed.
	older := `{"language":"de","search":{"providerOrder":["brave","duckduckgo"],"resultCount":8,"cacheMinutes":60}}`
	if err := st.PutSetting("settings", json.RawMessage(older)); err != nil {
		t.Fatal(err)
	}

	got, err := Load(st)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Search.UseLLMContext {
		t.Error("an older settings row switched off the newly added LLM-context option")
	}
	if want := Default().Search.LLMContextTokens; got.Search.LLMContextTokens != want {
		t.Errorf("llmContextTokens = %d, want the default %d", got.Search.LLMContextTokens, want)
	}
}

// TestNormaliseClampsLLMContextTokens covers hand-edited settings and blobs from
// a version with a different default: the API rejects an out-of-range token
// budget, so it is clamped here rather than sent and failed.
func TestNormaliseClampsLLMContextTokens(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 8192},
		{99, 1024},
		{1024, 1024},
		{8192, 8192},
		{32768, 32768},
		{1 << 20, 32768},
	}
	for _, tc := range cases {
		s := Default()
		s.Search.LLMContextTokens = tc.in
		s.normalise()
		if s.Search.LLMContextTokens != tc.want {
			t.Errorf("llmContextTokens %d normalised to %d, want %d", tc.in, s.Search.LLMContextTokens, tc.want)
		}
	}
}
