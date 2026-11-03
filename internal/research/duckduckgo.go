package research

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// DuckDuckGo has no official web-search API. This provider scrapes the
// no-JavaScript HTML endpoints, which is inherently fragile: the selectors below
// are covered by fixture tests so layout drift is caught immediately, and the
// lite endpoint is tried as a second shape. What is not a layout change is the
// anti-bot interstitial, which is reported as what it is.
//
// Requests go out as a browser (browserclient.go) because that is what the host judges: measured
// on 2026-11-03, the same query from the same machine was answered with the interstitial over
// net/http and with ten results over a Chrome handshake, in the same minute.
const (
	ddgHTMLEndpoint = "https://html.duckduckgo.com/html/"
	ddgLiteEndpoint = "https://lite.duckduckgo.com/lite/"
)

// ddgChallengeMarkers are strings only the interstitial carries: the challenge
// script, its form and its modal. They are what a block is detected by, because
// neither the status nor an empty parse can tell it apart from a real answer —
// measured on 2026-10-08, a host DuckDuckGo did not trust was answered with HTTP
// 202 (not an error status) and a page with no result nodes at all, which is
// exactly what a genuine zero-result page looks like from the outside.
var ddgChallengeMarkers = []string{"anomaly.js", "challenge-form", "anomaly-modal"}

// ddgBlockedErr is returned when the interstitial is served instead of results.
// It names the reason and the way out: "no results" would send the reader
// looking for a selector bug in a page that was never served.
//
// A function rather than a var because it is a sentence: the language is the caller's, and a
// package-level error would freeze whichever language was current when the package loaded.
func ddgBlockedErr(lang string) error {
	return fmt.Errorf("%w: %s", ErrBlocked, text(lang,
		"DuckDuckGo verlangt eine Bot-Prüfung (Captcha) statt Suchergebnissen – im Browser "+
			"lösen oder einen anderen Suchanbieter konfigurieren",
		"DuckDuckGo served a bot check (captcha) instead of results — solve it in a browser, "+
			"or configure another search provider"))
}

