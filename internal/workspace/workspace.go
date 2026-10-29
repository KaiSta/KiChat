// Package workspace owns the folder a coding project works in.
//
// It answers one question — which file on disk does this path mean — and it answers
// it in one place, because the answer is a boundary rather than a convenience. The
// model writes the paths in its tool calls, so a path that leaves the folder would
// let it read anything the user can read; three tools each doing their own
// filepath.Join would be three chances to get that wrong, and the next tool to be
// added is the one that would forget.
//
// It reads, and it writes exactly two things: a file's whole contents, and a string
// inside one (edit.go). Every modification is described before it is made, so the person
// who has to allow it is shown the change rather than a tool name — and nothing here asks
// anybody whether it is allowed, because that is the app's question and this package is
// the layer that can say what a change *is*.
//
// It also holds the one thing that has to be read out of the folder without being
// asked for: the project's own AGENTS.md, which the context assembly folds into the
// system prompt every turn (see AGENTS.md's discovery rule below).
package workspace

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Limits. They are exported because a caller that has to *say* what it left out —
// "the file is longer than this" — must be able to phrase it against the same number
// the read used, and because a test asserting a bound should not restate it.
const (
	// MaxReadLines is the largest window one read returns. A bigger window is not a
	// favour: it is charged against the turn's tool budget, and a file that does not
	// fit is better read in two calls the model chose than in one it did not.
	MaxReadLines = 2000
	// MaxReadBytes bounds the same window by size, because lines are a poor unit for
	// a file that is one minified line. Whichever bound is reached first cuts.
	MaxReadBytes = 80 << 10
	// MaxListEntries is how many entries one listing returns before it says how many
	// it left out. A folder of 40000 files is a listing nobody reads.
	MaxListEntries = 300
	// MaxSearchHits is the same for a text search.
	MaxSearchHits = 200
	// MaxAgentsBytes is the largest AGENTS.md folded into the prompt. The file is
	// written by whoever owns the repository, so it can be any size at all, and a
	// 2 MB one would spend the whole window before the first question is asked.
	MaxAgentsBytes = 48 << 10

	// maxScannedFiles and maxScannedBytes bound a search's walk. They exist because a
	// search is the one read tool whose cost is not knowable in advance: it walks
	// until it finds what it is looking for, and what it is looking for may not be
	// there.
	maxScannedFiles = 4000
	maxScannedBytes = 1 << 20
	// maxSniffBytes is how much of a file is examined before calling it binary.
	maxSniffBytes = 8000
)

// Errors a caller classifies rather than prints. The text is for a log; the window
// and the model are given sentences by internal/tools, which owns the dictionaries.
var (
	// ErrNoRoot is a chat with no folder. It is a state rather than a mistake: the
	// file tools are only advertised to a chat that has one.
	ErrNoRoot = errors.New("this chat has no workspace folder")
	// ErrOutside is a path that resolves outside the folder, including a symlink that
	// leaves it. It is the refusal this package exists for.
	ErrOutside = errors.New("the path is outside the workspace folder")
)

// Root is a resolved project folder.
type Root struct {
	// path is absolute and symlink-free. It is unexported so a Root that was never
	// opened — the zero value — cannot be mistaken for one that can read files: every
	// method on an unopened Root answers ErrNoRoot.
	path string
}

// Open resolves the folder a project was created on.
//
// The path is made absolute and its symlinks resolved once, here, rather than on
// every call. A folder that is itself reached through a symlink — /tmp on macOS, a
// home directory on a machine that moved it, a project reached through a symlinked
// parent — would otherwise make every containment check compare a resolved child
// against an unresolved parent, which refuses paths that are plainly inside the
// folder. Resolving once also means the path the window shows is the path on disk.
func Open(dir string) (Root, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return Root{}, ErrNoRoot
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Root{}, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Root{}, fmt.Errorf("%s: %w", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return Root{}, err
	}
	if !info.IsDir() {
		return Root{}, fmt.Errorf("%s is not a folder", resolved)
	}
	return Root{path: resolved}, nil
}

// Path is the folder as it is on disk, absolute.
func (r Root) Path() string { return r.path }

