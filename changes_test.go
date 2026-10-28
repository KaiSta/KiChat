package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/tools"
	"kichat/internal/uimsg"
	"kichat/internal/workspace"
)

// approvalApp is an app with the event sink replaced by a recorder, which is what lets a
// test watch a question go out and answer it the way the window does — through
// ResolveToolApproval, by id.
type approvalApp struct {
	app  *App
	mu   sync.Mutex
	sent []map[string]any
	// arrived is closed by the first chat:approval, so a test can wait for the question
	// instead of sleeping and hoping.
	arrived chan struct{}
}

func newApprovalApp(t *testing.T) *approvalApp {
	t.Helper()
	recorder := &approvalApp{arrived: make(chan struct{})}
	recorder.app = &App{ctx: context.Background()}
	recorder.app.emitFn = func(name string, payload any) {
		if name != events.Approval {
			return
		}
		recorder.mu.Lock()
		if fields, ok := payload.(map[string]any); ok {
			recorder.sent = append(recorder.sent, fields)
		}
		recorder.mu.Unlock()
		select {
		case <-recorder.arrived:
		default:
			close(recorder.arrived)
		}
	}
	return recorder
}

func (r *approvalApp) questions() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.sent...)
}

// waitQuestions waits until the recorded questions number want, or fails. It exists so a
// test can observe the *absence* of a second question: after an assertion there is no event
// left to wait for, and a sleep would make the test slow without making it reliable.
func (r *approvalApp) waitQuestions(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.questions()) >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%d questions were asked, want %d", len(r.questions()), want)
}

func changeRequest(chatID, callID string, change workspace.Change) tools.ApprovalRequest {
	return tools.ApprovalRequest{
		ChatID:     chatID,
		MessageID:  "msg-1",
		ToolCallID: callID,
		Tool:       tools.ToolWriteFile,
		Kind:       tools.ApprovalKindChange,
		Change:     change,
	}
}

// TestTheApprovalPayloadCarriesWhatTheWindowReads pins the one interface in this feature
// that neither side's compiler checks. The window subscribes by the event name and reads
// the payload by key; a renamed field is not an error anywhere — it is a card that silently
// renders nothing while every change still works.
func TestTheApprovalPayloadCarriesWhatTheWindowReads(t *testing.T) {
	payload := approvalPayload(changeRequest("chat-1", "call-1", workspace.Change{
		Path:      "internal/store/notes.go",
		Operation: workspace.OperationCreate,
		After:     "package store\n",
		Added:     1,
	}))

	for _, key := range []string{
		"approvalId", "chatId", "messageId", "toolCallId", "tool", "kind", "change", "command",
	} {
		if _, ok := payload[key]; !ok {
			t.Errorf("the payload has no %q; the window reads it by that name", key)
		}
	}
	if payload["approvalId"] != "call-1" || payload["toolCallId"] != "call-1" {
		t.Errorf("the answer cannot be matched to its call: %+v", payload)
	}
	if payload["kind"] != tools.ApprovalKindChange {
		t.Errorf("kind = %v, want %q — the card switches on it", payload["kind"], tools.ApprovalKindChange)
	}
	if payload["command"] != nil {
		t.Errorf("a file change carries a command as well: %+v", payload["command"])
	}
	change, ok := payload["change"].(map[string]any)
	if !ok {
		t.Fatalf("a file change carries no change object: %+v", payload["change"])
	}
	for _, key := range []string{
		"path", "operation", "before", "after", "added", "removed", "truncated", "createsDirs",
	} {
		if _, ok := change[key]; !ok {
			t.Errorf("the change has no %q; the window reads it by that name", key)
		}
	}
	if change["operation"] != workspace.OperationCreate {
		t.Errorf("operation = %v, want %q", change["operation"], workspace.OperationCreate)
	}

	// The folders list is walked by the window, so it has to be a list even when there are
	// none: a nil Go slice marshals to JSON null, and `null.length` is how a change with no
	// new folders becomes a render error.
	encoded, err := json.Marshal(change)
	if err != nil {
		t.Fatalf("marshal the change: %v", err)
	}
	if !strings.Contains(string(encoded), `"createsDirs":[]`) {
		t.Errorf("a change that creates no folders marshals as %s, want an empty list", encoded)
	}
	if strings.Contains(string(encoded), "null") {
		t.Errorf("the change carries a null the window would have to guard: %s", encoded)
	}
}

