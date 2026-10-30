//go:build !unix && !windows

package proc

import "os/exec"

// StartInOwnGroup has nothing to do where a process group is not a portable notion. The
// platforms here are built but not shipped, and the cost of pretending otherwise would be a
// build-tagged file claiming a guarantee it cannot keep.
func StartInOwnGroup(*exec.Cmd) {}

// StopRequestName is what the log line says on a platform with neither signals nor
// TerminateProcess: the child is killed, and there is no graceful half to name.
func StopRequestName() string { return "kill" }

// Terminate stops the child. There is no group to address and no tree walk to ask for, so
// this is the only mechanism these platforms have — the same reason the Windows side ignores
// force.
func Terminate(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
