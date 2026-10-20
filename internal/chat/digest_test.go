package chat

import (
	"context"
	"errors"
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

// digestSettings is a chat on backend "fake" that has asked for its tool results to be
// condensed, with a smaller model on "digester" writing them. The digest model is
// deliberately a different backend from the chat's: that is the configuration the
// feature exists for, and the one where the condensation does not spend the chat
// backend's own allowance.
func digestSettings() config.Settings {
	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = 3
	settings.Backends = []llm.BackendConfig{
		{ID: "fake", Name: "Fake", Kind: "custom", Enabled: true, Digest: true},
	}
	settings.Digest = config.DigestSettings{
		Model:     llm.Ref{Backend: "digester", Model: "small"},
		Threshold: 500,
	}
	return settings
}

func newDigester(replies ...[]llm.StreamEvent) *fakeProvider {
	return &fakeProvider{
		id:      "digester",
		name:    "Digester",
		model:   llm.ModelInfo{ID: "small", Backend: "digester", ContextLimit: 32768},
		replies: replies,
	}
}

func newDigestEngine(t *testing.T, st *store.Store, settings config.Settings,
	providers ...*fakeProvider) (*Engine, *eventLog) {
	t.Helper()
	registry := llm.NewRegistry(nil)
	for _, p := range providers {
		registry.Register(p)
	}
	log := &eventLog{}
	return NewEngine(st, registry, tools.New(st, nil, nil),
		func() config.Settings { return settings }, log.emit), log
}

func scrapeCall(url string) llm.ToolCall {
	return llm.ToolCall{
		ID:        "call-1",
		Name:      tools.ToolScrapeURL,
		Arguments: fmt.Sprintf(`{"url": %q}`, url),
	}
}

func bigPage() string {
	// ~6,000 tokens of the same paragraph: far past the 500-token threshold and past
	// anything a per-request allowance would take whole.
	return strings.Repeat("Ein langer Absatz mit Inhalt. ", 1000)
}

// TestACondensationReplacesThePageTheModelReads pins the feature end to end: the model
// asks for a page, and the text that lands in its context is the condensation rather
// than the page. The size is the point — on a backend that refuses an oversized
// request this is the difference between reading the page and being told it was
// withheld — so the assertion is on the message the *next* request carries.
func TestACondensationReplacesThePageTheModelReads(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	page := `<!doctype html><html><head><title>Bericht</title></head><body><article><h1>Bericht</h1>` +
		strings.Repeat("<p>Ein langer Absatz mit Inhalt.</p>", 900) +
		`</article></body></html>`
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
			{{Type: llm.EventDelta, Text: "Antwort"}, {Type: llm.EventDone}},
		},
	}
	digester := newDigester([]llm.StreamEvent{
		{Type: llm.EventDelta, Text: "- Der Bericht besteht aus einem langen Absatz."},
		{Type: llm.EventDone},
	})

	settings := digestSettings()
	registry := llm.NewRegistry(nil)
	registry.Register(provider)
	registry.Register(digester)
	scraper := research.NewScraper(st, research.ScrapeOptions{
		AllowPrivateHosts: true, RespectRobots: false, Timeout: 5 * time.Second,
	}, nil)
	log := &eventLog{}
	engine := NewEngine(st, registry, tools.New(st, nil, scraper),
		func() config.Settings { return settings }, log.emit)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Lies den Bericht."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 10*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	if got := digester.requestCount(); got != 1 {
		t.Fatalf("digester calls = %d, want one condensation", got)
	}
	second := provider.requestAt(1)
	carried := ""
	for _, m := range second.Messages {
		if m.Role == llm.RoleTool {
			carried = m.Content
		}
	}
	if carried == "" {
		t.Fatal("the model never received the tool result")
	}
	if !strings.HasPrefix(carried, "[Zusammenfassung der Seite "+srv.URL) {
		t.Fatalf("the tool result does not say what it is: %q", truncateForTest(carried, 200))
	}
	if !strings.Contains(carried, "Der Bericht besteht aus einem langen Absatz.") {
		t.Fatalf("the model did not receive the condensation: %q", truncateForTest(carried, 200))
	}
	if strings.Contains(carried, "Ein langer Absatz mit Inhalt.") {
		t.Fatalf("the page reached the model despite the condensation (%d tokens)",
			llm.EstimateTokens(carried))
	}
	if tokens := llm.EstimateTokens(carried); tokens >= settings.Digest.Threshold {
		t.Errorf("the condensed result is %d tokens, past the %d-token threshold it was meant to fit",
			tokens, settings.Digest.Threshold)
	}
}

