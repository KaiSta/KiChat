package ingest

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"

	"kichat/internal/uimsg"
)

// OOXML files are ZIP archives of XML parts. Rather than pull in a heavy office
// library, the text runs are collected directly from the document parts, which
// is enough for the model and keeps the dependency surface small.

// extractDocx pulls paragraphs from word/document.xml.
func extractDocx(data []byte, lang string) (string, []string, error) {
	zr, err := zipReader(data)
	if err != nil {
		return "", nil, uimsg.Errorf("ingest.docxOpen", err)
	}
	var warnings []string

	var out []string
	// Headers and footers carry real content in many documents.
	names := []string{"word/document.xml"}
	for _, name := range zipNames(zr) {
		if strings.HasPrefix(name, "word/header") || strings.HasPrefix(name, "word/footer") {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		part, err := zipPart(zr, name)
		if err != nil {
			continue
		}
		partText, err := collectOOXMLText(part, ooxmlSpec{
			textTags:    []string{"t"},
			breakTags:   []string{"p"},
			tabTags:     []string{"tab"},
			breakMarker: "\n\n",
		})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(text(lang,
				"%s konnte nicht gelesen werden", "%s could not be read"), name))
			continue
		}
		if strings.TrimSpace(partText) != "" {
			if strings.Contains(name, "header") || strings.Contains(name, "footer") {
				out = append(out, "### "+path.Base(name)+"\n"+partText)
			} else {
				out = append(out, partText)
			}
		}
	}
	if len(out) == 0 {
		warnings = append(warnings, text(lang,
			"DOCX enthielt keinen Text (evtl. nur Bilder)",
			"the DOCX contained no text (perhaps only pictures)"))
	}
	return normaliseLines(strings.Join(out, "\n\n")), warnings, nil
}

// extractPptx pulls slide text, keeping slides separate and ordered.
func extractPptx(data []byte, lang string) (string, int, []string, error) {
	zr, err := zipReader(data)
	if err != nil {
		return "", 0, nil, uimsg.Errorf("ingest.pptxOpen", err)
	}

	type slide struct {
		index int
		name  string
	}
	var slides []slide
	for _, name := range zipNames(zr) {
		base := path.Base(name)
		if path.Dir(name) != "ppt/slides" || !strings.HasPrefix(base, "slide") ||
			!strings.HasSuffix(base, ".xml") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(base, "slide"), ".xml"))
		if err != nil {
			continue
		}
		slides = append(slides, slide{index: n, name: name})
	}
	if len(slides) == 0 {
		return "", 0, nil, uimsg.Errorf("ingest.pptxNoSlides")
	}
	sort.Slice(slides, func(i, j int) bool { return slides[i].index < slides[j].index })

	var (
		sb       strings.Builder
		notes    []string
		warnings []string
	)
	for _, s := range slides {
		part, err := zipPart(zr, s.name)
		if err != nil {
			continue
		}
		slideText, err := collectOOXMLText(part, ooxmlSpec{
			textTags:    []string{"t"},
			breakTags:   []string{"p"},
			breakMarker: "\n",
		})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(text(lang,
				"Folie %d nicht lesbar", "slide %d could not be read"), s.index))
			continue
		}
		fmt.Fprintf(&sb, "## Folie %d\n%s\n\n", s.index, strings.TrimSpace(slideText))

		if noteName := fmt.Sprintf("ppt/notesSlides/notesSlide%d.xml", s.index); hasZipPart(zr, noteName) {
			if notePart, err := zipPart(zr, noteName); err == nil {
				if noteText, err := collectOOXMLText(notePart, ooxmlSpec{
					textTags: []string{"t"}, breakTags: []string{"p"}, breakMarker: "\n",
				}); err == nil && strings.TrimSpace(noteText) != "" {
					notes = append(notes, fmt.Sprintf("- Folie %d: %s", s.index, strings.TrimSpace(noteText)))
				}
			}
		}
	}
	if len(notes) > 0 {
		sb.WriteString("\n### Sprechernotizen\n")
		sb.WriteString(strings.Join(notes, "\n"))
	}
	return normaliseLines(sb.String()), len(slides), warnings, nil
}

