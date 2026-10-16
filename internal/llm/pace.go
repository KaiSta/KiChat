package llm

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Pacing a turn against the minute's allowance.
//
// The per-request ceiling (limits.go, RequestCeiling) answers "will this request
// fit at all". This file answers a different question, the one that makes a long
// chat feel broken: "is there any of the minute's budget left to spend". Groq
// measures that minute two ways, and a turn can run out of either:
//
//   - **Tokens**, which the response headers state outright. A chat that sends a
//     turn every few seconds with a growing context spends them long before the
//     conversation is over — the refusal that follows is a 429 that names a wait of
//     seconds, and the turn that could have been sent a few seconds later was sent
//     now and lost.
//   - **Requests**, which no header of this API states at all. A single tool-heavy
//     turn is many requests back to back, and it can spend a minute's request rate
//     alone; the vendor's own word for that refusal is *"Rate limit reached for
//     model … on requests per minute (RPM): Limit 30, Used 30, Requested 1."*
//
// The two are read from different kinds of evidence, and the difference is the whole
// reason this file has two halves. Tokens are the vendor's *statement*: a remainder
// and a reset time with every response (noteRequestBudget), which is why a budget is
// never estimated — no statement means no wait. Requests are a *rate*, and the only
// thing this app can measure is its own part of it: how many of this model's requests
// it has had answered in the trailing minute. The limit that count is compared
// against is still the vendor's own number, learned from a refusal that named one
// (noteRequestRate), because the family of headers beside it describes the *day*.
//
// So a request is held back when the backend's own numbers say it cannot be paid
// for, and the wait is shown the way a retry's wait is (see RetryWatcher). Four
// boundaries, each of them the reason this is not simply "sleep until the window
// resets":
//
//   - Only a wait that can be shown is taken. A paced wait is published as a phase
//     on the chat (internal/chat/throttle.go), and only the turn's own request
//     carries a watcher. A listing, a health probe or an availability check has no
//     phase to say anything with, and parking one of those silently for a minute
//     would look exactly like a hung app — and a probe is bounded by its own
//     timeout, so it would fail rather than wait. Those requests are sent as
//     before; the vendor's own 429 and its hint still cover them.
//   - Only the vendor's numbers are used. The remaining allowance, the reset time
//     and the request rate are all read from statements the vendor made — a response
//     that says nothing teaches nothing, so no number here is ever assumed, and an
//     unknown limit means no wait rather than a guessed one.
//   - A wait is never longer than the window it is waiting for (retryHintCeiling).
//     These limits are defined per minute, so a reset beyond that is not this
//     vendor describing its own minute — and a turn parked for longer than a
//     minute is worth less than the refusal it avoids. The request-rate wait needs
//     no such check: it is the rest of a trailing minute by construction.
//   - Both allowances have to have room, so the wait taken is the longer of the two.
//     A minute with tokens to spare and no requests left is still a minute this
//     request cannot be sent in.
//
// What is *not* done here is reserving budget for a turn that is merely being
// typed, or pacing requests that are not part of a turn. The allowance is spent by
// requests, so that is where it is measured.

// The two headers beside the limit: what is left of the minute's allowance, and
// how long that remainder lasts. Groq documents both on every response — the
// answers as much as the refusals — and gives the reset as a duration ("7.66s",
// "2m59.56s"), which is what time.ParseDuration reads.
const (
	rateLimitRemainingTokensHeader = "X-Ratelimit-Remaining-Tokens"
	rateLimitResetTokensHeader     = "X-Ratelimit-Reset-Tokens"
)

