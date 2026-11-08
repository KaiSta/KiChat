package workspace

// Finding a file by name, which is the one question the read tools could not answer.
//
// `list_dir` is one level and `search_files` looks for text, so "which files are in this
// tree" had no cheap answer: a model either guessed a path and was refused, or read folder
// after folder. A glob is the shape it already knows from every other tool, and it is also
// the shape a person writes in a terminal — `**/*.go`, `internal/*/doc.go`, `*.md`.

import (
	"errors"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// MaxFindResults is how many paths one call returns. A cap exists because a pattern like
// `**` in a large tree would otherwise spend the request's whole room on filenames.
const MaxFindResults = 500

// maxFindScanned bounds the walk the same way a search's budget does: a pattern that matches
// nothing still costs every file it looked at.
const maxFindScanned = 20000

// Found is what one glob turned up.
type Found struct {
	Pattern string
	// Paths are workspace-relative and sorted, so two calls with the same tree agree.
	Paths []string
	// Scanned is how many files were considered.
	Scanned int
	// Truncated is "there were more matches than the limit", which the tool has to say: a
	// list that stops at its cap without saying so reads as the whole tree.
	Truncated bool
	// ScanLimit means the walk stopped before all files were checked.
	ScanLimit bool
	// Ignored counts what a .gitignore excluded, as Search reports it.
	Ignored int
}

// Find returns the files whose workspace-relative path matches pattern.
//
// A pattern without a slash matches the file's own name at any depth, which is what a person
// typing `*.md` means. One with a slash is anchored to the project root. `*` and `?` stop at a
// directory separator and `**` crosses them, so `internal/**/*_test.go` is the tree under
// internal while `internal/*_test.go` is only its top level.
//
// The walk skips what a search skips — the fixed build and dependency folders, and whatever
// the project's own .gitignore names — because a list of generated files is not the answer to
// "where is this".
func (r Root) Find(pattern string, max int) (Found, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return Found{}, errors.New("the pattern is empty")
	}
	if r.path == "" {
		return Found{}, ErrNoRoot
	}
	if max <= 0 || max > MaxFindResults {
		max = MaxFindResults
	}
	re, err := globPathRegexp(pattern)
	if err != nil {
		return Found{}, err
	}
	out := Found{Pattern: pattern}
	ignores := newIgnoreTree(r.path)
	walkErr := filepath.WalkDir(r.path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path != r.path && (skipDirs[entry.Name()] || ignores.ignored(path, true)) {
				out.Ignored++
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || ignores.ignored(path, false) {
			return nil
		}
		if out.Scanned >= maxFindScanned {
			out.Truncated = true
			out.ScanLimit = true
			return fs.SkipAll
		}
		out.Scanned++
		rel := r.Relative(path)
		if re.MatchString(rel) {
			if len(out.Paths) >= max {
				out.Truncated = true
				return fs.SkipAll
			}
			out.Paths = append(out.Paths, rel)
		}
		return nil
	})
	if walkErr != nil {
		return Found{}, walkErr
	}
	sort.Strings(out.Paths)
	return out, nil
}

// globPathRegexp compiles a path pattern for Find. It is the same glob the ignore matcher
// uses — one implementation, so a pattern that finds a file and a `!`-rule that excludes it
// cannot disagree about what `**/` means.
func globPathRegexp(pattern string) (*regexp.Regexp, error) {
	pattern = filepath.ToSlash(strings.TrimPrefix(filepath.ToSlash(pattern), "./"))
	if pattern == "" {
		return nil, errors.New("the pattern is empty")
	}
	body := globToRegexp(pattern)
	if strings.Contains(pattern, "/") {
		return regexp.Compile("^" + body + "$")
	}
	// A bare name is matched at any depth, like a .gitignore rule without a slash.
	return regexp.Compile(`(^|/)` + body + "$")
}
