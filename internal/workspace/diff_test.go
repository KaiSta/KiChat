package workspace

import (
	"strings"
	"testing"
)

// TestAOneLineChangeShowsThatLineAndItsNeighbours is the case the whole diff exists for: a
// single line in a four-hundred-line file used to arrive on the card as two pages that differ
// by one character somewhere in the middle. What the card gets now is the line, the three
// above it and the three below it, numbered on both sides.
func TestAOneLineChangeShowsThatLineAndItsNeighbours(t *testing.T) {
	var before strings.Builder
	for i := 1; i <= 400; i++ {
		if i == 200 {
			before.WriteString("const retries = 3\n")
			continue
		}
		before.WriteString("line of the file\n")
	}
	after := strings.Replace(before.String(), "const retries = 3", "const retries = 5", 1)

	diff := CompareDiff(before.String(), after)
	if diff.Added != 1 || diff.Removed != 1 {
		t.Errorf("Added/Removed = %d/%d, want 1/1 for a one-line change", diff.Added, diff.Removed)
	}
	if len(diff.Hunks) != 1 {
		t.Fatalf("%d hunks for one change, want 1", len(diff.Hunks))
	}
	rows := diff.Hunks[0].Lines
	// The old line and the new one, plus three lines of context on each side.
	if len(rows) != 2+3+3 {
		t.Fatalf("%d rows, want the changed line and three either side:\n%v", len(rows), rows)
	}

	// What is removed and what is added, at the line it happened on, numbered as the file
	// numbers it — a card that says line 4 for a change on line 200 is worse than no number.
	var removed, added *DiffLine
	for i, row := range rows {
		switch row.Kind {
		case DiffRemoved:
			removed = &rows[i]
		case DiffAdded:
			added = &rows[i]
		}
	}
	if removed == nil || removed.Text != "const retries = 3" || removed.Old != 200 || removed.New != 0 {
		t.Errorf("the removed row is %+v, want line 200's old text and no new number", removed)
	}
	if added == nil || added.Text != "const retries = 5" || added.New != 200 || added.Old != 0 {
		t.Errorf("the added row is %+v, want line 200's new text and no old number", added)
	}
	// The context above the change is numbered too, so the reader can find it in the file.
	if rows[0].Kind != DiffContext || rows[0].Old != 197 || rows[0].New != 197 {
		t.Errorf("the first row is %+v, want line 197 as context on both sides", rows[0])
	}
}

// TestTwoRemoteChangesAreTwoHunks keeps the unchanged span between them off the wire: the two
// changes are what the reader has to check, and a hundred lines of untouched code between them
// is not.
func TestTwoRemoteChangesAreTwoHunks(t *testing.T) {
	lines := make([]string, 120)
	for i := range lines {
		lines[i] = "untouched"
	}
	lines[5] = "first"
	lines[100] = "second"
	before := strings.Join(lines, "\n") + "\n"

	lines[5] = "first changed"
	lines[100] = "second changed"
	after := strings.Join(lines, "\n") + "\n"

	diff := CompareDiff(before, after)
	if len(diff.Hunks) != 2 {
		t.Fatalf("%d hunks, want 2 for two changes a hundred lines apart", len(diff.Hunks))
	}
	// Neither hunk reaches the other change: each is the change and its three lines of
	// context, and nothing between.
	if got := len(diff.Hunks[0].Lines); got != 8 {
		t.Errorf("the first hunk has %d rows, want the change and three either side", got)
	}
	// The change is on line 101, so the context above it starts at 98 — from the file, not
	// from the hunk: the number is what lets the reader find the line.
	if first := diff.Hunks[1].Lines[0]; first.New != 98 {
		t.Errorf("the second hunk starts at line %d, want 98", first.New)
	}
}

// TestTwoChangesCloseEnoughToMergeShareOneHunk: an `if` and its closing brace four lines
// apart are one thing to read, and two hunks would print the lines between them twice.
func TestTwoChangesCloseEnoughToMergeShareOneHunk(t *testing.T) {
	before := "a\nb\nc\nd\ne\nf\ng\nh\n"
	after := "a\nB\nc\nd\ne\nf\nG\nh\n"

	diff := CompareDiff(before, after)
	if len(diff.Hunks) != 1 {
		t.Fatalf("%d hunks, want the two changes in one", len(diff.Hunks))
	}
	// a, b−, B+, c, d, e, f, g−, G+, h: the six unchanged lines are printed once, not twice.
	if got := len(diff.Hunks[0].Lines); got != 10 {
		t.Errorf("%d rows, want the eight lines with the two changed ones on both sides", got)
	}
}

// TestACreateIsEveryLineAdded: there is nothing on the other side, so the diff is the file.
func TestACreateIsEveryLineAdded(t *testing.T) {
	diff := CompareDiff("", "package deep\n\nfunc main() {}\n")
	if diff.Added != 3 || diff.Removed != 0 {
		t.Errorf("Added/Removed = %d/%d, want 3/0 for a new file", diff.Added, diff.Removed)
	}
	if len(diff.Hunks) != 1 || len(diff.Hunks[0].Lines) != 3 {
		t.Fatalf("hunks = %+v, want one hunk of three rows", diff.Hunks)
	}
	for _, row := range diff.Hunks[0].Lines {
		if row.Kind != DiffAdded || row.Old != 0 {
			t.Errorf("row %+v is not an addition with no old line", row)
		}
	}
}

