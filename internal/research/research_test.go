package research

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A trimmed recording of a real Brave response shape, used so the parser is
// tested offline and layout drift is caught without spending API quota.
const braveFixture = `{
  "web": {
    "results": [
      {
        "title": "Mietrecht: Kündigung &amp; Mieterhöhung",
        "url": "https://www.example.de/mietrecht?utm_source=newsletter&id=7",
        "description": "Überblick über  <strong>Mietrecht</strong> in Deutschland.",
        "age": "3 Tage",
        "extra_snippets": ["§ 573 BGB regelt die ordentliche Kündigung.", "Fristen sind gesetzlich festgelegt."]
      },
      {
        "title": "Zweiter Treffer",
        "url": "https://www.example.de/mietrecht/?id=7",
        "description": "Ein Duplikat mit anderer URL-Schreibweise."
      }
    ]
  },
  "news": {
    "results": [
      {"title": "BGH entscheidet", "url": "https://news.example.de/bgh", "description": "Urteil vom 01.02.2026.", "age": "1 Tag"}
    ]
  },
  "faq": {
    "results": [
      {"question": "Wie lange ist die Kündigungsfrist?", "answer": "In der Regel drei Monate.", "url": "https://faq.example.de/frist"}
    ]
  },
  "infobox": {
    "title": "Mietrecht",
    "long_desc": "Mietrecht ist der Teil des Zivilrechts, der Mietverhältnisse regelt."
  }
}`

func TestParseBraveResponse(t *testing.T) {
	var parsed braveResponse
	if err := json.Unmarshal([]byte(braveFixture), &parsed); err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	results := parseBraveResponse(parsed, 20)
	if len(results) == 0 {
		t.Fatal("no results parsed")
	}

	// The infobox is lifted to the front so an at-a-glance answer is available.
	if results[0].Provider != "brave-infobox" {
		t.Errorf("first result = %+v, want the infobox first", results[0])
	}
	if !strings.Contains(results[0].Snippet, "Zivilrechts") {
		t.Errorf("infobox text missing: %q", results[0].Snippet)
	}

	byProvider := map[string]int{}
	for _, r := range results {
		byProvider[r.Provider]++
	}
	for _, want := range []string{"brave", "brave-news", "brave-faq", "brave-infobox"} {
		if byProvider[want] == 0 {
			t.Errorf("no results from %s", want)
		}
	}

	for _, r := range results {
		if r.URL == "" && r.Provider != "brave-infobox" {
			t.Errorf("result without URL: %+v", r)
		}
		if strings.Contains(r.Title, "&amp;") {
			t.Errorf("HTML entity not decoded: %q", r.Title)
		}
		if strings.Contains(r.Snippet, "<strong>") {
			t.Errorf("HTML markup leaked into the snippet: %q", r.Snippet)
		}
		if strings.Contains(r.Snippet, "  ") {
			t.Errorf("snippet whitespace not collapsed: %q", r.Snippet)
		}
	}

	// Tracking parameters are stripped so the same page is not cached or cited
	// twice under different URLs.
	for _, r := range results {
		if strings.Contains(r.URL, "utm_source") {
			t.Errorf("tracking parameter survived: %s", r.URL)
		}
	}

	// The two example.de entries differ only by a trailing slash and tracking
	// params, so they must collapse into one result.
	seen := map[string]int{}
	for _, r := range results {
		if strings.Contains(r.URL, "example.de/mietrecht") && r.Provider == "brave" {
			seen[strings.TrimRight(r.URL, "/")]++
		}
	}
	if len(seen) != 1 {
		t.Errorf("expected the duplicate URL to be folded, got %v", seen)
	}

	// Extra snippets are preserved because they often contain the actual answer.
	foundExtra := false
	for _, r := range results {
		if len(r.Extra) > 0 {
			foundExtra = true
		}
	}
	if !foundExtra {
		t.Error("extra_snippets were dropped")
	}
}

func TestParseBraveRespectsLimit(t *testing.T) {
	var parsed braveResponse
	if err := json.Unmarshal([]byte(braveFixture), &parsed); err != nil {
		t.Fatal(err)
	}
	if got := len(parseBraveResponse(parsed, 2)); got > 2 {
		t.Fatalf("limit ignored: got %d results", got)
	}
}

// ddgFixture mirrors the html.duckduckgo.com markup, including an ad row and a
// redirect-wrapped link.
const ddgFixture = `<!DOCTYPE html><html><body>
<div class="result results_links">
  <div class="result__body">
    <h2 class="result__title">
      <a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fwww.gesetze-im-internet.de%2Fbgb%2F__573.html&rut=abc">§ 573 BGB - Kündigung</a>
    </h2>
    <a class="result__snippet">Der Vermieter kann nur kündigen, wenn er ein berechtigtes Interesse hat.</a>
  </div>
</div>
<div class="result result--ad">
  <div class="result__body">
    <a class="result__a" href="https://spam.example/ad">Anzeige: Mietrecht Anwalt</a>
    <a class="result__snippet">Jetzt Beratung buchen.</a>
  </div>
</div>
<div class="result results_links">
  <div class="result__body">
    <h2 class="result__title">
      <a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fde.wikipedia.org%2Fwiki%2FMietvertrag&rut=def">Mietvertrag – Wikipedia</a>
    </h2>
    <a class="result__snippet">Ein Mietvertrag ist ein Vertrag zwischen Vermieter und Mieter.</a>
  </div>
</div>
<div class="no-results">Keine Ergebnisse</div>
</body></html>`

