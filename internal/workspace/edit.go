package workspace

// The write side of a project folder: two operations, each described before it is made.
//
// The split between Describe* and the operation itself is the whole design here, and it
// is not a convenience. A person cannot consent to "write_file pkg/a.go" — that is a tool
// name, not a change — and asking them *after* the file was written is not consent at
// all. So every modification is computed twice: once to be shown, and once to be
// performed. The second computation is deliberately independent, because a described
// change is a value that travels through a window and comes back, and a span of time in
// which the folder can move under it is exactly when a preview stops matching what gets
// applied.
//
// Nothing here asks anybody anything. Whether a change is allowed is the app's question
// (internal/tools carries it to the window), and this package stays the layer that can say
// what a change *is* and can keep it inside the folder.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The operations, as a value rather than a sentence: the window translates them.
const (
	// OperationCreate is a file that does not exist yet.
	OperationCreate = "create"
	// OperationOverwrite replaces a file's whole contents.
	OperationOverwrite = "overwrite"
	// OperationEdit replaces one occurrence — or every one — of a string inside a file.
	OperationEdit = "edit"
)

// Limits on what one call may do.
const (
	// MaxWriteBytes bounds a single write, and the size of a file that may be overwritten.
	// A model asked to rewrite a half-gigabyte file is a mistake worth refusing, and a
	// refusal it can read is better than an out-of-memory kill halfway through a write
	// that has already truncated the file.
	MaxWriteBytes = 4 << 20
	// MaxChangePreviewBytes caps one side of a preview. The change itself is not capped by
	// this: it exists so that a 4 MB rewrite is shown as the first 16 kB and a line saying
	// so, rather than freezing the window while it renders.
	MaxChangePreviewBytes = 16 << 10
)

// Change is a modification to one file, described before it is made.
type Change struct {
	// Path is workspace-relative, the form the model wrote.
	Path string
	// Operation is one of OperationCreate, OperationOverwrite, OperationEdit.
	Operation string
	// Before is the text being replaced — the file's contents, or the matched string for
	// an edit. Empty for a create.
	Before string
	// After is the text that will be there instead.
	After string
	// Added and Removed are line counts, not a diff: for an edit they are the two
	// snippets, and for a write they are the file before and after. They are what lets the
	// window say "14 lines become 210" without showing either.
	Added   int
	Removed int
	// Truncated is "a preview here is shorter than the change", so the window can say the
	// diff it shows is not the whole of it.
	Truncated bool
	// CreatesDirs lists folders that will be created to hold the file, outermost first.
	// They are part of what is being approved: a typo in a path makes folders, and a
	// listing that says so is how the person notices before rather than after.
	CreatesDirs []string
}

// WriteResult is what a write did.
type WriteResult struct {
	// Path is workspace-relative.
	Path    string
	Created bool
	// Lines is the file's length after the write, PreviousLines before it.
	Lines         int
	PreviousLines int
}

// EditResult is what an edit did.
type EditResult struct {
	Path string
	// Occurrences is how many matches were replaced, and Line the first one's number.
	Occurrences int
	Line        int
	// Added and Removed are the line counts of the two snippets.
	Added   int
	Removed int
	// Approximate is "the text matched once its indentation was ignored". The edit still
	// replaced the file's real text, but the caller says so: a model that learns this
	// succeeded with different whitespace will keep guessing, and the person reading the
	// card deserves to know the line they picture is not byte-for-byte the one it found.
	Approximate bool
	// Digest is the fingerprint of the text the edit left behind. It is computed here because
	// this is the one place the new text exists before it reaches the disk, and the undo record
	// needs it to notice a file that changed afterwards (see internal/tools/snapshot.go).
	Digest string
}

// DescribeWrite reports what writing content to rel would do.
//
// An error here means the write cannot happen at all — a folder where a file was named, a
// file too large to replace — and the caller must not ask anybody for permission for a
// change that was never possible.
func (r Root) DescribeWrite(rel, content string) (Change, error) {
	abs, before, mode, err := r.inspectWrite(rel, content)
	if err != nil {
		return Change{}, err
	}
	change := Change{
		Path:    r.Relative(abs),
		Added:   countLines(content),
		Removed: countLines(before),
	}
	if mode == 0 {
		change.Operation = OperationCreate
	} else {
		change.Operation = OperationOverwrite
	}
	change.CreatesDirs = r.missingDirs(abs)
	change.Before, change.After, change.Truncated = preview(before, content)
	return change, nil
}