// Resolve turns a path the model wrote into an absolute path inside the folder.
//
// An absolute path is accepted when it points inside the folder: a model that has just
// been shown /home/me/proj/main.go will reasonably write that back, and refusing it
// costs a round for nothing. Everything else is ErrOutside — a `..` that climbs out,
// another folder, an empty string that would have meant "wherever the app is", and a
// symlink that leaves the tree even though the link itself is inside it.
//
// A path whose last element does not exist is checked through its deepest existing
// ancestor, so the answer is about the folder it would land in rather than about the
// leaf. That is what a later write tool needs; today it also means a mistyped name is
// reported as missing rather than as out of bounds.
func (r Root) Resolve(rel string) (string, error) {
	if r.path == "" {
		return "", ErrNoRoot
	}
	rel = strings.TrimSpace(rel)
	// Models quote their arguments and sometimes write the folder's root as "/", and
	// both mean the folder itself rather than something outside it.
	rel = strings.Trim(rel, `"'`)
	if rel == "" || rel == "." || rel == "/" || rel == "./" {
		return r.path, nil
	}
	candidate := rel
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(r.path, candidate)
	}
	candidate = filepath.Clean(candidate)
	if !inside(r.path, candidate) {
		return "", fmt.Errorf("%s: %w", candidate, ErrOutside)
	}
	// The parent chain is resolved through the deepest element that exists. A link
	// inside the folder that points out of it is the one shape a textual check cannot
	// see, because the path it reads is inside the folder.
	real, err := resolveExisting(candidate)
	if err != nil {
		return "", err
	}
	if !inside(r.path, real) {
		return "", fmt.Errorf("%s: %w", candidate, ErrOutside)
	}
	return real, nil
}

// resolveExisting resolves candidate through its deepest existing ancestor.
func resolveExisting(candidate string) (string, error) {
	dir := candidate
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			rest, relErr := filepath.Rel(dir, candidate)
			if relErr != nil {
				return resolved, nil
			}
			return filepath.Join(resolved, rest), nil
		}
		// A missing or unreadable element is not the answer here: the leaf of a path
		// that is about to be created never exists, and the containment question is
		// about the directory that does.
		parent := filepath.Dir(dir)
		if parent == dir {
			// Nothing on the way exists. The root itself was checked by Open, so the
			// candidate is inside the folder by construction.
			return candidate, nil
		}
		dir = parent
	}
}

// inside reports whether path is root itself or below it.
//
// filepath.Rel is the comparison rather than a string prefix on purpose: "/proj" is a
// prefix of "/project", and a prefix test would call a sibling folder part of the
// project.
func inside(root, path string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		// Different volumes on Windows, which cannot be a path inside root.
		return false
	}
	if rel == "." {
		return true
	}
	if filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Relative renders an absolute path inside the folder the way the model wrote it: as a
// slash-separated path relative to the folder's root. A path outside the folder is
// returned unchanged, because the only caller that has one is reporting on it.
func (r Root) Relative(abs string) string {
	if r.path == "" {
		return abs
	}
	rel, err := filepath.Rel(r.path, abs)
	if err != nil || rel == "" {
		return abs
	}
	if rel == "." {
		return "."
	}
	if strings.HasPrefix(rel, "..") {
		return abs
	}
	return filepath.ToSlash(rel)
}

// Text is one file as it is on disk, for the two callers that need the file rather than a
// window into it: the pre-image kept before a change, and the diff a revert shows.
type Text struct {
	// Path is workspace-relative.
	Path string
	// Content is the whole file.
	Content string
	// Existed is false for a path that is not there yet — the ordinary case for a create, and
	// the one a revert answers by removing the file rather than by writing an empty one.
	Existed bool
	// Writable is false for a file this app will not rewrite: a binary, or one larger than a
	// change may touch. A pre-image of such a file would be a promise the undo could not keep,
	// so it is not offered rather than offered and broken.
	Writable bool
}

