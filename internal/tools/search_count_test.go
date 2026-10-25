package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kichat/internal/research"
)

// braveResultsJSON is the shape /res/v1/web/search returns, with one hit per
// index, so a test can ask for fewer hits than the provider sends.
func braveResultsJSON(n int) string {
	entries := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		entries = append(entries, fmt.Sprintf(
			`{"title":"Treffer %d","url":"https://example.de/%d","description":"Beschreibung %d"}`,
			i, i, i))
	}
	return `{"web":{"results":[` + strings.Join(entries, ",") + `]}}`
}

// TestSearchWebUsesConfiguredResultCount is the bug this exists for: the tool
// advertised a hardcoded default of 8, so a model that left `count` out got 8
// hits however few the user had configured. The setting is the default; a count
// the model asks for on purpose still wins.
func TestSearchWebUsesConfiguredResultCount(t *testing.T) {
	const configured = 3
	var lastCount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastCount = r.URL.Query().Get("count")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(braveResultsJSON(10)))
	}))
	defer srv.Close()

	chain := research.NewChain(nil, research.Options{
		ProviderOrder: []string{"brave"},
		BraveAPIKey:   "test-key",
		ResultCount:   configured,
	}, srv.URL)
	svc := &Service{Chain: chain}

	out, err := svc.Execute(context.Background(), "chat-1", ToolSearchWeb, `{"query":"mietrecht"}`)
	if err != nil {
		t.Fatalf("search_web: %v", err)
	}
	if got := strings.Count(out, "    URL: "); got != configured {
		t.Errorf("model saw %d results, want the configured %d\n%s", got, configured, out)
	}
	if lastCount != "3" {
		t.Errorf("brave was asked for count=%q, want %q", lastCount, "3")
	}

	// An explicit count is the model's decision and must survive the setting.
	out, err = svc.Execute(context.Background(), "chat-1", ToolSearchWeb, `{"query":"mietrecht","count":2}`)
	if err != nil {
		t.Fatalf("search_web with count: %v", err)
	}
	if got := strings.Count(out, "    URL: "); got != 2 {
		t.Errorf("model saw %d results for count=2\n%s", got, out)
	}
}
