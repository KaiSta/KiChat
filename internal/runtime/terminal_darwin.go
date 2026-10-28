//go:build darwin

package runtime

import "os/exec"

// terminalCommand opens a Terminal window at dir.
//
// `open -a Terminal <folder>` is macOS's own hand-off: LaunchServices starts the
// application the system ships, and the folder is a document it knows how to open, which
// Terminal answers with a window already in it. Nothing here needs AppleScript, and that is
// deliberate — automation would ask for permission to control another application, for a
// job the Finder does with one argument.
func terminalCommand(dir string) (*exec.Cmd, error) {
	return exec.Command("open", "-a", "Terminal", dir), nil
}
