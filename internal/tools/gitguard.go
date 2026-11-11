package tools

// Why a command that discards work is refused rather than undone.
//
// The file tools can take a change back: every write or edit keeps the text it replaced
// (internal/tools/snapshot.go). A command cannot be given the same treatment, and the reason is
// the shape of that record: it is keyed by (chat, path) and deliberately keeps the *first*
// pre-image the chat saw. A snapshot taken just before `git reset --hard` would therefore mean
// two different things in one row — "before this chat touched the file" and "before this
// command" — and the panel has one button for both.
//
// So the promise here is narrower and honest: the work is not lost. A line that would discard
// uncommitted changes is refused *before* the confirmation card is built, the way an elevation
// request is — the person should not be asked to approve something the app already knows it will
// not run — and the model is told what to do instead. On a clean tree the same line is a no-op,
// so it runs: the guard only ever fires when there is something to lose.
//
// What it is, and is not: a guard over the obvious forms, not a boundary. `bash -c "git reset
// --hard"`, an alias, `cd sub && git reset --hard`, and a line with a quoted path are all left
// alone — the first two because the words are not the command, the third because the pathspecs
// would be relative to another folder, and the fourth because the word split cannot size it.
// The boundary is elsewhere: the command runs with the user's rights, and this exists to stop a
// routine mistake, which is the one that actually happens.

import (
	"fmt"
	"strings"

	"kichat/internal/uimsg"
	"kichat/internal/workspace"
)

// discardClass is what a recognised line destroys. The commands differ: `checkout --` throws
// away unstaged changes, `reset --hard` also throws away what was staged, and `clean` touches
// only what git does not track.
type discardClass int

const (
	discardWorktree         discardClass = iota // checkout --, restore
	discardTracked                              // reset --hard
	discardUntracked                            // clean -f
	discardUntrackedIgnored                     // clean -fx
	discardIgnored                              // clean -fX
)

// destructiveGit is one recognised invocation: what it destroys, and where.
type destructiveGit struct {
	verb  string
	class discardClass
	// specs are the pathspecs it was given, empty for the whole folder.
	specs []string
}

func (d destructiveGit) needsIgnored() bool {
	return d.class == discardUntrackedIgnored || d.class == discardIgnored
}

// count is how much of the scope this command would destroy.
func (d destructiveGit) count(scope workspace.GitScope) int {
	switch d.class {
	case discardWorktree:
		return scope.Worktree
	case discardTracked:
		return scope.Tracked
	case discardUntracked:
		return scope.Untracked
	case discardUntrackedIgnored:
		return scope.Untracked + scope.Ignored
	case discardIgnored:
		return scope.Ignored
	}
	return 0
}

// samples names up to five of the paths the command would destroy, taken from the class it acts
// on: a `clean` refusal must not be shown a modified file that clean leaves alone.
func (d destructiveGit) samples(scope workspace.GitScope) []string {
	switch d.class {
	case discardWorktree:
		return scope.WorktreeSamples
	case discardTracked:
		return scope.TrackedSamples
	case discardUntracked:
		return scope.UntrackedSamples
	case discardUntrackedIgnored:
		out := append([]string(nil), scope.UntrackedSamples...)
		for _, path := range scope.IgnoredSamples {
			if len(out) == 5 {
				break
			}
			out = append(out, path)
		}
		if len(out) > 5 {
			out = out[:5]
		}
		return out
	case discardIgnored:
		return scope.IgnoredSamples
	}
	return nil
}

// refuseDestructiveGit returns the refusal when the line would discard uncommitted work, and nil
// when it would not — including when there is no git, no repository, or the folder has gone: a
// command tool that failed because git is not installed would be a worse trade than the guard is
// worth.
func refuseDestructiveGit(lang string, root workspace.Root, line string) error {
	discard, ok := parseDestructiveGit(line)
	if !ok {
		return nil
	}
	scope, err := root.GitDiscardScope(discard.specs, discard.needsIgnored())
	if err != nil {
		return nil
	}
	count := discard.count(scope)
	if count == 0 {
		return nil
	}
	examples := strings.Join(discard.samples(scope), ", ")
	detail := fmt.Sprintf(text(lang,
		"dieser Befehl würde %d nicht committete Änderung(en) verwerfen",
		"this command would discard %d uncommitted change(s)"), count)
	if examples != "" {
		detail += " (" + examples + ")"
	}
	detail += text(lang,
		"; committe oder stashe sie zuerst, oder bitte den Nutzer, die Zeile selbst auszuführen",
		"; commit or stash them first, or ask the user to run the line themselves")
	return uimsg.Detailf("tools.destructiveGit", detail, count, examples)
}

