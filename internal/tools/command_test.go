package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kichat/internal/uimsg"
)

// jsonString is one argument value as the model would send it: a real JSON string, so the
// tests go through the same decoding a tool call does rather than around it.
func jsonString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// cwdCommand prints the working directory, which is the one thing that proves the command
// runs where the project is rather than wherever the app happens to live. Two spellings,
// because the shell is the platform's: `pwd` is a program on Unix and a builtin in cmd.
func cwdCommand() string {
	if runtime.GOOS == "windows" {
		return "cd"
	}
	return "pwd"
}

// exitCommand ends with a chosen status, so the exit code can be asserted on both platforms.
func exitCommand(code string) string {
	if runtime.GOOS == "windows" {
		return "exit /b " + code
	}
	return "exit " + code
}

// sleepCommand runs for a while, for the timeout tests.
func sleepCommand(seconds int) string {
	if runtime.GOOS == "windows" {
		// ping is the portable Windows sleeper: `timeout` needs a console it does not have
		// here, and it fails immediately without one.
		return "ping -n " + strconv.Itoa(seconds+1) + " 127.0.0.1 > NUL"
	}
	return "sleep " + strconv.Itoa(seconds)
}

// commandService is projectService with a confirmer that says yes, so a test can be about
// what the command does rather than about the gate.
func commandService(t *testing.T) (*Service, string, string) {
	t.Helper()
	return writeService(t, nil, &asked{})
}

// TestACommandRunsInTheProjectFolder is the whole point of putting a terminal inside a
// project: the model's file tools resolve paths against the folder, so the commands it asks
// for have to see the same directory or nothing it reads and runs agrees.
func TestACommandRunsInTheProjectFolder(t *testing.T) {
	service, chatID, folder := commandService(t)

	out, err := service.Execute(context.Background(), chatID, ToolRunCommand,
		`{"command":`+jsonString(cwdCommand())+`}`)
	if err != nil {
		t.Fatalf("run_command: %v", err)
	}
	// macOS reports /private/tmp for a temp dir listed as /tmp; the resolved path is what
	// the tool ran in either way, so the comparison is on the last segment.
	if !strings.Contains(out, filepath.Base(folder)) {
		t.Errorf("the command did not run in the project folder %s:\n%s", folder, out)
	}
	if !strings.Contains(out, "Exit-Code 0") {
		t.Errorf("the result does not report the exit code:\n%s", out)
	}
	if !strings.HasPrefix(out, "$ "+cwdCommand()+"\n") {
		t.Errorf("the result does not begin with the command it ran:\n%s", out)
	}
}

// TestAFailingCommandIsAResult pins the distinction the whole tool depends on: a test suite
// that fails is the tool working. Reporting it as an error would put a red card on a normal
// outcome — and would tell the model the call went wrong rather than that the code does.
func TestAFailingCommandIsAResult(t *testing.T) {
	service, chatID, _ := commandService(t)

	out, err := service.Execute(context.Background(), chatID, ToolRunCommand,
		`{"command":`+jsonString(exitCommand("3"))+`}`)
	if err != nil {
		t.Fatalf("a failing command returned an error: %v", err)
	}
	if !strings.Contains(out, "Exit-Code 3") {
		t.Errorf("the exit code is not reported:\n%s", out)
	}
}

// TestAnEmptyOutputSaysSo keeps a silent command from looking like a broken card: `true`
// and `ls` of an empty folder both print nothing, and a result with an empty body reads as a
// result that was lost.
func TestAnEmptyOutputSaysSo(t *testing.T) {
	service, chatID, _ := commandService(t)
	silent := "true"
	if runtime.GOOS == "windows" {
		silent = "ver > NUL"
	}

	out, err := service.Execute(context.Background(), chatID, ToolRunCommand,
		`{"command":`+jsonString(silent)+`}`)
	if err != nil {
		t.Fatalf("run_command: %v", err)
	}
	if !strings.Contains(out, "keine Ausgabe") {
		t.Errorf("a silent command produced:\n%s", out)
	}
}

