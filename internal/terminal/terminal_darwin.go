//go:build darwin

package terminal

import "os/exec"

// terminalApp is what `open -a` starts. Terminal ships with macOS, so there is no list to
// search on this platform: the program is the system's own, and the folder is a document it
// knows how to open, which Terminal answers with a window already in it.
const terminalApp = "Terminal"

// resolve reports Terminal.app, whose launcher is `open`.
//
// The path is `open`'s rather than Terminal.app's because that is what will run — a path to
// the .app bundle would suggest the app is being executed directly, which it is not.
func resolve() (string, string, error) {
	path, err := exec.LookPath("open")
	if err != nil {
		return "", "", ErrNoTerminal
	}
	return terminalApp, path, nil
}

// command opens a Terminal window at dir.
//
// `open -a Terminal <folder>` is macOS's own hand-off: LaunchServices starts the application
// the system ships, and the folder is a document it knows how to open. Nothing here needs
// AppleScript, and that is deliberate — automation would ask for permission to control
// another application, for a job the Finder does with one argument.
func command(dir string) (*exec.Cmd, error) {
	path, err := exec.LookPath("open")
	if err != nil {
		return nil, ErrNoTerminal
	}
	return exec.Command(path, "-a", terminalApp, dir), nil
}
