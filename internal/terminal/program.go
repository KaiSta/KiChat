package terminal

// terminalProgram is one terminal emulator and the arguments that start it in a folder.
//
// The argv is built per program rather than from one generic template, because the
// terminals disagree about everything: some take a working directory, some need a command
// to run, and the spelling of the flag differs in each. What they must *not* disagree
// about is the folder: it is passed as one argument, never spliced into a string a shell
// then parses, so a directory called `a; rm -rf ~` is a directory.
type terminalProgram struct {
	name string
	argv func(dir string) []string
}