// isDDGChallenge reports whether a response body is the interstitial rather than
// a page of results.
func isDDGChallenge(body string) bool {
	for _, marker := range ddgChallengeMarkers {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

// Pacing and retry policy for the scraped provider.
//
// A round of parallel search_web calls is a burst from one client, and a burst is the shape
// DuckDuckGo answers with its anti-bot interstitial. Reported in use: of four searches started
// together, the first came back with results and the rest with the challenge. The numbers
// below are the answer to that, and they are conservative on purpose — a scrape is punished
// far earlier than a documented API call.
const (
	// ddgBaseGap is the gap between two requests: 0.5 per second, half of Brave's rate.
	ddgBaseGap = 2 * time.Second
	// ddgMaxGap is what the gap may grow to after a refusal. Beyond it a burst is cheaper to
	// answer from the other layout or from another provider than to keep queueing.
	ddgMaxGap = 8 * time.Second
	// ddgMaxWait is the longest one search waits for its slot before it reports a throttle
	// instead of queueing: a turn spent in a queue is a turn that never arrives.
	//
	// It has to hold two searches queued behind one refusal: the gap grows to 4 s on the first
	// and 8 s on the second, so the burst that starts together is served 8 s and 16 s out. The
	// old 10 s turned the second one away without asking the host at all — which is what "one of
	// two parallel searches always fails immediately" was. The ceiling stays well under
	// ddgSearchBudget, so a queue still cannot outlast the search it belongs to.
	ddgMaxWait = 20 * time.Second
	// ddgAttempts is how many times one search asks before it reports the failure: one look,
	// then retries for a connection that dropped or a page that arrived empty.
	ddgAttempts = 3
	// ddgBlockTries caps how often a search asks after the interstitial: one look and two
	// retries, the second at the 8 s ceiling. The retry exists because a burst's later calls
	// are often refused while the first was not, and the cooldown is what it waits out — so it
	// is worth asking while the cooldown is still growing. A host that refuses that third look
	// is refusing the client rather than pacing it, and saying so is cheaper than asking again.
	ddgBlockTries = 3
	// ddgRetryBase is the first retry delay: 1 s, doubling, capped at ddgRetryMax.
	ddgRetryBase = 1 * time.Second
	ddgRetryMax  = 6 * time.Second
	// ddgSearchBudget bounds one search end to end — attempts, backoff and pacing alike.
	ddgSearchBudget = 30 * time.Second
)

// errDDGBusy is the pacer declining to wait for a slot: the next one is further away than one
// search may spend. Nothing was asked of the host, so it is a throttle rather than a refusal.
var errDDGBusy = errors.New("duckduckgo: no free slot within the wait")

// ddgBusyErr is errDDGBusy worded for the caller, as ErrRateLimited: retrying in a moment is
// worth it, and a chain with another provider should fall through to it now.
func ddgBusyErr(lang string) error {
	return fmt.Errorf("%w: %s", ErrRateLimited, text(lang,
		"zu viele DuckDuckGo-Anfragen gleichzeitig – kurz warten und erneut versuchen",
		"too many DuckDuckGo requests at once — wait a moment and try again"))
}

// errDDGNoResults is a page that parsed but carried no hit: either the query really has none,
// or the layout drifted. Both look identical from outside, and both are worth one more look.
var errDDGNoResults = errors.New("duckduckgo: no results")

// ddgPacer is the pacing a scraped provider needs beyond a fixed interval.
//
// A fixed gap cannot absorb a burst, because the requests in one are already spaced by it:
// what changes the host's mind is that the gap grows. So a refusal doubles the gap (up to
// ddgMaxGap) and holds the next slot one whole gap into the future, and the first real answer
// puts both back. Every caller shares one pacer, which is what makes a round of parallel
// searches queue behind a refusal instead of all landing on a host that has just decided this
// machine is a bot.
type ddgPacer struct {
	mu   sync.Mutex
	base time.Duration
	max  time.Duration
	gap  time.Duration // the current gap between two requests
	next time.Time     // the earliest start of the next request
	// blocks counts consecutive refusals, which is what grows the gap; a success drops it.
	blocks int
}

func newDDGPacer(base, max time.Duration) *ddgPacer {
	return &ddgPacer{base: base, max: max, gap: base}
}

// Wait reserves the next slot and sleeps until it arrives. errDDGBusy means the slot is
// further away than one search may wait — somebody was just refused, and queueing behind that
// would spend the turn to learn the same thing. A declined slot is not consumed: a caller that
// is turned away must not push everyone behind it further out, which is how a busy pacer used
// to keep making itself busier.
//
// The slot is claimed when the caller actually fires, not when it asks to: the sleep is
// followed by another look at the queue, because a refusal during it moves the cooldown and
// the caller has to wait that out. Booking at ask time made the cooldown apply to nobody who
// was already waiting — a burst of four reserved four slots two seconds apart and then fired
// three of them inside the cooldown the first refusal had just created, which is the same
// failure the pacer exists to prevent. Pinned by TestDDGPacerHoldsACallerThatIsAlreadyWaiting.
func (p *ddgPacer) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.mu.Lock()
		now := time.Now()
		start := p.next
		if start.Before(now) {
			start = now
		}
		if start.Sub(now) > ddgMaxWait {
			p.mu.Unlock()
			return errDDGBusy
		}
		if !start.After(now) {
			// The slot is free now: claim it and go.
			p.next = now.Add(p.gap)
			p.mu.Unlock()
			return nil
		}
		p.mu.Unlock()

		if err := sleepContext(ctx, time.Until(start)); err != nil {
			return err
		}
	}
}

// Blocked records a refusal: the gap doubles up to the ceiling, and no request goes out for
// one whole gap — whichever caller it belongs to.
//
// The first refusal doubles it too, which is the whole point: a refusal is the host saying the
// spacing that was fine a moment ago is not, so the next request has to be further out than
// the one that was refused rather than one base gap behind it. Leaving the first refusal at the
// base gap made the retry land as soon as the request it repeated, so a search refused once was
// refused again and gave up — the case two parallel searches hit every time.
func (p *ddgPacer) Blocked() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocks++
	p.gap = p.base
	for i := 0; i < p.blocks && p.gap < p.max; i++ {
		p.gap *= 2
	}
	if p.gap > p.max {
		p.gap = p.max
	}
	if hold := time.Now().Add(p.gap); p.next.Before(hold) {
		p.next = hold
	}
}

// Succeeded puts the pacing back to its base: a real answer means the host is not refusing this
// client, so staying slow after it would only cost time.
func (p *ddgPacer) Succeeded() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocks = 0
	p.gap = p.base
}

// sleepContext waits for d unless the context ends first, and answers the context's own error
// in that case. A zero or negative d is not a wait at all.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ddgJitter spreads a retry delay by up to a fifth either way, so two searches refused in the
// same round do not come back in lockstep and draw a second refusal together.
func ddgJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	spread := float64(d) * 0.2
	return d + time.Duration((rand.Float64()*2-1)*spread)
}

