package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"kichat/internal/backup"
	"kichat/internal/store"
	"kichat/internal/uimsg"
	"kichat/internal/version"
)

// The backup file's extension. It is not a registered MIME type and does not need to
// be: the point of a suffix is that a person can tell these files apart in a folder of
// downloads, and that a cloud client leaves them alone.
const backupExtension = ".kichat"

// BackupExportRequest is what the Data tab sends when the user exports.
type BackupExportRequest struct {
	// ChatIDs is the selection, in the order the panel listed it. An empty list is
	// refused rather than read as "everything": an archive the user did not ask for
	// is a file in a folder they did not choose.
	ChatIDs    []string `json:"chatIds"`
	Passphrase string   `json:"passphrase"`
	// Title is the save dialog's title, supplied by the UI so it follows the
	// interface language — the same arrangement PickImageFile has.
	Title string `json:"title"`
}

// BackupExportResult reports what was written. Path is empty when the user closed the
// save dialog, which is not a failure.
type BackupExportResult struct {
	Path  string `json:"path"`
	Chats int    `json:"chats"`
	Bytes int64  `json:"bytes"`
}

// ExportBackup writes the selected chats into one encrypted archive.
func (a *App) ExportBackup(req BackupExportRequest) (BackupExportResult, error) {
	if err := a.ensure(); err != nil {
		return BackupExportResult{}, err
	}
	if a.ctx == nil {
		return BackupExportResult{}, uimsg.Errorf("app.notReady")
	}
	raw, chats, err := a.buildBackup(req.ChatIDs, req.Passphrase)
	if err != nil {
		return BackupExportResult{}, err
	}

	path, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           req.Title,
		DefaultFilename: "kichat-backup-" + time.Now().Format("2006-01-02") + backupExtension,
	})
	if err != nil {
		return BackupExportResult{}, err
	}
	if strings.TrimSpace(path) == "" {
		return BackupExportResult{}, nil // Dialog closed; not an error.
	}
	if !strings.HasSuffix(strings.ToLower(path), backupExtension) {
		path += backupExtension
	}
	// 0600: the archive is encrypted, and a backup that is readable because of its
	// mode would make the passphrase pointless.
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return BackupExportResult{}, uimsg.Errorf("backup.writeFailed", err)
	}
	return BackupExportResult{Path: path, Chats: chats, Bytes: int64(len(raw))}, nil
}

// buildBackup produces the archive bytes, with no dialog anywhere in it.
//
// Split out from the binding so the whole journey — a real database out, a real archive
// in — can be exercised by a test. The dialog is the one part that cannot be, and it is
// the part with the least to go wrong.
func (a *App) buildBackup(chatIDs []string, passphrase string) ([]byte, int, error) {
	if len(chatIDs) == 0 {
		return nil, 0, uimsg.Errorf("backup.noChats")
	}
	if len([]rune(passphrase)) < backup.MinPassphrase {
		return nil, 0, uimsg.Errorf("backup.passphraseShort", backup.MinPassphrase)
	}
	archive := backup.Archive{App: version.String(), Exported: time.Now().UTC()}
	for _, id := range chatIDs {
		bundle, err := a.store.LoadChatBundle(id)
		if err != nil {
			return nil, 0, uimsg.Errorf("backup.readChat", id, err)
		}
		archive.Chats = append(archive.Chats, bundle)
	}
	raw, err := backup.Encode(archive, passphrase)
	if err != nil {
		// The two refusals Encode can make are the two checked above, so anything
		// reaching here is a failure to produce the bytes, not a bad selection.
		return nil, 0, uimsg.Errorf("backup.encodeFailed", err)
	}
	return raw, len(archive.Chats), nil
}

