package tools

// The terminal: one line, run in the project folder, behind the same confirmation every
// other change needs.
//
// Four decisions shape this file, and each is a place where a friendlier-looking choice
// would be the wrong one.
//
// **A failing command is a result, not a failure.** `go test ./...` exits 1 and prints why;
// that is the tool working, and the exit code and the output are what the model needs to do
// anything about it. Only the app's own troubles are errors — no folder, no confirmation, a
// killed turn — so a red card means something actually went wrong rather than "the test
// failed", which is the reading a user stops noticing.
//
// **The confirmation says what will run, not that something will run.** The card shows the
// line, the folder and the timeout, because a shell command is the one tool whose effect
// cannot be previewed: `rm -rf build` is its own description. That is also why this is not
// sandboxed and does not pretend to be — the process runs with the user's own rights and can
// leave the folder. The gate is a person reading the line, and everything here is arranged
// around that being possible: one line, shown before it runs, per call.
//
// **Output is capped at both ends.** A build prints what it is doing first and what went
// wrong last, so keeping only one end loses either the beginning of the story or its
// conclusion. What is dropped is counted and named in the result.
//
// **A command that does not end is stopped.** A wall-clock timeout sizes every call, and
// what the timeout kills is the whole process tree: `go test` is a parent of test binaries,
// `make` is a parent of compilers, and killing only the shell leaves those writing to a
// folder and holding the pipes this call is waiting on.

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"kichat/internal/llm"
	"kichat/internal/proc"
	"kichat/internal/uimsg"
	"kichat/internal/workspace"
)

// ToolRunCommand is the terminal tool.
const ToolRunCommand = "run_command"

// Limits on one call.
const (
	// DefaultCommandTimeout is how long a command gets when the model names no timeout. It
	// is sized for a test suite or a build on a slow machine rather than for a quick `ls`:
	// a timeout that is too short is worse than one that is too long, because it turns a
	// working command into a mystery, while a long wait is at least visible.
	DefaultCommandTimeout = 120
	// MaxCommandTimeout bounds what the model may ask for. Ten minutes covers a full build
	// of a large project; beyond that it is not a command, it is a service, and a service
	// belongs in a terminal the user owns rather than in a turn that blocks on it.
	MaxCommandTimeout = 600
	// commandHeadBytes and commandTailBytes are how much of each end is kept.
	commandHeadBytes = 32 << 10
	commandTailBytes = 32 << 10
)

// How a running command reports itself.
const (
	// commandStreamInterval is the shortest gap between two reports. A compiler loop
	// writes thousands of tiny chunks, and one report per chunk would be more traffic
	// than the build is worth while the card redraws faster than anyone can read it.
	commandStreamInterval = 150 * time.Millisecond
	// commandStreamBytes is how much of the output the live view carries: the *end* of
	// it, which is where a build's errors arrive. The finished result keeps both ends
	// (commandHeadBytes/commandTailBytes); this is only what the card shows while the
	// command is still running.
	commandStreamBytes = 8 << 10
)

// Command is a command waiting for a person's answer.
//
// It carries the three things the card has to show for the question to be answerable at
// all: the exact line, where it runs, and how long it may take. A question that said
// "run a command in this folder" would be asking for consent to nothing in particular.
type Command struct {
	// Line is exactly what the shell will be given.
	Line string
	// Dir is where it runs: the project root, always — a command that wants another
	// directory says so itself, in the line the user reads.
	Dir string
	// TimeoutSec is how long it may take before it is stopped.
	TimeoutSec int
	// Shell names the program that interprets the line, for the card and the result.
	Shell string
}

// elevationPrograms are the programs whose whole purpose is to ask the operating system for
// another account's rights. They are refused before the line reaches the person at the
// window, for the reason in elevationRequest.
var elevationPrograms = map[string]bool{
	"sudo":     true,
	"doas":     true,
	"pkexec":   true,
	"su":       true,
	"runas":    true,
	"gsudo":    true,
	"sudoedit": true,
}

