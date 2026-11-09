package main

import (
	"context"
	"net/url"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"kichat/internal/config"
	"kichat/internal/llm"
	"kichat/internal/research"
	"kichat/internal/secret"
	"kichat/internal/uimsg"
	"kichat/internal/version"
)

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
	// The record the draft is merged against. The draft only ever carries a boolean
	// about the Brave key (GetSettings blanks the secret), and it never carries the
	// installed build at all, so both are read from here.
	recorded := a.Settings()
	// An empty BraveAPIKey means "leave the stored key alone".
	s.Search.BraveAPIKey = recorded.Search.BraveAPIKey
	// The image server's credential follows the same rule as a backend key: a
	// non-empty draft overwrites what is stored, and an empty one leaves it alone.
	// "Empty" therefore cannot clear it, which is the right trade — a save that
	// does not touch the field must not silently undo a configured credential.
	if want := strings.TrimSpace(s.Images.APIKey); want != "" {
		if err := a.secrets.Set(config.ImageAPIKeyName, want); err != nil {
			return s, err
		}
		s.Images.APIKey = want
	} else if stored, ok := a.secrets.Get(config.ImageAPIKeyName); ok {
		s.Images.APIKey = stored
	} else {
		s.Images.APIKey = ""
	}
	// The installed build is the installer's record, and no control in the panel edits
	// the tag. A download writes it from its own goroutine, after the bound call that
	// started it returned, so a draft the view read before that still names the build
	// the install replaced. Two tags that differ therefore say the draft predates the
	// install — which also means its source and family are the ones the install
	// superseded and not a choice the user just made — so the recorded runtime wins
	// whole. Without this, saving an unrelated setting put the replaced build back (the
	// next start ran the old llama.cpp while the panel showed the new one) or stranded
	// a fork install under a reverted upstream source. A draft whose tag already matches
	// is current, and its source and family are the pickers' own.
	if strings.TrimSpace(s.Runtime.Tag) != strings.TrimSpace(recorded.Runtime.Tag) {
		s.Runtime.Distribution = recorded.Runtime.Distribution
		s.Runtime.Accelerator = recorded.Runtime.Accelerator
	}
	s.Runtime.Tag = recorded.Runtime.Tag
	// The mlx-lm version is the installer's record in the same way the build tag is:
	// no control edits it, and a draft read before the pip run would otherwise put
	// the previous version — or none at all — back over the one that just landed.
	s.Runtime.MLX.Version = recorded.Runtime.MLX.Version
	// The folders the model row remembers are repaired here as well as on the next
	// load, because this save is what the row reads back: a draft that arrived with a
	// duplicate or an over-long list would otherwise be shown exactly as it came in,
	// and the folder this very save is switching to has to be in it — that is the whole
	// of what makes returning to the previous library one click.
	s.Runtime.ModelDirs = config.NormaliseModelDirs(s.Runtime.ModelDirs, s.Runtime.ModelDir)
	// The availability blacklist is written by the model check, not by any control
	// in the panel, so no draft ever carries it. A draft the view read before the
	// last sweep would otherwise put every hidden model back into the picker on the
	// next unrelated save.
	s.HiddenModels = recorded.HiddenModels
	// The time of the last release check is the app's record in the same way: a draft
	// read before one would otherwise put the older time back and make the next start
	// ask GitHub again. The switch beside it is the draft's, because that is a control.
	s.Updates.LastCheckUnix = recorded.Updates.LastCheckUnix
	// The per-model overrides are written by SetModelOverride, which is also outside
	// this form, so the same rule applies: a draft read before the last override save
	// would put the older map back — silently losing a per-request ceiling the user
	// typed a moment ago — on the next unrelated save.
	s.ModelOverrides = recorded.Clone().ModelOverrides
	// Moving the untouched prompts to the new language, and repairing an empty
	// default prompt, happen here as well as on load: clearing the textarea and
	// switching the language in the same save must not leave either undone.
	s.LocalisePrompts()
	// A save is also where "look again" happens: the launcher may have been
	// replaced since its version was read.
	a.forgetVersions()
	// A Save must not hand a running managed model back an address it no longer
	// listens on; see adoptManagedAddresses.
	s = a.adoptManagedAddresses(s)
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
	// Saving is also what applies this switch, in both directions: without it, turning
	// the daily check on would wait for the next start to show its badge, and turning it
	// off would leave a look already under way in place. A check that is off, or not due,
	// returns immediately.
	a.startUpdateCheck()
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
	// The image server's "user:pass" is a credential like the others, so it never
	// reaches the settings blob or the bridge.
	s.Images.APIKey = ""
	// A profile carries a whole ImageSettings, so the same rule has to reach each
	// one. The slice is copied before it is blanked, for the reason the backend
	// slice above is: sanitize hands back a value the caller keeps as its snapshot,
	// and blanking the elements in place would take the credential out of that copy
	// too. The panel never fills a profile's key — the credential belongs to the
	// feature — so this is the second line of defence behind normalise().
	profiles := make([]config.ImageProfile, len(s.ImageProfiles))
	copy(profiles, s.ImageProfiles)
	for i := range profiles {
		profiles[i].Settings.APIKey = ""
	}
	s.ImageProfiles = profiles
	return s
}

