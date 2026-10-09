package research

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

/*
 * Brave's LLM Context API (GET /res/v1/llm/context) is the AI-oriented sibling of
 * the web-search endpoint. Instead of titles and one-line descriptions it returns
 * pre-extracted page chunks, already selected for the query and already bounded
 * to a token budget.
 *
 * That matters here because KiChat's search-first path used to hand the model
 * nothing but headlines, so reading a source required a second round trip
 * through scrape_url that could fail, be blocked, or hit a paywall. With the
 * context route real page text reaches the prompt in a single metered request.
 */

// Documented limits for /res/v1/llm/context. They are enforced locally because
// the API answers an over-long query with a 422 rather than searching what it
// can, which would turn a usable question into a failed turn.
const (
	BraveContextQueryMaxChars = 600
	BraveContextQueryMaxWords = 75
	BraveContextCountMax      = 50
	BraveContextURLsMax       = 50
	BraveContextTokensMin     = 1024
	BraveContextTokensMax     = 32768
	// BraveContextTokensDefault mirrors the API default.
	BraveContextTokensDefault = 8192
	// BraveContextCountDefault mirrors the API default.
	BraveContextCountDefault = 20
)

// Error codes Brave returns inside its error envelope. The envelope is the only
// reliable discriminator: a used-up credit balance and a transient burst limit
// can arrive with the same HTTP status.
const (
	braveCodeQuota       = "QUOTA_LIMITED"
	braveCodeRateLimited = "RATE_LIMITED"
	// braveCodeSubscriptionTokenInvalid is what a rejected credential comes back
	// as. Measured against the real API (both routes, 422): the code is stable,
	// the free-text detail is not — a key-shaped value is answered with "The
	// provided API key is invalid." and anything else with "The provided
	// subscription token is invalid.". Classifying on the wording therefore
	// misses half of the failures, so the code decides and the wording is only a
	// fallback for a response without an envelope.
	braveCodeSubscriptionTokenInvalid = "SUBSCRIPTION_TOKEN_INVALID"
)

// GroundedSource is one URL with the chunks Brave extracted for a query.
type GroundedSource struct {
	URL      string   `json:"url"`
	Title    string   `json:"title"`
	Snippets []string `json:"snippets"`
	Kind     string   `json:"kind,omitempty"` // page | poi | place
}

// GroundedContext is token-budgeted page content ready to place in a prompt.
type GroundedContext struct {
	Sources   []GroundedSource `json:"sources"`
	Provider  string           `json:"provider"`
	Cached    bool             `json:"cached"`
	MaxTokens int              `json:"maxTokens"`
	// Warning explains a degraded result, e.g. a narrower fallback response.
	Warning string `json:"warning,omitempty"`
}

// Empty reports whether there is nothing worth putting in a prompt.
func (g GroundedContext) Empty() bool { return len(g.Sources) == 0 }

type braveContextResponse struct {
	Grounding *struct {
		Generic []struct {
			URL      string   `json:"url"`
			Title    string   `json:"title"`
			Snippets []string `json:"snippets"`
		} `json:"generic"`
		POI *struct {
			Name     string   `json:"name"`
			URL      string   `json:"url"`
			Title    string   `json:"title"`
			Snippets []string `json:"snippets"`
		} `json:"poi"`
		Map []struct {
			Name     string   `json:"name"`
			URL      string   `json:"url"`
			Title    string   `json:"title"`
			Snippets []string `json:"snippets"`
		} `json:"map"`
	} `json:"grounding"`
	Error *braveError `json:"error"`
}

