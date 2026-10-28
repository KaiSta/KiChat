//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// The Unix half of running a command: which shell, and how a command tree is ended.
//
// internal/runtime and internal/images each carry the same two helpers for the servers they
// supervise (`proc_unix.go` in both). This is the third copy, and that is a cost rather than
// a design: the three differ in what they are stopping — a long-lived server that gets a
// graceful SIGTERM first, versus a command that is killed outright — but the group handling
// and the negative-pid kill are the same code. Extracting them into one small package is the
// right cleanup, and it is deliberately not done here, where the change under review is a new
// tool. When it is done, it is these three files that move.

// shellCommand is what interprets one line here.
//
// /bin/sh rather than $SHELL, bash or zsh, and the reason is reviewability rather than
// taste: this app's users ask for commands like `make check` and `go test ./...`, whose
// meaning is defined by POSIX sh — `set -e`, `&&`, a pipe. A developer's interactive shell
// brings aliases, history expansion, `**` globbing and `setopt` behaviour that a model
// cannot know and a user cannot see, which is the opposite of a line somebody is being asked
// to approve. A command that genuinely needs another shell can ask for it by name:
// `bash -c "…"` is itself a line this shell runs.
func shellCommand(line string) (string, []string) { return "/bin/sh", []string{"-c", line} }

// shellLabel names that shell on the confirmation card, so the person reading the question
// knows what will interpret the line.
func shellLabel() string { return "/bin/sh -c" }

// startInOwnGroup puts the command in a process group of its own.
//
// Without it the command shares the app's group, and the kill below — which addresses the
// negative pid, meaning "the whole group" — would reach KiChat itself. A group of its own
// makes that call address exactly this command and everything it starts.
func startInOwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree ends the command and everything it started.
//
// The tree matters here more than it does for a server: `go test` is the parent of the test
// binaries it builds, and `make` is the parent of every compiler it starts. Killing only the
// shell would leave those writing to the project folder after the result said the command
// was stopped — and holding the pipes this call reads from, which is the other half of why
// exec's Wait would not return. SIGKILL rather than SIGTERM because the call is already over
// its budget: there is no second chance being offered, and a shell can ignore a signal.
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// No group to signal — a command already gone (ESRCH), or one whose group this
		// process could not address. The direct child is then the fallback, and a process
		// that has already exited is the outcome asked for rather than a failure.
		_ = cmd.Process.Kill()
	}
}