// elevationRequest reports the program on this line that would ask for administrator rights.
//
// The app runs every command with the user's own rights, with stdin closed and no terminal,
// so an elevation prompt cannot be answered: sudo, doas and pkexec fail at the prompt, and on
// Windows the UAC window appears outside the app entirely. Left alone that failure arrives
// *after* the person has read and allowed the line — a card, a click, and then a refusal they
// could have been told about first. So the line is refused here instead, with a sentence that
// names the way forward.
//
// What this is, and is not: a hint about what a line asks for, never a security boundary.
// `env sudo`, `sh -c "sudo …"` and a Makefile target that runs sudo all get past it, and that
// is acceptable because the boundary is elsewhere — the process has no terminal to prompt in.
// The parser exists to save a useless click and to give the model an instruction, so it looks
// at simple commands as a shell starts them rather than trying to be one. A command that only
// *mentions* elevation (`grep sudo README.md`, `echo sudo`) is not one.
func elevationRequest(line string) (string, bool) {
	// Split where a shell starts a new simple command. Parentheses and redirections are left
	// alone on purpose: this reads the line the way the common cases are written.
	segments := strings.FieldsFunc(line, func(r rune) bool {
		switch r {
		case ';', '&', '|', '\n':
			return true
		}
		return false
	})
	for _, segment := range segments {
		words := strings.Fields(segment)
		// Leading assignments are not the command: `DEBIAN_FRONTEND=noninteractive sudo …`
		// asks for elevation just as plainly as the bare line.
		for len(words) > 0 && isAssignment(words[0]) {
			words = words[1:]
		}
		// `env sudo …` names the program one word later, and `env -i sudo …` two.
		if len(words) > 0 && strings.ToLower(words[0]) == "env" {
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				words = words[1:]
			}
		}
		if len(words) == 0 {
			continue
		}
		if program := commandWord(words[0]); elevationPrograms[program] {
			return program, true
		}
	}
	return "", false
}