// modelBudget is one model's tokens-per-minute allowance, as the backend last
// stated it on a response.
//
// It is a statement with a timestamp rather than a counter the app decrements: the
// allowance belongs to the account and the model, and every response restates it.
// Keeping the time is what lets the window be recognised as rolled over — after
// Reset has passed, the whole allowance is back, whether or not anything was sent.
type modelBudget struct {
	// Limit is the whole allowance for the minute (Groq's `limit-tokens`), which is
	// also the largest single request this account may send. 0 when the response
	// stated none, in which case the remainder is still usable: "12 tokens are left"
	// is a reading, even without knowing what they are left of.
	Limit int
	// Remaining is what was left of that allowance when the response arrived.
	Remaining int
	// Reset is how long the remaining allowance was said to last, and 0 when the
	// response stated no readable one — which is also "no usable statement".
	Reset time.Duration
	// At is when that response arrived, the anchor Reset is measured from.
	At time.Time
}

// remainingAt is what the allowance has left at a later moment.
//
// Past the reset the whole allowance is back, which is the difference between a
// budget and a counter: nothing has to be refilled for the next minute to be
// whole, so a window that has rolled over is not a window that is still spent.
func (b modelBudget) remainingAt(now time.Time) int {
	if b.Reset <= 0 || now.Sub(b.At) >= b.Reset {
		return b.Limit
	}
	return b.Remaining
}

// resetIn is how long the statement lasts from a later moment, or 0 once it has.
func (b modelBudget) resetIn(now time.Time) time.Duration {
	left := b.At.Add(b.Reset).Sub(now)
	if left < 0 {
		return 0
	}
	return left
}

// budgetFromHeaders reads one model's minute out of a response, or reports that the
// response stated none it can be planned against.
//
// A response that names a remainder without a readable reset is not a budget: the
// remainder says how much is left and the reset says for how long, and only the two
// together say whether waiting helps. Nothing here is guessed — an unparseable
// duration, a negative number or a missing header all mean "no statement", which
// costs a throttle the vendor's own retry path already handles and never costs a
// wait that nothing asked for.
func budgetFromHeaders(h http.Header) (modelBudget, bool) {
	// The limit is read by the same function the per-request ceiling is, because on
	// this vendor they are one number: the tokens-per-minute allowance is also the
	// largest request the account may send.
	limit := ceilingFromHeaders(h)

	raw := strings.TrimSpace(h.Get(rateLimitRemainingTokensHeader))
	if raw == "" {
		return modelBudget{}, false
	}
	remaining, err := strconv.Atoi(raw)
	if err != nil || remaining < 0 {
		return modelBudget{}, false
	}

	reset, err := time.ParseDuration(strings.TrimSpace(h.Get(rateLimitResetTokensHeader)))
	if err != nil || reset <= 0 {
		return modelBudget{}, false
	}
	return modelBudget{Limit: limit, Remaining: remaining, Reset: reset, At: time.Now()}, true
}

// noteRequestBudget remembers what one response said about a model's minute.
//
// The newest statement wins, and that is deliberately the opposite rule from
// noteRequestCeiling's: the ceiling is one fixed number that two places describe, so
// the tighter of them is kept, while this is a *reading of a moving quantity* — the
// allowance after this request — and a later response is not a second opinion about
// it but a fresher measurement of it. Keeping the older, smaller remainder would
// make the app wait for a minute that has already been refilled.
func (p *openAICompatProvider) noteRequestBudget(model string, h http.Header) {
	if model == "" {
		return
	}
	budget, ok := budgetFromHeaders(h)
	if !ok {
		return
	}
	p.budgetMu.Lock()
	defer p.budgetMu.Unlock()
	if p.requestBudgets == nil {
		p.requestBudgets = map[string]modelBudget{}
	}
	p.requestBudgets[model] = budget
}

// budgetFor reports the last thing a model's minute was said to be.
func (p *openAICompatProvider) budgetFor(model string) (modelBudget, bool) {
	p.budgetMu.Lock()
	defer p.budgetMu.Unlock()
	b, ok := p.requestBudgets[model]
	return b, ok
}

// requestWindow is the period a requests-per-minute allowance is defined over.
//
// It is a *trailing* minute rather than a fixed one, and the token headers are why
// that is the reading: a spent minute is described with a reset of 7.66s, which no
// clock-aligned window could produce — a fixed window would reset at its boundary and
// report up to a minute. So the requests this app has made are kept as the times they
// happened and the window is whatever the last minute of that list is.
const requestWindow = time.Minute

