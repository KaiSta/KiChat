// Package trace writes a diagnostic log of what KiChat did, without ever writing
// what it was working on.
//
// The database is encrypted now, which is the right default — and one consequence is
// that a conversation that went wrong leaves nothing a later post-mortem can read:
// the rows are ciphertext, and the sentence that explained the failure was shown once
// in a window that is gone. This log is the readable half. It records the shape of a
// turn — which backend, which model, which tool, how long, how many tokens, which
// kind of failure — and never its content.
//
// **The guarantee is structural, not a redaction pass.** Event has a fixed set of
// fields, every one of them an identifier, a count, a duration, an enum or a public
// name. There is no field that can hold a prompt, a reply, a query, a URL, a file
// path, a chat title or an error string, so there is no place for personal data to
// be written even by mistake. Errors are classified by KindOf into a closed set of
// words; the error's own text is never copied. `TestEventFieldsAreAllowlisted` pins
// the field set and `TestKindOfNeverCarriesText` pins the classification.
//
// A file that cannot be opened is not a reason to fail a turn: the logger keeps its
// path and writes nowhere, which is also what a nil *Logger does. Every method is
// nil-safe because the packages that record events are usable without an app around
// them, which is what the tests do.
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"kichat/internal/llm"
	"kichat/internal/research"
)

const (
	// Name is the file inside the data directory, beside kichat.log and the database.
	Name = "kichat-trace.jsonl"

	// MaxSize is where the file rolls over. Events are a few hundred bytes each and a
	// turn produces a handful, so 2 MiB is tens of thousands of turns — months of use,
	// and small enough that a bug report can attach it whole. One previous file is
	// kept, which is enough for "it happened twice".
	MaxSize int64 = 2 << 20
)

// Event names. They are constants so a typo cannot silently produce a category
// nobody filters for.
const (
	EvAppStart    = "app.start"
	EvAppStop     = "app.stop"
	EvDBOpen      = "db.open"
	EvServerStart = "server.start"
	EvTurnStart   = "turn.start"
	EvTurnEnd     = "turn.end"
	EvLLMRequest  = "llm.request"
	EvLLMResponse = "llm.response"
	EvToolCall    = "tool.call"
	EvSearch      = "search"
)

// Levels, from the smallest set that still lets a reader filter for trouble.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Event is one line of the trace.
//
// Every field is optional except the ones Record stamps (ts, session, event, level).
// Adding a field is a privacy decision, not a formatting one: it has to be a value
// from a closed set or an opaque identifier, never something a user or a model wrote.
type Event struct {
	TS      string `json:"ts"`
	Session string `json:"session"`
	Event   string `json:"event"`
	Level   string `json:"level"`

	// Turn is this process's sequence number for a turn, so the events of one turn
	// can be read together. Chat and Message are the app's own UUIDs.
	Turn    string `json:"turn,omitempty"`
	Chat    string `json:"chat,omitempty"`
	Message string `json:"message,omitempty"`

	// Backend, Model, Tool and Provider are the public names the app already shows
	// in its pickers and settings.
	Backend  string `json:"backend,omitempty"`
	Model    string `json:"model,omitempty"`
	Tool     string `json:"tool,omitempty"`
	Provider string `json:"provider,omitempty"`

	// Phase is a short internal state word such as "locked" or "stream" — and, on a
	// `tool.call` for `read_file`, how that call was shaped: `bare` (a whole-file read),
	// `bounded` (a window the model chose), `outline` (the declaration list) or
	// `continuation` (a window taken from this app's own `offset=N` offer). Four words from
	// a closed set, which is why the field can carry them at all; counting them is how the
	// outline feature is measured rather than guessed at.
	Phase string `json:"phase,omitempty"`
	// Kind is the classified failure, one of the Kind* constants, never an error's text.
	Kind string `json:"kind,omitempty"`
	// Version is the build string, which a bug report needs and nothing personal.
	Version string `json:"version,omitempty"`

	Status int   `json:"status,omitempty"`
	Count  int   `json:"count,omitempty"`
	Tokens int   `json:"tokensIn,omitempty"`
	Out    int   `json:"tokensOut,omitempty"`
	Bytes  int64 `json:"bytes,omitempty"`
	MS     int64 `json:"ms,omitempty"`
	OK     *bool `json:"ok,omitempty"`
}

// Failure kinds. A closed set: KindOf answers one of these and nothing else, which
// is what makes "no error text in the trace" checkable rather than aspirational.
const (
	KindCanceled     = "canceled"
	KindTimeout      = "timeout"
	KindNetwork      = "network"
	KindUnreachable  = "unreachable"
	KindRateLimited  = "rate_limited"
	KindQuota        = "quota"
	KindInvalidKey   = "invalid_key"
	KindBlocked      = "blocked"
	KindTooLarge     = "too_large"
	KindModelMissing = "model_missing"
	KindMalformed    = "malformed_tool_call"
	KindOther        = "other"
)

