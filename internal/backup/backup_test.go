package backup

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"kichat/internal/store"
)

func fixture() Archive {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	return Archive{
		App:      "0.4.0",
		Exported: base,
		Chats: []store.ChatBundle{{
			Chat: store.Chat{
				// Long, distinctive markers: a two-character id or a four-digit number
				// has a real chance of occurring in random ciphertext, which would make
				// TestArchiveLeaksNothing flaky rather than wrong.
				ID: "chat-7f3a9c2e", Title: "Secret plans", CreatedAt: base, UpdatedAt: base,
			},
			Messages: []store.Message{{
				ID: "msg-2b8e41da", ChatID: "chat-7f3a9c2e", Seq: 1, Role: "user",
				Content: "the vault code is 4711-ALPHA-ZULU", Status: store.StatusComplete,
				CreatedAt: base,
			}},
			Documents: []store.Document{{
				ID: "doc-9c1f55ab", ChatID: "chat-7f3a9c2e", Title: "notes.txt",
				Text: "a document only the user should read", CreatedAt: base,
			}},
		}},
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	raw, err := Encode(fixture(), "correct horse battery")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(raw, "correct horse battery")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Format != Format {
		t.Errorf("format = %d, want %d", got.Format, Format)
	}
	if len(got.Chats) != 1 || got.Chats[0].Chat.Title != "Secret plans" {
		t.Fatalf("chats = %+v", got.Chats)
	}
	if got.Chats[0].Messages[0].Content != "the vault code is 4711-ALPHA-ZULU" {
		t.Errorf("message text did not survive: %q", got.Chats[0].Messages[0].Content)
	}
	if !got.Exported.Equal(fixture().Exported) {
		t.Errorf("exported = %v, want %v", got.Exported, fixture().Exported)
	}
}

// TestArchiveLeaksNothing is the point of the format: a file that sits in a cloud
// folder must not be readable by whoever finds it. If any of this text appears in the
// container, the encryption is not covering what it claims to.
func TestArchiveLeaksNothing(t *testing.T) {
	raw, err := Encode(fixture(), "correct horse battery")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, secret := range []string{
		"Secret plans",
		"4711-ALPHA-ZULU",
		"a document only the user should read",
		"chat-7f3a9c2e",
	} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("%q appears in the archive", secret)
		}
	}
	if !bytes.HasPrefix(raw, []byte(magic)) {
		t.Errorf("archive does not start with its magic")
	}
}

func TestWrongPassphrase(t *testing.T) {
	raw, err := Encode(fixture(), "correct horse battery")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := Decode(raw, "correct horse batterY"); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("err = %v, want ErrPassphrase", err)
	}
}

// TestEditedArchiveFails covers the header being the additional data: flipping a byte
// anywhere must not produce a readable archive.
func TestEditedArchiveFails(t *testing.T) {
	raw, err := Encode(fixture(), "correct horse battery")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, at := range []int{len(magic) + 1 + 5, headerLength + 2, len(raw) - 1} {
		edited := append([]byte(nil), raw...)
		edited[at] ^= 0xff
		if _, err := Decode(edited, "correct horse battery"); err == nil {
			t.Errorf("a byte flipped at %d still decoded", at)
		}
	}
}

func TestRejectsForeignAndTruncatedFiles(t *testing.T) {
	if _, err := Decode([]byte("SQLite format 3\x00"), "whatever"); !errors.Is(err, ErrFormat) {
		t.Errorf("foreign file: err = %v, want ErrFormat", err)
	}
	raw, err := Encode(fixture(), "correct horse battery")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := Decode(raw[:headerLength-1], "correct horse battery"); !errors.Is(err, ErrFormat) {
		t.Errorf("truncated header: err = %v, want ErrFormat", err)
	}
	if _, err := Decode(raw[:len(raw)-1], "correct horse battery"); !errors.Is(err, ErrPassphrase) {
		t.Errorf("truncated body: err = %v, want ErrPassphrase", err)
	}
}

func TestEncodeRefusesWhatCannotBeOpenedLater(t *testing.T) {
	if _, err := Encode(fixture(), "short"); err == nil {
		t.Error("a passphrase below the minimum was accepted")
	}
	empty := fixture()
	empty.Chats = nil
	if _, err := Encode(empty, "correct horse battery"); !errors.Is(err, ErrEmpty) {
		t.Errorf("empty archive: err = %v, want ErrEmpty", err)
	}
}

func TestManifestSummarises(t *testing.T) {
	m := ManifestOf(fixture())
	if m.App != "0.4.0" || len(m.Chats) != 1 {
		t.Fatalf("manifest = %+v", m)
	}
	if m.Chats[0].Title != "Secret plans" || m.Chats[0].Messages != 1 {
		t.Errorf("summary = %+v", m.Chats[0])
	}
	if m.Bytes == 0 {
		t.Error("bytes = 0, want the document text to be counted")
	}
	if strings.Contains(m.Chats[0].Title, "\x00") {
		t.Error("unexpected NUL in the title")
	}
}
