//go:build unix

package images

import (
	"os/exec"
	"syscall"
)

// isolateProcess puts a child into a process group of its own.
//
// It exists for Stop. Killing the child alone is not enough when the configured "binary" is
// a wrapper script rather than the program itself — a shell that sets up an environment and
// then runs sd-cli without `exec` is an ordinary thing to configure — because the script
// dies and the real drawing process, which inherited its output pipes, carries on: the user
// presses Stop, the wait ends, and the machine keeps grinding. A group of its own means the
// kill can reach everything the child started, and it can never reach this app, because the
// group has a different id.
func isolateProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcess stops the child and everything in its group.
func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// A negative pid is the group. A group that is already gone is not a failure to stop
	// it, so the fallback is the child itself rather than an error about a race.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