// Text reads a whole file the way a change or a revert needs it.
//
// A missing file is not an error here — it is the state a create starts from — and neither is a
// folder's absence: both answer Existed=false. Everything else that can go wrong (a folder
// where a file was named, a permission) is returned, because the caller is about to write.
func (r Root) Text(rel string) (Text, error) {
	abs, err := r.Resolve(rel)
	if err != nil {
		return Text{}, err
	}
	out := Text{Path: r.Relative(abs)}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return Text{}, err
	}
	if info.IsDir() {
		return Text{}, fmt.Errorf("%s is a folder, not a file", out.Path)
	}
	out.Existed = true
	if info.Size() > MaxWriteBytes {
		return out, nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Text{}, err
	}
	// The same sniff the reader uses, and for the same reason: what a revert restores has to be
	// bytes the app itself read, not a page of replacement characters.
	if isBinary(data[:min(len(data), maxSniffBytes)]) {
		return out, nil
	}
	out.Content, out.Writable = string(data), true
	return out, nil
}

// Remove deletes one file from the folder.
//
// It is the undo of a create, and it is deliberately a file: a folder is left alone, because
// the folders a write made may now hold work this chat did not put there.
func (r Root) Remove(rel string) error {
	abs, err := r.Resolve(rel)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a folder", r.Relative(abs))
	}
	return os.Remove(abs)
}

// Digest is the fingerprint of a text, used to notice that a file changed after this app wrote
// it: a revert compares it against what the tool recorded, so an edit made in the user's own
// editor in the meantime is a refusal rather than something to overwrite.
func Digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Absolute is the inverse of Relative: the path on disk a workspace-relative path names.
//
// It is not Resolve, and the difference is what it is for. Resolve validates a path a model
// wrote — `..`, an absolute path pointing elsewhere, a symlink that leaves the tree — and the
// callers here already hold a path this app produced itself, from a write it just made. Doing
// that validation again would be a second opinion about a path that came out of the folder.
func (r Root) Absolute(rel string) string {
	if r.path == "" {
		return rel
	}
	return filepath.Join(r.path, filepath.FromSlash(rel))
}

// --- reading ---------------------------------------------------------------

// File is one file's text, bounded, with what it takes to say what was left out.
type File struct {
	// Path is the workspace-relative path, the form the model asked in.
	Path string
	// Text is the window, line-numbered from From.
	Text string
	// Total is the file's length in lines, and it is known only when the whole file was
	// read: a truncated window leaves it at 0 rather than at the lower bound it stopped
	// counting at, because a lower bound printed as a length is worse than no number at
	// all. A zero Total with Truncated false is an empty file.
	Total int
	// From and Returned describe the window: 1-based first line, and how many.
	From     int
	Returned int
	// Truncated is "there are more lines after the window".
	Truncated bool
	// Binary is a file no model should be reading as text.
	Binary bool
}

// Read returns a window of a text file.
//
// Lines are numbered because the model will be asked to change one of them, and a
// number it read is what lets it name the line it means. offset is 1-based, so it can
// be handed back exactly as it appeared in a previous window.
func (r Root) Read(rel string, offset, lines int) (File, error) {
	abs, err := r.Resolve(rel)
	if err != nil {
		return File{}, err
	}
	if lines <= 0 {
		lines = 500
	}
	if lines > MaxReadLines {
		lines = MaxReadLines
	}
	if offset <= 0 {
		offset = 1
	}

	f, err := os.Open(abs)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return File{}, err
	}
	if info.IsDir() {
		return File{}, fmt.Errorf("%s is a folder, not a file", r.Relative(abs))
	}

	out := File{Path: r.Relative(abs), From: offset}
	reader := bufio.NewReaderSize(f, 64<<10)
	if sniff, err := reader.Peek(maxSniffBytes); err == nil || errors.Is(err, io.EOF) {
		if isBinary(sniff) {
			out.Binary = true
			return out, nil
		}
	}

	line := 0
	bytesUsed := 0
	var text strings.Builder
	for {
		raw, err := reader.ReadString('\n')
		atEOF := errors.Is(err, io.EOF)
		if err != nil && !atEOF {
			return File{}, err
		}
		if raw == "" && atEOF {
			break
		}
		line++
		if line < offset {
			if atEOF {
				break
			}
			continue
		}
		if out.Returned >= lines || bytesUsed+len(raw) > MaxReadBytes {
			// The file continues past the window, whether by the line bound or the byte
			// bound, so this line is the one after it and the file's length is unknown.
			out.Truncated = true
			break
		}
		// The number is padded so a file read in two windows stays a column, and the
		// newline is trimmed so the reader's own line breaks are the only ones.
		fmt.Fprintf(&text, "%6d| %s\n", line, strings.TrimSuffix(raw, "\n"))
		bytesUsed += len(raw)
		out.Returned++
		if atEOF {
			break
		}
	}
	out.Text = strings.TrimRight(text.String(), "\n")
	if !out.Truncated {
		out.Total = line
	}
	return out, nil
}

