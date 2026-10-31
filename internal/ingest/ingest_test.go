package ingest

import (
	"archive/zip"
	"bytes"
	"fmt"
	"kichat/internal/config"
	"strings"
	"testing"

	"kichat/internal/llm"
)

// makeTestPDF builds a genuinely valid single-page PDF, including a correct
// cross-reference table, so the extraction path is tested against a real file
// rather than a stub.
func makeTestPDF(lines ...string) []byte {
	var buf bytes.Buffer
	var offsets []int

	buf.WriteString("%PDF-1.4\n")
	writeObj := func(body string) {
		offsets = append(offsets, buf.Len())
		buf.WriteString(body)
	}

	writeObj("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	writeObj("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	writeObj("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 400 400] " +
		"/Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>\nendobj\n")

	var stream strings.Builder
	y := 340
	for _, line := range lines {
		fmt.Fprintf(&stream, "BT /F1 12 Tf 30 %d Td (%s) Tj ET\n", y, line)
		y -= 20
	}
	writeObj(fmt.Sprintf("4 0 obj\n<< /Length %d >>\nstream\n%sendstream\nendobj\n",
		stream.Len(), stream.String()))
	writeObj("5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n")

	xref := buf.Len()
	buf.WriteString("xref\n0 6\n")
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return buf.Bytes()
}

// TestExtractPDFGetsTextAndPageMarkers exercises the whole PDF path and checks
// the page labelling the model relies on for citations.
func TestExtractPDFGetsTextAndPageMarkers(t *testing.T) {
	data := makeTestPDF("Mietrecht in Baden-Wuerttemberg", "Kuendigungsfrist drei Monate")
	if !looksLikePDF(data) {
		t.Fatal("synthesised file is not recognised as a PDF")
	}

	for _, preferExternal := range []bool{false, true} {
		method := "builtin"
		if preferExternal {
			method = "pdftotext"
		}
		text, pages, warnings, used, err := extractPDF(data, preferExternal, config.LangGerman)
		if err != nil {
			t.Fatalf("%s path: extractPDF failed: %v", method, err)
		}
		if pages < 1 {
			t.Errorf("%s path: pages = %d, want >= 1", method, pages)
		}
		if !strings.Contains(text, "## Seite 1") {
			t.Errorf("%s path: page marker missing from %q", method, text)
		}
		if !strings.Contains(text, "Kuendigungsfrist") {
			t.Errorf("%s path: extracted text missing content: %q", method, text)
		}
		if used == "" {
			t.Errorf("%s path: no extraction method reported", method)
		}
		// A text PDF must not be misreported as a scan; that warning tells the
		// user their document is unreadable, so a false positive is harmful.
		for _, w := range warnings {
			if strings.Contains(w, "gescannte") {
				t.Errorf("%s path: falsely flagged as scanned: %s", method, w)
			}
		}
	}
	if !PDFToTextAvailable() {
		t.Log("pdftotext is not installed; only the built-in parser was exercised")
	}
}

func TestExtractPDFRejectsNonPDF(t *testing.T) {
	if _, _, _, _, err := extractPDF([]byte("das ist kein PDF"), false, config.LangGerman); err == nil {
		t.Error("expected an error for non-PDF input")
	}
}

func TestExtractPDFSurvivesTruncatedFile(t *testing.T) {
	data := makeTestPDF("Inhalt")
	truncated := data[:len(data)/2]
	// It must not panic; an error is the correct outcome.
	_, _, _, _, err := extractPDF(truncated, false, config.LangGerman)
	if err == nil {
		t.Log("truncated input parsed without error; acceptable as long as it did not panic")
	}
}

func TestIsLikelyScanned(t *testing.T) {
	// A page of real text is not a scan.
	if isLikelyScanned("## Seite 1\n"+strings.Repeat("Ein Satz mit Inhalt. ", 20), 1) {
		t.Error("text PDF misdetected as scanned")
	}
	// Ten pages with almost no text is a scan.
	if !isLikelyScanned("## Seite 1\n\n## Seite 2\n", 10) {
		t.Error("an image-only PDF should be detected as scanned")
	}
	if isLikelyScanned("anything", 0) {
		t.Error("a document with no pages cannot be judged")
	}
}

func TestPageMarkedTextLabelsPages(t *testing.T) {
	raw := "Erste Seite Inhalt\fZweite Seite Inhalt\f"
	text, pages := pageMarkedText(raw, config.LangGerman)
	if pages != 2 {
		t.Fatalf("pages = %d, want 2", pages)
	}
	if !strings.Contains(text, "## Seite 1") || !strings.Contains(text, "## Seite 2") {
		t.Fatalf("page labels missing: %q", text)
	}
	if strings.Contains(text, "\f") {
		t.Errorf("form feed leaked into the text: %q", text)
	}
}

func TestIngestDispatchesByExtension(t *testing.T) {
	e := New()

	txt, err := e.Ingest("notizen.md", "text/markdown", []byte("# Titel\n\nInhalt"))
	if err != nil || txt.Kind != KindText {
		t.Fatalf("markdown: %+v, %v", txt, err)
	}
	if txt.Tokens <= 0 {
		t.Error("no tokens estimated for text input")
	}

	pdf, err := e.Ingest("bericht.pdf", "application/pdf", makeTestPDF("Hallo Welt"))
	if err != nil || pdf.Kind != KindPDF {
		t.Fatalf("pdf: %+v, %v", pdf, err)
	}
	if pdf.Pages != 1 {
		t.Errorf("pdf pages = %d, want 1", pdf.Pages)
	}

	img, err := e.Ingest("foto.png", "", []byte{0x89, 'P', 'N', 'G'})
	if err != nil || img.Kind != KindImage {
		t.Fatalf("image: %+v, %v", img, err)
	}
	if len(img.Warnings) == 0 {
		t.Error("an image attachment should warn unless the model supports vision")
	}

	// A binary with no text must be reported, never silently sent as empty.
	bin, err := e.Ingest("datei.bin", "application/octet-stream",
		[]byte{0x00, 0x01, 0x02, 0x00, 0x03})
	if err != nil {
		t.Fatalf("binary: %v", err)
	}
	if bin.Kind != KindBinary || len(bin.Warnings) == 0 {
		t.Errorf("binary = %+v, want KindBinary with a warning", bin)
	}
}

func TestDecodeTextHandlesEncodings(t *testing.T) {
	// UTF-8 with a BOM.
	if got := decodeText([]byte{0xEF, 0xBB, 0xBF, 'H', 'i'}); got != "Hi" {
		t.Errorf("BOM not stripped: %q", got)
	}
	// UTF-16LE with a BOM.
	utf16le := []byte{0xFF, 0xFE, 'H', 0x00, 'i', 0x00}
	if got := decodeText(utf16le); got != "Hi" {
		t.Errorf("UTF-16LE = %q, want Hi", got)
	}
	// Windows-1252 bytes that are not valid UTF-8 must still decode.
	// 0xFC is ü and 0xDF is ß in Windows-1252.
	windows1252 := []byte{0x4D, 0xFC, 0x6E, 0x63, 0x68, 0x6E, 0x65, 0x72, 0x20, 0x47, 0x72, 0xFC, 0xDF, 0x65}
	got := decodeText(windows1252)
	if !strings.Contains(got, "ü") || !strings.Contains(got, "ß") {
		t.Errorf("Windows-1252 not decoded: %q", got)
	}
	if strings.Contains(got, "\uFFFD") {
		t.Errorf("decoding produced replacement characters: %q", got)
	}
}

func TestExtractDocxAndPptx(t *testing.T) {
	docx := makeZip(t, map[string]string{
		"word/document.xml": `<?xml version="1.0"?>
<w:document xmlns:w="x"><w:body>
<w:p><w:r><w:t>Erster Absatz</w:t></w:r></w:p>
<w:p><w:r><w:t>Zweiter</w:t></w:r><w:r><w:t> Absatz</w:t></w:r></w:p>
</w:body></w:document>`,
	})
	text, warnings, err := extractDocx(docx, config.LangGerman)
	if err != nil {
		t.Fatalf("extractDocx: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if !strings.Contains(text, "Erster Absatz") || !strings.Contains(text, "Zweiter Absatz") {
		t.Fatalf("docx text = %q", text)
	}
	// Paragraphs must stay separate, not run together.
	if strings.Contains(text, "AbsatzZweiter") {
		t.Error("paragraph boundaries were lost")
	}

	pptx := makeZip(t, map[string]string{
		"ppt/slides/slide1.xml": `<p:sld xmlns:a="x"><a:t>Folie eins Titel</a:t><a:p/></p:sld>`,
		"ppt/slides/slide2.xml": `<p:sld xmlns:a="x"><a:t>Folie zwei Titel</a:t></p:sld>`,
	})
	slideText, slides, _, err := extractPptx(pptx, config.LangGerman)
	if err != nil {
		t.Fatalf("extractPptx: %v", err)
	}
	if slides != 2 {
		t.Errorf("slides = %d, want 2", slides)
	}
	if !strings.Contains(slideText, "## Folie 1") || !strings.Contains(slideText, "## Folie 2") {
		t.Fatalf("slide markers missing: %q", slideText)
	}
	if !strings.Contains(slideText, "Folie eins Titel") {
		t.Fatalf("slide text = %q", slideText)
	}
}

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip add %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// --- chunker ---------------------------------------------------------------

func TestChunkDocumentRespectsTargetSize(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 120; i++ {
		sb.WriteString("Dies ist Absatz Nummer ")
		sb.WriteString(fmt.Sprint(i))
		sb.WriteString(" mit ausreichend Text, um mehrere Tokens zu belegen und den Chunker zu fordern.\n\n")
	}
	spec := ChunkSpec{TargetTokens: 300, OverlapRatio: 0.15}
	chunks := ChunkDocument(sb.String(), spec)
	if len(chunks) < 3 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Tokens == 0 {
			t.Errorf("chunk %d has no token estimate", i)
		}
		// Allow generous headroom: a chunk may overshoot by one block.
		if c.Tokens > spec.TargetTokens*3 {
			t.Errorf("chunk %d is %d tokens, far above the %d target", i, c.Tokens, spec.TargetTokens)
		}
		if c.Ordinal != i {
			t.Errorf("chunk %d has ordinal %d", i, c.Ordinal)
		}
	}
}

// TestChunkDocumentOverlapsSoFactsSurviveBoundaries is the reason for overlapping
// chunks: a sentence spanning a boundary must still be retrievable.
func TestChunkDocumentOverlapsSoFactsSurviveBoundaries(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&sb, "Abschnitt %d. ", i)
		sb.WriteString(strings.Repeat("Inhalt für den Chunker. ", 6))
		sb.WriteString("\n\n")
	}
	chunks := ChunkDocument(sb.String(), ChunkSpec{TargetTokens: 200, OverlapRatio: 0.25})
	if len(chunks) < 3 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	overlaps := 0
	for i := 1; i < len(chunks); i++ {
		// The end of one chunk should reappear at the start of the next.
		tail := tailWords(chunks[i-1].Text, 6)
		if tail != "" && strings.Contains(chunks[i].Text, tail) {
			overlaps++
		}
	}
	if overlaps == 0 {
		t.Error("no overlap detected between consecutive chunks")
	}
	if chunks[0].Ordinal != 0 {
		t.Error("ordinals must start at zero")
	}
}

