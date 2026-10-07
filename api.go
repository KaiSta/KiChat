package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"kichat/internal/chat"
	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/ingest"
	"kichat/internal/llm"
	"kichat/internal/research"
	"kichat/internal/secret"
	"kichat/internal/store"
)

// --- settings --------------------------------------------------------------

// GetSettings returns the current settings with every secret blanked: the Brave
// key and each backend's API key are mirrored only as shape/booleans and never
// cross the bridge.
func (a *App) GetSettings() (config.Settings, error) {
	if err := a.ensure(); err != nil {
		return config.Settings{}, err
	}
	return a.sanitize(a.Settings()), nil
}

// SaveSettings persists settings and rebuilds the services that depend on them.
//
// It returns what was stored instead of nothing because the built-in system
// prompts follow the interface language, and only the backend knows which of the
// prompts in the payload are still untouched: handing the caller its own draft
// back would leave the prompt textarea showing the previous language until the
// next start.
//
// Backend API keys are secrets. Each backend's key is stored under its own name
// in the secret store, the same way the Brave key is, and neither the blob nor
// the bridge ever carries the secret. SaveSettings commits the draft's keys to
// the secret store and reads the real stored values back into the snapshot the
// registry is built from, so a key typed in Settings reaches the provider. The
// copy it persists is sanitised; the keys stay only in memory and in the store.
func (a *App) SaveSettings(s config.Settings) (config.Settings, error) {
	if err := a.ensure(); err != nil {
		return config.Settings{}, err
	}
	// The draft only ever carries a boolean about the Brave key (GetSettings
	// blanks the secret), so an empty BraveAPIKey means "leave the stored key
	// alone".
	s.Search.BraveAPIKey = a.Settings().Search.BraveAPIKey
	// Moving the untouched prompts to the new language, and repairing an empty
	// default prompt, happen here as well as on load: clearing the textarea and
	// switching the language in the same save must not leave either undone.
	s.LocalisePrompts()
	// Commit each backend key through the secret store, then reconcile the
	// snapshot from what is actually stored so the registry sees the real key on
	// the next rebuild rather than the draft's blank.
	s, err := a.reconcileBackendKeys(s)
	if err != nil {
		return s, err
	}
	// Persist the sanitised copy: the secrets live in the secret store, never in
	// the settings blob. The in-memory snapshot keeps the real keys so the
	// rebuild below can authenticate.
	if err := a.sanitize(s).Save(a.store); err != nil {
		return s, err
	}
	a.settingsMu.Lock()
	a.settings = s
	a.settingsMu.Unlock()
	a.rebuildServices(s)
	return a.sanitize(s), nil
}

// reconcileBackendKeys commits each backend's draft key to the secret store and
// reads the real stored value back into the snapshot. A non-empty draft key
// overwrites what is stored, the same way SetBackendAPIKey does; an empty one
// leaves the stored key alone — it never clears a key the user set earlier, and
// it never rewrites over one with a blank.
func (a *App) reconcileBackendKeys(s config.Settings) (config.Settings, error) {
	for i := range s.Backends {
		b := &s.Backends[i]
		name := llm.BackendAPIKeyName(b.ID)
		if want := strings.TrimSpace(b.APIKey); want != "" {
			if err := a.secrets.Set(name, want); err != nil {
				return s, err
			}
			b.APIKey = want
			continue
		}
		// Empty draft: keep whatever is on disk, which may be nothing.
		if stored, ok := a.secrets.Get(name); ok {
			b.APIKey = stored
		} else {
			b.APIKey = ""
		}
	}
	return s, nil
}

// sanitize returns the copy that the bridge is allowed to hand back and that the
// settings blob is allowed to store. Secrets never cross it. The backend slice is
// copied before it is blanked, so the caller's snapshot — the one the registry is
// built from — keeps its real keys.
func (a *App) sanitize(s config.Settings) config.Settings {
	backends := make([]llm.BackendConfig, len(s.Backends))
	copy(backends, s.Backends)
	for i := range backends {
		backends[i].APIKey = ""
	}
	s.Backends = backends
	s.Search.BraveAPIKey = ""
	return s
}