// TestBothStreamsComeBack is the reason stdout and stderr share one buffer: a build's
// progress and its errors arrive on different streams, and a model given only stdout reads a
// failure as silence.
func TestBothStreamsComeBack(t *testing.T) {
	service, chatID, _ := commandService(t)
	line := "echo auf-stdout && echo auf-stderr 1>&2"
	if runtime.GOOS == "windows" {
		line = "echo auf-stdout & echo auf-stderr 1>&2"
	}

	out, err := service.Execute(context.Background(), chatID, ToolRunCommand, `{"command":`+jsonString(line)+`}`)
	if err != nil {
		t.Fatalf("run_command: %v", err)
	}
	for _, want := range []string{"auf-stdout", "auf-stderr"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q is missing from the result:\n%s", want, out)
		}
	}
}

// TestTheQuestionCarriesTheLineAndItsLimits is the other half of the gate: what the user is
// shown has to be the line that runs, the folder it runs in, the shell that reads it and the
// time it gets. A question that said "run a command" would be consent to nothing.
func TestTheQuestionCarriesTheLineAndItsLimits(t *testing.T) {
	conf := &asked{}
	service, chatID, folder := writeService(t, nil, conf)
	ctx := WithToolCall(WithCallingMessage(context.Background(), "msg-1"), "call-1")

	if _, err := service.Execute(ctx, chatID, ToolRunCommand,
		`{"command":"go test ./...","timeout_seconds":45}`); err != nil {
		t.Fatalf("run_command: %v", err)
	}
	req := conf.request
	if req.Kind != ApprovalKindCommand {
		t.Errorf("kind = %q, want %q", req.Kind, ApprovalKindCommand)
	}
	if req.Command.Line != "go test ./..." {
		t.Errorf("the question carries %q as the line, which is not what would run", req.Command.Line)
	}
	if req.Command.Dir != folder {
		t.Errorf("the question says the command runs in %q, want the project folder %q", req.Command.Dir, folder)
	}
	if req.Command.TimeoutSec != 45 {
		t.Errorf("timeout = %d, want the 45 the model asked for", req.Command.TimeoutSec)
	}
	if req.Command.Shell == "" {
		t.Error("the question does not say which shell interprets the line")
	}
	if req.Tool != ToolRunCommand || req.ToolCallID != "call-1" {
		t.Errorf("the question cannot be matched to its call: %+v", req)
	}
	// A command is not a file change: the request must not carry a diff, or the card would
	// have something to render in both halves.
	if req.Change.Path != "" || req.Change.Operation != "" {
		t.Errorf("a command request carries a file change as well: %+v", req.Change)
	}
}

// TestACommandWithNobodyToAskDoesNotRun is the fail-closed rule for the one tool that can do
// anything: the refusal has to come *before* anything is executed, so a build with no way to
// ask cannot run a line by accident.
func TestACommandWithNobodyToAskDoesNotRun(t *testing.T) {
	service, chatID, folder := writeService(t, nil, nil)
	line := "echo ran > evidence.txt"
	if runtime.GOOS == "windows" {
		line = "echo ran> evidence.txt"
	}

	_, err := service.Execute(context.Background(), chatID, ToolRunCommand, `{"command":`+jsonString(line)+`}`)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("a command with nobody to ask returned %v, want a refusal", err)
	}
	if code := refusalCode(t, err); code != "tools.denied" {
		t.Errorf("refusal code = %q, want tools.denied", code)
	}
	if _, statErr := os.Stat(filepath.Join(folder, "evidence.txt")); !os.IsNotExist(statErr) {
		t.Errorf("the command ran although nobody could confirm it: %v", statErr)
	}
}