// ddgRetryable reports whether asking again can plausibly change the answer. A refusal, a
// throttle, a server fault and a dropped connection all can clear by themselves, and an empty
// page can be the tail of a burst; a 4xx is the request being wrong.
func ddgRetryable(err error) bool {
	var httpErr *httpError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= 500
	}
	return true
}

// DuckDuckGoProvider scrapes DuckDuckGo's HTML search results.
type DuckDuckGoProvider struct {
	Endpoint string
	LiteURL  string
	// pacer spaces the requests, and one pacer is shared by every caller of this provider:
	// that is what keeps a round of parallel tool calls from arriving as a burst.
	pacer *ddgPacer
	// gate holds one search at a time, so a round of parallel search_web calls is served in
	// sequence rather than interleaved. Spacing alone was not enough: two searches started
	// together reserved their slots at the same instant, two seconds apart, and the host
	// refuses a second request two seconds after the first. Run one after the other, the same
	// pair is separated by the first search's own duration plus the pacing gap — four to five
	// seconds in the measurement that matters — and the second one is asked only after the
	// first has reported whether the client is being refused at all.
	gate chan struct{}
	// client is the browser identity the requests go out as. It matters more than the
	// request does: measured on 2026-11-03, the same query from the same machine was answered
	// with the anti-bot interstitial over net/http and with ten results over this client.
	client *browserClient

	// The retry policy lives on the struct rather than in the constants alone, so a test can
	// make a retry instantaneous instead of a second of sleeping (newTestDDGProvider).
	attempts   int
	blockTries int
	retryBase  time.Duration
	budget     time.Duration
	jitter     func(time.Duration) time.Duration
}

// NewDuckDuckGoProvider builds the provider. Endpoints are overridable for tests.
func NewDuckDuckGoProvider(endpoint string) *DuckDuckGoProvider {
	if endpoint == "" {
		endpoint = ddgHTMLEndpoint
	}
	return &DuckDuckGoProvider{
		Endpoint:   endpoint,
		LiteURL:    ddgLiteEndpoint,
		pacer:      newDDGPacer(ddgBaseGap, ddgMaxGap),
		gate:       make(chan struct{}, 1),
		client:     newBrowserClient(chromeWindows),
		attempts:   ddgAttempts,
		blockTries: ddgBlockTries,
		retryBase:  ddgRetryBase,
		budget:     ddgSearchBudget,
		jitter:     ddgJitter,
	}
}

// retryDelay is the wait before attempt n: doubling from retryBase, capped, and spread by the
// jitter that keeps two searches refused in the same round from retrying in lockstep.
func (d *DuckDuckGoProvider) retryDelay(attempt int) time.Duration {
	delay := d.retryBase
	for i := 1; i < attempt && delay > 0 && delay < ddgRetryMax; i++ {
		delay *= 2
	}
	if delay > ddgRetryMax {
		delay = ddgRetryMax
	}
	if d.jitter == nil {
		return delay
	}
	return d.jitter(delay)
}

func (d *DuckDuckGoProvider) Name() string      { return "duckduckgo" }
func (d *DuckDuckGoProvider) Available() bool   { return true }
func (d *DuckDuckGoProvider) RequiresKey() bool { return false }

// Search asks for one query, retrying the failures that can clear by themselves.
//
// The retry is what a burst needs rather than what one query needs. DuckDuckGo hands its
// anti-bot interstitial out per client, so the second and third call of a round — the ones a
// model starts in parallel — are refused while the first was answered; asking again a moment
// later, after the pacer has grown its gap and held every other caller back by the same
// amount, is what turns that round from three failures into a queue that fills in over a few
// seconds. Transient failures (a dropped connection, a 5xx, an empty page) are retried by the
// same loop, and a 4xx is not: that is the request being wrong.

