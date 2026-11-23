package config

import (
	"encoding/json"
	"math"
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

	// A blob written before the LLM-context option existed, and before a managed
	// model carried a projector.
	older := `{"language":"de","search":{"providerOrder":["brave","duckduckgo"],"resultCount":8,"cacheMinutes":60},"runtime":{"models":{"llamacpp":{"modelPath":"/models/a.gguf"}}}}`
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
	// The between-rounds check costs a summarisation inside an answer the user is
	// waiting for, so an install that never asked for it must not start folding
	// mid-turn after an upgrade.
	if got.Compact.MidTurnCheck {
		t.Error("an older settings row switched on the mid-turn check")
	}
	// A managed model saved before the projector field existed keeps an empty one, and
	// empty means "the projector beside the model" rather than "run text-only": a
	// multimodal GGUF nobody can pair by hand is the state this field exists to end, so
	// the migration has to be nothing at all.
	if projector := got.Runtime.Models["llamacpp"].MMProj; projector != "" {
		t.Errorf("mmproj = %q, want it empty so the file beside the model is used", projector)
	}
	if path := got.Runtime.Models["llamacpp"].ModelPath; path != "/models/a.gguf" {
		t.Errorf("modelPath = %q, want the stored model kept", path)
	}
}

// TestToolLoopDefaultsSuitACodingProject pins the round figures the two modes are
// measured against: a project's reply gets enough rounds to read, edit and run a check more
// than once — more than an ordinary chat, which ends when the model stops asking for tools.
// Both used to be one, sized for a research turn (6, then 24), which a two-file change
// exhausts before it has read the failure.
func TestToolLoopDefaultsSuitACodingProject(t *testing.T) {
	def := Default()
	if def.ToolIterations != 24 {
		t.Errorf("ToolIterations = %d, want 24", def.ToolIterations)
	}
	if def.CodingToolIterations != 60 {
		t.Errorf("CodingToolIterations = %d, want 60", def.CodingToolIterations)
	}
	// A project must not be the smaller of the two: the field it reads is the one that has
	// to survive a refactor, and a chat's figure was never sized for one.
	if def.CodingToolIterations <= def.ToolIterations {
		t.Errorf("CodingToolIterations = %d, want more than the chat's %d",
			def.CodingToolIterations, def.ToolIterations)
	}
	// The ceiling has to leave room above both defaults, or the default is the ceiling: a
	// refactor with a few more files than usual is a choice the user can make.
	if MaxToolIterations <= DefaultCodingToolIterations {
		t.Errorf("MaxToolIterations = %d, want more than the coding default %d",
			MaxToolIterations, DefaultCodingToolIterations)
	}
}

// TestToolLoopRepairKeepsAChoiceAndBoundsAHandEditedBlob pins the three readings of
// the stored number: zero is "nobody chose" and takes the default, a value above the
// ceiling is clamped like every other bounded field, and a value the user picked — the
// old default of 6 included — is left alone. Guessing that 6 meant "not chosen" would
// silently override somebody who deliberately set it.
func TestToolLoopRepairKeepsAChoiceAndBoundsAHandEditedBlob(t *testing.T) {
	unset := Settings{}
	unset.normalise()
	if unset.ToolIterations != DefaultToolIterations {
		t.Errorf("an unset ToolIterations became %d, want the default %d",
			unset.ToolIterations, DefaultToolIterations)
	}
	// A blob written before the project's field existed carries no coding figure, and that
	// absence is exactly what a project upgrading to it has: it must open on the default.
	if unset.CodingToolIterations != DefaultCodingToolIterations {
		t.Errorf("an unset CodingToolIterations became %d, want the default %d",
			unset.CodingToolIterations, DefaultCodingToolIterations)
	}

	huge := Settings{ToolIterations: 5000, CodingToolIterations: 5000}
	huge.normalise()
	if huge.ToolIterations != MaxToolIterations {
		t.Errorf("a stored 5000 became %d, want the %d ceiling", huge.ToolIterations, MaxToolIterations)
	}
	if huge.CodingToolIterations != MaxToolIterations {
		t.Errorf("a stored coding 5000 became %d, want the %d ceiling",
			huge.CodingToolIterations, MaxToolIterations)
	}

	chosen := Settings{ToolIterations: 6, CodingToolIterations: 12}
	chosen.normalise()
	if chosen.ToolIterations != 6 {
		t.Errorf("a stored 6 became %d, want it kept", chosen.ToolIterations)
	}
	if chosen.CodingToolIterations != 12 {
		t.Errorf("a stored coding 12 became %d, want it kept", chosen.CodingToolIterations)
	}
}

