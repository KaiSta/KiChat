package chat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kichat/internal/llm"
	"kichat/internal/tools"
	"kichat/internal/trace"
)

// TestTurnTraceKeepsTheShapeAndNotTheText is the end-to-end privacy check: a turn
// whose backend refuses with a sentence that quotes the user must leave a record of
// what happened and none of what was said.
func TestTurnTraceKeepsTheShapeAndNotTheText(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	const secret = "Erika Mustermann, Hauptstraße 5"
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		// A provider body that echoes the request is exactly the case the trace must
		// not copy; it is the reason the failure is classified instead of quoted.
		refusals: []error{errors.New("400 bad request: " + secret)},
	}
	engine, _ := newTestEngine(t, st, provider)
	dir := t.TempDir()
	engine.Trace = trace.New(dir)
	t.Cleanup(engine.Trace.Close)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: secret}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("turn did not finish")
	}
	engine.Trace.Close()

	raw, err := os.ReadFile(filepath.Join(dir, trace.Name))
	if err != nil {
		t.Fatalf("trace file: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "Erika") || strings.Contains(text, "Hauptstraße") {
		t.Fatalf("user content reached the trace:\n%s", text)
	}

	var events []trace.Event
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		var ev trace.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line is not an event: %v (%q)", err, line)
		}
		events = append(events, ev)
	}
	turnID := ""
	seenStart, seenEnd := false, false
	for _, ev := range events {
		switch ev.Event {
		case trace.EvTurnStart:
			seenStart = true
			turnID = ev.Turn
			if ev.Backend != "fake" || ev.Model != "fake-model" {
				t.Errorf("turn.start names %q/%q, want the chat's backend and model", ev.Backend, ev.Model)
			}
		case trace.EvLLMRequest:
			if ev.Turn != turnID {
				t.Errorf("llm.request turn %q does not match the turn %q", ev.Turn, turnID)
			}
		case trace.EvLLMResponse:
			if ev.OK == nil || *ev.OK {
				t.Error("a refused request was recorded as ok")
			}
			if ev.Kind != trace.KindOther {
				t.Errorf("kind = %q, want %q", ev.Kind, trace.KindOther)
			}
		case trace.EvTurnEnd:
			seenEnd = true
			if ev.Chat != "chat-1" || ev.Turn != turnID {
				t.Errorf("turn.end = %+v, want it to close the turn that started", ev)
			}
			if ev.Phase != "failed" || ev.Kind != trace.KindOther {
				t.Errorf("turn.end phase/kind = %q/%q, want failed/other", ev.Phase, ev.Kind)
			}
		}
	}
	if !seenStart || !seenEnd {
		t.Fatalf("turn was not bracketed in the trace (start=%v end=%v):\n%s", seenStart, seenEnd, text)
	}
}

// TestTheShapeOfAReadReachesTheTrace is what makes the question answerable after real use:
// every `read_file` call leaves one word in the trace — bare, bounded, outline or
// continuation — so a week of chats can be counted instead of argued about.
func TestTheShapeOfAReadReachesTheTrace(t *testing.T) {
	folder := t.TempDir()
	name := sourceFile(t, folder, "big.go", 60)
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: readFileCall(name, `, "outline": true`)}},
			{{Type: llm.EventDelta, Text: "Fertig."}, {Type: llm.EventDone}},
		},
	}
	digester := newDigester([]llm.StreamEvent{{Type: llm.EventDelta, Text: "sollte nicht laufen"}})
	engine, _ := codingDigestEngine(t, folder, digestSettings(), provider, digester)
	dir := t.TempDir()
	engine.Trace = trace.New(dir)
	t.Cleanup(engine.Trace.Close)
	runTurn(t, engine, "project-1", "Gib mir den Umriss von big.go")
	engine.Trace.Close()

	raw, err := os.ReadFile(filepath.Join(dir, trace.Name))
	if err != nil {
		t.Fatalf("trace file: %v", err)
	}
	var shapes []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var ev trace.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line is not an event: %v (%q)", err, line)
		}
		if ev.Event == trace.EvToolCall && ev.Tool == tools.ToolReadFile {
			shapes = append(shapes, ev.Phase)
		}
	}
	if len(shapes) != 1 || shapes[0] != tools.ReadShapeOutline {
		t.Errorf("read_file phases = %v, want one %q", shapes, tools.ReadShapeOutline)
	}
}

// TestNilTraceDoesNotBreakATurn keeps the engine usable in the tests and in any
// future caller that builds one without an app.
func TestNilTraceDoesNotBreakATurn(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)
	provider := &fakeProvider{
		model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "Antwort"}}},
	}
	engine, _ := newTestEngine(t, st, provider)
	if engine.Trace != nil {
		t.Fatal("this test is about the nil case")
	}
	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Hallo"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("turn did not finish")
	}
}
