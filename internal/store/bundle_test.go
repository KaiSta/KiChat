package store

import (
	"reflect"
	"testing"
	"time"
)

// bundleFixture is one of everything a bundle can carry. Times are whole milliseconds
// in UTC because that is what the database stores — a nanosecond fixture would fail the
// comparison for a reason that has nothing to do with the code under test.
func bundleFixture() ChatBundle {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	return ChatBundle{
		Chat: Chat{
			ID: "c1", Title: "Trip", CreatedAt: base, UpdatedAt: base.Add(time.Hour),
			Backend: "lmstudio", Model: "m", SystemPrompt: "sys", Temperature: 0.7,
			TopP: 0.9, NumCtx: 8192, AutoCompact: true, CompactThreshold: 0.8,
			KeepRecentTurns: 6, SearchFirst: true,
		},
		Messages: []Message{
			{ID: "m1", ChatID: "c1", Seq: 1, Role: "user", Content: "hello",
				Status: StatusComplete, CreatedAt: base},
			{ID: "m2", ChatID: "c1", Seq: 2, Role: "assistant", Content: "hi",
				Reasoning: "because", Status: StatusInterrupted, CreatedAt: base.Add(time.Second),
				PromptTokens: 12, CompletionTokens: 34, TokensPerSecond: 5.5, TTFTMs: 120,
				GenerationMs: 900, StatsSource: "reported",
				Sources: []Source{{Index: 1, Title: "T", URL: "https://example.com"}}},
		},
		Compactions: []Compaction{{
			ID: "k1", ChatID: "c1", Trigger: "manual", FromSeq: 1, ToSeq: 2,
			Summary: "they greeted", TokensBefore: 100, TokensAfter: 10,
			Model: "m", Backend: "lmstudio", CreatedAt: base.Add(2 * time.Second),
		}},
		ToolCalls: []ToolCallRecord{{
			ID: "t1", MessageID: "m2", ChatID: "c1", Name: "web_search",
			ArgsJSON: `{"q":"x"}`, ResultJSON: `[]`, Status: ToolOK,
			SourceTokens: 10, ContextTokens: 5, Condensed: true,
			CreatedAt: base.Add(3 * time.Second),
		}},
		Attachments: []Attachment{{
			ID: "a1", ChatID: "c1", MessageID: "m2", Kind: "image", Filename: "x.png",
			Mime: "image/png", SizeBytes: 12, SHA256: "abc", Placement: PlacementInline,
			DocumentID: "d1", CreatedAt: base.Add(4 * time.Second),
			// A path on the machine the archive was written on. The import must drop it.
			SavedPath:  "/home/someone/pictures/x.png",
			Generation: &GenerationRecipe{Prompt: "a cat", Width: 512, Height: 512, Seed: 7},
		}},
		Documents: []Document{{
			ID: "d1", AttachmentID: "a1", ChatID: "c1", Title: "x.png",
			Text: "aGVsbG8=", TokenCount: 2, SHA256: "abc",
			CreatedAt: base.Add(4 * time.Second),
		}},
		Chunks: []Chunk{{
			DocumentID: "d1", ChatID: "c1", Ordinal: 0, Heading: "caption",
			Page: 1, Text: "hello world", Tokens: 2,
		}},
	}
}

// normalize drops what cannot survive a copy: chunk ids are rowids this database
// allocated, so the same chunk has a different number on each side.
func normalize(b ChatBundle) ChatBundle {
	for i := range b.Chunks {
		b.Chunks[i].ID = 0
	}
	for i := range b.Attachments {
		b.Attachments[i].SavedPath = ""
	}
	return b
}

