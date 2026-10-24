package main

import (
	"errors"
	"testing"
	"time"

	"kichat/internal/backup"
	"kichat/internal/config"
	"kichat/internal/store"
)

// openArchive is what OpenBackup does after the file dialog: decrypt the bytes and keep
// the result as the archive an import may draw from.
func openArchive(t *testing.T, a *App, raw []byte, passphrase string) {
	t.Helper()
	archive, err := backup.Decode(raw, passphrase)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	a.backupMu.Lock()
	a.pendingBackup = &archive
	a.backupMu.Unlock()
}

// TestBackupMovesAChatBetweenMachines is the whole feature in one test: a chat leaves
// one database as an encrypted file, and arrives in another one whole — messages,
// documents and the text of a picture included.
func TestBackupMovesAChatBetweenMachines(t *testing.T) {
	source := newSettingsTestApp(t, config.Default(), t.TempDir())
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := source.store.CreateChat(store.Chat{
		ID: "c1", Title: "Trip", CreatedAt: now, UpdatedAt: now,
		Backend: "lmstudio", Model: "m", Temperature: 0.7, TopP: 0.9,
		AutoCompact: true, CompactThreshold: 0.8, KeepRecentTurns: 6,
	}); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	msg, err := source.store.AppendMessage(store.Message{
		ID: "m1", ChatID: "c1", Role: "user", Content: "where next?",
		Status: store.StatusComplete, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := source.store.CreateDocument(store.Document{
		ID: "d1", ChatID: "c1", Title: "map.png", Text: "aGVsbG8=", CreatedAt: now,
	}); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	const pass = "correct horse battery"
	raw, chats, err := source.buildBackup([]string{"c1"}, pass)
	if err != nil {
		t.Fatalf("buildBackup: %v", err)
	}
	if chats != 1 {
		t.Fatalf("chats = %d, want 1", chats)
	}

	target := newSettingsTestApp(t, config.Default(), t.TempDir())
	openArchive(t, target, raw, pass)
	if state, err := target.importState(store.BundleSummary{ChatID: "c1", UpdatedAt: msg.CreatedAt}); err != nil || state != "new" {
		t.Fatalf("state = %q (%v), want new", state, err)
	}
	result, err := target.ImportBackup([]string{"c1"})
	if err != nil {
		t.Fatalf("ImportBackup: %v", err)
	}
	if len(result.Imported) != 1 || len(result.Replaced) != 0 {
		t.Fatalf("result = %+v, want one import", result)
	}

	restored, err := target.store.LoadChatBundle("c1")
	if err != nil {
		t.Fatalf("LoadChatBundle: %v", err)
	}
	if restored.Chat.Title != "Trip" {
		t.Errorf("title = %q", restored.Chat.Title)
	}
	if len(restored.Messages) != 1 || restored.Messages[0].Content != "where next?" {
		t.Errorf("messages = %+v", restored.Messages)
	}
	if len(restored.Documents) != 1 || restored.Documents[0].Text != "aGVsbG8=" {
		t.Errorf("documents = %+v", restored.Documents)
	}
	if target.pendingBackup != nil {
		t.Error("the decrypted archive was kept after the import")
	}
}

// TestImportNeverOverwritesNewerWork is the policy the user chose: the newer copy wins,
// in both directions, so an archive can add to a machine but cannot roll it back.
func TestImportNeverOverwritesNewerWork(t *testing.T) {
	a := newSettingsTestApp(t, config.Default(), t.TempDir())
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := a.store.CreateChat(store.Chat{
		ID: "c1", Title: "Local", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	// An archive from before the local copy was edited.
	older := backup.Archive{Format: backup.Format, Chats: []store.ChatBundle{{
		Chat: store.Chat{ID: "c1", Title: "Archive", CreatedAt: now, UpdatedAt: now.Add(-time.Hour)},
		Messages: []store.Message{{
			ID: "m1", ChatID: "c1", Seq: 1, Role: "user", Content: "old",
			Status: store.StatusComplete, CreatedAt: now,
		}},
	}}}
	a.backupMu.Lock()
	a.pendingBackup = &older
	a.backupMu.Unlock()
	result, err := a.ImportBackup([]string{"c1"})
	if err != nil {
		t.Fatalf("ImportBackup: %v", err)
	}
	if len(result.Skipped) != 1 || len(result.Replaced) != 0 {
		t.Fatalf("result = %+v, want the chat skipped", result)
	}
	local, err := a.store.GetChat("c1")
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if local.Title != "Local" {
		t.Errorf("title = %q, want the local copy untouched", local.Title)
	}

	// The same archive, now newer than the local copy: it replaces it whole.
	newer := backup.Archive{Format: backup.Format, Chats: []store.ChatBundle{{
		Chat: store.Chat{ID: "c1", Title: "Archive", CreatedAt: now, UpdatedAt: now.Add(2 * time.Hour)},
		Messages: []store.Message{{
			ID: "m1", ChatID: "c1", Seq: 1, Role: "user", Content: "new",
			Status: store.StatusComplete, CreatedAt: now,
		}},
	}}}
	a.backupMu.Lock()
	a.pendingBackup = &newer
	a.backupMu.Unlock()
	result, err = a.ImportBackup([]string{"c1"})
	if err != nil {
		t.Fatalf("ImportBackup: %v", err)
	}
	if len(result.Replaced) != 1 {
		t.Fatalf("result = %+v, want one replacement", result)
	}
	bundle, err := a.store.LoadChatBundle("c1")
	if err != nil {
		t.Fatalf("LoadChatBundle: %v", err)
	}
	if bundle.Chat.Title != "Archive" || bundle.Messages[0].Content != "new" {
		t.Errorf("bundle = %+v, want the newer archive written over the local copy", bundle)
	}
}

// TestImportRefusesWhatWasNeverOpened covers the two calls a panel can make out of
// order: an import with no archive, and a wrong passphrase.
func TestImportRefusesWhatWasNeverOpened(t *testing.T) {
	a := newSettingsTestApp(t, config.Default(), t.TempDir())
	if _, err := a.ImportBackup([]string{"c1"}); err == nil {
		t.Error("an import with no archive open was accepted")
	}

	source := newSettingsTestApp(t, config.Default(), t.TempDir())
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := source.store.CreateChat(store.Chat{ID: "c1", Title: "T", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	raw, _, err := source.buildBackup([]string{"c1"}, "correct horse battery")
	if err != nil {
		t.Fatalf("buildBackup: %v", err)
	}
	if _, err := backup.Decode(raw, "wrong passphrase!"); !errors.Is(err, backup.ErrPassphrase) {
		t.Errorf("err = %v, want ErrPassphrase", err)
	}
}

// TestBuildBackupRefusesAnEmptySelection pins that an empty list is not read as
// "everything": a backup the user did not ask for is a file in a folder they did not
// choose.
func TestBuildBackupRefusesAnEmptySelection(t *testing.T) {
	a := newSettingsTestApp(t, config.Default(), t.TempDir())
	if _, _, err := a.buildBackup(nil, "correct horse battery"); err == nil {
		t.Error("an empty selection was accepted")
	}
	if _, _, err := a.buildBackup([]string{"c1"}, "short"); err == nil {
		t.Error("a too-short passphrase was accepted")
	}
}
