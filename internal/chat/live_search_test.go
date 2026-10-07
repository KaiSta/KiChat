package chat

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/llm"
	"kichat/internal/research"
	"kichat/internal/store"
	"kichat/internal/tools"
)

// TestLiveSearchFirstTurn is the whole research path against the real services:
// settings → chain → Brave LLM context → seeded system message → prompt →
// LM Studio. The offline tests pin the pieces; only this proves the text the
// model actually reads contains page content rather than headlines, and that
// the LLM-context route really does beat the plain result listing it replaces
// instead of merely being assumed to.
func TestLiveSearchFirstTurn(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to run against live services")
	}
	key := strings.TrimSpace(os.Getenv("KICHAT_BRAVE_KEY"))
	if key == "" {
		t.Skip("set KICHAT_BRAVE_KEY to run the live search turn")
	}
	base := os.Getenv("KICHAT_LMSTUDIO")
	if base == "" {
		base = "http://localhost:1234"
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "live-search.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	registry := llm.NewRegistry([]llm.BackendConfig{{
		ID: "lmstudio", Name: "LM Studio", Kind: "lmstudio", BaseURL: base, Enabled: true,
	}})
	provider, ok := registry.Provider("lmstudio")
	if !ok {
		t.Fatal("lmstudio provider not constructed")
	}
	models, err := provider.ListModels(t.Context())
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	chosen := ""
	for _, m := range models {
		if m.State == llm.StateLoaded && (m.Type == "llm" || m.Type == "vlm") {
			chosen = m.ID
			break
		}
	}
	if chosen == "" {
		t.Skipf("no loaded chat model in LM Studio (%d models); load one and re-run", len(models))
	}

	chain := research.NewChain(st, research.Options{
		ProviderOrder: []string{"brave", "duckduckgo"},
		BraveAPIKey:   key,
		Country:       "DE",
		SearchLang:    "de",
		ResultCount:   8,
		CacheMinutes:  60,
	}, "")
	service := tools.New(st, chain, research.NewScraper(st, research.ScrapeOptions{Timeout: 20 * time.Second}, nil))

	// The engine asks for settings on every turn, so the test flips the search
	// mode between the two turns through this shared value.
	var settingsMu sync.Mutex
	settings := config.Default()
	settings.AutoTitle = false
	settings.Search.Country = "DE"
	settings.Search.SearchLang = "de"
	engine := NewEngine(st, registry, service,
		func() config.Settings {
			settingsMu.Lock()
			defer settingsMu.Unlock()
			return settings
		},
		func(string, any) {})

	question := "Wie tief ist das Mittelmeer an seiner tiefsten Stelle?"

	grounded := runSearchTurn(t, st, engine, &settingsMu, &settings, chosen, question, true)
	listing := runSearchTurn(t, st, engine, &settingsMu, &settings, chosen, question, false)

	t.Logf("LLM context seeded %d chars; result listing seeded %d chars", len(grounded), len(listing))
	if len(grounded) <= len(listing) {
		t.Errorf("the LLM-context payload (%d chars) is not larger than the headline listing (%d chars)",
			len(grounded), len(listing))
	}
	if !strings.Contains(grounded, "brave-context") {
		t.Errorf("seeded message does not name the context provider:\n%.400s", grounded)
	}
	if !strings.Contains(grounded, "Budget:") {
		t.Errorf("seeded message does not report the token budget:\n%.400s", grounded)
	}
	if !strings.Contains(grounded, "unvertrauenswürdige") {
		t.Error("the untrusted-content notice is missing from the seeded message")
	}
	if n := strings.Count(grounded, "URL: "); n < 3 {
		t.Errorf("only %d cited sources in the seeded message", n)
	}
}

// runSearchTurn sends one question with search-first enabled and returns the
// seeded search message the engine wrote.
func runSearchTurn(t *testing.T, st *store.Store, engine *Engine,
	mu *sync.Mutex, settings *config.Settings, model, question string, useContext bool) string {
	t.Helper()

	mu.Lock()
	settings.Search.UseLLMContext = useContext
	mu.Unlock()

	id := "live-listing"
	if useContext {
		id = "live-ctx"
	}
	now := time.Now()
	chat := store.Chat{
		ID:           id,
		Title:        "Live search",
		CreatedAt:    now,
		UpdatedAt:    now,
		Backend:      "lmstudio",
		Model:        model,
		SystemPrompt: "Antworte knapp in einem Satz und nenne die Zahl.",
		Temperature:  0.3,
		TopP:         0.9,
		AutoCompact:  false,
		NumCtx:       48128,
		SearchFirst:  true,
	}
	if err := st.CreateChat(chat); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if err := engine.Send(TurnRequest{ChatID: chat.ID, Text: question}); err != nil {
		t.Fatalf("send: %v", err)
	}

	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) && engine.IsRunning(chat.ID) {
		time.Sleep(50 * time.Millisecond)
	}
	if engine.IsRunning(chat.ID) {
		t.Fatalf("turn %q did not finish in time", chat.ID)
	}

	msgs, err := st.ListMessages(chat.ID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	seeded := ""
	for _, m := range msgs {
		if m.Role == "system" && m.Name == "search" {
			seeded = m.Content
		}
		t.Logf("  [%s/%s] %.110s", m.Role, m.Name, strings.ReplaceAll(m.Content, "\n", " "))
	}
	if seeded == "" {
		t.Fatalf("no seeded search message in %q — the pre-flight research did not run", chat.ID)
	}
	return seeded
}
