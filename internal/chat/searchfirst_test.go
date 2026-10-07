package chat

import (
	"strings"
	"testing"

	"kichat/internal/config"
	"kichat/internal/research"
)

// TestSearchTokenBudget covers the interaction that is easy to get wrong: the
// configured budget is an upper bound, but a small model window must win over
// it, and the API's own floor still applies underneath.
func TestSearchTokenBudget(t *testing.T) {
	base := config.Default()
	base.Search.LLMContextTokens = 8192

	cases := []struct {
		name         string
		tokens       int
		contextLimit int
		want         int
	}{
		{"configured budget when the window is unknown", 8192, 0, 8192},
		{"quarter of a large window", 8192, 48128, 8192},
		{"small window caps the budget", 8192, 8000, 2000},
		{"tiny window still respects the API floor", 8192, 2000, 1024},
		{"explicit smaller budget wins over a large window", 4096, 48128, 4096},
		{"unset budget falls back to the API default", 0, 0, 8192},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := base
			settings.Search.LLMContextTokens = tc.tokens
			if got := searchTokenBudget(settings, tc.contextLimit); got != tc.want {
				t.Errorf("budget = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRenderGroundedContext pins the exact prompt text. It is the payload the
// model reads, so a missing provider name, budget or source number would be
// invisible in the UI and only show up as a worse answer.
func TestRenderGroundedContext(t *testing.T) {
	grounded := research.GroundedContext{
		Provider:  "brave-context",
		MaxTokens: 4096,
		Sources: []research.GroundedSource{
			{URL: "https://example.de/a", Title: "Erste Quelle", Snippets: []string{"Absatz eins.", "Absatz zwei."}, Kind: "page"},
			{URL: "https://example.de/b", Title: "", Snippets: []string{"Ohne Titel."}, Kind: "place"},
		},
	}
	got := renderGroundedContext(grounded)

	for _, want := range []string{
		"brave-context",
		"Budget: 4096 Tokens",
		"[1] Erste Quelle",
		"URL: https://example.de/a",
		"Absatz eins.",
		"Absatz zwei.",
		"[2] https://example.de/b", // a source without a title is labelled by URL
		"Ohne Titel.",
		"unvertrauenswürdige",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("seeded message is missing %q:\n%s", want, got)
		}
	}

	// A single-snippet source must not be numbered as if there were more, and
	// the untrusted-data notice has to come last so it is the final instruction.
	if !strings.HasSuffix(strings.TrimSpace(got), strings.TrimSpace(untrustedTail(got))) {
		t.Errorf("the untrusted-content notice is not the closing paragraph:\n%s", got)
	}

	// The headline-only fallback has no token budget and must say so.
	fallback := renderGroundedContext(research.GroundedContext{
		Provider: "duckduckgo",
		Sources:  []research.GroundedSource{{URL: "https://a.example", Title: "T", Snippets: []string{"kurz"}}},
	})
	if !strings.Contains(fallback, "nur Kurzbeschreibungen") {
		t.Errorf("fallback without a budget does not advertise it:\n%s", fallback)
	}

	if out := renderGroundedContext(research.GroundedContext{}); out != "" {
		t.Errorf("an empty context rendered %q, want nothing", out)
	}

	// The warning produced by a degraded provider has to reach the prompt.
	withWarning := renderGroundedContext(research.GroundedContext{
		Provider: "brave-context",
		Warning:  "Suchanfrage auf 75 Wörter gekürzt (API-Limit)",
		Sources:  []research.GroundedSource{{URL: "https://a.example", Title: "T", Snippets: []string{"x"}}},
	})
	if !strings.Contains(withWarning, "Hinweis: Suchanfrage") {
		t.Errorf("warning missing from the seeded message:\n%s", withWarning)
	}
}

// TestRenderResultListing keeps the older headline-only path honest: it is still
// used when LLM context is switched off, and it must carry the notice too.
func TestRenderResultListing(t *testing.T) {
	out := renderResultListing(research.SearchOutcome{
		Provider: "duckduckgo",
		Warning:  "brave war nicht verfügbar",
		Results: []research.SearchResult{
			{Title: "Erster Treffer", URL: "https://example.org/1", Snippet: "Beschreibung eins.", Age: "3 Tage", Rank: 1},
		},
	})
	for _, want := range []string{"duckduckgo", "[1] Erster Treffer", "URL: https://example.org/1", "Beschreibung eins.", "Datum: 3 Tage", "Hinweis: brave war nicht verfügbar", "unvertrauenswürdige"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Budget:") {
		t.Error("the headline listing claims a token budget it does not have")
	}
	if got := renderResultListing(research.SearchOutcome{}); got != "" {
		t.Errorf("an empty outcome rendered %q, want nothing", got)
	}
}

// untrustedTail returns the closing paragraph so the ordering assertion does not
// duplicate the notice text.
func untrustedTail(rendered string) string {
	idx := strings.Index(rendered, "Diese Ergebnisse stammen")
	if idx < 0 {
		return ""
	}
	return rendered[idx:]
}
