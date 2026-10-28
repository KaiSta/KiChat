package runtime

// Starting the user's own terminal, in the folder they are working in.
//
// The app has no terminal of its own and is not getting one: what needs a terminal is
// something the app *cannot* do — today, a command that asks for administrator rights,
// where a password prompt has nowhere to go. The honest move for that case is to put the
// person in the program they already use, already configured, already trusted, opened at
// the folder the project works in. Nothing is run here: the window opens, the prompt
// waits, and whatever gets typed is theirs.
//
// So this is a launcher and nothing more. It does not pass a command to execute, it does
// not wait for the terminal to close, and it says nothing about whether what the user runs
// there is safe — the refusal that led here exists precisely because the app will not make
// that judgement for them.

import (
	"errors"
	"time"
)

// ErrNoTerminal is returned when this machine has no terminal emulator the app knows how
// to start. It is its own error rather than a bare failure because the window has a
// different sentence for it: "no terminal found" tells somebody what to install, while
// "could not start" invites them to try again.
var ErrNoTerminal = errors.New("no terminal emulator found")

// terminalStartupWindow is how long OpenTerminal watches the program it started.
//
// A terminal that does not know the flag it was given prints its usage and exits non-zero,
// and `Start` reports success for that: the process did start, it just did not do what was
// asked. Without this the window would say a terminal opened while one flashed and
// disappeared — a false claim, and the one case where the program list above is wrong about
// a machine. Watching the first half second turns that into the sentence that already exists
// for a failed start. It is a window and not a wait: a terminal still running afterwards is
// the outcome asked for, and a program that hands off to an instance already running (the
// gnome-terminal family, `open`, `wt`) exits 0 and is likewise fine.
const terminalStartupWindow = 500 * time.Millisecond

// terminalProgram is one terminal emulator and the arguments that start it in a folder.
//
// The argv is built per program rather than from one generic template, because the
// terminals disagree about everything: some take a working directory, some need a command
// to run, and the spelling of the flag differs in each. What they must *not* disagree
// about is the folder: it is passed as one argument, never spliced into a string a shell
// then parses, so a directory called `a; rm -rf ~` is a directory.
type terminalProgram struct {
	name string
	argv func(dir string) []string
}

// OpenTerminal starts a terminal in dir and returns once it is running — or as soon as it
// has failed, which is the other thing a start can turn out to be (see
// terminalStartupWindow).
//
// The terminal is the user's program, not this app's: it is not attached to the app's
// output, it is not waited for beyond that half second, and closing it is their business. It
// is started in a process group of its own for the same reason a supervised server is, in
// reverse — a signal that ends this app must not take a shell somebody is typing in with it.
func OpenTerminal(dir string) error {
	cmd, err := terminalCommand(dir)
	if err != nil {
		return err
	}
	startInOwnGroup(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reaped in the background: Wait releases the child's resources, and the app must not
	// block on a window the user may keep open all afternoon. The channel is buffered so
	// the goroutine cannot outlive this call with a send nobody reads.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		// Already gone: either it could not open a window, or it handed the job to an
		// instance that was already running. Only the first is a failure, and only a
		// non-zero exit means that.
		return err
	case <-time.After(terminalStartupWindow):
		return nil
	}
}
