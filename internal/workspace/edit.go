package workspace

// The write side of a project folder: two operations, each described before it is made.
//
// The split between Describe* and the operation itself is the whole design here, and it
// is not a convenience. A person cannot consent to "write_file pkg/a.go" — that is a tool
// name, not a change — and asking them *after* the file was written is not consent at
// all. A described change keeps the exact pre-image and replacement so that applying it
// after approval can verify the opened target and write precisely what was shown.
//
// Nothing here asks anybody anything. Whether a change is allowed is the app's question
// (internal/tools carries it to the window), and this package stays the layer that can say
// what a change *is* and can keep it inside the folder.

import (
	"fmt"
	"io"
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
)

// Change is a modification to one file, described before it is made.
type Change struct {
	// Path is workspace-relative, the form the model wrote.
	Path string
	// Operation is one of OperationCreate, OperationOverwrite, OperationEdit.
	Operation string
	// Hunks is the change as a line diff: what differs, with the unchanged lines around it
	// that make it readable. Never nil, because the window maps over it. For a replace_all it
	// shows the first place only — Occurrences says how many others there are.
	Hunks []Hunk
	// Added and Removed count the diff's lines rather than either text's: a one-line edit in a
	// long file is 1 and 1, which is the number a reader needs next to a diff of one line. For
	// a replace_all they are totals over every replacement, not one snippet's.
	Added   int
	Removed int
	// Occurrences is how many places an edit replaces — 1 for a single match, the number of
	// matches for replace_all — and 0 for a write, which has no occurrences to count. The card
	// needs it because the preview can only show one of them: an edit that replaces twelve
	// places is one pane of text here, and a person approving it has to be told the other eleven
	// exist rather than reading a diff that looks like a single line.
	Occurrences int
	// Truncated is "there is more change than these hunks show", so the window can say the
	// diff it shows is not the whole of it.
	Truncated bool
	// NewlineOnly is "the only difference is the newline at the end of the file": there are no
	// hunks to draw, and the card has to say that rather than "no line differs".
	NewlineOnly bool
	// CreatesDirs lists folders that will be created to hold the file, outermost first.
	// They are part of what is being approved: a typo in a path makes folders, and a
	// listing that says so is how the person notices before rather than after.
	CreatesDirs []string

	// The approved preview is valid only while the same path and contents remain.
	approvedPath string
	beforeDigest string
	existed      bool
	fileInfo     fs.FileInfo
	rootInfo     fs.FileInfo
	before       string
	updated      string
	editResult   EditResult
}

// Before returns the pre-image used to construct this change.
func (c Change) Before() Text {
	return Text{Path: c.Path, Content: c.before, Existed: c.existed, Writable: true}
}

// WriteResult is what a write did.
type WriteResult struct {
	// Path is workspace-relative.
	Path    string
	Created bool
	// Lines is the file's length after the write, PreviousLines before it.
	Lines         int
	PreviousLines int
	// Before is the verified pre-image for undo.
	Before Text
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
	// Before is the verified pre-image for undo.
	Before Text
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
	diff := CompareDiff(before, content)
	change := Change{
		Path:         r.Relative(abs),
		Hunks:        diff.Hunks,
		Added:        diff.Added,
		Removed:      diff.Removed,
		Truncated:    diff.Truncated,
		NewlineOnly:  diff.NewlineOnly,
		approvedPath: abs,
		beforeDigest: Digest(before),
		existed:      mode != 0,
	}
	if mode == 0 {
		change.Operation = OperationCreate
	} else {
		change.Operation = OperationOverwrite
	}
	change.CreatesDirs = r.missingDirs(abs)
	change.before, change.updated = before, content
	if err := r.captureChangeIdentity(&change); err != nil {
		return Change{}, err
	}
	return change, nil
}

// captureChangeIdentity lets the write reject replacement files even when their contents match.
func (r Root) captureChangeIdentity(change *Change) error {
	var err error
	change.rootInfo, err = os.Stat(r.path)
	if err != nil {
		return err
	}
	if change.existed {
		change.fileInfo, err = os.Stat(change.approvedPath)
	}
	return err
}

// ValidateChange refuses a preview whose target or contents changed during approval.
func (r Root) ValidateChange(rel string, change Change) error {
	abs, err := r.Resolve(rel)
	if err != nil {
		return err
	}
	file, err := r.Text(rel)
	if err != nil {
		return err
	}
	if abs != change.approvedPath || file.Existed != change.existed ||
		(file.Existed && (!file.Writable || Digest(file.Content) != change.beforeDigest)) {
		return fmt.Errorf("the file changed while approval was pending; read it again and request a new change")
	}
	return nil
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
	change, err := r.DescribeWrite(rel, content)
	if err != nil {
		return WriteResult{}, err
	}
	return r.WriteApproved(rel, change)
}