// TestACommandNobodyConfirmsDoesNotRun is the same rule with a person behind it, for the one
// tool whose effect cannot be previewed: a declined command must not have run — not even
// partly — and the refusal must not be a fault the model retries.
func TestACommandNobodyConfirmsDoesNotRun(t *testing.T) {
	conf := &asked{answer: errors.New("the user said no")}
	service, chatID, folder := writeService(t, nil, conf)
	line := "echo ran > evidence.txt"
	if runtime.GOOS == "windows" {
		line = "echo ran> evidence.txt"
	}

	_, err := service.Execute(context.Background(), chatID, ToolRunCommand, `{"command":`+jsonString(line)+`}`)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("a declined command returned %v, want a refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(folder, "evidence.txt")); !os.IsNotExist(statErr) {
		t.Errorf("a declined command ran anyway: %v", statErr)
	}
}

// TestACommandThatDoesNotEndIsStoppedAndItsTreeDies is the tree kill, tested through the one
// observable it has: a *grandchild* that writes a file after the shell is gone. A kill aimed
// at the shell alone leaves that subshell alive, and it writes the file a few seconds later —
// which is exactly what a stuck `go test` looks like from the outside.
func TestACommandThatDoesNotEndIsStoppedAndItsTreeDies(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The grandchild here is a POSIX subshell; the Windows half of the tree kill is
		// compiled and vetted by `make check` but this test cannot speak for it.
		t.Skip("the grandchild fixture is a sh subshell")
	}
	service, chatID, folder := commandService(t)
	marker := filepath.Join(folder, "late.txt")
	line := "(sleep 2; echo late > late.txt) & sleep 30"

	start := time.Now()
	out, err := service.Execute(context.Background(), chatID, ToolRunCommand,
		`{"command":`+jsonString(line)+`,"timeout_seconds":1}`)
	if err != nil {
		t.Fatalf("run_command: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the command took %s to stop, want it stopped at its 1 s timeout", elapsed)
	}
	if !strings.Contains(out, "abgebrochen") {
		t.Errorf("the result does not say it was stopped:\n%s", out)
	}
	// The subshell had time to write its file after the kill. It must not exist.
	time.Sleep(2500 * time.Millisecond)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("a grandchild survived the timeout and wrote into the project folder")
	}
}

// TestAStoppedTurnKillsTheCommand is the other end of the same mechanism: the user presses
// Stop, and the command has to die with the turn rather than outlive the chat that asked for
// it. The error is what the record needs — nobody is waiting for the result.
func TestAStoppedTurnKillsTheCommand(t *testing.T) {
	service, chatID, _ := commandService(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		_, err := service.Execute(ctx, chatID, ToolRunCommand,
			`{"command":`+jsonString(sleepCommand(30))+`,"timeout_seconds":60}`)
		done <- err
	}()
	// A moment for the command to actually start, so the cancellation lands on a running
	// process rather than on nothing.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if code := refusalCode(t, err); code != "tools.commandStopped" {
			t.Fatalf("a stopped turn reported %q, want tools.commandStopped", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the command outlived the turn that asked for it")
	}
}

// collector gathers a stream's reports, which arrive on the goroutine copying the
// command's output rather than on the test's.
type collector struct {
	mu      sync.Mutex
	reports []string
}

func (c *collector) report(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reports = append(c.reports, text)
}

func (c *collector) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.reports...)
}

// TestTheLiveViewKeepsTheEndAndIsBounded pins what a card can afford to be handed: the end
// of the output, capped, so a build that prints a hundred thousand lines costs the same
// message as one that prints ten. The end rather than the beginning because that is where a
// build's errors are, and the finished result keeps both ends anyway.
func TestTheLiveViewKeepsTheEndAndIsBounded(t *testing.T) {
	collected := &collector{}
	stream := newCommandStream(collected.report)
	// Every write reports, so this is about the window rather than about the clock.
	stream.interval = 0

	if _, err := stream.Write([]byte(strings.Repeat("a", 2*commandStreamBytes))); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Write([]byte("ENDE")); err != nil {
		t.Fatal(err)
	}
	stream.Close()

	reports := collected.seen()
	if len(reports) < 2 {
		t.Fatalf("%d report(s), want one per write while the command runs: %q", len(reports), reports)
	}
	last := reports[len(reports)-1]
	if len(last) > commandStreamBytes {
		t.Errorf("the live view carries %d bytes, want it capped at %d", len(last), commandStreamBytes)
	}
	if !strings.HasSuffix(last, "aaaaENDE") {
		t.Errorf("the live view does not end with the newest output: %q", last[max(0, len(last)-16):])
	}
}

