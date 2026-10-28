//go:build !windows

package tools

// The Unix half of running a command: which shell interprets one line.
//
// Stopping the command and everything it started used to live here as well, in a third copy
// of the two helpers internal/runtime and internal/images carried. That is now
// internal/proc, which is where the process group and the tree kill went — this file is
// only the platform decision that belongs to a tool, because it is the one the card shows.

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
