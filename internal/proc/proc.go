// Package proc starts a child in a process group of its own, and ends that group.
//
// Three packages had grown the same helpers beside the thing they stop: the supervised
// llama.cpp server in internal/runtime, the picture servers in internal/images, and the
// project terminal in internal/tools — each copy documenting that the others existed and
// that extracting them was the right cleanup. This is that cleanup, and it was worth doing
// before a fourth caller (the coding hand-off's terminal) added another copy.
//
// What the copies did not agree on was the *first* half of a stop. The server is asked to
// shut down (SIGTERM) before it is killed, because a model holding gigabytes of memory
// releases them in an orderly way; a command or a drawing process is killed outright,
// because the call is already over its budget and there is no second chance to give it.
// Both are here as `Terminate` and `KillTree` rather than one function with a flag nobody
// remembers — and the reason to keep them together is that they share the part that must
// not drift: which process ids get signalled.
//
// The group is the whole point. Killing the child alone leaves whatever it started running:
// `go test` is a parent of the binaries it builds, `make` of every compiler, and a wrapper
// script of the program it was supposed to be. Those survivors write to the folder after
// the result said the command was stopped, and they hold the output pipes that keep exec's
// Wait from returning — which is how a stop turns into a hang.
package proc

import "os/exec"

// KillTree ends cmd and everything it started, without waiting for anything.
//
// It is the answer for a call that is already over: the command hit its timeout, the user
// pressed stop, or the turn was cancelled. A process that is already gone is the outcome
// asked for rather than a failure, so an error is returned only when the child could not be
// killed either as a group or on its own.
func KillTree(cmd *exec.Cmd) error { return Terminate(cmd, true) }
