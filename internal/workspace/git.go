package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// What the project folder's version control says about it.
//
// A coding agent starts every turn blind: the transcript says what it wrote, the folder says
// what is there now, and nothing says what has changed *since the last commit* — which is the
// question that decides whether its earlier work is finished, staged, or already committed by
// somebody else. Reading it here rather than asking the model to run `git status` is the cheap
// half of that gap: a tool call costs a round and a confirmation card, and the answer is the
// same on every turn of a project whether the model thought to ask or not.
//
// Three constraints shape the code below.
//
//   - **It is a read, and it must not take a lock or run a hook.** `git status` refreshes the
//     index by default, which is a write to the repository, and a repository can name programs
//     in its own config for fsmonitor and for diff drivers — a cloned project is somebody
//     else's config. `GIT_OPTIONAL_LOCKS=0` keeps the index untouched; `core.fsmonitor=false`,
//     `--no-ext-diff` and `--no-textconv` keep this function from executing anything the folder
//     asked for. Running commands in the folder is a decision the user makes per call
//     (internal/tools); this runs unprompted, so it must stay inert.
//   - **It is bounded.** One shared deadline covers every call, so a repository on a slow disk
//     costs the prompt an omission rather than a stuck turn, and the entry list is capped so a
//     dirty tree cannot spend the window on paths.
//   - **Only what is inside the project counts.** The folder may be a subdirectory of a larger
//     repository, where porcelain paths are relative to that repository's root — naming files
//     outside the project would be both wrong (the model cannot read them relative to the
//     folder) and a leak of a surrounding tree's shape. The repo's prefix is stripped and
//     anything outside it is dropped.
const (
	// MaxGitEntries is how many changed paths the state carries before it says how many it left
	// out. A tree with more changes than this is one the model should ask about, not read as a
	// list.
	MaxGitEntries = 40
	// gitTimeout bounds the whole probe, not each call: three `git` invocations on a normal
	// checkout are about 11 ms each (measured on the KiChat tree, 20 runs), and a repository
	// where they are not is one whose answer is not worth waiting for.
	gitTimeout = 3 * time.Second
)

// ErrNoGit is the folder not being a git working tree — or git not being installed, which from
// the caller's side is the same answer: there is no state to report, and no failure to show.
var ErrNoGit = errors.New("the folder is not a git working tree")

// GitScope is how much uncommitted work lies in a set of pathspecs, split by what a git command
// would destroy.
//
// It exists for one decision rather than for the prompt: a command that discards work should be
// refused when there is work to discard, and allowed when there is not. The counts are separate
// because the commands differ — `checkout --` throws away unstaged changes, `reset --hard` also
// throws away what was staged, and `clean` touches only what git does not track.
type GitScope struct {
	// Tracked counts pending tracked paths: staged, unstaged or deleted.
	Tracked int
	// Worktree counts tracked paths whose working copy differs from the index.
	Worktree int
	// Untracked counts paths git does not track.
	Untracked int
	// Ignored counts ignored paths, which only a `clean -x` would remove. It is non-zero only
	// when the scope was asked for with ignored paths included.
	Ignored int
	// The samples name up to five pending paths of each class, so a refusal's sentence can
	// name a few of what it is about without carrying a list nobody reads. They are kept per
	// class because the commands differ: `clean` must not be shown a modified file it leaves
	// alone. The counts above stay complete whatever the samples hold.
	TrackedSamples   []string
	WorktreeSamples  []string
	UntrackedSamples []string
	IgnoredSamples   []string
}

// maxGitSamples is how many paths a refusal can name.
const maxGitSamples = 5

// GitEntry is one path the working tree reports as changed.
type GitEntry struct {
	// Path is what git reported, relative to the project folder. A rename keeps git's own
	// `old -> new` form.
	Path string
	// Index and Worktree are the two halves of git's porcelain status: the staged code and the
	// unstaged one. Both are ' ' for an untracked path, whose pair is "??".
	Index    byte
	Worktree byte
	// Added and Deleted are the line counts of the change against HEAD, and Counted says whether
	// git reported them: an untracked file and a binary one have no line diff.
	Added   int
	Deleted int
	Counted bool
}