// requestRate is one model's requests-per-minute allowance: the number the vendor
// stated, and the requests this app itself has had answered inside the trailing
// window.
//
// The number and the count come from two different places on purpose. The limit is
// evidence and only ever the vendor's own — a refusal that said "Limit 30" — while the
// count is the app's own spend, which is the only thing it can measure about a rate no
// header publishes. The count is therefore a *lower bound* on the account's spend (a
// second client, or this app's own listings and probes, are spent too and are not
// counted), and the direction of that error is the safe one: the vendor's 429 still
// covers what the count misses, while a count that over-estimated would park turns for
// room the account had all along.
type requestRate struct {
	// Limit is the requests per minute the vendor stated, or 0 when nothing has said.
	Limit int
	// At holds the times, oldest first, of the responses this app has had for the
	// model. Anything older than the window is dropped on the next read.
	At []time.Time
}

// spentIn drops the requests that have fallen out of the window and reports how many
// are left in it.
func (r *requestRate) spentIn(now time.Time) int {
	kept := 0
	for _, at := range r.At {
		if now.Sub(at) < requestWindow {
			r.At[kept] = at
			kept++
		}
	}
	r.At = r.At[:kept]
	return kept
}

// roomIn is how long until the window has room for one more request, or 0 when it has
// room now — which is also the answer when nothing has stated a limit, because a rate
// nobody published is not something to wait for.
//
// A request is refused when the count has *reached* the limit rather than passed it
// (the vendor's own sentence: "Limit 30, Used 30, Requested 1"), so a full minute waits
// for its oldest request to fall out. That wait is the remainder of a trailing minute
// and can never exceed one.
func (r *requestRate) roomIn(now time.Time) time.Duration {
	if r.Limit <= 0 {
		return 0
	}
	if spent := r.spentIn(now); spent >= r.Limit {
		if wait := requestWindow - now.Sub(r.At[0]); wait > 0 {
			return wait
		}
	}
	return 0
}

// noteRequestRate remembers the requests-per-minute allowance a refusal named.
//
// The tighter of the two numbers is kept, for the reason noteRequestCeiling keeps the
// tighter: both describe one limit the account is governed by, and a plan that fits the
// smaller of two statements fits both — while the looser one costs the refusal it was
// learned from, again. The cost is one direction only, and it is the gap GROQ.md §9.7
// records for the token ceiling too: an allowance the account *raises* is not noticed
// until the app restarts, which is the price of never pacing against a number that is
// too generous.
func (p *openAICompatProvider) noteRequestRate(model string, limit int) {
	if model == "" || limit <= 0 {
		return
	}
	p.rateMu.Lock()
	defer p.rateMu.Unlock()
	r := p.rateFor(model)
	if r.Limit > 0 && r.Limit < limit {
		return
	}
	r.Limit = limit
}

// noteRequestSpent counts one of this model's requests against the minute.
//
// It is called from the response watcher, once per response, which is what makes the
// count the number of requests that actually reached the vendor rather than the number
// this app intended to make: a retry the shared client performed is its own request and
// is counted as one, and a request that never got an answer — a connection refused, a
// context cut short — is counted as none. A refusal is counted too, which is the
// conservative direction: whether a refused request is charged against the minute is not
// documented, and counting it costs a slower turn while not counting it costs the
// refusal this pacing exists to avoid.
func (p *openAICompatProvider) noteRequestSpent(model string, now time.Time) {
	if model == "" {
		return
	}
	p.rateMu.Lock()
	defer p.rateMu.Unlock()
	r := p.rateFor(model)
	r.At = append(r.At, now)
}

// requestWait is how long the minute's request rate needs before it can pay for one
// more request of this model, or 0 when it has room now or nothing has stated the rate.
func (p *openAICompatProvider) requestWait(model string, now time.Time) time.Duration {
	p.rateMu.Lock()
	defer p.rateMu.Unlock()
	r := p.requestRates[model]
	if r == nil {
		return 0
	}
	return r.roomIn(now)
}

