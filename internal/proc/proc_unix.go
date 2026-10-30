//go:build unix

package proc

import (
	"errors"
	"os/exec"
	"syscall"
)

// StartInOwnGroup puts the child in a process group of its own.
//
// Without this the child shares the app's group: a signal sent to "the whole group" would
// reach KiChat itself, and a child the app never sees — a test binary under `go test`, a
// compiler under `make`, a program a wrapper script started — would be left behind, still
// holding the output pipes this process waits on.
func StartInOwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// StopRequestName is what the graceful half of a stop is called here, for log lines that can
// only report what was attempted.
func StopRequestName() string { return "SIGTERM" }

// Terminate signals the child's whole process group: SIGTERM first, or SIGKILL when force is
// set, which cannot be ignored and is what a caller that is out of patience sends.
//
// The group is addressed through the negative pid, so a child the process forked dies with
// it. A process that has already exited is the result asked for rather than a failure to
// stop it, which is what ESRCH means here.
func Terminate(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return nil
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	// No group to signal: a child started without StartInOwnGroup, or a group this process
	// may not address. The direct child is then the fallback.
	if serr := cmd.Process.Signal(sig); serr != nil && !errors.Is(serr, syscall.ESRCH) {
		return serr
	}
	return nil
}
