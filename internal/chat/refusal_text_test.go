package chat

import (
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/llm"
	"kichat/internal/tools"
	"kichat/internal/uimsg"
)

// turnErrorText returns what the UI was told a failed turn was, or "" when the turn
// reported success. A turn failure is an event, not Send's return value — Send only
// reports what it could not even start — so this is the string a user actually reads.
func turnErrorText(t *testing.T, engine *Engine, log *eventLog, chatID string) string {
	t.Helper()
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning(chatID) }) {
		t.Fatal("the turn did not finish")
	}
	for _, ev := range log.snapshot() {
		if ev.name == events.Error {
			text, _ := ev.payload["error"].(string)
			return text
		}
	}
	return ""
}

// TestAMalformedToolCallIsExplainedRatherThanQuoted pins the sentence a user reads when
// the model names a tool the request never declared. Groq's own words name a tool the app
// never sent (`scr scrape_url`), so quoting them reads as a bug in the app's tool list.
// What is shown instead says who produced the name and what helps next.
func TestAMalformedToolCallIsExplainedRatherThanQuoted(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072,
			State: llm.StateLoaded, Capabilities: []string{llm.CapTools}},
		refusals: []error{&llm.HTTPError{
			StatusCode: 400,
			Body: `{"error":{"message":"tool call validation failed: attempted to call tool ` +
				`'scr scrape_url' which was not in request.tools","type":"invalid_request_error"}}`,
		}},
	}
	engine, log := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Lies den Bericht."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	text := turnErrorText(t, engine, log, "chat-1")
	if text == "" {
		t.Fatal("a refused request was reported to the user as a finished turn")
	}
	if strings.Contains(text, "request.tools") || strings.Contains(text, "scr scrape_url") {
		t.Errorf("the vendor's validation sentence reached the user verbatim: %q", text)
	}
	message, ok := uimsg.Parse(text)
	if !ok {
		t.Fatalf("the refusal is not a message the window can translate: %q", text)
	}
	// The sentence itself lives in the dictionaries now (err.chat.malformedToolCall),
	// and i18n_test.go fails if any language lacks it. What a Go test can pin is
	// which explanation was chosen for this refusal.
	if message.Code != "chat.malformedToolCall" {
		t.Errorf("code = %q, want chat.malformedToolCall", message.Code)
	}
}

// TestADifferentRefusalIsStillQuoted keeps the replacement narrow: only the malformed
// tool call is explained in the app's own words. Anything else is the backend's answer,
// and inventing an explanation for it would hide the cause.
func TestADifferentRefusalIsStillQuoted(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	provider := &fakeProvider{
		model:    llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		refusals: []error{&llm.HTTPError{StatusCode: 400, Body: `{"error":{"message":"bad api key"}}`}},
	}
	engine, log := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Hallo"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	text := turnErrorText(t, engine, log, "chat-1")
	if text == "" {
		t.Fatal("a refused request was reported to the user as a finished turn")
	}
	if !strings.Contains(text, "bad api key") {
		t.Errorf("an unrelated refusal lost its cause: %q", text)
	}
	if strings.Contains(text, "Tool-Aufruf") {
		t.Errorf("an unrelated refusal was explained as a malformed tool call: %q", text)
	}
}

// TestTheTextProtocolListsOnlyOfferedTools is the prompt half of the same bug: a model
// without native tool calling is told which tools exist, and that list is the request's
// own — not the service's whole catalogue. A tool the engine withheld (here `search_web`,
// with no search provider configured) must not be named anywhere in the prompt, the
// sample call included, because the only answer to such a call is an error the model
// cannot act on.
func TestTheTextProtocolListsOnlyOfferedTools(t *testing.T) {
	st := newTestStore(t)
	chat := makeChat(t, st)

	provider := &fakeProvider{
		// No tool capability: the text protocol is what this turn uses.
		model:   llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "Antwort"}, {Type: llm.EventDone}}},
	}

	settings := config.Default()
	settings.AutoTitle = false
	registry := llm.NewRegistry(nil)
	registry.Register(provider)
	// No search chain, so `search_web` is filtered out of the offered definitions.
	engine := NewEngine(st, registry, tools.New(st, nil, nil),
		func() config.Settings { return settings }, (&eventLog{}).emit)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Hallo"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	system := provider.lastRequest().Messages[0].Content
	if !strings.Contains(system, tools.TextProtocolHint) {
		t.Fatalf("the text protocol hint is missing, so this test proves nothing:\n%s", system)
	}
	if strings.Contains(system, tools.ToolSearchWeb) {
		t.Errorf("the prompt names %q although no search provider is configured:\n%s",
			tools.ToolSearchWeb, system)
	}
	offered := engine.toolDefinitions(t.Context(), &chat, provider.model, settings)
	list := tools.TextProtocolTools(offered)
	if list == "" {
		t.Skip("no tools are offered in this configuration, so there is no list to compare")
	}
	if !strings.Contains(system, list) {
		t.Errorf("the prompt does not carry the offered tool list\nwant: %s\ngot:\n%s", list, system)
	}
}
