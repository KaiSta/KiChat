package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"kichat/internal/llm"
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

// TestEnglishIsTheDefaultLanguage pins the shipped default. The German research
// prompt is still the default *prompt*; the interface language is separate.
func TestEnglishIsTheDefaultLanguage(t *testing.T) {
	if got := Default().Language; got != LangEnglish {
		t.Errorf("Default().Language = %q, want %q", got, LangEnglish)
	}
}

// TestNormaliseKeepsSupportedLanguages covers a hand-edited or older settings
// blob: the three shipped languages survive, anything else falls back to English
// rather than leaving the UI unable to resolve a dictionary.
func TestNormaliseKeepsSupportedLanguages(t *testing.T) {
	cases := []struct{ in, want string }{
		{LangEnglish, LangEnglish},
		{LangGerman, LangGerman},
		{LangChinese, LangChinese},
		{"", LangEnglish},
		{"fr", LangEnglish},
		{"DE", LangEnglish},
	}
	for _, tc := range cases {
		s := Default()
		s.Language = tc.in
		s.normalise()
		if s.Language != tc.want {
			t.Errorf("normalise(%q) = %q, want %q", tc.in, s.Language, tc.want)
		}
	}
}

// TestDefaultPresetsKeepTheShippedResearchPrompt is the contract behind "use the
// current prompt as the default": the research preset must stay byte-identical to
// the shipped research prompt of that language, and it must stay the first entry,
// since the frontend's reset button and the new-chat path both start from it.
func TestDefaultPresetsKeepTheShippedResearchPrompt(t *testing.T) {
	for _, lang := range shippedLanguages {
		presets := DefaultPresets(lang)
		if len(presets) == 0 {
			t.Fatalf("no presets shipped for %q", lang)
		}
		if presets[0].ID != PresetResearch {
			t.Errorf("first preset is %q, want %q", presets[0].ID, PresetResearch)
		}
		if want := PromptBody(PresetResearch, lang); presets[0].Prompt != want {
			t.Errorf("the research preset for %q does not match the shipped body", lang)
		}

		seen := map[string]bool{}
		for _, p := range presets {
			if p.ID == "" || p.Name == "" {
				t.Errorf("preset %+v has no id or name; the UI needs both", p)
			}
			if seen[p.ID] {
				t.Errorf("duplicate preset id %q", p.ID)
			}
			seen[p.ID] = true
			if !p.Built {
				t.Errorf("shipped preset %q is not marked as built-in", p.ID)
			}
		}

		// The one added for weaker local models, and the variables it relies on.
		var small string
		for _, p := range presets {
			if p.ID == PresetSmallLocal {
				small = p.Prompt
			}
		}
		if small == "" {
			t.Fatalf("the %q preset ships with no prompt for %q", PresetSmallLocal, lang)
		}
		for _, want := range []string{"{{datetime}}", "{{tool_list}}"} {
			if !strings.Contains(small, want) {
				t.Errorf("the small-model preset for %q does not reference %s", lang, want)
			}
		}
	}
	// The exported constant stays the default language's research prompt.
	if PromptBody(PresetResearch, LangEnglish) != DefaultSystemPrompt {
		t.Error("DefaultSystemPrompt is no longer the English research prompt")
	}
}

// shippedLanguages is every language the interface offers, which is also every
// language a system prompt has to exist in.
var shippedLanguages = []string{LangEnglish, LangGerman, LangChinese}

// TestEveryPresetExistsInEveryLanguage is the point of the whole exercise: an
// English interface must not show a German prompt. A translation that silently
// fell back to English would look like a pass to every other test here, so the
// bodies are compared against each other rather than merely checked for content.
func TestEveryPresetExistsInEveryLanguage(t *testing.T) {
	for _, id := range []string{PresetResearch, PresetSmallLocal, PresetAssistant, PresetCoding} {
		bodies := map[string]string{}
		for _, lang := range shippedLanguages {
			body := PromptBody(id, lang)
			if body == "" {
				t.Fatalf("preset %q has no %q body", id, lang)
			}
			// Every body uses these; losing one silently drops the date or the
			// tool list from the prompt the model receives.
			for _, want := range []string{"{{datetime}}", "{{tool_list}}"} {
				if !strings.Contains(body, want) {
					t.Errorf("the %q body of %q does not reference %s", lang, id, want)
				}
			}
			bodies[lang] = body
		}
		for _, a := range shippedLanguages {
			for _, b := range shippedLanguages {
				if a < b && bodies[a] == bodies[b] {
					t.Errorf("preset %q ships the same text for %q and %q", id, a, b)
				}
			}
		}
	}
}