// TestReportsAreThrottledAndTheLastOneClosesTheGap is the other half: a compiler writes
// thousands of tiny chunks, so writes inside the interval are held, and the command's end
// sends what they held. Without the final flush the last line of a build — usually the one
// that explains the failure — would be missing until the result arrived.
func TestReportsAreThrottledAndTheLastOneClosesTheGap(t *testing.T) {
	collected := &collector{}
	stream := newCommandStream(collected.report)
	stream.interval = time.Hour

	if _, err := stream.Write([]byte("erste Zeile\n")); err != nil {
		t.Fatal(err)
	}
	if got := collected.seen(); len(got) != 1 {
		// The first write is always reported: an empty card would otherwise wait out the
		// first interval before showing anything at all.
		t.Fatalf("the first write reported %d time(s), want once", len(got))
	}
	if _, err := stream.Write([]byte("letzte Zeile\n")); err != nil {
		t.Fatal(err)
	}
	if got := collected.seen(); len(got) != 1 {
		t.Errorf("a second write inside the interval was reported anyway: %d reports", len(got))
	}
	stream.Close()
	reports := collected.seen()
	if len(reports) != 2 {
		t.Fatalf("%d report(s) after Close, want the two writes as two reports", len(reports))
	}
	if !strings.Contains(reports[1], "letzte Zeile") {
		t.Errorf("the closing report does not carry the last output: %q", reports[1])
	}

	// And a silent command sends nothing at all: an empty live view is not output.
	silent := &collector{}
	quiet := newCommandStream(silent.report)
	quiet.interval = 0
	quiet.Close()
	if got := silent.seen(); len(got) != 0 {
		t.Errorf("a command that printed nothing reported %q", got)
	}
}

// TestACommandWithNobodyWatchingStillReportsNothing is the fail-open half: every other test
// in this file runs without a reporter, because a test service has no window — so this pins
// the one thing they cannot show, that a command run that way still behaves and does not
// panic on the absent callback.
func TestACommandWithNobodyWatchingStillReportsNothing(t *testing.T) {
	service, chatID, _ := commandService(t)
	out, err := service.Execute(context.Background(), chatID, ToolRunCommand,
		`{"command":`+jsonString("echo still-fine")+`}`)
	if err != nil {
		t.Fatalf("run_command: %v", err)
	}
	if !strings.Contains(out, "still-fine") {
		t.Errorf("an unwatched command lost its output:\n%s", out)
	}
}

// TestAnElevationCommandIsRefusedBeforeTheQuestion is the whole point of the detector: the
// app cannot answer a password prompt (stdin is closed, there is no terminal), so asking the
// person to allow the line would be asking them to watch it fail. They are not asked, and the
// refusal says what to do instead.
func TestAnElevationCommandIsRefusedBeforeTheQuestion(t *testing.T) {
	conf := &asked{}
	service, chatID, folder := writeService(t, nil, conf)

	_, err := service.Execute(context.Background(), chatID, ToolRunCommand,
		`{"command":`+jsonString("sudo apt-get install -y cowsay")+`}`)
	if code := refusalCode(t, err); code != "tools.needsElevation" {
		t.Fatalf("refusal code = %q, want tools.needsElevation", code)
	}
	if conf.calls != 0 {
		t.Errorf("the user was asked to allow a command the app cannot run: %d question(s)", conf.calls)
	}
	// And the model is told in words it can act on — a code is not something to correct a call
	// from. Naming the program is the part that makes the retry a different call.
	detail := uimsg.Sentence(err)
	if !strings.Contains(detail, "sudo") {
		t.Errorf("the model is not told which program asked for the rights: %q", detail)
	}
	if !strings.Contains(detail, "administrator") {
		t.Errorf("the model is not told why the command cannot run: %q", detail)
	}
	// Nothing ran, so nothing in the folder changed — the refusal is before any execution.
	if entries, readErr := os.ReadDir(folder); readErr == nil && len(entries) != 0 {
		t.Errorf("the refused command left %d entry/entries in the project folder", len(entries))
	}
}

