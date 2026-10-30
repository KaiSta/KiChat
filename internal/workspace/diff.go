package workspace

// The diff a change is shown as: the lines that differ, with a few lines around them.
//
// The first version of this sent the whole text on either side, each side clamped to a byte
// budget. That is fine for a create and useless for the change an agent actually makes most
// often: one line inside a four-hundred-line file, which arrived on the card as two pages
// that differ by a character somewhere in the middle. A person cannot check that, and a card
// nobody can check is a card that gets approved without being read.
//
// So the diff is computed here, in Go, for three reasons. The window should not be the second
// opinion about what is changing — it draws what it is given; a byte budget at the transport
// is not a shape a hunk can be cut from; and the same comparison has to answer for the changes
// panel, which is a diff of a file against the state the chat found it in rather than against
// the previous write.
//
// What is sent is *hunks*: runs of changed lines with the unchanged lines around them that
// make them readable, and with line numbers on both sides. The unchanged span between two
// hunks is not sent at all — it is what the row of dots stands for.

import (
	"strings"
	"unicode/utf8"
)

// The three kinds of row a hunk holds. They are strings rather than an enum because they
// travel over the bridge as JSON, and the window switches on them.
const (
	// DiffContext is an unchanged line, included so a hunk has something to be read against.
	DiffContext = "context"
	// DiffRemoved is a line of the old text.
	DiffRemoved = "removed"
	// DiffAdded is a line of the new text.
	DiffAdded = "added"
)

// The bounds on what one diff carries.
const (
	// diffContext is how many unchanged lines each side of a change hunk shows. Three is what
	// every diff tool has settled on: enough to recognise the function a line sits in, few
	// enough that a one-line edit is a handful of rows rather than a page.
	diffContext = 3
	// maxDiffRows bounds the rows one diff sends. A rewrite that changes everything produces
	// one enormous hunk, and the card is a preview inside a fixed-height box — the counts say
	// how big the change is, and the hunks show where it starts.
	maxDiffRows = 240
	// maxDiffBytes is the second bound, because rows are not the same size: a minified file is
	// a handful of very long rows, and the row count alone would let one line be a megabyte.
	maxDiffBytes = 32 << 10
	// maxDiffLineBytes cuts one row. A minified line is the case: the whole file on one line
	// cannot be read on a card at any width, and the row is here to show *that* it changed.
	maxDiffLineBytes = 240
	// maxDiffCells bounds the line-by-line comparison. An LCS table is (n+1)×(m+1) cells, so
	// two files of a thousand lines each are four megabytes of int32 — fine — while two files
	// of fifty thousand lines would be ten gigabytes. Past the bound the middle is reported as
	// replaced as a block, which is the honest answer for two texts with nothing in common.
	maxDiffCells = 1 << 20
)

// DiffLine is one row of a hunk: what kind it is, the line it stands for, and its number on
// each side. A row only has a number on the side it exists on, so a removed line has Old and
// no New.
type DiffLine struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	Old  int    `json:"old"`
	New  int    `json:"new"`
}

// Hunk is a run of changed lines with its context, ready to draw as it is.
//
// Hunks carry no header of their own (`@@ -12,7 +12,6 @@`): every row has its line numbers,
// and a header repeating the first of them is a second way to say the same thing. The gap
// between two hunks is the window's own business — it knows it is drawing a row of dots.
type Hunk struct {
	Lines []DiffLine `json:"lines"`
}

// Diff is a change as the window shows it.
type Diff struct {
	// Added and Removed count the lines of the diff, not of either file: a one-line edit in a
	// four-hundred-line file is 1 and 1, which is what the reader needs the numbers to mean
	// next to a diff of one line. They count the *whole* change even when the hunks are cut
	// short, because a total that shrank with the preview would understate the change by
	// exactly the amount the reader is being warned about.
	Added   int
	Removed int
	// Hunks is never nil: the window maps over it.
	Hunks []Hunk
	// Truncated is "there is more change than these hunks show".
	Truncated bool
	// NewlineOnly is "the only difference is the newline at the end of the file". Lines are
	// split without their terminator, so that difference produces no row at all — and a card
	// reading "no line differs" about a file whose last byte changed would be claiming
	// something false. It is the difference the old two-pane preview could not show either,
	// and the one a model makes most often when it re-emits a file from memory.
	NewlineOnly bool
}

