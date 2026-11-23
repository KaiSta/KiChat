package config

import "testing"

// TestTheModeSelectionsSurviveAnOlderBlob pins the migration rule the settings blob relies on:
// a blob written before these fields existed decodes over the defaults, so both modes still
// name a prompt. A selection naming a preset the blob does not carry is repaired rather than
// kept, because a mode with no prompt is worse than the default.
func TestTheModeSelectionsSurviveAnOlderBlob(t *testing.T) {
	s := Default()
	if s.ChatPromptPreset != PresetResearch {
		t.Errorf("chat selection = %q, want %q", s.ChatPromptPreset, PresetResearch)
	}
	if s.CodingPromptPreset != PresetCodingAgent {
		t.Errorf("coding selection = %q, want %q", s.CodingPromptPreset, PresetCodingAgent)
	}

	// An explicit empty selection — a hand-edited blob, or a draft from a window that did not
	// know the field — is the default, not "no prompt".
	s.ChatPromptPreset, s.CodingPromptPreset = "", ""
	s.normalise()
	if s.ChatPromptPreset != PresetResearch || s.CodingPromptPreset != PresetCodingAgent {
		t.Errorf("empty selections were left unnamed: chat %q, coding %q",
			s.ChatPromptPreset, s.CodingPromptPreset)
	}

	// A selection whose preset is gone falls back per mode.
	s = Default()
	s.ChatPromptPreset, s.CodingPromptPreset = "gone", "gone"
	s.normalise()
	if s.ChatPromptPreset != PresetResearch || s.CodingPromptPreset != PresetCodingAgent {
		t.Errorf("a selection naming a missing preset survived: chat %q, coding %q",
			s.ChatPromptPreset, s.CodingPromptPreset)
	}

	// And a selection that names a preset the blob carries is kept, which is the point of the
	// field: the mode selectors would be decoration otherwise.
	s = Default()
	s.ChatPromptPreset, s.CodingPromptPreset = PresetAssistant, PresetCoding
	s.normalise()
	if s.ChatPromptPreset != PresetAssistant || s.CodingPromptPreset != PresetCoding {
		t.Errorf("a real selection was rewritten: chat %q, coding %q",
			s.ChatPromptPreset, s.CodingPromptPreset)
	}
}

// TestAnEditedDefaultPromptBecomesANamedPreset is the half of the migration that keeps a user's
// work: the textarea used to write into DefaultSystemPrompt, which had no name, and the mode
// selectors now pick prompts by name. An edited default is therefore moved into a preset of
// its own and selected for chat — once, however often the settings are loaded.
func TestAnEditedDefaultPromptBecomesANamedPreset(t *testing.T) {
	s := Default()
	s.Language = LangGerman
	s.normalise()
	const edited = "Antworte immer auf Deutsch und ohne Einleitung."
	s.DefaultSystemPrompt = edited
	s.normalise()

	if s.ChatPromptPreset != PresetCustom {
		t.Fatalf("chat selection = %q, want the migrated %q", s.ChatPromptPreset, PresetCustom)
	}
	var custom *PromptPreset
	for i := range s.PromptPresets {
		if s.PromptPresets[i].ID == PresetCustom {
			custom = &s.PromptPresets[i]
		}
	}
	if custom == nil {
		t.Fatalf("the edited default prompt was not kept as a preset: %v", s.PromptPresets)
	}
	if custom.Prompt != edited {
		t.Errorf("the migrated preset holds %q, want the edited default prompt", custom.Prompt)
	}
	// The field itself went back to the shipped body, so the text lives in one named place and
	// the move cannot fire again on the next load.
	if want := PromptBody(PresetResearch, LangGerman); s.DefaultSystemPrompt != want {
		t.Errorf("the default prompt field still holds the moved text: %q", s.DefaultSystemPrompt)
	}
	if custom.Name == "" {
		t.Error("the migrated preset has no name, so no selector could show it")
	}
	if custom.Built {
		t.Error("the migrated preset is marked as shipped, so a language switch would rewrite it")
	}

	// Loading again must not add a second copy: the blob is normalised on every start.
	before := len(s.PromptPresets)
	s.normalise()
	if len(s.PromptPresets) != before {
		t.Errorf("a second normalise added a preset: %d, want %d", len(s.PromptPresets), before)
	}
}

// TestADefaultPromptThatAlreadyHasANameKeepsIt is the other side of the migration: pasting a
// stored prompt into the default field must not create a second copy of text that already has
// a name — the selector then picks the preset that was there all along.
func TestADefaultPromptThatAlreadyHasANameKeepsIt(t *testing.T) {
	s := Default()
	s.DefaultSystemPrompt = PromptBody(PresetAssistant, s.Language)
	s.normalise()

	if s.ChatPromptPreset != PresetAssistant {
		t.Errorf("chat selection = %q, want the assistant preset it matches", s.ChatPromptPreset)
	}
	if hasPromptPreset(s.PromptPresets, PresetCustom) {
		t.Error("a copy of an existing prompt was added as a custom preset")
	}
}
