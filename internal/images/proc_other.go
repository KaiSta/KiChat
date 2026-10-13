//go:build !unix

package images

import "os/exec"

// isolateProcess has nothing to do where a process group is not a portable notion.
func isolateProcess(*exec.Cmd) {}

// killProcess stops the child.
//
// A script that started something of its own keeps running here, which is the one place
// this backend is weaker than the one KiChat is built and tested on: on Unix the group is
// killed instead, so a wrapper script cannot outlive the Stop.
func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