// diffRow is a line of the comparison before it is shaped into hunks: its kind, its text, and
// the numbers it has on each side (0 where it has none).
type diffRow struct {
	kind string
	text string
	old  int
	new  int
}

// CompareDiff compares two texts line by line and reports the hunks that differ.
//
// Lines are compared byte for byte, and a trailing carriage return is hidden from display
// only: a file that switched from CRLF to LF would otherwise read as four hundred identical
// changes, while a comparison that ignored the difference would claim nothing changed in a
// file whose bytes did.
func CompareDiff(before, after string) Diff { return compareDiffAt(before, after, 1) }

// compareDiffAt is CompareDiff for a text that is a window into a file rather than the file:
// an edit compares the span the model named, and its rows have to carry the file's own numbers
// — a person told "line 1" about a change on line 200 goes to the wrong place.
//
// The new side is numbered from the same place, which is exact when the replacement has the
// same number of lines and a best guess when it does not: the lines after it move, and how far
// is something only the whole file knows. A number that is one line off is still where to look.
func compareDiffAt(before, after string, firstLine int) Diff {
	if firstLine < 1 {
		firstLine = 1
	}
	a, b := diffLines(before), diffLines(after)

	// The equal head and tail are found first and are not compared again. This is what keeps
	// the ordinary case — one line changed in a long file — cheap, and it is why the cell
	// bound above only ever applies to a middle that is genuinely different.
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix &&
		a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}

	offset := firstLine - 1
	rows := make([]diffRow, 0, len(a)+len(b))
	for i := 0; i < prefix; i++ {
		rows = append(rows, diffRow{kind: DiffContext, text: a[i], old: offset + i + 1, new: offset + i + 1})
	}
	rows = append(rows, middleRows(a[prefix:len(a)-suffix], b[prefix:len(b)-suffix], offset+prefix)...)
	for k := 0; k < suffix; k++ {
		rows = append(rows, diffRow{
			kind: DiffContext,
			text: a[len(a)-suffix+k],
			old:  offset + len(a) - suffix + k + 1,
			new:  offset + len(b) - suffix + k + 1,
		})
	}

	diff := Diff{Hunks: []Hunk{}}
	for _, row := range rows {
		switch row.kind {
		case DiffAdded:
			diff.Added++
		case DiffRemoved:
			diff.Removed++
		}
	}
	diff.Hunks, diff.Truncated = diffHunks(rows)
	// Nothing to draw, but the texts are not equal: the newline at the end of the file is the
	// whole of it. Every other byte difference produces a row, a carriage return included.
	diff.NewlineOnly = before != after && diff.Added == 0 && diff.Removed == 0
	return diff
}