// TestAnUnknownLanguageFallsBackToTheEnglishPrompts covers a prompt body for a
// language added to the interface before it was translated: an empty textarea
// would be worse than an English one.
func TestAnUnknownLanguageFallsBackToTheEnglishPrompts(t *testing.T) {
	if got := PromptBody(PresetAssistant, "fr"); got != PromptBody(PresetAssistant, LangEnglish) {
		t.Error("an untranslated language did not fall back to the English body")
	}
	if got := PromptBody("not-a-preset", LangGerman); got != "" {
		t.Errorf("an unknown preset id returned %q, want the empty string", got)
	}
}

// TestUneditedPromptsFollowTheAppLanguage is the feature: a settings blob saved
// while the interface was German hands out English prompts once the language is
// switched, in the preset list and in the default prompt alike.
func TestUneditedPromptsFollowTheAppLanguage(t *testing.T) {
	s := Default()
	s.Language = LangGerman
	s.normalise()
	if got := s.DefaultSystemPrompt; got != PromptBody(PresetResearch, LangGerman) {
		t.Fatal("the German default prompt was not applied")
	}

	s.Language = LangEnglish
	s.normalise()
	if got := s.DefaultSystemPrompt; got != PromptBody(PresetResearch, LangEnglish) {
		t.Error("the default prompt did not follow the language switch")
	}
	for _, p := range s.PromptPresets {
		if !p.Built {
			continue
		}
		if want := PromptBody(p.ID, LangEnglish); p.Prompt != want {
			t.Errorf("preset %q kept the German text after the switch", p.ID)
		}
	}

	// And back, so the switch is not one-way.
	s.Language = LangChinese
	s.normalise()
	for _, p := range s.PromptPresets {
		if !p.Built {
			continue
		}
		if want := PromptBody(p.ID, LangChinese); p.Prompt != want {
			t.Errorf("preset %q did not move to Chinese", p.ID)
		}
	}
}

// TestTheSwitchedLanguageSurvivesTheRoundTrip closes the last link: localising
// in memory is only useful if the moved prompts are what the next start reads
// back, so the language switch is saved to a real store and reloaded. This is
// the path SaveSettings takes — localise, then write.
func TestTheSwitchedLanguageSurvivesTheRoundTrip(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// An installation that has been running in English.
	s := Default()
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}

	s.Language = LangGerman
	s.LocalisePrompts()
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}

	back, err := Load(st)
	if err != nil {
		t.Fatal(err)
	}
	if back.Language != LangGerman {
		t.Fatalf("language = %q, want %q", back.Language, LangGerman)
	}
	if want := PromptBody(PresetResearch, LangGerman); back.DefaultSystemPrompt != want {
		t.Error("the default prompt came back in the old language")
	}
	for _, p := range back.PromptPresets {
		if !p.Built {
			continue
		}
		if want := PromptBody(p.ID, LangGerman); p.Prompt != want {
			t.Errorf("preset %q came back in the old language", p.ID)
		}
	}

	// And the untouched German text is still recognised as shipped, so a second
	// switch away from it works instead of freezing this language in place.
	back.Language = LangChinese
	back.LocalisePrompts()
	for _, p := range back.PromptPresets {
		if !p.Built {
			continue
		}
		if want := PromptBody(p.ID, LangChinese); p.Prompt != want {
			t.Errorf("preset %q did not move a second time", p.ID)
		}
	}
}