// extractXlsx renders each sheet as CSV, which keeps tables interpretable.
//
// It recovers from panics for the same reason extractPDFBuiltin does, and one is
// known rather than hypothetical: excelize has no recover anywhere, and a workbook
// whose pivot table names a field the pivot cache does not have panics with an
// index out of range (measured against v2.11.0 — an 8 KB workbook with
// `<dataField fld="99999">` and a two-field cache dies at pivotTable.go:1444 via
// GetPivotTables). That particular call is not made here, but the file this
// function is handed comes from a chat attachment, and a parser that ends the
// process on a malformed one is worse than a spreadsheet that cannot be read.
func extractXlsx(data []byte, lang string) (content string, sheets int, warnings []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			content, sheets, warnings, err = "", 0, nil, uimsg.Errorf("ingest.xlsxCrashed", r)
		}
	}()
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return "", 0, nil, uimsg.Errorf("ingest.xlsxOpen", err)
	}
	defer f.Close()

	names := f.GetSheetList()
	if len(names) == 0 {
		return "", 0, nil, uimsg.Errorf("ingest.xlsxNoSheets")
	}
	var sb strings.Builder
	const maxRows = 2000
	for _, sheet := range names {
		fmt.Fprintf(&sb, "## Blatt: %s\n\n```csv\n", sheet)
		read, truncated, failed := 0, false, false
		it, err := f.Rows(sheet)
		if err != nil {
			if fallback, rowCount, truncatedFallback, ok, fallbackErr := xlsxSheetFallbackCSV(data, sheet, maxRows); fallbackErr == nil && ok {
				sb.WriteString(fallback)
				read = rowCount
				truncated = truncatedFallback
			} else {
				failed = true
			}
		} else {
			for it.Next() {
				if read >= maxRows {
					truncated = true
					break
				}
				row, err := it.Columns()
				if err != nil {
					failed = true
					break
				}
				sb.WriteString(strings.Join(row, ","))
				sb.WriteString("\n")
				read++
			}
			if err := it.Error(); err != nil {
				failed = true
			}
			_ = it.Close()
			if failed {
				if fallback, rowCount, truncatedFallback, ok, fallbackErr := xlsxSheetFallbackCSV(data, sheet, maxRows); fallbackErr == nil && ok {
					sb.Reset()
					fmt.Fprintf(&sb, "## Blatt: %s\n\n```csv\n", sheet)
					sb.WriteString(fallback)
					read = rowCount
					truncated = truncatedFallback
					failed = false
				} else {
					failed = true
				}
			}
		}
		sb.WriteString("```\n\n")
		if failed {
			warnings = append(warnings, fmt.Sprintf(text(lang,
				"Blatt %q nicht lesbar", "sheet %q could not be read"), sheet))
		}
		if truncated {
			warnings = append(warnings, fmt.Sprintf(text(lang,
				"Blatt %q auf %d Zeilen gekürzt", "sheet %q was cut to %d rows"), sheet, maxRows))
		}
	}
	return normaliseLines(sb.String()), len(names), warnings, nil
}

var xlsxSheetNameRE = regexp.MustCompile(`(?s)<sheet\b[^>]*name="([^"]+)"[^>]*r:id="([^"]+)"[^>]*>`)
var xlsxRelationshipRE = regexp.MustCompile(`(?s)<Relationship\b[^>]*Id="([^"]+)"[^>]*Target="([^"]+)"[^>]*>`)
var xlsxRowRE = regexp.MustCompile(`(?s)<row\b[^>]*>(.*?)</row>`)
var xlsxCellRE = regexp.MustCompile(`(?s)<c\b([^>]*)>(.*?)</c>`)
var xlsxAttrRE = regexp.MustCompile(`\s+r="([^"]+)"`)
var xlsxValueRE = regexp.MustCompile(`(?s)<v>(.*?)</v>`)
var xlsxTextRE = regexp.MustCompile(`(?s)<t>(.*?)</t>`)

func xlsxSheetFallbackCSV(data []byte, sheet string, maxRows int) (string, int, bool, bool, error) {
	zr, err := zipReader(data)
	if err != nil {
		return "", 0, false, false, err
	}
	sheetPath, err := xlsxSheetPath(zr, sheet)
	if err != nil {
		return "", 0, false, false, err
	}
	part, err := zipPart(zr, sheetPath)
	if err != nil {
		return "", 0, false, false, err
	}
	raw, err := io.ReadAll(part)
	if err != nil {
		return "", 0, false, false, err
	}
	csv, count, truncated, err := xlsxRowsToCSV(raw, maxRows)
	if err != nil {
		return "", 0, false, false, err
	}
	if count == 0 && strings.TrimSpace(csv) == "" {
		return "", 0, false, false, nil
	}
	return csv, count, truncated, true, nil
}

func xlsxSheetPath(zr *zip.Reader, sheet string) (string, error) {
	wb, err := zipPart(zr, "xl/workbook.xml")
	if err != nil {
		return "", err
	}
	workbook, err := io.ReadAll(wb)
	if err != nil {
		return "", err
	}
	rels, err := zipPart(zr, "xl/_rels/workbook.xml.rels")
	if err != nil {
		return "", err
	}
	relRaw, err := io.ReadAll(rels)
	if err != nil {
		return "", err
	}
	relMap := map[string]string{}
	for _, match := range xlsxRelationshipRE.FindAllSubmatch(relRaw, -1) {
		if len(match) < 3 {
			continue
		}
		relMap[string(match[1])] = string(match[2])
	}
	for _, match := range xlsxSheetNameRE.FindAllSubmatch(workbook, -1) {
		if len(match) < 3 {
			continue
		}
		name := string(match[1])
		if !strings.EqualFold(name, sheet) {
			continue
		}
		target := relMap[string(match[2])]
		if target == "" {
			return "", uimsg.Errorf("ingest.xlsxNoSheetRel", sheet)
		}
		if strings.HasPrefix(target, "/") {
			return "xl" + target, nil
		}
		if strings.HasPrefix(target, "xl/") {
			return target, nil
		}
		return "xl/" + strings.TrimPrefix(target, "./"), nil
	}
	return "", uimsg.Errorf("ingest.xlsxMissingSheet", sheet)
}

