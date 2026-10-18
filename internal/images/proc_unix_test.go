//go:build unix

package images

import "syscall"

// processAlive reports whether a pid still exists, which is the only way to tell a killed
// program from one that was simply left to run: both answer a cancelled call eventually.
//
// Signal 0 is the portable-on-unix way to ask the kernel about a process without touching
// it; an ESRCH means it is gone.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