// SecretStatus reports how secrets are stored.
type SecretStatus struct {
	Backend     string `json:"backend"`
	Keyring     bool   `json:"keyring"`
	BraveKeySet bool   `json:"braveKeySet"`
	Warning     string `json:"warning,omitempty"`
}

// GetSecretStatus reports where the Brave key is kept.
func (a *App) GetSecretStatus() (SecretStatus, error) {
	if err := a.ensure(); err != nil {
		return SecretStatus{}, err
	}
	status := SecretStatus{
		Backend: a.secrets.Backend(),
		Keyring: a.secrets.KeyringAvailable(),
	}
	_, status.BraveKeySet = a.secrets.Get("brave")
	if !status.Keyring {
		status.Warning = "Kein Schlüsselbund verfügbar (libsecret fehlt). " +
			"Der Key wird lokal verschlüsselt gespeichert — das schützt vor versehentlicher " +
			"Offenlegung, aber nicht gegen einen Angreifer mit Zugriff auf dieses Benutzerkonto."
	}
	return status, nil
}

// setBackendAPIKey stores one backend's key in the secret store and rebuilds the
// registry so the change takes effect at once. The secret store owns the key, so
// it never reaches the settings blob or the bridge.
func (a *App) setBackendAPIKey(backendID, key string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	if err := a.secrets.Set(llm.BackendAPIKeyName(backendID), key); err != nil {
		return err
	}
	a.settingsMu.Lock()
	for i := range a.settings.Backends {
		if a.settings.Backends[i].ID == backendID {
			a.settings.Backends[i].APIKey = key
		}
	}
	a.settingsMu.Unlock()
	a.rebuildServices(a.Settings())
	return nil
}

// SetBackendAPIKey stores an API key for one backend. Empty clears it.
func (a *App) SetBackendAPIKey(backendID, apiKey string) error {
	return a.setBackendAPIKey(backendID, apiKey)
}

// SetBraveKey stores the Brave Search API key. It is kept separate from
// setBackendAPIKey because Brave is a search provider, not a chat backend: its key
// lives under the plain "brave" name the rest of the app reads, not under a
// backend-qualified one.
func (a *App) SetBraveKey(key string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	if err := a.secrets.Set("brave", key); err != nil {
		return err
	}
	a.settingsMu.Lock()
	a.settings.Search.BraveAPIKey = key
	a.settingsMu.Unlock()
	a.rebuildServices(a.Settings())
	return nil
}

// TestBraveKey validates a key with a single live query.
func (a *App) TestBraveKey(key string) (research.SearchOutcome, error) {
	if err := a.ensure(); err != nil {
		return research.SearchOutcome{}, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		if stored, ok := a.secrets.Get("brave"); ok {
			key = stored
		}
	}
	if key == "" {
		return research.SearchOutcome{}, errors.New("kein Brave API-Key angegeben")
	}
	provider := research.NewBraveProvider(key, "")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	results, err := provider.Search(ctx, research.SearchRequest{
		Query: "test", Count: 1, Country: "DE", SearchLang: "de",
	})
	if err != nil {
		return research.SearchOutcome{}, err
	}
	return research.SearchOutcome{Results: results, Provider: "brave"}, nil
}

// --- backends and models ---------------------------------------------------

// GetHealth probes every configured backend.
func (a *App) GetHealth() ([]llm.HealthStatus, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	return a.registry.Health(ctx), nil
}

// ListModels returns the merged cross-backend model catalogue.
func (a *App) ListModels(forceRefresh bool) ([]llm.ModelInfo, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if forceRefresh {
		a.registry.Invalidate()
	}
	return a.visibleModels(ctx), nil
}

