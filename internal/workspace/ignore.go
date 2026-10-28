package workspace

// The folders a walk leaves alone, beyond the fixed name list.
//
// A project's own `.gitignore` is the only statement of "this is not source" that is kept
// current: build output, virtualenvs, coverage, editor caches, generated clients — every
// project names its own, and the fixed `skipDirs` list can only ever guess. Left unread, the
// cost is not just noise in the hits: a search walks at most `maxScannedFiles` files, so on a
// real repository the junk consumes the budget and a search for something that *is* there
// ends in `Stopped`.
//
// This is a matcher for the syntax people actually write, not a complete implementation of
// git's: comments, blank lines, `!` negation, a trailing `/` for directories only, a leading
// `/` or any interior `/` for a pattern anchored to its own directory, `**` across
// directories, `*` and `?` within one, and character classes. Rules from a nested
// `.gitignore` are applied after its parent's, and the last matching rule wins — which is what
// makes `!keep.me` work inside an ignored folder's parent.
//
// What it does not do: `.git/info/exclude`, a global `core.excludesFile`, and the
// "everything under a leading `!` re-include" edge cases that need git's own two-pass walk.
// A missed rule leaves a file searched that git would have ignored, which is the harmless
// direction — the opposite mistake would hide source from the model.

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// maxIgnoreFiles bounds how many `.gitignore` files one walk reads. A tree with thousands of
// them is not a project, it is a vendored dump, and the walk has a budget of its own.
const maxIgnoreFiles = 256

// ignoreRule is one line of a `.gitignore`, compiled.
type ignoreRule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

// ignoreTree answers "is this path ignored" for the .gitignore files above it.
//
// The files are read on demand rather than up front: a project with one at the root — the
// common case — costs one read, and the ones in subdirectories are only touched when the walk
// enters those directories. Parsed rules are cached, so the ancestor walk for each file does
// not re-read anything.
type ignoreTree struct {
	root    string
	byDir   map[string][]ignoreRule
	missing map[string]bool
	read    int
}

func newIgnoreTree(root string) *ignoreTree {
	return &ignoreTree{
		root:    root,
		byDir:   map[string][]ignoreRule{},
		missing: map[string]bool{},
	}
}

// ignored reports whether path (absolute, inside root) is excluded.
//
// isDir matters for a rule that ends in `/`: `.venv/` ignores a folder and not a file of the
// same name.
//
// Two things are asked, in this order. First, whether any *parent* is excluded — because a
// parent that is excluded hides everything under it, which is git's own rule and the reason a
// nested `.gitignore` cannot rescue a file from an excluded folder. Then whether the path
// itself is excluded by the rules of the directories above it, outermost first, so a nested
// file's own rules are applied after its parent's and the last match wins.
func (t *ignoreTree) ignored(path string, isDir bool) bool {
	for _, dir := range t.dirChain(filepath.Dir(path)) {
		if dir == path {
			continue
		}
		if t.excluded(dir, true) {
			return true
		}
	}
	return t.excluded(path, isDir)
}

// excluded applies the rules of the directories above p — root down to p's own directory — to
// p. A directory's own `.gitignore` describes its contents rather than itself, which is why
// the chain stops at the parent in both the file and the directory case.
func (t *ignoreTree) excluded(p string, isDir bool) bool {
	ignored := false
	for _, dir := range t.dirChain(filepath.Dir(p)) {
		if verdict, matched := t.verdict(dir, p, isDir); matched {
			ignored = verdict
		}
	}
	return ignored
}

// dirChain lists the directories from the root down to dir, inclusive. A dir outside the root
// yields nothing, so a path that escaped — which the boundary check refuses anyway — cannot
// borrow rules from somewhere else on the disk.
func (t *ignoreTree) dirChain(dir string) []string {
	var chain []string
	for d := dir; len(chain) < maxIgnoreFiles; d = filepath.Dir(d) {
		if !strings.HasPrefix(d, t.root) {
			break
		}
		chain = append(chain, d)
		if d == t.root {
			break
		}
		if parent := filepath.Dir(d); parent == d {
			break
		}
	}
	// Outermost first: the reverse of the walk up.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// verdict applies one directory's rules and reports the last one that matched.
func (t *ignoreTree) verdict(dir, path string, isDir bool) (bool, bool) {
	rules := t.rules(dir)
	if len(rules) == 0 {
		return false, false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false, false
	}
	rel = filepath.ToSlash(rel)
	verdict, matched := false, false
	for _, rule := range rules {
		if rule.dirOnly && !isDir {
			continue
		}
		if rule.re.MatchString(rel) {
			verdict, matched = !rule.negate, true
		}
	}
	return verdict, matched
}

// rules reads and caches the .gitignore in dir.
func (t *ignoreTree) rules(dir string) []ignoreRule {
	if rules, ok := t.byDir[dir]; ok {
		return rules
	}
	if t.missing[dir] || t.read >= maxIgnoreFiles {
		return nil
	}
	t.read++
	file, err := os.Open(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.missing[dir] = true
		return nil
	}
	defer file.Close()
	var rules []ignoreRule
	scanner := bufio.NewScanner(file)
	// A generated .gitignore can carry very long lines; a short cap would silently drop
	// them, which is the direction that searches junk.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		if rule, ok := parseIgnoreLine(scanner.Text()); ok {
			rules = append(rules, rule)
		}
	}
	t.byDir[dir] = rules
	return rules
}

// parseIgnoreLine compiles one line of a .gitignore. ok is false for a blank line, a comment
// and a pattern this matcher cannot express.
func parseIgnoreLine(line string) (ignoreRule, bool) {
	// Leading whitespace is trimmed as well as trailing. git keeps a leading space as part of
	// the pattern, but a file whose name starts with one is not a thing anybody means, while
	// an indented comment in a hand-edited .gitignore is — and reading it as a pattern would
	// hide a file rather than nothing.
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}
	rule := ignoreRule{}
	if strings.HasPrefix(line, "!") {
		rule.negate = true
		line = line[1:]
	}
	if line == "" {
		return ignoreRule{}, false
	}
	// A trailing slash means "directories only".
	if strings.HasSuffix(line, "/") {
		rule.dirOnly = true
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			return ignoreRule{}, false
		}
	}
	// Anything with a slash is anchored to the directory holding the .gitignore; a bare name
	// matches at any depth below it.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return ignoreRule{}, false
	}
	body := globToRegexp(line)
	var pattern string
	if anchored {
		pattern = "^" + body + "$"
	} else {
		pattern = `(^|/)` + body + "$"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return ignoreRule{}, false
	}
	rule.re = re
	return rule, true
}

// globToRegexp turns a gitignore glob into the body of a regular expression.
//
// `**/` matches zero or more directories, `**` inside a pattern matches across them, `*` and
// `?` stop at a separator, and a `[...]` class is passed through — git uses the same bracket
// syntax Go's regexp does, with `!` accepted as the negation of a class.
func globToRegexp(glob string) string {
	var sb strings.Builder
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				// `**/` — and any leading `**` — also matches nothing at all.
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					sb.WriteString(`(.*/)?`)
				} else {
					sb.WriteString(`.*`)
				}
				continue
			}
			sb.WriteString(`[^/]*`)
		case '?':
			sb.WriteString(`[^/]`)
		case '[':
			end := strings.IndexByte(glob[i:], ']')
			if end < 0 {
				sb.WriteString(`\[`)
				continue
			}
			class := glob[i : i+end+1]
			// git spells a negated class `[!a]`; regexp wants `[^a]`.
			class = strings.Replace(class, "[!", "[^", 1)
			sb.WriteString(class)
			i += end
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return sb.String()
}