// TestElevationIsDetectedWhereItIsAskedAndNowhereElse pins the two halves that matter: the
// forms a model actually writes are caught (`cd … && sudo …`, an absolute path, `env sudo`,
// an assignment in front), and a line that merely mentions the word is not — a false refusal
// would make `grep sudo README.md` impossible with no way for the user to override it.
func TestElevationIsDetectedWhereItIsAskedAndNowhereElse(t *testing.T) {
	caught := []struct{ line, want string }{
		{"sudo apt install cowsay", "sudo"},
		{"  sudo -u root systemctl restart nginx", "sudo"},
		{"/usr/bin/sudo make install", "sudo"},
		{"cd /tmp && sudo ls", "sudo"},
		{"make && sudo make install", "sudo"},
		{"echo ready | sudo tee /etc/motd", "sudo"},
		{"DEBIAN_FRONTEND=noninteractive sudo apt-get install -y x", "sudo"},
		{"env sudo id", "sudo"},
		{"env -i sudo id", "sudo"},
		{"doas pkg_add curl", "doas"},
		{"pkexec systemctl restart docker", "pkexec"},
		{"su -c 'id'", "su"},
		{"runas /user:Administrator cmd", "runas"},
		{"C:\\Windows\\System32\\runas.exe /user:Admin cmd", "runas"},
		{"SUDO id", "sudo"},
	}
	for _, c := range caught {
		got, ok := elevationRequest(c.line)
		if !ok || got != c.want {
			t.Errorf("elevationRequest(%q) = %q, %v; want %q, true", c.line, got, ok, c.want)
		}
	}

	// Not elevation: the word in an argument, a file name, a different program whose name
	// merely starts the same way, and the empty line.
	allowed := []string{
		"",
		"grep -rn sudo README.md",
		"echo sudo",
		"ls sudoers.d",
		"sudoku --generate",
		"git commit -m 'document sudo'",
		"go test ./...",
	}
	for _, line := range allowed {
		if program, ok := elevationRequest(line); ok {
			t.Errorf("elevationRequest(%q) refused it as %q", line, program)
		}
	}
}

// TestTheTerminalSaysItHasNoAdministratorRights keeps the instruction where the model reads
// it before the call rather than only after a refusal: a description that promised nothing
// about elevation is how a small model keeps proposing `sudo apt install` in a loop.
func TestTheTerminalSaysItHasNoAdministratorRights(t *testing.T) {
	description := commandDefinitions()[0].Description
	for _, want := range []string{"sudo", "Administratorrechte"} {
		if !strings.Contains(description, want) {
			t.Errorf("the tool description does not mention %q:\n%s", want, description)
		}
	}
}

// TestTheTimeoutIsClamped pins the two limits and the default. A model that asks for a
// timeout of zero means "use the default"; one that asks for an hour gets ten minutes,
// because a turn that blocks for longer than that is not a command any more.
func TestTheTimeoutIsClamped(t *testing.T) {
	cases := []struct {
		asked int
		want  time.Duration
	}{
		{0, DefaultCommandTimeout * time.Second},
		{-5, DefaultCommandTimeout * time.Second},
		{30, 30 * time.Second},
		{MaxCommandTimeout + 5000, MaxCommandTimeout * time.Second},
	}
	for _, c := range cases {
		if got := commandTimeout(c.asked); got != c.want {
			t.Errorf("commandTimeout(%d) = %s, want %s", c.asked, got, c.want)
		}
	}
}

