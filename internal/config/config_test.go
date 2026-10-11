package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"kichat/internal/llm"
	"kichat/internal/secret"
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

	got, err := Load(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Search.UseLLMContext {
		t.Error("an older settings row switched off the newly added LLM-context option")
	}
	if want := Default().Search.LLMContextTokens; got.Search.LLMContextTokens != want {
		t.Errorf("llmContextTokens = %d, want the default %d", got.Search.LLMContextTokens, want)
	}
	// The summary model defaults to the empty ref, which means "the chat's own
	// model": a stored blob that predates the field must not quietly acquire one,
	// or every compaction would go looking for a backend nobody chose.
	if got.Compact.SummaryModel != (llm.Ref{}) {
		t.Errorf("summaryModel = %+v, want an empty ref meaning the chat's own model",
			got.Compact.SummaryModel)
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
	ss, err := secret.NewIsolated(t.TempDir() + "/secrets")
	if err != nil {
		t.Fatal(err)
	}
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

	back, err := Load(st, ss)
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

	// llama.cpp arrives the same way, and is also shipped off — 9931 is its
	// documented default but not a port llama-server owns the way LM Studio owns
	// 1234, and the server is normally started by hand on a port the user picked.
	llama, ok := byID["llamacpp"]
	if !ok {
		t.Fatalf("llama.cpp did not reach existing settings: %+v", s.Backends)
	}
	if llama.Kind != "llamacpp" || llama.BaseURL != llm.LlamaCppDefaultBaseURL {
		t.Errorf("added entry = %+v, want the shipped llama.cpp config", llama)
	}
	if llama.Enabled {
		t.Error("the repaired llama.cpp entry is enabled")
	}

	// Mistral arrives the same way — the second hosted backend shipped after this
	// test was written, which is exactly the case it exists to cover — and is
	// also off, because it is metered and needs a key.
	mistral, ok := byID["mistral"]
	if !ok {
		t.Fatalf("Mistral did not reach existing settings: %+v", s.Backends)
	}
	if mistral.Kind != "mistral" || mistral.BaseURL != llm.MistralDefaultBaseURL {
		t.Errorf("added entry = %+v, want the shipped Mistral config", mistral)
	}
	if mistral.Enabled {
		t.Error("the repaired Mistral entry is enabled")
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
	// and be probed twice by the registry. Every shipped id is checked, so a new
	// backend that is appended unconditionally — rather than only when it is
	// missing — fails here instead of showing up twice in the settings panel.
	seen := map[string]int{}
	for _, b := range s2.Backends {
		seen[b.ID]++
	}
	if seen["deepseek"] != 1 {
		t.Errorf("deepseek appears %d times, want once", seen["deepseek"])
	}
	for _, b := range s2.Backends {
		if seen[b.ID] != 1 {
			t.Errorf("%s appears %d times, want once", b.ID, seen[b.ID])
		}
	}
}

// TestNormaliseRestoresAShippedBackendsAddress covers a settings blob that names
// a shipped backend but carries no address for it — which is what an entry added
// by hand, or a partly written blob, looks like. Both shipped kinds with a
// documented address are repaired, and a kind without one is left alone so no
// address is invented for it.
func TestNormaliseRestoresAShippedBackendsAddress(t *testing.T) {
	s := Default()
	s.Backends = []llm.BackendConfig{
		{ID: "llamacpp", Name: "llama.cpp", Kind: "llamacpp", Enabled: true},
		{ID: "deepseek", Name: "DeepSeek", Kind: "deepseek", Enabled: true},
		{ID: "mistral", Name: "Mistral", Kind: "mistral", Enabled: true},
		{ID: "my-server", Name: "Eigenbau", Kind: "custom", Enabled: true},
	}
	s.normalise()

	got := map[string]string{}
	for _, b := range s.Backends {
		got[b.ID] = b.BaseURL
	}
	if got["llamacpp"] != llm.LlamaCppDefaultBaseURL {
		t.Errorf("llama.cpp address = %q, want %q", got["llamacpp"], llm.LlamaCppDefaultBaseURL)
	}
	if got["deepseek"] != llm.DeepSeekDefaultBaseURL {
		t.Errorf("deepseek address = %q, want %q", got["deepseek"], llm.DeepSeekDefaultBaseURL)
	}
	// The field shows the address the app would really use, and the /v1 prefix is
	// part of it: an empty field repaired to the bare host would name an address
	// no route of that API answers on.
	if got["mistral"] != llm.MistralDefaultBaseURL {
		t.Errorf("mistral address = %q, want %q", got["mistral"], llm.MistralDefaultBaseURL)
	}
	if got["my-server"] != "" {
		t.Errorf("a custom endpoint was given an invented address: %q", got["my-server"])
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

// TestLoadReadsKeysFromTheSecretStore pins the split of responsibilities the
// secret store introduced: the settings blob is written without secrets, so the
// Brave key and each backend's API key only come back when Load is handed the
// secret store. Without this the keys a user typed in Settings would be gone on
// the next start — the exact bug the reconcile path was added to fix.
func TestLoadReadsKeysFromTheSecretStore(t *testing.T) {
	t.Run("the brave key is read back", func(t *testing.T) {
		ss, err := secret.NewIsolated(filepath.Join(t.TempDir(), "secrets"))
		if err != nil {
			t.Fatal(err)
		}
		if err := ss.Set("brave", "brave-key"); err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if err := Default().Save(st); err != nil {
			t.Fatal(err)
		}

		got, err := Load(st, ss)
		if err != nil {
			t.Fatal(err)
		}
		if got.Search.BraveAPIKey != "brave-key" {
			t.Errorf("BraveAPIKey = %q, want the key from the secret store", got.Search.BraveAPIKey)
		}
	})

	t.Run("a backend key is read back", func(t *testing.T) {
		ss, err := secret.NewIsolated(filepath.Join(t.TempDir(), "secrets"))
		if err != nil {
			t.Fatal(err)
		}
		if err := ss.Set(llm.BackendAPIKeyName("deepseek"), "sk-deepseek"); err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		// The blob SaveSettings writes: Default() already carries the shipped
		// DeepSeek entry, and its key is blank in the blob by design.
		if err := Default().Save(st); err != nil {
			t.Fatal(err)
		}

		got, err := Load(st, ss)
		if err != nil {
			t.Fatal(err)
		}
		var key string
		for _, b := range got.Backends {
			if b.ID == "deepseek" {
				key = b.APIKey
			}
		}
		if key != "sk-deepseek" {
			t.Errorf("deepseek key = %q, want the key from the secret store", key)
		}
	})
}

// TestRuntimeSettingsAreUsableAsStored pins the managed-llama.cpp section the way
// every other settings field is pinned: a blob written before the section existed
// decodes over Default(), and whatever survives that has to be something the
// installer and the supervisor can act on.
//
// The accelerator in particular is not cosmetic: it is interpolated into the
// archive name that gets downloaded, so a stored value that is not a published
// build would ask GitHub for a file that does not exist.
func TestRuntimeSettingsAreUsableAsStored(t *testing.T) {
	// A blob from before the field: the zero values are what normalise sees.
	s := Default()
	s.Runtime = RuntimeSettings{}
	s.normalise()
	if s.Runtime.Accelerator != AccelCPU {
		t.Errorf("accelerator = %q, want the build that needs no GPU toolkit", s.Runtime.Accelerator)
	}
	if s.Runtime.ReadySeconds != DefaultReadySeconds {
		t.Errorf("ready seconds = %d, want %d", s.Runtime.ReadySeconds, DefaultReadySeconds)
	}
	if s.Runtime.Models == nil {
		t.Error("models map is nil, want an empty map so a managed entry can be added")
	}
	if s.Runtime.ModelDir != "" {
		t.Errorf("model dir = %q, want empty: an unset folder means \"use the one that is there\", not a path invented at normalise time", s.Runtime.ModelDir)
	}

	// A chosen folder survives even when it is not mounted right now, because
	// clearing it would quietly undo the user's choice; surrounding space does not.
	s = Default()
	s.Runtime = RuntimeSettings{ModelDir: "  /mnt/models  "}
	s.normalise()
	if s.Runtime.ModelDir != "/mnt/models" {
		t.Errorf("model dir = %q, want the trimmed path kept", s.Runtime.ModelDir)
	}

	// The GPU build survives, and the two points where the wait is absurd are
	// clamped rather than used as given.
	s = Default()
	s.Runtime = RuntimeSettings{
		Accelerator:  AccelVulkan,
		ReadySeconds: 2,
		Models: map[string]ManagedModel{
			"llamacpp": {ModelPath: "/models/a.gguf", CtxSize: 512},
		},
	}
	s.normalise()
	if s.Runtime.Accelerator != AccelVulkan {
		t.Errorf("accelerator = %q, want the stored %q kept", s.Runtime.Accelerator, AccelVulkan)
	}
	if s.Runtime.ReadySeconds < 15 {
		t.Errorf("ready seconds = %d, want the wait clamped up to something a model can load in", s.Runtime.ReadySeconds)
	}
	if got := s.Runtime.Models["llamacpp"].CtxSize; got != MinCtxSize {
		t.Errorf("ctx size = %d, want it clamped up to %d: below that a system prompt and a source cannot fit", got, MinCtxSize)
	}
	if s.Runtime.Models["llamacpp"].ModelPath != "/models/a.gguf" {
		t.Error("the model path was not kept")
	}

	// Every family the app knows survives normalise. A value that quietly became
	// the CPU build would mean a user who chose ROCm gets the CPU build on the next
	// load, with nothing on screen saying so.
	for _, accel := range Accelerators() {
		s = Default()
		s.Runtime = RuntimeSettings{Accelerator: accel}
		s.normalise()
		if s.Runtime.Accelerator != accel {
			t.Errorf("accelerator %q became %q", accel, s.Runtime.Accelerator)
		}
	}

	// An accelerator no release publishes must not reach a download URL.
	s = Default()
	s.Runtime = RuntimeSettings{Accelerator: "cuda-99", ReadySeconds: 99999}
	s.normalise()
	if s.Runtime.Accelerator != AccelCPU {
		t.Errorf("accelerator = %q, want the unknown value replaced", s.Runtime.Accelerator)
	}
	if s.Runtime.ReadySeconds > 1800 {
		t.Errorf("ready seconds = %d, want it clamped down", s.Runtime.ReadySeconds)
	}

	// Zero keeps its own meaning here: "let the server choose its window" is a
	// choice, not a missing value.
	s = Default()
	s.Runtime = RuntimeSettings{Models: map[string]ManagedModel{"llamacpp": {CtxSize: 0}}}
	s.normalise()
	if got := s.Runtime.Models["llamacpp"].CtxSize; got != 0 {
		t.Errorf("ctx size = %d, want 0 left alone as \"the server decides\"", got)
	}
}

// Flash attention and the KV cache type are optional additions to a managed
// model, so a settings blob written before they existed carries neither. An empty
// field has to mean the server's own default rather than a flag, and a value this
// build does not recognise must not reach `--flash-attn`/`--cache-type-k/-v`,
// where llama-server would reject it at startup. The GPU offload of the same test
// is the third of those fields and follows the same rule: the two explicit words
// are passed on, everything else means "as many layers as fit".
func TestRuntimeNormalisesFlashAttentionAndCacheTypes(t *testing.T) {
	// Absent fields keep their meaning: "auto" and f16, the server's own defaults.
	s := Default()
	s.Runtime = RuntimeSettings{Models: map[string]ManagedModel{"llamacpp": {ModelPath: "/a.gguf"}}}
	s.normalise()
	mm := s.Runtime.Models["llamacpp"]
	if mm.FlashAttention != FlashAuto {
		t.Errorf("flash attention = %q, want %q for an absent field", mm.FlashAttention, FlashAuto)
	}
	if mm.CacheTypeK != CacheTypeF16 || mm.CacheTypeV != CacheTypeF16 {
		t.Errorf("cache types = %q/%q, want f16/f16 for absent fields", mm.CacheTypeK, mm.CacheTypeV)
	}
	if mm.Offload != OffloadAuto {
		t.Errorf("offload = %q, want %q for an absent field", mm.Offload, OffloadAuto)
	}

	// Every value the picker can store survives normalise untouched — a choice
	// that quietly became the default would be the setting not working.
	for _, mode := range FlashAttentionModes() {
		s = Default()
		s.Runtime = RuntimeSettings{Models: map[string]ManagedModel{"llamacpp": {FlashAttention: mode}}}
		s.normalise()
		if got := s.Runtime.Models["llamacpp"].FlashAttention; got != mode {
			t.Errorf("flash attention %q became %q", mode, got)
		}
	}
	for _, ct := range CacheTypes() {
		s = Default()
		s.Runtime = RuntimeSettings{Models: map[string]ManagedModel{"llamacpp": {CacheTypeK: ct, CacheTypeV: ct}}}
		s.normalise()
		got := s.Runtime.Models["llamacpp"]
		if got.CacheTypeK != ct || got.CacheTypeV != ct {
			t.Errorf("cache type %q became %q/%q", ct, got.CacheTypeK, got.CacheTypeV)
		}
	}

	for _, off := range OffloadModes() {
		s = Default()
		s.Runtime = RuntimeSettings{Models: map[string]ManagedModel{"llamacpp": {Offload: off}}}
		s.normalise()
		if got := s.Runtime.Models["llamacpp"].Offload; got != off {
			t.Errorf("offload %q became %q", off, got)
		}
	}

	// Junk falls back to the default rather than being passed on.
	s = Default()
	s.Runtime = RuntimeSettings{Models: map[string]ManagedModel{"llamacpp": {
		FlashAttention: "maybe", CacheTypeK: "q3_k", CacheTypeV: "  ", Offload: "gpu",
	}}}
	s.normalise()
	mm = s.Runtime.Models["llamacpp"]
	if mm.FlashAttention != FlashAuto {
		t.Errorf("flash attention = %q, want an unknown value replaced by %q", mm.FlashAttention, FlashAuto)
	}
	if mm.CacheTypeK != CacheTypeF16 || mm.CacheTypeV != CacheTypeF16 {
		t.Errorf("cache types = %q/%q, want unknown values replaced by f16", mm.CacheTypeK, mm.CacheTypeV)
	}
	if mm.Offload != OffloadAuto {
		t.Errorf("offload = %q, want an unknown value replaced by %q", mm.Offload, OffloadAuto)
	}
}
