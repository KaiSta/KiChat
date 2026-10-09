package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	rt "kichat/internal/runtime"
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

	// installer fetches llama-server; servers holds one supervisor per backend
	// entry that KiChat runs itself. The two are separate because the binaries
	// are shared by every managed entry.
	installer *rt.Installer

	serversMu sync.Mutex
	servers   map[string]*rt.Server

	// install tracks a download in progress. It is read by the UI through a
	// bound method rather than pushed, because a download outlives the call that
	// asked for it and polling cannot miss the end of one.
	installMu    sync.Mutex
	installBusy  bool
	installPhase string
	installDone  int64
	installTotal int64
	installErr   string
}

// NewApp creates the application.
func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if err := a.ensure(); err != nil {
		runtime.LogErrorf(ctx, "Initialisierung fehlgeschlagen: %v", err)
	}
	a.startEnabledModels()
}

func (a *App) shutdown(ctx context.Context) {
	if a.engine != nil {
		a.engine.StopAll()
	}
	// Stopping the managed servers is not optional: they are child processes, and
	// a llama-server left behind holds its model in memory with nothing left in
	// the UI to stop it.
	a.stopAllManaged()
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

	secrets, err := secret.New(filepath.Join(a.dataDir, "secrets"))
	if err != nil {
		return err
	}
	a.secrets = secrets

	settings, err := config.Load(st, secrets)
	if err != nil {
		// A corrupt settings blob should not stop the app from starting.
		settings = config.Default()
	}

	a.settingsMu.Lock()
	a.settings = settings
	a.settingsMu.Unlock()

	a.extractor = ingest.New()
	a.installer = rt.NewInstaller(filepath.Join(a.dataDir, "runtime"))
	a.installer.OnProgress = a.recordInstallProgress
	a.servers = map[string]*rt.Server{}
	a.rebuildServices(settings)
	return nil
}

// startEnabledModels starts every managed model the user left switched on. It is
// what makes "the app runs the model" true across restarts: nothing else has to
// be started by hand.
func (a *App) startEnabledModels() {
	for id, mm := range a.Settings().Runtime.Models {
		if !mm.Enabled || mm.ModelPath == "" {
			continue
		}
		if err := a.startManagedModel(id); err != nil {
			// Not fatal to the app: the failure is on the entry's status, where
			// the user can read what to fix.
			runtime.LogErrorf(a.ctx, "Modell %s starten: %v", id, err)
		}
	}
}

// startManagedModel starts the server of one managed backend.
func (a *App) startManagedModel(backendID string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	settings := a.Settings()
	mm, ok := settings.Runtime.Models[backendID]
	if !ok {
		return fmt.Errorf("für %s ist kein eigenes Modell eingerichtet", backendID)
	}
	binary, err := a.llamaServerPath(settings)
	if err != nil {
		return err
	}

	srv := a.serverFor(backendID, settings)
	return srv.Start(rt.Options{
		Binary: binary,
		Model:  mm.ModelPath,
		Ctx:    mm.CtxSize,
		Extra:  rt.SplitArgs(mm.ExtraArgs),
	})
}

// llamaServerPath resolves the launcher: the user's own copy if they gave one,
// otherwise the build the installer fetched.
func (a *App) llamaServerPath(settings config.Settings) (string, error) {
	if p := strings.TrimSpace(settings.Runtime.BinaryPath); p != "" {
		return p, nil
	}
	tag, binary := a.installer.Find()
	if tag == "" || binary == "" {
		return "", errors.New("llama-server ist noch nicht installiert")
	}
	return binary, nil
}

// serverFor returns the supervisor of one backend, creating it if needed.
func (a *App) serverFor(backendID string, settings config.Settings) *rt.Server {
	a.serversMu.Lock()
	defer a.serversMu.Unlock()
	if a.servers == nil {
		a.servers = map[string]*rt.Server{}
	}
	srv, ok := a.servers[backendID]
	if !ok {
		srv = rt.NewServer(backendID)
		// The id is captured rather than read back from the status: the handler
		// outlives the settings snapshot it was created from.
		id := backendID
		srv.SetStateHandler(func(st rt.Status) { a.onManagedState(id, st) })
		a.servers[backendID] = srv
	}
	srv.SetReadyTimeout(time.Duration(settings.Runtime.ReadySeconds) * time.Second)
	return srv
}

// onManagedState follows a managed server's address into its backend entry.
//
// A managed server listens on a port chosen when it starts, so the backend the
// user set up cannot hold its address in advance: it has to be written when the
// server is up, and persisted, so the row in Settings and the chat's registry
// both name the server that is actually running. Nothing happens on the way down:
// the address stays as the last one, and the health check reports what it finds.
func (a *App) onManagedState(backendID string, st rt.Status) {
	if st.State != rt.StateRunning || st.URL == "" {
		return
	}
	a.settingsMu.Lock()
	changed := false
	for i := range a.settings.Backends {
		if a.settings.Backends[i].ID != backendID {
			continue
		}
		if a.settings.Backends[i].BaseURL != st.URL || !a.settings.Backends[i].Enabled {
			a.settings.Backends[i].BaseURL = st.URL
			a.settings.Backends[i].Enabled = true
			changed = true
		}
	}
	settings := a.settings
	a.settingsMu.Unlock()
	if !changed {
		return
	}
	if err := a.sanitize(settings).Save(a.store); err != nil {
		runtime.LogErrorf(a.ctx, "Adresse des laufenden Servers speichern: %v", err)
	}
	a.rebuildServices(settings)
}

// adoptManagedAddresses restores the address of every backend whose model the app
// is running itself.
//
// A managed server picks its port when it starts, and it is the app that writes
// the address it settled on into the backend entry. A settings view opened before
// that start still holds the previous one, so its Save would put a dead address
// back and the running model would look unreachable until it was started again.
// While the app owns the server it owns the address, so the field is restored here
// rather than trusted from the caller. Nothing happens for a backend the app is not
// running, which is where a hand-started server's address belongs to the user.
func (a *App) adoptManagedAddresses(s config.Settings) config.Settings {
	a.serversMu.Lock()
	defer a.serversMu.Unlock()
	for i := range s.Backends {
		srv := a.servers[s.Backends[i].ID]
		if srv == nil {
			continue
		}
		st := srv.Status()
		if st.State != rt.StateRunning || st.URL == "" {
			continue
		}
		s.Backends[i].BaseURL = st.URL
		s.Backends[i].Enabled = true
	}
	return s
}

// stopAllManaged stops every server the app started.
func (a *App) stopAllManaged() {
	a.serversMu.Lock()
	servers := make([]*rt.Server, 0, len(a.servers))
	for _, srv := range a.servers {
		servers = append(servers, srv)
	}
	a.serversMu.Unlock()
	for _, srv := range servers {
		if err := srv.Stop(); err != nil {
			runtime.LogErrorf(a.ctx, "llama-server beenden: %v", err)
		}
	}
}

// managedStatus is one managed entry as the UI asks for it.
type managedStatus struct {
	BackendID string    `json:"backendId"`
	Running   bool      `json:"running"`
	Status    rt.Status `json:"status"`
	Model     string    `json:"model"`
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
	// Servers survive a settings save: rebuilding services must not restart a
	// model that is already loaded, it only updates the settings each one runs
	// with. So this creates the supervisors that are missing and touches nothing
	// else.
	for id, mm := range settings.Runtime.Models {
		if mm.Enabled || mm.ModelPath != "" {
			a.serverFor(id, settings)
		}
	}
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
