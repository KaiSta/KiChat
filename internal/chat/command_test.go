package chat

// A project's terminal, driven through a whole turn: the gate, the execution and what the
// model reads next.
//
// The tools package tests the command in isolation and the root tests test the confirmation
// in isolation. Neither answers the question that matters most for the one tool that can do
// anything: does a *turn* ask before it runs, and does the model afterwards see what the
// command actually did? That path goes through the engine's tool gate, the confirmation
// carried on the context, the execution and the persisted result, and it is the path a user
// meets.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/tools"
	"kichat/internal/workspace"
)

const chatCommandID = "chat-project-command"

// writeFileCommand writes one file into the working directory, which is the one observable a
// command has that the test can check without parsing the result text: either the file is
// there or the command did not run.
func writeFileCommand() string {
	if runtime.GOOS == "windows" {
		return "echo hi> from-the-command.txt"
	}
	return "echo hi > from-the-command.txt"
}

// recordingConfirm is a person at the window: it counts the questions and answers however
// the test says.
type recordingConfirm struct {
	asked  int
	line   string
	answer error
}

func (c *recordingConfirm) confirm(_ context.Context, req *tools.ApprovalRequest) error {
	c.asked++
	c.line = req.Command.Line
	return c.answer
}

// commandEngine is a project chat on a real folder, with a scripted model whose first reply
// asks for a command and whose second is the answer after reading its result.
//
// The command it asks for is the caller's, because the tests differ in what they want to
// watch happen; the log comes back because two of them assert on the events the window
// receives rather than on the records the database keeps.
func commandEngine(t *testing.T, conf *recordingConfirm, line string) (*Engine, *fakeProvider, store.Chat, string, *eventLog) {
	t.Helper()
	folder := t.TempDir()
	// The package caches a guidance file by path, size and modification time, and this
	// folder is new each run; clearing it keeps a previous test's entry from answering.
	workspace.ForgetAgentDocs()

	st := newTestStore(t)
	project := store.Chat{
		ID: chatCommandID, Title: "Projekt", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "fake", Model: "fake-model",
		SystemPrompt: "Verfügbare Tools: {{tool_list}}",
		Temperature:  0.7, TopP: 0.9, AutoCompact: false,
		Mode: store.ModeCoding, Workspace: folder,
	}
	if err := st.CreateChat(project); err != nil {
		t.Fatalf("create chat: %v", err)
	}

	provider := &fakeProvider{
		model: llm.ModelInfo{
			ID: "fake-model", Backend: "fake", ContextLimit: 8192,
			State: llm.StateLoaded, Capabilities: []string{llm.CapTools},
		},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{
				ID: "call-1", Name: tools.ToolRunCommand,
				Arguments: `{"command":` + quoteJSON(line) + `,"timeout_seconds":30}`,
			}}},
			{{Type: llm.EventDelta, Text: "Die Datei steht."}, {Type: llm.EventDone}},
		},
	}
	registry := llm.NewRegistry(nil)
	registry.Register(provider)

	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = 3
	service := &tools.Service{Store: st}
	if conf != nil {
		service.Confirm = conf.confirm
	}
	log := &eventLog{}
	engine := NewEngine(st, registry, service, func() config.Settings { return settings }, log.emit)
	return engine, provider, project, folder, log
}