// SetModelOverride stores a per-model context window or capability correction.
func (a *App) SetModelOverride(backend, model string, ov llm.ModelOverride) error {
	if err := a.ensure(); err != nil {
		return err
	}
	a.settingsMu.Lock()
	if a.settings.ModelOverrides == nil {
		a.settings.ModelOverrides = map[string]llm.ModelOverride{}
	}
	key := backend + "/" + model
	if ov.ContextLimit == 0 && len(ov.Capabilities) == 0 {
		delete(a.settings.ModelOverrides, key)
	} else {
		a.settings.ModelOverrides[key] = ov
	}
	settings := a.settings
	a.settingsMu.Unlock()

	if err := a.store.PutModelOverride(backend, model, ov); err != nil {
		return err
	}
	a.registry.Invalidate()
	return settings.Save(a.store)
}

// --- prompt presets --------------------------------------------------------

// PromptVariable describes a template variable available in system prompts.
//
// Key is the i18n key the settings view translates and Description is the
// untranslated fallback, the same pair a built-in preset name uses: the list sits
// directly under the prompt editor, so leaving it in one language made the screen
// read as English above and German below.
type PromptVariable struct {
	Name        string `json:"name"`
	Key         string `json:"key"`
	Description string `json:"description"`
}

// GetPromptVariables lists the supported template variables.
func (a *App) GetPromptVariables() []PromptVariable {
	return []PromptVariable{
		{Name: "datetime", Key: "promptvar.datetime", Description: "Aktuelles Datum, Uhrzeit und Wochentag"},
		{Name: "date", Key: "promptvar.date", Description: "Nur das Datum (TT.MM.JJJJ)"},
		{Name: "time", Key: "promptvar.time", Description: "Nur die Uhrzeit (HH:MM)"},
		{Name: "weekday", Key: "promptvar.weekday", Description: "Wochentag ausgeschrieben"},
		{Name: "location", Key: "promptvar.location", Description: "Der in den Einstellungen hinterlegte Standort"},
		{Name: "model", Key: "promptvar.model", Description: "Das aktuell gewählte Modell"},
		{Name: "backend", Key: "promptvar.backend", Description: "Das aktuelle Backend (LM Studio, Ollama, …)"},
		{Name: "context_limit", Key: "promptvar.context_limit", Description: "Größe des Kontextfensters in Tokens"},
		{Name: "context_used", Key: "promptvar.context_used", Description: "Zuletzt gemessene Kontextbelegung"},
		{Name: "tool_list", Key: "promptvar.tool_list", Description: "Liste der verfügbaren Tools"},
	}
}

// PreviewSystemPrompt renders a prompt template with live values.
func (a *App) PreviewSystemPrompt(tmpl string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	settings := a.Settings()
	models := a.visibleModels(context.Background())
	model := ""
	if len(models) > 0 {
		model = models[0].ID
	}
	return chat.PreviewPrompt(tmpl, settings.Location, model), nil
}

// UnresolvedPromptVars reports template variables that do not exist.
func (a *App) UnresolvedPromptVars(tmpl string) []string {
	return chat.UnresolvedVars(tmpl)
}

// --- compaction ------------------------------------------------------------

// CompactRequest is the manual compaction payload.
type CompactRequest struct {
	ChatID  string  `json:"chatId"`
	Focus   string  `json:"focus"`
	Target  float64 `json:"target"`
	FromSeq int     `json:"fromSeq"`
	ToSeq   int     `json:"toSeq"`
}

