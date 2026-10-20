package chat

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/llm"
	"kichat/internal/research"
	"kichat/internal/store"
	"kichat/internal/tools"
)

// TestTheAnswerKeepsTheSourcesItCited pins the two halves of a citation: the page the
// model read is numbered in the text it receives, and the answer is stored with that
// numbering, so a `[1]` in the transcript can be resolved to a URL long after the turn.
//
// Without the stored list, the numbers exist only in a tool result the model may not
// quote, and a reader is left with `[1]` and no idea which site it was.
func TestTheAnswerKeepsTheSourcesItCited(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	page := `<html><head><title>Wärmespeicher</title></head><body><article><p>` +
		strings.Repeat("Ein Absatz über Preise und Größen. ", 40) + `</p></article></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	}))
	t.Cleanup(srv.Close)

	call := fmt.Sprintf("```tool_call\n{\"name\": \"scrape_url\", \"arguments\": {\"url\": %q}}\n```", srv.URL)
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: call}},
			{{Type: llm.EventDelta, Text: "Der Speicher kostet laut Quelle [1] etwa 3.000 Euro."},
				{Type: llm.EventDone}},
		},
	}
	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = 3
	// The condensation is not what this test is about.
	settings.Digest.Model = llm.Ref{}

	registry := llm.NewRegistry(nil)
	registry.Register(provider)
	scraper := research.NewScraper(st, research.ScrapeOptions{
		AllowPrivateHosts: true, RespectRobots: false, Timeout: 5 * time.Second,
	}, nil)
	log := &eventLog{}
	engine := NewEngine(st, registry, tools.New(st, nil, scraper),
		func() config.Settings { return settings }, log.emit)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Was kostet ein Speicher?"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 10*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	msgs, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	// The tool result the model read names the page's number, which is what makes it
	// citable at all.
	toolText := ""
	answer := store.Message{}
	for _, m := range msgs {
		if m.Role == string(llm.RoleTool) {
			toolText = m.Content
		}
		if m.Role == string(llm.RoleAssistant) && m.Content != "" {
			answer = m
		}
	}
	if !strings.Contains(toolText, "Quelle [1]: "+srv.URL) {
		t.Fatalf("the scraped page was not numbered in the result the model read:\n%s",
			truncateForTest(toolText, 300))
	}
	if answer.ID == "" {
		t.Fatal("no answer was stored")
	}
	if len(answer.Sources) != 1 {
		t.Fatalf("the answer carries %d sources, want one: %+v", len(answer.Sources), answer.Sources)
	}
	source := answer.Sources[0]
	if source.Index != 1 || source.URL != srv.URL || source.Title == "" {
		t.Errorf("the stored source is %+v, want [1] with the page's title and URL", source)
	}
}

// TestASearchContinuesTheSeededNumbering is the mixed turn: a pre-flight search numbers
// its results, and the tool search the model runs afterwards must continue from there —
// otherwise the answer's [7] points at a different site depending on which list is read.
func TestASearchContinuesTheSeededNumbering(t *testing.T) {
	sources := tools.NewSourceList()
	seeded := renderResultListing(research.SearchOutcome{
		Provider: "brave",
		Results: []research.SearchResult{
			{Title: "Erster Treffer", URL: "https://example.de/1"},
			{Title: "Zweiter Treffer", URL: "https://example.de/2"},
		},
	}, sources)
	if !strings.Contains(seeded, "[1] Erster Treffer") || !strings.Contains(seeded, "[2] Zweiter Treffer") {
		t.Fatalf("the seeded list does not number from 1:\n%s", seeded)
	}

	// What a later search_web call sees, given the same turn list.
	if got := sources.Add("Dritter Treffer", "https://example.de/3"); got != 3 {
		t.Fatalf("the first tool result of a seeded turn numbered %d, want 3", got)
	}
	// And a page that was already in the seeded list keeps its seeded number when read.
	if got := sources.Add("Erster Treffer", "https://example.de/1"); got != 1 {
		t.Errorf("re-reading a seeded source numbered %d, want its seeded 1", got)
	}
}
