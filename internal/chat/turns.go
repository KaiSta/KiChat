package chat

import (
	"sync"
	"time"
)

// TurnState is the part of an engine that has to outlive the Engine value itself.
//
// A generation runs in its own goroutine and holds the Engine it was started on.
// The app rebuilds its services whenever settings are saved (a theme or language
// switch saves settings, too) or a managed server comes up, and that rebuild used
// to install a *new* Engine. The reply kept streaming from the old one while the
// new one — the only one the bound methods could reach — knew nothing about it:
// Stop did nothing, the row reported itself idle, and a second message started a
// second generation beside the first.
//
// Everything that answers "is this chat busy" and "what did the backend say the
// prompt cost" therefore lives here, in one value shared by every Engine the app
// builds, and each turn claims its chat in it. A claim is what Stop cancels and
// what a second turn is refused by.
type TurnState struct {
	mu           sync.Mutex
	active       map[string]*turnHandle
	measurements map[string]Measurement
}

// NewTurnState builds the shared turn registry.
func NewTurnState() *TurnState {
	return &TurnState{
		active:       map[string]*turnHandle{},
		measurements: map[string]Measurement{},
	}
}

// claim reserves a chat for one turn and reports whether it succeeded. The check
// and the reservation happen under one lock on purpose: reading, deciding and
// writing in separate steps would let two callers win the same chat.
func (t *TurnState) claim(chatID string, h *turnHandle) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, busy := t.active[chatID]; busy {
		return false
	}
	t.active[chatID] = h
	return true
}

// release gives a chat back, but only to the turn that still holds it. A turn whose
// engine was replaced can otherwise outlive its claim and remove the one a newer
// turn has since made.
func (t *TurnState) release(chatID string, h *turnHandle) {
	t.mu.Lock()
	if t.active[chatID] == h {
		delete(t.active, chatID)
	}
	t.mu.Unlock()
}

// handle returns the turn currently holding a chat, if any.
func (t *TurnState) handle(chatID string) (*turnHandle, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h, ok := t.active[chatID]
	return h, ok
}

// running reports whether any turn holds the chat.
func (t *TurnState) running(chatID string) bool {
	_, ok := t.handle(chatID)
	return ok
}

// handles snapshots every claimed turn, so a caller can cancel them all without
// holding the lock while it does.
func (t *TurnState) handles() []*turnHandle {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*turnHandle, 0, len(t.active))
	for _, h := range t.active {
		out = append(out, h)
	}
	return out
}

// chats lists the chats with a turn in flight, for the UI's running indicator.
func (t *TurnState) chats() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.active))
	for id := range t.active {
		out = append(out, id)
	}
	return out
}

// waitIdle waits until no chat is claimed, or the timeout expires. The report says
// whether it really became idle, because a caller about to close the database has
// to know whether a turn may still write to it.
func (t *TurnState) waitIdle(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		t.mu.Lock()
		idle := len(t.active) == 0
		t.mu.Unlock()
		if idle {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// measurement returns the last recorded prompt size for a chat.
func (t *TurnState) measurement(chatID string) (Measurement, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.measurements[chatID]
	return m, ok
}

func (t *TurnState) setMeasurement(chatID string, m Measurement) {
	t.mu.Lock()
	t.measurements[chatID] = m
	t.mu.Unlock()
}

func (t *TurnState) invalidateMeasurement(chatID string) {
	t.mu.Lock()
	delete(t.measurements, chatID)
	t.mu.Unlock()
}
