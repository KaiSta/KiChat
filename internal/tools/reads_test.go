package tools

// The read shortcut, and the one case it must not answer.
//
// Two promises are pinned here, and they are different promises. The first is the shortcut
// itself: a second read of an untouched range costs a sentence rather than the file. The
// second is the reason that shortcut is told about changes instead of trusting the filesystem:
// a model that re-reads a file it has just written is asking what is *in* the file, and an
// answer about the app's memory would be wrong in the one situation where being wrong is
// expensive — the model would conclude its change is there when it is not, or that it was not
// written when it was.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// turnContext is a tool call inside a turn, which is what the shortcut is scoped to: without a
// turn there is no "already" to speak of, and a read outside one always returns the file.
func turnContext(t *testing.T, chatID string) context.Context {
	t.Helper()
	return WithTurn(context.Background(), TurnRef{BackendID: "test-backend", Turn: "turn-1"})
}

// TestASecondReadOfAnUntouchedFileIsASentence is the shortcut working as intended: the file
// text is not sent twice, and the sentence tells the model what to do instead.
func TestASecondReadOfAnUntouchedFileIsASentence(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{"notes.txt": "alpha\nbeta\n"})
	ctx := turnContext(t, chatID)

	first, err := service.Execute(ctx, chatID, ToolReadFile, `{"path":"notes.txt"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(first, "alpha") {
		t.Fatalf("the first read did not return the file:\n%s", first)
	}

	second, err := service.Execute(ctx, chatID, ToolReadFile, `{"path":"notes.txt"}`)
	if err != nil {
		t.Fatalf("read_file again: %v", err)
	}
	if !strings.Contains(second, "bereits gelesen") {
		t.Errorf("the second read was not answered with the shortcut:\n%s", second)
	}
	if strings.Contains(second, "alpha") {
		t.Errorf("the shortcut still sent the file:\n%s", second)
	}
}

// TestAReadAfterAWriteReturnsTheFile is the one the cache must never get wrong, and it is
// written so that the *filesystem* cannot save it: the write is given the same byte length and
// the modification time is put back afterwards, so a size-and-mtime comparison sees an
// untouched file. Only the tool forgetting what it touched can make the second read show the
// new text — which is what a model checking its own work needs.
func TestAReadAfterAWriteReturnsTheFile(t *testing.T) {
	service, chatID, folder := projectService(t, map[string]string{"notes.txt": "alpha\n"})
	service.Confirm = (&asked{}).confirm
	ctx := turnContext(t, chatID)

	if _, err := service.Execute(ctx, chatID, ToolReadFile, `{"path":"notes.txt"}`); err != nil {
		t.Fatalf("read_file: %v", err)
	}
	before, err := os.Stat(filepath.Join(folder, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}

	// Same number of bytes as before, so the size is no help, and the stamp is put back so the
	// time is no help either.
	if _, err := service.Execute(ctx, chatID, ToolWriteFile,
		`{"path":"notes.txt","content":"bravo\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if err := os.Chtimes(filepath.Join(folder, "notes.txt"), before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}

	after, err := service.Execute(ctx, chatID, ToolReadFile, `{"path":"notes.txt"}`)
	if err != nil {
		t.Fatalf("read_file after the write: %v", err)
	}
	if !strings.Contains(after, "bravo") {
		t.Errorf("the read after a write did not return the new text:\n%s", after)
	}
	if strings.Contains(after, "bereits gelesen") {
		t.Errorf("the read after a write was answered from memory:\n%s", after)
	}
}

// TestAReadAfterAnEditReturnsTheFile is the same promise for the other changing tool. An edit
// is the one a model most often re-reads: it replaced a line it only ever saw in a diff, so
// the text around the replacement is what it has never actually read.
func TestAReadAfterAnEditReturnsTheFile(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{"main.go": "package main\n\nfunc A() {}\n"})
	service.Confirm = (&asked{}).confirm
	ctx := turnContext(t, chatID)

	if _, err := service.Execute(ctx, chatID, ToolReadFile, `{"path":"main.go"}`); err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if _, err := service.Execute(ctx, chatID, ToolEditFile,
		`{"path":"main.go","old_string":"func A() {}","new_string":"func A() { return }"}`); err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	after, err := service.Execute(ctx, chatID, ToolReadFile, `{"path":"main.go"}`)
	if err != nil {
		t.Fatalf("read_file after the edit: %v", err)
	}
	if !strings.Contains(after, "func A() { return }") {
		t.Errorf("the read after an edit did not return the edited file:\n%s", after)
	}
}

