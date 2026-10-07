package research

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Trimmed recording of the documented /res/v1/llm/context shape. Kept offline so
// the parser is exercised without spending metered Brave quota.
const braveContextFixture = `{
  "grounding": {
    "generic": [
      {
        "url": "https://www.example.de/mittelmeer?utm_source=newsletter&id=7",
        "title": "Mittelmeer: Tiefe &amp; Lage",
        "snippets": ["Die tiefste Stelle liegt im <strong>Kalypso-Tief</strong>.", "Durchschnittlich etwa 1.500 m."]
      },
      {
        "url": "https://www.example.de/mittelmeer/?id=7",
        "title": "Dieselbe Seite, andere Schreibweise",
        "snippets": ["Ein Duplikat, das das Budget nicht zweimal kosten darf."]
      }
    ],
    "poi": {
      "name": "Mittelmeer",
      "url": "https://poi.example.de/mittelmeer",
      "title": "Mittelmeer",
      "snippets": ["Mittelmeer als Ort aufbereitet."]
    },
    "map": [
      {"name": "Kalypso-Tief", "url": "https://map.example.de/kalypso", "title": "Kalypso-Tief", "snippets": ["Tiefste Stelle, rund 5.267 m."]}
    ]
  },
  "sources": {}
}`

func TestParseBraveContext(t *testing.T) {
	var parsed braveContextResponse
	if err := json.Unmarshal([]byte(braveContextFixture), &parsed); err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	got := parseBraveContext(parsed, 4096)

	if got.Provider != "brave-context" {
		t.Errorf("provider = %q, want brave-context", got.Provider)
	}
	if got.MaxTokens != 4096 {
		t.Errorf("maxTokens = %d, want 4096", got.MaxTokens)
	}
	// Two example.de URLs differ only by a tracking parameter and a trailing
	// slash, so they must collapse into one source.
	if len(got.Sources) != 3 {
		t.Fatalf("got %d sources, want 3 (duplicate URL not collapsed): %+v", len(got.Sources), got.Sources)
	}
	kinds := map[string]int{}
	for _, s := range got.Sources {
		kinds[s.Kind]++
	}
	for _, want := range []string{"page", "poi", "place"} {
		if kinds[want] == 0 {
			t.Errorf("no %s source parsed (kinds=%v)", want, kinds)
		}
	}

	var page GroundedSource
	for _, s := range got.Sources {
		if s.Kind == "page" {
			page = s
			break
		}
	}
	// Entities decoded and the <strong> highlight stripped before it reaches a prompt.
	if page.Title != "Mittelmeer: Tiefe & Lage" {
		t.Errorf("title = %q, want the entity decoded", page.Title)
	}
	if len(page.Snippets) != 2 {
		t.Fatalf("snippets = %v, want 2", page.Snippets)
	}
	if strings.Contains(page.Snippets[0], "<strong>") {
		t.Errorf("markup leaked into snippet: %q", page.Snippets[0])
	}
	if !strings.Contains(page.Snippets[0], "Kalypso-Tief") {
		t.Errorf("highlight text lost: %q", page.Snippets[0])
	}
	if page.URL != "https://www.example.de/mittelmeer?id=7" {
		t.Errorf("url not normalised: %q", page.URL)
	}
	if got.Empty() {
		t.Error("Empty() reported true for a populated context")
	}
}

func TestParseBraveContextDropsEmptySnippets(t *testing.T) {
	var parsed braveContextResponse
	fixture := `{"grounding":{"generic":[{"url":"https://a.example","title":"Leer","snippets":["   ","<b></b>"]}]}}`
	if err := json.Unmarshal([]byte(fixture), &parsed); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	got := parseBraveContext(parsed, 1024)
	if !got.Empty() {
		t.Errorf("expected no sources for empty snippets, got %+v", got.Sources)
	}
}

func TestClampBraveQuery(t *testing.T) {
	// Exactly at the word limit must survive untouched.
	words := make([]string, BraveContextQueryMaxWords)
	for i := range words {
		words[i] = "wort"
	}
	atLimit := strings.Join(words, " ")
	if out, clamped := clampBraveQuery(atLimit); clamped || out != atLimit {
		t.Errorf("query at the word limit was altered (clamped=%v)", clamped)
	}

	// One word over must be trimmed to the documented cap.
	over := atLimit + " extra"
	out, clamped := clampBraveQuery(over)
	if !clamped {
		t.Error("over-long query was not reported as clamped")
	}
	if got := len(strings.Fields(out)); got != BraveContextQueryMaxWords {
		t.Errorf("words = %d, want %d", got, BraveContextQueryMaxWords)
	}

	// A long character run is cut at a word boundary, never mid-word and never
	// leaving the trailing space behind: both would be sent as-is.
	long := strings.Repeat("abcdefghij ", 80)
	out, clamped = clampBraveQuery(long)
	if !clamped {
		t.Error("over-long character query was not clamped")
	}
	if len([]rune(out)) > BraveContextQueryMaxChars {
		t.Errorf("runes = %d, want <= %d", len([]rune(out)), BraveContextQueryMaxChars)
	}
	if strings.HasSuffix(out, " ") {
		t.Errorf("query left dangling whitespace: %q", out)
	}
	if !strings.HasSuffix(out, "abcdefghij") {
		t.Errorf("query was cut mid-word: %q", out)
	}

	if out, _ := clampBraveQuery("   "); out != "" {
		t.Errorf("blank query = %q, want empty", out)
	}
}

func TestBraveFailureClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{
			name:   "quota exhausted",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"status":429,"detail":"quota reached","code":"QUOTA_LIMITED"}}`,
			want:   ErrQuotaExhausted,
		},
		{
			name:   "rate limited by code",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"status":429,"detail":"slow down","code":"RATE_LIMITED"}}`,
			want:   ErrRateLimited,
		},
		{
			// Some plans report a plain 429 without a code; treat it as transient.
			name:   "bare 429",
			status: http.StatusTooManyRequests,
			body:   `too many requests`,
			want:   ErrRateLimited,
		},
		{
			// Measured against the real API: a rejected key comes back as 422
			// with this wording, not as the 401 the reference documents. It must
			// stay distinguishable from a throttle, otherwise the app would
			// retry a key that can never work.
			name:   "invalid key as 422",
			status: http.StatusUnprocessableEntity,
			body:   `{"error":{"status":422,"detail":"The provided API key is invalid.","code":""}}`,
			want:   ErrInvalidKey,
		},
		{
			name:   "invalid key as 401",
			status: http.StatusUnauthorized,
			body:   `{"error":{"status":401,"code":"INTERNAL"}}`,
			want:   ErrInvalidKey,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyBraveFailure(tc.status, []byte(tc.body))
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want to wrap %v", err, tc.want)
			}
			// A quota problem must not be mistaken for a throttle: the two need
			// opposite handling.
			if tc.want == ErrQuotaExhausted && errors.Is(err, ErrRateLimited) {
				t.Error("quota error also matched ErrRateLimited")
			}
			if tc.want == ErrInvalidKey && (errors.Is(err, ErrRateLimited) || errors.Is(err, ErrQuotaExhausted)) {
				t.Error("a rejected key was classified as a throttle or an exhausted balance")
			}
		})
	}

	// Anything that is neither a throttle nor a key problem stays an httpError
	// with its status intact, so the real reason is still reportable.
	err := classifyBraveFailure(http.StatusInternalServerError, []byte(`{"error":{"status":500,"code":"INTERNAL"}}`))
	var httpErr *httpError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("err = %v, want an httpError with status 500", err)
	}
}

func TestBraveContextRequestShape(t *testing.T) {
	var (
		gotQuery  string
		gotHeader string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotHeader = r.Header.Get("X-Subscription-Token")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "42")
		_, _ = w.Write([]byte(braveContextFixture))
	}))
	defer srv.Close()

	p := NewBraveProvider("secret-token", "")
	p.ContextEndpoint = srv.URL
	got, err := p.Context(context.Background(), SearchRequest{
		Query:      "wie tief ist das mittelmeer",
		Country:    "de",
		SearchLang: "de",
		// Deliberately out of range to prove local clamping.
		Count: 999,
	}, 999999)
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if len(got.Sources) == 0 {
		t.Fatal("no sources returned")
	}

	params := parseTestQuery(t, gotQuery)
	if params.Get("q") != "wie tief ist das mittelmeer" {
		t.Errorf("q = %q", params.Get("q"))
	}
	if params.Get("count") != "50" {
		t.Errorf("count = %q, want the 50 ceiling", params.Get("count"))
	}
	if params.Get("maximum_number_of_tokens") != "32768" {
		t.Errorf("maximum_number_of_tokens = %q, want the 32768 ceiling", params.Get("maximum_number_of_tokens"))
	}
	if params.Get("country") != "DE" {
		t.Errorf("country = %q, want uppercase DE", params.Get("country"))
	}
	if params.Get("safesearch") != "moderate" {
		t.Errorf("safesearch = %q, want moderate", params.Get("safesearch"))
	}
	if gotHeader != "secret-token" {
		t.Errorf("subscription header = %q", gotHeader)
	}
	// The token must never appear in the URL, where it could reach logs.
	if strings.Contains(gotQuery, "secret-token") {
		t.Error("API key leaked into the query string")
	}
	if p.LastQuota() != 42 {
		t.Errorf("lastQuota = %d, want 42 from the rate-limit header", p.LastQuota())
	}
}

// TestBraveContextFloorsTokenBudget proves a too-small budget is raised to the
// documented floor instead of being sent and rejected.
func TestBraveContextFloorsTokenBudget(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RawQuery
		_, _ = w.Write([]byte(braveContextFixture))
	}))
	defer srv.Close()

	p := NewBraveProvider("secret-token", "")
	p.ContextEndpoint = srv.URL
	if _, err := p.Context(context.Background(), SearchRequest{Query: "test"}, 1); err != nil {
		t.Fatalf("Context: %v", err)
	}
	if tokens := parseTestQuery(t, seen).Get("maximum_number_of_tokens"); tokens != "1024" {
		t.Errorf("maximum_number_of_tokens = %q, want the 1024 floor", tokens)
	}
}

