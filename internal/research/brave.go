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
	"sync/atomic"
	"time"
)

// BraveEndpoint is the documented web-search route.
const BraveEndpoint = "https://api.search.brave.com/res/v1/web/search"

// BraveLLMContextEndpoint returns pre-extracted, token-budgeted page chunks.
// It is used when results are handed straight to a model.
const BraveLLMContextEndpoint = "https://api.search.brave.com/res/v1/llm/context"

// BraveProvider queries the Brave Search API.
type BraveProvider struct {
	APIKey   string
	Endpoint string
	// ContextEndpoint is the LLM-context route, overridable for the same reason
	// as Endpoint: fixtures must be replayable without spending API quota.
	ContextEndpoint string
	limiter         *rateLimiter
	client          *http.Client
	// lastQuota is the remaining-requests header of the last response. It is an
	// atomic because a provider is shared through the chain and the header is
	// written on the request goroutine while a status call may read it.
	lastQuota atomic.Int64
}

// NewBraveProvider builds a Brave provider. The endpoint is overridable for
// tests so parser fixtures can be replayed without network access.
func NewBraveProvider(apiKey, endpoint string) *BraveProvider {
	if endpoint == "" {
		endpoint = BraveEndpoint
	}
	return &BraveProvider{
		APIKey:          apiKey,
		Endpoint:        endpoint,
		ContextEndpoint: BraveLLMContextEndpoint,
		// Brave's published limit on the metered tier is about one query per
		// second; stay slightly under it.
		limiter: newRateLimiter(0.9),
		client:  &http.Client{Timeout: 20 * time.Second},
	}
}

func (b *BraveProvider) Name() string      { return "brave" }
func (b *BraveProvider) Available() bool   { return strings.TrimSpace(b.APIKey) != "" }
func (b *BraveProvider) RequiresKey() bool { return true }

// LastQuota reports the remaining requests when the API exposes it.
func (b *BraveProvider) LastQuota() int { return int(b.lastQuota.Load()) }

type braveResponse struct {
	Web *struct {
		Results []struct {
			Title         string   `json:"title"`
			URL           string   `json:"url"`
			Description   string   `json:"description"`
			Age           string   `json:"age"`
			ExtraSnippets []string `json:"extra_snippets"`
		} `json:"results"`
	} `json:"web"`
	News *struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
			Age         string `json:"age"`
		} `json:"results"`
	} `json:"news"`
	Infobox *struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		LongDesc    string `json:"long_desc"`
	} `json:"infobox"`
	FAQ *struct {
		Results []struct {
			Question string `json:"question"`
			Answer   string `json:"answer"`
			Title    string `json:"title"`
			URL      string `json:"url"`
		} `json:"results"`
	} `json:"faq"`
	Discussions *struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"discussions"`
}

// Search runs a Brave web search.
func (b *BraveProvider) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	if !b.Available() {
		return nil, fmt.Errorf("brave: kein API-Key konfiguriert")
	}
	if err := b.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("q", req.Query)
	count := req.Count
	if count <= 0 {
		count = 8
	}
	if count > 20 {
		count = 20
	}
	params.Set("count", strconv.Itoa(count))
	if req.Offset > 0 {
		if req.Offset > 9 {
			req.Offset = 9
		}
		params.Set("offset", strconv.Itoa(req.Offset))
	}
	if req.Freshness != "" {
		params.Set("freshness", req.Freshness)
	}
	if req.Country != "" {
		params.Set("country", strings.ToUpper(req.Country))
	}
	if req.SearchLang != "" {
		params.Set("search_lang", req.SearchLang)
	}
	if req.ExtraSnippets {
		params.Set("extra_snippets", "true")
	}
	// Keep the payload clean for the model: no HTML decoration in descriptions.
	params.Set("text_decorations", "false")
	params.Set("safesearch", "moderate")

	u := b.Endpoint + "?" + params.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// The subscription token goes in a header, never in the URL, so it cannot
	// leak through logs, proxies or error messages.
	httpReq.Header.Set("X-Subscription-Token", strings.TrimSpace(b.APIKey))
	httpReq.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("brave: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// Shared with the LLM-context route: also tells an exhausted credit
		// balance apart from a momentary throttle, which the status alone cannot.
		if err := classifyBraveFailure(resp.StatusCode, raw); err != nil {
			return nil, err
		}
	}
	if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			b.lastQuota.Store(int64(n))
		}
	}

	var parsed braveResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("brave: Antwort nicht lesbar: %w", err)
	}
	return parseBraveResponse(parsed, count), nil
}

// parseBraveResponse is separated so parser fixtures can be unit tested.
func parseBraveResponse(parsed braveResponse, limit int) []SearchResult {
	out := make([]SearchResult, 0, limit)
	if parsed.Web != nil {
		for _, r := range parsed.Web.Results {
			out = append(out, SearchResult{
				Title:    cleanText(r.Title),
				URL:      normaliseURL(r.URL),
				Snippet:  cleanText(r.Description),
				Age:      r.Age,
				Extra:    cleanTextSlice(r.ExtraSnippets),
				Provider: "brave",
			})
		}
	}
	if parsed.News != nil {
		for _, r := range parsed.News.Results {
			out = append(out, SearchResult{
				Title:    cleanText(r.Title),
				URL:      normaliseURL(r.URL),
				Snippet:  cleanText(r.Description),
				Age:      r.Age,
				Provider: "brave-news",
			})
		}
	}
	if parsed.FAQ != nil {
		for _, r := range parsed.FAQ.Results {
			out = append(out, SearchResult{
				Title:    cleanText(r.Question),
				URL:      normaliseURL(r.URL),
				Snippet:  cleanText(r.Answer),
				Provider: "brave-faq",
			})
		}
	}
	if parsed.Discussions != nil {
		for _, r := range parsed.Discussions.Results {
			out = append(out, SearchResult{
				Title:    cleanText(r.Title),
				URL:      normaliseURL(r.URL),
				Snippet:  cleanText(r.Description),
				Provider: "brave-discussions",
			})
		}
	}
	if parsed.Infobox != nil && parsed.Infobox.LongDesc != "" {
		out = append([]SearchResult{{
			Title:    cleanText(parsed.Infobox.Title),
			Snippet:  cleanText(firstNonEmpty(parsed.Infobox.LongDesc, parsed.Infobox.Description)),
			Provider: "brave-infobox",
		}}, out...)
	}
	out = dedupeResults(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	if len(out) == 0 {
		return nil
	}
	sortResultsByRank(out)
	return out
}