// TestAnOutlineNeitherCountsAsAReadNorIsAnsweredByOne pins both directions of the same
// distinction: an outline is a map, not a reading of the section. A model that outlines a file
// and then reads it must get the file, and a model that read the file and then asks for the map
// has not "already read" the map — telling it so would be a lie about what it received.
func TestAnOutlineNeitherCountsAsAReadNorIsAnsweredByOne(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{"main.go": "package main\n\nfunc A() {}\n"})
	ctx := turnContext(t, chatID)

	outline, err := service.Execute(ctx, chatID, ToolReadFile, `{"path":"main.go","outline":true}`)
	if err != nil {
		t.Fatalf("read_file with outline: %v", err)
	}
	if !strings.Contains(outline, "func A()") {
		t.Fatalf("the outline did not list the declaration:\n%s", outline)
	}
	if strings.Contains(outline, "bereits gelesen") {
		t.Errorf("the first call was answered from a memory it never wrote:\n%s", outline)
	}

	whole, err := service.Execute(ctx, chatID, ToolReadFile, `{"path":"main.go"}`)
	if err != nil {
		t.Fatalf("read_file after the outline: %v", err)
	}
	if !strings.Contains(whole, "package main") {
		t.Errorf("the read after an outline was answered as a repeat:\n%s", whole)
	}

	again, err := service.Execute(ctx, chatID, ToolReadFile, `{"path":"main.go","outline":true}`)
	if err != nil {
		t.Fatalf("read_file with outline after a read: %v", err)
	}
	if strings.Contains(again, "bereits gelesen") {
		t.Errorf("the map was refused because the text had been read:\n%s", again)
	}
}

// TestTheShapeOfAReadIsClassifiedForTheTrace pins the four words the trace counts, including
// the one no argument can tell on its own: a bounded read that takes the offset this app just
// offered is a continuation, and the same offset typed from a model's own guess is not.
func TestTheShapeOfAReadIsClassifiedForTheTrace(t *testing.T) {
	var body strings.Builder
	for i := 1; i <= 40; i++ {
		body.WriteString("line\n")
	}
	service, chatID, folder := projectService(t, map[string]string{
		"long.txt": body.String(),
		"pkg/a.go": "package pkg\n\nfunc A() {}\n",
	})
	ctx := turnContext(t, chatID)
	shape := func(call string) string {
		t.Helper()
		result, err := service.ExecuteWithSummary(ctx, chatID, ToolReadFile, call)
		if err != nil {
			t.Fatalf("read_file %s: %v", call, err)
		}
		return result.Shape
	}

	if got := shape(`{"path":"pkg/a.go"}`); got != ReadShapeBare {
		t.Errorf("a whole-file read is %q, want %q", got, ReadShapeBare)
	}
	if got := shape(`{"path":"pkg/a.go","offset":1,"limit":2}`); got != ReadShapeBounded {
		t.Errorf("a chosen window is %q, want %q", got, ReadShapeBounded)
	}
	if got := shape(`{"path":"pkg/a.go","outline":true}`); got != ReadShapeOutline {
		t.Errorf("an outline is %q, want %q", got, ReadShapeOutline)
	}

	// A truncated read ends by naming the line it continues at; a call that takes it is the
	// app's own suggestion followed, and a call that picks another line is not.
	first := shape(`{"path":"long.txt","limit":10}`)
	if first != ReadShapeBounded {
		t.Fatalf("the first window is %q, want %q", first, ReadShapeBounded)
	}
	if got := shape(`{"path":"long.txt","offset":11,"limit":5}`); got != ReadShapeContinuation {
		t.Errorf("the offered offset is %q, want %q", got, ReadShapeContinuation)
	}
	if got := shape(`{"path":"long.txt","offset":31,"limit":5}`); got != ReadShapeBounded {
		t.Errorf("an offset of the model's own is %q, want %q", got, ReadShapeBounded)
	}

	// An offer names lines, and a write can move them, so the memory of it goes with the rest.
	service.ForgetReads(chatID, filepath.Join(folder, "long.txt"))
	if got := shape(`{"path":"long.txt","offset":11,"limit":5}`); got != ReadShapeBounded {
		t.Errorf("an offer that outlived a write is %q, want %q", got, ReadShapeBounded)
	}
}

// TestACommandDropsTheChatsMemory is the third way the folder changes. A command's effect
// cannot be enumerated — `gofmt -w .` rewrites every file under a folder — so the honest rule
// is that after one the app knows nothing, and the next read costs a disk read.
func TestACommandDropsTheChatsMemory(t *testing.T) {
	service, chatID, _ := commandService(t)
	ctx := turnContext(t, chatID)
	remembered := readKey{chatID: chatID, turn: "turn-1", path: "/tmp/anything"}
	service.rememberRead(remembered, readStamp{})
	if _, err := service.Execute(ctx, chatID, ToolRunCommand,
		`{"command":`+jsonString("echo done")+`}`); err != nil {
		t.Fatalf("run_command: %v", err)
	}
	if service.readSeen(remembered, readStamp{}) {
		t.Error("a command left the chat's read memory in place")
	}
}

// TestARememberedReadIsScopedToItsTurn pins the boundary of the memory: the next turn starts
// clean, so a model that continues a conversation is never told about a read it cannot
// remember making.
func TestARememberedReadIsScopedToItsTurn(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{"notes.txt": "alpha\n"})

	first := WithTurn(context.Background(), TurnRef{BackendID: "test-backend", Turn: "turn-1"})
	if _, err := service.Execute(first, chatID, ToolReadFile, `{"path":"notes.txt"}`); err != nil {
		t.Fatalf("read_file: %v", err)
	}
	second := WithTurn(context.Background(), TurnRef{BackendID: "test-backend", Turn: "turn-2"})
	out, err := service.Execute(second, chatID, ToolReadFile, `{"path":"notes.txt"}`)
	if err != nil {
		t.Fatalf("read_file in the next turn: %v", err)
	}
	if !strings.Contains(out, "alpha") {
		t.Errorf("the next turn's first read was answered from the previous turn:\n%s", out)
	}
}