// GitState is the project folder's working tree, as far as the prompt needs it.
type GitState struct {
	// Branch is the checked-out branch, empty when HEAD is detached.
	Branch string
	// Detached reports a HEAD that is not on a branch.
	Detached bool
	// Ahead and Behind are the branch's position against its upstream, from the local
	// remote-tracking ref. Zero when there is no upstream, which is not the same as being level
	// with one — git reports nothing at all in that case.
	Ahead  int
	Behind int
	// Entries is up to MaxGitEntries of the changed paths, and Total is how many there were
	// before the cap.
	Entries []GitEntry
	Total   int
}

// GitState reads the working tree's state from the folder.
//
// It answers ErrNoGit — wrapped, with git's own sentence when there is one — for a folder that
// is not in a working tree, for a machine without git, and for a probe that ran out of time.
// None of those is a fault worth showing: each means the prompt simply carries no block.
func (r Root) GitState() (GitState, error) {
	if r.path == "" {
		return GitState{}, ErrNoRoot
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	// The prefix first: with `--porcelain` git ignores relative-path config and reports every
	// path from the repository root, so this is what turns those into paths from the folder.
	prefix, err := gitOutput(ctx, r.path, "rev-parse", "--show-prefix")
	if err != nil {
		return GitState{}, err
	}
	prefix = strings.TrimSuffix(strings.TrimSpace(prefix), "\r")

	status, err := gitOutput(ctx, r.path, "status", "--porcelain=v1", "--branch",
		"--untracked-files=normal", "--", ".")
	if err != nil {
		return GitState{}, err
	}
	state := parseGitStatus(status, prefix)

	// No changed path means no numbers to add, and on an unborn HEAD the diff has nothing to
	// compare against anyway; that second case is why a failure here is not an error.
	if state.Total > 0 {
		if diff, diffErr := gitOutput(ctx, r.path, "diff", "HEAD", "--numstat", "--no-ext-diff",
			"--no-textconv", "--relative", "--", "."); diffErr == nil {
			applyGitNumstat(&state, diff)
		}
	}
	return state, nil
}

// GitDiscardScope counts the pending paths in pathspecs, which is the scope one destructive git
// command would touch.
//
// An empty pathspecs means the whole folder, which is what a bare `git reset --hard` does. The
// pathspecs are git's own, so a directory or a glob works exactly as it does on a command line;
// includeIgnored asks for ignored paths too, because `git clean -x` removes them as well and a
// refusal that undercounted would be a refusal the user could not make sense of.
func (r Root) GitDiscardScope(pathspecs []string, includeIgnored bool) (GitScope, error) {
	if r.path == "" {
		return GitScope{}, ErrNoRoot
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	prefix, err := gitOutput(ctx, r.path, "rev-parse", "--show-prefix")
	if err != nil {
		return GitScope{}, err
	}
	prefix = strings.TrimSuffix(strings.TrimSpace(prefix), "\r")

	args := []string{"status", "--porcelain=v1", "--untracked-files=normal"}
	if includeIgnored {
		args = append(args, "--ignored=traditional")
	}
	args = append(args, "--")
	if len(pathspecs) == 0 {
		args = append(args, ".")
	} else {
		args = append(args, pathspecs...)
	}
	out, err := gitOutput(ctx, r.path, args...)
	if err != nil {
		return GitScope{}, err
	}
	return parseGitScope(out, prefix), nil
}

// parseGitScope counts the four classes and keeps a few paths to name. It is the same porcelain
// format parseGitStatus reads, without the branch header and without the cap that applies to the
// prompt: a count has to be complete to be worth refusing on.
func parseGitScope(out, prefix string) GitScope {
	var scope GitScope
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "## ") || len(line) < 4 {
			continue
		}
		path, keep := projectGitPath(line[3:], prefix)
		if !keep {
			continue
		}
		entry := GitEntry{Path: path, Index: line[0], Worktree: line[1]}
		switch {
		case entry.Index == '?':
			scope.Untracked++
			scope.UntrackedSamples = appendGitSample(scope.UntrackedSamples, entry.Path)
		case entry.Index == '!':
			scope.Ignored++
			scope.IgnoredSamples = appendGitSample(scope.IgnoredSamples, entry.Path)
		default:
			scope.Tracked++
			scope.TrackedSamples = appendGitSample(scope.TrackedSamples, entry.Path)
			if entry.Worktree != ' ' {
				scope.Worktree++
				scope.WorktreeSamples = appendGitSample(scope.WorktreeSamples, entry.Path)
			}
		}
	}
	return scope
}

func appendGitSample(samples []string, path string) []string {
	if len(samples) >= maxGitSamples {
		return samples
	}
	return append(samples, path)
}