// isBinary reports whether a prefix looks like a file that is not text. A NUL byte is
// the test: it is what separates an image, an archive or a compiled object from a file
// somebody edited, and every one of them would otherwise be handed to a model as a
// page of replacement characters.
func isBinary(prefix []byte) bool {
	for _, b := range prefix {
		if b == 0 {
			return true
		}
	}
	return false
}

// --- listing ---------------------------------------------------------------

// Entry is one name in a folder.
type Entry struct {
	Name string
	Dir  bool
	Size int64
}

// Listing is one folder's contents.
type Listing struct {
	// Path is the workspace-relative folder, "." for the folder itself.
	Path string
	// Entries are folders first, then files, each alphabetically.
	Entries []Entry
	// Total is how many entries the folder holds, Omitted how many are not listed.
	Total   int
	Omitted int
}

// List returns a folder's immediate contents.
//
// Immediate rather than recursive: a walk of a repository is a search (Search) or a
// command, and a listing that descends is the one that returns 40000 entries from a
// node_modules nobody asked about. Directories are marked so the model can descend by
// asking for one by name.
func (r Root) List(rel string, entries int) (Listing, error) {
	abs, err := r.Resolve(rel)
	if err != nil {
		return Listing{}, err
	}
	if entries <= 0 || entries > MaxListEntries {
		entries = MaxListEntries
	}
	dir, err := os.ReadDir(abs)
	if err != nil {
		return Listing{}, err
	}
	out := Listing{Path: r.Relative(abs), Total: len(dir)}
	var dirs, files []Entry
	for _, e := range dir {
		entry := Entry{Name: e.Name(), Dir: e.IsDir()}
		if !entry.Dir {
			if info, err := e.Info(); err == nil {
				entry.Size = info.Size()
			}
		}
		// A symlink is reported as what it points at, because that is what reading it
		// does. A broken one stays a file with no size, which is the truth about it.
		if e.Type()&fs.ModeSymlink != 0 {
			if info, err := os.Stat(filepath.Join(abs, e.Name())); err == nil {
				entry.Dir = info.IsDir()
				entry.Size = info.Size()
			}
		}
		if entry.Dir {
			dirs = append(dirs, entry)
		} else {
			files = append(files, entry)
		}
	}
	sortEntries(dirs)
	sortEntries(files)
	all := append(dirs, files...)
	if len(all) > entries {
		out.Omitted = len(all) - entries
		all = all[:entries]
	}
	out.Entries = all
	return out, nil
}

func sortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
}

// --- searching -------------------------------------------------------------

// Hit is one matching line.
type Hit struct {
	Path string
	Line int
	Text string
}

// Matches is a text search's result.
type Matches struct {
	Query string
	Hits  []Hit
	// Scanned is how many files were read looking for the query.
	Scanned int
	// Ignored counts the files and folders a .gitignore excluded, so the model can see
	// that the answer was shaped by one rather than concluding a folder is empty.
	Ignored int
	// Stopped is "the walk ended at one of its own budgets rather than at the end of
	// the tree". It is the difference between "the string is not in the project" and
	// "it was not in the part that was read", which is a difference the model has to
	// be able to see: the two call for opposite next moves.
	Stopped bool
}

// skipDirs are the folders a search does not walk. They are the ones that hold other
// people's code or this project's own build output: nothing a model searches for is
// inside a node_modules, and reading one costs more than the whole search it was part
// of. It is a name list rather than a size threshold because a small node_modules is
// the same kind of folder as a large one.
var skipDirs = map[string]bool{
	".git": true, ".svn": true, ".hg": true,
	"node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, "out": true, "coverage": true,
	"__pycache__": true, ".venv": true, "venv": true, "env": true,
	".next": true, ".nuxt": true, ".cache": true, ".gradle": true,
	".idea": true, ".vscode": true, "Pods": true, "DerivedData": true,
}