// CompactNow runs a manual compaction. It returns what the run did rather than
// only an error, because "there was nothing to fold" is a normal answer to a
// manual request and the UI has to be able to say so.
func (a *App) CompactNow(req CompactRequest) (chat.CompactResult, error) {
	if err := a.ensure(); err != nil {
		return chat.CompactResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, err := a.engine.Compact(ctx, req.ChatID, chat.CompactOptions{
		Force:   true,
		Focus:   req.Focus,
		Target:  req.Target,
		FromSeq: req.FromSeq,
		ToSeq:   req.ToSeq,
	})
	if err != nil {
		return chat.CompactResult{}, err
	}
	if result.Created {
		a.emit(events.ChatsChanged, map[string]any{})
	}
	return result, nil
}

// ListCompactions returns a chat's compaction history.
func (a *App) ListCompactions(chatID string) ([]store.Compaction, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	return a.store.ListCompactions(chatID)
}

// DeleteCompaction removes a compaction, restoring the folded messages to the
// live context. The original messages were never deleted, so this is complete.
//
// Any compaction that was summarised on top of this one is dropped with it; see
// DeleteCompactionsFrom for why that is not optional. The measurement goes too,
// because the prompt has changed shape and the old count no longer describes it.
func (a *App) DeleteCompaction(chatID, compactionID string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	compactions, err := a.store.ListCompactions(chatID)
	if err != nil {
		return err
	}
	target, found := 0, false
	for _, c := range compactions {
		if c.ID == compactionID {
			target, found = c.ToSeq, true
			break
		}
	}
	if !found {
		return fmt.Errorf("Kompaktierung %q existiert nicht mehr", compactionID)
	}
	if err := a.store.DeleteCompactionsFrom(chatID, target); err != nil {
		return err
	}
	a.engine.InvalidateMeasurement(chatID)
	return nil
}

// --- attachments and documents --------------------------------------------

// AttachmentInput describes a file to add.
type AttachmentInput struct {
	ChatID    string `json:"chatId"`
	Path      string `json:"path"`
	Placement string `json:"placement"`
}

// AutoIndexThreshold is the token count above which an attachment is indexed
// rather than inlined.
const AutoIndexThreshold = 8000

// AddAttachment reads a file, extracts its text, and attaches it to a chat.
func (a *App) AddAttachment(in AttachmentInput) (store.Attachment, error) {
	if err := a.ensure(); err != nil {
		return store.Attachment{}, err
	}
	if strings.TrimSpace(in.Path) == "" {
		return store.Attachment{}, errors.New("kein Dateipfad angegeben")
	}
	info, err := os.Stat(in.Path)
	if err != nil {
		return store.Attachment{}, fmt.Errorf("Datei nicht lesbar: %w", err)
	}
	if info.IsDir() {
		return store.Attachment{}, errors.New("Ordner können nicht angehängt werden")
	}
	const maxSize = 64 << 20
	if info.Size() > maxSize {
		return store.Attachment{}, fmt.Errorf("Datei ist zu groß (%d MB, Maximum %d MB)",
			info.Size()>>20, int64(maxSize)>>20)
	}
	data, err := os.ReadFile(in.Path)
	if err != nil {
		return store.Attachment{}, fmt.Errorf("Datei nicht lesbar: %w", err)
	}

	result, err := a.extractor.Ingest(filepath.Base(in.Path), "", data)
	if err != nil {
		return store.Attachment{}, err
	}

	placement := in.Placement
	if placement == "" {
		if result.Tokens > AutoIndexThreshold {
			placement = store.PlacementIndexed
		} else {
			placement = store.PlacementInline
		}
	}

	attachment := store.Attachment{
		ID:            newID(),
		ChatID:        in.ChatID,
		Kind:          string(result.Kind),
		Filename:      filepath.Base(in.Path),
		Mime:          result.Mime,
		SizeBytes:     info.Size(),
		Pages:         result.Pages,
		TokenEstimate: result.Tokens,
		Placement:     placement,
		CreatedAt:     time.Now(),
	}
	if len(result.Warnings) > 0 {
		attachment.Warning = strings.Join(result.Warnings, " ")
	}

	// Images carry their payload rather than text, so a vision model can receive
	// them; everything else stores the extracted text.
	documentText := result.Text
	if result.Kind == ingest.KindImage {
		documentText = base64.StdEncoding.EncodeToString(data)
		attachment.TokenEstimate = 1024
	}

	document := store.Document{
		ID:           newID(),
		AttachmentID: attachment.ID,
		ChatID:       in.ChatID,
		Title:        filepath.Base(in.Path),
		Text:         documentText,
		TokenCount:   attachment.TokenEstimate,
		PageCount:    result.Pages,
		CreatedAt:    time.Now(),
	}
	if err := a.store.CreateDocument(document); err != nil {
		return store.Attachment{}, err
	}
	attachment.DocumentID = document.ID

	if placement == store.PlacementIndexed && result.Kind != ingest.KindImage {
		chunks := ingest.ChunkDocument(result.Text, ingest.DefaultChunkSpec())
		out := make([]store.Chunk, 0, len(chunks))
		for _, c := range chunks {
			out = append(out, store.Chunk{
				DocumentID: document.ID,
				ChatID:     in.ChatID,
				Ordinal:    c.Ordinal,
				Heading:    c.Heading,
				Page:       c.Page,
				Text:       c.Text,
				Tokens:     c.Tokens,
			})
		}
		if err := a.store.InsertChunks(out); err != nil {
			attachment.Warning = strings.TrimSpace(attachment.Warning +
				" Indizierung fehlgeschlagen: " + err.Error())
		}
	}

	if err := a.store.CreateAttachment(attachment); err != nil {
		return store.Attachment{}, err
	}
	a.emit(events.Attachments, map[string]any{"chatId": in.ChatID})
	return attachment, nil
}

// RemoveAttachment deletes an attachment and its extracted text.
func (a *App) RemoveAttachment(attachmentID string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	attachment, err := a.store.GetAttachment(attachmentID)
	if err != nil {
		return err
	}
	if err := a.store.DeleteAttachment(attachmentID); err != nil {
		return err
	}
	a.emit(events.Attachments, map[string]any{"chatId": attachment.ChatID})
	return nil
}

// SetAttachmentPlacement switches an attachment between inline and indexed.
func (a *App) SetAttachmentPlacement(attachmentID, placement string) (store.Attachment, error) {
	if err := a.ensure(); err != nil {
		return store.Attachment{}, err
	}
	attachment, err := a.store.GetAttachment(attachmentID)
	if err != nil {
		return store.Attachment{}, err
	}
	attachment.Placement = placement
	if placement == store.PlacementIndexed {
		document, err := a.store.GetDocument(attachment.DocumentID)
		if err == nil {
			existing, _ := a.store.ListChunks(document.ID)
			if len(existing) == 0 {
				chunks := ingest.ChunkDocument(document.Text, ingest.DefaultChunkSpec())
				out := make([]store.Chunk, 0, len(chunks))
				for _, c := range chunks {
					out = append(out, store.Chunk{
						DocumentID: document.ID, ChatID: attachment.ChatID,
						Ordinal: c.Ordinal, Heading: c.Heading, Page: c.Page,
						Text: c.Text, Tokens: c.Tokens,
					})
				}
				if err := a.store.InsertChunks(out); err != nil {
					return store.Attachment{}, err
				}
			}
		}
	}
	if err := a.store.UpdateAttachment(attachment); err != nil {
		return store.Attachment{}, err
	}
	a.emit(events.Attachments, map[string]any{"chatId": attachment.ChatID})
	return attachment, nil
}

// PickFiles opens a native file dialog and returns the chosen paths.
func (a *App) PickFiles() ([]string, error) {
	if a.ctx == nil {
		return nil, errors.New("App ist noch nicht bereit")
	}
	return runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Dateien zum Chat hinzufügen",
	})
}

