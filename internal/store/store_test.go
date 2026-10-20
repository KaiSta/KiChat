package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// rowCount counts rows so a test can assert on the tables no API method would
// ever show again — which is exactly where a leak hides.
func rowCount(t *testing.T, st *Store, table, where string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM "+table+" WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func chatFixture(id string) Chat {
	return Chat{
		ID: id, Title: "Test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "lmstudio", Model: "m", SystemPrompt: "sys",
		Temperature: 0.7, TopP: 0.9, AutoCompact: true,
		CompactThreshold: 0.8, KeepRecentTurns: 6,
	}
}

// TestAppendMessageAllocatesEachSequenceOnce is the regression test for the
// sequence number that was read and written as two steps: two writers could both
// read the same MAX(seq) and store the same number, which silently reorders a
// conversation and breaks the turn-boundary cuts compaction relies on. The
// allocation now happens inside one transaction.
func TestAppendMessageAllocatesEachSequenceOnce(t *testing.T) {
	st := newStore(t)
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	const writers = 24
	var wg sync.WaitGroup
	seqs := make([]int, writers)
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg, err := st.AppendMessage(Message{
				ID: fmt.Sprintf("m%02d", i), ChatID: "c1", Role: "user",
				Content: fmt.Sprintf("Nachricht %d", i), CreatedAt: time.Now(),
			})
			seqs[i], errs[i] = msg.Seq, err
		}(i)
	}
	wg.Wait()

	seen := map[int]string{}
	for i, seq := range seqs {
		if errs[i] != nil {
			t.Fatalf("writer %d: %v", i, errs[i])
		}
		if seq < 1 || seq > writers {
			t.Fatalf("writer %d got seq %d, outside 1..%d", i, seq, writers)
		}
		if other, dup := seen[seq]; dup {
			t.Fatalf("seq %d was handed to both %s and m%02d", seq, other, i)
		}
		seen[seq] = fmt.Sprintf("m%02d", i)
	}

	messages, err := st.ListMessages("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != writers {
		t.Fatalf("stored %d messages, want %d", len(messages), writers)
	}
	for i, m := range messages {
		if m.Seq != i+1 {
			t.Fatalf("message %d has seq %d, want %d", i, m.Seq, i+1)
		}
	}
}

