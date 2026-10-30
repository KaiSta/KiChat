// Package terminal starts the user's own terminal emulator in a folder.
//
// The app has no terminal of its own and is not getting one: what needs a terminal is
// something the app *cannot* do — today, a command that asks for administrator rights,
// where a password prompt has nowhere to go. The honest move for that case is to put the
// person in the program they already use, already configured, already trusted, opened at
// the folder the project works in. Nothing is run here: the window opens, the prompt waits,
// and whatever gets typed is theirs.
//
// So this is a launcher and nothing more. It does not pass a command to execute, it does
// not wait for the terminal to close, and it says nothing about whether what the user runs
// there is safe — the refusal that led here exists precisely because the app will not make
// that judgement for them.
//
// It lives in its own package rather than beside the llama.cpp supervisor it was written
// next to: a terminal has nothing to do with a model server, and a package that owns one
// platform list has the natural place for the per-operating-system test files the three
// lists want and cannot have while they sit inside `internal/runtime`.
package terminal

import (
	"errors"
	"time"

	"kichat/internal/proc"
)

// ErrNoTerminal is returned when this machine has no terminal emulator the app knows how to
// start. It is its own error rather than a bare failure because the window has a different
// sentence for it: "no terminal found" tells somebody what to install, while "could not
// start" invites them to try again.
var ErrNoTerminal = errors.New("no terminal emulator found")

// startupWindow is how long Open watches the program it started.
//
// A terminal that does not know the flag it was given prints its usage and exits non-zero,
// and `Start` reports success for that: the process did start, it just did not do what was
// asked. Without this the window would say a terminal opened while one flashed and
// disappeared — a false claim, and the one case where the platform list is wrong about a
// machine. Watching the first half second turns that into the sentence that already exists
// for a failed start. It is a window and not a wait: a terminal still running afterwards is
// the outcome asked for, and a program that hands off to an instance already running (the
// gnome-terminal family, `open`, `wt`) exits 0 and is likewise fine.
const startupWindow = 500 * time.Millisecond

// Program is a terminal emulator this machine has, as the window names it.
type Program struct {
	// Name is the program's own name — `gnome-terminal`, `Terminal`, `Windows Terminal` —
	// which is what Settings → About shows and what a person recognises.
	Name string
	// Path is the executable that was resolved, for the diagnostics line and for the log.
	Path string
}

// Detect reports the terminal this machine would open, without opening anything.
//
// It is the same resolution Open does, and it exists for two reasons: the window can say
// which program the button will use (or that there is none) *before* the click, and a
// person on Windows or macOS — where the suite cannot run — can check that their platform's
// list resolves to something real instead of taking it on trust.
func Detect() (Program, error) {
	name, path, err := resolve()
	if err != nil {
		return Program{}, err
	}
	return Program{Name: name, Path: path}, nil
}

// Open starts a terminal in dir and returns once it is running — or as soon as it has
// failed, which is the other thing a start can turn out to be (see startupWindow).
//
// The terminal is the user's program, not this app's: it is not attached to the app's
// output, it is not waited for beyond that half second, and closing it is their business. It
// is started in a process group of its own for the same reason a supervised server is, in
// reverse — a signal that ends this app must not take a shell somebody is typing in with it.
func Open(dir string) error {
	cmd, err := command(dir)
	if err != nil {
		return err
	}
	proc.StartInOwnGroup(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reaped in the background: Wait releases the child's resources, and the app must not
	// block on a window the user may keep open all afternoon. The channel is buffered so the
	// goroutine cannot outlive this call with a send nobody reads.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		// Already gone: either it could not open a window, or it handed the job to an
		// instance that was already running. Only the first is a failure, and only a
		// non-zero exit means that.
		return err
	case <-time.After(startupWindow):
		return nil
	}
}
