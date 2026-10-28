//go:build windows

package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// killTreeTimeout bounds the walk that ends a process tree: a guard against a wedged
// taskkill rather than a promise about how long a tree takes, with the direct kill as the
// fallback when it expires.
const killTreeTimeout = 5 * time.Second

// StartInOwnGroup has no Windows equivalent worth the complexity. A job object with
// KILL_ON_JOB_CLOSE is what would hold "the process and its children" from the moment it
// starts, and it has to be created before the process does and carried beside it — which
// StartInOwnGroup cannot do. The cost is paid at the other end instead: Terminate asks for
// the tree by walking the parent chain when it runs.
func StartInOwnGroup(*exec.Cmd) {}

// StopRequestName is the name the shared stop path logs for the graceful half of a stop, and
// on Windows that name is a call rather than a signal: there is nothing to deliver, so the
// first Terminate already ends the process. Naming the mechanism honestly matters for the one
// case the line exists for — a stop that did *not* work, which somebody reads in the log tail.
func StopRequestName() string { return "TerminateProcess" }

// Terminate ends the child and everything it started.
//
// Two things differ from the Unix side of this package, and neither is a preference:
//
//   - **There is no graceful signal.** Windows has no SIGTERM, and os.Process.Signal is not
//     supported for a process; TerminateProcess — what Kill calls — is the only mechanism
//     there is, so force decides nothing here. It stays in the signature because the
//     caller's two-phase stop is about releasing a large model's memory in an orderly way
//     on the platforms that can ask for it.
//   - **The tree comes down with it.** `taskkill /T` walks the parent chain and ends the
//     children with the parent; `/F` is required because without it the request is a close
//     message for a window a console program does not have. This is taskkill's documented
//     behaviour, not a measurement taken here.
func Terminate(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return nil
	}
	if err := killTreeByPID(cmd.Process.Pid); err == nil {
		return nil
	}
	// A machine where taskkill cannot run still has to stop what it started, so the direct
	// child is killed as well. A process that is already gone is the outcome asked for,
	// which is what ErrProcessDone says — the reading the Unix side gives ESRCH.
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// killTreeByPID ends one process and its children.
func killTreeByPID(pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), killTreeTimeout)
	defer cancel()
	kill := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	// The wait is bounded as well, so a pipe inherited by something taskkill did not end
	// cannot hold this call open.
	kill.WaitDelay = time.Second
	return kill.Run()
}