// TestTheOutputKeepsBothEndsAndNamesWhatItDropped is the cap, tested where it is
// implemented: through a command's output, the middle is the least useful part, and the
// amount dropped is reported rather than left for the reader to guess at.
func TestTheOutputKeepsBothEndsAndNamesWhatItDropped(t *testing.T) {
	w := newHeadTailWriter(8, 8)
	if _, err := w.Write([]byte("HEAD")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("TAIL")); err != nil {
		t.Fatal(err)
	}
	kept := w.String()
	if !strings.HasPrefix(kept, "HEAD0123") {
		t.Errorf("the beginning was not kept: %q", kept)
	}
	if !strings.HasSuffix(kept, "6789TAIL") {
		t.Errorf("the end was not kept: %q", kept)
	}
	if !strings.Contains(kept, "weggelassen") {
		t.Errorf("the dropped middle is not named: %q", kept)
	}
	if w.omitted() != 2 {
		t.Errorf("omitted = %d, want the 2 bytes (45, 6) that fell out of the middle", w.omitted())
	}

	// Everything that fits is kept whole: a cap that inserted a marker into output that was
	// never truncated would be lying about the command.
	small := newHeadTailWriter(64, 64)
	if _, err := small.Write([]byte("kurz\n")); err != nil {
		t.Fatal(err)
	}
	if got := small.String(); got != "kurz\n" {
		t.Errorf("output that fits came back as %q", got)
	}
}

// TestTheExitCodeIsTheLastThingTheModelReads keeps the result's shape: the status line ends
// it, so a model reading the tail of a long build sees how it ended rather than a line of
// compiler output.
func TestTheExitCodeIsTheLastThingTheModelReads(t *testing.T) {
	w := newHeadTailWriter(64, 64)
	if _, err := w.Write([]byte("viele Zeilen\n")); err != nil {
		t.Fatal(err)
	}
	out := commandResult("go build ./...", w, "Exit-Code 1 (2.0 s)")
	if !strings.HasSuffix(out, "Exit-Code 1 (2.0 s)\n") {
		t.Errorf("the result does not end with its status:\n%q", out)
	}
	if !strings.HasPrefix(out, "$ go build ./...\n") {
		t.Errorf("the result does not begin with the command:\n%q", out)
	}
}

// TestTheTerminalIsGatedAndOffered keeps the three lists in step. The engine advertises only
// what ProjectToolNames names, and asks for confirmation only for what NeedsApproval answers
// yes to — so a terminal missing from either list would be a tool that runs command lines
// without asking anybody.
func TestTheTerminalIsGatedAndOffered(t *testing.T) {
	if !NeedsApproval(ToolRunCommand) {
		t.Error("run_command is not gated, so it would run without a confirmation")
	}
	if !containsString(ProjectToolNames(), ToolRunCommand) {
		t.Error("run_command is missing from the project tools, so no project would be offered it")
	}
	advertised := false
	for _, def := range commandDefinitions() {
		if def.Name == ToolRunCommand {
			advertised = true
		}
	}
	if !advertised {
		t.Error("run_command has no definition, so a request could never call it")
	}
	// And the schema has to declare the one argument that matters, or a model sending it
	// gets a call that is silently ignored.
	def := commandDefinitions()[0]
	props, _ := def.Schema["properties"].(map[string]any)
	if _, ok := props["command"]; !ok {
		t.Error("the schema does not declare the command argument")
	}
	required, _ := def.Schema["required"].([]string)
	if !containsString(required, "command") {
		t.Error("command is not required by the schema")
	}
}

// editingConfirm is a person who corrects the line in the card's field and then allows it.
//
// It writes through the request rather than returning an answer because that is the contract
// the app implements: the field belongs to the answer, and the confirmer is the only one that
// can know what it held.
type editingConfirm struct{ line string }