// TestTheToolCardIsToldHowMuchWasCut is the number a reader sees on the card. The page
// is condensed, so the record must carry both sizes — what the tool returned and what
// of it the model reads — and say that a second model was the cause. The second case is
// the same page under a threshold it cannot reach: one number, and no claim of a cut
// that did not happen. Both are asserted on what was emitted and on what was stored,
// because the card is drawn from the event and rebuilt from the row after a reload.
func TestTheToolCardIsToldHowMuchWasCut(t *testing.T) {
	page := `<!doctype html><html><head><title>Bericht</title></head><body><article><h1>Bericht</h1>` +
		strings.Repeat("<p>Ein langer Absatz mit Inhalt.</p>", 900) +
		`</article></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	}))
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name      string
		threshold int
		condensed bool
	}{
		{"past the threshold", 500, true},
		{"below the threshold", 1_000_000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			makeChat(t, st)
			call := fmt.Sprintf("```tool_call\n{\"name\": \"scrape_url\", \"arguments\": {\"url\": %q}}\n```", srv.URL)
			provider := &fakeProvider{
				model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
				replies: [][]llm.StreamEvent{
					{{Type: llm.EventDelta, Text: call}},
					{{Type: llm.EventDelta, Text: "Antwort"}, {Type: llm.EventDone}},
				},
			}
			digester := newDigester([]llm.StreamEvent{
				{Type: llm.EventDelta, Text: "- Ein langer Absatz."}, {Type: llm.EventDone},
			})
			settings := digestSettings()
			settings.Digest.Threshold = tc.threshold
			registry := llm.NewRegistry(nil)
			registry.Register(provider)
			registry.Register(digester)
			scraper := research.NewScraper(st, research.ScrapeOptions{
				AllowPrivateHosts: true, RespectRobots: false, Timeout: 5 * time.Second,
			}, nil)
			log := &eventLog{}
			engine := NewEngine(st, registry, tools.New(st, nil, scraper),
				func() config.Settings { return settings }, log.emit)

			if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Lies den Bericht."}); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if !waitFor(t, 10*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
				t.Fatal("the turn did not finish")
			}

			// The last report for the call is the one the card ends up showing: the first
			// is written while the tool runs, the last after the condensation.
			var emitted store.ToolCallRecord
			sawOK := false
			for _, ev := range log.snapshot() {
				if ev.name != "chat:tool" {
					continue
				}
				record, ok := ev.payload["toolCall"].(store.ToolCallRecord)
				if ok && record.Status == store.ToolOK {
					emitted, sawOK = record, true
				}
			}
			if !sawOK {
				t.Fatal("the tool call was never reported as finished")
			}
			stored := persistedToolCall(t, st)
			for name, record := range map[string]store.ToolCallRecord{"emitted": emitted, "stored": stored} {
				if record.SourceTokens == 0 {
					t.Fatalf("%s record carries no source size: %+v", name, record)
				}
				if record.Condensed != tc.condensed {
					t.Errorf("%s condensed = %v, want %v", name, record.Condensed, tc.condensed)
				}
				if record.Withheld {
					t.Errorf("%s withheld the result, but the page fitted", name)
				}
				if tc.condensed {
					if record.ContextTokens >= record.SourceTokens {
						t.Errorf("%s claims a cut that saved nothing: %d -> %d", name,
							record.SourceTokens, record.ContextTokens)
					}
					if record.ContextTokens == 0 {
						t.Errorf("%s condensed the page and recorded no size for what is read", name)
					}
				} else if record.ContextTokens != record.SourceTokens {
					t.Errorf("%s reports a cut it did not make: %d -> %d", name,
						record.SourceTokens, record.ContextTokens)
				}
			}
			if tc.condensed && digester.requestCount() != 1 {
				t.Errorf("digester calls = %d, want the one condensation the numbers describe",
					digester.requestCount())
			}
		})
	}
}

