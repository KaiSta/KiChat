package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"kichat/internal/config"
	"kichat/internal/store"
)

// Options configures the search chain.
type Options struct {
	ProviderOrder []string
	BraveAPIKey   string
	Country       string
	SearchLang    string
	ResultCount   int
	ExtraSnippets bool
	CacheMinutes  int
}

// SearchOutcome is the result of a chained search.
type SearchOutcome struct {
	Results  []SearchResult `json:"results"`
	Provider string         `json:"provider"`
	Cached   bool           `json:"cached"`
	// Warning explains a degraded result, e.g. a fallback after a quota error.
	// It is surfaced in the UI rather than hidden.
	Warning string `json:"warning,omitempty"`
}

// Chain tries search providers in the configured order and falls through on
// failure, so one exhausted API quota never breaks a turn.
type Chain struct {
	mu        sync.RWMutex
	providers map[string]SearchProvider
	order     []string
	opts      Options
	store     *store.Store

	// braveEndpoint is overridable for tests.
	braveEndpoint string
}

// NewChain builds the chain. Both providers are always constructed; availability
// is decided per request.
func NewChain(st *store.Store, opts Options, braveEndpoint string) *Chain {
	chain := &Chain{store: st, braveEndpoint: braveEndpoint}
	chain.Configure(opts)
	return chain
}

// Configure (re)builds providers from settings.
func (c *Chain) Configure(opts Options) {
	if len(opts.ProviderOrder) == 0 {
		opts.ProviderOrder = []string{"brave", "duckduckgo"}
	}
	if opts.ResultCount <= 0 {
		opts.ResultCount = 8
	}
	if opts.CacheMinutes <= 0 {
		opts.CacheMinutes = 60
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts = opts
	c.order = append([]string(nil), opts.ProviderOrder...)
	c.providers = map[string]SearchProvider{
		"brave":      NewBraveProvider(opts.BraveAPIKey, c.braveEndpoint),
		"duckduckgo": NewDuckDuckGoProvider(""),
	}
}

// Options returns the active configuration.
func (c *Chain) Options() Options {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.opts
}

// ProviderAvailability reports which providers are usable right now.
func (c *Chain) ProviderAvailability() map[string]bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := map[string]bool{}
	for name, p := range c.providers {
		out[name] = p.Available()
	}
	return out
}

// BraveQueriesThisMonth reports Brave usage against the free monthly credit.
func (c *Chain) BraveQueriesThisMonth() int64 {
	if c.store == nil {
		return 0
	}
	return c.store.Counter("search:brave", config.CurrentPeriod())
}

// Search runs a query through the chain.
func (c *Chain) Search(ctx context.Context, query string, req SearchRequest) (SearchOutcome, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return SearchOutcome{}, errors.New("leere Suchanfrage")
	}

	c.mu.RLock()
	order := append([]string(nil), c.order...)
	providers := make(map[string]SearchProvider, len(c.providers))
	for k, v := range c.providers {
		providers[k] = v
	}
	opts := c.opts
	c.mu.RUnlock()

	if req.Count <= 0 {
		req.Count = opts.ResultCount
	}
	if req.Country == "" {
		req.Country = opts.Country
	}
	if req.SearchLang == "" {
		req.SearchLang = opts.SearchLang
	}
	if !req.ExtraSnippets {
		req.ExtraSnippets = opts.ExtraSnippets
	}
	req.Query = query

	// Cache lookup is keyed on everything that changes the response.
	cacheKey := hashKey(fmt.Sprintf("v1|%s|%d|%s|%s|%s|%d",
		strings.ToLower(query), req.Count, req.Freshness, req.Country, req.SearchLang, req.Offset))
	if c.store != nil {
		if entry, ok := c.store.GetSearchCache(cacheKey); ok {
			var cached []SearchResult
			if err := jsonUnmarshalResults(entry.ResponseJSON, &cached); err == nil && len(cached) > 0 {
				return SearchOutcome{Results: cached, Provider: entry.Provider, Cached: true}, nil
			}
		}
	}

	var (
		errs       []string
		fellBack   bool
		firstTried string
	)
	for _, name := range order {
		p, ok := providers[name]
		if !ok {
			continue
		}
		if !p.Available() {
			reason := "nicht konfiguriert"
			if !p.RequiresKey() {
				reason = "nicht verfügbar"
			}
			errs = append(errs, fmt.Sprintf("%s: %s", name, reason))
			continue
		}
		if firstTried == "" {
			firstTried = name
		}

		results, err := p.Search(ctx, req)
		if err == nil && len(results) > 0 {
			outcome := SearchOutcome{Results: results, Provider: name}
			if fellBack {
				outcome.Warning = fmt.Sprintf("%s war nicht verfügbar, Ergebnisse von %s", firstTried, name)
			}
			if c.store != nil {
				if raw, mErr := jsonMarshalResults(results); mErr == nil {
					_ = c.store.PutSearchCache(store.SearchCacheEntry{
						QueryHash:    cacheKey,
						Provider:     name,
						Query:        query,
						ResponseJSON: raw,
						CreatedAt:    time.Now(),
						ExpiresAt:    time.Now().Add(time.Duration(opts.CacheMinutes) * time.Minute),
					})
				}
				if name == "brave" {
					_, _ = c.store.IncrCounter("search:brave", config.CurrentPeriod())
				}
			}
			return outcome, nil
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return SearchOutcome{}, err
			}
			errs = append(errs, err.Error())
		} else {
			errs = append(errs, fmt.Sprintf("%s: keine Ergebnisse", name))
		}
		fellBack = true
	}

	if len(errs) == 0 {
		return SearchOutcome{}, ErrNoProvider
	}
	return SearchOutcome{}, fmt.Errorf("Suche fehlgeschlagen: %s", strings.Join(errs, " | "))
}

