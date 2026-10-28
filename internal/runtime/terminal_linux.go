//go:build linux

package runtime

import (
	"os/exec"
)

// linuxTerminalPrograms is the list this build tries, in the order it tries them.
//
// The order is a preference and nothing more: the first one installed wins. The ones that
// take a working directory — in three spellings — need no shell at all, which is why they
// come first; the two of the rxvt family take a command instead and run `/bin/sh` with the
// folder as `$1`, so the folder is still one argument and never part of the script that
// gets parsed.
//
// Why not one generic call: `x-terminal-emulator` is a symlink to whichever terminal the
// distribution chose, and the flags it accepts follow that choice — passing
// `--working-directory` to a build that is xterm opens a terminal in the user's home and
// reports success, and the `-e` form is not interchangeable either (the rxvt family takes
// the rest of the line as argv, the gnome-terminal family a single string). It is therefore
// *not* in this list: naming the programs costs a list and buys an answer that is either
// right or absent, and the ones not named here fall through to the honest "no terminal
// found" instead of a window that flashes an error.
func linuxTerminalPrograms() []terminalProgram {
	// The script a command-taking terminal runs: change into the folder from `$1`, then
	// hand over to the user's own shell. The folder never appears in this string.
	const shellIn = `cd "$1" && exec "${SHELL:-/bin/sh}"`
	execIn := func(dir string) []string { return []string{"-e", "/bin/sh", "-c", shellIn, "sh", dir} }
	return []terminalProgram{
		{"gnome-terminal", func(dir string) []string { return []string{"--working-directory", dir} }},
		{"konsole", func(dir string) []string { return []string{"--workdir", dir} }},
		{"xfce4-terminal", func(dir string) []string { return []string{"--working-directory", dir} }},
		{"kitty", func(dir string) []string { return []string{"--directory", dir} }},
		{"alacritty", func(dir string) []string { return []string{"--working-directory", dir} }},
		{"foot", func(dir string) []string { return []string{"--working-directory", dir} }},
		{"terminator", func(dir string) []string { return []string{"--working-directory", dir} }},
		{"xterm", execIn},
		{"urxvt", execIn},
	}
}

// terminalCommand resolves the first terminal on this machine and builds its command.
func terminalCommand(dir string) (*exec.Cmd, error) {
	for _, program := range linuxTerminalPrograms() {
		path, err := exec.LookPath(program.name)
		if err != nil {
			continue
		}
		return exec.Command(path, program.argv(dir)...), nil
	}
	return nil, ErrNoTerminal
}
