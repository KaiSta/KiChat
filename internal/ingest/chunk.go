package ingest

import (
	"regexp"
	"strconv"
	"strings"

	"kichat/internal/llm"
)

// ChunkSpec controls how documents are split for indexing.
type ChunkSpec struct {
	// TargetTokens is the approximate size of a chunk.
	TargetTokens int
	// OverlapRatio is how much of the previous chunk is repeated in the next,
	// so a fact spanning a boundary is still retrievable.
	OverlapRatio float64
}

// DefaultChunkSpec is the shipped configuration.
func DefaultChunkSpec() ChunkSpec {
	return ChunkSpec{TargetTokens: 800, OverlapRatio: 0.15}
}

// ChunkOut is one indexed slice.
type ChunkOut struct {
	Ordinal int
	Heading string
	Page    int
	Text    string
	Tokens  int
}

var (
	// A page marker is the first line of the page it labels, not a paragraph of
	// its own: the PDF extractor writes "## Seite 3\n<page text>". Matching it as a
	// whole paragraph therefore never fired on real output, and every chunk of
	// every PDF came back with page 0. The marker may also be alone, which is how
	// hand-written Markdown writes it, so both forms have to match.
	pageMarker = regexp.MustCompile(`^##[ \t]+Seite[ \t]+(\d+)[ \t]*(?:\r?\n|$)`)
	// A heading is only one at the start of a block; whatever follows on the next
	// line is that heading's body, not part of its text.
	headingMarker = regexp.MustCompile(`^(#{1,6})[ \t]+([^\n]+)`)
	// sentenceSplitter keeps the terminating punctuation with its sentence.
	sentenceSplitter = regexp.MustCompile(`[^.!?]+[.!?]*\s*`)
)

// ChunkDocument splits a document into overlapping, heading-aware chunks.
func ChunkDocument(text string, spec ChunkSpec) []ChunkOut {
	if spec.TargetTokens <= 0 {
		spec.TargetTokens = 800
	}
	if spec.OverlapRatio <= 0 || spec.OverlapRatio >= 0.9 {
		spec.OverlapRatio = 0.15
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	type block struct {
		text    string
		heading string
		page    int
	}

	// Parse the document into blocks, tracking the heading and page in force.
	var blocks []block
	currentHeading := ""
	currentPage := 0
	for _, raw := range strings.Split(text, "\n\n") {
		part := strings.TrimSpace(raw)
		if part == "" {
			continue
		}
		// A page marker applies to everything after it, including its own block.
		afterMarker := part
		if m := pageMarker.FindStringSubmatch(part); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				currentPage = n
			}
			afterMarker = part[len(m[0]):]
			if strings.TrimSpace(afterMarker) == "" {
				// The marker stood alone: it labels the blocks that follow it.
				continue
			}
		}
		if m := headingMarker.FindStringSubmatch(afterMarker); m != nil {
			currentHeading = strings.TrimSpace(m[2])
		}
		blocks = append(blocks, block{text: part, heading: currentHeading, page: currentPage})
	}
	if len(blocks) == 0 {
		return nil
	}

	var chunks []ChunkOut
	var (
		current   []string
		tokens    int
		heading   = blocks[0].heading
		page      = blocks[0].page
		ordinal   int
		lastChunk []string
	)

	flush := func() {
		if len(current) == 0 {
			return
		}
		body := strings.Join(current, "\n\n")
		chunks = append(chunks, ChunkOut{
			Ordinal: ordinal,
			Heading: heading,
			Page:    page,
			Text:    body,
			Tokens:  llm.EstimateTokens(body),
		})
		ordinal++
		lastChunk = current
	}

	for _, b := range blocks {
		blockTokens := llm.EstimateTokens(b.text)

		// A single oversized block is split at sentence boundaries so a chunk is
		// never bigger than the search budget allows.
		if blockTokens > spec.TargetTokens*2 {
			for _, piece := range splitLongText(b.text, spec.TargetTokens) {
				pieceTokens := llm.EstimateTokens(piece)
				if tokens+pieceTokens > spec.TargetTokens && len(current) > 0 {
					flush()
					current = overlapTail(lastChunk, spec.OverlapRatio, spec.TargetTokens)
					tokens = llm.EstimateTokens(strings.Join(current, "\n\n"))
					heading, page = b.heading, b.page
				}
				current = append(current, piece)
				tokens += pieceTokens
			}
			continue
		}

		if tokens+blockTokens > spec.TargetTokens && len(current) > 0 {
			flush()
			// Start the next chunk with an overlapping tail.
			current = overlapTail(lastChunk, spec.OverlapRatio, spec.TargetTokens)
			tokens = llm.EstimateTokens(strings.Join(current, "\n\n"))
			heading, page = b.heading, b.page
		} else if len(current) == 0 {
			heading, page = b.heading, b.page
		}
		current = append(current, b.text)
		tokens += blockTokens
	}
	flush()
	return chunks
}

// overlapTail returns the trailing blocks worth repeating, up to the overlap
// budget.
func overlapTail(blocks []string, ratio float64, target int) []string {
	budget := int(float64(target) * ratio)
	if budget <= 0 || len(blocks) == 0 {
		return nil
	}
	var (
		out    []string
		tokens int
	)
	for i := len(blocks) - 1; i >= 0; i-- {
		t := llm.EstimateTokens(blocks[i])
		if tokens+t > budget {
			break
		}
		out = append([]string{blocks[i]}, out...)
		tokens += t
	}
	return out
}

// splitLongText breaks a block that exceeds the target into sentence-aligned
// pieces.
func splitLongText(text string, target int) []string {
	if llm.EstimateTokens(text) <= target {
		return []string{text}
	}
	sentences := splitSentences(text)
	var (
		out     []string
		current []string
		tokens  int
	)
	for _, s := range sentences {
		t := llm.EstimateTokens(s)
		if tokens+t > target && len(current) > 0 {
			out = append(out, strings.Join(current, " "))
			current = nil
			tokens = 0
		}
		current = append(current, s)
		tokens += t
	}
	if len(current) > 0 {
		out = append(out, strings.Join(current, " "))
	}
	return out
}

func splitSentences(text string) []string {
	parts := sentenceSplitter.FindAllString(text, -1)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return []string{text}
	}
	return out
}