// Kinds lists every failure kind, so a test can check the set as a whole instead of
// only the words it remembers.
func Kinds() []string {
	return []string{
		KindCanceled, KindTimeout, KindNetwork, KindUnreachable, KindRateLimited,
		KindQuota, KindInvalidKey, KindBlocked, KindTooLarge, KindModelMissing,
		KindMalformed, KindOther,
	}
}

// KindOf classifies an error into one of the Kind* words.
//
// This is the whole of what the trace may know about a failure. The error's text can
// contain a prompt, a model's own sentence, a URL or a provider body that echoes the
// request, so none of it is copied — only the category, which is the part a support
// case actually needs ("it was a quota refusal, not a mistyped key").
func KindOf(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return KindCanceled
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return KindTimeout
	case errors.Is(err, research.ErrQuotaExhausted):
		return KindQuota
	case errors.Is(err, research.ErrRateLimited):
		return KindRateLimited
	case errors.Is(err, research.ErrInvalidKey):
		return KindInvalidKey
	case errors.Is(err, research.ErrBlocked):
		return KindBlocked
	case errors.Is(err, llm.ErrContextExceeded):
		return KindTooLarge
	case errors.Is(err, llm.ErrModelNotFound):
		return KindModelMissing
	case errors.Is(err, llm.ErrUnavailable):
		return KindUnreachable
	case llm.MalformedToolCall(err):
		return KindMalformed
	}
	if _, ok := llm.RequestTooLarge(err); ok {
		return KindTooLarge
	}
	var httpErr *llm.HTTPError
	if errors.As(err, &httpErr) {
		switch {
		case httpErr.StatusCode == 429:
			return KindRateLimited
		case httpErr.StatusCode == 401 || httpErr.StatusCode == 403:
			return KindInvalidKey
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return KindTimeout
		}
		return KindNetwork
	}
	return KindOther
}

// StatusOf returns the HTTP status a classified error carried, or 0. It is the one
// number from an error worth keeping: 429 and 400 need different fixes, and the
// category alone does not always say which arrived.
func StatusOf(err error) int {
	var httpErr *llm.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode
	}
	return 0
}

// Logger appends events to dir/Name. The zero value writes nothing; New is what an
// app uses, and a nil *Logger is safe everywhere for the tests.
type Logger struct {
	mu      sync.Mutex
	file    *os.File
	path    string
	size    int64
	session string
	turns   atomic.Int64
}

// New opens the trace in dir. A directory or file that cannot be opened is not an
// error: the logger still reports the path it wanted, so Settings can show it, and
// every Record is a no-op — losing diagnostics is never worth losing the app.
func New(dir string) *Logger {
	l := &Logger{session: newSession()}
	path := filepath.Join(dir, Name)
	l.path = path
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return l
	}
	// Owner-only. The trace is plaintext beside an encrypted database, so it carries
	// no content and still no reason to be readable by anyone else on the machine.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return l
	}
	l.file = file
	if info, err := file.Stat(); err == nil {
		l.size = info.Size()
	}
	return l
}

// Path is the file the trace is written to, whether or not it could be opened.
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Turn allocates this process's next turn number. It is what groups the events of
// one exchange; a chat UUID alone cannot, because a chat has many turns.
func (l *Logger) Turn() string {
	if l == nil {
		return ""
	}
	return itoa(l.turns.Add(1))
}

// Record stamps and appends one event. A nil logger, an unopened file and a failed
// write are all silent: the caller is in the middle of answering somebody.
func (l *Logger) Record(ev Event) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	if ev.TS == "" {
		ev.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	ev.Session = l.session
	if ev.Level == "" {
		ev.Level = LevelInfo
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if l.size+int64(len(line)) > MaxSize {
		l.rotateLocked()
		if l.file == nil {
			return
		}
	}
	if n, err := l.file.Write(line); err == nil {
		l.size += int64(n)
	}
}

// rotateLocked renames the current file out of the way and starts a new one. One
// previous file is kept; a rename that fails leaves the file in place and the log
// growing, which is a worse log than a rolled one and a better outcome than silence.
func (l *Logger) rotateLocked() {
	_ = l.file.Close()
	l.file = nil
	previous := l.path + ".1"
	// Windows refuses a rename onto an existing name, so the old copy goes first.
	_ = os.Remove(previous)
	_ = os.Rename(l.path, previous)
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	l.file = file
	l.size = 0
}

// Close closes the file. Recording afterwards is a no-op.
func (l *Logger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

// newSession names this process in the trace, so two runs can be told apart in one
// file without needing a hostname or a user name.
func newSession() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return itoa(time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// itoa is a tiny local decimal conversion so the package needs no formatting import
// for the two numbers it writes.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}
