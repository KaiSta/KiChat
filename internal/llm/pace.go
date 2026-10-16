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
// chat feel broken: "is there any of the minute's budget left to spend". Groq's
// token allowance is per minute, and a chat that sends a turn every few seconds
// with a growing context spends it long before the conversation is over — the
// refusal that follows is a 429 that names a wait of seconds, and the turn that
// could have been sent a few seconds later was sent now and lost.
//
// So a request is held back when the backend's own numbers say it cannot be paid
// for, and the wait is shown the way a retry's wait is (see RetryWatcher). Three
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
//   - Only the vendor's numbers are used. The remaining allowance and the reset
//     time are read from the response that carried them (noteRequestBudget), and a
//     response without them teaches nothing: no budget found means no wait, never
//     an assumed one.
//   - A wait is never longer than the window it is waiting for (retryHintCeiling).
//     These limits are defined per minute, so a reset beyond that is not this
//     vendor describing its own minute — and a turn parked for longer than a
//     minute is worth less than the refusal it avoids.
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

// paceRequest holds a request back until the backend's own allowance can pay for it,
// and reports how long it waited.
//
// It returns 0 when nothing was waited for, which is the answer of every backend but
// Groq, of every request with no budget learned yet, and of every request the
// allowance already covers. The wait is published on the request's watcher, so what
// the user sees is the same kind of line a retry shows, with the seconds this
// vendor's own reset header named.
func (p *openAICompatProvider) paceRequest(ctx context.Context, req ChatRequest) time.Duration {
	if p.modelsFlavour != flavourGroq {
		return 0
	}
	// A wait that cannot be shown is not taken: see the file comment.
	watcher := RetryWatcherOf(ctx)
	if watcher == nil {
		return 0
	}
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

	now := time.Now()
	if budget.remainingAt(now) >= want {
		return 0
	}
	wait := budget.resetIn(now)
	if wait <= 0 || wait > retryHintCeiling {
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