// quoteJSON wraps a value as the JSON string a model would send in a tool call.
func quoteJSON(value string) string {
	replaced := strings.ReplaceAll(value, `\`, `\\`)
	return `"` + strings.ReplaceAll(replaced, `"`, `\"`) + `"`
}

// TestAProjectTurnRunsACommandAfterAsking is the whole path in one test: the model asks, the
// user is asked first, the command runs in the project folder, and the model's next round
// carries a result it can read — the exit code and the command's own output.
func TestAProjectTurnRunsACommandAfterAsking(t *testing.T) {
	conf := &recordingConfirm{}
	engine, _, project, folder, _ := commandEngine(t, conf, writeFileCommand())

	if err := engine.Send(TurnRequest{ChatID: project.ID, Text: "Schreib die Datei."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Send starts the turn on its own goroutine, so the assertions below wait for it.
	if !waitFor(t, 30*time.Second, func() bool { return !engine.IsRunning(project.ID) }) {
		t.Fatal("the turn did not finish")
	}
	if conf.asked != 1 {
		t.Fatalf("the user was asked %d times, want once", conf.asked)
	}
	if conf.line != writeFileCommand() {
		t.Errorf("the question was about %q, want the line that ran", conf.line)
	}

	if _, err := os.Stat(filepath.Join(folder, "from-the-command.txt")); err != nil {
		t.Errorf("the command did not run in the project folder: %v", err)
	}

	records, err := engine.Store.ListToolCalls(project.ID)
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("tool call records = %d, want one", len(records))
	}
	if records[0].Status != store.ToolOK {
		t.Errorf("the call is recorded as %q, want %q — a command that ran and exited 0 is not a fault",
			records[0].Status, store.ToolOK)
	}

	// What the model reads on the round after the call: the line it asked for and how the
	// command ended. Without this the model is answering about a command it cannot see.
	result := lastToolResult(t, engine, project.ID)
	if !strings.Contains(result, writeFileCommand()) {
		t.Errorf("the model's tool result does not say what ran:\n%s", result)
	}
	if !strings.Contains(result, "Exit-Code 0") {
		t.Errorf("the model's tool result does not report the exit code:\n%s", result)
	}
}

// TestADeclinedCommandIsRecordedAsADecision pins the other end of the gate at the same level:
// the command must not have run, the record must read `denied` rather than `error`, and the
// sentence the model reads has to tell it not to try again. A declined command reported as a
// fault is a command the model retries.
func TestADeclinedCommandIsRecordedAsADecision(t *testing.T) {
	conf := &recordingConfirm{answer: errors.New("the user said no")}
	engine, _, project, folder, _ := commandEngine(t, conf, writeFileCommand())

	if err := engine.Send(TurnRequest{ChatID: project.ID, Text: "Schreib die Datei."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Send starts the turn on its own goroutine, so the assertions below wait for it.
	if !waitFor(t, 30*time.Second, func() bool { return !engine.IsRunning(project.ID) }) {
		t.Fatal("the turn did not finish")
	}
	if _, err := os.Stat(filepath.Join(folder, "from-the-command.txt")); !os.IsNotExist(err) {
		t.Errorf("a declined command ran anyway: %v", err)
	}

	records, err := engine.Store.ListToolCalls(project.ID)
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(records) != 1 || records[0].Status != store.ToolDenied {
		t.Fatalf("the declined call is recorded as %+v, want one call with status %q", records, store.ToolDenied)
	}
	result := lastToolResult(t, engine, project.ID)
	if !strings.Contains(result, "Nicht ausgeführt") {
		t.Errorf("the model is not told the command was not run:\n%s", result)
	}
	// And the turn finished rather than failing: the model answered after the refusal.
	if !strings.Contains(lastAssistantText(t, engine, project.ID), "Die Datei steht.") {
		t.Error("the turn did not continue after the refusal")
	}
}

// pauseCommand waits a little without doing anything, in the shell each platform has.
func pauseCommand(seconds int) string {
	if runtime.GOOS == "windows" {
		// ping is the portable Windows sleeper: `timeout` needs a console it does not have
		// here, and it fails immediately without one.
		return "ping -n " + strconv.Itoa(seconds+1) + " 127.0.0.1 > NUL"
	}
	return "sleep " + strconv.Itoa(seconds)
}

// TestTheWindowSeesACommandWhileItRuns is why the terminal streams at all: a build that
// prints nothing until it ends is indistinguishable from one that hung, so the card is given
// the output as it arrives. What is pinned here is the wiring a compile check cannot see —
// that a report reaches `chat:tool-output` naming the chat and the call, and that it arrives
// *before* the record that ends the call rather than beside it.
func TestTheWindowSeesACommandWhileItRuns(t *testing.T) {
	// One line, a pause, then another. The pause is what makes "the first report has no
	// second line" mean the report left while the command was still running.
	line := "echo eins && " + pauseCommand(1) + " && echo zwei"
	conf := &recordingConfirm{}
	engine, _, project, _, log := commandEngine(t, conf, line)

	if err := engine.Send(TurnRequest{ChatID: project.ID, Text: "Lauf mal los."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 30*time.Second, func() bool { return !engine.IsRunning(project.ID) }) {
		t.Fatal("the turn did not finish")
	}

	seen := log.snapshot()
	firstReport, firstRecord := -1, -1
	var report map[string]any
	for i, ev := range seen {
		switch ev.name {
		case events.ToolOutput:
			if firstReport < 0 {
				firstReport, report = i, ev.payload
			}
		case events.Tool:
			if record, ok := ev.payload["toolCall"].(store.ToolCallRecord); ok && record.Status != store.ToolPending && firstRecord < 0 {
				firstRecord = i
			}
		}
	}
	if firstReport < 0 {
		t.Fatal("no chat:tool-output event was emitted, so the card stayed empty until the command ended")
	}
	// The keys are the interface the window reads: a renamed one is not a compile error on
	// either side of the bridge, it is a live view that silently never appears.
	if got := report["chatId"]; got != project.ID {
		t.Errorf("the report names chat %v, want %q", got, project.ID)
	}
	if got := report["toolCallId"]; got != "call-1" {
		t.Errorf("the report names call %v, want call-1 — the card it belongs to", got)
	}
	text, _ := report["text"].(string)
	if !strings.Contains(text, "eins") {
		t.Errorf("the first report carries no output: %q", text)
	}
	if strings.Contains(text, "zwei") {
		t.Errorf("the first report already held the last line, so nothing was streamed: %q", text)
	}
	if firstRecord >= 0 && firstRecord < firstReport {
		t.Error("the command's record arrived before its first output, so the card had nothing to watch")
	}
}

// lastToolResult is the text of the newest tool message in a chat: the result as the model
// reads it, which is not the same thing as the record on the card (that one holds the whole
// output, this one only what fitted the answer's budget).
func lastToolResult(t *testing.T, engine *Engine, chatID string) string {
	t.Helper()
	messages, err := engine.Store.ListMessages(chatID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == string(llm.RoleTool) {
			return messages[i].Content
		}
	}
	t.Fatal("the turn produced no tool result")
	return ""
}

// lastAssistantText is the newest assistant reply.
func lastAssistantText(t *testing.T, engine *Engine, chatID string) string {
	t.Helper()
	messages, err := engine.Store.ListMessages(chatID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == string(llm.RoleAssistant) && messages[i].Content != "" {
			return messages[i].Content
		}
	}
	t.Fatal("the turn produced no assistant reply")
	return ""
}