// Write writes content to rel, creating the file and any missing folders above it.
//
// The file is written in place rather than by renaming a temporary over it. A rename
// would break what a working tree is full of: hard links, a file's owner and group, and
// every watcher that holds the old inode — the last of which is a running dev server
// noticing the change, which is the point of editing a project from inside it. What is
// lost instead is atomicity: a crash mid-write leaves a truncated file. For a text file a
// person can see and a version control system can restore, that is the better trade, and
// it is what every editor in the same folder does.
//
// The content is written exactly as given, including its final newline or lack of one. A
// tool that helpfully appended one would put a line in somebody's diff that the model
// never wrote, and a model can be told to end a file with a newline.
func (r Root) Write(rel, content string) (WriteResult, error) {
	abs, before, mode, err := r.inspectWrite(rel, content)
	if err != nil {
		return WriteResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return WriteResult{}, err
	}
	created := mode == 0
	if created {
		// A new file: the mode a text file gets, adjusted by the umask. An existing file
		// keeps its own bits, which os.WriteFile does by itself when the file is there.
		mode = 0o644
	}
	if err := os.WriteFile(abs, []byte(content), mode); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{
		Path:          r.Relative(abs),
		Created:       created,
		Lines:         countLines(content),
		PreviousLines: countLines(before),
	}, nil
}

// inspectWrite resolves a path and reports what is at it: its text, and the permission
// bits to keep when it is replaced (zero when there is nothing there).
//
// Both bounds are checked here rather than in each caller, because a bound enforced on
// one path and not the other is the bug this pair of methods has to avoid: DescribeWrite
// is what decides whether anybody is asked, so a size it accepted and Write refused
// would ask for permission for something that cannot happen.
func (r Root) inspectWrite(rel, content string) (abs, before string, mode fs.FileMode, err error) {
	if len(content) > MaxWriteBytes {
		return "", "", 0, fmt.Errorf("the text to write is %s, larger than the %s a single write may hold",
			humanBytes(int64(len(content))), humanBytes(MaxWriteBytes))
	}
	abs, err = r.Resolve(rel)
	if err != nil {
		return "", "", 0, err
	}
	if abs == r.path {
		return "", "", 0, fmt.Errorf("the project folder itself cannot be written as a file")
	}
	info, statErr := os.Stat(abs)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return abs, "", 0, nil
		}
		return "", "", 0, statErr
	}
	if info.IsDir() {
		return "", "", 0, fmt.Errorf("%s is a folder", r.Relative(abs))
	}
	if info.Size() > MaxWriteBytes {
		return "", "", 0, fmt.Errorf("%s is %s, larger than the %s a single write may replace",
			r.Relative(abs), humanBytes(info.Size()), humanBytes(MaxWriteBytes))
	}
	data, readErr := os.ReadFile(abs)
	if readErr != nil {
		return "", "", 0, readErr
	}
	return abs, string(data), info.Mode().Perm(), nil
}

// missingDirs lists the folders that a write to abs would create, outermost first.
func (r Root) missingDirs(abs string) []string {
	var missing []string
	for dir := filepath.Dir(abs); dir != r.path && inside(r.path, dir); dir = filepath.Dir(dir) {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		missing = append([]string{r.Relative(dir)}, missing...)
	}
	return missing
}

// DescribeEdit reports what replacing old with new inside rel would do, and refuses
// exactly the edits that Edit would refuse — so a person is never asked to approve
// something that cannot be applied.
func (r Root) DescribeEdit(rel, old, new string, replaceAll bool) (Change, error) {
	abs, _, _, _, matched, _, err := r.inspectEdit(rel, old, replaceAll)
	if err != nil {
		return Change{}, err
	}
	change := Change{
		Path:      r.Relative(abs),
		Operation: OperationEdit,
		Added:     countLines(new),
		Removed:   countLines(matched),
	}
	// The preview shows what would actually be replaced, which for an approximate match is
	// the file's own lines rather than the ones the model typed.
	change.Before, change.After, change.Truncated = preview(matched, new)
	return change, nil
}