// TestTheCommandPayloadCarriesTheWholeQuestion is the other kind: a line, the folder it runs
// in, the shell that reads it and the time it gets. A command is the one tool whose effect
// cannot be previewed, so everything needed to judge it has to be on the card.
func TestTheCommandPayloadCarriesTheWholeQuestion(t *testing.T) {
	payload := approvalPayload(tools.ApprovalRequest{
		ChatID:     "chat-1",
		MessageID:  "msg-1",
		ToolCallID: "call-1",
		Tool:       tools.ToolRunCommand,
		Kind:       tools.ApprovalKindCommand,
		Command: tools.Command{
			Line:       "go test ./...",
			Dir:        "/home/you/projects/kichat",
			TimeoutSec: 120,
			Shell:      "/bin/sh -c",
		},
	})

	if payload["kind"] != tools.ApprovalKindCommand {
		t.Errorf("kind = %v, want %q", payload["kind"], tools.ApprovalKindCommand)
	}
	if payload["change"] != nil {
		t.Errorf("a command carries a file change as well: %+v", payload["change"])
	}
	command, ok := payload["command"].(map[string]any)
	if !ok {
		t.Fatalf("a command carries no command object: %+v", payload["command"])
	}
	for _, key := range []string{"line", "dir", "timeoutSec", "shell"} {
		if _, ok := command[key]; !ok {
			t.Errorf("the command has no %q; the window reads it by that name", key)
		}
	}
	if command["line"] != "go test ./..." {
		t.Errorf("the card would show %v, which is not the line that runs", command["line"])
	}
	if command["timeoutSec"] != 120 {
		t.Errorf("timeoutSec = %v, want 120", command["timeoutSec"])
	}
}

// TestAnAnswerReachesTheWaitingChange is the round trip the whole gate rests on: the turn
// blocks, the question goes out naming the call, and the answer comes back naming the same
// one. Anything else and the turn waits forever.
func TestAnAnswerReachesTheWaitingChange(t *testing.T) {
	recorder := newApprovalApp(t)
	done := make(chan error, 1)

	go func() {
		done <- recorder.app.confirmChange(context.Background(),
			changeRequest("chat-1", "call-1", workspace.Change{Path: "notes.go", Operation: workspace.OperationCreate}))
	}()

	<-recorder.arrived
	if got := recorder.questions(); len(got) != 1 {
		t.Fatalf("the question was asked %d times, want once", len(got))
	}
	if !recorder.app.ResolveToolApproval("call-1", approvalOnce) {
		t.Fatal("the answer did not reach the waiting change")
	}
	if err := <-done; err != nil {
		t.Fatalf("an allowed change returned %v, want nil", err)
	}
	// The waiter is gone once it was answered, so the same call cannot be answered again.
	if recorder.app.ResolveToolApproval("call-1", approvalOnce) {
		t.Error("the same question was answered twice")
	}
}

// TestNoAnswerMeansNoChange is the rule that matters most: a turn that was stopped, a click
// after the question had moved on, and a window that never answered all end in the same
// place — nothing was allowed. Silence is not consent.
func TestNoAnswerMeansNoChange(t *testing.T) {
	recorder := newApprovalApp(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- recorder.app.confirmChange(ctx,
			changeRequest("chat-1", "call-1", workspace.Change{Path: "notes.go", Operation: workspace.OperationCreate}))
	}()
	<-recorder.arrived
	cancel()

	err := <-done
	if !errors.Is(err, tools.ErrDenied) {
		t.Fatalf("a stopped turn returned %v, want a refusal", err)
	}
	if _, ok := uimsg.Parse(err.Error()); !ok {
		t.Errorf("the refusal carries no code, so the window would show raw text: %v", err)
	}
	// The waiting change is dropped with the turn, so a click that arrives afterwards is
	// reported as what it is — a late answer to a question nobody is waiting for — rather
	// than being left in the map forever.
	if recorder.app.ResolveToolApproval("call-1", approvalOnce) {
		t.Error("a stopped turn was still waiting for an answer")
	}
	if got := recorder.app.changesAllowed("chat-1"); got {
		t.Error("a stopped turn granted a standing permission")
	}
}

// TestAChangeNobodyCanBeAskedAboutIsRefused covers the one call the app cannot correlate
// with a window: no tool call id, so no answer could ever name it.
func TestAChangeNobodyCanBeAskedAboutIsRefused(t *testing.T) {
	recorder := newApprovalApp(t)

	err := recorder.app.confirmChange(context.Background(),
		changeRequest("chat-1", "", workspace.Change{Path: "notes.go", Operation: workspace.OperationCreate}))
	if !errors.Is(err, tools.ErrDenied) {
		t.Fatalf("an uncorrelatable change returned %v, want a refusal", err)
	}
	if len(recorder.questions()) != 0 {
		t.Error("a question nobody could answer was put on screen anyway")
	}
}

