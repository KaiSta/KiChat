//go:build !unix && !windows

package images

import "os/exec"

// isolateProcess has nothing to do where a process group is not a portable notion.
func isolateProcess(*exec.Cmd) {}

// killProcess stops the child.
//
// A script that started something of its own keeps running here. That is *not* true on the
// two platforms this app is built for: Unix kills the group (proc_unix.go) and Windows walks
// the tree with taskkill (proc_windows.go), so a wrapper script cannot outlive the Stop on
// either. What is left in this file is the platforms neither of those describes — Plan 9, a
// JS host — where killing exactly the process that was started is the only thing that can be
// promised without measuring how that system names a tree.
func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