// isAssignment reports whether a word is a leading `NAME=value` of a command, as opposed to
// the command itself — which is why everything after the first `=` has to be ignored.
func isAssignment(word string) bool {
	name, _, ok := strings.Cut(word, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// commandWord is the program a word names: no directory, no `.exe`, lower case. The
// separators are both kinds rather than filepath's, so a Windows path is read the same way
// when the test that reads it runs on Linux.
func commandWord(word string) string {
	word = strings.Trim(word, "'\"")
	if i := strings.LastIndexAny(word, `/\`); i >= 0 {
		word = word[i+1:]
	}
	return strings.ToLower(strings.TrimSuffix(word, ".exe"))
}

// runCommandArgs is what the model may send. timeout_seconds is `any` for the reason every
// numeric argument in this package is: a small model writes "120" as readily as 120.
type runCommandArgs struct {
	Command string `json:"command"`
	Timeout any    `json:"timeout_seconds"`
}

// commandDefinitions advertises the terminal.
//
// The description carries the four rules a model cannot guess from the schema: a non-zero
// exit code is normal and reported, there is no stdin, a server would be killed, and the
// user confirms first. Without the first one a model retries a failing test instead of
// reading it; without the second it writes an interactive command and loses two minutes;
// without the third it starts a development server; and the fourth is what keeps it from
// asking for a command it should have run after explaining.
func commandDefinitions(lang string) []llm.ToolDefinition {
	return []llm.ToolDefinition{
		{
			Name: ToolRunCommand,
			Description: text(lang,
				"Führt eine Kommandozeile im Projektordner aus und gibt die Ausgabe samt Exit-Code "+
					"zurück. Nutze es, um den eigenen Code zu prüfen: Build, Tests, Linter, Formatierung, "+
					"oder ein Programm, das du gerade geschrieben hast — der Befehl läuft im Projektordner, "+
					"also mit denselben relativen Pfaden wie deine Datei-Tools. Ein Exit-Code ungleich 0 ist "+
					"ein Ergebnis, kein Fehler: lies die Ausgabe und arbeite damit. Es gibt keine Eingabe "+
					"(die Standardeingabe ist geschlossen) und keine Administratorrechte — Befehle mit sudo, "+
					"doas oder pkexec werden abgelehnt; schlage sie dem Nutzer vor, statt sie zu wiederholen. "+
					"Befehle, die nicht committete Arbeit verwerfen (git reset --hard, git checkout --, git "+
					"restore, git clean -f), werden ebenso abgelehnt — committe oder stashe die Änderungen "+
					"zuerst. "+
					"Schreibe keine Befehle, die eine Frage stellen, und starte keinen Server oder Beobachter, "+
					"der weiterläuft: er wird nach dem Timeout abgebrochen. Eine Pipe wie `| tail` gibt erst "+
					"am Ende aus, und bei einem Abbruch ist ihre Ausgabe verloren — prüfe lieber enger "+
					"(`-run`, ein Paket) und lass das Programm sein eigenes Timeout setzen. Dateien, die du ändern willst, "+
					"änderst du mit edit_file/write_file, nicht über die Shell. Der Nutzer sieht den Befehl "+
					"und muss ihn bestätigen.",
				"Runs a command line in the project folder and returns its output and exit code. Use it "+
					"to check your own code: build, tests, linter, formatting, or a program you just wrote — "+
					"the command runs in the project folder, so with the same relative paths as your file "+
					"tools. An exit code other than 0 is a result, not a failure: read the output and work "+
					"with it. There is no input (stdin is closed) and no administrator rights — commands "+
					"with sudo, doas or pkexec are refused; suggest them to the user instead of repeating "+
					"them. Commands that discard uncommitted work (git reset --hard, git checkout --, git "+
					"restore, git clean -f) are refused as well — commit or stash the changes first. Do not "+
					"write commands that ask a question, and do not start a server or watcher that keeps "+
					"running: it is stopped at the timeout. A pipe like `| tail` only writes when the "+
					"command ends, and after a stop its output is lost — narrow the scope instead (`-run`, "+
					"one package) and let the program set its own timeout. Files you want to change you "+
					"change with edit_file/write_file, not through the shell. The user sees the command "+
					"and has to confirm it."),
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type": "string",
						"description": text(lang,
							"Eine Kommandozeile, z. B. \\\"go test ./...\\\" oder \\\"npm run build\\\". Sie wird von "+
								"einer Shell ausgeführt, also sind Pipes und && erlaubt.",
							"One command line, e.g. \\\"go test ./...\\\" or \\\"npm run build\\\". It is run by a "+
								"shell, so pipes and && are allowed."),
					},
					"timeout_seconds": map[string]any{
						"type": "integer",
						"description": fmt.Sprintf(text(lang,
							"Höchstdauer in Sekunden (1-%d, Standard %d). Setze einen höheren Wert für einen "+
								"vollständigen Build, einen niedrigeren, wenn du einen Verdacht schnell prüfen willst.",
							"Maximum duration in seconds (1-%d, default %d). Set a higher value for a full build, "+
								"a lower one to check a suspicion quickly."), MaxCommandTimeout, DefaultCommandTimeout),
					},
				},
				"required": []string{"command"},
			},
		},
	}
}

func (s *Service) runCommand(ctx context.Context, chatID, argsJSON string) (Result, error) {
	var args runCommandArgs
	if err := decodeLoose(argsJSON, &args); err != nil {
		return Result{}, s.badArguments(err)
	}
	lang := s.Language()
	line := strings.TrimSpace(args.Command)
	if line == "" {
		return Result{}, uimsg.Detailf("tools.commandMissing", text(lang,
			"das Argument command ist leer; nenne eine Kommandozeile",
			"the command argument is empty; name one command line to run"))
	}
	// Before the folder is even looked at: whether this machine can run the line at all does
	// not depend on the project, and a question the app cannot act on should not be asked.
	// The line stays on the card either way, which is where the person copies it from.
	if program, ok := elevationRequest(line); ok {
		return Result{}, uimsg.Detailf("tools.needsElevation",
			fmt.Sprintf(text(lang,
				"dieser Befehl verlangt Administratorrechte (%s), die diese App nicht geben kann: sie "+
					"führt Befehle mit den Rechten des Nutzers aus und hat kein Terminal, um eine "+
					"Passwortabfrage zu beantworten. Wiederhole ihn nicht — nenne dem Nutzer die genaue "+
					"Zeile, damit er sie selbst ausführen kann, oder schlage einen Weg ohne "+
					"Rechteerweiterung vor",
				"this command asks for administrator rights (%s), which this app cannot supply: it "+
					"runs commands with the user's own rights and has no terminal to answer a password "+
					"prompt. Do not retry it — tell the user the exact line so they can run it themselves, "+
					"or suggest a way that needs no elevation"), program))
	}
	root, err := s.workspaceFor(chatID)
	if err != nil {
		return Result{}, err
	}
	timeout := commandTimeout(looseInt(args.Timeout, 0))
	// Before the card, for the reason the elevation refusal is: the person should not be asked
	// to approve a line the app already knows it will not run. Nothing is snapshotted — the undo
	// record is per file and keeps the chat's first pre-image, so it cannot represent "the
	// working tree just before this command" (gitguard.go has the argument) — which makes the
	// promise the narrow one: the work is not lost, or the line does not run.
	if err := refuseDestructiveGit(lang, root, line); err != nil {
		return Result{}, err
	}
	// The line can come back from the card edited: a person who spots a typo in an approval
	// should be able to fix it there instead of declining and typing it again in the composer.
	// The edited line is a *new* line, so the elevation check runs on it too — the card is a
	// text field, and the one thing an edit must not do is slip past the refusal below.
	request := ApprovalRequest{
		ChatID:     chatID,
		MessageID:  callingMessage(ctx),
		ToolCallID: toolCallFrom(ctx),
		Tool:       ToolRunCommand,
		Kind:       ApprovalKindCommand,
		Command: Command{
			Line:       line,
			Dir:        root.Path(),
			TimeoutSec: int(timeout / time.Second),
			Shell:      shellLabel(),
		},
	}
	if err := s.approve(ctx, &request); err != nil {
		return Result{}, err
	}
	if request.EditedLine != "" && request.EditedLine != line {
		if program, ok := elevationRequest(request.EditedLine); ok {
			return Result{}, uimsg.Detailf("tools.needsElevation",
				fmt.Sprintf(text(lang,
					"der Befehl, den du bestätigt hast, verlangt Administratorrechte (%s), die diese App "+
						"nicht geben kann: sie führt Befehle mit den Rechten des Nutzers aus und hat kein "+
						"Terminal, um eine Passwortabfrage zu beantworten. Wiederhole ihn nicht — nenne dem "+
						"Nutzer die genaue Zeile, damit er sie selbst ausführen kann, oder schlage einen Weg "+
						"ohne Rechteerweiterung vor",
					"the command you approved asks for administrator rights (%s), which this app "+
						"cannot supply: it runs commands with the user's own rights and has no terminal to "+
						"answer a password prompt. Do not retry it — tell the user the exact line so they can "+
						"run it themselves, or suggest a way that needs no elevation"), program))
		}
		line = request.EditedLine
		// The edited line is a new line, so it is checked like one: the card is a text field,
		// and the one thing an edit must not do is turn a harmless command into a destructive one.
		if err := refuseDestructiveGit(lang, root, line); err != nil {
			return Result{}, err
		}
	}
	// A command is the one tool whose effect cannot be enumerated — `gofmt -w .` rewrites
	// every file under a folder — so the read cache is dropped for the whole chat, whatever
	// the line turned out to do.
	defer s.forgetAllReads(chatID)
	return s.execCommand(ctx, chatID, root, line, timeout)
}

// commandTimeout clamps what the model asked for into the two limits, and turns "no
// argument" into the default rather than into zero — a zero timeout would kill every
// command in the millisecond it started, which reads as the tool being broken.
func commandTimeout(asked int) time.Duration {
	switch {
	case asked <= 0:
		asked = DefaultCommandTimeout
	case asked > MaxCommandTimeout:
		asked = MaxCommandTimeout
	}
	return time.Duration(asked) * time.Second
}

// commandKey is one running command: the chat it belongs to and the call that started it.
// Both halves are needed because two chats can run the same line at the same time.
type commandKey struct {
	chatID string
	callID string
}

// commandHandle is a command somebody may still stop.
//
// byUser is the bit that makes a stop a *result* rather than a fault: the command was killed,
// but the turn is alive and the model should be told the person cut it short rather than that
// the app failed. Without it, cancelling the context is indistinguishable from the user
// stopping the whole turn, which ends the answer.
type commandHandle struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	byUser bool
}

func (h *commandHandle) stop() {
	h.mu.Lock()
	h.byUser = true
	h.mu.Unlock()
	h.cancel()
}

func (h *commandHandle) stoppedByUser() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.byUser
}

// StopCommand ends one running command and reports whether there was one to end.
//
// It is the per-call half of stopping: the turn's own Stop cancels everything and abandons the
// answer, while this kills one command and lets the model read the output so far and decide
// what next. The tree is ended, not the shell alone (proc.KillTree), because that is what
// stops the work.
func (s *Service) StopCommand(chatID, toolCallID string) bool {
	s.runningMu.Lock()
	handle := s.running[commandKey{chatID: chatID, callID: toolCallID}]
	s.runningMu.Unlock()
	if handle == nil {
		return false
	}
	handle.stop()
	return true
}

// beginCommand registers a running command and returns the context a stop cancels.
func (s *Service) beginCommand(ctx context.Context, key commandKey) (context.Context, *commandHandle) {
	callCtx, cancel := context.WithCancel(ctx)
	handle := &commandHandle{cancel: cancel}
	s.runningMu.Lock()
	if s.running == nil {
		s.running = make(map[commandKey]*commandHandle)
	}
	s.running[key] = handle
	s.runningMu.Unlock()
	return callCtx, handle
}

// endCommand forgets a command that is no longer running.
func (s *Service) endCommand(key commandKey) {
	s.runningMu.Lock()
	delete(s.running, key)
	s.runningMu.Unlock()
}

// execCommand runs one line in the project folder and renders the result.
func (s *Service) execCommand(ctx context.Context, chatID string, root workspace.Root, line string, timeout time.Duration) (Result, error) {
	// A context of its own, so the window's stop can end this command without ending the turn
	// it belongs to. The turn's cancellation is still the parent: stopping the turn stops it.
	key := commandKey{chatID: chatID, callID: toolCallFrom(ctx)}
	ctx, handle := s.beginCommand(ctx, key)
	defer s.endCommand(key)

	program, argv := shellCommand(line)
	lang := s.Language()
	cmd := exec.Command(program, argv...)
	cmd.Dir = root.Path()
	// No stdin. A command that stops to ask a question would otherwise wait for the
	// timeout doing nothing, and the failure it gets instead is immediate and readable:
	// an EOF at the prompt, which is `git commit` without -m complaining rather than a
	// two-minute silence.
	cmd.Stdin = nil
	out := newHeadTailWriter(commandHeadBytes, commandTailBytes)
	// The same output also goes to the window while the command runs, when somebody asked
	// to watch it. One writer for both streams, so stdout and stderr stay interleaved in
	// the live view exactly as they are in the result.
	var live *commandStream
	var sink io.Writer = out
	if report, ok := ctx.Value(commandOutputKey{}).(func(string)); ok && report != nil {
		live = newCommandStream(report)
		sink = io.MultiWriter(out, live)
	}
	// One stream for both: a build's errors arrive on stderr and its progress on stdout,
	// and a person reading the result wants them interleaved the way a terminal shows
	// them, not in two piles a model has to correlate by itself.
	cmd.Stdout = sink
	cmd.Stderr = sink
	proc.StartInOwnGroup(cmd)
	// WaitDelay bounds the wait *after* the process is gone. A grandchild that inherited
	// these pipes keeps Wait from returning, and without this the timeout would be a
	// request rather than a limit. It is the backstop behind proc.KillTree, not a substitute:
	// the tree kill is what stops the work.
	cmd.WaitDelay = 5 * time.Second

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, uimsg.Detailf("tools.commandFailed",
			fmt.Sprintf(text(lang, "der Befehl ließ sich nicht starten: %v",
				"the command could not be started: %v"), err))
	}

	// The timeout and the turn's own cancellation both end the *tree*: killing the shell
	// alone leaves test binaries and compilers running, and leaves the pipes they hold
	// open, which is what a stuck call looks like from here.
	//
	// Both reasons go through one gate, because the two can race — a timeout firing in the
	// same millisecond as the user's Stop — and the gate is what keeps the kill to one and,
	// more importantly, what stops it happening after the command has already come back:
	// the group is addressed by pid, and a pid that has been reaped can by then belong to
	// somebody else's process.
	var mu sync.Mutex
	killed, finished, timedOut := false, false, false
	end := func(timedOutCall bool) {
		mu.Lock()
		if finished || killed {
			mu.Unlock()
			return
		}
		killed = true
		if timedOutCall {
			timedOut = true
		}
		mu.Unlock()
		_ = proc.KillTree(cmd)
	}
	timer := time.AfterFunc(timeout, func() { end(true) })
	// The turn being stopped (the user pressed Stop) is the other end: the context is
	// cancelled and the command has to die with it rather than outlive the chat.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			end(false)
		case <-watchDone:
		}
	}()

	waitErr := cmd.Wait()
	timer.Stop()
	mu.Lock()
	finished = true
	wasTimedOut := timedOut
	mu.Unlock()
	close(watchDone)
	elapsed := time.Since(started)
	// The last report goes out before the result does: the live view is what the card
	// shows until the finished record replaces it, and output arriving after that would
	// land on a card that already holds the same text.
	if live != nil {
		live.Close()
	}

	switch {
	case ctx.Err() != nil && handle.stoppedByUser():
		// The person stopped this command, not the turn: the output so far is the result,
		// and the model is told what happened so it can decide rather than retry blindly.
		return summarized(commandResult(lang, line, out, fmt.Sprintf(text(lang,
			"Vom Nutzer nach %s gestoppt. Die Ausgabe oben ist unvollständig — frage nach, was als "+
				"Nächstes passieren soll, statt denselben Befehl erneut zu starten.",
			"Stopped by the user after %s. The output above is incomplete — ask what should happen "+
				"next instead of starting the same command again."), humanDuration(elapsed))),
			"tool.sum.commandStopped", humanDuration(elapsed)), nil
	case ctx.Err() != nil:
		// The turn is over, so nobody is waiting for this result; the error is what the
		// record needs to say why there is no output above it.
		return Result{}, uimsg.Wrapf("tools.commandStopped", text(lang,
			"der Turn wurde gestoppt, deshalb wurde der Befehl beendet",
			"the turn was stopped, so the command was killed"), ctx.Err())
	case wasTimedOut:
		return summarized(commandResult(lang, line, out, timeoutNote(lang, line, out, elapsed, timeout)),
			"tool.sum.commandTimeout", humanDuration(elapsed)), nil
	case waitErr != nil:
		// A non-zero exit, which is a result: the code is what the model reasons about,
		// and the output above it is the reason.
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		if code < 0 {
			return summarized(commandResult(lang, line, out, fmt.Sprintf(text(lang,
				"Der Befehl wurde beendet, ohne einen Exit-Code zu hinterlassen (%s).",
				"The command ended without leaving an exit code (%s)."), humanDuration(elapsed))),
				"tool.sum.commandUnknown", humanDuration(elapsed)), nil
		}
		return summarized(commandResult(lang, line, out, fmt.Sprintf(text(lang,
			"Exit-Code %d (%s)", "Exit code %d (%s)"), code, humanDuration(elapsed))),
			"tool.sum.command", fmt.Sprint(code), humanDuration(elapsed)), nil
	default:
		return summarized(commandResult(lang, line, out, fmt.Sprintf(text(lang,
			"Exit-Code 0 (%s)", "Exit code 0 (%s)"), humanDuration(elapsed))),
			"tool.sum.command", "0", humanDuration(elapsed)), nil
	}
}

// timeoutNote is what the model reads when the call's own limit stopped the command.
//
// It is written as three cases rather than one, because "(keine Ausgabe)" beside "the output
// above is incomplete" reads as a silent success — the worst of the readings available to a
// model — and because an empty result has more than one cause.
//
// A line that hands its output to something else produces nothing here while it runs: `… | tail
// -40` keeps the last lines in memory until its *input* ends, and the kill means it never does,
// so a build that printed thousands of lines arrives empty. A redirect (`… > build.log`) writes
// into that file instead of into this result. Neither is the command being quiet, and the way
// out differs from the third case, which is a line with neither: a compile that had not reached
// its first line yet. Telling *that* model to "drop the pipe" sends it to fix something it never
// wrote, so the sentence has to follow what the line actually does.
//
// The detection is a hint, not a boundary (buffersOutput): the shell is the parser and this is
// not it.
func timeoutNote(lang, line string, out *headTailWriter, elapsed, limit time.Duration) string {
	when := fmt.Sprintf(text(lang,
		"Nach %s abgebrochen: das Zeitlimit dieses Aufrufs (%s) war erreicht.",
		"Stopped after %s: this call's time limit (%s) was reached."),
		humanDuration(elapsed), humanDuration(limit))
	if strings.TrimSpace(out.String()) != "" {
		return when + text(lang,
			" Die Ausgabe oben ist unvollständig. Ein erneuter Lauf mit derselben Zeile kostet nur "+
				"wieder die volle Zeit — grenze den Umfang ein (ein Paket, ein Test, `-run`) und lass "+
				"das Programm sein eigenes Limit setzen (bei `go test` z. B. `-timeout 60s`), damit es "+
				"vorher ausgibt, wo es hängt.",
			" The output above is incomplete. Repeating the same line only costs the full time again "+
				"— narrow the scope (one package, one test, `-run`) and let the program set its own "+
				"limit (for `go test`, e.g. `-timeout 60s`), so it prints where it hangs before the cut.")
	}
	if buffersOutput(line) {
		return when + text(lang,
			" Es wurde keine Ausgabe aufgefangen — das heißt nicht, dass der Befehl still war: eine "+
				"Pipe wie `| tail`, `| head` oder `| sort` schreibt erst, wenn der Befehl endet, und "+
				"dieser endete nicht; eine Umleitung wie `> datei` schreibt in die Datei statt in "+
				"dieses Ergebnis. Führe die Zeile ohne Pipe und ohne Umleitung aus, grenze den "+
				"Umfang ein, und lass das Programm sein eigenes Limit setzen (bei `go test` z. B. "+
				"`-timeout 60s`), damit es vorher ausgibt, wo es hängt.",
			" No output was captured — which does not mean the command was silent: a pipe like "+
				"`| tail`, `| head` or `| sort` only writes when the command ends, and this one did "+
				"not; a redirect like `> file` writes into that file instead of into this result. Run "+
				"the line without the pipe and without the redirect, narrow the scope, and let the "+
				"program set its own limit (for `go test`, e.g. `-timeout 60s`), so it prints where it "+
				"hangs before the cut.")
	}
	return when + text(lang,
		" Es wurde bis zum Abbruch nichts ausgegeben, und die Zeile hat weder eine Pipe noch eine "+
			"Umleitung — die Ausgabe kann also nicht unterwegs hängen geblieben sein: der Befehl "+
			"rechnete noch, bevor er etwas sagen konnte. Grenze den Umfang ein (ein Paket, ein "+
			"Test, `-run`) und lass das Programm sein eigenes Limit setzen (bei `go test` z. B. "+
			"`-timeout 60s`), damit es vorher ausgibt, wo es hängt.",
		" Nothing was printed before the cut, and the line has neither a pipe nor a redirect — so "+
			"the output cannot have been held back on the way here: the command was still working "+
			"before it could say anything. Narrow the scope (one package, one test, `-run`) and let "+
			"the program set its own limit (for `go test`, e.g. `-timeout 60s`), so it prints where "+
			"it hangs before the cut.")
}

// outputHandoff strips the two shell spellings that look like a handoff but are not one.
var outputHandoff = strings.NewReplacer("||", " ", "2>&1", " ", ">&1", " ")

// buffersOutput reports whether a line hands its output to something that decides when it
// arrives: a pipe, or a redirect into a file.
//
// A hint for one sentence rather than a parse of the shell. `||` is the shell's "or", and
// `2>&1` points stderr at the stream this app already captures — a line that carries only that
// prints into the result like any other.
func buffersOutput(line string) bool {
	return strings.ContainsAny(outputHandoff.Replace(line), "|>")
}

// commandResult is what the model reads: what ran, what it printed, and how it ended.
//
// The line is repeated because the result is replayed into later turns on its own — by the
// time a model reads this it may be several rounds past the call, and "Exit-Code 1" without
// the command above it says nothing.
func commandResult(lang, line string, out *headTailWriter, status string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "$ %s\n", line)
	body := out.String()
	if strings.TrimSpace(body) == "" {
		sb.WriteString(text(lang, "(keine Ausgabe)\n", "(no output)\n"))
	} else {
		sb.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			sb.WriteString("\n")
		}
	}
	sb.WriteString(status)
	sb.WriteString("\n")
	return sb.String()
}

// humanDuration renders an elapsed time the way a terminal listing should: never as a
// number of nanoseconds, which is what the raw Duration would print.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1f s", d.Seconds())
	default:
		return fmt.Sprintf("%d min %.0f s", int(d.Minutes()), d.Seconds()-float64(int(d.Minutes()))*60)
	}
}

