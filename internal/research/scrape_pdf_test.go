package research

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kichat/internal/llm"
)

// newPDFScraper builds a scraper that talks to a local test server and reports the
// bytes its PDF extractor was handed. AllowPrivateHosts is on because httptest
// listens on loopback, which the guard blocks by default.
func newPDFScraper(t *testing.T, opts ScrapeOptions, extractor pdfExtractor) *Scraper {
	t.Helper()
	opts.AllowPrivateHosts = true
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Second
	}
	return NewScraper(nil, opts, extractor)
}

// TestAPDFLargerThanThePageCapIsDownloadedWhole is the bug this cap exists for: the
// 5 MB bound meant for HTML cut a 9.7 MB manual in half, the trailer xref went with
// it, and the parser reported "not a PDF file: missing %%EOF" — a broken file blamed
// on the website. The extractor must see every byte.
func TestAPDFLargerThanThePageCapIsDownloadedWhole(t *testing.T) {
	pdf := make([]byte, 6<<20)
	copy(pdf, "%PDF-1.5\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdf)
	}))
	defer server.Close()

	var seen int
	scraper := newPDFScraper(t, ScrapeOptions{MaxBytes: 1 << 20}, func(data []byte, filename string) (string, int, error) {
		seen = len(data)
		return "## Seite 1\nHandbuch", 1, nil
	})

	result, err := scraper.Scrape(context.Background(), server.URL+"/Manual.pdf", 8000)
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	if seen != len(pdf) {
		t.Fatalf("the extractor received %d bytes of a %d-byte PDF; the page cap cut it", seen, len(pdf))
	}
	if result.Pages != 1 || !strings.Contains(result.Markdown, "Handbuch") {
		t.Errorf("result = %+v, want the extracted text and its page count", result)
	}
}

// TestAPDFOverThePDFCapSaysWhatHappened: past the cap there is nothing sensible to
// extract, so the answer names the limit rather than handing a cut file to a parser
// whose complaint reads as a broken PDF.
func TestAPDFOverThePDFCapSaysWhatHappened(t *testing.T) {
	pdf := make([]byte, 4096)
	copy(pdf, "%PDF-1.5\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdf)
	}))
	defer server.Close()

	called := false
	scraper := newPDFScraper(t, ScrapeOptions{PDFMaxBytes: 1024}, func(data []byte, filename string) (string, int, error) {
		called = true
		return "", 0, fmt.Errorf("missing %%EOF")
	})
	_, err := scraper.Scrape(context.Background(), server.URL+"/Manual.pdf", 8000)
	if err == nil {
		t.Fatal("a PDF over the cap was accepted")
	}
	if called {
		t.Error("the extractor was handed a truncated PDF; that is what produced the misleading parse error")
	}
	if !strings.Contains(err.Error(), "research.pdfTooLarge") {
		t.Errorf("error = %v, want the pdfTooLarge message rather than a parser complaint", err)
	}
}

// TestAPDFUnderTheCapIsNotReportedAsCut guards the other direction: the truncation
// check must not fire on a file that simply ends at its own size.
func TestAPDFUnderTheCapIsNotReportedAsCut(t *testing.T) {
	pdf := make([]byte, 2048)
	copy(pdf, "%PDF-1.5\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdf)
	}))
	defer server.Close()

	scraper := newPDFScraper(t, ScrapeOptions{PDFMaxBytes: 2048}, func(data []byte, filename string) (string, int, error) {
		return "## Seite 1\ntext", 1, nil
	})
	if _, err := scraper.Scrape(context.Background(), server.URL+"/exact.pdf", 8000); err != nil {
		t.Fatalf("a PDF exactly at the cap was rejected: %v", err)
	}
}

// TestScrapeDocumentKeepsWhatScrapeTrims is the difference the indexing caller
// needs: the whole text for the chunks, and a bounded excerpt only for the answer.
func TestScrapeDocumentKeepsWhatScrapeTrims(t *testing.T) {
	page := "<html><body><article>" + strings.Repeat("<p>Ein Absatz mit Inhalt und noch einem Satz.</p>", 400) + "</article></body></html>"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	defer server.Close()

	scraper := newPDFScraper(t, ScrapeOptions{}, nil)

	full, err := scraper.ScrapeDocument(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("ScrapeDocument: %v", err)
	}
	trimmed, err := scraper.Scrape(context.Background(), server.URL, 500)
	if err != nil {
		t.Fatalf("Scrape: %v", err)
	}
	if full.Tokens <= 500 {
		t.Fatalf("the whole page is %d tokens, too small a fixture to prove anything", full.Tokens)
	}
	if trimmed.Tokens > full.Tokens {
		t.Errorf("the bounded read returned %d tokens from a %d-token page", trimmed.Tokens, full.Tokens)
	}
	if !trimmed.Truncated || trimmed.Truncated != (llm.EstimateTokens(trimmed.Markdown) < full.Tokens) {
		t.Errorf("truncated = %v for a 500-token budget on a %d-token page", trimmed.Truncated, full.Tokens)
	}
	if full.Truncated {
		t.Error("the untrimmed read reported itself truncated")
	}
	if len(full.Markdown) <= len(trimmed.Markdown) {
		t.Errorf("untrimmed read is %d bytes, trimmed is %d; the whole page must be longer", len(full.Markdown), len(trimmed.Markdown))
	}
}
