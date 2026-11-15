package chat

import (
	"sync"
	"time"

	"uuid"
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
	// queued holds the messages waiting behind each chat's running turn, oldest first.
	queued map[string][]queuedRequest
}

// QueuedTurn is one message waiting for a chat's running turn to finish, in the form the
// window draws it: the text to show, the id it is deleted by, and the attachments it travels
// with — which the window needs off screen, to keep two waiting messages off one file.
type QueuedTurn struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// AttachmentIDs is never null: the window maps over it, and a nil slice marshals to null.
	AttachmentIDs []string `json:"attachmentIds"`
}

// queuedRequest is one waiting turn with the identity the window deletes it by. The whole
// request is kept, not a rendering of it: what starts when its turn comes is the turn the user
// asked for, model override and attachments included.
type queuedRequest struct {
	id  string
	req TurnRequest
}

// turn converts the entry to the shape the window reads. The attachment slice is a copy, so a
// caller cannot reach into the queue through it.
func (q queuedRequest) turn() QueuedTurn {
	return QueuedTurn{
		ID:            q.id,
		Text:          q.req.Text,
		AttachmentIDs: append([]string{}, q.req.AttachmentIDs...),
	}
}

// NewTurnState builds the shared turn registry.
func NewTurnState() *TurnState {
	return &TurnState{
		active:       map[string]*turnHandle{},
		measurements: map[string]Measurement{},
		queued:       map[string][]queuedRequest{},
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

// accept puts a request where it belongs in one step: it claims the chat for a turn that starts
// now, or — when a turn already holds the chat — appends the request to its queue. It reports
// which of the two happened.
//
// One lock for both answers is the point. Deciding in two steps would let a message be refused
// for a chat that had just gone quiet, or start ahead of a queue, in the gap between them.
func (t *TurnState) accept(req TurnRequest, h *turnHandle) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, busy := t.active[req.ChatID]; busy {
		t.queued[req.ChatID] = append(t.queued[req.ChatID], queuedRequest{
			id:  uuid.New().String(),
			req: req,
		})
		return false
	}
	t.active[req.ChatID] = h
	return true
}

// queuedTurns lists a chat's queue in the order it will be sent. The answer is never null:
// a chat with nothing queued gets an empty list rather than the null a nil slice marshals to.
func (t *TurnState) queuedTurns(chatID string) []QueuedTurn {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]QueuedTurn, 0, len(t.queued[chatID]))
	for _, q := range t.queued[chatID] {
		out = append(out, q.turn())
	}
	return out
}

// dropQueued removes one entry and reports whether it was still there. A click that arrives
// after the entry's turn started is the ordinary false, not an error.
func (t *TurnState) dropQueued(chatID, id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	list := t.queued[chatID]
	for i, q := range list {
		if q.id != id {
			continue
		}
		// Three-index slicing so the entries after this one are copied rather than shifted
		// into a backing array the popped head still points at.
		rest := append(list[:i:i], list[i+1:]...)
		if len(rest) == 0 {
			delete(t.queued, chatID)
		} else {
			t.queued[chatID] = rest
		}
		return true
	}
	return false
}

// head is the oldest queued request, still in the queue. The engine stores its message first and
// takes the entry out afterwards, so a message that could not be stored stays where the user can
// see it and delete it instead of disappearing.
func (t *TurnState) head(chatID string) (queuedRequest, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	list := t.queued[chatID]
	if len(list) == 0 {
		return queuedRequest{}, false
	}
	return list[0], true
}

// clearQueued empties one chat's queue and reports whether anything was in it.
func (t *TurnState) clearQueued(chatID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.queued[chatID]) == 0 {
		return false
	}
	delete(t.queued, chatID)
	return true
}

// clearAllQueued drops every chat's queue. A cancelled turn drains its chat's queue on the
// way out, and a process that is closing must not start the next message.
func (t *TurnState) clearAllQueued() {
	t.mu.Lock()
	t.queued = map[string][]queuedRequest{}
	t.mu.Unlock()
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