// TestChatBundleRoundTrip pins what a backup is supposed to be: a faithful copy of a
// chat. Everything that makes a chat work after the import is checked, including the
// full-text index — a restored chat whose documents cannot be searched is not restored.
func TestChatBundleRoundTrip(t *testing.T) {
	source := newStore(t)
	if err := source.SaveChatBundle(bundleFixture()); err != nil {
		t.Fatalf("SaveChatBundle: %v", err)
	}
	loaded, err := source.LoadChatBundle("c1")
	if err != nil {
		t.Fatalf("LoadChatBundle: %v", err)
	}
	if got, want := loaded.Chat.Title, "Trip"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
	if got, want := len(loaded.Messages), 2; got != want {
		t.Fatalf("messages = %d, want %d", got, want)
	}

	target := newStore(t)
	if err := target.SaveChatBundle(loaded); err != nil {
		t.Fatalf("import: %v", err)
	}
	restored, err := target.LoadChatBundle("c1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reflect.DeepEqual(normalize(restored), normalize(loaded)) {
		t.Fatalf("bundle changed in the round trip:\ngot  %+v\nwant %+v", restored, loaded)
	}

	// The path belonged to the source machine and must not survive.
	if restored.Attachments[0].SavedPath != "" {
		t.Errorf("saved path = %q, want it dropped", restored.Attachments[0].SavedPath)
	}
	if restored.Attachments[0].Generation == nil || restored.Attachments[0].Generation.Prompt != "a cat" {
		t.Errorf("generation recipe lost: %+v", restored.Attachments[0].Generation)
	}

	// The index has to be rebuilt from the chunks, not copied from the archive: the
	// rowids differ, so an FTS row keyed to the source number would point nowhere.
	hits, err := target.SearchChunks("c1", "hello", 5)
	if err != nil {
		t.Fatalf("SearchChunks: %v", err)
	}
	if len(hits) != 1 || hits[0].Chunk.DocumentID != "d1" {
		t.Fatalf("hits = %+v, want the imported chunk", hits)
	}
}

// TestSaveChatBundleReplaces is what makes an import idempotent and what an
// updated-at comparison relies on: the bundle is the whole chat, so writing it twice
// leaves one copy rather than two.
func TestSaveChatBundleReplaces(t *testing.T) {
	st := newStore(t)
	bundle := bundleFixture()
	if err := st.SaveChatBundle(bundle); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// A second version of the same chat, as a newer export would be.
	bundle.Chat.Title = "Trip, day 2"
	bundle.Messages = append(bundle.Messages, Message{
		ID: "m3", ChatID: "c1", Seq: 3, Role: "user", Content: "again",
		Status: StatusComplete, CreatedAt: bundle.Chat.UpdatedAt,
	})
	if err := st.SaveChatBundle(bundle); err != nil {
		t.Fatalf("second save: %v", err)
	}
	loaded, err := st.LoadChatBundle("c1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Messages) != 3 {
		t.Errorf("messages = %d, want 3 (replaced, not appended)", len(loaded.Messages))
	}
	if rowCount(t, st, "documents", "chat_id=?", "c1") != 1 {
		t.Errorf("documents were duplicated")
	}
	if rowCount(t, st, "chunks", "chat_id=?", "c1") != 1 {
		t.Errorf("chunks were duplicated")
	}
	if got := len(mustSearch(t, st, "hello")); got != 1 {
		t.Errorf("index rows = %d after a second save, want 1", got)
	}
}

// TestSaveChatBundleNormalisesChatID is the guard against a hand-edited archive: a
// child row naming a chat other than its own must land in the bundle's chat rather
// than write itself into a conversation the archive does not contain.
func TestSaveChatBundleNormalisesChatID(t *testing.T) {
	st := newStore(t)
	other := bundleFixture().Chat
	other.ID = "c2"
	other.Title = "Other"
	if err := st.CreateChat(other); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	bundle := bundleFixture()
	bundle.Messages[0].ChatID = "c2"
	bundle.ToolCalls[0].ChatID = "c2"
	if err := st.SaveChatBundle(bundle); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := rowCount(t, st, "messages", "chat_id=?", "c2"); got != 0 {
		t.Errorf("%d messages leaked into c2", got)
	}
	if got := rowCount(t, st, "tool_calls", "chat_id=?", "c2"); got != 0 {
		t.Errorf("%d tool calls leaked into c2", got)
	}
	if got := rowCount(t, st, "messages", "chat_id=?", "c1"); got != 2 {
		t.Errorf("messages in c1 = %d, want 2", got)
	}
}

func mustSearch(t *testing.T, st *Store, query string) []ChunkHit {
	t.Helper()
	hits, err := st.SearchChunks("c1", query, 5)
	if err != nil {
		t.Fatalf("SearchChunks: %v", err)
	}
	return hits
}