// enter claims the provider for one search, or reports that the line is too long to stand in.
//
// The wait is bounded like the pacer's, for the same reason: a turn spent queueing behind a
// search that is itself waiting out a refusal learns nothing the queue could not have said
// immediately. Nothing about the caller's own context is shortened by waiting here — the
// search's budget starts once the search actually starts.
func (d *DuckDuckGoProvider) enter(ctx context.Context) (func(), error) {
	if d.gate == nil {
		return func() {}, nil
	}
	timer := time.NewTimer(ddgMaxWait)
	defer timer.Stop()
	select {
	case d.gate <- struct{}{}:
		return func() { <-d.gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errDDGBusy
	}
}

// Search asks for one query, retrying the failures that can clear by themselves. The gate is
// taken first, so a round of parallel calls is served one search at a time and the budget below
// starts when the search does rather than while it stands in line.
func (d *DuckDuckGoProvider) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	release, err := d.enter(ctx)
	if err != nil {
		if errors.Is(err, errDDGBusy) {
			return nil, ddgBusyErr(req.Lang)
		}
		return nil, err
	}
	defer release()

	searchCtx := ctx
	if d.budget > 0 {
		var cancel context.CancelFunc
		searchCtx, cancel = context.WithTimeout(ctx, d.budget)
		defer cancel()
	}

	var lastErr error
	blocks := 0
	for attempt := 1; attempt <= d.attempts; attempt++ {
		if attempt > 1 {
			if err := sleepContext(searchCtx, d.retryDelay(attempt-1)); err != nil {
				break
			}
		}
		results, err := d.searchOnce(searchCtx, req)
		if err == nil {
			d.pacer.Succeeded()
			return results, nil
		}
		lastErr = err
		if errors.Is(err, ErrBlocked) {
			blocks++
			// The refusal is the host's own pacing signal: the gap grows, and the next
			// request — this search's or another one's — waits out the cooldown.
			d.pacer.Blocked()
			if blocks >= d.blockTries {
				break
			}
			continue
		}
		if errors.Is(err, errDDGBusy) || !ddgRetryable(err) {
			break
		}
	}

	// The caller's own context is the one failure that must reach the chain as a
	// cancellation: a turn being stopped is not evidence about the provider.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	switch {
	case errors.Is(lastErr, errDDGNoResults):
		return nil, errors.New(text(req.Lang,
			"duckduckgo: keine Ergebnisse gefunden (Layout möglicherweise geändert)",
			"duckduckgo: no results found (the layout may have changed)"))
	case errors.Is(lastErr, errDDGBusy):
		return nil, ddgBusyErr(req.Lang)
	case errors.Is(lastErr, context.DeadlineExceeded):
		return nil, fmt.Errorf("%w: %s", ErrRateLimited, text(req.Lang,
			"DuckDuckGo hat nicht rechtzeitig geantwortet",
			"DuckDuckGo did not answer in time"))
	case lastErr == nil:
		return nil, errors.New(text(req.Lang, "duckduckgo: keine Antwort",
			"duckduckgo: no answer"))
	}
	return nil, lastErr
}

// searchOnce asks both layouts once, one paced request each. The first refusal ends it: the
// check is handed out per client rather than per layout, so the second shape cannot succeed
// where the first was refused, and the pacing it would wait out is time the turn does not
// have. The same reasoning one step later keeps a lite refusal whole instead of nesting it
// under whichever other failure came first — it explains itself better on its own.
func (d *DuckDuckGoProvider) searchOnce(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	if err := d.pacer.Wait(ctx); err != nil {
		return nil, err
	}
	results, err := d.searchHTML(ctx, req)
	if err == nil && len(results) > 0 {
		return results, nil
	}
	htmlErr := err
	if errors.Is(htmlErr, ErrBlocked) {
		return nil, htmlErr
	}

	if err := d.pacer.Wait(ctx); err != nil {
		return nil, err
	}
	liteResults, liteErr := d.searchLite(ctx, req)
	if liteErr == nil && len(liteResults) > 0 {
		return liteResults, nil
	}
	if errors.Is(liteErr, ErrBlocked) {
		return nil, liteErr
	}
	switch {
	case htmlErr != nil && liteErr != nil:
		return nil, fmt.Errorf(text(req.Lang, "duckduckgo: html: %v; lite: %v",
			"duckduckgo: html: %v; lite: %v"), htmlErr, liteErr)
	case liteErr != nil:
		return nil, fmt.Errorf(text(req.Lang, "duckduckgo lite: %w", "duckduckgo lite: %w"), liteErr)
	case htmlErr != nil:
		return nil, htmlErr
	default:
		return nil, errDDGNoResults
	}
}

