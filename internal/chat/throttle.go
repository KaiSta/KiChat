package chat

import (
	"context"
	"time"

	"kichat/internal/events"
	"kichat/internal/llm"
)

// The three chat phases a request can wait under. All are reported together with the
// wait, because "waiting" alone does not say whether this is a second or a minute, nor
// whether anything went wrong.
const (
	// throttledPhase is shown while a retry waits out a backend's throttle: the
	// request was sent, refused, and is going out again.
	throttledPhase = "throttled"
	// pacedPhase is shown while a request waits for the backend's own allowance to have
	// room for it, *before* it is sent (internal/llm/pace.go). It is a phase of its own
	// rather than the throttle's because the sentence differs: nothing was refused, so
	// telling the user the backend is throttling this request — and that it is being
	// retried — would be describing something that has not happened.
	pacedPhase = "paced"
	// spreadPhase is shown while a *turn* is spaced against the request rate the backend
	// published: the answer needs more requests than the minute allows, so its rounds go
	// out one per slot rather than in a burst that then sits at the wall
	// (internal/chat/pace.go). It has a sentence of its own for the same reason paced
	// has one: nothing was refused here either, and unlike a paced wait the allowance
	// is not spent — the app is deliberately spreading the turn, and a line that said
	// the minute was used up would be false.
	spreadPhase = "spread"
)

// watchingRetries returns the context the turn's own request is made with, so a
// retry that sleeps is reported as a phase on this chat.
//
// Only the turn's provider call is watched. The other calls a turn makes already
// say what they are doing with a phase of their own — "searching", "compacting" —
// and a listing or a probe is answering no user turn, so it has nothing to explain.
func (e *Engine) watchingRetries(ctx context.Context, chatID string) context.Context {
	return llm.WithRetryWatcher(ctx, retryWatch{engine: e, chatID: chatID})
}

// retryWatch turns the HTTP client's wait back into an event on one chat.
//
// It exists in this package rather than in internal/llm because only the engine
// knows which chat is asking — and the retry lives one layer below the provider,
// in a client shared by every backend. The failure that caused the wait is not
// passed on: it is the vendor's raw envelope, which belongs in the error the turn
// ends with, not in a one-line status.
type retryWatch struct {
	engine *Engine
	chatID string
}

func (w retryWatch) Waiting(wait time.Duration, err error) {
	// A wait with no failure behind it is the client pacing a request rather than
	// retrying one, which is the only difference the user is shown. See
	// llm.RetryWatcher.Waiting: the error is where that distinction already lives, so
	// no second interface is needed to carry it.
	phase := throttledPhase
	if err == nil {
		phase = pacedPhase
	}
	w.engine.emitWaiting(w.chatID, phase, wait)
}

func (w retryWatch) Waited() {
	w.engine.emitStatus(w.chatID, "idle")
}

// emitWaiting publishes the phase and the length of the wait. The wait is part of
// the payload rather than left implicit in the phase, because the number is the
// one fact that makes the wait legible: a panel can show "about 10 s" instead of an
// indefinite something-is-happening.
func (e *Engine) emitWaiting(chatID, phase string, wait time.Duration) {
	if e.Emit == nil {
		return
	}
	e.Emit(events.Status, map[string]any{
		"chatId": chatID,
		"phase":  phase,
		"waitMs": wait.Milliseconds(),
	})
}