// middleRows compares the part of two files the common head and tail did not settle.
//
// The comparison is a longest-common-subsequence table walked backwards, which is what makes
// a moved line show as removed *there* and added *there* rather than as the whole block
// replaced. offset is how many lines the common head took, on top of the window's own first
// line: both sides start at the same number there, because the head was equal on both.
func middleRows(a, b []string, offset int) []diffRow {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	// Past the cell bound, the middle is one replacement. Two texts this different have no
	// line-by-line answer worth the table, and pretending otherwise is how a diff of two
	// unrelated files turns into ninety seconds of a frozen window.
	if int64(len(a))*int64(len(b)) > maxDiffCells {
		rows := make([]diffRow, 0, len(a)+len(b))
		for i, line := range a {
			rows = append(rows, diffRow{kind: DiffRemoved, text: line, old: offset + i + 1})
		}
		for j, line := range b {
			rows = append(rows, diffRow{kind: DiffAdded, text: line, new: offset + j + 1})
		}
		return rows
	}

	width := len(b) + 1
	table := make([]int32, (len(a)+1)*width)
	at := func(i, j int) int { return i*width + j }
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[at(i, j)] = table[at(i+1, j+1)] + 1
				continue
			}
			if down, right := table[at(i+1, j)], table[at(i, j+1)]; down >= right {
				table[at(i, j)] = down
			} else {
				table[at(i, j)] = right
			}
		}
	}

	rows := make([]diffRow, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			rows = append(rows, diffRow{kind: DiffContext, text: a[i], old: offset + i + 1, new: offset + j + 1})
			i++
			j++
		case table[at(i+1, j)] >= table[at(i, j+1)]:
			// Removals are emitted before the additions that replace them, which is the order
			// every diff reader expects to see them in.
			rows = append(rows, diffRow{kind: DiffRemoved, text: a[i], old: offset + i + 1})
			i++
		default:
			rows = append(rows, diffRow{kind: DiffAdded, text: b[j], new: offset + j + 1})
			j++
		}
	}
	for ; i < len(a); i++ {
		rows = append(rows, diffRow{kind: DiffRemoved, text: a[i], old: offset + i + 1})
	}
	for ; j < len(b); j++ {
		rows = append(rows, diffRow{kind: DiffAdded, text: b[j], new: offset + j + 1})
	}
	return rows
}

// diffHunks cuts the comparison into hunks with context, merging two changes that would
// otherwise show the same unchanged lines twice.
func diffHunks(rows []diffRow) ([]Hunk, bool) {
	hunks := []Hunk{}

	// Every run of changed rows, widened by the context and merged where the widened spans
	// touch. A merge is not cosmetic: `if` on one line and its closing brace four lines later
	// are one change to read, and two hunks each repeating the lines between them is the same
	// text printed twice.
	var spans [][2]int
	for i := 0; i < len(rows); {
		if rows[i].kind == DiffContext {
			i++
			continue
		}
		start := i
		for i < len(rows) && rows[i].kind != DiffContext {
			i++
		}
		spans = append(spans, [2]int{start, i - 1})
	}
	merged := make([][2]int, 0, len(spans))
	for _, span := range spans {
		lo, hi := max(0, span[0]-diffContext), min(len(rows)-1, span[1]+diffContext)
		if n := len(merged); n > 0 && lo <= merged[n-1][1]+1 {
			merged[n-1][1] = hi
			continue
		}
		merged = append(merged, [2]int{lo, hi})
	}

	usedRows, usedBytes := 0, 0
	for _, span := range merged {
		hunk := Hunk{Lines: []DiffLine{}}
		cut := false
		for i := span[0]; i <= span[1]; i++ {
			line := DiffLine{
				Kind: rows[i].kind,
				Text: clipRow(rows[i].text),
				Old:  rows[i].old,
				New:  rows[i].new,
			}
			cost := len(line.Text) + 8
			if usedRows+1 > maxDiffRows || usedBytes+cost > maxDiffBytes {
				cut = true
				break
			}
			hunk.Lines = append(hunk.Lines, line)
			usedRows++
			usedBytes += cost
		}
		if len(hunk.Lines) > 0 {
			hunks = append(hunks, hunk)
		}
		if cut {
			return hunks, true
		}
	}
	return hunks, false
}

// diffLines splits a text the way a reader counts it: a final newline ends the last line
// rather than starting an empty one, so "a\nb\n" is two lines and countLines agrees.
func diffLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if n := len(lines); lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// clipRow hides a carriage return and bounds the line's length, on a rune boundary: a row cut
// in the middle of a character would arrive as a replacement glyph.
func clipRow(line string) string {
	line = strings.TrimSuffix(line, "\r")
	if len(line) <= maxDiffLineBytes {
		return line
	}
	cut := line[:maxDiffLineBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
