package chat

import (
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/uimsg"
)

// truncationEngine builds an engine whose rounds are scripted, with the round budget a test
// needs: a continuation is a round, so a test about three attempts needs four of them.
func truncationEngine(t *testing.T, rounds int, replies [][]llm.StreamEvent) (*Engine, *fakeProvider, *eventLog) {
	t.Helper()
	st := newTestStore(t)
	makeChat(t, st)

	provider := &fakeProvider{
		model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
		replies: replies,
	}
	registry := llm.NewRegistry(nil)
	registry.Register(provider)

	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = rounds

	log := &eventLog{}
	engine := NewEngine(st, registry, nil, func() config.Settings { return settings }, log.emit)
	return engine, provider, log
}

// requestText is everything the model was sent on one round, so a test can ask what a round
// carried without caring which message it was appended to.
func requestText(req llm.ChatRequest) string {
	var sb strings.Builder
	for _, m := range req.Messages {
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}

// warningCodes returns the codes of the warnings a turn emitted.
func warningCodes(log *eventLog) []string {
	var codes []string
	for _, ev := range log.snapshot() {
		if ev.name != events.Warning {
			continue
		}
		text, _ := ev.payload["warning"].(string)
		if message, ok := uimsg.Parse(text); ok {
			codes = append(codes, message.Code)
		}
	}
	return codes
}

// TestATruncatedReplyIsContinued is the whole point: a reply the output limit cut short is asked
// to carry on instead of ending the turn on half a sentence. The second round continues the text
// rather than restating it — the first request carries no note, the second carries the note and
// the text that was already written.
func TestATruncatedReplyIsContinued(t *testing.T) {
	engine, provider, log := truncationEngine(t, 5, [][]llm.StreamEvent{
		{
			{Type: llm.EventDelta, Text: "Die Antwort ist "},
			{Type: llm.EventDone, Stats: &llm.GenerationStats{StopReason: "length"}},
		},
		{
			{Type: llm.EventDelta, Text: "lang geworden."},
			{Type: llm.EventDone, Stats: &llm.GenerationStats{StopReason: "stop"}},
		},
	})

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Erzähl was."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	if n := provider.requestCount(); n != 2 {
		t.Fatalf("requests = %d, want a second round asking for the rest", n)
	}
	if first := requestText(provider.requestAt(0)); strings.Contains(first, "<output_limit>") {
		t.Error("the first request carried a continuation note for a reply that was not cut off yet")
	}
	second := provider.requestAt(1)
	if !strings.Contains(requestText(second), "<output_limit>") {
		t.Errorf("the continuation round carries no note:\n%s", requestText(second))
	}
	// The text already written is in the history, which is what makes the retry cost the
	// remainder rather than the whole answer.
	if !strings.Contains(requestText(second), "Die Antwort ist ") {
		t.Error("the cut-off text is not in the continuation request")
	}
	// Its own user turn, after the reply: a request ending on an assistant message is a shape
	// some vendors refuse, and the strict templates this app works around are the local half of
	// that. The two messages are still one reply in the transcript.
	last := len(second.Messages) - 1
	if second.Messages[last].Role != llm.RoleUser ||
		!strings.Contains(second.Messages[last].Content, "Setze genau an der Stelle fort") {
		t.Errorf("the note is not the user turn after the reply: %+v", second.Messages[last].Role)
	}
	if second.Messages[last-1].Role != llm.RoleAssistant ||
		!strings.Contains(second.Messages[last-1].Content, "Die Antwort ist ") {
		t.Error("the cut-off reply is not the message in front of the note")
	}

	messages, err := engine.Store.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	var assistants []store.Message
	for _, m := range messages {
		if m.Role == string(llm.RoleAssistant) {
			assistants = append(assistants, m)
		}
	}
	if len(assistants) != 2 {
		t.Fatalf("assistant rows = %d, want the cut-off reply and its continuation", len(assistants))
	}
	if assistants[0].StopReason != "length" || assistants[1].StopReason != "stop" {
		t.Errorf("stop reasons = %q, %q; want length then stop",
			assistants[0].StopReason, assistants[1].StopReason)
	}
	// A continued answer is a finished one, so nothing is reported as a problem.
	for _, code := range warningCodes(log) {
		if code == "chat.outputLimit" {
			t.Error("a reply that was continued reported the limit as a failure")
		}
	}
}

// TestATruncatedReplyStopsAfterThreeAttempts is the other end: the retry is bounded, and when it
// runs out the user is told what happened and what the setting is, rather than being left with a
// sentence that stops.
func TestATruncatedReplyStopsAfterThreeAttempts(t *testing.T) {
	cut := []llm.StreamEvent{
		{Type: llm.EventDelta, Text: "immer weiter "},
		{Type: llm.EventDone, Stats: &llm.GenerationStats{StopReason: "length"}},
	}
	engine, provider, log := truncationEngine(t, 10, [][]llm.StreamEvent{cut, cut, cut, cut, cut})

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Erzähl was."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	if n := provider.requestCount(); n != 1+maxTruncatedRetries {
		t.Errorf("requests = %d, want one round and %d continuations", n, maxTruncatedRetries)
	}
	var reported int
	for _, code := range warningCodes(log) {
		if code == "chat.outputLimit" {
			reported++
		}
	}
	if reported != 1 {
		t.Errorf("the output limit was reported %d times, want once when the attempts ran out", reported)
	}
}

// TestATruncatedToolRoundIsNotContinued: a round that asked for tools has something to do — the
// calls are executed and the model reads their results — so there is nothing to continue, and a
// note asking for more prose would be an instruction the round does not want.
func TestATruncatedToolRoundIsNotContinued(t *testing.T) {
	engine, provider, _ := truncationEngine(t, 5, [][]llm.StreamEvent{
		{
			{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{
				ID: "call-1", Name: "read_file", Arguments: `{"path":"a.go"}`,
			}},
			{Type: llm.EventDelta, Text: "Ich lese "},
			{Type: llm.EventDone, Stats: &llm.GenerationStats{StopReason: "length"}},
		},
		{
			{Type: llm.EventDelta, Text: "Fertig."},
			{Type: llm.EventDone, Stats: &llm.GenerationStats{StopReason: "stop"}},
		},
	})

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Lies die Datei."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}
	if n := provider.requestCount(); n != 2 {
		t.Fatalf("requests = %d, want the tool round and the answer after it", n)
	}
	if text := requestText(provider.requestAt(1)); strings.Contains(text, "<output_limit>") {
		t.Errorf("a tool round was continued as if it were prose:\n%s", text)
	}
}

// TestAReasoningOnlyTruncationIsReported: a thinking model can spend the whole output budget
// before writing a word, and there is nothing to continue from. Asking it to be briefer would be
// advice it cannot use, so the cause is named and the rest is left to the setting.
func TestAReasoningOnlyTruncationIsReported(t *testing.T) {
	engine, provider, log := truncationEngine(t, 5, [][]llm.StreamEvent{
		{
			{Type: llm.EventReasoning, Text: "Ich denke sehr lange nach."},
			{Type: llm.EventDone, Stats: &llm.GenerationStats{StopReason: "length"}},
		},
	})

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Denk nach."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}
	if n := provider.requestCount(); n != 1 {
		t.Errorf("requests = %d, want no continuation of an answer that never started", n)
	}
	found := false
	for _, code := range warningCodes(log) {
		if code == "chat.outputLimit" {
			found = true
		}
	}
	if !found {
		t.Error("a reply whose whole budget went to reasoning was not reported")
	}
}
