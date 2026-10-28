//go:build windows

package terminal

import (
	"os"
	"os/exec"
	"syscall"
)

// createNewConsole is CREATE_NEW_CONSOLE from the Windows API (winbase.h). It is spelled
// out here rather than pulled in with golang.org/x/sys/windows for one flag: this package
// already reaches the platform through os/exec and syscall, and a dependency is not worth
// its upkeep for a constant.
const createNewConsole = 0x00000010

// shellName is the display name for the fallback: a console window, not a terminal emulator
// with a name of its own.
const shellName = "cmd.exe"

// resolve reports Windows Terminal when it is installed, and the shell's console otherwise.
func resolve() (string, string, error) {
	if path, err := exec.LookPath("wt.exe"); err == nil {
		return "Windows Terminal", path, nil
	}
	// `ComSpec` is the variable Windows itself keeps for this path, and the shell's own
	// directory is not reliably on PATH. LookPath and then the bare name are the fallbacks
	// for an environment that has been stripped.
	if comspec := os.Getenv("ComSpec"); comspec != "" {
		return shellName, comspec, nil
	}
	if path, err := exec.LookPath("cmd.exe"); err == nil {
		return shellName, path, nil
	}
	return shellName, shellName, nil
}

// command starts a shell window in dir.
//
// Windows Terminal first, because it takes the folder as a flag. Otherwise a console is
// created for the shell — twice over, in fact: the CREATE_NEW_CONSOLE flag is what opens the
// window, and `cmd` with no arguments is what keeps it open, so no `/K` and no command line
// that another parser has to read.
//
// That last part is the reason this is not `start cmd /K cd /D "%1"`: the folder would
// travel through `cmd`'s own parser, where a directory whose name contains `&` or `(` is a
// command separator rather than part of a path. Here the working directory is set by
// `exec.Cmd.Dir`, which passes it to the process directly — the path is never parsed by
// anything.
func command(dir string) (*exec.Cmd, error) {
	if path, err := exec.LookPath("wt.exe"); err == nil {
		return exec.Command(path, "-d", dir), nil
	}
	shell := os.Getenv("ComSpec")
	if shell == "" {
		if path, err := exec.LookPath("cmd.exe"); err == nil {
			shell = path
		} else {
			shell = shellName
		}
	}
	cmd := exec.Command(shell)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewConsole}
	return cmd, nil
}
