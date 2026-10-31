// Package research provides web search across pluggable providers and turns
// fetched pages into clean Markdown for the model.
package research

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// SearchRequest is a normalised query.
type SearchRequest struct {
	Query         string `json:"query"`
	Count         int    `json:"count"`
	Freshness     string `json:"freshness,omitempty"` // pd | pw | pm | py | YYYY-MM-DDtoYYYY-MM-DD
	Country       string `json:"country,omitempty"`
	SearchLang    string `json:"searchLang,omitempty"`
	ExtraSnippets bool   `json:"extraSnippets,omitempty"`
	Offset        int    `json:"offset,omitempty"`
	// Lang is the resolved language a provider words its own errors and warnings in
	// (config.ModelLanguage). The chain fills it before every call, so a provider needs no
	// language of its own and cannot be built with a stale one: a settings save reaches the
	// next request rather than the next restart. It is not part of the wire format.
	Lang string `json:"-"`
}

// SearchResult is one normalised hit.
type SearchResult struct {
	Title    string   `json:"title"`
	URL      string   `json:"url"`
	Snippet  string   `json:"snippet"`
	Age      string   `json:"age,omitempty"`
	Extra    []string `json:"extra,omitempty"`
	Provider string   `json:"provider"`
	Rank     int      `json:"rank"`
}

// SearchProvider is one search backend.
type SearchProvider interface {
	// Name is the stable identifier used in settings ordering.
	Name() string
	// Available reports whether the provider can be used at all, e.g. whether an
	// API key is configured. DuckDuckGo is always available.
	Available() bool
	// RequiresKey reports whether the provider needs an API key.
	RequiresKey() bool
	Search(ctx context.Context, req SearchRequest) ([]SearchResult, error)
}

// ErrNoProvider means every configured provider was unavailable.
var ErrNoProvider = errors.New("no search provider available")

// ErrRateLimited is returned when a provider throttles a request for now.
var ErrRateLimited = errors.New("search provider rate limited")

// ErrQuotaExhausted means the account's plan or credit balance is used up, so
// waiting and retrying will not help: the caller should fall through to another
// provider and tell the user why rather than pretending it was a hiccup.
var ErrQuotaExhausted = errors.New("search provider quota exhausted")

// ErrEmptyQuery means there was nothing usable left to search for.
var ErrEmptyQuery = errors.New("empty search query")

// ErrInvalidKey means the provider rejected the configured API key. It is
// distinct from a throttle or an exhausted balance because retrying cannot help
// and the user has to be told to fix the key instead.
var ErrInvalidKey = errors.New("search provider rejected the API key")

// ErrBlocked means the provider answered an automated request with an anti-bot
// interstitial instead of results. Neither waiting nor repairing a selector
// helps: the request has to come from a client the provider trusts, or from a
// different provider, and until then the truth is that the search was refused
// rather than empty.
var ErrBlocked = errors.New("search provider blocked the request as automation")

// httpError mirrors the backend HTTP error for search providers.
type httpError struct {
	StatusCode int
	Status     string
	Body       string
	Provider   string
}

func (e *httpError) Error() string {
	body := e.Body
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	return fmt.Sprintf("%s: %s %s", e.Provider, e.Status, body)
}

// hashKey returns a stable cache key for an arbitrary string.
func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// rateLimiter enforces a minimum interval between calls to keep within the
// lowest published provider tier (Brave allows roughly one query per second).
type rateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
}

func newRateLimiter(perSecond float64) *rateLimiter {
	if perSecond <= 0 {
		return &rateLimiter{}
	}
	return &rateLimiter{interval: time.Duration(float64(time.Second) / perSecond)}
}

// Wait blocks until the next call is allowed or the context is cancelled.
func (r *rateLimiter) Wait(ctx context.Context) error {
	// An already-cancelled context must not proceed to make a network request.
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.interval == 0 {
		return nil
	}
	r.mu.Lock()
	wait := r.interval - time.Since(r.last)
	if wait < 0 {
		wait = 0
	}
	r.last = time.Now().Add(wait)
	r.mu.Unlock()
	if wait == 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dedupeResults removes duplicate URLs and re-ranks, so a provider returning the
// same link twice does not waste context. Results without a URL are kept: an
// infobox or FAQ answer is valuable even though it has no link of its own.
func dedupeResults(results []SearchResult) []SearchResult {
	seen := map[string]bool{}
	out := make([]SearchResult, 0, len(results))
	for _, r := range results {
		key := strings.ToLower(strings.TrimRight(r.URL, "/"))
		if key == "" {
			// Fall back to the title so two distinct URL-less answers survive
			// while an exact repeat does not.
			key = "title:" + strings.ToLower(strings.TrimSpace(r.Title))
		}
		if key == "title:" || seen[key] {
			continue
		}
		seen[key] = true
		r.Rank = len(out) + 1
		out = append(out, r)
	}
	return out
}

// trackingParams are query keys that identify a click rather than a document.
var trackingParams = map[string]bool{
	"utm_source": true, "utm_medium": true, "utm_campaign": true,
	"utm_term": true, "utm_content": true, "gclid": true, "fbclid": true,
	"mc_cid": true, "mc_eid": true, "ref": true, "yt0": true, "rut": true,
}

// NormaliseURL is normaliseURL for callers outside this package.
//
// The scrape cache is keyed by it, and so is the source URL an indexed document is
// stored under, so anything looking one of those up has to normalise the URL it was
// given the same way this package does.
func NormaliseURL(raw string) string { return normaliseURL(raw) }

// normaliseURL collapses the variations of one address into a single form:
// tracking parameters removed, fragment dropped, and a trailing path slash
// trimmed. Without this the same page can be scraped, cached and cited twice.
func normaliseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		// Not a normal absolute URL; leave it untouched rather than corrupt it.
		return raw
	}
	parsed.Fragment = ""
	if len(parsed.Path) > 1 {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	}
	if parsed.RawQuery != "" {
		keep := make([]string, 0, 4)
		for _, part := range strings.Split(parsed.RawQuery, "&") {
			if part == "" {
				continue
			}
			name := part
			if eq := strings.Index(part, "="); eq >= 0 {
				name = part[:eq]
			}
			if trackingParams[strings.ToLower(name)] {
				continue
			}
			keep = append(keep, part)
		}
		parsed.RawQuery = strings.Join(keep, "&")
	}
	return parsed.String()
}

func sortResultsByRank(results []SearchResult) {
	sort.SliceStable(results, func(i, j int) bool { return results[i].Rank < results[j].Rank })
}
