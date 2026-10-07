package research

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// liveBraveKey opts a test run into the real API. Two gates are deliberate:
// KICHAT_LIVE keeps a normal `go test ./...` offline, and the key is never read
// from a file in the repository, only from the environment.
func liveBraveKey(t *testing.T) string {
	t.Helper()
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to run against the real API")
	}
	key := strings.TrimSpace(os.Getenv("KICHAT_BRAVE_KEY"))
	if key == "" {
		t.Skip("set KICHAT_BRAVE_KEY to run the live Brave tests")
	}
	return key
}

// TestLiveBraveContext is the end-to-end check of the LLM-context route: a real
// request, real page chunks, and a result the prompt builder can use. The
// offline fixtures pin the *shape* of the answer; only this proves the request
// parameters are accepted and the text is actually usable.
func TestLiveBraveContext(t *testing.T) {
	key := liveBraveKey(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	p := NewBraveProvider(key, "")
	got, err := p.Context(ctx,
		SearchRequest{Query: "Wie tief ist das Mittelmeer?", Country: "DE", SearchLang: "de"},
		4096)
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if got.Provider != "brave-context" {
		t.Errorf("provider = %q, want brave-context (a plain search would mean the fallback ran)", got.Provider)
	}
	if got.Empty() {
		t.Fatal("no sources returned")
	}
	if got.MaxTokens != 4096 {
		t.Errorf("maxTokens = %d, want the requested 4096", got.MaxTokens)
	}
	if got.Warning != "" {
		t.Errorf("unexpected warning for a valid in-range query: %q", got.Warning)
	}

	snippets, chars := 0, 0
	for _, s := range got.Sources {
		if s.URL == "" {
			t.Errorf("source without URL: %+v", s)
		}
		switch s.Kind {
		case "page", "poi", "place":
		default:
			t.Errorf("unknown kind %q", s.Kind)
		}
		if strings.Contains(s.Title, "<") || strings.Contains(strings.Join(s.Snippets, " "), "<") {
			t.Errorf("markup leaked into source %q", s.URL)
		}
		snippets += len(s.Snippets)
		for _, snip := range s.Snippets {
			chars += len(snip)
		}
	}
	if snippets == 0 {
		t.Error("sources carried no snippets — the model would get URLs only")
	}
	// 4096 tokens is roughly 16k characters; the route should fill a usable
	// share of that rather than returning a couple of lines.
	if chars < 2000 {
		t.Errorf("only %d characters of page text for a 4096-token budget across %d sources", chars, len(got.Sources))
	}
	t.Logf("provider=%s sources=%d snippets=%d chars=%d kinds=%v",
		got.Provider, len(got.Sources), snippets, chars, kindsOf(got.Sources))
	first := got.Sources[0]
	t.Logf("first source: %s | %s | %.160s", first.Kind, first.Title, strings.Join(first.Snippets, " "))
}

// TestLiveBraveContextClampsLongQuery proves the local limit handling against
// the real endpoint: an over-long query must be trimmed instead of being
// rejected with a 422, and the caller has to be told it was trimmed.
func TestLiveBraveContextClampsLongQuery(t *testing.T) {
	key := liveBraveKey(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	long := strings.Repeat("Mittelmeer Tiefe Randmeer Wasservolumen Salzgehalt ", 20)
	p := NewBraveProvider(key, "")
	got, err := p.Context(ctx, SearchRequest{Query: long}, 2048)
	if err != nil {
		t.Fatalf("Context with an over-long query: %v", err)
	}
	if got.Warning == "" {
		t.Error("query was clamped but the caller was not told")
	}
	if got.Empty() {
		t.Error("clamped query returned no sources")
	}
	t.Logf("warning=%q sources=%d", got.Warning, len(got.Sources))
}

// TestLiveBraveChainUsesContext is the integration case that matters: through
// the chain, with a working key, the turn must be grounded by the context route
// and the Brave counter must be incremented — not a silent fallback to
// DuckDuckGo, which would look identical in the chat but read worse.
func TestLiveBraveChainUsesContext(t *testing.T) {
	key := liveBraveKey(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	chain := NewChain(nil, Options{
		ProviderOrder: []string{"brave", "duckduckgo"},
		BraveAPIKey:   key,
		Country:       "DE",
		SearchLang:    "de",
		CacheMinutes:  60,
	}, "")
	got, err := chain.GroundedContext(ctx, "Wie tief ist das Mittelmeer?", SearchRequest{}, 4096)
	if err != nil {
		t.Fatalf("GroundedContext: %v", err)
	}
	if got.Provider != "brave-context" {
		t.Fatalf("provider = %q, want brave-context; warning=%q", got.Provider, got.Warning)
	}
	if got.Warning != "" {
		t.Errorf("warning = %q, want none when the context route answered", got.Warning)
	}
	if got.Empty() {
		t.Fatal("no sources")
	}
}

// TestLiveBraveBadKeyIsNotMisclassified: the error envelope is what separates an
// exhausted balance from a throttle, so a plain authentication failure must not
// be reported as either. A misclassification would make the app retry a key that
// will never work, or fall back as if the plan were used up.
func TestLiveBraveBadKeyIsNotMisclassified(t *testing.T) {
	liveBraveKey(t) // only the gating (KICHAT_LIVE + a key present) is reused
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p := NewBraveProvider("BSA-invalid-key-for-testing", "")
	_, err := p.Context(ctx, SearchRequest{Query: "Mittelmeer Tiefe"}, 1024)
	if err == nil {
		t.Fatal("an invalid key was accepted")
	}
	if errors.Is(err, ErrQuotaExhausted) || errors.Is(err, ErrRateLimited) {
		t.Errorf("an invalid key was classified as a quota/throttle problem: %v", err)
	}
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("err = %v, want it to match ErrInvalidKey so the UI can say the key is wrong", err)
	}
	t.Logf("invalid key -> %v", err)
}

func kindsOf(sources []GroundedSource) map[string]int {
	out := map[string]int{}
	for _, s := range sources {
		out[s.Kind]++
	}
	return out
}
