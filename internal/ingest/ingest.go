// Package ingest turns attached files into text the model can read. PDFs use a
// hybrid extractor, Office formats are parsed natively, and everything else is
// decoded with charset detection.
package ingest

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	"kichat/internal/llm"
)

// Kind classifies what was ingested.
type Kind string

const (
	KindText   Kind = "text"
	KindPDF    Kind = "pdf"
	KindDocx   Kind = "docx"
	KindPptx   Kind = "pptx"
	KindXlsx   Kind = "xlsx"
	KindImage  Kind = "image"
	KindBinary Kind = "binary"
)

// Result is the outcome of ingesting one file.
type Result struct {
	Kind     Kind
	Title    string
	Text     string
	Pages    int
	Tokens   int
	Warnings []string
	// Method records how the text was obtained, e.g. "pdftotext" or "builtin".
	Method string
	// ImageData carries base64 image bytes for vision models.
	ImageData string
	Mime      string
}

// PDFExtractor is the interface the scraper uses, so a scraped PDF and an
// attached PDF go through exactly the same code path.
type PDFExtractor func(data []byte, filename string) (text string, pages int, err error)

// Extractor ingests files.
type Extractor struct {
	// PreferExternalPDF uses poppler's pdftotext when it is installed. It gives
	// noticeably better fidelity on multi-column and table-heavy PDFs.
	PreferExternalPDF bool
}

// New returns an Extractor with pdftotext auto-detected.
func New() *Extractor {
	return &Extractor{PreferExternalPDF: PDFToTextAvailable()}
}

// pdfToTextPath caches the resolved pdftotext location (-1 when absent).
var pdfToTextPath = func() *string {
	if p, err := exec.LookPath("pdftotext"); err == nil {
		return &p
	}
	var none string
	return &none
}()

// PDFToTextAvailable reports whether poppler's pdftotext is on PATH.
func PDFToTextAvailable() bool { return *pdfToTextPath != "" }

// PdfTextPath returns the resolved pdftotext path, or "".
func PdfTextPath() string { return *pdfToTextPath }

// Ingest extracts text from a file's bytes.
func (e *Extractor) Ingest(filename, mime string, data []byte) (Result, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	title := filepath.Base(filename)

	switch {
	case isImageExt(ext) || strings.HasPrefix(mime, "image/"):
		if mime == "" {
			mime = guessImageMime(ext)
		}
		return Result{
			Kind:  KindImage,
			Title: title,
			Mime:  mime,
			Warnings: []string{
				"Bilddatei: wird nur an Modelle mit Vision-Unterstützung gesendet.",
			},
		}, nil
	case ext == ".pdf" || mime == "application/pdf":
		text, pages, warnings, method, err := extractPDF(data, e.PreferExternalPDF)
		if err != nil {
			return Result{}, err
		}
		return Result{
			Kind:     KindPDF,
			Title:    title,
			Text:     text,
			Pages:    pages,
			Tokens:   llm.EstimateTokens(text),
			Warnings: warnings,
			Method:   method,
			Mime:     "application/pdf",
		}, nil
	case ext == ".docx" || ext == ".doc":
		text, warnings, err := extractDocx(data)
		if err != nil {
			return Result{}, err
		}
		return Result{
			Kind:     KindDocx,
			Title:    title,
			Text:     text,
			Tokens:   llm.EstimateTokens(text),
			Warnings: warnings,
			Method:   "ooxml",
			Mime:     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		}, nil
	case ext == ".pptx" || ext == ".ppt":
		text, pages, warnings, err := extractPptx(data)
		if err != nil {
			return Result{}, err
		}
		return Result{
			Kind:     KindPptx,
			Title:    title,
			Text:     text,
			Pages:    pages,
			Tokens:   llm.EstimateTokens(text),
			Warnings: warnings,
			Method:   "ooxml",
			Mime:     "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		}, nil
	case ext == ".xlsx" || ext == ".xls" || ext == ".ods":
		text, pages, warnings, err := extractXlsx(data)
		if err != nil {
			return Result{}, err
		}
		return Result{
			Kind:     KindXlsx,
			Title:    title,
			Text:     text,
			Pages:    pages,
			Tokens:   llm.EstimateTokens(text),
			Warnings: warnings,
			Method:   "excelize",
			Mime:     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		}, nil
	}

	// Anything else is treated as text and decoded with charset detection.
	if isBinary(data) {
		return Result{
			Kind:     KindBinary,
			Title:    title,
			Mime:     mime,
			Warnings: []string{"Binärformat ohne Textinhalt — nicht in den Kontext übernommen."},
		}, nil
	}
	text := decodeText(data)
	if strings.TrimSpace(text) == "" {
		return Result{}, fmt.Errorf("Datei enthält keinen lesbaren Text")
	}
	return Result{
		Kind:   KindText,
		Title:  title,
		Text:   text,
		Tokens: llm.EstimateTokens(text),
		Method: "plain",
		Mime:   mime,
	}, nil
}

func isImageExt(ext string) bool {
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp":
		return true
	}
	return false
}

func guessImageMime(ext string) string {
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".bmp":
		return "image/bmp"
	}
	return "application/octet-stream"
}

// isBinary uses a NUL-byte heuristic, which reliably separates text from
// binaries without guessing at the extension.
func isBinary(data []byte) bool {
	limit := len(data)
	if limit > 8192 {
		limit = 8192
	}
	for i := 0; i < limit; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// decodeText decodes bytes to UTF-8, honouring BOMs and falling back to the
// Windows-1252/Latin-1 pair that older German documents still use.
func decodeText(data []byte) string {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return string(data[3:])
	}
	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE {
		if out, _, err := transform.Bytes(unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder(), data[2:]); err == nil {
			return string(out)
		}
	}
	if len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF {
		if out, _, err := transform.Bytes(unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewDecoder(), data[2:]); err == nil {
			return string(out)
		}
	}
	if utf8.Valid(data) {
		return string(data)
	}
	if out, _, err := transform.Bytes(charmap.Windows1252.NewDecoder(), data); err == nil {
		return string(out)
	}
	if out, _, err := transform.Bytes(charmap.ISO8859_1.NewDecoder(), data); err == nil {
		return string(out)
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}

func trimBOM(s string) string { return strings.TrimPrefix(s, "\uFEFF") }

// normaliseLines collapses trailing whitespace and excess blank lines.
func normaliseLines(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			blank++
			if blank > 2 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// looksLikeZip reports whether data starts with the ZIP magic used by OOXML.
func looksLikeZip(data []byte) bool {
	return len(data) > 4 && bytes.HasPrefix(data, []byte{0x50, 0x4B})
}
