package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kichat/internal/llm"
	"kichat/internal/research"
)

// TestEventFieldsAreAllowlisted is the privacy guarantee in one assertion: the set of
// keys an event can produce is closed, and none of them can hold free text. Adding a
// field means adding it here on purpose and justifying it as an identifier, a count,
// a duration, an enum or a public name — which is the review this feature needs.
func TestEventFieldsAreAllowlisted(t *testing.T) {
	yes := true
	all := Event{
		TS: "2026-10-25T10:00:00Z", Session: "abcd1234", Event: EvToolCall, Level: LevelError,
		Turn: "7", Chat: "chat-uuid", Message: "message-uuid",
		Backend: "groq", Model: "llama-3.3-70b", Tool: "search_web", Provider: "brave",
		Phase: "stream", Kind: KindRateLimited, Version: "0.6.0",
		Status: 429, Count: 3, Tokens: 1200, Out: 340, Bytes: 900, MS: 812, OK: &yes,
	}
	raw, err := json.Marshal(all)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	allowed := map[string]bool{
		"ts": true, "session": true, "event": true, "level": true,
		"turn": true, "chat": true, "message": true,
		"backend": true, "model": true, "tool": true, "provider": true,
		"phase": true, "kind": true, "version": true,
		"status": true, "count": true, "tokensIn": true, "tokensOut": true,
		"bytes": true, "ms": true, "ok": true,
	}
	for name := range fields {
		if !allowed[name] {
			t.Errorf("event field %q is not allowlisted — it is either undocumented or a channel for content", name)
		}
	}
	if len(fields) != len(allowed) {
		t.Errorf("got %d fields, allowlist has %d; a field was added or dropped without updating this test", len(fields), len(allowed))
	}
}

// TestKindOfNeverCarriesText is the other half: an error's sentence can contain a
// prompt, a URL or a provider body, so only the category may travel.
func TestKindOfNeverCarriesText(t *testing.T) {
	secret := "Mein Name ist Erika Mustermann und ich wohne in der Hauptstraße 5"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"cancelled", context.Canceled, KindCanceled},
		{"deadline", context.DeadlineExceeded, KindTimeout},
		{"quota", fmt.Errorf("brave: %w", research.ErrQuotaExhausted), KindQuota},
		{"rate limited", fmt.Errorf("brave: %w", research.ErrRateLimited), KindRateLimited},
		{"bad key", fmt.Errorf("brave: %w", research.ErrInvalidKey), KindInvalidKey},
		{"blocked", fmt.Errorf("duckduckgo: %w", research.ErrBlocked), KindBlocked},
		{"context exceeded", llm.ErrContextExceeded, KindTooLarge},
		{"model missing", llm.ErrModelNotFound, KindModelMissing},
		{"unavailable", llm.ErrUnavailable, KindUnreachable},
		{"http 429", &llm.HTTPError{StatusCode: 429, Body: secret}, KindRateLimited},
		{"http 401", &llm.HTTPError{StatusCode: 401, Body: secret}, KindInvalidKey},
		{"http 500", &llm.HTTPError{StatusCode: 500, Body: secret}, KindOther},
		{"network timeout", &net.DNSError{IsTimeout: true, Name: secret}, KindTimeout},
		{"network", &net.OpError{Op: "dial", Err: errors.New(secret)}, KindNetwork},
		{"unknown", errors.New(secret), KindOther},
	}
	known := map[string]bool{}
	for _, kind := range Kinds() {
		known[kind] = true
	}
	for _, tc := range cases {
		got := KindOf(tc.err)
		if got != tc.want {
			t.Errorf("%s: KindOf = %q, want %q", tc.name, got, tc.want)
		}
		if got != "" && !known[got] {
			t.Errorf("%s: %q is not one of the documented kinds", tc.name, got)
		}
		if strings.Contains(got, "Erika") || strings.Contains(got, "Hauptstraße") {
			t.Fatalf("%s: error text leaked into the kind: %q", tc.name, got)
		}
	}
}