// TestAWithheldResultIsNotReportedAsASize is the case a reader hit: a search whose
// results never reached the model, because the answer's tool budget was already spent.
// The card used to print "4,374 → 41 tokens" for it — the 41 were the exhausted notice —
// which reads as a summary the second model wrote. The record must say the result was
// withheld, and the size the model actually read must not be the one it is compared to.
func TestAWithheldResultIsNotReportedAsASize(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body><article><p>`+strings.Repeat("Ein Absatz mit Inhalt. ", 400)+
			`</p></article></body></html>`)
	}))
	t.Cleanup(srv.Close)

	settings := digestSettings()
	// No summary model: this is the budget alone, which is what a spent budget looks
	// like in a chat that never switched the condensation on.
	settings.Digest.Model = llm.Ref{}
	scraper := research.NewScraper(st, research.ScrapeOptions{
		AllowPrivateHosts: true, RespectRobots: false, Timeout: 5 * time.Second,
	}, nil)
	log := &eventLog{}
	engine := NewEngine(st, llm.NewRegistry(nil), tools.New(st, nil, scraper),
		func() config.Settings { return settings }, log.emit)

	// One token of budget: nothing of the page can fit, so the model gets the notice.
	budget := &toolOutputBudget{remaining: 1}
	engine.runTool(context.Background(), "chat-1", store.Message{ID: "m1", ChatID: "chat-1"},
		scrapeCall(srv.URL), budget, tools.TurnRef{BackendID: "fake"})

	record := persistedToolCall(t, st)
	if record.SourceTokens == 0 {
		t.Fatal("the page's own size was not recorded, so the card has nothing to say")
	}
	if !record.Withheld {
		t.Errorf("a withheld result is reported as a size: %+v", record)
	}
	if record.Condensed {
		t.Error("a withheld result claims a condensation nobody wrote")
	}
	if record.ContextTokens >= record.SourceTokens {
		t.Errorf("a withheld result reports the page as its context size: %d >= %d",
			record.ContextTokens, record.SourceTokens)
	}
	// And the event the live card renders, not only the row.
	last := store.ToolCallRecord{}
	for _, ev := range log.snapshot() {
		if ev.name == "chat:tool" {
			if reported, ok := ev.payload["toolCall"].(store.ToolCallRecord); ok {
				last = reported
			}
		}
	}
	if !last.Withheld {
		t.Errorf("the emitted card does not know the result was withheld: %+v", last)
	}
	// The model was told the budget was spent, rather than handed a fragment of the
	// page that would read as the whole of it.
	msgs, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != exhaustedNotice {
		t.Fatalf("the model received %q, want the exhausted notice", msgs[0].Content)
	}
}

// persistedToolCall reads back the chat's one tool call, which is what a reopened chat
// builds its cards from.
func persistedToolCall(t *testing.T, st *store.Store) store.ToolCallRecord {
	t.Helper()
	records, err := st.ListToolCalls("chat-1")
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("tool calls = %d, want one", len(records))
	}
	return records[0]
}

// TestABackendWithoutTheSwitchKeepsTheResult keeps the opt-in honest: the same page,
// the same configured model, and one backend that never asked for the feature. The
// result must arrive uncondensed, and no request may be spent on it.
func TestABackendWithoutTheSwitchKeepsTheResult(t *testing.T) {
	st := newTestStore(t)
	settings := digestSettings()
	settings.Backends = []llm.BackendConfig{
		{ID: "fake", Name: "Fake", Kind: "custom", Enabled: true, Digest: false},
	}
	digester := newDigester([]llm.StreamEvent{{Type: llm.EventDelta, Text: "nie gelesen"}})
	engine, _ := newDigestEngine(t, st, settings, digester)

	out, _ := engine.condenseToolOutput(context.Background(), "chat-1", scrapeCall("https://example.com/a"),
		bigPage(), tools.TurnRef{BackendID: "fake"})

	if out != bigPage() {
		t.Fatal("the result was condensed for a backend that did not ask for it")
	}
	if got := digester.requestCount(); got != 0 {
		t.Fatalf("digester calls = %d, want none", got)
	}
}

// TestAnUnconfiguredDigestModelIsOff is the default an existing install gets: the
// backend switch may be on, but with no model chosen nothing happens — and in
// particular the chat's own model is never used for it, because that is the model
// whose request the condensation exists to shrink.
func TestAnUnconfiguredDigestModelIsOff(t *testing.T) {
	st := newTestStore(t)
	settings := digestSettings()
	settings.Digest.Model = llm.Ref{}
	digester := newDigester([]llm.StreamEvent{{Type: llm.EventDelta, Text: "nie gelesen"}})
	engine, _ := newDigestEngine(t, st, settings, digester)

	if out, _ := engine.condenseToolOutput(context.Background(), "chat-1", scrapeCall("https://example.com/a"),
		bigPage(), tools.TurnRef{BackendID: "fake"}); out != bigPage() {
		t.Fatal("a result was condensed with no model configured")
	}
	if got := digester.requestCount(); got != 0 {
		t.Fatalf("digester calls = %d, want none", got)
	}
}

// TestASmallResultIsNotCondensed pins the threshold: below it the result is handed
// over as it is, because the extra request would cost more in time than it saves.
func TestASmallResultIsNotCondensed(t *testing.T) {
	st := newTestStore(t)
	digester := newDigester([]llm.StreamEvent{{Type: llm.EventDelta, Text: "nie gelesen"}})
	engine, _ := newDigestEngine(t, st, digestSettings(), digester)

	small := "Ein kurzer Absatz."
	if out, _ := engine.condenseToolOutput(context.Background(), "chat-1", scrapeCall("https://example.com/a"),
		small, tools.TurnRef{BackendID: "fake"}); out != small {
		t.Fatal("a result below the threshold was condensed")
	}
	if got := digester.requestCount(); got != 0 {
		t.Fatalf("digester calls = %d, want none", got)
	}
}

// TestAnEmptyThresholdMeansTheDefault keeps a save from turning the feature into a
// request per tool call. A save persists the draft as it arrived, so a numeric field
// the user emptied reaches the running app as 0; read literally that would condense a
// five-token error message, every time.
func TestAnEmptyThresholdMeansTheDefault(t *testing.T) {
	st := newTestStore(t)
	settings := digestSettings()
	settings.Digest.Threshold = 0
	digester := newDigester([]llm.StreamEvent{{Type: llm.EventDelta, Text: "nie gelesen"}})
	engine, _ := newDigestEngine(t, st, settings, digester)

	small := "Ein kurzer Absatz."
	if out, _ := engine.condenseToolOutput(context.Background(), "chat-1", scrapeCall("https://example.com/a"),
		small, tools.TurnRef{BackendID: "fake"}); out != small {
		t.Fatal("a zero threshold condensed a result far below the default")
	}
	if got := digester.requestCount(); got != 0 {
		t.Fatalf("digester calls = %d, want none", got)
	}
}

// TestAFailedCondensationStillDeliversTheResult is the rule that makes the feature
// safe to switch on: the model that writes the condensation is an optimisation, and
// its failure must cost the chat nothing but tokens. The page is delivered, and the
// turn is told why it is large.
func TestAFailedCondensationStillDeliversTheResult(t *testing.T) {
	st := newTestStore(t)
	digester := &fakeProvider{
		id:       "digester",
		model:    llm.ModelInfo{ID: "small", Backend: "digester", ContextLimit: 32768},
		refusals: []error{errors.New("das Modell ist nicht erreichbar")},
	}
	engine, log := newDigestEngine(t, st, digestSettings(), digester)

	out, _ := engine.condenseToolOutput(context.Background(), "chat-1", scrapeCall("https://example.com/a"),
		bigPage(), tools.TurnRef{BackendID: "fake"})

	if out != bigPage() {
		t.Fatal("a failed condensation lost the result")
	}
	warned := false
	for _, ev := range log.snapshot() {
		if ev.name == "chat:warning" && strings.Contains(fmt.Sprint(ev.payload["warning"]), "nicht gekürzt") {
			warned = true
		}
	}
	if !warned {
		t.Error("a failed condensation was reported as nothing at all")
	}
}

// TestTheSecondReadIsTheEscapeHatch pins the way back to the full text. A summary is
// lossy, so a model that needs a quotation asks for the same page again — and that
// request must hand over the page, not the same summary, or the escape closes and the
// model is answered with a condensation of what it just failed to read.
func TestTheSecondReadIsTheEscapeHatch(t *testing.T) {
	st := newTestStore(t)
	digester := newDigester([]llm.StreamEvent{{Type: llm.EventDelta, Text: "Kurzfassung"}})
	engine, _ := newDigestEngine(t, st, digestSettings(), digester)
	call := scrapeCall("https://example.com/a")

	first, _ := engine.condenseToolOutput(context.Background(), "chat-1", call, bigPage(),
		tools.TurnRef{BackendID: "fake"})
	if !strings.Contains(first, "Kurzfassung") {
		t.Fatalf("the first read was not condensed: %q", truncateForTest(first, 120))
	}
	second, _ := engine.condenseToolOutput(context.Background(), "chat-1", call, bigPage(),
		tools.TurnRef{BackendID: "fake"})
	if second != bigPage() {
		t.Fatal("the second read of the same page did not hand over the full text")
	}
	if got := digester.requestCount(); got != 1 {
		t.Fatalf("digester calls = %d, want one — the escape hatch must not cost a second condensation", got)
	}
}

// TestACondensationIsReusedAcrossChats pins the cache: the same page in a different
// chat is condensed for free, which is what makes a page read twice cost one request
// rather than two. The ledger is per chat, so the second chat still tells its model
// that this is a summary.
func TestACondensationIsReusedAcrossChats(t *testing.T) {
	st := newTestStore(t)
	digester := newDigester([]llm.StreamEvent{{Type: llm.EventDelta, Text: "Kurzfassung"}})
	engine, _ := newDigestEngine(t, st, digestSettings(), digester)
	call := scrapeCall("https://example.com/a")

	_, _ = engine.condenseToolOutput(context.Background(), "chat-1", call, bigPage(), tools.TurnRef{BackendID: "fake"})
	other, _ := engine.condenseToolOutput(context.Background(), "chat-2", call, bigPage(), tools.TurnRef{BackendID: "fake"})

	if !strings.Contains(other, "Kurzfassung") {
		t.Fatalf("the second chat did not receive the cached condensation: %q", truncateForTest(other, 120))
	}
	if got := digester.requestCount(); got != 1 {
		t.Fatalf("digester calls = %d, want one for the same page", got)
	}
}

// TestADocumentIsNotCondensed keeps the scope of the feature where it belongs. A
// document is already chunked and read by range, so a condensation would throw away
// the precision retrieval exists to deliver.
func TestADocumentIsNotCondensed(t *testing.T) {
	st := newTestStore(t)
	digester := newDigester([]llm.StreamEvent{{Type: llm.EventDelta, Text: "nie gelesen"}})
	engine, _ := newDigestEngine(t, st, digestSettings(), digester)

	call := llm.ToolCall{ID: "call-1", Name: tools.ToolReadPages,
		Arguments: `{"document":"bericht","from":0,"to":3}`}
	if out, _ := engine.condenseToolOutput(context.Background(), "chat-1", call, bigPage(),
		tools.TurnRef{BackendID: "fake"}); out != bigPage() {
		t.Fatal("a document chunk was condensed")
	}
	if got := digester.requestCount(); got != 0 {
		t.Fatalf("digester calls = %d, want none", got)
	}
}

// TestACondensationIsKeyedByTheWholeModelReference pins the cache key's identity. Two
// backends can expose the same model id — a local file named like a hosted model, two
// vendors reselling the same weights — and their condensations are not
// interchangeable, so the cache must not serve one for the other. The content hash is
// the other half of the key, and it is what makes a changed page a different page.
func TestACondensationIsKeyedByTheWholeModelReference(t *testing.T) {
	a := digestKey(llm.Ref{Backend: "groq", Model: "same-id"}, 600, "die Seite")
	b := digestKey(llm.Ref{Backend: "nvidia", Model: "same-id"}, 600, "die Seite")
	if a == b {
		t.Fatal("the same model id on two backends shares a condensation")
	}
	if again := digestKey(llm.Ref{Backend: "groq", Model: "same-id"}, 600, "die Seite"); again != a {
		t.Error("the same page and model produced a different key")
	}
	if changed := digestKey(llm.Ref{Backend: "groq", Model: "same-id"}, 600, "die Seite, geändert"); changed == a {
		t.Error("a changed page inherited the old condensation")
	}
}

// TestTheConfiguredCapIsWhatTheModelIsAskedFor pins the setting on the wire. The cap
// is the summary's length, so it has to be the request's `max_tokens` — a value read
// into the settings and never sent would leave the panel lying about what it does.
func TestTheConfiguredCapIsWhatTheModelIsAskedFor(t *testing.T) {
	st := newTestStore(t)
	settings := digestSettings()
	settings.Digest.MaxTokens = 180
	digester := newDigester([]llm.StreamEvent{{Type: llm.EventDelta, Text: "Kurzfassung"}})
	engine, _ := newDigestEngine(t, st, settings, digester)

	_, _ = engine.condenseToolOutput(context.Background(), "chat-1", scrapeCall("https://example.com/a"),
		bigPage(), tools.TurnRef{BackendID: "fake"})

	if got := digester.requestCount(); got != 1 {
		t.Fatalf("digester calls = %d, want one condensation", got)
	}
	if got := digester.requestAt(0).MaxTokens; got != 180 {
		t.Errorf("max_tokens = %d, want the configured 180", got)
	}
}

// TestACondensationStripsInlineThinking pins the reasoning-model measure a fold
// already takes and a condensation must not lose: a backend with no separate
// reasoning channel inlines the deliberation in the content, and the chat must not
// receive it. It is the same stripThinking call, because both go through
// extractWithBudget — this test is what keeps that true.
func TestACondensationStripsInlineThinking(t *testing.T) {
	st := newTestStore(t)
	digester := newDigester([]llm.StreamEvent{
		{Type: llm.EventDelta, Text: "<think>Der Nutzer will nur die Zahlen.</think>- Kurzfassung der Seite."},
		{Type: llm.EventDone},
	})
	engine, _ := newDigestEngine(t, st, digestSettings(), digester)

	out, _ := engine.condenseToolOutput(context.Background(), "chat-1", scrapeCall("https://example.com/a"),
		bigPage(), tools.TurnRef{BackendID: "fake"})

	if !strings.Contains(out, "Kurzfassung der Seite.") {
		t.Fatalf("the summary was lost with the thinking: %q", truncateForTest(out, 200))
	}
	if strings.Contains(out, "Der Nutzer will nur die Zahlen") {
		t.Fatalf("the model's private thinking reached the chat: %q", truncateForTest(out, 200))
	}
	if got := digester.requestCount(); got != 1 {
		t.Fatalf("digester calls = %d, want one: stripping the thinking is not a retry", got)
	}
}

// TestACondensationRetriesAnAnswerThatWasOnlyThinking is the other half: an answer
// made of nothing but an inline thinking block is not a summary, it is the
// reasoning-budget failure. It must be retried with room for that thinking, exactly
// as a fold is — a 600-token cap is tight enough that this is the likely outcome for
// a small thinking model, not an exotic one.
func TestACondensationRetriesAnAnswerThatWasOnlyThinking(t *testing.T) {
	st := newTestStore(t)
	onlyThinking := []llm.StreamEvent{
		{Type: llm.EventDelta, Text: "<think>" + strings.Repeat("ich überlege die Struktur der Seite. ", 20) + "</think>"},
		{Type: llm.EventDone, Stats: &llm.GenerationStats{StopReason: "length"}},
	}
	digester := newDigester(onlyThinking, []llm.StreamEvent{
		{Type: llm.EventDelta, Text: "Kurzfassung"},
		{Type: llm.EventDone},
	})
	engine, _ := newDigestEngine(t, st, digestSettings(), digester)

	out, _ := engine.condenseToolOutput(context.Background(), "chat-1", scrapeCall("https://example.com/a"),
		bigPage(), tools.TurnRef{BackendID: "fake"})

	if !strings.Contains(out, "Kurzfassung") {
		t.Fatalf("a thinking-only answer was accepted as a summary: %q", truncateForTest(out, 200))
	}
	if got := digester.requestCount(); got != 2 {
		t.Fatalf("digester calls = %d, want a retry with room for the thinking", got)
	}
	first, second := digester.requestAt(0).MaxTokens, digester.requestAt(1).MaxTokens
	if second <= first {
		t.Errorf("retry budget %d is not larger than the first attempt's %d: the thinking is what ate it",
			second, first)
	}
}

// TestTheCapIsHeldUnderTheWindow pins the other half of making the cap a setting. A
// backend validates the prompt and the output allowance together, so a large cap beside
// a page that fills a small window is an oversized request, refused before a token is
// generated. That was theoretical while the cap was 600; at 8000 it is the first thing a
// small local model sees when someone raises the setting. The cap shrinks to the room
// that is left, and the page — not the cap — decides the rest.
func TestTheCapIsHeldUnderTheWindow(t *testing.T) {
	st := newTestStore(t)
	settings := digestSettings()
	settings.Digest.MaxTokens = 8000
	digester := newDigester([]llm.StreamEvent{{Type: llm.EventDelta, Text: "Kurzfassung"}})
	// A window just large enough for the page and the prompt, and nowhere near the cap.
	digester.model.ContextLimit = 2000
	engine, _ := newDigestEngine(t, st, settings, digester)

	// Roughly 1,200 tokens: above the 500-token threshold, below the window.
	page := strings.Repeat("Ein langer Absatz mit Inhalt. ", 200)
	_, _ = engine.condenseToolOutput(context.Background(), "chat-1", scrapeCall("https://example.com/a"),
		page, tools.TurnRef{BackendID: "fake"})

	if got := digester.requestCount(); got != 1 {
		t.Fatalf("digester calls = %d, want one condensation", got)
	}
	req := digester.requestAt(0)
	room := 2000 - llm.EstimateMessagesTokens(req.Messages)
	if room <= 0 {
		t.Fatalf("the page does not fit the window at all (%d tokens), so the test proves nothing",
			llm.EstimateMessagesTokens(req.Messages))
	}
	if req.MaxTokens > room {
		t.Errorf("max_tokens = %d, want at most the %d tokens the window has left", req.MaxTokens, room)
	}
	if req.MaxTokens == 8000 {
		t.Error("the configured cap was sent whole beside a page that fills the window")
	}
}

// TestAChangedCapDoesNotReuseTheOldSummary pins the cap as part of the cache key. A
// summary written to answer in 600 tokens is not the summary the same model gives at
// 2000, so raising the setting has to miss the cache — otherwise the new value looks
// applied while every already-read page keeps the old length, for up to 30 days.
func TestAChangedCapDoesNotReuseTheOldSummary(t *testing.T) {
	st := newTestStore(t)
	settings := digestSettings()
	settings.Digest.MaxTokens = 600
	digester := newDigester(
		[]llm.StreamEvent{{Type: llm.EventDelta, Text: "Kurzfassung"}},
		[]llm.StreamEvent{{Type: llm.EventDelta, Text: "Längere Fassung"}},
	)
	engine, _ := newDigestEngine(t, st, settings, digester)
	call := scrapeCall("https://example.com/a")

	_, _ = engine.condenseToolOutput(context.Background(), "chat-1", call, bigPage(), tools.TurnRef{BackendID: "fake"})
	// A fresh engine on the same store, which is what a save produces: the settings a
	// running engine reads are the ones it was built with. The second chat is what
	// keeps the per-chat escape hatch out of the answer.
	changed := settings
	changed.Digest.MaxTokens = 1200
	next, _ := newDigestEngine(t, st, changed, digester)
	other, _ := next.condenseToolOutput(context.Background(), "chat-2", call, bigPage(),
		tools.TurnRef{BackendID: "fake"})

	if got := digester.requestCount(); got != 2 {
		t.Fatalf("digester calls = %d, want a second request for the new cap", got)
	}
	if !strings.Contains(other, "Längere Fassung") {
		t.Fatalf("the raised cap served the old length: %q", truncateForTest(other, 120))
	}
	if got := digester.requestAt(1).MaxTokens; got != 1200 {
		t.Errorf("max_tokens = %d, want the new 1200", got)
	}
}

// truncateForTest keeps a failure message readable when the value under test is a
// page.
func truncateForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
