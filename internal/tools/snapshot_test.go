package tools

// The undo record a change leaves behind, and what it must be able to answer.
//
// A revert is only as good as this record: the file as the chat first found it (not as the
// previous round left it), and the digest of what was last written, so that a file somebody
// edited in their own editor afterwards is a refusal rather than something to overwrite.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"kichat/internal/workspace"
)

// TestAWriteRecordsWhatItReplaced is the whole promise of the undo: the text that was there
// before this chat touched the file, kept where the panel can find it.
func TestAWriteRecordsWhatItReplaced(t *testing.T) {
	service, chatID, folder := projectService(t, map[string]string{"main.go": "package main\n"})
	service.Confirm = (&asked{}).confirm

	if _, err := service.Execute(context.Background(), chatID, ToolWriteFile,
		`{"path":"main.go","content":"package other\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	snap, ok, err := service.Store.GetFileSnapshot(chatID, "main.go")
	if err != nil {
		t.Fatalf("read the snapshot: %v", err)
	}
	if !ok {
		t.Fatal("a write left no snapshot, so it cannot be undone")
	}
	if snap.BeforeText != "package main\n" {
		t.Errorf("the pre-image is %q, want the text that was replaced", snap.BeforeText)
	}
	if !snap.Existed {
		t.Error("the snapshot says the file did not exist, but it did")
	}
	// The digest describes what is on disk now, which is what makes a revert notice a file
	// that changed afterwards.
	current, err := os.ReadFile(filepath.Join(folder, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if snap.AfterSHA != workspace.Digest(string(current)) {
		t.Errorf("the recorded digest does not match the file the write left behind")
	}
}

// TestACreatedFileIsRecordedAsAbsent is the case a text alone cannot express: an empty file and
// no file at all look the same in a string, and the undo of a create is a removal.
func TestACreatedFileIsRecordedAsAbsent(t *testing.T) {
	service, chatID, _ := projectService(t, nil)
	service.Confirm = (&asked{}).confirm

	if _, err := service.Execute(context.Background(), chatID, ToolWriteFile,
		`{"path":"docs/new.md","content":"# New\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	snap, ok, err := service.Store.GetFileSnapshot(chatID, "docs/new.md")
	if err != nil || !ok {
		t.Fatalf("no snapshot for a created file: ok=%v err=%v", ok, err)
	}
	if snap.Existed {
		t.Error("the snapshot says the file existed before the chat created it")
	}
	if snap.BeforeText != "" {
		t.Errorf("the pre-image of a create is %q, want empty", snap.BeforeText)
	}
}

// TestASecondChangeKeepsTheFirstPreImage is the difference between "undo this chat's change"
// and "undo the last round": the model usually takes several rounds to get a file right, and a
// person asking to undo wants the file back as they wrote it, not as the model's second draft
// left it.
func TestASecondChangeKeepsTheFirstPreImage(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{"main.go": "package mine\n"})
	service.Confirm = (&asked{}).confirm
	ctx := context.Background()

	if _, err := service.Execute(ctx, chatID, ToolWriteFile,
		`{"path":"main.go","content":"package draft\n"}`); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := service.Execute(ctx, chatID, ToolEditFile,
		`{"path":"main.go","old_string":"package draft","new_string":"package final"}`); err != nil {
		t.Fatalf("edit: %v", err)
	}
	snap, ok, err := service.Store.GetFileSnapshot(chatID, "main.go")
	if err != nil || !ok {
		t.Fatalf("no snapshot after two changes: ok=%v err=%v", ok, err)
	}
	if snap.BeforeText != "package mine\n" {
		t.Errorf("the pre-image is %q, want the text before the chat's first change", snap.BeforeText)
	}
	if snap.Revisions != 2 {
		t.Errorf("revisions = %d, want 2", snap.Revisions)
	}
	if want := workspace.Digest("package final\n"); snap.AfterSHA != want {
		t.Errorf("the digest is not the file the edit left behind")
	}
}

// TestADeclinedChangeRecordsNothing keeps the refusal and the undo record consistent: a change
// nobody allowed must not appear in the list, and must not offer to restore a file it never
// touched.
func TestADeclinedChangeRecordsNothing(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{"main.go": "package main\n"})
	service.Confirm = (&asked{answer: ErrDenied}).confirm

	if _, err := service.Execute(context.Background(), chatID, ToolWriteFile,
		`{"path":"main.go","content":"package other\n"}`); err == nil {
		t.Fatal("a declined write reported success")
	}
	if _, ok, err := service.Store.GetFileSnapshot(chatID, "main.go"); err != nil || ok {
		t.Errorf("a declined change left a snapshot: ok=%v err=%v", ok, err)
	}
}