type braveError struct {
	ID     string `json:"id"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
}

// Context fetches pre-extracted, token-budgeted page content for a query.
func (b *BraveProvider) Context(ctx context.Context, req SearchRequest, maxTokens int) (GroundedContext, error) {
	// Local validation comes first: an empty query is a caller bug that no
	// amount of configuration can fix, so it must not be reported as a
	// missing-key problem.
	query, clamped := clampBraveQuery(req.Query)
	if query == "" {
		return GroundedContext{}, ErrEmptyQuery
	}
	if !b.Available() {
		return GroundedContext{}, fmt.Errorf("brave: kein API-Key konfiguriert")
	}
	if err := b.limiter.Wait(ctx); err != nil {
		return GroundedContext{}, err
	}

	maxTokens = clampInt(maxTokens, BraveContextTokensMin, BraveContextTokensMax, BraveContextTokensDefault)

	count := req.Count
	if count <= 0 {
		count = BraveContextCountDefault
	}
	if count > BraveContextCountMax {
		count = BraveContextCountMax
	}

	params := url.Values{}
	params.Set("q", query)
	params.Set("count", strconv.Itoa(count))
	params.Set("maximum_number_of_tokens", strconv.Itoa(maxTokens))
	params.Set("maximum_number_of_urls", strconv.Itoa(min(count, BraveContextURLsMax)))
	params.Set("safesearch", "moderate")
	if req.Freshness != "" {
		params.Set("freshness", req.Freshness)
	}
	if req.Country != "" {
		params.Set("country", strings.ToUpper(req.Country))
	}
	if req.SearchLang != "" {
		params.Set("search_lang", req.SearchLang)
	}

	endpoint := b.ContextEndpoint
	if endpoint == "" {
		endpoint = BraveLLMContextEndpoint
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return GroundedContext{}, err
	}
	// The subscription token travels in a header only, never in the URL, so it
	// cannot leak through logs, proxies or error strings.
	httpReq.Header.Set("X-Subscription-Token", strings.TrimSpace(b.APIKey))
	httpReq.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return GroundedContext{}, fmt.Errorf("brave: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if err := classifyBraveFailure(resp.StatusCode, raw); err != nil {
			return GroundedContext{}, err
		}
	}
	if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			b.lastQuota = n
		}
	}

	var parsed braveContextResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return GroundedContext{}, fmt.Errorf("brave: Antwort nicht lesbar: %w", err)
	}
	// A 200 can still carry an error object on some plans.
	if parsed.Error != nil && parsed.Error.Code != "" {
		if err := classifyBraveFailure(resp.StatusCode, mustJSON(parsed.Error)); err != nil {
			return GroundedContext{}, err
		}
	}

	out := parseBraveContext(parsed, maxTokens)
	if clamped {
		out.Warning = fmt.Sprintf("Suchanfrage auf %d Wörter gekürzt (API-Limit)", BraveContextQueryMaxWords)
	}
	return out, nil
}

// parseBraveContext flattens the grounding envelope. Places and points of
// interest carry the same shape as pages, so they are kept as sources too: a
// local answer is still something the model should be able to cite.
func parseBraveContext(parsed braveContextResponse, maxTokens int) GroundedContext {
	out := GroundedContext{Provider: "brave-context", MaxTokens: maxTokens}
	if parsed.Grounding == nil {
		return out
	}
	add := func(url, title string, snippets []string, kind string) {
		cleaned := cleanTextSlice(snippets)
		if len(cleaned) == 0 {
			return
		}
		source := GroundedSource{
			URL:      normaliseURL(url),
			Title:    cleanText(title),
			Snippets: cleaned,
			Kind:     kind,
		}
		if source.URL == "" && source.Title == "" {
			return
		}
		out.Sources = append(out.Sources, source)
	}

	if p := parsed.Grounding.POI; p != nil {
		title := firstNonEmpty(p.Title, p.Name)
		add(p.URL, title, p.Snippets, "poi")
	}
	for _, place := range parsed.Grounding.Map {
		add(place.URL, firstNonEmpty(place.Title, place.Name), place.Snippets, "place")
	}
	for _, item := range parsed.Grounding.Generic {
		add(item.URL, item.Title, item.Snippets, "page")
	}

	out.Sources = dedupeGrounded(out.Sources)
	return out
}

// dedupeGrounded keeps one entry per URL so the same page cannot eat the budget
// twice. Entries without a URL are keyed by title.
func dedupeGrounded(sources []GroundedSource) []GroundedSource {
	seen := map[string]bool{}
	out := make([]GroundedSource, 0, len(sources))
	for _, s := range sources {
		key := strings.ToLower(strings.TrimRight(s.URL, "/"))
		if key == "" {
			key = "title:" + strings.ToLower(strings.TrimSpace(s.Title))
		}
		if key == "title:" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}

// clampBraveQuery trims a query to the documented llm/context limits. The second
// return value reports whether anything was dropped, so the caller can say so
// instead of silently searching something different from what was asked.
func clampBraveQuery(q string) (string, bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", false
	}
	clamped := false
	if words := strings.Fields(q); len(words) > BraveContextQueryMaxWords {
		words = words[:BraveContextQueryMaxWords]
		q = strings.Join(words, " ")
		clamped = true
	}
	if runes := []rune(q); len(runes) > BraveContextQueryMaxChars {
		cut := string(runes[:BraveContextQueryMaxChars])
		// Avoid ending mid-word when a space is close to the cut.
		if idx := strings.LastIndex(cut, " "); idx > BraveContextQueryMaxChars/2 {
			cut = cut[:idx]
		}
		q = cut
		clamped = true
	}
	return strings.TrimSpace(q), clamped
}

// classifyBraveFailure turns a non-200 into an error the UI can act on. Being
// out of credits and being throttled for a moment look alike over HTTP but need
// opposite responses — the first should fall through to another provider and say
// why, the second is worth retrying — so the error envelope's code decides.
func classifyBraveFailure(status int, body []byte) error {
	detail := strings.TrimSpace(string(body))
	code := ""
	var env struct {
		Error *braveError `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error != nil {
		code = strings.ToUpper(strings.TrimSpace(env.Error.Code))
		if d := strings.TrimSpace(env.Error.Detail); d != "" {
			detail = d
		}
	}
	switch code {
	case braveCodeQuota:
		return fmt.Errorf("%w: %s", ErrQuotaExhausted, detail)
	case braveCodeRateLimited:
		return fmt.Errorf("%w: %s", ErrRateLimited, detail)
	case braveCodeSubscriptionTokenInvalid:
		return braveKeyRejected(detail)
	}
	// A rejected key is worth its own error: measured against the real API it
	// arrives as 422, not the 401 the reference suggests, so the status alone
	// would be filed as a generic server error and the user would never be told
	// to fix the key. The status is only a fallback for an envelope without a
	// code: a 422 that blames the credential is the same authentication failure,
	// while one that blames the request — an over-long query, say — is fixed by
	// clamping and must keep its own error.
	if status == http.StatusUnauthorized || status == http.StatusForbidden ||
		(status == http.StatusUnprocessableEntity && namesCredential(detail)) {
		return braveKeyRejected(detail)
	}
	if status == http.StatusTooManyRequests {
		return fmt.Errorf("%w: %s", ErrRateLimited, detail)
	}
	return &httpError{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       detail,
		Provider:   "brave",
	}
}

// braveKeyRejected wraps a rejected credential. The detail Brave sends is in
// English, so the one action that can fix it is appended in the app's language:
// the message a user reads should point at the settings instead of looking like
// a server fault.
func braveKeyRejected(detail string) error {
	return fmt.Errorf("%w: %s (Brave-API-Key in den Einstellungen prüfen)", ErrInvalidKey, detail)
}

// namesCredential reports whether an envelope's detail blames the credential
// rather than the request. Both wordings are real, for the same failure: a
// key-shaped value is answered with "The provided API key is invalid." and any
// other value with "The provided subscription token is invalid.", so matching on
// one of them leaves the other classified as a server error.
func namesCredential(detail string) bool {
	d := strings.ToLower(detail)
	return strings.Contains(d, "api key") || strings.Contains(d, "subscription token")
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

func clampInt(v, lo, hi, fallback int) int {
	if v <= 0 {
		v = fallback
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
