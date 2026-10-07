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
