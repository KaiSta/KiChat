package research

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kichat/internal/llm"
	"kichat/internal/store"
)

// TestScrapeAppliesTokenBudgetToCachedContent: a page cached under a large budget
// must still be trimmed when a later caller asks for less. The cached path used
// to hand back the stored text untouched, so a request bounded to a small budget
// could receive a whole page.
func TestScrapeAppliesTokenBudgetToCachedContent(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	const pageURL = "https://example.com/big"
	big := strings.Repeat("wort wort wort wort wort\n", 500) // far larger than the budget below
	if err := st.PutScrapeCache(store.ScrapeCacheEntry{
		URLHash:   hashKey(normaliseURL(pageURL)),
		URL:       pageURL,
		Title:     "big",
		ContentMD: big,
		Tokens:    llm.EstimateTokens(big),
		FetchedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("PutScrapeCache: %v", err)
	}

	scraper := NewScraper(st, ScrapeOptions{AllowPrivateHosts: true, CacheHours: 1}, nil)
	result, err := scraper.Scrape(context.Background(), pageURL, 2000)
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	if !result.Cached {
		t.Fatal("expected the cached copy to be used")
	}
	full := llm.EstimateTokens(big)
	got := llm.EstimateTokens(result.Markdown)
	if got >= full {
		t.Errorf("cached result was not trimmed: %d of %d tokens", got, full)
	}
	// The cut is made to the budget and then carries the "[gekürzt]" marker, so a
	// little slack is expected; the point is that the result is bounded near the
	// budget rather than the whole page.
	if got > 2100 {
		t.Errorf("cached result is %d tokens, want it bounded near the 2000-token budget", got)
	}
	if !result.Truncated {
		t.Error("a trimmed cached result must be flagged as truncated")
	}
}
