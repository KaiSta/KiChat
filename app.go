package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"kichat/internal/chat"
	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/ingest"
	"kichat/internal/llm"
	"kichat/internal/research"
	"kichat/internal/secret"
	"kichat/internal/store"
	"kichat/internal/tools"
)

// Event names live in internal/events so the emitters and the frontend's
// subscriber list cannot drift apart; see that package for why.

// App is the bound Wails application object.
type App struct {
	ctx context.Context

	initOnce sync.Once
	initErr  error

	store     *store.Store
	registry  *llm.Registry
	engine    *chat.Engine
	toolSvc   *tools.Service
	chain     *research.Chain
	scraper   *research.Scraper
	extractor *ingest.Extractor
	secrets   *secret.Store

	settingsMu sync.RWMutex
	settings   config.Settings

	dataDir string
}

// NewApp creates the application.
func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if err := a.ensure(); err != nil {
		runtime.LogErrorf(ctx, "Initialisierung fehlgeschlagen: %v", err)
	}
}

func (a *App) shutdown(ctx context.Context) {
	if a.engine != nil {
		a.engine.StopAll()
	}
	if a.store != nil {
		a.store.Close()
	}
}

// ensure performs one-time initialisation. Every bound method calls it, so the
// app works even if startup has not completed yet.
func (a *App) ensure() error {
	a.initOnce.Do(func() {
		a.initErr = a.init()
	})
	return a.initErr
}

func (a *App) init() error {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	a.dataDir = filepath.Join(dir, "kichat")
	if err := os.MkdirAll(a.dataDir, 0o755); err != nil {
		return fmt.Errorf("Datenverzeichnis anlegen: %w", err)
	}

	st, err := store.Open(filepath.Join(a.dataDir, "kichat.db"))
	if err != nil {
		return err
	}
	a.store = st

	settings, err := config.Load(st)
	if err != nil {
		// A corrupt settings blob should not stop the app from starting.
		settings = config.Default()
	}

	secrets, err := secret.New(filepath.Join(a.dataDir, "secrets"))
	if err != nil {
		return err
	}
	a.secrets = secrets

	// The Brave key lives in the keyring; settings only mirror whether it is set.
	if stored, ok := secrets.Get("brave"); ok {
		settings.Search.BraveAPIKey = stored
	}

	a.settingsMu.Lock()
	a.settings = settings
	a.settingsMu.Unlock()

	a.extractor = ingest.New()
	a.rebuildServices(settings)
	return nil
}

// rebuildServices (re)creates the objects that depend on settings.
func (a *App) rebuildServices(settings config.Settings) {
	a.registry = llm.NewRegistry(settings.Backends)
	a.chain = research.NewChain(a.store, research.Options{
		ProviderOrder: settings.Search.ProviderOrder,
		BraveAPIKey:   settings.Search.BraveAPIKey,
		Country:       settings.Search.Country,
		SearchLang:    settings.Search.SearchLang,
		ResultCount:   settings.Search.ResultCount,
		ExtraSnippets: settings.Search.ExtraSnippets,
		CacheMinutes:  settings.Search.CacheMinutes,
	}, "")

	a.scraper = research.NewScraper(a.store, research.ScrapeOptions{
		MaxBytes:          settings.Search.ScrapeMaxBytes,
		Timeout:           time.Duration(settings.Search.ScrapeTimeoutSeconds) * time.Second,
		RespectRobots:     settings.Search.ScrapeRespectRobots,
		AllowPrivateHosts: settings.Search.ScrapeAllowPrivateHosts,
		CacheHours:        settings.Search.ScrapeCacheHours,
	}, a.extractor.PDFExtractorFunc())

	a.toolSvc = tools.New(a.store, a.chain, a.scraper)
	a.engine = chat.NewEngine(a.store, a.registry, a.toolSvc, a.Settings, a.emit)
}

func (a *App) emit(name string, payload any) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, name, payload)
}

// Settings returns a snapshot of the current settings.
func (a *App) Settings() config.Settings {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.settings
}

// --- chat API --------------------------------------------------------------