// TestAStandingAnswerSkipsTheFileQuestion pins the setting's effect on this gate: a call the
// user has already allowed runs like a read tool — no card, no blocked turn — while the other
// half of the gate stays shut, because the two are separate risks rather than two strengths of
// the same one.
func TestAStandingAnswerSkipsTheFileQuestion(t *testing.T) {
	recorder := newApprovalApp(t)
	recorder.app.settingsMu.Lock()
	recorder.app.settings = config.Default()
	recorder.app.settings.AutoApprove = config.AutoApproveSettings{Files: true}
	recorder.app.settingsMu.Unlock()

	if err := recorder.app.confirmChange(context.Background(),
		changeRequest("chat-1", "call-1", workspace.Change{Path: "a.go", Operation: workspace.OperationCreate})); err != nil {
		t.Fatalf("a file change the user allowed in advance was refused: %v", err)
	}
	if got := len(recorder.questions()); got != 0 {
		t.Errorf("%d questions were asked for a file change that was allowed in advance, want none", got)
	}

	// A command is the other question. Allowing the files said nothing about the machine, so
	// it is still asked — and it is answered on its own.
	command := tools.ApprovalRequest{
		ChatID:     "chat-1",
		MessageID:  "msg-1",
		ToolCallID: "call-2",
		Tool:       tools.ToolRunCommand,
		Kind:       tools.ApprovalKindCommand,
		Command: tools.Command{
			Line: "go test ./...", Dir: "/tmp/project", TimeoutSec: 120, Shell: "/bin/sh -c",
		},
	}
	done := make(chan error, 1)
	go func() { done <- recorder.app.confirmChange(context.Background(), command) }()
	recorder.waitQuestions(t, 1)
	if !recorder.app.ResolveToolApproval("call-2", approvalOnce) {
		t.Fatal("the command's question could not be answered")
	}
	if err := <-done; err != nil {
		t.Fatalf("the allowed command returned %v, want nil", err)
	}
}

// TestAGuardedToolTheSettingDoesNotNameStillAsks is the fail-closed half of the setting. It
// answers by *kind* for the tools that exist when it is set, so a tool added later cannot
// inherit a permission somebody granted for a different one.
func TestAGuardedToolTheSettingDoesNotNameStillAsks(t *testing.T) {
	recorder := newApprovalApp(t)
	recorder.app.settingsMu.Lock()
	recorder.app.settings = config.Default()
	recorder.app.settings.AutoApprove = config.AutoApproveSettings{Files: true, Commands: true}
	recorder.app.settingsMu.Unlock()

	request := changeRequest("chat-1", "call-1", workspace.Change{Path: "a.go", Operation: workspace.OperationCreate})
	request.Tool = "delete_file"
	done := make(chan error, 1)
	go func() { done <- recorder.app.confirmChange(context.Background(), request) }()
	recorder.waitQuestions(t, 1)
	if !recorder.app.ResolveToolApproval("call-1", "no") {
		t.Fatal("the future tool's question could not be answered")
	}
	if err := <-done; !errors.Is(err, tools.ErrDenied) {
		t.Fatalf("a declined call to a tool the setting does not name returned %v, want a refusal", err)
	}
}

// TestAlwaysLastsForOneChatAndOneProcess pins the scope of the blanket answer: it saves a
// long task from forty identical dialogs, and it must not become a permission the user
// cannot see the edges of.
func TestAlwaysLastsForOneChatAndOneProcess(t *testing.T) {
	recorder := newApprovalApp(t)

	done := make(chan error, 1)
	go func() {
		done <- recorder.app.confirmChange(context.Background(),
			changeRequest("chat-1", "call-1", workspace.Change{Path: "a.go", Operation: workspace.OperationCreate}))
	}()
	<-recorder.arrived
	if !recorder.app.ResolveToolApproval("call-1", approvalAlways) {
		t.Fatal("the answer did not reach the waiting change")
	}
	if err := <-done; err != nil {
		t.Fatalf("an allowed change returned %v, want nil", err)
	}

	// The next change in that chat is allowed with no question.
	if err := recorder.app.confirmChange(context.Background(),
		changeRequest("chat-1", "call-2", workspace.Change{Path: "b.go", Operation: workspace.OperationCreate})); err != nil {
		t.Fatalf("the remembered answer did not cover the next change: %v", err)
	}
	if got := len(recorder.questions()); got != 1 {
		t.Errorf("%d questions were asked, want only the first", got)
	}

	// A different chat is a different decision: it is asked, and answered on its own.
	other := make(chan error, 1)
	go func() {
		other <- recorder.app.confirmChange(context.Background(),
			changeRequest("chat-2", "call-3", workspace.Change{Path: "c.go", Operation: workspace.OperationCreate}))
	}()
	recorder.waitQuestions(t, 2)
	if !recorder.app.ResolveToolApproval("call-3", approvalOnce) {
		t.Fatal("the second chat's question could not be answered")
	}
	if err := <-other; err != nil {
		t.Fatalf("the second chat's change was refused: %v", err)
	}

	// And deleting the chat takes the permission with it: an answer given for a
	// conversation must not outlive it.
	recorder.app.forgetChanges("chat-1")
	if recorder.app.changesAllowed("chat-1") {
		t.Error("a deleted chat kept its standing permission")
	}
}