// TestSwitchingTheLanguageKeepsEditedAndCustomPrompts is the other half of the
// rule: only a prompt that is still exactly a shipped body is translated, so a
// user's own text is never rewritten behind their back.
func TestSwitchingTheLanguageKeepsEditedAndCustomPrompts(t *testing.T) {
	s := Default()
	s.Language = LangGerman
	s.normalise()

	s.PromptPresets = append(s.PromptPresets, PromptPreset{ID: "mine", Name: "Mine", Prompt: "Antworte nur mit Ja oder Nein."})
	for i := range s.PromptPresets {
		if s.PromptPresets[i].ID == PresetCoding {
			s.PromptPresets[i].Prompt = "mein eigener Coding-Prompt"
		}
	}
	s.DefaultSystemPrompt = "mein eigener Standard-Prompt"

	s.Language = LangEnglish
	s.normalise()

	if got := s.DefaultSystemPrompt; got != "mein eigener Standard-Prompt" {
		t.Errorf("the edited default prompt was overwritten: %q", got)
	}
	byID := map[string]string{}
	for _, p := range s.PromptPresets {
		byID[p.ID] = p.Prompt
	}
	if got := byID[PresetCoding]; got != "mein eigener Coding-Prompt" {
		t.Errorf("the preset the user edited was rewritten: %q", got)
	}
	if got := byID["mine"]; got != "Antworte nur mit Ja oder Nein." {
		t.Errorf("the preset the user added was rewritten: %q", got)
	}
	// The one preset nobody touched still moved.
	if got := byID[PresetAssistant]; got != PromptBody(PresetAssistant, LangEnglish) {
		t.Errorf("an untouched preset did not follow the language switch: %q", got)
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

// TestNormaliseAddsMissingBackendsWithoutClobberingEdits covers the other half of
// the upgrade path: the backend list is stored whole too, so a backend shipped in
// a later release (DeepSeek) only reaches an existing installation if normalise
// appends it. That append is what makes the feature arrive without a migration,
// and it must not overwrite a shipped entry the user has configured.
func TestNormaliseAddsMissingBackendsWithoutClobberingEdits(t *testing.T) {
	// A blob written before DeepSeek existed: the two local backends only.
	s := Default()
	s.Backends = []llm.BackendConfig{
		{ID: "lmstudio", Name: "LM Studio", Kind: "lmstudio", BaseURL: "http://127.0.0.1:1234", Enabled: true},
		{ID: "ollama", Name: "Ollama", Kind: "ollama", BaseURL: "http://127.0.0.1:11434", Enabled: false},
	}
	s.normalise()

	byID := map[string]llm.BackendConfig{}
	for _, b := range s.Backends {
		byID[b.ID] = b
	}
	added, ok := byID["deepseek"]
	if !ok {
		t.Fatalf("DeepSeek did not reach existing settings: %+v", s.Backends)
	}
	if added.Kind != "deepseek" || added.BaseURL != llm.DeepSeekDefaultBaseURL {
		t.Errorf("added entry = %+v, want the shipped DeepSeek config", added)
	}
	// It is a metered remote service, so repairing an absence must not enable it.
	if added.Enabled {
		t.Error("the repaired DeepSeek entry is enabled")
	}

	// And an entry the user has already configured is left exactly as it is.
	s2 := Default()
	s2.Backends = []llm.BackendConfig{
		{ID: "deepseek", Name: "Mein DeepSeek", Kind: "deepseek", BaseURL: "http://proxy.lan:8080", APIKey: "configured", Enabled: true},
	}
	s2.normalise()
	if len(s2.Backends) != len(llm.DefaultBackendConfigs()) {
		t.Errorf("backends = %+v, want the configured entry plus the missing defaults", s2.Backends)
	}
	var kept *llm.BackendConfig
	for i := range s2.Backends {
		if s2.Backends[i].ID == "deepseek" {
			kept = &s2.Backends[i]
		}
	}
	if kept == nil {
		t.Fatal("the configured DeepSeek entry disappeared")
	}
	if kept.Name != "Mein DeepSeek" || kept.BaseURL != "http://proxy.lan:8080" || kept.APIKey != "configured" || !kept.Enabled {
		t.Errorf("the user's backend was overwritten: %+v", *kept)
	}
	// Exactly once, in both cases: a duplicated entry would show twice in Settings
	// and be probed twice by the registry.
	seen := map[string]int{}
	for _, b := range s2.Backends {
		seen[b.ID]++
	}
	if seen["deepseek"] != 1 {
		t.Errorf("deepseek appears %d times, want once", seen["deepseek"])
	}
}

// TestNormaliseAddsMissingPresetsWithoutClobberingEdits covers the upgrade path.
// The preset list is stored whole, so a preset added in a later release only
// reaches an existing installation if normalise appends it — and it must append
// without overwriting a built-in the user has edited.
func TestNormaliseAddsMissingPresetsWithoutClobberingEdits(t *testing.T) {
	s := Default()
	s.PromptPresets = []PromptPreset{
		{ID: PresetResearch, Name: "Edited", Prompt: "my own prompt", Built: true},
	}
	s.normalise()

	byID := map[string]PromptPreset{}
	for _, p := range s.PromptPresets {
		byID[p.ID] = p
	}
	if got := byID[PresetResearch].Prompt; got != "my own prompt" {
		t.Errorf("the user's edit was overwritten: %q", got)
	}
	if _, ok := byID[PresetSmallLocal]; !ok {
		t.Error("a preset added in a later release did not reach existing settings")
	}
	// Every shipped preset must be reachable exactly once after the merge.
	seen := map[string]int{}
	for _, p := range s.PromptPresets {
		seen[p.ID]++
	}
	for _, def := range DefaultPresets(LangEnglish) {
		if seen[def.ID] != 1 {
			t.Errorf("preset %q appears %d times, want once", def.ID, seen[def.ID])
		}
	}
}
