//go:build !windows

package runtime

import (
	"errors"
	"os/exec"
	"syscall"
)

// startInOwnGroup puts the server in a process group of its own.
//
// Without this, the server shares the app's group: a shutdown signal sent to the
// group would reach the app's own process, and a child the server forked would be
// left behind. A group of its own makes the shutdown below address exactly the
// server and everything it started, and nothing else.
func startInOwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminate signals the server's whole process group. force sends SIGKILL, which
// cannot be ignored, for a server that did not stop on SIGTERM.
//
// The group is addressed through the negative pid, so a child the server forked
// dies with it. That matters beyond tidiness: a surviving child inherits the
// server's output pipes, and a pipe still open is what keeps exec's Wait from
// returning even though the process it names is long gone.
func terminate(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return nil
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		// ESRCH means it is already gone, which is the outcome asked for.
		return nil
	}
	if serr := cmd.Process.Signal(sig); serr != nil && !errors.Is(serr, syscall.ESRCH) {
		return serr
	}
	return nil
}