// rateFor returns the model's rate for writing, creating it on first use. Callers hold
// rateMu.
func (p *openAICompatProvider) rateFor(model string) *requestRate {
	if p.requestRates == nil {
		p.requestRates = map[string]*requestRate{}
	}
	r := p.requestRates[model]
	if r == nil {
		r = &requestRate{}
		p.requestRates[model] = r
	}
	return r
}

// paceRequest holds a request back until both of the backend's minute allowances can
// pay for it, and reports how long it waited.
//
// It returns 0 when nothing was waited for, which is the answer of every backend but
// Groq, of every request neither allowance has been learned for, and of every request
// the minute already has room for. The wait is published on the request's watcher, so
// what the user sees is the same kind of line a retry shows, with the seconds one of
// the vendor's own numbers named.
func (p *openAICompatProvider) paceRequest(ctx context.Context, req ChatRequest) time.Duration {
	if p.modelsFlavour != flavourGroq {
		return 0
	}
	// A wait that cannot be shown is not taken: see the file comment.
	watcher := RetryWatcherOf(ctx)
	if watcher == nil {
		return 0
	}

	// The longer of the two, because the minute has room for this request only when
	// both allowances do — and each half reports 0 for "no wait" in every case where
	// waiting is not what this request needs (an unknown number, a covered request, a
	// token reset too far out to be this vendor's own minute).
	now := time.Now()
	wait := p.tokenWait(req, now)
	if requests := p.requestWait(req.Model, now); requests > wait {
		wait = requests
	}
	if wait <= 0 {
		return 0
	}

	// A wait with no failure behind it: nothing was refused, the request has not been
	// sent yet. internal/chat/throttle.go reads that as its own phase, because the
	// sentence a throttle deserves ("retrying in about 9 s") is the wrong one here.
	watcher.Waiting(wait, nil)
	defer watcher.Waited()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return wait
	case <-ctx.Done():
		// A Stop during the wait ends the turn, which is the same thing a Stop during
		// a retry's wait does.
		return 0
	}
}

// tokenWait is how long the minute's *token* allowance needs before it can pay for this
// request, or 0 when there is nothing to wait for: no statement has been read for the
// model, the request is already covered, or waiting cannot help at all.
func (p *openAICompatProvider) tokenWait(req ChatRequest, now time.Time) time.Duration {
	budget, ok := p.budgetFor(req.Model)
	if !ok {
		return 0
	}

	// What this request costs, estimated with the same counter the context meter and
	// the compaction planner use.
	prompt := EstimateMessagesTokens(req.Messages) + EstimateToolTokens(req.Tools)
	if prompt <= 0 {
		return 0
	}
	// A *prompt* larger than a whole minute's allowance can never be sent, however long
	// anyone waits — that is the size fold's case, and waiting for it would delay the
	// fold by the very minute it is trying to fit inside. It is the prompt and not the
	// request that is compared here, deliberately: the answer's room is a cap the user
	// set rather than a promise, and a large `max_tokens` must not read as a request too
	// big to send — which would switch the pacing off for exactly the chats that spend
	// the allowance fastest.
	if budget.Limit > 0 && prompt >= budget.Limit {
		return 0
	}
	want := prompt
	if req.MaxTokens > 0 {
		want += req.MaxTokens
	}
	// The answer's own room is part of the cost, because the allowance is spent by
	// tokens rather than by prompts. One minute is the most a request can need in order
	// to be *sent*, so the want is capped there: beyond it the answer is "wait for a
	// whole window", which is the honest reading of a request that needs most of one.
	if budget.Limit > 0 && want > budget.Limit {
		want = budget.Limit
	}

	if budget.remainingAt(now) >= want {
		return 0
	}
	wait := budget.resetIn(now)
	if wait <= 0 || wait > retryHintCeiling {
		// Beyond the window these limits are defined over, which is not this vendor
		// describing its own minute: the request goes out and the vendor's own 429,
		// with the wait it names, is the answer.
		return 0
	}
	return wait
}