func tailWords(text string, n int) string {
	words := strings.Fields(text)
	if len(words) < n {
		return ""
	}
	return strings.Join(words[len(words)-n:], " ")
}

func TestChunkDocumentTracksPageAndHeading(t *testing.T) {
	text := "## Seite 4\n\n# Mietrecht\n\nDie Kündigungsfrist beträgt drei Monate für Mieter.\n\n" +
		"# Steuerrecht\n\nEin ganz anderer Abschnitt über Steuern und Abgaben.\n"
	chunks := ChunkDocument(text, ChunkSpec{TargetTokens: 12, OverlapRatio: 0})
	if len(chunks) < 2 {
		t.Fatalf("expected the headings to split chunks, got %d", len(chunks))
	}
	sawPage := false
	sawHeading := ""
	for _, c := range chunks {
		if c.Page == 4 {
			sawPage = true
		}
		if c.Heading != "" {
			sawHeading = c.Heading
		}
	}
	if !sawPage {
		t.Error("the page marker was not carried into the chunks")
	}
	if sawHeading == "" {
		t.Error("headings were not attached to chunks")
	}
}

// TestChunkDocumentReadsThePageMarkersTheExtractorWrites is the regression test
// for page numbers never reaching the index. The extractor emits a marker as the
// first line of a page ("## Seite 3\n<content>"), while this package's own test
// fixture wrote it as a paragraph of its own — so the chunker passed its test
// and produced page 0 for every real PDF. The two sides are pinned against each
// other here by chunking the extractor's actual output rather than a hand-written
// look-alike.
func TestChunkDocumentReadsThePageMarkersTheExtractorWrites(t *testing.T) {
	marked, pages := pageMarkedText("Erste Seite mit Inhalt.\n\fZweite Seite mit anderem Inhalt.\n", config.LangGerman)
	if pages != 2 {
		t.Fatalf("fixture produced %d pages, want 2", pages)
	}
	chunks := ChunkDocument(marked, ChunkSpec{TargetTokens: 8, OverlapRatio: 0})
	if len(chunks) == 0 {
		t.Fatal("the extractor's own output produced no chunks")
	}

	byPage := map[int]int{}
	for _, c := range chunks {
		byPage[c.Page]++
	}
	if byPage[0] != 0 {
		t.Errorf("%d chunk(s) have no page at all: %+v", byPage[0], chunks)
	}
	if byPage[1] == 0 || byPage[2] == 0 {
		t.Errorf("expected chunks on pages 1 and 2, got %+v", byPage)
	}
	// The page marker stays in the text so the page remains citable, and it must
	// not be mistaken for a heading.
	for _, c := range chunks {
		if strings.HasPrefix(c.Text, "## Seite") && c.Heading != "" {
			t.Errorf("page marker became a heading on page %d: %q", c.Page, c.Heading)
		}
	}
}