// GetDocument returns a document including its full text.
func (a *App) GetDocument(documentID string) (store.Document, error) {
	if err := a.ensure(); err != nil {
		return store.Document{}, err
	}
	return a.store.GetDocument(documentID)
}

// ListChunks returns the indexed chunks of a document.
func (a *App) ListChunks(documentID string) ([]store.Chunk, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	return a.store.ListChunks(documentID)
}

// --- search and scraping ---------------------------------------------------

// Search runs a live web search, bypassing nothing but the provider chain.
func (a *App) Search(query string) (research.SearchOutcome, error) {
	if err := a.ensure(); err != nil {
		return research.SearchOutcome{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	outcome, err := a.chain.Search(ctx, query, research.SearchRequest{})
	if err != nil {
		return research.SearchOutcome{}, err
	}
	return outcome, nil
}

// Scrape fetches a URL and returns clean Markdown.
func (a *App) Scrape(url string, maxTokens int) (research.ScrapeResult, error) {
	if err := a.ensure(); err != nil {
		return research.ScrapeResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if maxTokens <= 0 {
		maxTokens = 12000
	}
	return a.scraper.Scrape(ctx, url, maxTokens)
}

// ScrapeIntoChat scrapes a page and attaches it to a chat as an indexed or
// inline document, which is how "read this page" becomes permanent context.
func (a *App) ScrapeIntoChat(chatID, pageURL string, maxTokens int) (store.Attachment, error) {
	if err := a.ensure(); err != nil {
		return store.Attachment{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if maxTokens <= 0 {
		maxTokens = 12000
	}
	result, err := a.scraper.Scrape(ctx, pageURL, maxTokens)
	if err != nil {
		return store.Attachment{}, err
	}

	title := result.Title
	if title == "" {
		title = result.FinalURL
	}
	attachment := store.Attachment{
		ID:            newID(),
		ChatID:        chatID,
		Kind:          "web",
		Filename:      title,
		Mime:          "text/markdown",
		SizeBytes:     int64(len(result.Markdown)),
		TokenEstimate: result.Tokens,
		Placement:     store.PlacementInline,
		CreatedAt:     time.Now(),
	}
	if result.Warning != "" {
		attachment.Warning = result.Warning
	}
	document := store.Document{
		ID:           newID(),
		AttachmentID: attachment.ID,
		ChatID:       chatID,
		Title:        title,
		Text:         result.Markdown,
		TokenCount:   result.Tokens,
		SourceURL:    result.FinalURL,
		CreatedAt:    time.Now(),
	}
	if err := a.store.CreateDocument(document); err != nil {
		return store.Attachment{}, err
	}
	attachment.DocumentID = document.ID
	if err := a.store.CreateAttachment(attachment); err != nil {
		return store.Attachment{}, err
	}
	a.emit(events.Attachments, map[string]any{"chatId": chatID})
	return attachment, nil
}

// SearchStatus reports provider availability and quota usage.
type SearchStatus struct {
	Providers     map[string]bool `json:"providers"`
	Order         []string        `json:"order"`
	BraveQueries  int64           `json:"braveQueriesThisMonth"`
	BraveQuotaEst int64           `json:"braveQuotaEstimate"`
	CacheMinutes  int             `json:"cacheMinutes"`
	SecretBackend string          `json:"secretBackend"`
}

// GetSearchStatus reports the search configuration state.
func (a *App) GetSearchStatus() (SearchStatus, error) {
	if err := a.ensure(); err != nil {
		return SearchStatus{}, err
	}
	settings := a.Settings()
	return SearchStatus{
		Providers:    a.chain.ProviderAvailability(),
		Order:        settings.Search.ProviderOrder,
		BraveQueries: a.chain.BraveQueriesThisMonth(),
		// Brave's metered plan gives $5 of free credit per month at
		// $5 per 1,000 requests, so about a thousand queries.
		BraveQuotaEst: 1000,
		CacheMinutes:  settings.Search.CacheMinutes,
		SecretBackend: a.secrets.Backend(),
	}, nil
}

// OpenExternal opens a URL in the user's browser.
func (a *App) OpenExternal(target string) {
	if a.ctx == nil {
		return
	}
	runtime.BrowserOpenURL(a.ctx, target)
}

// RedactSecrets strips stored secrets from arbitrary text.
func (a *App) RedactSecrets(text string) string {
	if a.secrets == nil {
		return text
	}
	var values []string
	if v, ok := a.secrets.Get("brave"); ok {
		values = append(values, v)
	}
	// Backend API keys are secrets too, so they are redacted alongside the Brave
	// key rather than being left to leak into a log or a toast.
	for _, b := range a.Settings().Backends {
		if v, ok := a.secrets.Get(llm.BackendAPIKeyName(b.ID)); ok {
			values = append(values, v)
		}
	}
	return secret.Redact(text, values...)
}
