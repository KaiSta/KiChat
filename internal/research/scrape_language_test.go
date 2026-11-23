package research

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kichat/internal/config"
)

// TestScrapeAsksForPagesInTheAppLanguage pins the Accept-Language header to the window's
// setting, including Chinese. config.ModelLanguage is deliberately not the source: it
// collapses a Chinese window to English because there is no model-facing Chinese wording,
// while the header names the language the reader asked for.
func TestScrapeAsksForPagesInTheAppLanguage(t *testing.T) {
	cases := []struct {
		name string
		lang string
		set  bool
		want string
	}{
		{"german", config.LangGerman, true, "de-DE,de;q=0.9,en;q=0.8"},
		{"english", config.LangEnglish, true, "en-US,en;q=0.9"},
		{"chinese", config.LangChinese, true, "zh-CN,zh;q=0.9,en;q=0.8"},
		{"unset", "", false, "en-US,en;q=0.9"},
	}

	headers := make(chan string, len(cases))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get("Accept-Language")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(srv.Close)

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			scraper := NewScraper(nil, ScrapeOptions{AllowPrivateHosts: true, Timeout: 2 * time.Second}, nil)
			if tc.set {
				scraper.Lang = func() string { return tc.lang }
			}
			if _, err := scraper.Scrape(context.Background(), srv.URL, 100); err != nil {
				t.Fatalf("Scrape: %v", err)
			}
			if got := <-headers; got != tc.want {
				t.Errorf("Accept-Language = %q, want %q", got, tc.want)
			}
		})
	}
}