// SendMessageRequest is the payload for SendMessage.
type SendMessageRequest struct {
	ChatID        string   `json:"chatId"`
	Text          string   `json:"text"`
	AttachmentIDs []string `json:"attachmentIds"`
	Backend       string   `json:"backend"`
	Model         string   `json:"model"`
	Regenerate    bool     `json:"regenerate"`
}

// SendMessage starts a generation.
func (a *App) SendMessage(req SendMessageRequest) error {
	if err := a.ensure(); err != nil {
		return err
	}
	if req.Regenerate {
		return a.Regenerate(req.ChatID, req.Backend, req.Model)
	}
	return a.engine.Send(chat.TurnRequest{
		ChatID:        req.ChatID,
		Text:          req.Text,
		AttachmentIDs: req.AttachmentIDs,
		Backend:       req.Backend,
		Model:         req.Model,
	})
}

// Regenerate re-runs the last user turn, optionally on a different model.
func (a *App) Regenerate(chatID, backend, model string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	return a.engine.Send(chat.TurnRequest{
		ChatID:     chatID,
		Backend:    backend,
		Model:      model,
		Regenerate: true,
	})
}

// StopGeneration cancels a running generation, keeping the partial reply.
func (a *App) StopGeneration(chatID string) bool {
	if err := a.ensure(); err != nil {
		return false
	}
	return a.engine.Stop(chatID)
}

// ActiveGenerations lists chats currently generating.
func (a *App) ActiveGenerations() []string {
	if err := a.ensure(); err != nil {
		return nil
	}
	return a.engine.ActiveChats()
}

// ListChats returns all chats, most recent first.
func (a *App) ListChats(includeArchived bool) ([]store.Chat, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	return a.store.ListChats(includeArchived)
}

// CreateChat makes a new chat, defaulting to the first available model.
func (a *App) CreateChat(backend, model string) (store.Chat, error) {
	if err := a.ensure(); err != nil {
		return store.Chat{}, err
	}
	settings := a.Settings()
	if backend == "" || model == "" {
		backend, model = a.firstAvailableModel()
	}
	now := time.Now()
	newChat := store.Chat{
		ID:                 newID(),
		Title:              "",
		CreatedAt:          now,
		UpdatedAt:          now,
		Backend:            backend,
		Model:              model,
		SystemPrompt:       settings.DefaultSystemPrompt,
		SystemPromptPreset: config.PresetResearch,
		Temperature:        settings.Sampler.Temperature,
		TopP:               settings.Sampler.TopP,
		AutoCompact:        true,
		CompactThreshold:   settings.Compact.Threshold,
		KeepRecentTurns:    settings.Compact.KeepRecentTurns,
	}
	if err := a.store.CreateChat(newChat); err != nil {
		return store.Chat{}, err
	}
	a.emit(events.ChatsChanged, map[string]any{})
	return newChat, nil
}

// firstAvailableModel picks a sensible default, preferring a loaded model.
func (a *App) firstAvailableModel() (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	models := a.visibleModels(ctx)
	for _, m := range models {
		if m.State == llm.StateLoaded && m.Type != "embeddings" {
			return m.Backend, m.ID
		}
	}
	for _, m := range models {
		if m.Type != "embeddings" {
			return m.Backend, m.ID
		}
	}
	return "", ""
}

// visibleModels is the merged catalogue with user overrides applied and
// embedding-only models filtered out of the picker.
func (a *App) visibleModels(ctx context.Context) []llm.ModelInfo {
	settings := a.Settings()
	models := llm.MergeOverrides(a.registry.ListModels(ctx, false), settings.ModelOverrides)
	out := make([]llm.ModelInfo, 0, len(models))
	for _, m := range models {
		if m.Type == "embeddings" {
			continue
		}
		// Fill in capability hints when a backend reports none, so the UI can
		// still show something useful instead of a blank badge row.
		if len(m.Capabilities) == 0 {
			m.Capabilities = inferCapabilities(m)
		}
		out = append(out, m)
	}
	return out
}