// TestMidTurnCheckShipsOffAndSurvivesARestart pins both halves of the switch: the
// shipped default (off, because it spends a request inside an answer somebody is waiting
// for) and the round trip through the one JSON row the settings blob is.
func TestMidTurnCheckShipsOffAndSurvivesARestart(t *testing.T) {
	if Default().Compact.MidTurnCheck {
		t.Error("the mid-turn check is on by default")
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := Default()
	s.Compact.MidTurnCheck = true
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSetting("settings", json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
	got, err := Load(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Compact.MidTurnCheck {
		t.Error("a stored mid-turn check did not survive the reload")
	}
}

// TestDigestShipsOffAndBelongsToTheBackend pins the three things that decide whether
// this feature can surprise anybody: the model ships unset, so nothing is condensed —
// and in particular never by the chat's own model, which is the one whose request the
// condensation exists to shrink — the threshold survives a restart, and the switch is
// carried by the backend row rather than by the shared settings.
func TestDigestShipsOffAndBelongsToTheBackend(t *testing.T) {
	if def := Default(); def.Digest.Model != (llm.Ref{}) {
		t.Errorf("digest model = %+v, want an empty ref so the feature is off by default",
			def.Digest.Model)
	} else if def.Digest.Threshold != 2000 {
		t.Errorf("digest threshold = %d, want the shipped 2000", def.Digest.Threshold)
	} else if def.Digest.MaxTokens != DefaultDigestMaxTokens {
		t.Errorf("digest cap = %d, want the shipped %d", def.Digest.MaxTokens, DefaultDigestMaxTokens)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := Default()
	s.Backends = []llm.BackendConfig{
		{ID: "groq", Name: "Groq", Kind: "groq", Enabled: true, Digest: true},
		{ID: "lmstudio", Name: "LM Studio", Kind: "lmstudio", Enabled: true},
	}
	s.Digest = DigestSettings{Model: llm.Ref{Backend: "ollama", Model: "qwen3:4b"}, Threshold: 4000,
		MaxTokens: 1200}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSetting("settings", json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}

	got, err := Load(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest.Model != s.Digest.Model || got.Digest.Threshold != 4000 {
		t.Errorf("digest = %+v, want the stored model and the 4000-token threshold", got.Digest)
	}
	if got.Digest.MaxTokens != 1200 {
		t.Errorf("digest cap = %d, want the stored 1200", got.Digest.MaxTokens)
	}
	byID := map[string]bool{}
	for _, b := range got.Backends {
		byID[b.ID] = b.Digest
	}
	if !byID["groq"] {
		t.Error("the backend that asked for condensation lost the switch")
	}
	if byID["lmstudio"] {
		t.Error("a backend that never asked for condensation acquired the switch")
	}
}

// TestTheDigestCapIsResolvedNotTrusted pins how the summary length is read: an
// emptied field is the default rather than a one-token summary, and the range is
// bounded.
//
// It also pins the independence from Threshold, which an earlier version got wrong: the
// threshold is the smallest page worth a request, not the size of the page in hand, so a
// cap above it is a legitimate choice — and capping it there silently rewrote the stored
// value, so a chosen 3000 came back as 2000 and raising the threshold later did not
// restore it.
func TestTheDigestCapIsResolvedNotTrusted(t *testing.T) {
	for _, tc := range []struct {
		name           string
		max, threshold int
		want           int
	}{
		{"unset", 0, 2000, DefaultDigestMaxTokens},
		{"emptied by a save", 0, 2000, DefaultDigestMaxTokens},
		{"below the floor", 20, 2000, MinDigestMaxTokens},
		{"above the ceiling", 50000, 60000, MaxDigestMaxTokens},
		{"above the threshold", 3000, 1500, 3000},
		{"a plain choice", 1200, 4000, 1200},
	} {
		d := DigestSettings{MaxTokens: tc.max, Threshold: tc.threshold}
		if got := d.DigestMaxTokens(); got != tc.want {
			t.Errorf("%s: cap %d with threshold %d = %d, want %d",
				tc.name, tc.max, tc.threshold, got, tc.want)
		}
	}
	// A stored cap above the threshold survives a load. This is the data-loss half of
	// the same defect: normalise used to write the capped value back.
	s := Default()
	s.Digest.Threshold = 1500
	s.Digest.MaxTokens = 3000
	s.normalise()
	if s.Digest.MaxTokens != 3000 {
		t.Errorf("normalise rewrote the chosen cap to %d; the threshold is not its ceiling",
			s.Digest.MaxTokens)
	}
	// And an out-of-range value is stored bounded, so the panel shows a number that
	// can be sent rather than one the engine silently ignores.
	s = Default()
	s.Digest.MaxTokens = 9000
	s.normalise()
	if s.Digest.MaxTokens != MaxDigestMaxTokens {
		t.Errorf("normalise kept %d, want it bounded to %d", s.Digest.MaxTokens, MaxDigestMaxTokens)
	}
}

// TestNormaliseKeepsTheDigestSettingsUsable covers a hand-edited or older blob: a
// threshold outside the range where the feature can pay for itself falls back to the
// default, and a half-filled model counts as unset — the same rule the summarisation
// model follows, because a half-filled ref looks configured and then fails every
// oversized result.
func TestNormaliseKeepsTheDigestSettingsUsable(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, 2000}, {-5, 2000}, {200, 200}, {500000, 100000}} {
		s := Default()
		s.Digest.Threshold = tc.in
		s.normalise()
		if s.Digest.Threshold != tc.want {
			t.Errorf("normalise(threshold %d) = %d, want %d", tc.in, s.Digest.Threshold, tc.want)
		}
	}
	for _, ref := range []llm.Ref{{Backend: "ollama"}, {Model: "qwen3:4b"}} {
		s := Default()
		s.Digest.Model = ref
		s.normalise()
		if s.Digest.Model != (llm.Ref{}) {
			t.Errorf("normalise(%+v) kept a half-filled model; every oversized page would fail", ref)
		}
	}
}

// TestTheDefaultModelShipsUnsetAndSurvivesAReload pins both halves of the setting:
// an installation that predates it keeps choosing "whatever is available", and a
// chosen model is still there after a restart rather than resetting to automatic.
//
// The empty value is deliberately a real answer here, not a missing one — it is the
// behaviour every earlier version had, and the only way back to it.
func TestTheDefaultModelShipsUnsetAndSurvivesAReload(t *testing.T) {
	if def := Default(); def.DefaultModel != (llm.Ref{}) {
		t.Errorf("default model = %+v, want an empty ref so a new chat picks the first available",
			def.DefaultModel)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := Default()
	s.DefaultModel = llm.Ref{Backend: "ollama", Model: "qwen3:4b"}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSetting("settings", json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}

	got, err := Load(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultModel != s.DefaultModel {
		t.Errorf("default model after a reload = %+v, want the stored %+v", got.DefaultModel, s.DefaultModel)
	}
}

// TestNormaliseClearsAHalfFilledDefaultModel covers a hand-edited blob: a pair with
// one half missing would look chosen and then hand every new chat a backend it is
// not on, so it counts as unset instead.
func TestNormaliseClearsAHalfFilledDefaultModel(t *testing.T) {
	for _, ref := range []llm.Ref{{Backend: "ollama"}, {Model: "qwen3:4b"}} {
		s := Default()
		s.DefaultModel = ref
		s.normalise()
		if s.DefaultModel != (llm.Ref{}) {
			t.Errorf("normalise(%+v) kept a half-filled default model; every new chat would start on it", ref)
		}
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
		if !strings.Contains(small, "{{datetime}}") {
			t.Errorf("the small-model preset for %q does not reference {{datetime}}", lang)
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
	for _, id := range []string{PresetResearch, PresetSmallLocal, PresetAssistant, PresetCoding, PresetCodingAgent} {
		bodies := map[string]string{}
		for _, lang := range shippedLanguages {
			body := PromptBody(id, lang)
			if body == "" {
				t.Fatalf("preset %q has no %q body", id, lang)
			}
			// Every body carries the date, and none of them names the tool list: a
			// request advertises its tools itself — the declared schemas on a native
			// backend, the generated hint on the text protocol — so a line repeating
			// that list is a second copy that can only drift from the first.
			if !strings.Contains(body, "{{datetime}}") {
				t.Errorf("the %q body of %q does not reference {{datetime}}", lang, id)
			}
			if strings.Contains(body, "{{tool_list}}") {
				t.Errorf("the %q body of %q repeats the tool list the request already advertises", lang, id)
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

	// An edited default prompt opens a chat in whatever language it was written in, so it moves
	// into a named prompt of its own and the field returns to the shipped body. The switch must
	// not translate the text on its way there, and it must not drop it either.
	byID := map[string]string{}
	for _, p := range s.PromptPresets {
		byID[p.ID] = p.Prompt
	}
	if got := s.DefaultSystemPrompt; got != PromptBody(PresetResearch, LangEnglish) {
		t.Errorf("the edited default prompt was not moved out of the field: %q", got)
	}
	if got := byID[PresetCustom]; got != "mein eigener Standard-Prompt" {
		t.Errorf("the edited default prompt was translated or lost: %q", got)
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

// TestACorrectedPromptStillCountsAsUnedited is why retiredBodies exists, and the half of
// the rule that a correction can silently break. The shipped bodies are the definition of
// "unedited", so a body this app corrects stops matching every stored prompt that still
// holds the old text — and those installations read as though the user had edited their
// prompt, keeping it in whatever language it was saved in for good.
//
// The retired bytes therefore have to be recognised, and the prompt that used them has to
// move to the current body like any untouched one.
func TestACorrectedPromptStillCountsAsUnedited(t *testing.T) {
	// Every preset that has a correction recorded, not only the one this test's second
	// half walks: a missing language or a forgotten entry is exactly the mistake the map
	// invites, and it would only show up as a frozen prompt in the field.
	covered := 0
	for _, id := range []string{PresetResearch, PresetSmallLocal, PresetAssistant, PresetCoding, PresetCodingAgent} {
		for _, lang := range shippedLanguages {
			for _, old := range retiredBodies[id][lang] {
				covered++
				if old == PromptBody(id, lang) {
					t.Fatalf("the retired %q body of %q is also the current one", lang, id)
				}
				if !isShippedPrompt(id, old) {
					t.Errorf("a %q body of %q that this app shipped is no longer recognised as unedited", lang, id)
				}
			}
		}
	}
	if covered == 0 {
		t.Fatal("no retired body is recorded; this test would prove nothing")
	}

	// An installation that saved the old German text and then switched the interface to
	// English: the default prompt and the preset both move, instead of freezing.
	old := retiredBodies[PresetResearch][LangGerman][0]
	s := Default()
	s.Language = LangGerman
	s.normalise()
	s.DefaultSystemPrompt = old
	for i := range s.PromptPresets {
		if s.PromptPresets[i].ID == PresetResearch {
			s.PromptPresets[i].Prompt = old
		}
	}

	s.Language = LangEnglish
	s.LocalisePrompts()
	if want := PromptBody(PresetResearch, LangEnglish); s.DefaultSystemPrompt != want {
		t.Error("a retired default prompt was left in its old language after the switch")
	}
	for _, p := range s.PromptPresets {
		if p.ID != PresetResearch {
			continue
		}
		if want := PromptBody(PresetResearch, LangEnglish); p.Prompt != want {
			t.Error("a retired preset was left in its old language after the switch")
		}
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

	// NVIDIA arrives the same way — the third hosted backend shipped after this
	// test was written — and is off for the same reason.
	nvidia, ok := byID["nvidia"]
	if !ok {
		t.Fatalf("NVIDIA did not reach existing settings: %+v", s.Backends)
	}
	if nvidia.Kind != "nvidia" || nvidia.BaseURL != llm.NvidiaDefaultBaseURL {
		t.Errorf("added entry = %+v, want the shipped NVIDIA config", nvidia)
	}
	if nvidia.Enabled {
		t.Error("the repaired NVIDIA entry is enabled")
	}

	// Groq arrives the same way — the fourth hosted backend shipped after this
	// test was written, and the one whose address is not a bare host plus /v1: the
	// repair has to land on `/openai/v1`, or the settings field would show an
	// address no route of that API answers on.
	groq, ok := byID["groq"]
	if !ok {
		t.Fatalf("Groq did not reach existing settings: %+v", s.Backends)
	}
	if groq.Kind != "groq" || groq.BaseURL != llm.GroqDefaultBaseURL {
		t.Errorf("added entry = %+v, want the shipped Groq config", groq)
	}
	if groq.Enabled {
		t.Error("the repaired Groq entry is enabled")
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
// by hand, or a partly written blob, looks like. Every shipped kind with a
// documented address is repaired, and a kind without one is left alone so no
// address is invented for it.
func TestNormaliseRestoresAShippedBackendsAddress(t *testing.T) {
	s := Default()
	s.Backends = []llm.BackendConfig{
		{ID: "llamacpp", Name: "llama.cpp", Kind: "llamacpp", Enabled: true},
		{ID: "deepseek", Name: "DeepSeek", Kind: "deepseek", Enabled: true},
		{ID: "mistral", Name: "Mistral", Kind: "mistral", Enabled: true},
		{ID: "nvidia", Name: "NVIDIA", Kind: "nvidia", Enabled: true},
		{ID: "groq", Name: "Groq", Kind: "groq", Enabled: true},
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
	// Same reason as Mistral's: /v1 is part of the address, and repairing an
	// empty field to the bare host would name one no route of that API answers on.
	if got["nvidia"] != llm.NvidiaDefaultBaseURL {
		t.Errorf("nvidia address = %q, want %q", got["nvidia"], llm.NvidiaDefaultBaseURL)
	}
	// The strongest case for that reason: a bare host completed with the usual
	// `/v1` would be right for every other kind here and wrong for this one, so
	// the repaired field is the whole documented path.
	if got["groq"] != llm.GroqDefaultBaseURL {
		t.Errorf("groq address = %q, want %q", got["groq"], llm.GroqDefaultBaseURL)
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

// The folders the model row remembers are thinned rather than trusted: a blob can
// carry a duplicate — the same folder picked twice — a blank, or a list that grew
// once per folder ever picked, and the row draws one chip per entry.
//
// The order is part of the answer, because the chips *are* the list: what is pinned
// here is that a stored list comes back in the order it was stored, and that the
// folder in use joins it at the head when it is missing. Reordering instead would
// move a chip the user is about to click, and dropping the folder in use would lose
// the one thing the row had before there were several.
func TestRememberedModelFoldersAreThinnedAndBounded(t *testing.T) {
	joined := func(dirs []string) string { return strings.Join(dirs, "|") }

	// Blanks and duplicates go, and the rest keeps the order it arrived in.
	s := Default()
	s.Runtime = RuntimeSettings{ModelDir: "/mnt/b", ModelDirs: []string{" /mnt/a ", "/mnt/b", "/mnt/a", "", "/mnt/c"}}
	s.normalise()
	if got := joined(s.Runtime.ModelDirs); got != "/mnt/a|/mnt/b|/mnt/c" {
		t.Errorf("remembered folders = %q, want the trimmed list without duplicates and in order", got)
	}

	// A folder the list does not have — a path typed into the text box, or a blob
	// written before this list existed — is added at the head, where the folder just
	// named belongs.
	s = Default()
	s.Runtime = RuntimeSettings{ModelDir: "/mnt/b", ModelDirs: []string{"/mnt/a"}}
	s.normalise()
	if got := joined(s.Runtime.ModelDirs); got != "/mnt/b|/mnt/a" {
		t.Errorf("remembered folders = %q, want the folder in use first", got)
	}

	// A folder that is already remembered does not move: picking the one at the end
	// of the row must not shuffle the row.
	s = Default()
	s.Runtime = RuntimeSettings{ModelDir: "/mnt/c", ModelDirs: []string{"/mnt/a", "/mnt/b", "/mnt/c"}}
	s.normalise()
	if got := joined(s.Runtime.ModelDirs); got != "/mnt/a|/mnt/b|/mnt/c" {
		t.Errorf("remembered folders = %q, want the stored order kept", got)
	}

	// The list is bounded, and it is the oldest that falls off the end.
	s = Default()
	if len(s.Runtime.ModelDirs) != 0 {
		t.Fatalf("a default blob carries %d remembered folders, want none", len(s.Runtime.ModelDirs))
	}
	dirs := make([]string, 0, MaxRememberedModelDirs+3)
	for i := 0; i < MaxRememberedModelDirs+3; i++ {
		dirs = append(dirs, "/mnt/"+string(rune('a'+i)))
	}
	s = Default()
	s.Runtime = RuntimeSettings{ModelDir: dirs[0], ModelDirs: dirs}
	s.normalise()
	if got, want := joined(s.Runtime.ModelDirs), joined(dirs[:MaxRememberedModelDirs]); got != want {
		t.Errorf("remembered folders = %q, want %q", got, want)
	}

	// A folder in use that sits past the limit is kept anyway, at the head, because
	// the panel reads it back: a row that forgot which folder it is listing would
	// fall back to LM Studio's directory and show the wrong models under the right
	// name.
	s = Default()
	s.Runtime = RuntimeSettings{ModelDir: dirs[len(dirs)-1], ModelDirs: dirs}
	s.normalise()
	if len(s.Runtime.ModelDirs) != MaxRememberedModelDirs {
		t.Fatalf("remembered folders = %d, want the list bounded to %d", len(s.Runtime.ModelDirs), MaxRememberedModelDirs)
	}
	if s.Runtime.ModelDirs[0] != dirs[len(dirs)-1] {
		t.Errorf("first remembered folder = %q, want the folder in use", s.Runtime.ModelDirs[0])
	}
}

// Flash attention and the KV cache type are optional additions to a managed
// model, so a settings blob written before they existed carries neither. An empty
// field has to mean the server's own default rather than a flag, and a value this
// build does not recognise must not reach `--flash-attn`/`--cache-type-k/-v`,
// where llama-server would reject it at startup. The GPU offload of the same test
// is the third of those fields and follows the same rule: the two explicit words
// are passed on, everything else means "as many layers as fit".
// One slot is the default, and it is stored as the absence of the parallel opt-in — so a
// blob written before the field existed arrives on one slot, which is the whole migration.
// A parallel choice that was made has to survive the save like any other answer.
func TestOneSlotIsTheDefaultAndParallelIsRemembered(t *testing.T) {
	s := Default()
	if err := json.Unmarshal([]byte(`{"runtime":{"models":{"llamacpp":{"modelPath":"/a.gguf"}}}}`), &s); err != nil {
		t.Fatalf("unmarshal an older blob: %v", err)
	}
	s.normalise()
	if s.Runtime.Models["llamacpp"].Parallel {
		t.Error("an older blob arrived opting into parallel sessions")
	}

	stored := Default()
	stored.Runtime = RuntimeSettings{Models: map[string]ManagedModel{
		"llamacpp": {ModelPath: "/a.gguf", Parallel: true},
	}}
	blob, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	again := Default()
	if err := json.Unmarshal(blob, &again); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	again.normalise()
	if !again.Runtime.Models["llamacpp"].Parallel {
		t.Error("the stored parallel choice came back off")
	}
}

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

	// The expert modes survive with their count, including the count 0 that means
	// "none of the experts leaves the device" — keyed by the file the entry names,
	// because the placement belongs to the file.
	for _, mode := range ExpertOffloadModes() {
		s = Default()
		s.Runtime = RuntimeSettings{Models: map[string]ManagedModel{"llamacpp": {
			ModelPath: "/models/a.gguf", ExpertOffload: mode, ExpertLayers: 13,
		}}}
		s.normalise()
		got := s.Runtime.Models["llamacpp"]
		choice := got.ExpertFor("/models/a.gguf")
		if mode == ExpertOffloadAuto {
			if choice.Offload != ExpertOffloadAuto || len(got.Experts) != 0 {
				t.Errorf("auto became %q/%d with %d stored choices, want none",
					choice.Offload, choice.Layers, len(got.Experts))
			}
			continue
		}
		if choice.Offload != mode || choice.Layers != 13 {
			t.Errorf("expert offload %q/13 became %q/%d", mode, choice.Offload, choice.Layers)
		}
	}

	// Junk falls back to the default rather than being passed on.
	s = Default()
	s.Runtime = RuntimeSettings{Models: map[string]ManagedModel{"llamacpp": {
		FlashAttention: "maybe", CacheTypeK: "q3_k", CacheTypeV: "  ", Offload: "gpu",
		ExpertOffload: "experts", ExpertLayers: -4,
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
	if mm.ExpertOffload != ExpertOffloadAuto {
		t.Errorf("expert offload = %q, want an unknown value replaced by %q", mm.ExpertOffload, ExpertOffloadAuto)
	}
	if mm.ExpertLayers != 0 {
		t.Errorf("expert layers = %d, want a negative count clamped to 0", mm.ExpertLayers)
	}
}

// TestExpertChoiceStaysWithItsFile pins why the placement is keyed by path: the chat's
// model picker moves only the path, so a count chosen for one mixture must not become
// the next file's — and a file nobody tuned keeps llama.cpp's own placement.
func TestExpertChoiceStaysWithItsFile(t *testing.T) {
	m := ManagedModel{
		ModelPath: "/models/a.gguf",
		Experts:   map[string]ExpertChoice{"/models/a.gguf": {Offload: ExpertOffloadLayers, Layers: 13}},
	}
	if got := m.ExpertFor("/models/b.gguf"); got.Offload != ExpertOffloadAuto || got.Layers != 0 {
		t.Errorf("another file got %q/%d, want the default placement", got.Offload, got.Layers)
	}
	if got := m.ExpertFor("/models/a.gguf"); got.Offload != ExpertOffloadLayers || got.Layers != 13 {
		t.Errorf("the file's own choice became %q/%d", got.Offload, got.Layers)
	}

	// A negative count is not a count and an unknown mode is not a mode: both are
	// repaired here, where every other out-of-range value is, so the map holds only
	// choices a server can be given. A literal auto is kept without a count beside it,
	// because the panel fills an undecided file with the count it suggests — so
	// "automatic" has to stay distinguishable from "nobody has looked at this yet".
	s := Default()
	s.Runtime = RuntimeSettings{Models: map[string]ManagedModel{"llamacpp": {
		ModelPath: "/models/a.gguf",
		Experts: map[string]ExpertChoice{
			"/models/a.gguf": {Offload: ExpertOffloadLayers, Layers: -4},
			"/models/b.gguf": {Offload: ExpertOffloadAuto, Layers: 7},
			"/models/c.gguf": {Offload: "experts", Layers: 2},
		},
	}}}
	s.normalise()
	got := s.Runtime.Models["llamacpp"]
	if choice := got.ExpertFor("/models/a.gguf"); choice.Offload != ExpertOffloadLayers || choice.Layers != 0 {
		t.Errorf("a negative count became %q/%d, want %q/0", choice.Offload, choice.Layers, ExpertOffloadLayers)
	}
	if choice := got.ExpertFor("/models/b.gguf"); choice.Offload != ExpertOffloadAuto || choice.Layers != 0 {
		t.Errorf("a stored auto became %q/%d, want auto with no count", choice.Offload, choice.Layers)
	}
	if _, ok := got.Experts["/models/c.gguf"]; ok {
		t.Error("an unknown mode was stored, want it dropped as auto")
	}
}

// The MTP count is per file for the same reason as the expert placement, and its zero is
// a real value: it is the stored "off", which is the only way to say that about a file
// whose header has the heads, since an absent path follows the recommendation. A
// negative count and a blank path drop out, and a count past the maximum is clamped
// rather than thrown away.
func TestMTPCountStaysWithItsFile(t *testing.T) {
	s := Default()
	s.Runtime = RuntimeSettings{Models: map[string]ManagedModel{"llamacpp": {
		ModelPath: "/models/a.gguf",
		MTP: map[string]int{
			"/models/a.gguf": 4,
			"/models/b.gguf": 0,
			"/models/c.gguf": -3,
			"/models/d.gguf": 99,
			"  ":             2,
		},
	}}}
	s.normalise()
	got := s.Runtime.Models["llamacpp"]
	if got.MTP["/models/a.gguf"] != 4 {
		t.Errorf("the chosen count became %d, want 4", got.MTP["/models/a.gguf"])
	}
	if drafts, ok := got.MTP["/models/b.gguf"]; !ok || drafts != 0 {
		t.Errorf("the stored off for /models/b.gguf became %d (present=%v), want 0", drafts, ok)
	}
	for _, path := range []string{"/models/c.gguf", "  "} {
		if _, ok := got.MTP[path]; ok {
			t.Errorf("%q kept an entry, want it dropped", path)
		}
	}
	if got.MTP["/models/d.gguf"] != MaxMTPDrafts {
		t.Errorf("an oversized count became %d, want it clamped to %d", got.MTP["/models/d.gguf"], MaxMTPDrafts)
	}
}

// The confidence floor is stored the same way and for the same reason, but it is a
// probability: 0 is a deliberate "no floor" and is kept, 1 passes no draft at all and a
// negative value is not a probability, so those two drop out. A number in between is
// kept exactly as entered — the flag takes a probability, not a shape.
func TestMTPConfidenceIsAPerFileFloor(t *testing.T) {
	s := Default()
	s.Runtime = RuntimeSettings{Models: map[string]ManagedModel{"llamacpp": {
		ModelPath: "/models/a.gguf",
		MTPConfidence: map[string]float64{
			"/models/a.gguf": 0.75,
			"/models/b.gguf": 0,
			"/models/c.gguf": 1,
			"/models/d.gguf": -0.2,
			"  ":             0.5,
		},
	}}}
	s.normalise()
	got := s.Runtime.Models["llamacpp"]
	if got.MTPConfidence["/models/a.gguf"] != 0.75 {
		t.Errorf("the chosen floor became %v, want 0.75", got.MTPConfidence["/models/a.gguf"])
	}
	if floor, ok := got.MTPConfidence["/models/b.gguf"]; !ok || floor != 0 {
		t.Errorf("the stored \"no floor\" for /models/b.gguf became %v (present=%v), want 0", floor, ok)
	}
	for _, path := range []string{"/models/c.gguf", "/models/d.gguf", "  "} {
		if _, ok := got.MTPConfidence[path]; ok {
			t.Errorf("%q kept a floor that cannot change anything, want it dropped", path)
		}
	}
}

// TestDefaultLeavesImageGenerationOff pins the decision the whole feature rests
// on: a fresh install contacts no image server. A WebUI is a several-gigabyte
// install of its own, so probing an address for a feature nobody has asked for
// would report a failure before the user has done anything — the same reason the
// metered chat backends ship disabled.
// TestAutoApproveShipsOffAndStaysOffWhenAbsent pins the two switches that turn the
// confirmation gate off. Both are off in Default() for the reason the hosted backends are:
// an installation whose first surprise is a file it did not expect is the wrong first
// impression. And a blob written before the group existed has to arrive at exactly that —
// an absent boolean read as "yes" would be a permission nobody granted.
func TestAutoApproveShipsOffAndStaysOffWhenAbsent(t *testing.T) {
	if s := Default(); s.AutoApprove.Files || s.AutoApprove.Commands {
		t.Fatalf("auto-approval is on in Default(): %+v", s.AutoApprove)
	}

	// The decode is where an absent field could have arrived as something else.
	older := Default()
	if err := json.Unmarshal([]byte(`{"language":"en","toolIterations":6}`), &older); err != nil {
		t.Fatalf("unmarshal an older blob: %v", err)
	}
	if older.AutoApprove.Files || older.AutoApprove.Commands {
		t.Errorf("an older blob arrived with a permission nobody gave: %+v", older.AutoApprove)
	}

	// And a yes the user gave once has to survive a save and a restart, because that is what
	// a standing answer means. The other half stays off: the two are separate questions.
	stored := Default()
	stored.AutoApprove = AutoApproveSettings{Files: true}
	blob, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	again := Default()
	if err := json.Unmarshal(blob, &again); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !again.AutoApprove.Files || again.AutoApprove.Commands {
		t.Errorf("the stored answer came back as %+v, want files allowed only", again.AutoApprove)
	}
}

// The daily update check ships on — the deliberate exception to "a switch costing a
// request ships off" — and an explicit no stays a no. The default being the migration is
// what carries the field to installations that predate it, so the case this pins is the
// one that matters: a person who turned it off must not be switched back on by a restart.
func TestTheDailyUpdateCheckShipsOnAndRemembersANo(t *testing.T) {
	if s := Default(); !s.Updates.CheckDaily || s.Updates.LastCheckUnix != 0 {
		t.Fatalf("Default() = %+v, want the daily check on and never run", s.Updates)
	}

	// An older blob has no `updates` object at all; it arrives on, which is the intended
	// price of the default being the whole migration story.
	older := Default()
	if err := json.Unmarshal([]byte(`{"language":"en","toolIterations":6}`), &older); err != nil {
		t.Fatalf("unmarshal an older blob: %v", err)
	}
	if !older.Updates.CheckDaily {
		t.Errorf("an older blob arrived with the check off although that is now the default: %+v", older.Updates)
	}

	// The no the user gave is stored, and survives into the next process.
	stored := Default()
	stored.Updates = UpdateSettings{CheckDaily: false, LastCheckUnix: 1762700000}
	blob, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	again := Default()
	if err := json.Unmarshal(blob, &again); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if again.Updates.CheckDaily || again.Updates.LastCheckUnix != 1762700000 {
		t.Errorf("the stored switch came back as %+v", again.Updates)
	}
}

func TestDefaultLeavesImageGenerationOff(t *testing.T) {
	s := Default()
	if s.Images.Enabled {
		t.Error("image generation is enabled on a fresh install")
	}
	if s.Images.BaseURL != DefaultImageBaseURL {
		t.Errorf("baseUrl = %q, want the offered default %q", s.Images.BaseURL, DefaultImageBaseURL)
	}
	// Every generation field stays empty, and that is load-bearing rather than
	// untidy: an empty value is omitted from the request, which is what leaves a
	// sampler, a step count, a size and a negative prompt configured in the user's
	// own WebUI in force instead of being replaced by this app's guesses.
	if s.Images.Width != 0 || s.Images.Height != 0 || s.Images.Steps != 0 ||
		s.Images.Sampler != "" || s.Images.Model != "" || s.Images.NegativePrompt != "" {
		t.Errorf("a generation field carries a value the server should have chosen: %+v", s.Images)
	}
}

// TestNormaliseClampsImageFields covers the bound a hand-edited blob — or a model
// passing a number it invented — runs into. A WebUI does not refuse a
// 20000-pixel request, it attempts it: minutes of work and tens of gigabytes.
func TestNormaliseClampsImageFields(t *testing.T) {
	cases := []struct {
		name          string
		width, height int
		steps         int
		wantW, wantH  int
		wantSteps     int
		cfg           float64
		wantCfg       float64
	}{
		{"unset stays unset", 0, 0, 0, 0, 0, 0, 0, 0},
		{"a real size survives", 768, 512, 25, 768, 512, 25, 0, 0},
		{"below the minimum is raised", 16, 16, 0, MinImageDimension, MinImageDimension, 0, 0, 0},
		{"above the maximum is capped", 99999, 4096, 9999, MaxImageDimension, MaxImageDimension, MaxImageSteps, 0, 0},
		{"a negative size is 'server default', not 'invalid'", -10, -10, -5, 0, 0, 0, 0, 0},
		// The guidance scale, the one float among these. 7.5 survives because a fractional
		// scale is a real request; the floor and ceiling are the app's own range, and zero
		// stays zero because that is the model's turn rather than the smallest scale.
		{"a fractional guidance scale survives", 0, 0, 0, 0, 0, 0, 7.5, 7.5},
		{"a guidance scale below the floor is raised", 0, 0, 0, 0, 0, 0, 0.2, MinImageCfgScale},
		{"a guidance scale above the ceiling is capped", 0, 0, 0, 0, 0, 0, 250, MaxImageCfgScale},
		{"an unset guidance scale is not a one", 0, 0, 0, 0, 0, 0, 0, 0},
		{"a negative guidance scale is the same statement", 0, 0, 0, 0, 0, 0, -3, 0},
		// Reachable rather than theoretical: the field arrives from a hand-edited blob as
		// readily as from the panel, and a NaN that survived into a payload would fail the
		// marshalling of the whole record.
		{"not a number folds to unset", 0, 0, 0, 0, 0, 0, math.NaN(), 0},
	}
	for _, tc := range cases {
		s := Default()
		s.Images.Width, s.Images.Height, s.Images.Steps = tc.width, tc.height, tc.steps
		s.Images.CfgScale = tc.cfg
		s.normalise()
		if s.Images.Width != tc.wantW || s.Images.Height != tc.wantH {
			t.Errorf("%s: %dx%d normalised to %dx%d, want %dx%d",
				tc.name, tc.width, tc.height, s.Images.Width, s.Images.Height, tc.wantW, tc.wantH)
		}
		if s.Images.Steps != tc.wantSteps {
			t.Errorf("%s: steps %d normalised to %d, want %d",
				tc.name, tc.steps, s.Images.Steps, tc.wantSteps)
		}
		if s.Images.CfgScale != tc.wantCfg {
			t.Errorf("%s: cfg scale %v normalised to %v, want %v",
				tc.name, tc.cfg, s.Images.CfgScale, tc.wantCfg)
		}
	}
}

// TestNormaliseRepairsTheSecondPassOnlyWhileItIsOn pins the one difference between the
// second pass's numbers and every other field: they are repaired while the switch is on and
// left exactly as stored while it is off. Flipping the switch back has to return the numbers
// the user chose, and a disabled pass must not quietly acquire defaults it would then send.
func TestNormaliseRepairsTheSecondPassOnlyWhileItIsOn(t *testing.T) {
	cases := []struct {
		name           string
		enabled        bool
		scale, denoise float64
		wantScale      float64
		wantDenoise    float64
	}{
		{"disabled keeps a surprising number", false, 4, 0.9, 4, 0.9},
		{"disabled keeps zero", false, 0, 0, 0, 0},
		{"enabled with nothing chosen gets the defaults", true, 0, 0, DefaultImageHiresScale, DefaultImageHiresDenoise},
		{"a real pair survives", true, 1.4, 0.35, 1.4, 0.35},
		{"below the floors is raised", true, 1.01, 0.05, MinImageHiresScale, MinImageHiresDenoise},
		{"above the ceilings is capped", true, 9, 0.99, MaxImageHiresScale, MaxImageHiresDenoise},
		{"negative is 'nobody chose one'", true, -1, -1, DefaultImageHiresScale, DefaultImageHiresDenoise},
		{"not a number is 'nobody chose one'", true, math.NaN(), math.NaN(), DefaultImageHiresScale, DefaultImageHiresDenoise},
	}
	for _, tc := range cases {
		s := Default()
		s.Images.HiresEnabled = tc.enabled
		s.Images.HiresScale, s.Images.HiresDenoise = tc.scale, tc.denoise
		s.normalise()
		if s.Images.HiresScale != tc.wantScale || s.Images.HiresDenoise != tc.wantDenoise {
			t.Errorf("%s: (%v, %v) normalised to (%v, %v), want (%v, %v)", tc.name,
				tc.scale, tc.denoise, s.Images.HiresScale, s.Images.HiresDenoise,
				tc.wantScale, tc.wantDenoise)
		}
	}
}

// TestNormaliseClampsTheVaeTileSize keeps the tile side under the same rule as the two
// picture sizes: zero is the build's own (not a size to raise), and a number a card cannot
// hold is clamped rather than handed to the program. The floor matters because a tile
// smaller than the decoder's own block is more pieces than pixels.
func TestNormaliseClampsTheVaeTileSize(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero is the build's own", 0, 0},
		{"a real tile survives", 512, 512},
		{"below the floor is raised", 8, MinImageDimension},
		{"above the ceiling is capped", 9000, MaxImageDimension},
		{"negative is the build's own", -4, 0},
	}
	for _, tc := range cases {
		s := Default()
		s.Images.CLIVaeTileSize = tc.in
		s.normalise()
		if s.Images.CLIVaeTileSize != tc.want {
			t.Errorf("%s: tile %d normalised to %d, want %d", tc.name, tc.in, s.Images.CLIVaeTileSize, tc.want)
		}
	}
}

// TestNormaliseFoldsAnUnknownCaptionSwitch pins the same rule the image kind follows: a
// value this app does not know means the behaviour that used to exist — nothing — rather
// than a feature nobody asked for, and the two spellings it does know survive untouched.
func TestNormaliseFoldsAnUnknownCaptionSwitch(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty follows the pipeline", "", ""},
		{"off survives", ImageCaptioningOff, ImageCaptioningOff},
		{"auto survives", ImageCaptioningAuto, ImageCaptioningAuto},
		{"model survives", ImageCaptioningModel, ImageCaptioningModel},
		{"a typo follows the pipeline", "atou", ""},
		{"an old boolean follows the pipeline", "true", ""},
		{"surrounding space is trimmed", "  auto  ", ImageCaptioningAuto},
	}
	for _, tc := range cases {
		s := Default()
		s.Images.Captioning = tc.in
		s.normalise()
		if s.Images.Captioning != tc.want {
			t.Errorf("%s: %q normalised to %q, want %q", tc.name, tc.in, s.Images.Captioning, tc.want)
		}
	}
}

// TestNormaliseFoldsAnUnknownPipeline is the same rule again: the file name is inferred
// when the value means nothing, which is the behaviour a typo would have had anyway.
func TestNormaliseFoldsAnUnknownPipeline(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty infers", "", ""},
		{"generic survives", ImagePipelineGeneric, ImagePipelineGeneric},
		{"klein survives", ImagePipelineFlux3Klein, ImagePipelineFlux3Klein},
		{"klein base survives", ImagePipelineFlux3KleinBase, ImagePipelineFlux3KleinBase},
		{"a typo infers", "flux3", ""},
		{"surrounding space is trimmed", "  generic  ", ImagePipelineGeneric},
	}
	for _, tc := range cases {
		s := Default()
		s.Images.Pipeline = tc.in
		s.normalise()
		if s.Images.Pipeline != tc.want {
			t.Errorf("%s: %q normalised to %q, want %q", tc.name, tc.in, s.Images.Pipeline, tc.want)
		}
	}
}

// TestNormaliseRestoresTheImageAddress keeps an emptied field from hiding the
// address the app would really use, exactly as the shipped backends do.
func TestNormaliseRestoresTheImageAddress(t *testing.T) {
	s := Default()
	s.Images.BaseURL = "   "
	s.normalise()
	if s.Images.BaseURL != DefaultImageBaseURL {
		t.Errorf("baseUrl = %q, want %q", s.Images.BaseURL, DefaultImageBaseURL)
	}
}

// TestAnOlderSettingsBlobKeepsDrawingThroughTheServer is the no-migration rule applied to
// the field this change added. A row written before the image backend could be anything but
// an HTTP server carries no `kind` at all, and it has to decide exactly as it did before:
// the server route. The check is on the *decode*, which is where an absent field could have
// arrived as something that selects the wrong backend.
func TestAnOlderSettingsBlobKeepsDrawingThroughTheServer(t *testing.T) {
	raw := []byte(`{"images":{"enabled":true,"baseUrl":"http://127.0.0.1:7860",` +
		`"model":"sdxl.safetensors","steps":25}}`)
	// Decoded whole, exactly as app.go decodes it: over a fully populated default, so an
	// absent field keeps what the default had.
	s := Default()
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal an older blob: %v", err)
	}
	if IsImageCLI(s.Images.Kind) {
		t.Errorf("kind %q selects the CLI backend on a row that never chose one", s.Images.Kind)
	}
	// And the fields it did carry survived the decode, which is what makes this a test of the
	// new field rather than of the decoder.
	if !s.Images.Enabled || s.Images.BaseURL != "http://127.0.0.1:7860" || s.Images.Steps != 25 {
		t.Errorf("the older blob's own fields did not survive: %+v", s.Images)
	}
	if s.Images.CLIBinary != "" {
		t.Errorf("clibinary = %q, want empty on a row that never named one", s.Images.CLIBinary)
	}
	// A mode nobody chose must be off. It is checked on this decode rather than on Default()
	// because "on" here would silently offload every draw on an installation that predates
	// the field, which is a performance change a user never asked for.
	if s.Images.CLIOffloadToCPU {
		t.Error("offload-to-cpu is on for a row that never chose it")
	}
}

// TestImageKindKeepsTheSpellingsItHas pins the normalisation rule, whose whole purpose is that
// a hand-edited blob cannot leave the app with a backend it cannot use. The two *local*
// lifetimes are stored as themselves — `cli` for the program run once per picture and
// `sdserver` for the same program run as a server KiChat starts and stops — and everything
// else, including the HTTP server kind spelled out in full, becomes the empty string that
// already means "the server you run yourself", so there is one spelling per option and a typo
// degrades to the backend that used to work.
//
// The managed kind is the one that must not be folded away: it is a process this app owns, so
// losing the name at load time would silently swap the user's sd-server for whatever answers
// on the stored HTTP address — the exact confusion the third kind was added to end. The
// hosted kind joins it for the opposite reason: nothing about it is local, so a folded value
// would silently spend the user's account quota on the WebUI's address instead.
func TestImageKindKeepsTheSpellingsItHas(t *testing.T) {
	cases := []struct {
		in   string
		want string
		why  string
	}{
		{"", "", "an installation predating the field keeps the server backend"},
		{"server", "", "the server kind is spelled empty, not \"server\""},
		{"cli", "cli", "the one-shot program is stored as itself"},
		{"  cli  ", "cli", "a value typed with spaces is trimmed"},
		{"sdserver", "sdserver", "the managed server is stored as itself, or a restart would lose it"},
		{"  sdserver ", "sdserver", "the same trim applies to it"},
		{"nim", "nim", "the hosted kind is stored as itself, or the panel would lose the choice"},
		{"  nim ", "nim", "the same trim applies to it"},
		{"NIM", "", "the kind is a key, so case matters here too"},
		{"sdcli", "", "a typo degrades to the backend that used to work"},
		{"sd-server", "", "the kind is a key, not the program's file name"},
		{"CLI", "", "the value is a key, so case matters"},
	}
	for _, tc := range cases {
		s := Default()
		s.Images.Kind = tc.in
		s.normalise()
		if s.Images.Kind != tc.want {
			t.Errorf("Kind %q normalised to %q, want %q (%s)", tc.in, s.Images.Kind, tc.want, tc.why)
		}
	}
}

// TestHostedImageModelDefaultsWithoutAMigration pins the two halves of the hosted model
// setting: a blob written before the field exists keeps a model that can draw, and a value
// the vendor adds later survives a save rather than being folded away.
func TestHostedImageModelDefaultsWithoutAMigration(t *testing.T) {
	s := Default()
	if err := json.Unmarshal([]byte(`{"images":{"kind":"nim"}}`), &s); err != nil {
		t.Fatalf("decode an older blob: %v", err)
	}
	if s.Images.NIMModel == "" {
		t.Fatal("an older blob left the hosted model empty; the default did not survive the decode")
	}
	s.normalise()
	if s.Images.NIMModel == "" {
		t.Fatal("normalise emptied the hosted model")
	}

	// A model id this build has never heard of is kept: the vendor adds them faster than
	// this app ships, and a folded id would be a setting the user cannot set.
	s.Images.NIMModel = "  acme/obscure-model  "
	s.normalise()
	if s.Images.NIMModel != "acme/obscure-model" {
		t.Errorf("NIMModel = %q, want the trimmed id kept as typed", s.Images.NIMModel)
	}

	// A bare name is completed to the publisher path the hosted route takes: an id without
	// its publisher is a 404 there rather than a different model, so a value written that way
	// must not be sent as-is.
	// `flux-3-image-klein` is here because a stored blob really carried it: an early build of
	// this field wrote the bare name, and a bare name is `404 page not found` on the hosted
	// route rather than a different model.
	for _, tc := range []struct{ in, want string }{
		{"flux.1-schnell", "black-forest-labs/flux.1-schnell"},
		{" flux.1-dev ", "black-forest-labs/flux.1-dev"},
		{"flux-3-image-klein", "black-forest-labs/flux-3-image-klein"},
		{"acme/obscure-model", "acme/obscure-model"},
	} {
		s.Images.NIMModel = tc.in
		s.normalise()
		if s.Images.NIMModel != tc.want {
			t.Errorf("NIMModel %q became %q, want %q", tc.in, s.Images.NIMModel, tc.want)
		}
	}
}

// TestTheHostedKindDrawsWithItsOwnModel pins the rule that keeps a checkpoint field from
// crossing kinds: the hosted route takes a model id, and the field a WebUI's checkpoint lives
// in must not be sent to it.
func TestTheHostedKindDrawsWithItsOwnModel(t *testing.T) {
	s := Default()
	s.Images.Model = "sdxl.safetensors [a1b2c3]"
	s.Images.NIMModel = "black-forest-labs/flux.1-dev"
	if got := s.Images.GenerationModel(); got != "sdxl.safetensors [a1b2c3]" {
		t.Errorf("the server kind asked for %q, want its checkpoint", got)
	}
	s.Images.Kind = ImageKindNIM
	if got := s.Images.GenerationModel(); got != "black-forest-labs/flux.1-dev" {
		t.Errorf("the hosted kind asked for %q, want its own model id", got)
	}
}