// commandOutputKey carries the reporter a running command hands its output to.
type commandOutputKey struct{}

// WithCommandOutput attaches the reporter that receives a running command's output as it
// arrives, so the window can show a build that is still building.
//
// It travels in the context for the reason the calling message id does: the service is one
// value for the whole app and two chats can run turns at once, so a field on it would be a
// race rather than a setting. Absent means nobody is watching, which is what a service
// exercised without an engine sees — and then the command simply reports nothing, because
// a command must not be slower or noisier for the sake of a card nobody renders.
func WithCommandOutput(ctx context.Context, report func(text string)) context.Context {
	return context.WithValue(ctx, commandOutputKey{}, report)
}

// commandStream reports a command's output while it runs.
//
// The alternative — the behaviour this replaced — is that nothing is shown until the
// command ends, so `go build ./...` on a cold cache looks exactly like a hang: the card
// says "running", the phase line says "tool", and two minutes pass. A person watching a
// command is the whole reason the terminal is in the app rather than in a chat message.
//
// What travels is the *end* of the output so far rather than a delta. A card can then
// replace one string instead of accumulating one, and the size of every report is bounded
// here rather than by a cap in the window; the end is also the half worth watching, because
// that is where a build's errors arrive. Reports are throttled: a compiler writes thousands
// of small chunks and one message per chunk would cost more than the build.
type commandStream struct {
	mu   sync.Mutex
	tail []byte
	// report is the sink; nil means the stream was never given one.
	report func(text string)
	last   time.Time
	// pending is whether output arrived since the last report, so the final one is only
	// sent when there is something it would add.
	pending bool
	// interval is the throttling gap. A field rather than the constant alone, so a test
	// can report every write instead of waiting out a clock.
	interval time.Duration
}

