package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kichat/internal/config"
	"kichat/internal/uimsg"
	"kichat/internal/workspace"
)

// asked is a confirmer that records what it was shown and answers however the test says.
//
// It is the whole point of these tests: the question is not "did the tool run" but "what
// did the person see before it ran", and a confirmer that only returned nil would prove
// neither half.
type asked struct {
	calls   int
	request ApprovalRequest
	answer  error
}

func (a *asked) confirm(_ context.Context, req *ApprovalRequest) error {
	a.calls++
	a.request = *req
	return a.answer
}

// diffText is every row of a change's hunks, so a test can talk about what the card would
// show without reaching into the hunk structure for each line.
func diffText(change workspace.Change) string {
	var b strings.Builder
	for _, hunk := range change.Hunks {
		for _, line := range hunk.Lines {
			b.WriteString(line.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// writeService is projectService with a confirmer attached.
func writeService(t *testing.T, files map[string]string, conf *asked) (*Service, string, string) {
	t.Helper()
	service, chatID, folder := projectService(t, files)
	if conf != nil {
		service.Confirm = conf.confirm
	}
	return service, chatID, folder
}

// TestAChangeIsShownBeforeItIsMade is the promise the whole write side exists for: the
// person is asked about a change that has not happened, and what they were shown is what
// they would get.
func TestAChangeIsShownBeforeItIsMade(t *testing.T) {
	conf := &asked{}
	service, chatID, folder := writeService(t, nil, conf)
	ctx := WithToolCall(WithCallingMessage(context.Background(), "msg-1"), "call-1")

	if _, err := service.Execute(ctx, chatID, ToolWriteFile,
		`{"path":"internal/store/notes.go","content":"package store\n\ntype Note struct{}\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}

	if conf.calls != 1 {
		t.Fatalf("the change was confirmed %d times, want exactly 1", conf.calls)
	}
	req := conf.request
	if req.Change.Operation != workspace.OperationCreate {
		t.Errorf("operation = %q, want %q", req.Change.Operation, workspace.OperationCreate)
	}
	if req.Change.Path != "internal/store/notes.go" {
		t.Errorf("path = %q, want the workspace-relative path the model wrote", req.Change.Path)
	}
	// What the card draws comes from the hunks now, so the text of the change has to be in
	// them: a tool that filled a count but no rows would ask about a change nobody can read.
	if shown := diffText(req.Change); !strings.Contains(shown, "type Note struct{}") {
		t.Errorf("the question carries %q as its diff, which is not what was written", shown)
	}
	if req.Change.Added != 3 {
		t.Errorf("Added = %d, want the three lines being written", req.Change.Added)
	}
	// The two identities the answer needs: the call it belongs to, and the row it renders
	// on. Without them the window could only guess which of several waiting calls answered.
	if req.ToolCallID != "call-1" {
		t.Errorf("ToolCallID = %q, want the id the answer will name", req.ToolCallID)
	}
	if req.MessageID != "msg-1" {
		t.Errorf("MessageID = %q, want the row the call is rendered on", req.MessageID)
	}
	if req.Tool != ToolWriteFile {
		t.Errorf("Tool = %q, want %q", req.Tool, ToolWriteFile)
	}

	body, err := os.ReadFile(filepath.Join(folder, "internal", "store", "notes.go"))
	if err != nil {
		t.Fatalf("the file was not written after the yes: %v", err)
	}
	if !strings.Contains(string(body), "type Note struct{}") {
		t.Errorf("the file holds %q, not the text that was approved", body)
	}
}

// TestACreateWouldNotBeShownWithoutTheContent is the other half of the same promise, and
// the reason the question is asked at all: a write that was never confirmed must leave the
// folder exactly as it was.
func TestACreateWouldNotBeShownWithoutTheContent(t *testing.T) {
	conf := &asked{answer: errors.New("no")}
	service, chatID, folder := writeService(t, nil, conf)

	_, err := service.Execute(context.Background(), chatID, ToolWriteFile,
		`{"path":"notes.go","content":"package store\n"}`)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("a refused change returned %v, want ErrDenied", err)
	}
	if _, err := os.Stat(filepath.Join(folder, "notes.go")); !os.IsNotExist(err) {
		t.Errorf("the file exists after a change nobody agreed to: %v", err)
	}
}

// TestAMissingConfirmerRefusesTheChange is the fail-closed rule, and it is the one that
// matters most: a build that cannot ask anybody must not change anything, and must say so
// as a refusal rather than as a fault — the model has to stop, not retry.
func TestAMissingConfirmerRefusesTheChange(t *testing.T) {
	service, chatID, folder := writeService(t, nil, nil)

	_, err := service.Execute(context.Background(), chatID, ToolWriteFile,
		`{"path":"notes.go","content":"package store\n"}`)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("a write with nobody to ask returned %v, want ErrDenied", err)
	}
	if code := refusalCode(t, err); code != "tools.denied" {
		t.Errorf("refusal code = %q, want tools.denied", code)
	}
	if _, err := os.Stat(filepath.Join(folder, "notes.go")); !os.IsNotExist(err) {
		t.Errorf("the file exists although nobody could be asked: %v", err)
	}
}

// TestAnImpossibleChangeIsNeverShown is the ordering rule between planning and asking: a
// change that cannot happen must not reach the person, because a dialog about a path
// outside the folder would train them to say yes to something that never runs — and,
// worse, it would spend their attention on the app's own bug.
func TestAnImpossibleChangeIsNeverShown(t *testing.T) {
	conf := &asked{}
	service, chatID, folder := writeService(t, nil, conf)

	_, err := service.Execute(context.Background(), chatID, ToolWriteFile,
		`{"path":"../outside.go","content":"package x\n"}`)
	if code := refusalCode(t, err); code != "tools.outsideWorkspace" {
		t.Fatalf("a path outside the folder refused with %q, want tools.outsideWorkspace", code)
	}
	if _, statErr := os.Stat(filepath.Join(folder, "..", "outside.go")); !os.IsNotExist(statErr) {
		t.Errorf("a file was written above the project folder: %v", statErr)
	}
	if conf.calls != 0 {
		t.Errorf("the user was asked to confirm a change that could not be made (%d times)", conf.calls)
	}

	// The same for an edit that no longer matches: the refusal names the fix, and asking
	// first would be asking about a change that cannot be applied.
	conf2 := &asked{}
	service2, chatID2, _ := writeService(t, map[string]string{"a.go": "package a\n"}, conf2)
	_, err = service2.Execute(context.Background(), chatID2, ToolEditFile,
		`{"path":"a.go","old_string":"not in the file","new_string":"x"}`)
	if code := refusalCode(t, err); code != "tools.editNoMatch" {
		t.Errorf("an edit that does not match refused with %q, want tools.editNoMatch", code)
	}
	if conf2.calls != 0 {
		t.Errorf("the user was asked about an edit that cannot be applied (%d times)", conf2.calls)
	}
}

// TestAnEditRefusalNamesTheProblemAndNotTheParser keeps the three edit failures apart. A
// model told "the change failed" rewrites the whole file, which is the expensive and
// destructive answer to a problem that one more read would have solved.
func TestAnEditRefusalNamesTheProblemAndNotTheParser(t *testing.T) {
	conf := &asked{}
	// One identifier declared twice with the same text: the case an exact-string edit
	// cannot resolve on its own.
	service, chatID, _ := writeService(t, map[string]string{
		"a.go": "package a\n\nvar same = 1\nvar same = 1\n",
	}, conf)

	_, err := service.Execute(context.Background(), chatID, ToolEditFile,
		`{"path":"a.go","old_string":"var same = 1","new_string":"var same = 3"}`)
	if code := refusalCode(t, err); code != "tools.editAmbiguous" {
		t.Fatalf("an edit matching twice refused with %q, want tools.editAmbiguous", code)
	}
	// The count and the path travel as arguments, so the window's sentence can name them
	// in its own language.
	message, ok := uimsg.Parse(err.Error())
	if !ok {
		t.Fatalf("the refusal carries no code: %v", err)
	}
	if len(message.Args) < 3 || message.Args[1] != "2" {
		t.Errorf("the refusal's arguments are %v, want the path, the count (2) and the first line", message.Args)
	}

	// And the same call with replace_all is allowed to be ambiguous, so the confirmation
	// is reached — that is the difference the two codes exist to express.
	conf.calls = 0
	if _, err := service.Execute(context.Background(), chatID, ToolEditFile,
		`{"path":"a.go","old_string":"var same = 1","new_string":"var same = 3","replace_all":true}`); err != nil {
		t.Fatalf("an edit with replace_all failed: %v", err)
	}
	if conf.calls != 1 {
		t.Errorf("the user was asked %d times for a change that can be made, want 1", conf.calls)
	}
}

// TestTheWriteToolsAreTheOnesTheEngineAsksAbout keeps the two lists that must agree in
// step: the engine derives the confirmation gate from WriteToolNames, so a third write
// tool added to the definitions but not to that list would be offered and then executed
// *without* asking anybody.
func TestTheWriteToolsAreTheOnesTheEngineAsksAbout(t *testing.T) {
	advertised := map[string]bool{}
	for _, def := range changeToolDefinitions(config.LangEnglish) {
		advertised[def.Name] = true
	}
	names := WriteToolNames()
	if len(names) != len(advertised) {
		t.Fatalf("WriteToolNames lists %d tools but the definitions hold %d: %v",
			len(names), len(advertised), names)
	}
	for _, name := range names {
		if !advertised[name] {
			t.Errorf("WriteToolNames names %q, which is not defined here", name)
		}
		if !NeedsApproval(name) {
			t.Errorf("%q is a write tool the engine would not gate", name)
		}
		if !containsString(ProjectToolNames(), name) {
			t.Errorf("%q is a write tool missing from the project tool list", name)
		}
	}
	// The read tools must not be gated: a dialog for reading a file is a dialog nobody
	// reads, which is how a confirmation stops meaning anything.
	for _, name := range []string{ToolReadFile, ToolListDir, ToolSearchFiles} {
		if NeedsApproval(name) {
			t.Errorf("%q only reads, but the engine would ask before running it", name)
		}
	}
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