func TestBraveContextRequiresKey(t *testing.T) {
	p := NewBraveProvider("", "")
	if _, err := p.Context(context.Background(), SearchRequest{Query: "x"}, 0); err == nil {
		t.Error("expected an error without an API key")
	} else if !strings.Contains(err.Error(), "API-Key") {
		t.Errorf("err = %v, want a clear missing-key message", err)
	}
	if _, err := p.Context(context.Background(), SearchRequest{Query: "   "}, 0); !errors.Is(err, ErrEmptyQuery) {
		t.Errorf("err = %v, want ErrEmptyQuery", err)
	}
}

// stubProvider stands in for DuckDuckGo so no test reaches the network.
type stubProvider struct {
	name      string
	available bool
	results   []SearchResult
	err       error
}

func (s *stubProvider) Name() string      { return s.name }
func (s *stubProvider) Available() bool   { return s.available }
func (s *stubProvider) RequiresKey() bool { return false }
func (s *stubProvider) Search(context.Context, SearchRequest) ([]SearchResult, error) {
	return s.results, s.err
}

// TestChainGroundedContextFallsBackOnQuota is the behaviour that matters on a
// metered plan: when Brave reports an exhausted credit balance the turn must
// still be grounded, and the degradation must be stated rather than hidden.
func TestChainGroundedContextFallsBackOnQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"status":429,"detail":"plan quota reached","code":"QUOTA_LIMITED"}}`))
	}))
	defer srv.Close()

	chain, brave := newTestChainWithBrave(t, srv.URL, "test-key")
	chain.providers["duckduckgo"] = &stubProvider{
		name:      "duckduckgo",
		available: true,
		results: []SearchResult{
			{Title: "Ersatzquelle", URL: "https://fallback.example/a", Snippet: "Kurzbeschreibung als Ersatztext.", Rank: 1},
		},
	}
	_ = brave

	got, err := chain.GroundedContext(context.Background(), "wie tief ist das mittelmeer", SearchRequest{}, 4096)
	if err != nil {
		t.Fatalf("GroundedContext: %v", err)
	}
	if got.Empty() {
		t.Fatal("fallback produced no sources")
	}
	if got.Provider != "duckduckgo" {
		t.Errorf("provider = %q, want the fallback provider", got.Provider)
	}
	if !strings.Contains(got.Warning, "Kontingent") {
		t.Errorf("warning does not name the exhausted quota: %q", got.Warning)
	}
	if got.Sources[0].Snippets[0] != "Kurzbeschreibung als Ersatztext." {
		t.Errorf("fallback snippet = %q", got.Sources[0].Snippets[0])
	}
}

func TestChainGroundedContextFallsBackWithoutKey(t *testing.T) {
	chain, _ := newTestChainWithBrave(t, "", "")
	chain.providers["duckduckgo"] = &stubProvider{
		name:      "duckduckgo",
		available: true,
		results:   []SearchResult{{Title: "T", URL: "https://a.example", Snippet: "s", Rank: 1}},
	}

	got, err := chain.GroundedContext(context.Background(), "eine frage", SearchRequest{}, 0)
	if err != nil {
		t.Fatalf("GroundedContext: %v", err)
	}
	if !strings.Contains(got.Warning, "API-Key") {
		t.Errorf("warning = %q, want it to mention the missing key", got.Warning)
	}
}

func TestChainGroundedContextRejectsEmptyQuery(t *testing.T) {
	chain := NewChain(nil, Options{}, "")
	if _, err := chain.GroundedContext(context.Background(), "   ", SearchRequest{}, 0); !errors.Is(err, ErrEmptyQuery) {
		t.Errorf("err = %v, want ErrEmptyQuery", err)
	}
}

// newTestChainWithBrave builds a chain whose Brave provider points at a local
// fixture server on both routes, so the fallback path cannot escape to the real
// API even when a test only expects the context route to be exercised.
func newTestChainWithBrave(t *testing.T, endpoint, apiKey string) (*Chain, *BraveProvider) {
	t.Helper()
	chain := NewChain(nil, Options{
		ProviderOrder: []string{"brave", "duckduckgo"},
		BraveAPIKey:   apiKey,
		CacheMinutes:  60,
	}, endpoint)
	brave, ok := chain.providers["brave"].(*BraveProvider)
	if !ok {
		t.Fatal("brave provider not constructed")
	}
	if endpoint != "" {
		brave.Endpoint = endpoint
		brave.ContextEndpoint = endpoint
	}
	return chain, brave
}

func parseTestQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	values, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("query %q does not parse: %v", raw, err)
	}
	return values
}