// TestAChangeWithNothingDifferentHasNoHunks: a model that rewrites a file with the text it
// already had has changed nothing a person can see, and an empty hunk list says so.
func TestAChangeWithNothingDifferentHasNoHunks(t *testing.T) {
	diff := CompareDiff("same\n", "same\n")
	if diff.Added != 0 || diff.Removed != 0 || len(diff.Hunks) != 0 || diff.Truncated {
		t.Errorf("diff = %+v, want nothing at all", diff)
	}
	if diff.Hunks == nil {
		t.Error("Hunks is nil, and the window maps over it")
	}
}

// TestAChangeOfOnlyTheFinalNewlineSaysSo: the line split ignores the terminator, so a write
// that only adds or drops the newline at the end of the file produces no rows. It is still a
// change to the file's bytes, and the card must not read "no line differs" beside a file it is
// about to rewrite — which is the shape a model produces when it re-emits a file from memory.
func TestAChangeOfOnlyTheFinalNewlineSaysSo(t *testing.T) {
	added := CompareDiff("package main", "package main\n")
	if !added.NewlineOnly || len(added.Hunks) != 0 || added.Added != 0 || added.Removed != 0 {
		t.Errorf("adding a final newline gives %+v, want no rows and NewlineOnly", added)
	}
	dropped := CompareDiff("package main\n", "package main")
	if !dropped.NewlineOnly {
		t.Errorf("dropping a final newline is %+v, want NewlineOnly", dropped)
	}
	// A real change that also happens to add the newline is not this case: the rows exist and
	// the flag would be a second, contradictory story.
	real := CompareDiff("package main", "package other\n")
	if real.NewlineOnly {
		t.Error("a changed line was reported as a newline-only change")
	}
	// And an identical text is neither.
	if same := CompareDiff("package main", "package main"); same.NewlineOnly {
		t.Error("an unchanged text was reported as a newline-only change")
	}
}

// TestALineEndingChangeIsVisibleAndReadable: the bytes did change, so it is a change — but the
// row's text must not carry the carriage return, which the window would render as nothing at
// best and as a broken row at worst.
func TestALineEndingChangeIsVisibleAndReadable(t *testing.T) {
	diff := CompareDiff("alpha\r\nbeta\r\n", "alpha\nbeta\n")
	if diff.Added != 2 || diff.Removed != 2 {
		t.Errorf("Added/Removed = %d/%d, want both lines reported as changed", diff.Added, diff.Removed)
	}
	for _, hunk := range diff.Hunks {
		for _, row := range hunk.Lines {
			if strings.Contains(row.Text, "\r") {
				t.Errorf("row %q still carries a carriage return", row.Text)
			}
		}
	}
}

// TestALongLineIsCutOnACharacterBoundary: a minified file is one enormous row, and a cut in
// the middle of a multi-byte character arrives as a replacement glyph.
func TestALongLineIsCutOnACharacterBoundary(t *testing.T) {
	long := "var x = '" + strings.Repeat("é", maxDiffLineBytes) + "'\n"
	diff := CompareDiff("", long)
	if len(diff.Hunks) != 1 || len(diff.Hunks[0].Lines) != 1 {
		t.Fatalf("hunks = %+v, want one row", diff.Hunks)
	}
	text := diff.Hunks[0].Lines[0].Text
	if len(text) > maxDiffLineBytes+len("…") {
		t.Errorf("the row is %d bytes, more than the budget", len(text))
	}
	if !strings.HasSuffix(text, "…") || !strings.HasSuffix(strings.TrimSuffix(text, "…"), "é") {
		t.Errorf("the row was not cut on a character boundary: %q", text[:min(len(text), 12)]+"…")
	}
}

// TestAWindowIsNumberedFromTheFile: an edit compares the span the model named, and those rows
// have to carry the numbers the file uses — a card that says line 1 about line 200 sends the
// reader to the wrong place.
func TestAWindowIsNumberedFromTheFile(t *testing.T) {
	diff := compareDiffAt("same\nold\nsame\n", "same\nnew\nsame\n", 200)
	if len(diff.Hunks) != 1 {
		t.Fatalf("hunks = %+v, want one", diff.Hunks)
	}
	rows := diff.Hunks[0].Lines
	if rows[0].Old != 200 || rows[0].New != 200 {
		t.Errorf("the window starts at %d/%d, want line 200", rows[0].Old, rows[0].New)
	}
	if rows[1].Old != 201 || rows[2].New != 201 {
		t.Errorf("the changed row is numbered %+v, want 201 on both sides", rows[1:3])
	}
}

// rowsOf is every row of a change's hunks, in order.
func rowsOf(change Change) []DiffLine {
	var rows []DiffLine
	for _, hunk := range change.Hunks {
		rows = append(rows, hunk.Lines...)
	}
	return rows
}

// addedText and removedText are the texts of one side, joined by newlines: a test that cares
// what the card shows should not have to walk the hunks to say so.
func addedText(change Change) string   { return sideText(change, DiffAdded) }
func removedText(change Change) string { return sideText(change, DiffRemoved) }

func sideText(change Change, kind string) string {
	var lines []string
	for _, row := range rowsOf(change) {
		if row.Kind == kind {
			lines = append(lines, row.Text)
		}
	}
	return strings.Join(lines, "\n")
}