// Edit replaces a string inside a file.
//
// The match is exact, and the refusals are the tool's answer to a model that guessed at
// whitespace: it reports how many times the text was found, and where, so the model can
// read the file again and copy the line it meant. Fuzzy matching was rejected on purpose —
// it would silently succeed on the wrong line, which is the one outcome an editing agent
// must not produce, because the user's next action is to trust the diff.
//
// The file is read whole, so the newline convention, the byte-order mark and the final
// newline all survive an edit they are not part of: this is a string replacement inside
// text, not a re-rendering of it.
func (r Root) Edit(rel, old, new string, replaceAll bool) (EditResult, error) {
	abs, text, mode, occurrences, matched, approx, err := r.inspectEdit(rel, old, replaceAll)
	if err != nil {
		return EditResult{}, err
	}
	// One occurrence, because inspectEdit has already refused an ambiguous match: an edit
	// whose text appears twice is an error, never the first of the two.
	updated := strings.Replace(text, matched, new, 1)
	if replaceAll {
		updated = strings.ReplaceAll(text, matched, new)
	}
	if err := os.WriteFile(abs, []byte(updated), mode); err != nil {
		return EditResult{}, err
	}
	replaced := 1
	if replaceAll {
		replaced = occurrences
	}
	return EditResult{
		Path:        r.Relative(abs),
		Occurrences: replaced,
		Line:        lineOf(text, matched),
		Added:       countLines(new),
		Removed:     countLines(matched),
		Approximate: approx,
		Digest:      Digest(updated),
	}, nil
}

// inspectEdit reads the file to be edited and refuses the edit if it cannot be made.
// inspectEdit reads the file to be edited and refuses the edit if it cannot be made.
//
// It returns the text it actually matched, which is `old` itself when the match was exact and
// the file's own lines when the match was approximate: what gets replaced has to be the bytes
// that are in the file, whatever the model typed.
func (r Root) inspectEdit(rel, old string, replaceAll bool) (abs, text string, mode fs.FileMode, occurrences int, matched string, approx bool, err error) {
	abs, err = r.Resolve(rel)
	if err != nil {
		return "", "", 0, 0, "", false, err
	}
	if strings.TrimSpace(old) == "" {
		return "", "", 0, 0, "", false, fmt.Errorf("the text to replace is empty")
	}
	info, statErr := os.Stat(abs)
	if statErr != nil {
		return "", "", 0, 0, "", false, statErr
	}
	if info.IsDir() {
		return "", "", 0, 0, "", false, fmt.Errorf("%s is a folder", r.Relative(abs))
	}
	if info.Size() > MaxWriteBytes {
		return "", "", 0, 0, "", false, fmt.Errorf("%s is %s, larger than the %s an edit may touch",
			r.Relative(abs), humanBytes(info.Size()), humanBytes(MaxWriteBytes))
	}
	data, readErr := os.ReadFile(abs)
	if readErr != nil {
		return "", "", 0, 0, "", false, readErr
	}
	text = string(data)
	if isBinary(data[:min(len(data), maxSniffBytes)]) {
		return "", "", 0, 0, "", false, fmt.Errorf("%s is not a text file", r.Relative(abs))
	}
	occurrences = strings.Count(text, old)
	matched, approx = old, false
	switch {
	case occurrences == 0:
		// A second attempt that ignores indentation, because the usual reason a model's
		// text is not found is that it reproduced the line with different leading
		// whitespace — a tab where the file has four spaces — and the alternative is a
		// round spent re-reading a file it already read.
		//
		// What it deliberately does not do: match a *different* line. The comparison is
		// per line after trimming, so a word changed, a line added or a line reordered all
		// fail here exactly as they did before, and an ambiguous approximate match is
		// refused like an ambiguous exact one.
		approxMatches, found := approximateMatches(text, old)
		switch {
		case found == 0:
			return "", "", 0, 0, "", false, ErrNoMatch
		case found > 1 && !replaceAll:
			return "", "", 0, 0, "", false, &AmbiguousMatch{
				Path: r.Relative(abs), Count: found, FirstLine: lineOf(text, approxMatches[0])}
		}
		matched, approx = approxMatches[0], true
		occurrences = found
	case occurrences > 1 && !replaceAll:
		return "", "", 0, 0, "", false, &AmbiguousMatch{Path: r.Relative(abs), Count: occurrences, FirstLine: lineOf(text, old)}
	}
	return abs, text, info.Mode().Perm(), occurrences, matched, approx, nil
}

