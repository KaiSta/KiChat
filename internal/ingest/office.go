package ingest

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path"
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
		// Streamed rather than read with GetRows, because GetRows costs one step per
		// row *number*, not per row present: a 1.6 KB workbook whose second <row>
		// carries r="2000000000" took 8.271 s and returned one row (measured on
		// v2.11.0 on this machine; the cost is linear, and the r="231999999999940"
		// in Apache POI's fuzz corpus is about eleven days of one core). The limit
		// is checked in Rows.Next but not in Rows.Columns, which sets the row number
		// from the next element it consumes — so the bound below is what keeps a
		// crafted attachment from parking a core forever. It also stops the whole
		// sheet being materialised before the row budget throws most of it away.
		it, err := f.Rows(sheet)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(text(lang,
				"Blatt %q nicht lesbar", "sheet %q could not be read"), sheet))
			continue
		}
		fmt.Fprintf(&sb, "## Blatt: %s\n\n```csv\n", sheet)
		read, truncated, failed := 0, false, false
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