// TestTraceFileIsOwnerOnly keeps the plaintext file beside the encrypted database out
// of other accounts on the machine.
func TestTraceFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	defer l.Close()
	l.Record(Event{Event: EvAppStart, Level: LevelInfo})

	info, err := os.Stat(filepath.Join(dir, Name))
	if err != nil {
		t.Fatalf("trace file missing: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
}

// TestRecordWritesOneJSONEventPerLine is what makes the file greppable and jq-able.
func TestRecordWritesOneJSONEventPerLine(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	turn := l.Turn()
	l.Record(Event{Event: EvTurnStart, Turn: turn, Chat: "chat-1", Backend: "fake", Model: "m"})
	l.Record(Event{Event: EvTurnEnd, Turn: turn, Chat: "chat-1", Phase: "complete"})
	l.Close()

	raw, err := os.ReadFile(filepath.Join(dir, Name))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), raw)
	}
	var first, second Event
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first line is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("second line is not JSON: %v", err)
	}
	if first.Event != EvTurnStart || second.Event != EvTurnEnd {
		t.Errorf("events out of order: %q, %q", first.Event, second.Event)
	}
	if first.Session == "" || first.Session != second.Session {
		t.Errorf("session ids differ or are empty: %q / %q", first.Session, second.Session)
	}
	if first.TS == "" {
		t.Error("ts was not stamped")
	}
	if first.Level != LevelInfo {
		t.Errorf("level = %q, want the info default", first.Level)
	}
}

// TestRecordFromManyGoroutinesKeepsWholeLines covers the real case: turns, tool calls
// and managed servers all record at once, and an interleaved write would produce a file
// no parser can read.
func TestRecordFromManyGoroutinesKeepsWholeLines(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				l.Record(Event{Event: EvToolCall, Tool: "search_web", Count: i, MS: int64(j)})
			}
		}(i)
	}
	wg.Wait()
	l.Close()

	raw, err := os.ReadFile(filepath.Join(dir, Name))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 200 {
		t.Fatalf("got %d lines, want 200", len(lines))
	}
	for _, line := range lines {
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("interleaved line: %v (%q)", err, line)
		}
	}
}

// TestRotationKeepsOnePreviousFile pins the size bound: an unbounded plaintext log is
// not a diagnostic aid, it is a file nobody deletes.
func TestRotationKeepsOnePreviousFile(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	// A payload big enough that a few hundred events pass MaxSize.
	pad := strings.Repeat("x", 1024)
	for i := 0; i < 3000; i++ {
		l.Record(Event{Event: EvToolCall, Tool: pad})
	}
	l.Close()

	current, err := os.Stat(filepath.Join(dir, Name))
	if err != nil {
		t.Fatalf("current file: %v", err)
	}
	if current.Size() > MaxSize {
		t.Errorf("current file is %d bytes, over the %d bound", current.Size(), MaxSize)
	}
	if _, err := os.Stat(filepath.Join(dir, Name+".1")); err != nil {
		t.Errorf("no rolled file: %v", err)
	}
}

// TestNilLoggerIsSafe covers every package that records without an app around it.
func TestNilLoggerIsSafe(t *testing.T) {
	var l *Logger
	l.Record(Event{Event: EvAppStart})
	if l.Turn() != "" {
		t.Error("a nil logger has no turn numbering")
	}
	if l.Path() != "" {
		t.Error("a nil logger has no path")
	}
	l.Close()
}

// TestUnwritableDirectoryStillReportsPath is the degraded case: Settings must be able
// to show where a trace would be even when it cannot be written.
func TestUnwritableDirectoryStillReportsPath(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file where a directory should be makes MkdirAll fail.
	l := New(filepath.Join(blocked, "sub"))
	defer l.Close()
	if l.Path() == "" {
		t.Error("path should be reported even when the file cannot be opened")
	}
	l.Record(Event{Event: EvAppStart}) // must not panic
}