func xlsxRowsToCSV(raw []byte, maxRows int) (string, int, bool, error) {
	var sb strings.Builder
	rowCount := 0
	truncated := false
	for _, rowMatch := range xlsxRowRE.FindAllSubmatch(raw, -1) {
		rowXML := string(rowMatch[0])
		rowAttr := xlsxAttrRE.FindStringSubmatch(rowXML)
		if len(rowAttr) < 2 {
			continue
		}
		rowNum, err := strconv.ParseUint(rowAttr[1], 10, 64)
		if err != nil || rowNum > uint64(maxRows)*10 {
			continue
		}
		if rowNum > uint64(maxRows) {
			continue
		}
		cells := map[int]string{}
		for _, cellMatch := range xlsxCellRE.FindAllSubmatch(rowMatch[1], -1) {
			if len(cellMatch) < 3 {
				continue
			}
			cellAttrs := string(cellMatch[1])
			cellRefAttr := xlsxAttrRE.FindStringSubmatch(cellAttrs)
			if len(cellRefAttr) < 2 {
				continue
			}
			col, err := xlsxColumnIndex(cellRefAttr[1])
			if err != nil {
				continue
			}
			cells[col] = xlsxCellValue(string(cellMatch[2]))
		}
		if len(cells) == 0 {
			continue
		}
		maxCol := 0
		for col := range cells {
			if col > maxCol {
				maxCol = col
			}
		}
		vals := make([]string, maxCol)
		for i := 1; i <= maxCol; i++ {
			vals[i-1] = cells[i]
		}
		sb.WriteString(strings.Join(vals, ","))
		sb.WriteString("\n")
		rowCount++
		if rowCount >= maxRows {
			truncated = true
			break
		}
	}
	return sb.String(), rowCount, truncated, nil
}

func xlsxColumnIndex(cellRef string) (int, error) {
	head := cellRef
	for i, ch := range head {
		if ch >= '0' && ch <= '9' {
			head = head[:i]
			break
		}
	}
	if head == "" {
		return 0, fmt.Errorf("missing column part in %q", cellRef)
	}
	v := 0
	for _, ch := range head {
		if ch < 'A' || ch > 'Z' {
			if ch < 'a' || ch > 'z' {
				return 0, fmt.Errorf("non-column ref %q", cellRef)
			}
			ch = ch - 'a' + 'A'
		}
		v = v*26 + int(ch-'A'+1)
	}
	return v, nil
}

func xlsxCellValue(raw string) string {
	if value := xlsxValueRE.FindStringSubmatch(raw); len(value) > 1 {
		return string(value[1])
	}
	if text := xlsxTextRE.FindStringSubmatch(raw); len(text) > 1 {
		return string(text[1])
	}
	return ""
}

// --- ooxml plumbing --------------------------------------------------------

type ooxmlSpec struct {
	textTags    []string
	breakTags   []string
	tabTags     []string
	breakMarker string
}

// collectOOXMLText walks an XML part and concatenates text runs, inserting the
// break marker whenever a break element closes.
func collectOOXMLText(r io.Reader, spec ooxmlSpec) (string, error) {
	decoder := xml.NewDecoder(r)
	var sb strings.Builder
	var depth int
	inText := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return sb.String(), err
		}
		switch t := token.(type) {
		case xml.StartElement:
			depth++
			name := t.Name.Local
			if containsFold(spec.textTags, name) {
				inText++
			}
			if containsFold(spec.tabTags, name) {
				sb.WriteString("\t")
			}
		case xml.CharData:
			if inText > 0 {
				sb.WriteString(string(t))
			}
		case xml.EndElement:
			depth--
			name := t.Name.Local
			if containsFold(spec.textTags, name) && inText > 0 {
				inText--
			}
			if containsFold(spec.breakTags, name) {
				if spec.breakMarker != "" {
					sb.WriteString(spec.breakMarker)
				}
			}
		}
	}
	return normaliseLines(sb.String()), nil
}

func containsFold(list []string, name string) bool {
	for _, item := range list {
		if strings.EqualFold(item, name) {
			return true
		}
	}
	return false
}

func zipReader(data []byte) (*zip.Reader, error) {
	return zip.NewReader(bytes.NewReader(data), int64(len(data)))
}

func zipNames(zr *zip.Reader) []string {
	out := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	return out
}

func hasZipPart(zr *zip.Reader, name string) bool {
	_, err := zipPart(zr, name)
	return err == nil
}

func zipPart(zr *zip.Reader, name string) (io.Reader, error) {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		// The part is consumed immediately by the XML decoder, so buffering it
		// keeps the returned reader independent of the zip handle.
		raw, err := io.ReadAll(io.LimitReader(rc, 64<<20))
		rc.Close()
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(raw), nil
	}
	return nil, uimsg.Errorf("ingest.partMissing", name)
}