func newCommandStream(report func(string)) *commandStream {
	return &commandStream{
		report:   report,
		tail:     make([]byte, 0, commandStreamBytes),
		interval: commandStreamInterval,
	}
}

// Write keeps the end of what the command printed and reports it when the interval has
// passed. It never blocks the command's own output and never fails: a log that is being
// dropped must not stall the process that is writing it.
func (s *commandStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.report == nil {
		return len(p), nil
	}
	s.tail = append(s.tail, p...)
	if len(s.tail) > commandStreamBytes {
		s.tail = append(s.tail[:0], s.tail[len(s.tail)-commandStreamBytes:]...)
	}
	if time.Since(s.last) >= s.interval {
		s.flush()
		return len(p), nil
	}
	s.pending = true
	return len(p), nil
}

// Close sends what arrived after the last report, so the card's live view ends on the same
// text the result begins from rather than on whatever the last tick happened to catch. It
// is called before the result travels, so nothing arrives after the finished card.
func (s *commandStream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.report != nil && s.pending {
		s.flush()
	}
}

// flush sends the tail as it stands. The caller holds the lock.
//
// Invalid UTF-8 is replaced rather than passed through, for the reason the result's own
// buffer does it: a compiler's mangled bytes would otherwise break the payload the event
// travels in, and the card would render a broken string.
func (s *commandStream) flush() {
	s.last = time.Now()
	s.pending = false
	text := strings.ToValidUTF8(string(s.tail), "\uFFFD")
	s.report(text)
}

