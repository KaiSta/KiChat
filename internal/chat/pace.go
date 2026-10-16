package chat

import (
	"context"
	"time"

	"kichat/internal/llm"
)

// A turn that makes more requests than the minute allows.
//
// Groq's minute is measured two ways and only one of them is published (GROQ.md §6.4): the
// token allowance arrives on every response, while the request rate — 30 per minute for the
// models it was measured against — is only ever learned from a refusal, and what the app can
// count is its own responses. internal/llm spends that count one request at a time: a request
// that finds the trailing minute full waits for the oldest of those requests to leave the
// window, which is correct and is what keeps a slow chat working.
//
// It is not a plan for a *turn*, and a tool-heavy turn is where that shows. Ten rounds against
// a minute with two slots left send two requests at once and then wait at the wall for each of
// the other eight — and the wall is where this app's arithmetic is weakest, because its count
// is a lower bound on the account's spend (a second client, or this app's own listings and
// probes, are spent too and cannot be counted) while the vendor refuses on its own. So the turn
// reads the allowance *before its first request* and, when it cannot fit, spaces its rounds at
// the rate the allowance grants: the burst never happens, no request is sent to sit at the
// limit, and every wait is known before the turn starts instead of being discovered by the
// request that hits it.
//
// Three boundaries, all deliberate:
//
//   - Nothing is spaced when the turn fits, or when no rate has been stated. Inside the
//     minute's room the earliest request is the best one — the window rolls continuously, so
//     taking a slot now cannot cost a later one — and a turn that fits therefore behaves
//     exactly as it did before this file existed.
//   - The plan is a schedule for the requests a turn *may* make (the tool-round cap), not a
//     reservation: a turn that uses fewer rounds leaves the later slots unspent, and one that
//     needs none of them pays nothing for their having been planned.
//   - The plan is read once, at the start, and it is a reading rather than a promise. A
//     request made outside the turn, or a retry the shared client performs, changes the count
//     under it — so internal/llm's per-request pacing stays as it is, behind the plan, and
//     remains the guard that decides whether a request may go at all.

// turnPacing is one turn's plan against a provider's request allowance: the moment each of its
// requests may be sent, read once before the first of them and consulted before each one.
type turnPacing struct {
	// at holds the planned moment of each of the turn's requests, oldest first. It is empty
	// when the turn is not spaced at all, which is the ordinary case.
	at []time.Time
}

// planTurnRequests reads the backend's request allowance and plans the turn's rounds against
// it. rounds is the most requests the turn can make, which is what the tool loop's iteration
// cap bounds, so the plan covers the worst case.
//
// A provider that cannot state an allowance is not planned for, and neither is one that has
// never been answered by or refused for this model: nothing is invented from a window or a
// guess, so the plan exists only where the vendor's own numbers do.
func planTurnRequests(provider llm.Provider, model string, rounds int) turnPacing {
	if rounds < 1 {
		return turnPacing{}
	}
	allowance, ok := provider.(llm.RequestAllowance)
	if !ok {
		return turnPacing{}
	}
	waits := allowance.RequestAllowance(model).Spread(rounds)
	if len(waits) == 0 {
		return turnPacing{}
	}
	now := time.Now()
	at := make([]time.Time, len(waits))
	for i, wait := range waits {
		at[i] = now.Add(wait)
	}
	return turnPacing{at: at}
}

// waitFor is how long the request at index (0-based) still has to wait, and 0 when it may go
// now — which is also the answer for a turn that was never planned and for every index past
// the plan.
func (p turnPacing) waitFor(index int, now time.Time) time.Duration {
	if index < 0 || index >= len(p.at) {
		return 0
	}
	if wait := p.at[index].Sub(now); wait > 0 {
		return wait
	}
	return 0
}

// holdRound waits out a planned round and reports whether the turn may go on; a Stop during
// the wait ends it, exactly as a Stop during a retry's wait does.
//
// The wait is published the way every other wait in a turn is — a phase carrying the seconds —
// and it has a sentence of its own for the reason the pacing phase has one of its own: nothing
// was refused *and* nothing is spent, the app is deliberately spreading this answer over the
// minute, and a line that said the allowance was spent would be describing an event that has
// not happened (see throttle.go).
func (e *Engine) holdRound(ctx context.Context, chatID string, wait time.Duration) bool {
	if wait <= 0 {
		return true
	}
	e.emitWaiting(chatID, spreadPhase, wait)
	defer e.emitStatus(chatID, "idle")

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