func (e editingConfirm) confirm(_ context.Context, req *ApprovalRequest) error {
	if req.Kind == ApprovalKindCommand {
		req.EditedLine = e.line
	}
	return nil
}

// TestTheLineTheUserEditsIsWhatRuns is the promise the editable field makes. A person who
// spots a wrong flag in an approval should not have to decline and rebuild the whole call —
// the folder, the timeout and the shell the tool chose all stay, and only the sentence
// changes.
func TestTheLineTheUserEditsIsWhatRuns(t *testing.T) {
	service, chatID, _ := projectService(t, nil)
	service.Confirm = editingConfirm{line: "echo edited"}.confirm

	out, err := service.Execute(context.Background(), chatID, ToolRunCommand,
		`{"command":`+jsonString("echo original")+`}`)
	if err != nil {
		t.Fatalf("the edited command returned an error: %v", err)
	}
	if !strings.Contains(out, "edited") {
		t.Errorf("the edited line did not run:\n%s", out)
	}
	// The model's line must not have run: a field that only *looked* editable would leave the
	// person believing they had corrected something.
	if strings.Contains(out, "original") {
		t.Errorf("the original line ran as well as the edit:\n%s", out)
	}
}

// TestAnEditedLineCannotSlipPastTheElevationRefusal closes the hole the field would otherwise
// open: the refusal exists because this app has no terminal to answer a password prompt, and
// an edit is a line like any other. It is checked after the answer rather than at the field,
// because the answer is where the line is known.
func TestAnEditedLineCannotSlipPastTheElevationRefusal(t *testing.T) {
	service, chatID, _ := projectService(t, nil)
	service.Confirm = editingConfirm{line: "sudo ls"}.confirm

	_, err := service.Execute(context.Background(), chatID, ToolRunCommand,
		`{"command":`+jsonString("echo harmless")+`}`)
	if code := refusalCode(t, err); code != "tools.needsElevation" {
		t.Fatalf("an edited elevation line reported %q, want tools.needsElevation", code)
	}
}

// TestARunningCommandCanBeStoppedWithoutEndingTheTurn pins the difference between the two
// stops this app has. The turn's Stop abandons the answer; this one ends a single command and
// reports it *as a result*, so the model can read what the command printed up to that point
// and decide what to do instead of retrying it.
func TestARunningCommandCanBeStoppedWithoutEndingTheTurn(t *testing.T) {
	service, chatID, _ := commandService(t)
	ctx := WithToolCall(context.Background(), "call-stop")
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := service.ExecuteWithSummary(ctx, chatID, ToolRunCommand,
			`{"command":`+jsonString(sleepCommand(30))+`,"timeout_seconds":60}`)
		done <- outcome{result: result, err: err}
	}()

	// The command registers itself when it starts, so a stop that lands early is a stop with
	// nothing to end — the loop waits for the registration rather than sleeping a guess.
	started := time.Now()
	stopped := false
	for time.Since(started) < 10*time.Second {
		if service.StopCommand(chatID, "call-stop") {
			stopped = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("a running command was not registered, so it could not be stopped")
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("a command stopped by the user returned an error: %v", got.err)
		}
		if !strings.Contains(got.result.Text, "gestoppt") {
			t.Errorf("the result does not say the person stopped it:\n%s", got.result.Text)
		}
		if !strings.Contains(got.result.Text, "unvollständig") {
			t.Errorf("the model is not told the output is incomplete:\n%s", got.result.Text)
		}
		if elapsed := time.Since(started); elapsed > 15*time.Second {
			t.Errorf("the stop took %s; the sleep was 30s, so the tree was not killed", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the stopped command never returned")
	}

	// And the registration is gone, so a second click is a no-op rather than a stale handle.
	if service.StopCommand(chatID, "call-stop") {
		t.Error("a finished command is still registered as running")
	}
}
