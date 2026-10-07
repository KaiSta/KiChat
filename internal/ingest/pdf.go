package ingest

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"

	"kichat/internal/llm"
)

// extractPDF reads a PDF, preferring poppler's pdftotext for layout fidelity and
// falling back to the built-in pure-Go parser. The method used is reported so
// the UI can be honest about the quality of the extraction.
func extractPDF(data []byte, preferExternal bool) (text string, pages int, warnings []string, method string, err error) {
	if preferExternal && PDFToTextAvailable() {
		out, runErr := runPDFToText(data)
		if runErr == nil && strings.TrimSpace(out) != "" {
			text, pages = pageMarkedText(out)
			if isLikelyScanned(text, pages) {
				warnings = append(warnings, scannedWarning)
			}
			return text, pages, warnings, "pdftotext", nil
		}
		if runErr != nil {
			warnings = append(warnings, fmt.Sprintf("pdftotext fehlgeschlagen (%v), nutze internen PDF-Parser", runErr))
		}
	}

	builtinText, builtinPages, builtinErr := extractPDFBuiltin(data)
	if builtinErr != nil {
		if len(warnings) > 0 {
			return "", 0, warnings, "", fmt.Errorf("PDF konnte nicht gelesen werden: %w", builtinErr)
		}
		return "", 0, nil, "", fmt.Errorf("PDF konnte nicht gelesen werden: %w", builtinErr)
	}
	if isLikelyScanned(builtinText, builtinPages) {
		warnings = append(warnings, scannedWarning)
	}
	return builtinText, builtinPages, warnings, "builtin", nil
}

// scannedWarning explains the one limitation the app cannot work around.
const scannedWarning = "Das PDF enthält offenbar kaum Text (gescannte Seiten). " +
	"OCR ist nicht installiert (tesseract fehlt), daher kann der Inhalt nicht gelesen werden."

// isLikelyScanned reports whether a PDF produced almost no text relative to its
// page count, which means it is a scan rather than a text document.
func isLikelyScanned(text string, pages int) bool {
	if pages <= 0 {
		return false
	}
	letters := 0
	for _, r := range text {
		if r != '#' && r != ' ' && r != '\n' && r != '\t' {
			letters++
		}
	}
	return letters < pages*20
}

func runPDFToText(data []byte) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// Form feeds are deliberately kept: pageMarkedText uses them to label pages.
	cmd := exec.CommandContext(ctx, PdfTextPath(),
		"-layout", "-enc", "UTF-8", "-q", "-", "-")
	var stdout, stderr bytes.Buffer
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	return stdout.String(), nil
}

// pageMarkedText splits pdftotext output on form feeds and labels each page so
// the model can cite page numbers.
func pageMarkedText(raw string) (string, int) {
	raw = normaliseLines(raw)
	parts := strings.Split(raw, "\f")
	if len(parts) <= 1 {
		return "## Seite 1\n" + strings.TrimSpace(raw), 1
	}
	var sb strings.Builder
	pages := 0
	for i, part := range parts {
		content := strings.TrimSpace(part)
		if content == "" && i == len(parts)-1 {
			continue
		}
		pages++
		fmt.Fprintf(&sb, "## Seite %d\n%s\n\n", pages, content)
	}
	if pages == 0 {
		pages = 1
	}
	return strings.TrimRight(sb.String(), "\n"), pages
}

// extractPDFBuiltin uses the pure-Go parser. It recovers from panics because
// malformed PDFs can trip up strict parsers.
func extractPDFBuiltin(data []byte) (text string, pages int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("PDF-Parser abgestürzt: %v", r)
		}
	}()
	if !looksLikePDF(data) {
		return "", 0, fmt.Errorf("keine gültige PDF-Datei")
	}
	reader, readErr := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if readErr != nil {
		return "", 0, readErr
	}
	total := reader.NumPage()
	if total <= 0 {
		return "", 0, fmt.Errorf("PDF ohne Seiten")
	}
	var sb strings.Builder
	for i := 1; i <= total; i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}
		content, pageErr := page.GetPlainText(nil)
		if pageErr != nil {
			continue
		}
		content = normaliseLines(content)
		fmt.Fprintf(&sb, "## Seite %d\n%s\n\n", i, content)
	}
	out := strings.TrimRight(sb.String(), "\n")
	if strings.TrimSpace(out) == "" {
		return "", total, nil
	}
	return out, total, nil
}

func looksLikePDF(data []byte) bool {
	if len(data) < 5 {
		return false
	}
	if bytes.HasPrefix(data, []byte("%PDF-")) {
		return true
	}
	// Some producers leave junk before the header.
	header := data
	if len(header) > 1024 {
		header = header[:1024]
	}
	return bytes.Contains(header, []byte("%PDF-"))
}

// pdfExtractorFunc adapts the extractor for the scraper, which only needs text
// and a page count.
func (e *Extractor) PDFExtractorFunc() PDFExtractor {
	return func(data []byte, filename string) (string, int, error) {
		res, err := e.Ingest(filename, "application/pdf", data)
		if err != nil {
			return "", 0, err
		}
		if res.Pages == 0 && llm.EstimateTokens(res.Text) == 0 {
			return "", 0, fmt.Errorf("PDF enthält keinen extrahierbaren Text")
		}
		return res.Text, res.Pages, nil
	}
}