// BackupEntry is one chat in an opened archive, with what importing it would do.
type BackupEntry struct {
	ChatID      string    `json:"chatId"`
	Title       string    `json:"title"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Messages    int       `json:"messages"`
	Attachments int       `json:"attachments"`
	Bytes       int64     `json:"bytes"`
	// State is "new", "newer", "older" or "same", measured against this database.
	// Only "new" and "newer" are written by ImportBackup: the rule is that the newer
	// copy wins, so an archive cannot roll a conversation back.
	State string `json:"state"`
}

// BackupPreview is an opened archive waiting for the user to choose from it.
type BackupPreview struct {
	Path     string        `json:"path"`
	App      string        `json:"app"`
	Exported time.Time     `json:"exported"`
	Chats    []BackupEntry `json:"chats"`
}

// OpenBackup asks for an archive, opens it with the passphrase, and reports what is
// inside without writing anything.
//
// The decrypted archive is held on the App between this call and ImportBackup. Holding
// it is what makes the two-step flow possible: the alternative is a second dialog and a
// second passphrase for the same file. It is dropped as soon as the import finishes.
func (a *App) OpenBackup(passphrase string, title string) (BackupPreview, error) {
	if err := a.ensure(); err != nil {
		return BackupPreview{}, err
	}
	if a.ctx == nil {
		return BackupPreview{}, uimsg.Errorf("app.notReady")
	}
	// A new open starts from nothing. An archive the user has moved on from must not
	// stay importable behind a dialog they cancelled or a file that failed to open.
	a.backupMu.Lock()
	a.pendingBackup = nil
	a.backupMu.Unlock()

	path, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{Title: title})
	if err != nil {
		return BackupPreview{}, err
	}
	if strings.TrimSpace(path) == "" {
		return BackupPreview{}, nil // Dialog closed; not an error.
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return BackupPreview{}, uimsg.Errorf("backup.readFileFailed", err)
	}
	archive, err := backup.Decode(raw, passphrase)
	if err != nil {
		return BackupPreview{}, decodeError(err)
	}

	preview := BackupPreview{
		Path:     path,
		App:      archive.App,
		Exported: archive.Exported,
		Chats:    make([]BackupEntry, 0, len(archive.Chats)),
	}
	for _, bundle := range archive.Chats {
		summary := bundle.Summary()
		state, err := a.importState(summary)
		if err != nil {
			return BackupPreview{}, err
		}
		preview.Chats = append(preview.Chats, BackupEntry{
			ChatID:      summary.ChatID,
			Title:       summary.Title,
			UpdatedAt:   summary.UpdatedAt,
			Messages:    summary.Messages,
			Attachments: summary.Attachments,
			Bytes:       summary.Bytes,
			State:       state,
		})
	}
	// Newest first, matching the chat list the user knows.
	sort.SliceStable(preview.Chats, func(i, j int) bool {
		return preview.Chats[i].UpdatedAt.After(preview.Chats[j].UpdatedAt)
	})

	a.backupMu.Lock()
	a.pendingBackup = &archive
	a.backupMu.Unlock()
	return preview, nil
}

// importState says what importing a chat would do, without doing it.
func (a *App) importState(summary store.BundleSummary) (string, error) {
	local, err := a.store.GetChat(summary.ChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return "new", nil
	}
	if err != nil {
		return "", uimsg.Errorf("backup.readChat", summary.ChatID, err)
	}
	switch {
	case summary.UpdatedAt.After(local.UpdatedAt):
		return "newer", nil
	case summary.UpdatedAt.Before(local.UpdatedAt):
		return "older", nil
	default:
		return "same", nil
	}
}

// BackupImportResult is what actually happened.
type BackupImportResult struct {
	Imported []string `json:"imported"`
	Replaced []string `json:"replaced"`
	Skipped  []string `json:"skipped"`
}

// ImportBackup writes the selected chats of the archive opened by OpenBackup.
//
// The newer copy wins: a chat that already exists here with a later timestamp is left
// alone and reported as skipped, because the alternative — replacing it — would undo
// work done on this machine since the archive was written. A chat that is new here, or
// newer in the archive, is written whole, through store.SaveChatBundle, so a chat is
// never half-imported.
func (a *App) ImportBackup(chatIDs []string) (BackupImportResult, error) {
	if err := a.ensure(); err != nil {
		return BackupImportResult{}, err
	}
	a.backupMu.Lock()
	pending := a.pendingBackup
	a.backupMu.Unlock()
	if pending == nil {
		return BackupImportResult{}, uimsg.Errorf("backup.nothingOpen")
	}
	selected := map[string]bool{}
	for _, id := range chatIDs {
		selected[id] = true
	}
	result := BackupImportResult{Imported: []string{}, Replaced: []string{}, Skipped: []string{}}
	for _, bundle := range pending.Chats {
		if !selected[bundle.Chat.ID] {
			continue
		}
		state, err := a.importState(bundle.Summary())
		if err != nil {
			return result, err
		}
		if state != "new" && state != "newer" {
			result.Skipped = append(result.Skipped, bundle.Chat.ID)
			continue
		}
		if err := a.store.SaveChatBundle(bundle); err != nil {
			return result, uimsg.Errorf("backup.importFailed", bundle.Chat.Title, err)
		}
		if state == "new" {
			result.Imported = append(result.Imported, bundle.Chat.ID)
		} else {
			result.Replaced = append(result.Replaced, bundle.Chat.ID)
		}
	}
	// The archive is done with: importing it again is a no-op under the same rule, and
	// keeping it in memory would leave one machine's history decrypted for the rest of
	// the session.
	a.backupMu.Lock()
	a.pendingBackup = nil
	a.backupMu.Unlock()
	return result, nil
}

// decodeError turns the format's errors into the one message each deserves. They are
// raised for three different reasons and read as three different sentences.
func decodeError(err error) error {
	switch {
	case errors.Is(err, backup.ErrPassphrase):
		return uimsg.Errorf("backup.wrongPassphrase")
	case errors.Is(err, backup.ErrVersion):
		return uimsg.Errorf("backup.newerVersion")
	case errors.Is(err, backup.ErrTooLarge):
		return uimsg.Errorf("backup.tooLarge")
	case errors.Is(err, backup.ErrEmpty):
		return uimsg.Errorf("backup.empty")
	case errors.Is(err, backup.ErrFormat):
		return uimsg.Errorf("backup.notArchive")
	default:
		return uimsg.Errorf("backup.decodeFailed", fmt.Sprint(err))
	}
}
