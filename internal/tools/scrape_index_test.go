package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kichat/internal/research"
	"kichat/internal/store"
)

// bigManualText is a document too large to inline: 20 pages of contract prose with
// one sentence that only exists on the last page, so a chunk read can be shown to
// reach text the bounded excerpt never contains.
func bigManualText() string {
	var sb strings.Builder
	for page := 1; page <= 20; page++ {
		fmt.Fprintf(&sb, "## Seite %d\n", page)
		for line := 0; line < 40; line++ {
			sb.WriteString("Die Kündigungsfrist beträgt drei Monate zum Monatsende, " +
				"gerechnet ab Zugang der Kündigung beim Vermieter.\n")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("## Seite 20\nDer Abschluss dieser Anleitung lautet: Filter alle sechs Monate spülen.\n")
	return sb.String()
}

// openChatStore is a store with one chat, which the document foreign keys require.
func openChatStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	const chatID = "chat-1"
	if err := st.CreateChat(store.Chat{ID: chatID, Title: "Test"}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	return st, chatID
}

// TestAScrapedDocumentAboveTheThresholdIsIndexed is the path the old 5 MB byte cap
// made impossible: the whole document arrives, it goes into the chat's index, and
// read_pages reaches a chunk the tool result never showed.
func TestAScrapedDocumentAboveTheThresholdIsIndexed(t *testing.T) {
	st, chatID := openChatStore(t)

	text := bigManualText()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.5\n"))
	}))
	t.Cleanup(server.Close)

	scraper := research.NewScraper(st, research.ScrapeOptions{
		AllowPrivateHosts: true,
	}, func(data []byte, filename string) (string, int, error) {
		return text, 20, nil
	})
	service := New(st, nil, scraper)

	result, err := service.Execute(context.Background(), chatID, ToolScrapeURL,
		fmt.Sprintf(`{"url":%q,"max_tokens":800}`, server.URL+"/Manual.pdf"))
	if err != nil {
		t.Fatalf("scrape_url: %v", err)
	}
	if !strings.Contains(result, "indiziert") {
		t.Fatalf("a 20-page document was not indexed; the model cannot reach the rest:\n%s", result)
	}
	// The excerpt stays bounded: the point of indexing is that the model reads the
	// rest on demand, not that it receives 24k tokens at once.
	if !strings.Contains(result, "gekürzt: ca.") {
		t.Errorf("the excerpt was not bounded to the requested budget:\n%s", result)
	}

	docs, err := st.ListDocuments(chatID)
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("indexed %d documents, want 1", len(docs))
	}
	if docs[0].SourceURL != server.URL+"/Manual.pdf" {
		t.Errorf("source url = %q, want the URL that was read", docs[0].SourceURL)
	}
	if docs[0].PageCount != 20 {
		t.Errorf("page count = %d, want 20", docs[0].PageCount)
	}

	chunks, err := st.ListChunks(docs[0].ID)
	if err != nil {
		t.Fatalf("ListChunks: %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("indexed %d chunks for a %d-token document", len(chunks), docs[0].TokenCount)
	}
	last := chunks[len(chunks)-1].Ordinal
	read, err := service.Execute(context.Background(), chatID, ToolReadPages,
		fmt.Sprintf(`{"document":%q,"from":%d}`, docs[0].Title, last))
	if err != nil {
		t.Fatalf("read_pages: %v", err)
	}
	if !strings.Contains(read, "Filter alle sechs Monate") {
		t.Errorf("the last page is not reachable through read_pages:\n%s", read)
	}
}

// TestASmallPageStaysInline is the other half of the rule: indexing every page would
// cost two extra tool calls for a paragraph that used to arrive in one.
func TestASmallPageStaysInline(t *testing.T) {
	st, chatID := openChatStore(t)

	page := "<html><head><title>Kurz</title></head><body><article><h1>Kurz</h1>" +
		"<p>Ein kurzer Absatz über die Kündigungsfrist.</p></article></body></html>"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(server.Close)

	service := New(st, nil, research.NewScraper(st, research.ScrapeOptions{
		AllowPrivateHosts: true,
	}, nil))

	result, err := service.Execute(context.Background(), chatID, ToolScrapeURL,
		fmt.Sprintf(`{"url":%q}`, server.URL))
	if err != nil {
		t.Fatalf("scrape_url: %v", err)
	}
	if strings.Contains(result, "indiziert") {
		t.Errorf("a one-paragraph page was indexed instead of inlined:\n%s", result)
	}
	if !strings.Contains(result, "Kündigungsfrist") {
		t.Errorf("the page text is missing from the result:\n%s", result)
	}
	docs, err := st.ListDocuments(chatID)
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("a small page created %d documents", len(docs))
	}
}