// TestChunkDocumentReadsAHeadingGluedToItsBody covers the same anchoring mistake
// for headings: an extractor that writes "# Titel\nBody" in one paragraph must
// still produce the heading, which is what the retrieval tools label their hits
// with.
func TestChunkDocumentReadsAHeadingGluedToItsBody(t *testing.T) {
	chunks := ChunkDocument("# Mietrecht\nDie Kündigungsfrist beträgt drei Monate.",
		ChunkSpec{TargetTokens: 200, OverlapRatio: 0})
	if len(chunks) != 1 {
		t.Fatalf("expected one chunk, got %d", len(chunks))
	}
	if chunks[0].Heading != "Mietrecht" {
		t.Errorf("Heading = %q, want Mietrecht", chunks[0].Heading)
	}
	if !strings.Contains(chunks[0].Text, "Mietrecht") {
		t.Errorf("the heading line should stay in the text, got %q", chunks[0].Text)
	}
}

func TestChunkDocumentSplitsOversizedParagraph(t *testing.T) {
	// One enormous paragraph with no blank lines must still be split, otherwise a
	// single chunk could exceed the retrieval budget.
	long := strings.Repeat("Dies ist ein Satz mit Inhalt. ", 400)
	chunks := ChunkDocument(long, ChunkSpec{TargetTokens: 200, OverlapRatio: 0.1})
	if len(chunks) < 2 {
		t.Fatalf("an oversized paragraph was not split: %d chunk(s)", len(chunks))
	}
	for _, c := range chunks {
		if c.Tokens > 700 {
			t.Errorf("chunk of %d tokens exceeds any reasonable bound", c.Tokens)
		}
	}
}