// Search finds a literal string in the folder's files.
//
// Literal rather than a regular expression: the model writes the argument, a pattern
// it meant as text is the common case, and a malformed pattern would be answered with
// a syntax error instead of the lines it was looking for. Case-insensitive, because a
// search for a name it saw in prose is the common case too.
//
// It is a walk with a budget rather than `grep` because the first release of coding
// mode has no command tool, and because a search the app performs is one the window
// can show a count for.
func (r Root) Search(query string, maxHits int) (Matches, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Matches{}, errors.New("the search text is empty")
	}
	if r.path == "" {
		return Matches{}, ErrNoRoot
	}
	if maxHits <= 0 || maxHits > MaxSearchHits {
		maxHits = MaxSearchHits
	}
	out := Matches{Query: query}
	needle := strings.ToLower(query)
	ignores := newIgnoreTree(r.path)

	walkErr := filepath.WalkDir(r.path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable folder inside the tree is skipped rather than fatal: the
			// search is about the rest of the project.
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
		if ignores.ignored(path, false) {
			out.Ignored++
			return nil
		}
		if !entry.Type().IsRegular() {
			// Symlinks, sockets and devices: a link is followed through its own
			// listing, and following one here would walk the same tree twice.
			return nil
		}
		if out.Scanned >= maxScannedFiles || len(out.Hits) >= maxHits {
			out.Stopped = true
			return fs.SkipAll
		}
		return r.searchFile(path, needle, maxHits, &out)
	})
	if walkErr != nil {
		return Matches{}, walkErr
	}
	return out, nil
}

// searchFile reads one file looking for the needle, adding what it finds.
func (r Root) searchFile(path, needle string, maxHits int, out *Matches) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if info.Size() > maxScannedBytes {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	head, _ := bufio.NewReader(f).Peek(maxSniffBytes)
	if isBinary(head) {
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	out.Scanned++

	rel := r.Relative(path)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), maxScannedBytes)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if !strings.Contains(strings.ToLower(text), needle) {
			continue
		}
		if len(out.Hits) >= maxHits {
			return nil
		}
		out.Hits = append(out.Hits, Hit{Path: rel, Line: line, Text: strings.TrimSpace(text)})
	}
	return nil
}

// --- the project's own guidance --------------------------------------------

// Doc is a repository's AGENTS.md.
type Doc struct {
	// Path is workspace-relative, so the model can name the file it was given.
	Path string
	Text string
	// Truncated is "the file is larger than the caller's budget".
	Truncated bool
}

// AgentsFile reads the folder's AGENTS.md.
//
// ok is false when there is none, which is not an error: an empty folder the user
// asked to build something in has no guidance to give, and the system prompt says
// what to do in that case.
//
// The name is the convention the coding agents settled on, so a project that already
// carries one for another tool carries it for this one — which is the whole point of
// reading a file rather than keeping the instructions in a setting.
func (r Root) AgentsFile(maxBytes int) (Doc, bool, error) {
	if r.path == "" {
		return Doc{}, false, ErrNoRoot
	}
	return r.agentsAt(r.path, "", maxBytes)
}