// headTailWriter keeps the beginning of a command's output and a window of its end,
// counting everything it saw.
//
// The alternative — one buffer that stops growing at a limit — keeps whichever end arrives
// first, and for a build that is the least useful end: the last error is the one being
// looked for. Both ends are kept here, the middle is dropped, and the amount dropped is
// reported rather than guessed at.
type headTailWriter struct {
	mu   sync.Mutex
	head []byte
	tail []byte
	// headMax and tailMax are the two budgets, fixed at construction.
	headMax int
	tailMax int
	// total is every byte written, including the part that was dropped.
	total int64
}

func newHeadTailWriter(headMax, tailMax int) *headTailWriter {
	return &headTailWriter{
		head:    make([]byte, 0, headMax),
		tail:    make([]byte, 0, tailMax),
		headMax: headMax,
		tailMax: tailMax,
	}
}

// Write never fails and never reports a short write: a command whose output is being
// dropped is still a command that must not be blocked by its own log. Everything offered
// is accepted, and what could not be kept is counted.
func (w *headTailWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total += int64(n)
	if room := w.headMax - len(w.head); room > 0 {
		if n <= room {
			w.head = append(w.head, p...)
			return n, nil
		}
		w.head = append(w.head, p[:room]...)
		p = p[room:]
	}
	// The tail is a window, not an append: when it is full the oldest bytes go, which for
	// chunked output is the right end to lose. The cost is a copy bounded by the tail
	// budget, which is why the budget is kilobytes rather than megabytes.
	w.tail = append(w.tail, p...)
	if len(w.tail) > w.tailMax {
		w.tail = append(w.tail[:0], w.tail[len(w.tail)-w.tailMax:]...)
	}
	return n, nil
}

// String is everything that was kept, with the dropped middle named where it was dropped.
//
// The marker sits *between* the two halves rather than after them, because that is where
// the missing part is: a note at the end would read as "the output was long" instead of
// "you are looking at two pieces of one log". Invalid UTF-8 is replaced rather than passed
// through — a compiler's mangled bytes would otherwise break the JSON the result travels
// in, and the window would show a broken card.
func (w *headTailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	head := strings.ToValidUTF8(string(w.head), "\uFFFD")
	tail := strings.ToValidUTF8(string(w.tail), "\uFFFD")
	if omitted := w.omitted(); omitted > 0 {
		return strings.TrimRight(head, "\n") +
			fmt.Sprintf("\n… %s in der Mitte der Ausgabe wurden weggelassen …\n", humanBytes(omitted)) +
			tail
	}
	return head + tail
}

// omitted is how many bytes were dropped. The caller holds the lock.
func (w *headTailWriter) omitted() int64 {
	kept := int64(len(w.head) + len(w.tail))
	if w.total <= kept {
		return 0
	}
	return w.total - kept
}
