//go:build windows

package runtime

import "os/exec"

// startInOwnGroup has no Windows equivalent that is worth the complexity: a job
// object would be the way to get "and everything it started", and llama-server
// starts no children. The no-op keeps the call site free of platform branches.
func startInOwnGroup(cmd *exec.Cmd) {}

// terminate ends the process. Windows has no SIGTERM and Signal on a process is
// not supported, so TerminateProcess — what Kill calls — is the only mechanism,
// whether or not a graceful shutdown was asked for.
func terminate(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
