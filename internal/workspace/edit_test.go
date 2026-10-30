package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestWriteCreatesAFileAndItsFolders is the shape a model writes when it adds a file to
// a project it has just read: the path it names may not exist yet, and neither may the
// folder above it.
func TestWriteCreatesAFileAndItsFolders(t *testing.T) {
	root, dir := project(t, nil)

	change, err := root.DescribeWrite("internal/deep/new.go", "package deep\n")
	if err != nil {
		t.Fatalf("DescribeWrite: %v", err)
	}
	if change.Operation != OperationCreate {
		t.Errorf("operation = %q for a file that does not exist, want %q", change.Operation, OperationCreate)
	}
	// A create has nothing to compare against, so every line of it is an addition and no row
	// has an old line number.
	if added := addedText(change); added != "package deep" {
		t.Errorf("a create shows %q as added, want the line being written", added)
	}
	if removed := removedText(change); removed != "" {
		t.Errorf("a create shows %q as removed, want nothing", removed)
	}
	if want := []string{"internal", "internal/deep"}; strings.Join(change.CreatesDirs, ",") != strings.Join(want, ",") {
		t.Errorf("CreatesDirs = %v, want %v in that order", change.CreatesDirs, want)
	}
	if change.Added != 1 {
		t.Errorf("Added = %d, want the one line being written", change.Added)
	}

	result, err := root.Write("internal/deep/new.go", "package deep\n")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !result.Created || result.Lines != 1 || result.PreviousLines != 0 {
		t.Errorf("result = %+v, want a new one-line file", result)
	}
	body, err := os.ReadFile(filepath.Join(dir, "internal", "deep", "new.go"))
	if err != nil {
		t.Fatalf("the file was not written: %v", err)
	}
	if string(body) != "package deep\n" {
		t.Errorf("the file holds %q, want exactly what was written", body)
	}
}

// TestWriteReplacesAFileWithoutLosingItsMode pins the in-place write: a file that was
// executable, or private, must not come back as an ordinary 0644 file because a tool
// rewrote its text.
func TestWriteReplacesAFileWithoutLosingItsMode(t *testing.T) {
	root, dir := project(t, map[string]string{"run.sh": "#!/bin/sh\necho old\n"})
	path := filepath.Join(dir, "run.sh")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}

	change, err := root.DescribeWrite("run.sh", "#!/bin/sh\necho new\n")
	if err != nil {
		t.Fatalf("DescribeWrite: %v", err)
	}
	if change.Operation != OperationOverwrite {
		t.Errorf("operation = %q for an existing file, want %q", change.Operation, OperationOverwrite)
	}
	if !strings.Contains(removedText(change), "echo old") {
		t.Errorf("the diff does not show the line being replaced: %q", removedText(change))
	}
	if !strings.Contains(addedText(change), "echo new") {
		t.Errorf("the diff does not show the line taking its place: %q", addedText(change))
	}

	result, err := root.Write("run.sh", "#!/bin/sh\necho new\n")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if result.Created || result.PreviousLines != 2 {
		t.Errorf("result = %+v, want an overwrite of a two-line file", result)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o after a rewrite, want the 755 it had", info.Mode().Perm())
	}
}

