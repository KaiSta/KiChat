package ingest

import (
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// makeTestXlsx builds a real workbook through excelize's own writer, so the
// extraction path is tested against a file the library accepts rather than a stub.
func makeTestXlsx(t *testing.T, sheets map[string][][]any) []byte {
	t.Helper()
	f := excelize.NewFile()
	defer f.Close()

	first := true
	for name, rows := range sheets {
		if first {
			if err := f.SetSheetName("Sheet1", name); err != nil {
				t.Fatalf("rename first sheet to %q: %v", name, err)
			}
			first = false
		} else if _, err := f.NewSheet(name); err != nil {
			t.Fatalf("add sheet %q: %v", name, err)
		}
		for i, row := range rows {
			for j, value := range row {
				cell, err := excelize.CoordinatesToCellName(j+1, i+1)
				if err != nil {
					t.Fatalf("cell name for %d/%d: %v", i+1, j+1, err)
				}
				if err := f.SetCellValue(name, cell, value); err != nil {
					t.Fatalf("set %s!%s: %v", name, cell, err)
				}
			}
		}
	}
	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatalf("write workbook: %v", err)
	}
	return buf.Bytes()
}

// A spreadsheet is attached for its tables, so each sheet has to arrive as CSV
// under a heading that names it, and the sheet count is what the ingestion result
// reports to the window.
func TestExtractXlsxRendersEachSheetAsCSV(t *testing.T) {
	data := makeTestXlsx(t, map[string][][]any{
		"Daten":   {{"Name", "Betrag"}, {"Miete", 850}, {"Nebenkosten", 210}},
		"Notizen": {{"Thema"}, {"Kündigung"}},
	})

	text, sheets, warnings, err := extractXlsx(data)
	if err != nil {
		t.Fatalf("extractXlsx failed: %v", err)
	}
	if sheets != 2 {
		t.Errorf("sheets = %d, want 2", sheets)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	for _, want := range []string{"## Blatt: Daten", "```csv", "Miete,850", "## Blatt: Notizen", "Kündigung"} {
		if !strings.Contains(text, want) {
			t.Errorf("extracted text is missing %q:\n%s", want, text)
		}
	}
}

// The sheet count and the row budget are what the result carries, so a sheet that
// runs past the budget has to say so rather than silently look complete.
func TestExtractXlsxWarnsWhenTheRowBudgetTruncates(t *testing.T) {
	rows := make([][]any, 0, 2101)
	rows = append(rows, []any{"Nr"})
	for i := 0; i < 2100; i++ {
		rows = append(rows, []any{i})
	}
	data := makeTestXlsx(t, map[string][][]any{"Lang": rows})

	text, _, warnings, err := extractXlsx(data)
	if err != nil {
		t.Fatalf("extractXlsx failed: %v", err)
	}
	if !strings.Contains(text, "\n1998\n") {
		t.Errorf("the last row inside the 2000-row budget is missing")
	}
	if strings.Contains(text, "\n1999\n") {
		t.Errorf("the output runs past the 2000-row budget")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "2000") {
		t.Errorf("warnings = %v, want one naming the 2000-row budget", warnings)
	}
}

// Bytes that are not a workbook are the ordinary case for a mislabelled
// attachment: an error the window can translate, not a crash.
func TestExtractXlsxRejectsSomethingThatIsNotAWorkbook(t *testing.T) {
	text, sheets, warnings, err := extractXlsx([]byte("das ist keine Tabelle"))
	if err == nil {
		t.Fatal("extractXlsx accepted bytes that are not a workbook")
	}
	if !strings.Contains(err.Error(), "ingest.xlsxOpen") {
		t.Errorf("error should carry the ingest.xlsxOpen code, got %v", err)
	}
	if text != "" || sheets != 0 || warnings != nil {
		t.Errorf("a rejected file should yield nothing, got text=%q sheets=%d warnings=%v", text, sheets, warnings)
	}
}