// AgentsFileNear reads the nearest AGENTS.md above a file, excluding the root one.
//
// The convention every coding agent follows is that a subdirectory may carry its own
// instructions, and that they apply to everything under it: a monorepo's `api/AGENTS.md`
// says how that service is built, and a model reading a file there needs it. The root file
// already travels in the system prompt, so this starts at the file's own folder and stops
// before the root — returning the root document a second time would spend context on
// guidance the model already has.
//
// The nearest one wins rather than every one on the way up: a rule written in `api/` knows
// about the rules in the root, and a stack of three documents that disagree is worse than
// the one the person meant.
func (r Root) AgentsFileNear(rel string, maxBytes int) (Doc, bool, error) {
	if r.path == "" {
		return Doc{}, false, ErrNoRoot
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	dir := filepath.Dir(clean)
	for dir != "." && dir != string(filepath.Separator) && dir != "" {
		if doc, ok, err := r.agentsAt(filepath.Join(r.path, dir), filepath.ToSlash(dir), maxBytes); ok || err != nil {
			return doc, ok, err
		}
		dir = filepath.Dir(dir)
	}
	return Doc{}, false, nil
}

// agentsAt reads the AGENTS.md in one folder. rel is where the model should think it lives,
// which for a nested file is its folder's path rather than the root.
func (r Root) agentsAt(dir, rel string, maxBytes int) (Doc, bool, error) {
	path := filepath.Join(dir, AgentsFileName)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Doc{}, false, nil
		}
		return Doc{}, false, err
	}
	if info.IsDir() || info.Size() == 0 {
		return Doc{}, false, nil
	}
	if maxBytes <= 0 {
		maxBytes = MaxAgentsBytes
	}
	// The cache is what keeps a file read once per turn from being a file read once
	// per turn: the size and the modification time together are the file's identity,
	// so an edit is picked up on the next turn and an untouched file is not re-read.
	if doc, ok := agentsCache.get(path, info); ok {
		return trimDocAt(doc, path, rel, maxBytes), true, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Doc{}, false, err
	}
	agentsCache.put(path, info, string(raw))
	return trimDocAt(string(raw), path, rel, maxBytes), true, nil
}

// AgentsFileName is the conventional name of the file above.
const AgentsFileName = "AGENTS.md"

// trimDoc is trimDocAt for the root file, whose path is its name.
func trimDoc(text, path string, maxBytes int) Doc { return trimDocAt(text, path, "", maxBytes) }

// trimDocAt renders a document with the path the model should reason about: the root file is
// "AGENTS.md", a nested one is "api/AGENTS.md".
func trimDocAt(text, path, rel string, maxBytes int) Doc {
	shown := AgentsFileName
	if rel != "" {
		shown = filepath.ToSlash(filepath.Join(rel, AgentsFileName))
	}
	doc := Doc{Path: shown}
	if len(text) > maxBytes {
		// Cut on a line boundary so the last thing the model reads is a whole
		// sentence rather than half of one.
		cut := text[:maxBytes]
		if i := strings.LastIndexByte(cut, '\n'); i > 0 {
			cut = cut[:i]
		}
		doc.Text = cut
		doc.Truncated = true
		return doc
	}
	doc.Text = text
	return doc
}

// agentsCache holds the last few AGENTS.md files, keyed by path.
//
// It is a package-level cache because the read it saves is per *turn* while the file
// belongs to a folder, and the folder belongs to a chat that is one of several. The
// alternative — the caller holding the text — puts the invalidation rule in every
// caller instead of in the one place that knows what makes a file a different file.
var agentsCache = &docCache{entries: map[string]cachedDoc{}}

// docCacheMax bounds the cache. It is small on purpose: this is a handful of folders
// being worked in, not a filesystem index, and a map that grows with every project a
// user ever opens is a leak with a pleasant name.
const docCacheMax = 16

type cachedDoc struct {
	size    int64
	modTime time.Time
	text    string
}

type docCache struct {
	mu      sync.Mutex
	entries map[string]cachedDoc
}

func (c *docCache) get(path string, info fs.FileInfo) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[path]
	if !ok || entry.size != info.Size() || !entry.modTime.Equal(info.ModTime()) {
		return "", false
	}
	return entry.text, true
}

func (c *docCache) put(path string, info fs.FileInfo, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= docCacheMax {
		// Wholesale rather than an eviction order: the cache exists to avoid re-reading
		// a file that is read every turn, so the entries that matter are the ones just
		// written, and those are the ones this keeps.
		c.entries = map[string]cachedDoc{}
	}
	c.entries[path] = cachedDoc{size: info.Size(), modTime: info.ModTime(), text: text}
}

// ForgetAgentDocs drops the cache. It exists for tests, which build a folder, read it,
// and then edit it to check that the next read sees the change.
func ForgetAgentDocs() {
	agentsCache.mu.Lock()
	defer agentsCache.mu.Unlock()
	agentsCache.entries = map[string]cachedDoc{}
}