// parseDestructiveGit reads a command line and reports the first destructive git invocation in
// it, if any.
//
// It reads the line the way a shell starts simple commands, the same way elevationRequest does,
// because `git add . && git checkout -- .` is one line the user approves and two things that
// happen. A `cd` before the git word stops the scan: the pathspecs would be relative to another
// folder, and a scope measured against the project root would be the wrong one.
func parseDestructiveGit(line string) (destructiveGit, bool) {
	segments := strings.FieldsFunc(line, func(r rune) bool {
		switch r {
		case ';', '&', '|', '\n':
			return true
		}
		return false
	})
	for _, segment := range segments {
		// A quoted path cannot be split into the path it names, and a pathspec guessed wrong
		// would size the scope wrong; the line is left alone rather than refused by guesswork.
		if strings.ContainsAny(segment, "\"'") {
			continue
		}
		// Assignments and `env` are not the command: `GIT_DIR=. git reset --hard` names git only
		// after them, and `env …` one word later — in either order, which is the part a copy of
		// the elevation parser got wrong until both read the prefix through one function.
		words := stripCommandPrefix(strings.Fields(segment))
		if len(words) == 0 {
			continue
		}
		switch commandWord(words[0]) {
		case "cd", "pushd", "popd":
			return destructiveGit{}, false
		case "git":
			if discard, ok := parseGitArguments(words[1:]); ok {
				return discard, true
			}
		}
	}
	return destructiveGit{}, false
}

// parseGitArguments skips git's own global options and dispatches on the subcommand.
//
// A global option that moves git elsewhere — `-C`, `--git-dir`, `--work-tree` — ends the
// attempt: the command would then read a different folder than the pathspecs below are measured
// against, and refusing the wrong amount is worse than not recognising the line.
func parseGitArguments(args []string) (destructiveGit, bool) {
	for len(args) > 0 {
		switch arg := args[0]; {
		// `-C../elsewhere` is the same option with its value attached. Reading only the
		// separated `-C` measured *this* folder for a command that runs in another one.
		case strings.HasPrefix(arg, "-C"), arg == "--exec-path", strings.HasPrefix(arg, "--git-dir"),
			strings.HasPrefix(arg, "--work-tree"):
			return destructiveGit{}, false
		case arg == "-c":
			if len(args) < 2 {
				return destructiveGit{}, false
			}
			args = args[2:]
		case strings.HasPrefix(arg, "-"):
			args = args[1:]
		default:
			sub, rest := arg, args[1:]
			switch sub {
			case "checkout":
				return parseGitCheckout(rest)
			case "restore":
				return parseGitRestore(rest)
			case "reset":
				return parseGitReset(rest)
			case "clean":
				return parseGitClean(rest)
			}
			return destructiveGit{}, false
		}
	}
	return destructiveGit{}, false
}