func TestChunkDocumentHandlesEmptyAndTinyInput(t *testing.T) {
	if got := ChunkDocument("", ChunkSpec{}); got != nil {
		t.Errorf("empty input should yield no chunks, got %+v", got)
	}
	if got := ChunkDocument("   \n\n  ", ChunkSpec{}); got != nil {
		t.Errorf("whitespace-only input should yield no chunks, got %+v", got)
	}
	got := ChunkDocument("Nur ein Satz.", ChunkSpec{})
	if len(got) != 1 {
		t.Fatalf("tiny input = %+v, want exactly one chunk", got)
	}
	if got[0].Tokens <= 0 {
		t.Error("token estimate missing")
	}
}

func TestDefaultChunkSpecIsSane(t *testing.T) {
	spec := DefaultChunkSpec()
	if spec.TargetTokens < 200 || spec.TargetTokens > 2000 {
		t.Errorf("target tokens = %d, want a retrieval-friendly size", spec.TargetTokens)
	}
	if spec.OverlapRatio <= 0 || spec.OverlapRatio >= 0.5 {
		t.Errorf("overlap = %v, want a modest overlap", spec.OverlapRatio)
	}
	if llm.EstimateTokens(strings.Repeat("wort ", 100)) == 0 {
		t.Error("estimator is not wired in")
	}
}
