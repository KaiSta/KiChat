package chat

import (
	"context"
	"time"

	"kichat/internal/events"
	"kichat/internal/llm"
)

// throttledPhase is the chat phase shown while a request waits out a backend's
// throttle. It is reported together with the wait the backend named, because
// "waiting" alone does not say whether this is a second or a minute.
const throttledPhase = "throttled"

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

func (w retryWatch) Waiting(wait time.Duration, _ error) {
	w.engine.emitWaiting(w.chatID, wait)
}

func (w retryWatch) Waited() {
	w.engine.emitStatus(w.chatID, "idle")
}

// emitWaiting publishes the phase and the length of the wait. The wait is part of
// the payload rather than left implicit in the phase, because the number is the
// one fact that makes the wait legible: a panel can show "about 10 s" instead of an
// indefinite something-is-happening.
func (e *Engine) emitWaiting(chatID string, wait time.Duration) {
	if e.Emit == nil {
		return
	}
	e.Emit(events.Status, map[string]any{
		"chatId": chatID,
		"phase":  throttledPhase,
		"waitMs": wait.Milliseconds(),
	})
}
