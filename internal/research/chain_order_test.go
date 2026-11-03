package research

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kichat/internal/store"
)

// TestChainAsksTheProvidersInTheConfiguredOrder pins the setting the window edits: the first
// entry of ProviderOrder answers, and the rest are only asked when it fails. It is the behaviour
// a reader of the providers list assumes, and the one the list used to misrepresent — enabling a
// provider appended it to the end, so "Brave back on" quietly made DuckDuckGo the first choice.
//
// Both orders are run against the same two fixture servers, so the only thing that changes
// between them is the order itself.
func TestChainAsksTheProvidersInTheConfiguredOrder(t *testing.T) {
	duck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(ddgFixture))
	}))
	defer duck.Close()
	brave := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(braveSearchFixture))
	}))
	defer brave.Close()

	orders := [][]string{
		{"duckduckgo", "brave"},
		{"brave", "duckduckgo"},
	}

	for _, order := range orders {
		t.Run(order[0]+" first", func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			defer st.Close()

			chain := NewChain(st, Options{
				ProviderOrder: order,
				BraveAPIKey:   "test-key",
				CacheMinutes:  60,
			}, brave.URL)

			// Both fixtures are reached through the providers the chain built, because that is
			// the object the order applies to.
			ddg := chain.providers["duckduckgo"].(*DuckDuckGoProvider)
			ddg.Endpoint = duck.URL
			ddg.LiteURL = duck.URL
			chain.providers["brave"].(*BraveProvider).Endpoint = brave.URL

			outcome, err := chain.Search(context.Background(),
				"priorität "+order[0], SearchRequest{Count: 3})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if outcome.Provider != order[0] {
				t.Errorf("provider = %q, want %q: the first entry of the order answers",
					outcome.Provider, order[0])
			}
		})
	}
}

// TestChainIgnoresACachedAnswerFromADemotedProvider pins the other half of a priority change:
// the cache is keyed by the query, so without this rule a query answered by DuckDuckGo keeps
// returning DuckDuckGo's results for the whole cache window after Brave was put first — which
// reads as the setting having done nothing.
//
// The entry is seeded rather than produced by a search, so the test cannot pass because the
// write failed. The second half is the control: the same entry *is* served while its provider is
// still in the order, which is what makes the first half mean something.
func TestChainIgnoresACachedAnswerFromADemotedProvider(t *testing.T) {
	const query = "priorität cache"

	brave := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(braveSearchFixture))
	}))
	defer brave.Close()
	// A local DuckDuckGo too: the control expects a cache hit, and a miss has to stay out of
	// the network rather than become a live search from a unit test.
	duck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(ddgFixture))
	}))
	defer duck.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cached, err := jsonMarshalResults([]SearchResult{{
		Title: "Alte Antwort", URL: "https://example.test/alt", Snippet: "x", Provider: "duckduckgo",
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := st.PutSearchCache(store.SearchCacheEntry{
		QueryHash:    hashKey(fmt.Sprintf("v1|%s|%d|%s|%s|%s|%d", strings.ToLower(query), 3, "", "", "", 0)),
		Provider:     "duckduckgo",
		Query:        query,
		ResponseJSON: cached,
		CreatedAt:    time.Now(),
		ExpiresAt:    time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// While DuckDuckGo is still asked, its cached answer is served — the cache itself works.
	kept := NewChain(st, Options{
		ProviderOrder: []string{"duckduckgo", "brave"},
		BraveAPIKey:   "test-key",
		CacheMinutes:  60,
	}, brave.URL)
	keptDDG := kept.providers["duckduckgo"].(*DuckDuckGoProvider)
	keptDDG.Endpoint = duck.URL
	keptDDG.LiteURL = duck.URL
	kept.providers["brave"].(*BraveProvider).Endpoint = brave.URL
	outcome, err := kept.Search(context.Background(), query, SearchRequest{Count: 3})
	if err != nil {
		t.Fatalf("kept search: %v", err)
	}
	if !outcome.Cached || outcome.Provider != "duckduckgo" {
		t.Fatalf("cached answer was not served while its provider is still asked: %+v", outcome)
	}

	// With DuckDuckGo dropped the same entry is ignored, and Brave answers.
	dropped := NewChain(st, Options{
		ProviderOrder: []string{"brave"},
		BraveAPIKey:   "test-key",
		CacheMinutes:  60,
	}, brave.URL)
	dropped.providers["brave"].(*BraveProvider).Endpoint = brave.URL
	outcome, err = dropped.Search(context.Background(), query, SearchRequest{Count: 3})
	if err != nil {
		t.Fatalf("dropped search: %v", err)
	}
	if outcome.Provider != "brave" {
		t.Errorf("provider = %q, want brave: a cached answer from a provider that is no longer asked has to be ignored", outcome.Provider)
	}
	if outcome.Cached {
		t.Error("the answer was reported as cached, but it came from a dropped provider")
	}
}