// TestWriteStillCannotLeaveTheFolder is the boundary applied to the operation that would
// matter most if it leaked: the whole package exists so that a tool named by a model
// cannot write outside the folder the user picked.
func TestWriteStillCannotLeaveTheFolder(t *testing.T) {
	root, dir := project(t, map[string]string{"keep.txt": "keep\n"})
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")

	for _, path := range []string{"../outside.txt", outside, "/tmp/kichat-should-not-exist.txt"} {
		if _, err := root.DescribeWrite(path, "x\n"); !errors.Is(err, ErrOutside) {
			t.Errorf("DescribeWrite(%q) = %v, want ErrOutside", path, err)
		}
		if _, err := root.Write(path, "x\n"); !errors.Is(err, ErrOutside) {
			t.Errorf("Write(%q) = %v, want ErrOutside", path, err)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("a write escaped the folder and created %s", outside)
	}

	// And through a symlink, which is the shape a textual check cannot see.
	link := filepath.Join(dir, "escape")
	if err := os.Symlink(filepath.Dir(dir), link); err != nil {
		t.Skipf("this machine cannot create symlinks: %v", err)
	}
	if _, err := root.Write("escape/outside.txt", "x\n"); !errors.Is(err, ErrOutside) {
		t.Errorf("a write through a symlink out of the folder = %v, want ErrOutside", err)
	}
}

// TestWriteRefusesWhatItCannotDo collects the refusals that must happen *before* anybody
// is asked for permission: asking about a folder, or about a file too large to replace,
// would be asking about something that can never happen.
func TestWriteRefusesWhatItCannotDo(t *testing.T) {
	root, _ := project(t, map[string]string{"a.txt": "a\n", "sub/b.txt": "b\n"})

	if _, err := root.Write("sub", "x\n"); err == nil {
		t.Error("writing over a folder was allowed")
	}
	if _, err := root.Write("", "x\n"); err == nil {
		t.Error("writing over the project folder itself was allowed")
	}
	big := strings.Repeat("x", MaxWriteBytes+1)
	if _, err := root.DescribeWrite("big.txt", big); err == nil {
		t.Error("a write larger than the limit was allowed")
	}
	// A file too large to replace is refused too, because replacing it means reading it
	// first, and a refusal the model can read beats a truncated file.
	large := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(large, []byte(strings.Repeat("y", MaxWriteBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	rootWithLarge, err := Open(filepath.Dir(large))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rootWithLarge.DescribeWrite("large", "x\n"); err == nil {
		t.Error("replacing a file larger than the limit was allowed")
	}
}

// TestEditReplacesOneOccurrence is the common case, and the count it reports is what the
// tool tells the model and the window.
func TestEditReplacesOneOccurrence(t *testing.T) {
	root, dir := project(t, map[string]string{
		"main.go": "package main\n\nfunc main() {\n\tprintln(\"old\")\n}\n",
	})

	change, err := root.DescribeEdit("main.go", "\tprintln(\"old\")", "\tprintln(\"new\")", false)
	if err != nil {
		t.Fatalf("DescribeEdit: %v", err)
	}
	if change.Operation != OperationEdit {
		t.Errorf("operation = %q, want %q", change.Operation, OperationEdit)
	}
	if !strings.Contains(removedText(change), "println(\"old\")") || !strings.Contains(addedText(change), "println(\"new\")") {
		t.Errorf("the diff does not show the two sides: removed %q, added %q", removedText(change), addedText(change))
	}
	if change.Added != 1 || change.Removed != 1 {
		t.Errorf("Added/Removed = %d/%d, want 1/1 for a one-line replacement", change.Added, change.Removed)
	}
	// Numbered from the file, not from the snippet: the card says line 4 because that is where
	// the line is, and a person can go and look at it.
	if rows := rowsOf(change); len(rows) != 2 || rows[0].Old != 4 || rows[1].New != 4 {
		t.Errorf("the diff is numbered %+v, want the match's line 4 on both sides", rows)
	}

	result, err := root.Edit("main.go", "\tprintln(\"old\")", "\tprintln(\"new\")", false)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if result.Occurrences != 1 || result.Line != 4 {
		t.Errorf("result = %+v, want one replacement at line 4", result)
	}
	body, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "println(\"new\")") || strings.Contains(string(body), "println(\"old\")") {
		t.Errorf("the edit did not land:\n%s", body)
	}
	// Everything around the change is untouched, byte for byte.
	if !strings.HasPrefix(string(body), "package main\n\nfunc main() {\n") {
		t.Errorf("the edit disturbed the text above it:\n%q", body)
	}
}

// TestEditRefusesAnAmbiguousMatch is the property that makes an editing agent
// trustworthy: two matches are an error rather than the first one, because the user's next
// action is to believe the diff.
func TestEditRefusesAnAmbiguousMatch(t *testing.T) {
	root, _ := project(t, map[string]string{
		"dup.go": "a := 1\nb := 1\n",
	})
	_, err := root.Edit("dup.go", ":= 1", ":= 2", false)
	var ambiguous *AmbiguousMatch
	if !errors.As(err, &ambiguous) {
		t.Fatalf("an ambiguous edit = %v, want AmbiguousMatch", err)
	}
	if ambiguous.Count != 2 || ambiguous.FirstLine != 1 {
		t.Errorf("the refusal says %+v, want two occurrences from line 1", ambiguous)
	}
	// The same describe refuses too, so nobody is ever asked to approve it.
	if _, err := root.DescribeEdit("dup.go", ":= 1", ":= 2", false); !errors.As(err, &ambiguous) {
		t.Errorf("DescribeEdit on an ambiguous edit = %v, want AmbiguousMatch", err)
	}
	// And by name, every occurrence is replaceable.
	result, err := root.Edit("dup.go", ":= 1", ":= 2", true)
	if err != nil {
		t.Fatalf("Edit with replace_all: %v", err)
	}
	if result.Occurrences != 2 {
		t.Errorf("replace_all replaced %d occurrences, want 2", result.Occurrences)
	}
}

// TestEditReportsTextThatIsNotThere is the answer to a model that guessed at whitespace:
// it has to be told the match failed, and told to read the file and copy it exactly.
func TestEditReportsTextThatIsNotThere(t *testing.T) {
	root, _ := project(t, map[string]string{"a.go": "func A() {}\n"})
	_, err := root.Edit("a.go", "func A() { }", "func B() {}", false)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("a missing match = %v, want ErrNoMatch", err)
	}
	if _, err := root.Edit("a.go", "   ", "x", false); err == nil {
		t.Error("an edit with no text to match was allowed")
	}
	if _, err := root.Edit("missing.go", "a", "b", false); err == nil {
		t.Error("an edit of a file that does not exist was allowed")
	}
}

// TestEditPreservesTheRestOfTheFileByteForByte covers the details a re-rendering would
// lose: a byte-order mark, CRLF line endings, a missing final newline, and a trailing
// blank line.
func TestEditPreservesTheRestOfTheFileByteForByte(t *testing.T) {
	original := "\ufeffpackage a\r\n\r\nvar x = 1\r\n\r\n"
	root, dir := project(t, map[string]string{"a.go": original})

	if _, err := root.Edit("a.go", "var x = 1", "var x = 2", false); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "\ufeffpackage a\r\n\r\nvar x = 2\r\n\r\n"; string(body) != want {
		t.Errorf("the file came back as %q, want %q", body, want)
	}
}

// TestEditRefusesABinaryFile keeps an edit from turning a picture into text: the model
// cannot see the bytes, and a replacement inside them would corrupt the file silently.
func TestEditRefusesABinaryFile(t *testing.T) {
	root, dir := project(t, nil)
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0x7f, 'E', 0, 'F'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Edit("blob.bin", "E", "L", false); err == nil {
		t.Error("a binary file was edited as text")
	}
	before, err := os.ReadFile(filepath.Join(dir, "blob.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string([]byte{0x7f, 'E', 0, 'F'}) {
		t.Errorf("the refusal still modified the file: %q", before)
	}
}

// TestALongChangeIsCutAndSaysSo keeps a four-thousand-line rewrite from being rendered in a
// window: the hunks are cut, the card can say they are cut, and the counts still describe the
// whole change rather than the part that fitted.
func TestALongChangeIsCutAndSaysSo(t *testing.T) {
	root, _ := project(t, nil)
	long := strings.Repeat("a line of the new file\n", 4_000)
	change, err := root.DescribeWrite("long.txt", long)
	if err != nil {
		t.Fatalf("DescribeWrite: %v", err)
	}
	if !change.Truncated {
		t.Error("a diff longer than the row budget was not reported as truncated")
	}
	if got := rowsOf(change); len(got) != maxDiffRows {
		t.Errorf("the diff carries %d rows, want the %d-row budget", len(got), maxDiffRows)
	}
	// The cut is at a row boundary: every row is a whole line of the file.
	if got := rowsOf(change); got[len(got)-1].Text != "a line of the new file" {
		t.Errorf("the diff was cut mid-line: %q", got[len(got)-1].Text)
	}
	// The counts are of the whole change, not of the preview.
	if change.Added != 4_000 {
		t.Errorf("Added = %d, want the whole file's 4000 lines", change.Added)
	}
}

// The failure this exists for: a model re-writes a line it read, but with spaces where the
// file has a tab. The exact match used to refuse, and the refusal cost a round — the model
// read the file again, copied the line, and tried once more.
func TestAnEditForgivesDifferentIndentation(t *testing.T) {
	root, _ := project(t, map[string]string{
		"main.go": "func main() {\n\tprintln(\"hi\")\n}\n",
	})

	// Four spaces where the file has a tab.
	result, err := root.Edit("main.go", "    println(\"hi\")", "\tprintln(\"bye\")", false)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !result.Approximate {
		t.Error("the result does not report the approximate match")
	}
	text, err := os.ReadFile(filepath.Join(root.Path(), "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(text), "func main() {\n\tprintln(\"bye\")\n}\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

// TestAWhitespaceTolerantReplaceAllReplacesEveryOccurrence is the combination the two
// behaviours make dangerous together. The spans a tolerant match finds are not the same text —
// each carries the file's own indentation — so replacing the first span everywhere touched only
// the occurrences that happened to look exactly like it, while the result and the tool card said
// every one had been replaced.
func TestAWhitespaceTolerantReplaceAllReplacesEveryOccurrence(t *testing.T) {
	root, dir := project(t, map[string]string{
		"main.go": "if a {\n\tx = 1\n}\nif b {\n    x = 1\n}\n",
	})
	// Six spaces are an exact substring of neither line, which is what makes the match
	// approximate — and leaves the two spans spelled differently from each other.
	result, err := root.Edit("main.go", "      x = 1", "x = 2", true)
	if err != nil {
		t.Fatalf("replace_all with an approximate match: %v", err)
	}
	if result.Occurrences != 2 {
		t.Errorf("the result says %d occurrences replaced, want 2", result.Occurrences)
	}
	text, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	want := "if a {\nx = 2\n}\nif b {\nx = 2\n}\n"
	if string(text) != want {
		t.Errorf("file = %q, want %q — the second occurrence was left alone", text, want)
	}
}

// TestAReplaceAllPreviewSaysHowManyPlacesItTouches is the card's half of a replace_all: the
// preview can only hold the one span the tool call carries, so an edit that touches twelve
// places used to look exactly like an edit that touched one — a person approved the other
// eleven blind.
func TestAReplaceAllPreviewSaysHowManyPlacesItTouches(t *testing.T) {
	root, _ := project(t, map[string]string{"a.go": "x := 1\ny := 1\nz := 1\n"})

	change, err := root.DescribeEdit("a.go", ":= 1", ":= 2", true)
	if err != nil {
		t.Fatalf("DescribeEdit: %v", err)
	}
	if change.Occurrences != 3 {
		t.Errorf("the change says %d occurrences, want 3", change.Occurrences)
	}
	// The counts are totals over every replacement rather than one snippet's lines, or the
	// card would show "−1 +1" for a change to three lines of the file.
	if change.Added != 3 || change.Removed != 3 {
		t.Errorf("Added/Removed = %d/%d, want 3/3 for three one-line replacements", change.Added, change.Removed)
	}

	// An ordinary edit is still one place, and says so.
	single, err := root.DescribeEdit("a.go", "x := 1", "x := 9", false)
	if err != nil {
		t.Fatalf("DescribeEdit: %v", err)
	}
	if single.Occurrences != 1 || single.Removed != 1 {
		t.Errorf("a single edit = %+v, want one occurrence of one line", single)
	}
}

// TestAWriteUnderAFileIsRefusedBeforeTheCard pins the promise of the describe-then-apply
// split: a change that cannot happen must never reach the person being asked. `main.go/x.go`
// used to be described as "creates the file" and then fail with ENOTDIR after the click.
func TestAWriteUnderAFileIsRefusedBeforeTheCard(t *testing.T) {
	root, _ := project(t, map[string]string{"main.go": "package main\n"})

	if _, err := root.DescribeWrite("main.go/x.go", "package x\n"); err == nil {
		t.Error("a write inside a file was described for approval")
	}
	if _, err := root.Write("main.go/x.go", "package x\n"); err == nil {
		t.Error("a write inside a file was allowed")
	}
}

// TestAWriteRefusesToReplaceSomethingThatIsNotText is the write side of the refusal
// edit_file already made, and it protects two things at once: the card's preview of a binary
// file is a page of replacement characters, and there is no pre-image for one — so a text write
// over a picture is a change somebody approved without seeing and could not put back.
func TestAWriteRefusesToReplaceSomethingThatIsNotText(t *testing.T) {
	root, dir := project(t, nil)
	blob := []byte{0x7f, 'E', 0, 'F', 0, 0}
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := root.DescribeWrite("blob.bin", "text"); err == nil {
		t.Error("a write over a binary file was described for approval")
	}
	if _, err := root.Write("blob.bin", "text"); err == nil {
		t.Error("a write over a binary file was allowed")
	}
	kept, err := os.ReadFile(filepath.Join(dir, "blob.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != string(blob) {
		t.Errorf("the refused write still changed the file: %q", kept)
	}

	// A new file is not that case: there is no binary to destroy.
	if _, err := root.Write("notes.txt", "text\n"); err != nil {
		t.Errorf("a write to a new path was refused: %v", err)
	}
}

// TestAnEditCannotGrowAFilePastTheWriteBound keeps the edit from being the way around the bound
// a write enforces: the preview is cut to 16 kB, so a replacement a person approves is not the
// replacement they saw, and a file past the bound can no longer be edited or undone.
func TestAnEditCannotGrowAFilePastTheWriteBound(t *testing.T) {
	root, dir := project(t, map[string]string{"a.go": "package a\n"})
	huge := strings.Repeat("a", MaxWriteBytes+1)
	if _, err := root.DescribeEdit("a.go", "package a", huge, false); err == nil {
		t.Error("a replacement larger than a write may hold was described for approval")
	}
	if _, err := root.Edit("a.go", "package a", huge, false); err == nil {
		t.Error("a replacement larger than a write may hold was written")
	}
	text, err := os.ReadFile(filepath.Join(dir, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(text) != "package a\n" {
		t.Errorf("the refused edit still changed the file: %d bytes", len(text))
	}
}

// TestATolerantMatchCountsOverlappingLinesOnce pins the count against the exact one. `a\na`
// occurs once in `a\na\na`, and strings.Count says so; a tolerant scan that advanced one line at
// a time found two overlapping spans, so the same file was one match or two depending on which
// matcher saw it — and replace_all would then claim a replacement it could not make.
func TestATolerantMatchCountsOverlappingLinesOnce(t *testing.T) {
	root, dir := project(t, map[string]string{"a.txt": "a\na\na\n"})

	result, err := root.Edit("a.txt", "  a\n  a", "b", true)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if result.Occurrences != 1 {
		t.Errorf("the overlapping pair was counted %d times, want once", result.Occurrences)
	}
	text, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(text) != "b\na\n" {
		t.Errorf("file = %q, want %q", text, "b\na\n")
	}
}

// What the forgiveness must not do: replace a line that says something else. A changed word is
// a different line, however close the whitespace is.
func TestAnEditDoesNotForgetWords(t *testing.T) {
	root, _ := project(t, map[string]string{"main.go": "keep = 1\n"})

	if _, err := root.Edit("main.go", "   keep = 2   ", "keep = 3", false); err == nil {
		t.Fatal("an edit of a line that is not there reported success")
	}
	text, err := os.ReadFile(filepath.Join(root.Path(), "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(text) != "keep = 1\n" {
		t.Errorf("the file changed: %q", text)
	}
}

// Two candidate lines are still ambiguous: the whitespace is forgiven, guessing is not.
func TestAnApproximateEditRefusesTwoCandidates(t *testing.T) {
	root, _ := project(t, map[string]string{
		"main.go": "if a {\n\tx = 1\n}\nif b {\n\tx = 1\n}\n",
	})

	_, err := root.Edit("main.go", "    x = 1", "x = 2", false)
	var ambiguous *AmbiguousMatch
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error = %v, want an AmbiguousMatch", err)
	}
	if ambiguous.Count != 2 {
		t.Errorf("count = %d, want 2", ambiguous.Count)
	}
}