func TestChatAndMessageRoundTrip(t *testing.T) {
	st := newStore(t)
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	msg, err := st.AppendMessage(Message{
		ID: "m1", ChatID: "c1", Role: "user", Content: "Hallo",
		Status: StatusComplete, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if msg.Seq != 1 {
		t.Fatalf("first message got seq %d, want 1", msg.Seq)
	}
	second, err := st.AppendMessage(Message{
		ID: "m1b", ChatID: "c1", Role: "assistant", Content: "Hallo zurück",
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if second.Seq != 2 {
		t.Fatalf("second message got seq %d, want 2", second.Seq)
	}
	if second.Status != StatusComplete {
		t.Fatalf("an empty status should default to complete, got %q", second.Status)
	}

	// Round-trip including the numeric fields the UI reads.
	msg.Status = StatusInterrupted
	msg.Content = "Hallo, teilweise"
	msg.TokensPerSecond = 31.25
	msg.TTFTMs = 250
	msg.GenerationMs = 640
	msg.PromptTokens = 42
	msg.CompletionTokens = 7
	msg.StatsSource = "lmstudio"
	msg.StopReason = "interrupted"
	if err := st.UpdateMessage(msg); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}

	got, err := st.GetMessage("m1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.Content != "Hallo, teilweise" || got.Status != StatusInterrupted {
		t.Errorf("message = %+v", got)
	}
	if got.TokensPerSecond != 31.25 || got.TTFTMs != 250 || got.GenerationMs != 640 {
		t.Errorf("stats did not round-trip: %+v", got)
	}
	if got.PromptTokens != 42 || got.CompletionTokens != 7 || got.StatsSource != "lmstudio" {
		t.Errorf("usage did not round-trip: %+v", got)
	}

	c, err := st.GetChat("c1")
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if !c.AutoCompact || c.CompactThreshold != 0.8 || c.KeepRecentTurns != 6 {
		t.Errorf("chat flags did not round-trip: %+v", c)
	}
}

// TestFTSSearchRanksByRelevance is the document-search path the model relies on.
func TestFTSSearchRanksByRelevance(t *testing.T) {
	st := newStore(t)
	if !st.FTS {
		t.Skip("FTS5 unavailable in this build")
	}
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatal(err)
	}
	doc := Document{ID: "d1", ChatID: "c1", Title: "mietrecht.pdf", Text: "…", CreatedAt: time.Now()}
	if err := st.CreateDocument(doc); err != nil {
		t.Fatal(err)
	}

	if err := st.InsertChunks([]Chunk{
		{DocumentID: "d1", ChatID: "c1", Ordinal: 0, Heading: "Einleitung", Page: 1,
			Text: "Dieses Dokument beschreibt die Kündigungsfristen im deutschen Mietrecht."},
		{DocumentID: "d1", ChatID: "c1", Ordinal: 1, Heading: "Fristen", Page: 2,
			Text: "Die ordentliche Kündigung hat eine Frist von drei Monaten. Mietrecht Mietrecht Mietrecht."},
		{DocumentID: "d1", ChatID: "c1", Ordinal: 2, Heading: "Anhang", Page: 9,
			Text: "Formulierungen für ein Anschreiben an den Vermieter."},
		{DocumentID: "d1", ChatID: "c1", Ordinal: 3, Heading: "Fremd", Page: 10,
			Text: "Ein Eintrag über ganz andere Themen wie Steuerrecht und Vereinswesen."},
	}); err != nil {
		t.Fatalf("InsertChunks: %v", err)
	}

	hits, err := st.SearchChunks("c1", "Mietrecht", 5)
	if err != nil {
		t.Fatalf("SearchChunks: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits for a term that appears in the corpus")
	}
	for _, h := range hits {
		if h.Chunk.ChatID != "c1" {
			t.Errorf("hit from the wrong chat: %+v", h.Chunk)
		}
		if h.Document != "mietrecht.pdf" {
			t.Errorf("document title not joined: %q", h.Document)
		}
	}
	// The chunk mentioning the term repeatedly must outrank a single mention.
	if hits[0].Chunk.Ordinal != 1 {
		t.Errorf("top hit = chunk %d, want chunk 1 (the densest match)", hits[0].Chunk.Ordinal)
	}
	// Content with no match must be excluded.
	for _, h := range hits {
		if h.Chunk.Ordinal == 2 {
			t.Error("a chunk without the search term was returned")
		}
	}

	// Chat scoping must exclude other chats entirely.
	if err := st.CreateChat(chatFixture("c2")); err != nil {
		t.Fatal(err)
	}
	other := Document{ID: "d2", ChatID: "c2", Title: "other.txt", Text: "…", CreatedAt: time.Now()}
	if err := st.CreateDocument(other); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertChunks([]Chunk{{DocumentID: "d2", ChatID: "c2", Ordinal: 0,
		Text: "Auch hier geht es um Mietrecht."}}); err != nil {
		t.Fatal(err)
	}
	scoped, err := st.SearchChunks("c1", "Mietrecht", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range scoped {
		if h.Chunk.ChatID != "c1" {
			t.Errorf("chat scoping leaked: %+v", h.Chunk)
		}
	}
}

// TestFTSQuerySurvivesHostileInput keeps user text from breaking the MATCH
// expression: FTS5 has its own operator syntax where a bare "-" or "*" is an
// error, not a search term.
func TestFTSQuerySurvivesHostileInput(t *testing.T) {
	st := newStore(t)
	if !st.FTS {
		t.Skip("FTS5 unavailable in this build")
	}
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDocument(Document{ID: "d1", ChatID: "c1", Title: "t", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertChunks([]Chunk{{DocumentID: "d1", ChatID: "c1", Ordinal: 0,
		Text: "Der Vertrag über 1.500,00 € gilt ab 2026."}}); err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{
		`-`, `*`, `"`, `NOT`, `AND OR`, `€`, `1.500,00`, `Mietrecht AND`,
		`a`, `""`, `+++`, `;;;`, `((((`, `x*/y`,
	} {
		if _, err := st.SearchChunks("c1", query, 5); err != nil {
			t.Errorf("query %q produced an error: %v", query, err)
		}
	}
	// A real term with surrounding punctuation must still match.
	hits, err := st.SearchChunks("c1", "Vertrag gilt?", 5)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(hits) == 0 {
		t.Error("expected a match for a term wrapped in punctuation")
	}
}

func TestDeleteChatCascadesAndRemovesIndexEntries(t *testing.T) {
	st := newStore(t)
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMessage(Message{ID: "m1", ChatID: "c1", Seq: 1, Role: "user",
		Content: "x", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDocument(Document{ID: "d1", ChatID: "c1", Title: "t",
		Text: "Mietrecht", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if st.FTS {
		if err := st.InsertChunks([]Chunk{{DocumentID: "d1", ChatID: "c1", Ordinal: 0,
			Text: "Mietrecht in Deutschland."}}); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.DeleteChat("c1"); err != nil {
		t.Fatalf("DeleteChat: %v", err)
	}
	if _, err := st.GetChat("c1"); err == nil {
		t.Error("chat still present after delete")
	}
	messages, err := st.ListMessages("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Errorf("messages survived the cascade: %+v", messages)
	}
	if st.FTS {
		hits, err := st.SearchChunks("", "Mietrecht", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 0 {
			t.Errorf("index entries survived the delete: %+v", hits)
		}
	}

	// Documents and chunks hold no foreign key to chats, so they are removed by
	// hand. Asserting on the rows rather than on a read result matters: these two
	// tables are the largest in the database, and a chat that keeps them leaks the
	// extracted text for good.
	if n := rowCount(t, st, "documents", "chat_id=?", "c1"); n != 0 {
		t.Errorf("%d documents survived the delete", n)
	}
	if n := rowCount(t, st, "chunks", "chat_id=?", "c1"); n != 0 {
		t.Errorf("%d chunks survived the delete", n)
	}
}

// TestDeleteAttachmentRemovesItsDocument pins the order the rows go in: the
// document carries the chunks, so it must not outlive the attachment pointing at
// it, and deleting something that is already gone must stay quiet.
func TestDeleteAttachmentRemovesItsDocument(t *testing.T) {
	st := newStore(t)
	now := time.Now()
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDocument(Document{ID: "d1", ChatID: "c1", Title: "t",
		Text: "Inhalt", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAttachment(Attachment{ID: "a1", ChatID: "c1", Kind: "file",
		Filename: "f.txt", Placement: PlacementIndexed, DocumentID: "d1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if st.FTS {
		if err := st.InsertChunks([]Chunk{{DocumentID: "d1", ChatID: "c1", Ordinal: 0,
			Text: "Inhalt"}}); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.DeleteAttachment("a1"); err != nil {
		t.Fatalf("DeleteAttachment: %v", err)
	}
	if n := rowCount(t, st, "attachments", "id=?", "a1"); n != 0 {
		t.Error("the attachment survived its own deletion")
	}
	if n := rowCount(t, st, "documents", "id=?", "d1"); n != 0 {
		t.Error("the document outlived its attachment")
	}
	if n := rowCount(t, st, "chunks", "document_id=?", "d1"); n != 0 {
		t.Error("the chunks outlived their document")
	}

	// A second click on delete, or a stale view, must not report an error.
	if err := st.DeleteAttachment("a1"); err != nil {
		t.Errorf("deleting an attachment that is already gone: %v", err)
	}
}

// TestDeleteMessagesFromSeqRemovesOwnedDocuments: regenerating deletes the
// trailing assistant message, and an attachment on the removed messages must take
// its document and chunks with it. Deleting only the attachment row left the
// extracted text unreachable — the same leak DeleteChat and DeleteAttachment fixed.
func TestDeleteMessagesFromSeqRemovesOwnedDocuments(t *testing.T) {
	st := newStore(t)
	now := time.Now()
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMessage(Message{ID: "m1", ChatID: "c1", Seq: 1, Role: "assistant",
		Content: "Antwort", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDocument(Document{ID: "d1", ChatID: "c1", Title: "t",
		Text: "Inhalt", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAttachment(Attachment{ID: "a1", ChatID: "c1", MessageID: "m1",
		Kind: "file", Filename: "f.txt", Placement: PlacementIndexed, DocumentID: "d1",
		CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if st.FTS {
		if err := st.InsertChunks([]Chunk{{DocumentID: "d1", ChatID: "c1", Ordinal: 0, Text: "Inhalt"}}); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.DeleteMessagesFromSeq("c1", 1); err != nil {
		t.Fatalf("DeleteMessagesFromSeq: %v", err)
	}
	if n := rowCount(t, st, "messages", "id=?", "m1"); n != 0 {
		t.Error("the message survived its own deletion")
	}
	if n := rowCount(t, st, "attachments", "id=?", "a1"); n != 0 {
		t.Error("the attachment survived the message it belonged to")
	}
	if n := rowCount(t, st, "documents", "id=?", "d1"); n != 0 {
		t.Error("the document outlived the attachment it belonged to")
	}
	if n := rowCount(t, st, "chunks", "document_id=?", "d1"); n != 0 {
		t.Error("the chunks outlived their document")
	}
}

// TestPruneExpiredCachesDropsOnlyExpiredRows draws the line between "reads as
// expired" and "is gone". GetSearchCache already hides an expired row, so only a
// count proves the row is removed — that is the difference between a cache and an
// archive, and nothing else in the store ever deletes from these two tables.
func TestPruneExpiredCachesDropsOnlyExpiredRows(t *testing.T) {
	st := newStore(t)
	entries := []SearchCacheEntry{
		{QueryHash: "fresh", Provider: "brave", Query: "q", ResponseJSON: "[]",
			CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)},
		{QueryHash: "stale", Provider: "brave", Query: "q", ResponseJSON: "[]",
			CreatedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)},
	}
	for _, e := range entries {
		if err := st.PutSearchCache(e); err != nil {
			t.Fatal(err)
		}
	}
	page := ScrapeCacheEntry{URLHash: "u1", URL: "https://x.de", Title: "T",
		ContentMD: strings.Repeat("Seiteninhalt ", 50), Tokens: 3,
		FetchedAt: time.Now().Add(-48 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)}
	if err := st.PutScrapeCache(page); err != nil {
		t.Fatal(err)
	}

	removed, err := st.PruneExpiredCaches()
	if err != nil {
		t.Fatalf("PruneExpiredCaches: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d rows, want 2: one stale search entry and one stale page", removed)
	}
	if n := rowCount(t, st, "search_cache", "query_hash=?", "stale"); n != 0 {
		t.Error("the expired search entry is still stored")
	}
	if n := rowCount(t, st, "scrape_cache", "url_hash=?", "u1"); n != 0 {
		t.Error("the expired page is still stored")
	}
	if n := rowCount(t, st, "search_cache", "query_hash=?", "fresh"); n != 1 {
		t.Error("a cache entry that is still valid was pruned")
	}

	// The app prunes on every start, so a second run has to find nothing.
	again, err := st.PruneExpiredCaches()
	if err != nil {
		t.Fatalf("second PruneExpiredCaches: %v", err)
	}
	if again != 0 {
		t.Errorf("a second prune removed %d rows, want 0", again)
	}
}

// TestDeleteCompactionKeepsOriginalMessages is the core promise of the
// compaction design: folding is a reading instruction, never a deletion.
func TestDeleteCompactionKeepsOriginalMessages(t *testing.T) {
	st := newStore(t)
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		if err := st.CreateMessage(Message{
			ID: "m" + string(rune('0'+i)), ChatID: "c1", Seq: i, Role: role,
			Content: "Inhalt", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateCompaction(Compaction{
		ID: "k1", ChatID: "c1", Trigger: "auto", FromSeq: 1, ToSeq: 3,
		Summary: "Zusammenfassung", TokensBefore: 500, TokensAfter: 80,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	compactions, err := st.ListCompactions("c1")
	if err != nil || len(compactions) != 1 {
		t.Fatalf("ListCompactions = %+v, %v", compactions, err)
	}
	if compactions[0].TokensBefore != 500 || compactions[0].TokensAfter != 80 {
		t.Errorf("compaction did not round-trip: %+v", compactions[0])
	}

	if err := st.DeleteCompactionsFrom("c1", 3); err != nil {
		t.Fatalf("DeleteCompactionsFrom: %v", err)
	}
	messages, err := st.ListMessages("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want 4: deleting a compaction must not delete history", len(messages))
	}
}

// TestDeleteCompactionsFromDropsTheStaleTail pins that re-opening history
// re-opens all of it: a compaction summarised on top of the deleted one is stale
// and goes with it, while earlier ones and other chats survive.
func TestDeleteCompactionsFromDropsTheStaleTail(t *testing.T) {
	st := newStore(t)
	for _, id := range []string{"c1", "c2"} {
		if err := st.CreateChat(chatFixture(id)); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range []Compaction{
		{ID: "k1", ChatID: "c1", Trigger: "auto", FromSeq: 1, ToSeq: 3, Summary: "a"},
		{ID: "k2", ChatID: "c1", Trigger: "auto", FromSeq: 1, ToSeq: 10, Summary: "a+b"},
		{ID: "k3", ChatID: "c1", Trigger: "auto", FromSeq: 1, ToSeq: 20, Summary: "a+b+c"},
		{ID: "other", ChatID: "c2", Trigger: "auto", FromSeq: 1, ToSeq: 10, Summary: "x"},
	} {
		c.CreatedAt = time.Now()
		if err := st.CreateCompaction(c); err != nil {
			t.Fatalf("CreateCompaction %s: %v", c.ID, err)
		}
	}

	// Deleting the middle compaction takes the later one with it.
	if err := st.DeleteCompactionsFrom("c1", 10); err != nil {
		t.Fatalf("DeleteCompactionsFrom: %v", err)
	}

	remaining, err := st.ListCompactions("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != "k1" {
		t.Errorf("remaining compactions = %+v, want only k1", remaining)
	}

	other, err := st.ListCompactions("c2")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].ID != "other" {
		t.Errorf("another chat's compactions were touched: %+v", other)
	}
}

func TestCountersResetWhenPeriodChanges(t *testing.T) {
	st := newStore(t)
	v, err := st.IncrCounter("search:brave", "2026-10")
	if err != nil || v != 1 {
		t.Fatalf("first IncrCounter = %d, %v", v, err)
	}
	if v, _ := st.IncrCounter("search:brave", "2026-10"); v != 2 {
		t.Fatalf("second IncrCounter = %d, want 2", v)
	}
	if got := st.Counter("search:brave", "2026-10"); got != 2 {
		t.Fatalf("Counter = %d, want 2", got)
	}
	// A new month starts from zero.
	if v, _ := st.IncrCounter("search:brave", "2026-11"); v != 1 {
		t.Fatalf("rollover IncrCounter = %d, want 1", v)
	}
	// Asking for the old period must not report the new month's value.
	if got := st.Counter("search:brave", "2026-10"); got != 0 {
		t.Fatalf("stale period reported %d, want 0", got)
	}
}

func TestCacheHonoursExpiry(t *testing.T) {
	st := newStore(t)
	fresh := SearchCacheEntry{
		QueryHash: "h1", Provider: "brave", Query: "q", ResponseJSON: "[]",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := st.PutSearchCache(fresh); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetSearchCache("h1"); !ok {
		t.Error("a fresh cache entry should be returned")
	}

	stale := SearchCacheEntry{
		QueryHash: "h2", Provider: "brave", Query: "q", ResponseJSON: "[]",
		CreatedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour),
	}
	if err := st.PutSearchCache(stale); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetSearchCache("h2"); ok {
		t.Error("an expired cache entry must not be returned")
	}
	if _, ok := st.GetSearchCache("never-stored"); ok {
		t.Error("an unknown key must not hit")
	}

	scrape := ScrapeCacheEntry{
		URLHash: "u1", URL: "https://x.de", Title: "T", ContentMD: "Inhalt",
		Tokens: 3, FetchedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := st.PutScrapeCache(scrape); err != nil {
		t.Fatal(err)
	}
	got, ok := st.GetScrapeCache("u1")
	if !ok || got.ContentMD != "Inhalt" || got.Tokens != 3 {
		t.Fatalf("scrape cache = %+v, ok=%v", got, ok)
	}
}

func TestSettingsRoundTripAndModelOverrides(t *testing.T) {
	st := newStore(t)
	if found, err := st.GetSetting("missing", &struct{}{}); err != nil || found {
		t.Fatalf("GetSetting(missing) = %v, %v", found, err)
	}

	settings := map[string]any{"language": "de", "threshold": 0.8}
	if err := st.PutSetting("settings", settings); err != nil {
		t.Fatal(err)
	}
	var readBack map[string]any
	found, err := st.GetSetting("settings", &readBack)
	if err != nil || !found {
		t.Fatalf("GetSetting = %v, %v", found, err)
	}
	if readBack["language"] != "de" {
		t.Errorf("settings did not round-trip: %+v", readBack)
	}

	type override struct {
		ContextLimit int `json:"contextLimit,omitempty"`
	}
	if err := st.PutModelOverride("custom", "local-7b", override{ContextLimit: 32768}); err != nil {
		t.Fatal(err)
	}
	overrides, err := st.ModelOverrides()
	if err != nil {
		t.Fatal(err)
	}
	if raw, ok := overrides["custom/local-7b"]; !ok || raw == "" {
		t.Fatalf("override missing: %+v", overrides)
	}
	// An empty override clears the row.
	if err := st.PutModelOverride("custom", "local-7b", override{}); err != nil {
		t.Fatal(err)
	}
	overrides, _ = st.ModelOverrides()
	if _, ok := overrides["custom/local-7b"]; ok {
		t.Error("an empty override should have cleared the stored value")
	}
}

// TestToolCallCancelledStateSurvives proves the cancellation bookkeeping the
// orphan guard depends on is actually persisted.
func TestToolCallCancelledStateSurvives(t *testing.T) {
	st := newStore(t)
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatal(err)
	}
	record := ToolCallRecord{
		ID: "call-a", MessageID: "m1", ChatID: "c1", Name: "search_web",
		ArgsJSON: `{"query":"x"}`, Status: ToolPending, CreatedAt: time.Now(),
	}
	if err := st.UpsertToolCall(record); err != nil {
		t.Fatal(err)
	}
	record.Status = ToolCancelled
	record.ResultJSON = `{"content":"abgebrochen"}`
	if err := st.UpsertToolCall(record); err != nil {
		t.Fatal(err)
	}

	records, err := st.ListToolCalls("c1")
	if err != nil || len(records) != 1 {
		t.Fatalf("ListToolCalls = %+v, %v", records, err)
	}
	if records[0].Status != ToolCancelled {
		t.Errorf("status = %q, want cancelled", records[0].Status)
	}
	if records[0].ResultJSON == "" {
		t.Error("the cancelled result was not stored")
	}
}

// TestAToolCallKeepsWhatItCostAndWhatWasRead is the row a reopened chat builds its
// cards from, so the two sizes and the cause have to survive the write: the card can
// only say "12,345 → 640 tokens, condensed" if the record still holds all three after
// a restart, and nothing else in the schema does.
func TestAToolCallKeepsWhatItCostAndWhatWasRead(t *testing.T) {
	st := newStore(t)
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertToolCall(ToolCallRecord{
		ID: "call-a", MessageID: "m1", ChatID: "c1", Name: "scrape_url",
		Status: ToolOK, SourceTokens: 12345, ContextTokens: 640, Condensed: true,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// A second call that reported nothing, which is every row written before migration
	// 8: zero is the absence of a measurement, not a result of no size.
	if err := st.UpsertToolCall(ToolCallRecord{
		ID: "call-b", MessageID: "m1", ChatID: "c1", Name: "search_web",
		Status: ToolOK, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// And the outcome that has no size of its own: the model was handed the notice.
	if err := st.UpsertToolCall(ToolCallRecord{
		ID: "call-c", MessageID: "m1", ChatID: "c1", Name: "search_web",
		Status: ToolOK, SourceTokens: 4374, ContextTokens: 41, Withheld: true,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	records, err := st.ListToolCalls("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("tool calls = %d, want three", len(records))
	}
	byID := map[string]ToolCallRecord{}
	for _, record := range records {
		byID[record.ID] = record
	}
	condensed := byID["call-a"]
	if condensed.SourceTokens != 12345 || condensed.ContextTokens != 640 || !condensed.Condensed {
		t.Errorf("the condensation was not stored: %+v", condensed)
	}
	plain := byID["call-b"]
	if plain.SourceTokens != 0 || plain.ContextTokens != 0 || plain.Condensed || plain.Withheld {
		t.Errorf("an unmeasured call reports a size: %+v", plain)
	}
	withheld := byID["call-c"]
	if !withheld.Withheld || withheld.SourceTokens != 4374 {
		t.Errorf("the withheld result was not stored as one: %+v", withheld)
	}
}

// TestAnAnswerKeepsItsSources is what a citation in a reopened chat resolves against:
// the numbers are written once the turn is over, and the streaming updates that rewrite
// the same row afterwards must not take them away again.
func TestAnAnswerKeepsItsSources(t *testing.T) {
	st := newStore(t)
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatal(err)
	}
	msg, err := st.AppendMessage(Message{
		ID: "m1", ChatID: "c1", Role: "assistant", Content: "laut Quelle [1] …",
	})
	if err != nil {
		t.Fatal(err)
	}
	sources := []Source{
		{Index: 1, Title: "Wärmespeicher", URL: "https://example.de/a"},
		{Index: 2, Title: "Preise", URL: "https://example.de/b"},
	}
	if err := st.SetMessageSources(msg.ID, sources); err != nil {
		t.Fatal(err)
	}

	// The stream writes the row again as the answer finishes; that update carries no
	// sources and must leave the stored list alone.
	msg.Content = "laut Quelle [1] … fertig"
	if err := st.UpdateMessage(msg); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetMessage(msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sources) != 2 {
		t.Fatalf("sources = %+v, want the two written after the turn", got.Sources)
	}
	if got.Sources[1].Index != 2 || got.Sources[1].URL != "https://example.de/b" ||
		got.Sources[1].Title != "Preise" {
		t.Errorf("source 2 was not stored as written: %+v", got.Sources[1])
	}

	// A message from before citations existed reads as having none.
	plain, err := st.AppendMessage(Message{ID: "m2", ChatID: "c1", Role: "assistant", Content: "alt"})
	if err != nil {
		t.Fatal(err)
	}
	if again, err := st.GetMessage(plain.ID); err != nil || len(again.Sources) != 0 {
		t.Errorf("a message without sources reports %+v (%v)", again.Sources, err)
	}
}

// TestACondensationIsReusedUntilItExpires pins the cache a second read of the same
// page depends on: the entry is found by its key, carries the summary and what it was
// made from, and stops being served once it has expired — an entry nobody reads again
// is still pruned rather than kept for the life of the installation.
func TestACondensationIsReusedUntilItExpires(t *testing.T) {
	st := newStore(t)
	entry := DigestCacheEntry{
		Key: "k1", Model: "small", SourceTokens: 19800,
		Summary: "- Kurzfassung", DigestTokens: 120,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	if err := st.PutDigestCache(entry); err != nil {
		t.Fatalf("PutDigestCache: %v", err)
	}
	got, ok := st.GetDigestCache("k1")
	if !ok {
		t.Fatal("a stored condensation was not found")
	}
	if got.Summary != entry.Summary || got.SourceTokens != 19800 || got.Model != "small" {
		t.Errorf("GetDigestCache = %+v, want the stored entry", got)
	}
	if _, ok := st.GetDigestCache("never-written"); ok {
		t.Error("an absent key reported a hit")
	}

	// An expired entry is a miss, and the start-up prune removes it for good: reads
	// treat it as absent either way, but nothing else would ever delete the row.
	if err := st.PutDigestCache(DigestCacheEntry{
		Key: "k2", Model: "small", Summary: "alt",
		CreatedAt: time.Now().Add(-48 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("PutDigestCache: %v", err)
	}
	if _, ok := st.GetDigestCache("k2"); ok {
		t.Error("an expired condensation was served")
	}
	if _, err := st.PruneExpiredCaches(); err != nil {
		t.Fatalf("PruneExpiredCaches: %v", err)
	}
	if n := rowCount(t, st, "digest_cache", "key = ?", "k2"); n != 0 {
		t.Errorf("the expired condensation survived the prune (%d rows)", n)
	}
}

// TestAnExistingInstallationGainsTheGenerationColumn is the upgrade path, and it is
// asserted rather than assumed because getting it wrong is silent and total.
//
// Migrations resume at the version a database already reports, so a new entry only
// reaches an existing installation if it is *appended*. One inserted earlier in the
// slice is skipped forever on every database that predates it: the version advances past
// it, no error is raised, and the first attachment read then fails on a column that was
// never added. This test builds exactly that older database — the current schema with
// the column dropped and the version rolled back — and reopens it.
func TestAnExistingInstallationGainsTheGenerationColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Every column a later migration adds, not just the first one: the version below is
	// rolled back to 3, so a real database from that version has none of them — and
	// dropping only one leaves an `ALTER TABLE ... ADD COLUMN` in the runner failing on a
	// column that is already there, which is how this test broke when the second one
	// arrived. A column added by migration N is dropped here in the same commit that adds it.
	for _, column := range []struct{ table, name string }{
		{"attachments", "generation_json"},
		{"attachments", "saved_path"},
		{"attachments", "derivation_json"},
		{"tool_calls", "source_tokens"},
		{"tool_calls", "context_tokens"},
		{"tool_calls", "condensed"},
		{"tool_calls", "withheld"},
		{"messages", "sources_json"},
	} {
		if _, err := st.DB().Exec(`ALTER TABLE ` + column.table + ` DROP COLUMN ` + column.name); err != nil {
			t.Fatalf("drop %s.%s to build the older schema: %v", column.table, column.name, err)
		}
	}
	if _, err := st.DB().Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatalf("roll the version back: %v", err)
	}
	// A row written before the column existed has to survive the upgrade too.
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO attachments
  (id,chat_id,message_id,kind,filename,mime,size_bytes,sha256,pages,token_estimate,
   placement,document_id,warning,created_at)
  VALUES ('a1','c1','m1','file','alt.txt','text/plain',3,'',0,1,'inline','','',0)`); err != nil {
		t.Fatalf("insert a pre-upgrade row: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen an older database: %v", err)
	}
	defer reopened.Close()

	var version int
	if err := reopened.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Errorf("user_version = %d after the upgrade, want %d", version, len(migrations))
	}
	// The old row is readable, and having no recipe is what "an attached file" means.
	old, err := reopened.GetAttachment("a1")
	if err != nil {
		t.Fatalf("the pre-upgrade row is unreadable after the upgrade: %v", err)
	}
	if old.Generation != nil {
		t.Errorf("a file attached before the column existed reports a recipe: %+v", old.Generation)
	}
	// And the second new column: an attachment that predates it has no recorded export, which
	// is the honest reading — nothing recorded a copy, so the app cannot claim there is one.
	if old.SavedPath != "" {
		t.Errorf("a file attached before the export column existed reports an export: %q", old.SavedPath)
	}
}

// TestTheGenerationRecipeRoundTrips keeps the column that makes a picture repeatable from
// quietly losing a field. The recipe is stored as JSON and decoded by name, so a field
// added to GenerationRecipe with a missing or misspelled tag vanishes here rather than in
// the UI, where it would look like a redraw that ignored one of its own settings.
func TestTheGenerationRecipeRoundTrips(t *testing.T) {
	st := newStore(t)
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatal(err)
	}
	recipe := &GenerationRecipe{
		Prompt: "a red fox in deep snow", NegativePrompt: "blurry", Width: 768, Height: 512,
		Steps: 25, Sampler: "Euler a", CfgScale: 6.5, Seed: 4242, Model: "sdxl.safetensors",
		Hires: true, HiresScale: 1.5, HiresDenoise: 0.4, DurationMs: 4321,
	}
	if err := st.CreateAttachment(Attachment{
		ID: "a1", ChatID: "c1", MessageID: "m1", Kind: "image",
		Filename: "a-red-fox-1.png", Mime: "image/png", SizeBytes: 12,
		TokenEstimate: 1024, Placement: PlacementInline, DocumentID: "d1",
		CreatedAt: time.Now(), Generation: recipe,
	}); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}

	got, err := st.GetAttachment("a1")
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if got.Generation == nil {
		t.Fatal("the recipe did not survive the write")
	}
	if *got.Generation != *recipe {
		t.Errorf("recipe came back as %+v, want %+v", *got.Generation, *recipe)
	}

	// The chat view renders a message's attachments from ListAttachments, so the list has
	// to carry the recipe as well as the single read does.
	list, err := st.ListAttachments("c1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListAttachments = %+v, %v", list, err)
	}
	if list[0].Generation == nil || list[0].Generation.Seed != 4242 {
		t.Errorf("the listed attachment lost its recipe: %+v", list[0].Generation)
	}

	// A file the user attached has no recipe at all, which is a different state from a
	// picture whose recipe happens to be empty: the first must not offer a redraw.
	if err := st.CreateAttachment(Attachment{
		ID: "a2", ChatID: "c1", Kind: "file", Filename: "notes.txt",
		TokenEstimate: 3, Placement: PlacementInline, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	plain, err := st.GetAttachment("a2")
	if err != nil {
		t.Fatal(err)
	}
	if plain.Generation != nil {
		t.Errorf("a text attachment reports a generation recipe: %+v", plain.Generation)
	}
}

// TestACorruptRecipeFailsTheRead pins the choice made for a recipe that will not parse.
//
// It is JSON this app wrote, so a value that does not decode means something outside the
// app edited the row. Reading it as "no recipe" would make the picture silently
// un-repeatable with nothing to explain why, which is the one outcome the column exists to
// prevent — so the read reports the problem instead, as config.Load does for its own
// stored JSON.
func TestACorruptRecipeFailsTheRead(t *testing.T) {
	st := newStore(t)
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAttachment(Attachment{
		ID: "a1", ChatID: "c1", Kind: "image", Filename: "x.png",
		TokenEstimate: 1, Placement: PlacementInline, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE attachments SET generation_json='{not json' WHERE id='a1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAttachment("a1"); err == nil {
		t.Error("an unparseable recipe was accepted as \"no recipe\"")
	}
}

// TestTheExportPathRoundTrips is what the pictures panel reads, and the trap it guards is the
// column list: `saved_path` travelled through `attachmentColumns` when it was added, and a
// rename or a missing entry there fails silently — the row is written, the reader gets an
// empty string, and every exported picture then reads as one its owner has no copy of, which
// is the state the clear button deletes.
func TestTheExportPathRoundTrips(t *testing.T) {
	st := newStore(t)
	now := time.Now()
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAttachment(Attachment{ID: "a1", ChatID: "c1", Kind: "image",
		Filename: "a-red-fox.png", Mime: "image/png", SizeBytes: 2048,
		Placement: PlacementInline, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	// A picture nobody exported yet: empty, which is not an error and not a zero value to
	// be normalised away — "no copy outside KiChat" is a fact about the picture.
	fresh, err := st.GetAttachment("a1")
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if fresh.SavedPath != "" {
		t.Errorf("SavedPath = %q on a picture that was never exported, want empty", fresh.SavedPath)
	}

	if err := st.SetAttachmentSavedPath("a1", "/home/user/Pictures/a-red-fox.png"); err != nil {
		t.Fatalf("SetAttachmentSavedPath: %v", err)
	}
	exported, err := st.GetAttachment("a1")
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if exported.SavedPath != "/home/user/Pictures/a-red-fox.png" {
		t.Errorf("SavedPath = %q, want the exported path", exported.SavedPath)
	}
	// The note is not an edit of the attachment, so the fields a reader may edit must be
	// untouched by it — and it must survive the update that follows a placement switch.
	if err := st.UpdateAttachment(exported); err != nil {
		t.Fatalf("UpdateAttachment: %v", err)
	}
	if again, err := st.GetAttachment("a1"); err != nil {
		t.Fatalf("GetAttachment: %v", err)
	} else if again.SavedPath != exported.SavedPath {
		t.Errorf("SavedPath = %q after an update, want %q", again.SavedPath, exported.SavedPath)
	}

	// An empty path is allowed and means the copy is gone as far as this app knows.
	if err := st.SetAttachmentSavedPath("a1", ""); err != nil {
		t.Fatalf("SetAttachmentSavedPath(\"\"): %v", err)
	}
	if again, err := st.GetAttachment("a1"); err != nil {
		t.Fatalf("GetAttachment: %v", err)
	} else if again.SavedPath != "" {
		t.Errorf("SavedPath = %q after clearing it, want empty", again.SavedPath)
	}

	// And the whole-table reader the panel uses sees the same row.
	all, err := st.AllAttachments()
	if err != nil {
		t.Fatalf("AllAttachments: %v", err)
	}
	if len(all) != 1 || all[0].ID != "a1" {
		t.Errorf("AllAttachments = %+v, want the one picture", all)
	}
	// Never nil: a slice that marshals to null is the bug this codebase has shipped
	// twice, and the caller here is a panel that walks it.
	empty, err := newStore(t).AllAttachments()
	if err != nil {
		t.Fatalf("AllAttachments on an empty store: %v", err)
	}
	if empty == nil {
		t.Error("AllAttachments returned nil for an empty store; the caller iterates it")
	}
}