func (d *DuckDuckGoProvider) searchHTML(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	// The fields are the ones the endpoint's own form posts: q, the empty page cursor b and
	// the region l. kl is not a field of this form at all — it is the setting the site stores
	// in a cookie for duckduckgo.com, and a submitted form carrying it is a form no browser
	// sends.
	form := url.Values{}
	form.Set("q", req.Query)
	form.Set("b", "")
	if region := ddgRegion(req); region != "" {
		form.Set("l", region)
	}
	if req.Freshness != "" {
		if df := ddgFreshness(req.Freshness); df != "" {
			form.Set("df", df)
		}
	}

	// POST is the more reliable form for this endpoint; some networks block the
	// GET variant.
	resp, err := d.client.postForm(ctx, d.Endpoint, form, req.SearchLang)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, &httpError{StatusCode: resp.StatusCode, Status: resp.Status, Provider: "duckduckgo"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if isDDGChallenge(string(body)) {
		return nil, ddgBlockedErr(req.Lang)
	}
	return parseDDGHtml(string(body), req.Count)
}

// parseDDGHtml extracts results from the html.duckduckgo.com markup.
func parseDDGHtml(body string, limit int) ([]SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	out := []SearchResult{}
	doc.Find("div.result, div.web-result").EachWithBreak(func(_ int, sel *goquery.Selection) bool {
		if hasClass(sel, "result--ad") || hasClass(sel, "result--ad--small") {
			return true
		}
		link := sel.Find("a.result__a").First()
		if link.Length() == 0 {
			return true
		}
		href, _ := link.Attr("href")
		target := unwrapDDGLink(href)
		title := cleanText(link.Text())
		if target == "" || title == "" {
			return true
		}
		snippet := cleanText(sel.Find(".result__snippet").First().Text())
		out = append(out, SearchResult{
			Title:    title,
			URL:      normaliseURL(target),
			Snippet:  snippet,
			Provider: "duckduckgo",
		})
		if limit > 0 && len(out) >= limit {
			return false
		}
		return true
	})

	out = dedupeResults(out)
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (d *DuckDuckGoProvider) searchLite(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	// The lite page's form posts the query alone; the region and freshness fields belong to
	// the html endpoint and are not read here.
	form := url.Values{}
	form.Set("q", req.Query)

	resp, err := d.client.postForm(ctx, d.LiteURL, form, req.SearchLang)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, &httpError{StatusCode: resp.StatusCode, Status: resp.Status, Provider: "duckduckgo-lite"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if isDDGChallenge(string(body)) {
		return nil, ddgBlockedErr(req.Lang)
	}
	return parseDDGLite(string(body), req.Count)
}

// parseDDGLite extracts results from the lite endpoint's table markup.
func parseDDGLite(body string, limit int) ([]SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	out := []SearchResult{}
	doc.Find("a.result-link").EachWithBreak(func(_ int, link *goquery.Selection) bool {
		href, _ := link.Attr("href")
		target := unwrapDDGLink(href)
		title := cleanText(link.Text())
		if target == "" || title == "" {
			return true
		}
		// The snippet lives in the following table row in the lite layout, but
		// older variants nest it next to the link, so both are tried.
		snippet := cleanText(link.Closest("tr").NextAll().Find(".result-snippet").First().Text())
		if snippet == "" {
			snippet = cleanText(link.Parent().Find(".result-snippet").First().Text())
		}
		if snippet == "" {
			snippet = cleanText(link.Closest("td").NextAllFiltered("td").First().Text())
		}
		out = append(out, SearchResult{
			Title:    title,
			URL:      normaliseURL(target),
			Snippet:  snippet,
			Provider: "duckduckgo-lite",
		})
		if limit > 0 && len(out) >= limit {
			return false
		}
		return true
	})
	out = dedupeResults(out)
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// ddgRegion renders the l parameter: the country and language, lower case, hyphenated, the
// form the endpoint's own region selector submits (de-de). An empty country leaves it out
// rather than guessing one, since the host then applies the request's own default.
func ddgRegion(req SearchRequest) string {
	if req.Country == "" {
		return ""
	}
	return strings.ToLower(req.Country) + "-" + defaultLang(req.SearchLang)
}

// unwrapDDGLink reverses DuckDuckGo's /l/?uddg= redirect wrapper.
func unwrapDDGLink(href string) string {
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return href
	}
	if uddg := parsed.Query().Get("uddg"); uddg != "" {
		if decoded, err := url.QueryUnescape(uddg); err == nil {
			return decoded
		}
		return uddg
	}
	if parsed.Host == "" && strings.HasPrefix(href, "/") {
		return "https://duckduckgo.com" + href
	}
	return href
}

func hasClass(sel *goquery.Selection, class string) bool {
	attr, _ := sel.Attr("class")
	for _, c := range strings.Fields(attr) {
		if c == class {
			return true
		}
	}
	return false
}

func defaultLang(lang string) string {
	if lang == "" {
		return "de"
	}
	return lang
}

// ddgFreshness maps our freshness tokens onto DuckDuckGo's df parameter.
func ddgFreshness(f string) string {
	switch f {
	case "pd":
		return "d"
	case "pw":
		return "w"
	case "pm":
		return "m"
	case "py":
		return "y"
	default:
		return ""
	}
}
