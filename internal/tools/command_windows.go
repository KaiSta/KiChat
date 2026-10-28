//go:build windows

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// The Windows half of running a command: which shell, and how a command tree is ended.
//
// This file is compiled and vetted on the Windows half of `make check` but has never been
// run on this machine — the Linux side is what it was written on — so it stays close to the
// two helpers internal/runtime and internal/images already use, where the same statement
// holds. What is asserted here is the documented behaviour of cmd.exe and taskkill, not a
// measurement.

// cmdProgram is the interpreter, and it is the one Windows question in this feature.
//
// `cmd.exe` rather than PowerShell, for three reasons that all point the same way:
//
//   - **It is always there, at one path.** PowerShell exists in two editions with two
//     executables (`powershell.exe`, `pwsh.exe`) and a machine can have neither on PATH in a
//     context that was not a shell. A tool that failed to start because of the shell would be
//     failing at the one thing that must always work.
//   - **Its quoting is the one tool authors already target.** `npx`, `go`, `npm` and every
//     README on Windows are written for cmd: `/flags` are not parsed as paths, `&&` chains,
//     `%VAR%` expands. PowerShell parses the same line differently — `curl` is an alias for
//     Invoke-WebRequest, `/` is division, `&&` needs PowerShell 7 — so a command that works
//     for one user would be a syntax error for another, and the model cannot see the
//     difference.
//   - **The line is what a user reviews.** The card shows a cmd line, and cmd is what the
//     window's own documentation and this project's Makefile-driven habits assume. A user
//     who wants PowerShell says so: `powershell -NoProfile -Command "…"` is a line this shell
//     runs, and then the choice is visible on the card rather than buried here.
const cmdProgram = "cmd.exe"

// shellCommand is what interprets one line here. `/C` runs the string and exits, which is
// the whole point — `/K` would leave a shell waiting for a person.
func shellCommand(line string) (string, []string) { return cmdProgram, []string{"/C", line} }

// shellLabel names that shell on the confirmation card.
func shellLabel() string { return "cmd.exe /C" }

// startInOwnGroup has no Windows equivalent worth the complexity, which is the same
// conclusion proc_windows.go in internal/runtime reaches: a job object is the mechanism for
// "this process and everything it starts", and it has to be created before the process and
// carried beside it, which this one call cannot do. The cost is paid in killTree instead,
// which asks the OS to walk the tree after the fact.
func startInOwnGroup(cmd *exec.Cmd) {}

// killTree ends the command and everything it started.
//
// There is no signal to send: TerminateProcess — what Kill calls — is the only mechanism,
// so `taskkill /T` is what reaches the children, and `/F` is required because without it the
// request is a close message for a window a console program does not have. The direct kill
// follows as a fallback, for a taskkill that could not run at all: leaving a command alive
// because its killer was missing is the one outcome a timeout must not have.
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := killProcessTree(cmd.Process.Pid); err == nil {
		return
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		_ = err
	}
}

// killProcessTree ends one process and its children.
func killProcessTree(pid int) error {
	// A bound on the walk, so a wedged taskkill cannot hold this call open past the wait it
	// is trying to end. Five seconds is longer than a taskkill takes and shorter than the
	// patience of anybody watching a stopped command.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	kill := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	kill.WaitDelay = time.Second
	return kill.Run()
}
