package chat

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/llm"
	"kichat/internal/store"
)

// TestLiveEngineTurn runs one real turn through the whole stack — prompt
// assembly, the LM Studio provider, usage accounting, persistence and the
// Measurement that drives the live context meter. Skipped unless KICHAT_LIVE=1.
//
// This is the check that proves the meter can show a *measured* number: if the
// provider ever stops asking for stream usage, Measurement.PromptTokens is 0
// and the UI can only ever display an estimate.
func TestLiveEngineTurn(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to run against a live backend")
	}
	base := os.Getenv("KICHAT_LMSTUDIO")
	if base == "" {
		base = "http://localhost:1234"
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "live.db"))
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
	t.Logf("using model %q", chosen)

	settings := config.Default()
	settings.AutoTitle = false
	settings.NativeTools = true

	var mu sync.Mutex
	events := map[string]int{}
	var done map[string]any
	engine := NewEngine(st, registry, nil, func() config.Settings { return settings },
		func(name string, payload any) {
			mu.Lock()
			defer mu.Unlock()
			events[name]++
			if name == "chat:done" {
				done, _ = payload.(map[string]any)
			}
		})

	now := time.Now()
	chat := store.Chat{
		ID: "live-1", Title: "Live", CreatedAt: now, UpdatedAt: now,
		Backend: "lmstudio", Model: chosen,
		SystemPrompt: "Du bist ein knapper Assistent. Antworte in einem Satz.",
		Temperature:  0.6,
		TopP:         0.9,
		AutoCompact:  false,
		NumCtx:       48128,
	}
	if err := st.CreateChat(chat); err != nil {
		t.Fatalf("create chat: %v", err)
	}

	if err := engine.Send(TurnRequest{ChatID: chat.ID, Text: "Was ist die Hauptstadt von Frankreich?"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if !engine.IsRunning(chat.ID) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if engine.IsRunning(chat.ID) {
		t.Fatal("turn did not finish within 5 minutes")
	}

	mu.Lock()
	mu.Unlock()
	t.Logf("events: %v", events)

	if done == nil {
		t.Fatalf("no chat:done event; events=%v", events)
	}
	if status, _ := done["status"].(string); status != store.StatusComplete {
		t.Errorf("status = %q, want %q (full done payload: %v)", status, store.StatusComplete, done)
	}

	usage, _ := done["usage"].(*llm.Usage)
	if usage == nil {
		t.Fatalf("chat:done carried no usage: the context meter cannot show a measured size")
	}
	t.Logf("usage: prompt=%d completion=%d total=%d", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	if usage.PromptTokens <= 0 {
		t.Errorf("promptTokens = %d, want a measured value > 0", usage.PromptTokens)
	}

	msgs, err := st.ListMessages(chat.ID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("expected user+assistant, got %d messages", len(msgs))
	}
	last := msgs[len(msgs)-1]
	t.Logf("assistant: role=%s status=%s content=%q", last.Role, last.Status, truncate(last.Content, 160))
	if last.Role != "assistant" || last.Content == "" {
		t.Errorf("assistant reply not persisted correctly")
	}

	m, ok := engine.Measurement(chat.ID)
	if !ok {
		t.Errorf("no Measurement recorded — the meter falls back to estimation")
	} else {
		t.Logf("measurement: backend=%s model=%s promptTokens=%d seq=%d",
			m.Backend, m.Model, m.PromptTokens, m.Seq)
		if m.PromptTokens <= 0 {
			t.Errorf("Measurement.PromptTokens = %d, want > 0", m.PromptTokens)
		}
		if m.Model != chosen {
			t.Errorf("Measurement.Model = %q, want %q", m.Model, chosen)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
