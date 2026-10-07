package store

import (
	"path/filepath"
	"testing"
	"time"
)

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

func TestChatAndMessageRoundTrip(t *testing.T) {
	st := newStore(t)
	if err := st.CreateChat(chatFixture("c1")); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	seq, err := st.NextSeq("c1")
	if err != nil || seq != 1 {
		t.Fatalf("NextSeq = %d, %v; want 1", seq, err)
	}
	msg := Message{
		ID: "m1", ChatID: "c1", Seq: seq, Role: "user", Content: "Hallo",
		Status: StatusComplete, CreatedAt: time.Now(),
	}
	if err := st.CreateMessage(msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	if seq, _ := st.NextSeq("c1"); seq != 2 {
		t.Fatalf("NextSeq after insert = %d, want 2", seq)
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

	if err := st.DeleteCompaction("k1"); err != nil {
		t.Fatalf("DeleteCompaction: %v", err)
	}
	messages, err := st.ListMessages("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want 4: deleting a compaction must not delete history", len(messages))
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
