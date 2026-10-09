package main

import (
	"os"
	"path/filepath"
	"testing"

	"kichat/internal/ingest"
	"kichat/internal/store"
)

// TestAddAttachmentCleansUpTheDocumentWhenItCannotBeAttached pins the failure
// path where the attachment row cannot be written. The document — the whole
// extracted text, and the parent of its chunks — is written first because the
// attachment references it, so a failed attachment insert used to leave that text
// in the database with nothing pointing at it: the largest row in the schema,
// unreachable, permanently.
//
// A chat id that does not exist produces the failure without mocking anything.
// attachments.chat_id has a foreign key to chats(id) and the pragma is on, while
// documents.chat_id deliberately has none — which is exactly why the document
// survives when the attachment does not.
func TestAddAttachmentCleansUpTheDocumentWhenItCannotBeAttached(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	a := &App{store: st, extractor: ingest.New()}
	// Every bound method calls ensure() first, and it would initialise the real
	// ~/.config/kichat. Marking the one-time step done keeps this test off the
	// user's data directory, which a test must never touch.
	a.initOnce.Do(func() {})

	source := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(source, []byte("Ein kurzer Text zum Anhaengen.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := a.AddAttachment(AttachmentInput{ChatID: "chat-does-not-exist", Path: source}); err == nil {
		t.Fatal("attaching to a missing chat reported success")
	}

	docs, err := st.ListDocuments("chat-does-not-exist")
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("the failed attachment left %d document(s) behind: %+v", len(docs), docs)
	}
}
