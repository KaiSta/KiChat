package main

import (
	"testing"

	"kichat/internal/config"
)

// TestANewConversationStartsOnTheModesSelectedPreset is the two selectors at the point they are
// read: a chat takes the named prompt chosen for Chat, a project the one chosen for Code, and
// nothing a conversation starts on is nameless.
func TestANewConversationStartsOnTheModesSelectedPreset(t *testing.T) {
	s := config.Default()
	wantChat := config.PromptBody(config.PresetResearch, s.Language)
	if prompt, id := chatPrompt(s); prompt != wantChat || id != config.PresetResearch {
		t.Errorf("a default chat starts on %q (%q), want the research preset", prompt, id)
	}
	wantProject := config.PromptBody(config.PresetCodingAgent, s.Language)
	if prompt, id := projectPrompt(s); prompt != wantProject || id != config.PresetCodingAgent {
		t.Errorf("a default project starts on %q (%q), want the coding-agent preset", prompt, id)
	}

	// A chosen preset is what the next chat starts on, text and tag together — and an edit to
	// the unnamed default prompt no longer leaks into a chat that names its prompt.
	s.ChatPromptPreset = config.PresetAssistant
	s.DefaultSystemPrompt = "edit that belongs to no preset"
	want := config.PromptBody(config.PresetAssistant, s.Language)
	if prompt, id := chatPrompt(s); prompt != want || id != config.PresetAssistant {
		t.Errorf("chat prompt = %q (%q), want the assistant preset", prompt, id)
	}

	// The empty preset is a choice, not a missing one: a chat with no system prompt is what
	// it asks for.
	s.ChatPromptPreset = config.PresetBlank
	if prompt, id := chatPrompt(s); prompt != "" || id != config.PresetBlank {
		t.Errorf("the empty preset produced %q (%q), want no prompt", prompt, id)
	}

	// Code has a selection of its own, and it does not follow the chat's.
	s.CodingPromptPreset = config.PresetCoding
	wantCode := config.PromptBody(config.PresetCoding, s.Language)
	if prompt, id := projectPrompt(s); prompt != wantCode || id != config.PresetCoding {
		t.Errorf("project prompt = %q (%q), want the coding preset", prompt, id)
	}

	// A selection naming a preset that is gone falls back instead of leaving a mode with an
	// empty prompt: the default prompt field for a chat, the shipped project prompt for a
	// project. The selectors cannot produce this, which is why it is only a safety net.
	s.ChatPromptPreset, s.CodingPromptPreset = "gone", "gone"
	if prompt, id := chatPrompt(s); prompt != s.DefaultSystemPrompt || id != config.PresetResearch {
		t.Errorf("a missing chat preset produced %q (%q), want the default prompt field", prompt, id)
	}
	if prompt, id := projectPrompt(s); prompt != wantProject || id != config.PresetCodingAgent {
		t.Errorf("a missing code preset produced %q (%q), want the coding-agent preset", prompt, id)
	}
}
