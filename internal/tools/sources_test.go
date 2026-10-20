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

// TestSourceNumbersAreOneSequencePerTurn is the rule citations rest on: a search that
// follows another continues the numbering instead of restarting at [1], a URL seen twice
// keeps its first number, and the list reads back in the order the model read it.
func TestSourceNumbersAreOneSequencePerTurn(t *testing.T) {
	list := NewSourceList()

	if got := list.Add("Erster", "https://example.de/a"); got != 1 {
		t.Fatalf("first source numbered %d, want 1", got)
	}
	if got := list.Add("Zweiter", "https://example.de/b"); got != 2 {
		t.Fatalf("second source numbered %d, want 2", got)
	}
	// The same page found by another query is one source, not two.
	if got := list.Add("Erster, anders geschrieben", "https://example.de/a"); got != 1 {
		t.Fatalf("a repeated URL was numbered %d, want its first 1", got)
	}
	if got := list.Add("Ohne URL", ""); got != 3 {
		t.Fatalf("a source with no URL numbered %d, want 3", got)
	}

	all := list.All()
	if len(all) != 3 {
		t.Fatalf("sources = %d, want three: %+v", len(all), all)
	}
	for i, source := range all {
		if source.Index != i+1 {
			t.Errorf("source %d carries index %d", i, source.Index)
		}
	}
	if all[0].Title != "Erster" || all[0].URL != "https://example.de/a" {
		t.Errorf("the repeat rewrote the first entry: %+v", all[0])
	}

	// All is a copy: the answer is written from it after the last tool call.
	all[0].Title = "geändert"
	if again := list.All(); again[0].Title != "Erster" {
		t.Error("mutating the returned slice changed the registry")
	}

	// A call outside a turn has no list and must not panic.
	var absent *SourceList
	if got := absent.Add("t", "u"); got != 0 {
		t.Errorf("a nil list numbered a source %d", got)
	}
	if got := absent.All(); got != nil {
		t.Errorf("a nil list returned %+v", got)
	}
}

// braveStub serves two results per query, named after the query, so a second search
// returns different pages and the continuation of the numbering is what is tested rather
// than a repeat of the same URLs. No key, no network.
func braveStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			q = "leer"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"web":{"results":[
  {"title":"Seite %[1]s eins","url":"https://example.de/%[1]s/1","description":"Beschreibung eins."},
  {"title":"Seite %[1]s zwei","url":"https://example.de/%[1]s/2","description":"Beschreibung zwei."}
]}}`, q)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSearchResultsContinueTheTurnsNumbering is the same rule where it reaches the model:
// two searches in one turn produce [1]/[2] and then [3]/[4], so a citation names one site.
// The page a result points at is the same page when scraped, which is why the shared list
// is keyed by URL.
func TestSearchResultsContinueTheTurnsNumbering(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	chain := research.NewChain(st, research.Options{
		BraveAPIKey:   "test-key",
		ProviderOrder: []string{"brave"},
		CacheMinutes:  1,
	}, braveStub(t).URL)
	service := New(st, chain, nil)

	list := NewSourceList()
	ctx := WithTurn(context.Background(), TurnRef{BackendID: "fake", Sources: list})

	first, err := service.Execute(ctx, "chat-1", ToolSearchWeb, `{"query":"alpha"}`)
	if err != nil {
		t.Fatalf("first search: %v", err)
	}
	if !strings.Contains(first, "[1] Seite alpha eins") || !strings.Contains(first, "[2] Seite alpha zwei") {
		t.Fatalf("the first search does not number from 1:\n%s", first)
	}

	second, err := service.Execute(ctx, "chat-1", ToolSearchWeb, `{"query":"beta"}`)
	if err != nil {
		t.Fatalf("second search: %v", err)
	}
	if !strings.Contains(second, "[3] Seite beta eins") || !strings.Contains(second, "[4] Seite beta zwei") {
		t.Fatalf("the second search restarted the numbering:\n%s", second)
	}
	if got := list.All(); len(got) != 4 {
		t.Fatalf("sources = %d, want the four unique pages: %+v", len(got), got)
	}

	// The same query again comes from the search cache, and its numbers must still be the
	// ones the first answer saw: a cached result is not a new source.
	again, err := service.Execute(ctx, "chat-1", ToolSearchWeb, `{"query":"alpha"}`)
	if err != nil {
		t.Fatalf("cached search: %v", err)
	}
	if !strings.Contains(again, "[1] Seite alpha eins") || strings.Contains(again, "[5]") {
		t.Fatalf("a cached search was renumbered:\n%s", again)
	}
	if got := list.All(); len(got) != 4 {
		t.Errorf("a cached search added a source: %d entries, want 4", len(got))
	}

	// Without a turn there is no list, and the old per-list numbering is what a
	// standalone call gets.
	bare, err := service.Execute(context.Background(), "chat-1", ToolSearchWeb, `{"query":"gamma"}`)
	if err != nil {
		t.Fatalf("standalone search: %v", err)
	}
	if !strings.Contains(bare, "[1] Seite gamma eins") {
		t.Fatalf("a standalone search does not number from 1:\n%s", bare)
	}
}