func TestParseDDGHtml(t *testing.T) {
	results, err := parseDDGHtml(ddgFixture, 10)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (the ad row must be skipped): %+v", len(results), results)
	}
	if results[0].URL != "https://www.gesetze-im-internet.de/bgb/__573.html" {
		t.Errorf("redirect not unwrapped: %q", results[0].URL)
	}
	if !strings.Contains(results[0].Title, "573") {
		t.Errorf("title = %q", results[0].Title)
	}
	if !strings.Contains(results[0].Snippet, "berechtigtes Interesse") {
		t.Errorf("snippet = %q", results[0].Snippet)
	}
	for _, r := range results {
		if strings.Contains(r.URL, "spam.example") {
			t.Error("advertisement row was not filtered out")
		}
		if strings.Contains(r.URL, "duckduckgo.com/l/") {
			t.Errorf("redirect wrapper left in %q", r.URL)
		}
		if !strings.HasPrefix(r.URL, "https://") {
			t.Errorf("URL is not absolute: %q", r.URL)
		}
	}
}

func TestParseDDGHtmlOnLayoutChangeReturnsNothing(t *testing.T) {
	results, err := parseDDGHtml("<html><body><p>Something completely different</p></body></html>", 10)
	if err != nil {
		t.Fatalf("a layout change should not be reported as a hard error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no results, got %+v", results)
	}
}

const ddgLiteFixture = `<html><body><table>
<tr><td>1.</td><td><a class="result-link" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.org%2Fa">Erster</a></td></tr>
<tr><td>&nbsp;</td><td class="result-snippet">Erste Beschreibung.</td></tr>
<tr><td>2.</td><td><a class="result-link" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.org%2Fb">Zweiter</a></td></tr>
<tr><td>&nbsp;</td><td class="result-snippet">Zweite Beschreibung.</td></tr>
</table></body></html>`

func TestParseDDGLite(t *testing.T) {
	results, err := parseDDGLite(ddgLiteFixture, 10)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].URL != "https://example.org/a" {
		t.Errorf("URL = %q", results[0].URL)
	}
	if !strings.Contains(results[0].Snippet, "Erste Beschreibung") {
		t.Errorf("snippet = %q; the lite layout puts it in the following row", results[0].Snippet)
	}
}

func TestNormaliseURLStripsTrackingButKeepsMeaning(t *testing.T) {
	cases := map[string]string{
		"https://a.de/x?utm_source=n&id=3":    "https://a.de/x?id=3",
		"https://a.de/x?gclid=abc":            "https://a.de/x",
		"https://a.de/x#section":              "https://a.de/x",
		"https://a.de/x?a=1&utm_medium=email": "https://a.de/x?a=1",
		"https://a.de/x":                      "https://a.de/x",
	}
	for in, want := range cases {
		if got := normaliseURL(in); got != want {
			t.Errorf("normaliseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDedupeResultsKeepsFirstAndReranks(t *testing.T) {
	in := []SearchResult{
		{Title: "a", URL: "https://x.de/1"},
		{Title: "b", URL: "https://x.de/1/"},
		{Title: "c", URL: "https://x.de/2"},
	}
	out := dedupeResults(in)
	if len(out) != 2 {
		t.Fatalf("got %d results, want 2", len(out))
	}
	if out[0].Rank != 1 || out[1].Rank != 2 {
		t.Errorf("ranks not renumbered: %+v", out)
	}
	if out[0].Title != "a" {
		t.Errorf("expected the first occurrence to win, got %q", out[0].Title)
	}
}

// TestScraperBlocksPrivateHosts is the SSRF guard: a page can contain links to
// local services, and following them would let remote content probe the host.
func TestScraperBlocksPrivateHosts(t *testing.T) {
	scraper := NewScraper(nil, ScrapeOptions{Timeout: 2 * time.Second}, nil)
	for _, target := range []string{
		"http://127.0.0.1:1234/v1/models",
		"http://localhost/admin",
		"http://10.0.0.5/secret",
		"http://192.168.1.1/router",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
	} {
		if _, err := scraper.Scrape(context.Background(), target, 100); err == nil {
			t.Errorf("expected %s to be blocked", target)
		}
	}
}

func TestScraperRejectsNonHTTPSchemes(t *testing.T) {
	scraper := NewScraper(nil, ScrapeOptions{}, nil)
	for _, target := range []string{"file:///etc/passwd", "gopher://x/1", "ftp://x/y"} {
		if _, err := scraper.Scrape(context.Background(), target, 100); err == nil {
			t.Errorf("expected %s to be rejected", target)
		}
	}
}

func TestRateLimiterRespectsContextCancellation(t *testing.T) {
	limiter := newRateLimiter(0.001) // one call per ~16 minutes
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := limiter.Wait(ctx); err == nil {
		t.Error("expected the limiter to abort when the context is cancelled")
	}
}

func TestParseRobotsExtractsWildcardDisallow(t *testing.T) {
	body := `
# comment
User-agent: Googlebot
Disallow: /private-google

User-agent: *
Disallow: /admin
Disallow: /tmp

User-agent: *
Allow: /admin/public
`
	rules := parseRobots(body)
	joined := strings.Join(rules, ",")
	if !strings.Contains(joined, "/admin") || !strings.Contains(joined, "/tmp") {
		t.Fatalf("wildcard rules missing: %v", rules)
	}
	if strings.Contains(joined, "/private-google") {
		t.Fatalf("rules for another user agent leaked in: %v", rules)
	}
}
