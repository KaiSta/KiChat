//go:build windows

package tools

// The Windows half of running a command: which shell interprets one line.
//
// Stopping the command tree used to live here too, in a copy of the helper internal/runtime
// and internal/images already carried; that is internal/proc now, so `taskkill /T /F` and
// its fallback are documented in one place instead of three. This file is compiled and
// vetted on the Windows half of `make check` but has never been run on this machine, so what
// is asserted here is the documented behaviour of cmd.exe, not a measurement.

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
