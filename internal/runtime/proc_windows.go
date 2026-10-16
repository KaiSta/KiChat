//go:build windows

package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// killTreeTimeout bounds the walk that ends a server tree: a guard against a wedged
// taskkill rather than a promise, with the direct kill as the fallback.
const killTreeTimeout = 5 * time.Second

// stopRequestName is the name the shared stop path logs for the graceful half of a stop,
// and on Windows that name is a call rather than a signal: there is nothing to deliver to a
// server here, so the first `terminate` already ends it. Naming the mechanism honestly
// matters for the one case the line exists for — a stop that did *not* work, which somebody
// will read in the log tail and search for.
func stopRequestName() string { return "TerminateProcess" }

// startInOwnGroup has no Windows equivalent that is worth the complexity: a job object
// would be the way to get "and everything it started", and it has to be created before the
// process does and carried beside it, which the two functions here cannot do. What that
// costs is paid at the other end instead — terminate below asks for the tree.
func startInOwnGroup(cmd *exec.Cmd) {}

// terminate ends the server and everything it started.
//
// Two things differ from the Unix side of this file, and neither is a preference:
//
//   - **There is no graceful signal.** Windows has no SIGTERM, and os.Process.Signal is not
//     supported for a process; TerminateProcess — what Kill calls — is the only mechanism
//     there is, so `force` decides nothing here. It is kept in the signature because the
//     caller's two-phase stop is about a large model's memory being released in an orderly
//     way on the platforms that can ask for it.
//   - **The tree comes down with it.** The reason is the same one the Unix group kill
//     documents, and it bites harder here: `llama-server` starts no children of its own, but
//     the *picture* server this same supervisor starts for the local stable-diffusion
//     build can be a build the user pointed a wrapper at, and a survivor inherits the
//     server's output pipes — which is what keeps exec's Wait from returning, and what the
//     stop path then reports as "the output channels are still open". `taskkill /T` walks
//     the parent chain and ends the children with the parent; `/F` is required because
//     without it the request is a close message for a window a console program does not
//     have — taskkill's documented behaviour, not a measurement taken here, since the
//     Windows side of this file is compiled and checked but never run on this machine.
//
// internal/images carries the same helper for the one-shot draw (`proc_windows.go` there);
// a change to the tree kill belongs in both.
func terminate(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return nil
	}
	if err := killTree(cmd.Process.Pid); err == nil {
		return nil
	}
	// A machine where taskkill cannot run still has to stop the model it started, so the
	// direct child is killed as well. A server that is already gone is the outcome asked
	// for rather than a failure to stop it, which is what ErrProcessDone says — the same
	// reading the Unix side gives ESRCH.
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// killTree ends one process and its children.
func killTree(pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), killTreeTimeout)
	defer cancel()
	kill := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	// The wait is bounded as well, so a pipe inherited by something taskkill did not end
	// cannot hold this call open.
	kill.WaitDelay = time.Second
	return kill.Run()
}