// WriteApproved applies exactly the previewed bytes and returns the pre-image used for undo.
func (r Root) WriteApproved(rel string, change Change) (WriteResult, error) {
	if change.Operation != OperationCreate && change.Operation != OperationOverwrite {
		return WriteResult{}, fmt.Errorf("not an approved write")
	}
	if err := r.applyChange(rel, change); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{Path: change.Path, Created: !change.existed,
		Lines: countLines(change.updated), PreviousLines: countLines(change.before),
		Before: Text{Path: change.Path, Content: change.before, Existed: change.existed, Writable: true}}, nil
}

// applyChange opens the resolved target beneath an anchored root, validates the opened inode,
// then writes through that same handle. O_EXCL prevents a create from replacing a new file.
func (r Root) applyChange(rel string, change Change) error {
	return r.applyChangeAtHooks(rel, change, nil, nil)
}

// The hooks let tests exercise filesystem changes at the open boundaries.
func (r Root) applyChangeAtHooks(rel string, change Change, beforeOpen, afterOpen func()) error {
	abs, err := r.Resolve(rel)
	if err != nil {
		return err
	}
	if abs != change.approvedPath || change.rootInfo == nil {
		return fmt.Errorf("the file changed while approval was pending; read it again and request a new change")
	}
	root, err := os.OpenRoot(r.path)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(info, change.rootInfo) {
		return fmt.Errorf("the project folder changed while approval was pending")
	}
	name := r.Relative(change.approvedPath)
	if !change.existed {
		if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
	}
	flags := os.O_RDWR
	if !change.existed {
		flags |= os.O_CREATE | os.O_EXCL
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	file, err := root.OpenFile(name, flags, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if afterOpen != nil {
		afterOpen()
	}
	if change.existed {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || change.fileInfo == nil || !os.SameFile(info, change.fileInfo) {
			return fmt.Errorf("the file changed while approval was pending; read it again and request a new change")
		}
		data, err := io.ReadAll(io.LimitReader(file, MaxWriteBytes+1))
		if err != nil {
			return err
		}
		if len(data) > MaxWriteBytes || Digest(string(data)) != change.beforeDigest {
			return fmt.Errorf("the file changed while approval was pending; read it again and request a new change")
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := file.Truncate(0); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(file, change.updated); err != nil {
		return err
	}
	return file.Close()
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
	// Every ancestor that exists has to be a folder. A path whose parent is a file cannot be
	// written — MkdirAll fails with ENOTDIR — and that failure would arrive *after* somebody had
	// approved a card saying the file would be created, which is the one thing the
	// describe-then-apply split exists to prevent.
	for dir := filepath.Dir(abs); dir != r.path && inside(r.path, dir); dir = filepath.Dir(dir) {
		dirInfo, dirErr := os.Stat(dir)
		if dirErr != nil {
			// Missing, so the write creates it. The walk is about the one that exists.
			continue
		}
		if !dirInfo.IsDir() {
			return "", "", 0, fmt.Errorf("%s is not a folder, so %s cannot be written in it",
				r.Relative(dir), r.Relative(abs))
		}
		break
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
	// The refusal edit_file makes, for two reasons rather than one: the card's preview of this
	// file would be a page of replacement characters, and there is no pre-image to put back (a
	// change to a binary has no undo, see internal/tools/snapshot.go) — so a text write over a
	// picture would be a change somebody approved without seeing and could not take back.
	if isBinary(data[:min(len(data), maxSniffBytes)]) {
		return "", "", 0, fmt.Errorf("%s is not a text file (it contains NUL bytes); overwriting it "+
			"would destroy bytes this app cannot restore", r.Relative(abs))
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
	abs, text, _, occurrences, matched, approx, err := r.inspectEdit(rel, old, new, replaceAll)
	if err != nil {
		return Change{}, err
	}
	// The diff shows what would actually be replaced, which for an approximate match is the
	// file's own lines rather than the ones the model typed — and it is compared once, with
	// the totals multiplied, because the other occurrences are the same replacement in a
	// context the card cannot show (Occurrences is what says they exist). Its rows are numbered
	// from the line the match really starts on, so the card points into the file.
	diff := compareDiffAt(matched, new, lineOf(text, matched))
	change := Change{
		Path:         r.Relative(abs),
		Operation:    OperationEdit,
		Hunks:        diff.Hunks,
		Added:        diff.Added * occurrences,
		Removed:      diff.Removed * occurrences,
		Truncated:    diff.Truncated,
		NewlineOnly:  diff.NewlineOnly,
		Occurrences:  occurrences,
		approvedPath: abs,
		beforeDigest: Digest(text),
		existed:      true,
	}
	updated := editText(text, old, new, matched, replaceAll, approx)
	if len(updated) > MaxWriteBytes {
		return Change{}, fmt.Errorf("%s would grow to %s, larger than the %s one change may produce",
			r.Relative(abs), humanBytes(int64(len(updated))), humanBytes(MaxWriteBytes))
	}
	replaced := 1
	if replaceAll {
		replaced = occurrences
	}
	change.before, change.updated = text, updated
	change.editResult = EditResult{Path: change.Path, Occurrences: replaced, Line: lineOf(text, matched),
		Added: countLines(new), Removed: countLines(matched), Approximate: approx, Digest: Digest(updated),
		Before: Text{Path: change.Path, Content: text, Existed: true, Writable: true}}
	if err := r.captureChangeIdentity(&change); err != nil {
		return Change{}, err
	}
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
	change, err := r.DescribeEdit(rel, old, new, replaceAll)
	if err != nil {
		return EditResult{}, err
	}
	return r.EditApproved(rel, change)
}

// EditApproved applies the exact edit shown in the preview.
func (r Root) EditApproved(rel string, change Change) (EditResult, error) {
	if change.Operation != OperationEdit {
		return EditResult{}, fmt.Errorf("not an approved edit")
	}
	if err := r.applyChange(rel, change); err != nil {
		return EditResult{}, err
	}
	return change.editResult, nil
}

func editText(text, old, new, matched string, replaceAll, approx bool) string {
	switch {
	case !replaceAll:
		return strings.Replace(text, matched, new, 1)
	case approx:
		return replaceWhitespaceMatches(text, old, new)
	default:
		return strings.ReplaceAll(text, matched, new)
	}
}

// inspectEdit reads the file to be edited and refuses the edit if it cannot be made.
//
// It returns the text it actually matched, which is `old` itself when the match was exact and
// the file's own lines when the match was approximate: what gets replaced has to be the bytes
// that are in the file, whatever the model typed.
func (r Root) inspectEdit(rel, old, new string, replaceAll bool) (abs, text string, mode fs.FileMode, occurrences int, matched string, approx bool, err error) {
	abs, err = r.Resolve(rel)
	if err != nil {
		return "", "", 0, 0, "", false, err
	}
	if strings.TrimSpace(old) == "" {
		return "", "", 0, 0, "", false, fmt.Errorf("the text to replace is empty")
	}
	// Bounded for the same reason a write is: an edit is the other way to put text in a file,
	// and a bound that only one of them enforces is not a bound. The preview is cut to 16 kB, so
	// a 200 MB replacement would be something a person approves without seeing.
	if len(new) > MaxWriteBytes {
		return "", "", 0, 0, "", false, fmt.Errorf("the replacement text is %s, larger than the %s a "+
			"single change may hold", humanBytes(int64(len(new))), humanBytes(MaxWriteBytes))
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
	// A file may only grow to what a change may hold, or an edit becomes the way around the
	// bound a write enforces (and a file too large to be edited could no longer be undone).
	// Exact for an exact match; the whitespace-tolerant case is checked again on the finished
	// text in Edit, because those spans carry their own indentation.
	grown := len(text) - occurrences*len(matched) + occurrences*len(new)
	if grown > MaxWriteBytes {
		return "", "", 0, 0, "", false, fmt.Errorf("%s would grow to %s, larger than the %s one "+
			"change may produce", r.Relative(abs), humanBytes(int64(grown)), humanBytes(MaxWriteBytes))
	}
	return abs, text, info.Mode().Perm(), occurrences, matched, approx, nil
}

// replaceWhitespaceMatches replaces every span that matches old once each line's whitespace is
// ignored, keeping the rest of the file byte for byte.
//
// Separate from strings.ReplaceAll because the spans are not all the same text: the match is the
// same lines, and each one has its own indentation in the file. The spans are found again here
// rather than carried out of inspectEdit, so the function that decides what a match *is* stays
// the one that enumerates them — a second matcher is how an edit ends up replacing a line the
// description never showed.
func replaceWhitespaceMatches(text, old, new string) string {
	spans, _ := approximateMatches(text, old)
	var sb strings.Builder
	last := 0
	for _, span := range spans {
		at := strings.Index(text[last:], span)
		if at < 0 {
			continue
		}
		at += last
		sb.WriteString(text[last:at])
		sb.WriteString(new)
		last = at + len(span)
	}
	sb.WriteString(text[last:])
	return sb.String()
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
		// Past the span, not one line on: a search for `a\na` in `a\na\na` must find one
		// occurrence, the way strings.Count does, rather than two that overlap. An exact match
		// and a whitespace-tolerant one have to count the same file the same way — the count is
		// what the refusal and the replace_all both act on.
		i += len(oldLines) - 1
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