// SecretStatus reports how secrets are stored.
type SecretStatus struct {
	Backend     string `json:"backend"`
	Keyring     bool   `json:"keyring"`
	BraveKeySet bool   `json:"braveKeySet"`
	// BackendKeys names the backends that have a key stored, one boolean per
	// configured entry and never a value. The settings blob is sanitized, so this
	// is the only way the panel can tell a configured entry from an untouched one.
	// It needs that to explain the state this was added for: a key pasted into a
	// shipped entry that is switched off is stored by the save, but no provider is
	// built for it and no probe is ever sent, and the panel could otherwise only
	// show a blank key field on a backend that is already configured.
	BackendKeys map[string]bool `json:"backendKeys,omitempty"`
	// ImageKeySet reports whether the image server has a credential stored. The
	// panel needs it for the same reason it needs BackendKeys: the blob is
	// sanitised, so a blank field cannot be told apart from a configured one.
	ImageKeySet bool   `json:"imageKeySet"`
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
	_, status.ImageKeySet = a.secrets.Get(config.ImageAPIKeyName)
	// Every configured entry, switched on or not: an entry that is off is exactly
	// the one whose key has to stay visible.
	status.BackendKeys = map[string]bool{}
	for _, b := range a.Settings().Backends {
		_, status.BackendKeys[b.ID] = a.secrets.Get(llm.BackendAPIKeyName(b.ID))
	}
	if !status.Keyring {
		status.Warning = "Kein Schlüsselbund verfügbar (libsecret fehlt). " +
			"Der Key wird lokal verschlüsselt gespeichert — das schützt vor versehentlicher " +
			"Offenlegung, aber nicht gegen einen Angreifer mit Zugriff auf dieses Benutzerkonto."
	}
	return status, nil
}

// AppInfo is the running build's identity, shown in Settings so a user can tell
// which version they are on and what to quote in a bug report.
type AppInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// GetAppInfo returns the build identity.
//
// Unlike every other bound method it does not call ensure(): the version has to
// answer even when initialisation failed, because that is exactly the moment
// someone wants to know what they are running.
func (a *App) GetAppInfo() AppInfo {
	return AppInfo{
		Version:   version.Version,
		Commit:    version.Commit,
		Date:      version.Date,
		GoVersion: goruntime.Version(),
		Platform:  goruntime.GOOS + "/" + goruntime.GOARCH,
	}
}

// GetTracePath returns the diagnostic trace's file.
//
// Settings -> About shows it and copies it, because the usual way to report "it
// failed yesterday" is a file the user cannot find on their own. The path is
// answered even when the database is locked: the trace belongs to the start, not
// to the history, and a failed unlock is exactly when it is worth reading.
func (a *App) GetTracePath() string {
	_ = a.ensureInit()
	if a.trace == nil {
		return ""
	}
	return a.trace.Path()
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
		return research.SearchOutcome{}, uimsg.Errorf("search.noBraveKey")
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
	return a.currentRegistry().Health(ctx), nil
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
		Providers:    a.currentChain().ProviderAvailability(),
		Order:        settings.Search.ProviderOrder,
		BraveQueries: a.currentChain().BraveQueriesThisMonth(),
		// Brave's metered plan gives $5 of free credit per month at
		// $5 per 1,000 requests, so about a thousand queries.
		BraveQuotaEst: 1000,
		CacheMinutes:  settings.Search.CacheMinutes,
		SecretBackend: a.secrets.Backend(),
	}, nil
}

// OpenExternal opens a URL in the user's browser.
//
// The string is not trusted to be a URL the operating system should act on. Three
// callers hand it text this app did not author: a markdown link in a model's reply,
// a citation row built from a scraped page's own title, and a help link in Settings.
// A `file://` path opens a local file in whatever program claims it, `smb://` asks
// for a network share, and any registered scheme reaches its handler — each from one
// click on text a model produced. Only what a citation can legitimately be is let
// through; everything else is dropped, because the click has nowhere useful to go and
// a dialog about a link that will not open is worse than nothing happening.
func (a *App) OpenExternal(target string) {
	if a.ctx == nil || !isWebURL(target) {
		return
	}
	runtime.BrowserOpenURL(a.ctx, target)
}

// isWebURL reports whether target is an absolute http(s) URL naming a host.
//
// The scheme is read from the parsed URL rather than from a prefix test: what a
// string starts with says nothing about the scheme `url.Parse` finds after leading
// whitespace or an escaped separator, and `javascript:` is as easy to spell as
// `https:`.
func isWebURL(target string) bool {
	parsed, err := url.Parse(strings.TrimSpace(target))
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return parsed.Host != ""
	}
	return false
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