// inferCapabilities guesses capabilities when a backend does not report them.
// The guess only affects the badges and whether native tool calling is used; the
// text-protocol fallback still works either way.
func inferCapabilities(m llm.ModelInfo) []string {
	caps := []string{}
	if m.Type == "vlm" {
		caps = append(caps, llm.CapVision)
	}
	return caps
}

// GetChat loads one chat.
func (a *App) GetChat(chatID string) (store.Chat, error) {
	if err := a.ensure(); err != nil {
		return store.Chat{}, err
	}
	return a.store.GetChat(chatID)
}

// UpdateChat persists chat settings (model, system prompt, samplers, flags).
func (a *App) UpdateChat(c store.Chat) (store.Chat, error) {
	if err := a.ensure(); err != nil {
		return store.Chat{}, err
	}
	existing, err := a.store.GetChat(c.ID)
	if err != nil {
		return store.Chat{}, err
	}
	// A model or backend change invalidates the recorded prompt size, because a
	// different tokenizer makes the old count meaningless.
	if existing.Backend != c.Backend || existing.Model != c.Model {
		a.engine.InvalidateMeasurement(c.ID)
	}
	if err := a.store.UpdateChat(c); err != nil {
		return store.Chat{}, err
	}
	a.emit(events.ChatsChanged, map[string]any{})
	return a.store.GetChat(c.ID)
}

// RenameChat sets a chat title.
func (a *App) RenameChat(chatID, title string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	if err := a.store.SetChatTitle(chatID, title); err != nil {
		return err
	}
	a.emit(events.ChatsChanged, map[string]any{})
	return nil
}

// DeleteChat removes a chat and its data.
func (a *App) DeleteChat(chatID string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	a.engine.Stop(chatID)
	if err := a.store.DeleteChat(chatID); err != nil {
		return err
	}
	a.engine.InvalidateMeasurement(chatID)
	a.emit(events.ChatsChanged, map[string]any{})
	return nil
}

// ChatState bundles everything the UI needs for one chat in a single call,
// which keeps the message list, budget, compactions and attachments consistent.
type ChatState struct {
	Chat        store.Chat         `json:"chat"`
	Messages    []store.Message    `json:"messages"`
	Compactions []store.Compaction `json:"compactions"`
	Attachments []store.Attachment `json:"attachments"`
	Documents   []store.Document   `json:"documents"`
	Budget      chat.ContextBudget `json:"budget"`
	Running     bool               `json:"running"`
}

// GetChatState returns the full state of a chat.
func (a *App) GetChatState(chatID string) (ChatState, error) {
	if err := a.ensure(); err != nil {
		return ChatState{}, err
	}
	c, err := a.store.GetChat(chatID)
	if err != nil {
		return ChatState{}, err
	}
	messages, err := a.store.ListMessages(chatID)
	if err != nil {
		return ChatState{}, err
	}
	compactions, err := a.store.ListCompactions(chatID)
	if err != nil {
		return ChatState{}, err
	}
	attachments, err := a.store.ListAttachments(chatID)
	if err != nil {
		return ChatState{}, err
	}
	documents, err := a.store.ListDocuments(chatID)
	if err != nil {
		return ChatState{}, err
	}
	budget, err := a.GetBudget(chatID, "", "")
	if err != nil {
		budget = chat.ContextBudget{}
	}
	return ChatState{
		Chat:        c,
		Messages:    messages,
		Compactions: compactions,
		Attachments: attachments,
		Documents:   documents,
		Budget:      budget,
		Running:     a.engine.IsRunning(chatID),
	}, nil
}

// GetBudget computes the context budget for a chat, optionally for a different
// model than the one currently selected. That is what lets the model switcher
// warn about a smaller window before the user commits to the switch.
func (a *App) GetBudget(chatID, backend, model string) (chat.ContextBudget, error) {
	if err := a.ensure(); err != nil {
		return chat.ContextBudget{}, err
	}
	return a.engine.PreviewBudget(context.Background(), chatID, backend, model)
}

// newID returns a fresh identifier.
func newID() string { return uuid.NewString() }