// gitOutput runs one git command in dir and returns its standard output.
//
// Standard error is kept out of the result and out of the error's own sentence: git explains a
// missing repository and a missing binary well enough for the wrapped error, and a warning a
// repository prints must not become part of what the prompt shows.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	// The config overrides come before the verb so they apply to the whole invocation. The first
	// is the inert-ness above: an fsmonitor program is named by the repository, which may be a
	// clone of somebody else's. The second keeps a path with an umlaut readable instead of
	// `"\303\274bung.go"` — in a German project that is most of them, and octal escapes also stop
	// the prefix comparison below from recognising the path as inside the folder.
	full := append([]string{"-c", "core.fsmonitor=false", "-c", "core.quotePath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	// A cancelled context kills git but not a child it forked, and a child inherits the pipe this
	// collects through — the same trap the runtime's probes close with WaitDelay.
	cmd.WaitDelay = 500 * time.Millisecond
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoGit, err)
	}
	return out.String(), nil
}

// parseGitStatus turns `git status --porcelain=v1 --branch` into the state.
//
// The entries are filtered to the folder by prefix and capped; Total still counts everything
// that survived the filter, so a caller can say how much it left out.
func parseGitStatus(out, prefix string) GitState {
	var state GitState
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "## "); ok {
			state.parseHeader(rest)
			continue
		}
		if len(line) < 4 {
			continue
		}
		path, keep := projectGitPath(line[3:], prefix)
		if !keep {
			continue
		}
		state.Total++
		if len(state.Entries) >= MaxGitEntries {
			continue
		}
		state.Entries = append(state.Entries, GitEntry{
			Path:     path,
			Index:    line[0],
			Worktree: line[1],
		})
	}
	return state
}

// parseHeader reads the branch line. Its shapes are `main...origin/main [ahead 1, behind 2]`,
// `HEAD (no branch)` and — before the first commit — `No commits yet on main`.
func (s *GitState) parseHeader(header string) {
	if branch, ok := strings.CutPrefix(header, "No commits yet on "); ok {
		s.Branch = branch
		return
	}
	if header == "HEAD (no branch)" {
		s.Detached = true
		return
	}
	if i := strings.Index(header, "..."); i >= 0 {
		s.Branch = header[:i]
	} else {
		s.Branch = header
	}
	open := strings.IndexByte(header, '[')
	shut := strings.IndexByte(header, ']')
	if open < 0 || shut < open {
		return
	}
	for _, part := range strings.Split(header[open+1:shut], ",") {
		fields := strings.Fields(part)
		if len(fields) != 2 {
			continue
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		switch fields[0] {
		case "ahead":
			s.Ahead = n
		case "behind":
			s.Behind = n
		}
	}
}

// projectGitPath trims the repository prefix from a porcelain path, reporting false for a path
// outside the folder. A rename carries two paths and is kept when its new name is inside.
func projectGitPath(path, prefix string) (string, bool) {
	if oldPath, newPath, ok := strings.Cut(path, " -> "); ok {
		rel, keep := trimGitPrefix(newPath, prefix)
		if !keep {
			return "", false
		}
		if oldRel, inside := trimGitPrefix(oldPath, prefix); inside {
			return oldRel + " -> " + rel, true
		}
		return rel, true
	}
	return trimGitPrefix(path, prefix)
}

func trimGitPrefix(path, prefix string) (string, bool) {
	if prefix == "" {
		return path, true
	}
	rel, ok := strings.CutPrefix(path, prefix)
	return rel, ok && rel != ""
}

// applyGitNumstat adds the line counts to the entries git reported them for.
//
// `git diff --numstat` prints `added\tdeleted\tpath`, with `-` instead of a number for a binary
// file. A path it does not report — an untracked one, or a rename, whose stat names both sides —
// simply keeps Counted false rather than being given a zero that reads like "nothing changed".
func applyGitNumstat(state *GitState, out string) {
	counts := make(map[string][2]int)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(strings.TrimSuffix(line, "\r"), "\t", 3)
		if len(fields) != 3 {
			continue
		}
		added, aErr := strconv.Atoi(fields[0])
		deleted, dErr := strconv.Atoi(fields[1])
		if aErr != nil || dErr != nil {
			continue
		}
		counts[fields[2]] = [2]int{added, deleted}
	}
	for i := range state.Entries {
		if n, ok := counts[state.Entries[i].Path]; ok {
			state.Entries[i].Added, state.Entries[i].Deleted = n[0], n[1]
			state.Entries[i].Counted = true
		}
	}
}
