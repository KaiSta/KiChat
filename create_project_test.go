package main

import (
	"os"
	"path/filepath"
	"testing"

	"kichat/internal/config"
	"kichat/internal/store"
)

// TestASecondProjectCannotClaimTheSameFolder pins the rule a project is built on: a project is
// its conversation, and `/clear` exists so a second one is never needed. Two chats on one
// folder would carry two sets of undo records and two histories of writes to the same files,
// and each would quietly contradict the other's to the model.
func TestASecondProjectCannotClaimTheSameFolder(t *testing.T) {
	folder := t.TempDir()
	other := t.TempDir()
	chats := []store.Chat{
		{ID: "p1", Title: "First", Mode: store.ModeCoding, Workspace: folder},
		{ID: "c1", Title: "Conversation", Mode: store.ModeChat},
		{ID: "p2", Title: "Finished", Mode: store.ModeCoding, Workspace: other, Archived: true},
	}

	found, ok := projectClaimingFolder(chats, folder)
	if !ok || found.ID != "p1" {
		t.Errorf("the folder reports %+v/%v, want the project that works in it", found, ok)
	}
	// An archived project does not claim its folder: the user finished with it, and no new
	// project could ever be started there again if it did.
	if found, ok := projectClaimingFolder(chats, other); ok {
		t.Errorf("an archived project still claims its folder: %+v", found)
	}
	// A plain chat has no folder, so it can never claim one.
	if _, ok := projectClaimingFolder(chats, t.TempDir()); ok {
		t.Error("a folder nobody works in is reported as claimed")
	}

	// The same folder through a symlink is the same folder, which a string comparison would
	// miss — and that is exactly the duplicate somebody creates by picking it twice.
	link := filepath.Join(t.TempDir(), "shortcut")
	if err := os.Symlink(folder, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	if found, ok := projectClaimingFolder(chats, link); !ok || found.ID != "p1" {
		t.Errorf("a symlink to the folder reports %+v/%v, want the same project", found, ok)
	}
}

// TestTheWindowCannotMoveAProjectsFolder keeps the boundary the folder was chosen through: a
// project's folder is resolved on disk when the project is created and refused if another
// project already works in it, while `UpdateChat` accepts a whole chat from the window. A field
// echoed back from there is a value nobody validated, and asking the file tools to resolve
// against it would point them anywhere.
func TestTheWindowCannotMoveAProjectsFolder(t *testing.T) {
	existing := store.Chat{
		ID: "p1", Title: "old", Mode: store.ModeCoding, Workspace: "/tmp/project",
		Backend: "b", Model: "m", Temperature: 0.7,
	}
	incoming := existing
	incoming.Title = "new"
	incoming.Temperature = 0.2
	incoming.Workspace = "/tmp/elsewhere"
	incoming.Mode = store.ModeChat

	got := updateFromWindow(incoming, existing)
	if got.Workspace != existing.Workspace || got.Mode != existing.Mode {
		t.Errorf("the update moved the project to %q/%q", got.Workspace, got.Mode)
	}
	// Everything the window does own still goes through: the point is the two fields, not a
	// frozen row.
	if got.Title != "new" || got.Temperature != 0.2 {
		t.Errorf("the update lost the settings it may change: %+v", got)
	}
}

// TestANewProjectStartsOnTheEditedPreset is the seam between Settings → Prompts and a new
// project: the preset a project is created with is the textarea the user sees, so a rewritten
// coding-agent prompt has to be what the next project actually starts on. The shipped body is
// only the fallback for somebody who never touched it (or emptied it).
func TestANewProjectStartsOnTheEditedPreset(t *testing.T) {
	settings := config.Default()
	if got := codingAgentPrompt(settings); got != config.PromptBody(config.PresetCodingAgent, settings.Language) {
		t.Errorf("an untouched preset gives %q, want the shipped body", got)
	}

	edited := "Arbeite nur in diesem Ordner und erkläre jede Änderung."
	for i := range settings.PromptPresets {
		if settings.PromptPresets[i].ID == config.PresetCodingAgent {
			settings.PromptPresets[i].Prompt = edited
		}
	}
	if got := codingAgentPrompt(settings); got != edited {
		t.Errorf("an edited preset gives %q, want the user's text", got)
	}

	// Emptying the textarea is not an edit worth honouring: a project with no instructions at
	// all would answer its first message from a model that was told nothing about the folder.
	for i := range settings.PromptPresets {
		if settings.PromptPresets[i].ID == config.PresetCodingAgent {
			settings.PromptPresets[i].Prompt = "   "
		}
	}
	if got := codingAgentPrompt(settings); got != config.PromptBody(config.PresetCodingAgent, settings.Language) {
		t.Errorf("an emptied preset gives %q, want the shipped body", got)
	}
}