// approximateMatches finds the spans of text that equal old once each line's leading and
// trailing whitespace is ignored.
//
// It returns the file's own text for each span, in order, so the caller replaces bytes that
// exist rather than bytes the model imagined. Line endings are normalised first: a model on
// one platform writing CRLF into a file that uses LF is the same mistake as indentation, and
// the span it returns keeps whatever the file actually has.
func approximateMatches(text, old string) ([]string, int) {
	textLines := strings.Split(text, "\n")
	oldLines := strings.Split(strings.ReplaceAll(old, "\r\n", "\n"), "\n")
	// A trailing newline in the searched text is a separator, not an empty last line.
	if n := len(oldLines); n > 0 && oldLines[n-1] == "" {
		oldLines = oldLines[:n-1]
	}
	if len(oldLines) == 0 {
		return nil, 0
	}
	trim := func(line string) string {
		return strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	}
	var spans []string
	// Offsets of each line's start, so a span can be cut out of the original text with the
	// bytes the file has rather than a re-join that would flatten a lone \r.
	start := 0
	offsets := make([]int, len(textLines))
	for i, line := range textLines {
		offsets[i] = start
		start += len(line) + 1
	}
	for i := 0; i+len(oldLines) <= len(textLines); i++ {
		matches := true
		for j := range oldLines {
			if trim(textLines[i+j]) != trim(oldLines[j]) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		begin := offsets[i]
		end := offsets[i+len(oldLines)-1] + len(textLines[i+len(oldLines)-1])
		spans = append(spans, text[begin:end])
	}
	return spans, len(spans)
}

// ErrNoMatch is an edit whose text is not in the file — usually whitespace the model
// guessed at rather than copied.
var ErrNoMatch = fmt.Errorf("the text to replace is not in the file")

// AmbiguousMatch is an edit whose text appears more than once, refused rather than
// guessed at. It carries the first occurrence's line so the refusal can tell the model
// where to look instead of only that it failed.
type AmbiguousMatch struct {
	Path  string
	Count int
	// FirstLine is the 1-based line of the first occurrence.
	FirstLine int
}

func (e *AmbiguousMatch) Error() string {
	return fmt.Sprintf("the text to replace appears %d times in %s, first at line %d",
		e.Count, e.Path, e.FirstLine)
}

// preview renders the two sides of a change for display, cutting each to
// MaxChangePreviewBytes on a line boundary and reporting whether either was cut.
func preview(before, after string) (shownBefore, shownAfter string, truncated bool) {
	shownBefore, cutBefore := clampPreview(before)
	shownAfter, cutAfter := clampPreview(after)
	return shownBefore, shownAfter, cutBefore || cutAfter
}

// clampPreview cuts text to the preview budget at the last whole line inside it, so the
// page ends on a sentence rather than mid-token.
func clampPreview(text string) (string, bool) {
	if len(text) <= MaxChangePreviewBytes {
		return text, false
	}
	cut := text[:MaxChangePreviewBytes]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut, true
}

// countLines is what the change totals show: the number of lines a reader would count,
// where a file not ending in a newline still has a last line.
func countLines(text string) int {
	if text == "" {
		return 0
	}
	lines := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		lines++
	}
	return lines
}

// lineOf is the 1-based line number of a substring's first occurrence, or 0.
func lineOf(text, needle string) int {
	index := strings.Index(text, needle)
	if index < 0 {
		return 0
	}
	return strings.Count(text[:index], "\n") + 1
}

// humanBytes renders a size the way a refusal should read: a person reading "4194304"
// has to count the digits to know whether they care.
func humanBytes(size int64) string {
	switch {
	case size < 1024:
		return fmt.Sprintf("%d bytes", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.0f kB", float64(size)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	}
}