// GroundedContext returns pre-extracted, token-budgeted page content for a
// query, preferring Brave's LLM-context route and degrading to a plain web
// search when that route is unavailable or the account cannot use it.
//
// The fallback still returns the same shape, built from result descriptions, so
// the caller has one thing to handle and the turn is never blocked by the state
// of a metered third-party plan. Whatever degradation happened is reported in
// Warning rather than hidden.
func (c *Chain) GroundedContext(ctx context.Context, query string, req SearchRequest, maxTokens int) (GroundedContext, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return GroundedContext{}, ErrEmptyQuery
	}

	c.mu.RLock()
	brave, _ := c.providers["brave"].(*BraveProvider)
	opts := c.opts
	c.mu.RUnlock()

	if req.Country == "" {
		req.Country = opts.Country
	}
	if req.SearchLang == "" {
		req.SearchLang = opts.SearchLang
	}
	req.Query = query

	// A separate key prefix namespaces this cache: the same query yields page
	// chunks here and result listings elsewhere, and they must not be confused.
	cacheKey := hashKey(fmt.Sprintf("ctx1|%s|%d|%d|%s|%s|%s",
		strings.ToLower(query), maxTokens, req.Count, req.Freshness, req.Country, req.SearchLang))
	if c.store != nil {
		if entry, ok := c.store.GetSearchCache(cacheKey); ok {
			var cached GroundedContext
			if err := json.Unmarshal([]byte(entry.ResponseJSON), &cached); err == nil && !cached.Empty() {
				cached.Cached = true
				return cached, nil
			}
		}
	}

	var reasons []string
	if brave != nil && brave.Available() {
		grounded, err := brave.Context(ctx, req, maxTokens)
		if err == nil && !grounded.Empty() {
			c.recordGrounded(cacheKey, opts, grounded)
			return grounded, nil
		}
		switch {
		case err == nil:
			reasons = append(reasons, "brave-context: keine Abschnitte")
		case errors.Is(err, ErrQuotaExhausted):
			reasons = append(reasons, "Brave-Kontingent aufgebraucht")
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return GroundedContext{}, err
		default:
			reasons = append(reasons, err.Error())
		}
	} else {
		reasons = append(reasons, "brave: kein API-Key konfiguriert")
	}

	// Fall back to a normal search. Sources are then only as good as their
	// descriptions, which is worth saying out loud.
	outcome, err := c.Search(ctx, query, req)
	if err != nil {
		return GroundedContext{}, fmt.Errorf("Recherche fehlgeschlagen: %s", strings.Join(reasons, " | "))
	}
	grounded := GroundedContext{
		Provider:  outcome.Provider,
		MaxTokens: 0,
		Warning: fmt.Sprintf(
			"LLM-Kontext nicht verfügbar (%s) – nur Kurzbeschreibungen aus der Websuche",
			strings.Join(reasons, " | ")),
	}
	if outcome.Warning != "" {
		grounded.Warning += "; " + outcome.Warning
	}
	for _, r := range outcome.Results {
		if r.Snippet == "" && len(r.Extra) == 0 {
			continue
		}
		snippets := make([]string, 0, 1+len(r.Extra))
		if r.Snippet != "" {
			snippets = append(snippets, r.Snippet)
		}
		snippets = append(snippets, r.Extra...)
		grounded.Sources = append(grounded.Sources, GroundedSource{
			URL:      r.URL,
			Title:    r.Title,
			Snippets: snippets,
			Kind:     "page",
		})
	}
	if grounded.Empty() {
		return GroundedContext{}, fmt.Errorf("Recherche ohne verwertbare Inhalte: %s", strings.Join(reasons, " | "))
	}
	c.recordGrounded(cacheKey, opts, grounded)
	return grounded, nil
}

// recordGrounded caches a grounded result and counts the metered request. The
// counter matters because the context route bills like any other Brave query.
func (c *Chain) recordGrounded(cacheKey string, opts Options, grounded GroundedContext) {
	if c.store == nil {
		return
	}
	if raw, err := json.Marshal(grounded); err == nil {
		_ = c.store.PutSearchCache(store.SearchCacheEntry{
			QueryHash:    cacheKey,
			Provider:     grounded.Provider,
			Query:        "",
			ResponseJSON: string(raw),
			CreatedAt:    time.Now(),
			ExpiresAt:    time.Now().Add(time.Duration(opts.CacheMinutes) * time.Minute),
		})
	}
	if strings.HasPrefix(grounded.Provider, "brave") {
		_, _ = c.store.IncrCounter("search:brave", config.CurrentPeriod())
	}
}

func jsonMarshalResults(results []SearchResult) (string, error) {
	raw, err := json.Marshal(results)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func jsonUnmarshalResults(raw string, out *[]SearchResult) error {
	return json.Unmarshal([]byte(raw), out)
}