// parseGitCheckout covers the two forms that discard: `checkout -- <paths>` (including
// `checkout <tree-ish> -- <paths>`), and a forced checkout, which rewrites the whole working copy.
//
// `git checkout <branch>` is deliberately not one of them. It carries local changes across, and
// git refuses by itself where it would overwrite something — judging it here would refuse a
// legitimate switch whenever the tree is merely dirty.
func parseGitCheckout(args []string) (destructiveGit, bool) {
	force := false
	head, specs, hasPaths := args, []string(nil), false
	if i := argIndex(args, "--"); i >= 0 {
		head, specs, hasPaths = args[:i], args[i+1:], true
	}
	for _, arg := range head {
		switch {
		case arg == "-f" || arg == "--force":
			force = true
		case strings.HasPrefix(arg, "-"):
			// -b/-B and the rest create or name branches; none of them discards on its own.
		case strings.HasPrefix(arg, "."):
			// A ref component may not begin with a dot, so this is unambiguously a pathspec
			// even without `--`. It is the form `git checkout .` people actually type.
			if !hasPaths {
				specs = append(specs, arg)
			}
		default:
			// A branch or a commit before `--`: a switch, not a discard.
			if !hasPaths && !force {
				return destructiveGit{}, false
			}
		}
	}
	if !force && len(specs) == 0 {
		return destructiveGit{}, false
	}
	return destructiveGit{verb: "checkout", class: discardWorktree, specs: specs}, true
}

// parseGitRestore covers restoring the working copy. `--staged` alone only unstages, which loses
// no text: the file still holds it, so that form is left alone.
func parseGitRestore(args []string) (destructiveGit, bool) {
	head, specs, hasPaths := args, []string(nil), false
	if i := argIndex(args, "--"); i >= 0 {
		head, specs, hasPaths = args[:i], args[i+1:], true
	}
	staged, worktree := false, false
	for _, arg := range head {
		switch {
		case arg == "--staged" || arg == "-S":
			staged = true
		case arg == "--worktree" || arg == "-W":
			worktree = true
		case strings.HasPrefix(arg, "-"):
			// --source, --ours, --theirs and the rest change where the text comes from.
		default:
			specs = append(specs, arg)
			hasPaths = true
		}
	}
	if staged && !worktree {
		return destructiveGit{}, false
	}
	if !hasPaths || len(specs) == 0 {
		return destructiveGit{}, false
	}
	return destructiveGit{verb: "restore", class: discardWorktree, specs: specs}, true
}

// parseGitReset covers `--hard`, the only reset that throws text away. `--soft` only moves HEAD;
// `--mixed` moves HEAD and the index, and the working copy still holds every line.
func parseGitReset(args []string) (destructiveGit, bool) {
	hard := false
	for _, arg := range args {
		if arg == "--hard" {
			hard = true
		}
	}
	if !hard {
		return destructiveGit{}, false
	}
	var specs []string
	if i := argIndex(args, "--"); i >= 0 {
		specs = args[i+1:]
	}
	return destructiveGit{verb: "reset", class: discardTracked, specs: specs}, true
}

// parseGitClean covers the forms that delete: `-f` without `-n`, and what `-x`/`-X` add.
func parseGitClean(args []string) (destructiveGit, bool) {
	force, dryRun, ignored, onlyIgnored := false, false, false, false
	var specs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			specs = append(specs, args[i+1:]...)
			i = len(args)
		case arg == "-e" || arg == "--exclude":
			// The pattern is a filter, not a pathspec: `git clean -f -e node_modules` deletes
			// everything except it. Taking the pattern as the scope measured the wrong list —
			// usually an empty one, which let the whole clean through.
			i++
		case strings.HasPrefix(arg, "--exclude="), len(arg) > 2 && strings.HasPrefix(arg, "-e"):
			// The pattern is attached to the flag; there is no next word to skip.
		case strings.HasPrefix(arg, "--"):
			switch arg {
			case "--force":
				force = true
			case "--dry-run":
				dryRun = true
			case "--ignored":
				ignored = true
			}
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			for _, flag := range arg[1:] {
				switch flag {
				case 'f':
					force = true
				case 'n':
					dryRun = true
				case 'x':
					ignored = true
				case 'X':
					onlyIgnored = true
				}
			}
		default:
			specs = append(specs, arg)
		}
	}
	if !force || dryRun {
		return destructiveGit{}, false
	}
	class := discardUntracked
	switch {
	case onlyIgnored:
		class = discardIgnored
	case ignored:
		class = discardUntrackedIgnored
	}
	return destructiveGit{verb: "clean", class: class, specs: specs}, true
}

// argIndex is the position of the first `--` in args, or -1.
func argIndex(args []string, want string) int {
	for i, arg := range args {
		if arg == want {
			return i
		}
	}
	return -1
}
