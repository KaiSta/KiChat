//go:build windows

package images

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// killTreeTimeout bounds the walk that ends a process tree. It is a guard against a
// wedged taskkill rather than a promise about how long a tree takes: the caller falls back
// to killing the child it knows about, so a bound that expires is not a failure.
const killTreeTimeout = 5 * time.Second

// isolateProcess has nothing to do on Windows: there is no process group to put the child
// into, and the thing it is standing in for — "and everything it started" — is asked for at
// the other end, in killProcess. See the note there; this is the one platform where the
// group trick has no counterpart to set up front.
func isolateProcess(*exec.Cmd) {}

// killProcess stops the child and everything it started.
//
// The case is the same one the Unix side of this file handles — a configured "binary" that
// is a wrapper (a `.bat`, a launcher) which runs sd-cli without waiting for it — and Windows
// has no `Setpgid` and no negative pid to address the group with. A job object with
// KILL_ON_JOB_CLOSE is the mechanism that *would* hold "the process and its children" from
// the moment it starts, but it has to be created before the process does and carried beside
// it, and this file's two entry points (one before the start, one after) have nowhere to
// carry it. `taskkill /T` is the Windows answer that needs neither: it walks the parent
// chain at the moment it runs and ends the children with the parent.
//
// So a wrapper script cannot outlive the Stop here either, which is what the doc comment in
// proc_other.go says is missing on a platform that is neither. internal/runtime carries the
// same helper for the server it supervises (`proc_windows.go` there); a change to the tree
// kill belongs in both.
func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := killTree(cmd.Process.Pid); err == nil {
		return nil
	}
	// The tree kill is the better answer, not the required one: a machine where taskkill
	// cannot run at all still has to stop the picture it started, so the direct child is
	// killed as well. A child that is already gone is the outcome asked for rather than a
	// failure to stop it, which is what ErrProcessDone says.
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// killTree ends one process and its children.
//
// `/F` is not optional, and that is taskkill's own documented behaviour rather than
// something measured here — there is no Windows machine behind this file: without `/F` the
// request is a close message for the target's window, and a console program like sd-cli has
// no window, so it would be reported as ended while it kept drawing.
func killTree(pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), killTreeTimeout)
	defer cancel()
	kill := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	// taskkill's own output is of no interest, but a pipe it inherited from a child it did
	// not manage to end would hold Run() past its process — the same trap the Unix side
	// documents — so the wait is bounded as well.
	kill.WaitDelay = time.Second
	return kill.Run()
}
