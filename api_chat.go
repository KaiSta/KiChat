package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"kichat/internal/chat"
	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/ingest"
	"kichat/internal/research"
	"kichat/internal/store"
	"kichat/internal/uimsg"
)

// CompactRequest is the manual compaction payload.
type CompactRequest struct {
	ChatID  string  `json:"chatId"`
	Focus   string  `json:"focus"`
	Target  float64 `json:"target"`
	FromSeq int     `json:"fromSeq"`
	ToSeq   int     `json:"toSeq"`
}

// CompactNow runs a manual compaction. It returns what the run did rather than
// only an error, because "there was nothing to fold" is a normal answer to a
// manual request and the UI has to be able to say so.
func (a *App) CompactNow(req CompactRequest) (chat.CompactResult, error) {
	if err := a.ensure(); err != nil {
		return chat.CompactResult{}, err
	}
	// A compaction reads a large transcript and writes a summary; on a local model
	// running on CPU that is minutes of work, and five of them was not enough — the
	// run was still producing tokens when the deadline cut it off, reported as
	// "context deadline exceeded". The output is bounded (see compactWith), so this
	// is a backstop against a stuck backend, not a budget the work has to fit in.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	result, err := a.currentEngine().Compact(ctx, req.ChatID, chat.CompactOptions{
		Force:   true,
		Focus:   req.Focus,
		Target:  req.Target,
		FromSeq: req.FromSeq,
		ToSeq:   req.ToSeq,
	})
	if err != nil {
		return chat.CompactResult{}, err
	}
	if result.Created {
		a.emit(events.ChatsChanged, map[string]any{})
	}
	return result, nil
}

// ListCompactions returns a chat's compaction history.
func (a *App) ListCompactions(chatID string) ([]store.Compaction, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	return a.store.ListCompactions(chatID)
}

// DeleteCompaction removes a compaction, restoring the folded messages to the
// live context. The original messages were never deleted, so this is complete.
//
// Any compaction that was summarised on top of this one is dropped with it; see
// DeleteCompactionsFrom for why that is not optional. The measurement goes too,
// because the prompt has changed shape and the old count no longer describes it.
func (a *App) DeleteCompaction(chatID, compactionID string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	compactions, err := a.store.ListCompactions(chatID)
	if err != nil {
		return err
	}
	target, found := 0, false
	for _, c := range compactions {
		if c.ID == compactionID {
			target, found = c.ToSeq, true
			break
		}
	}
	if !found {
		return uimsg.Errorf("compaction.gone", compactionID)
	}
	if err := a.store.DeleteCompactionsFrom(chatID, target); err != nil {
		return err
	}
	a.currentEngine().InvalidateMeasurement(chatID)
	return nil
}

// --- attachments and documents --------------------------------------------

// AttachmentInput describes a file to add.
type AttachmentInput struct {
	ChatID    string `json:"chatId"`
	Path      string `json:"path"`
	Placement string `json:"placement"`
}

// AutoIndexThreshold is the token count above which an attachment is indexed
// rather than inlined.
const AutoIndexThreshold = 8000

// AddAttachment reads a file, extracts its text, and attaches it to a chat.
func (a *App) AddAttachment(in AttachmentInput) (store.Attachment, error) {
	if err := a.ensure(); err != nil {
		return store.Attachment{}, err
	}
	if strings.TrimSpace(in.Path) == "" {
		return store.Attachment{}, uimsg.Errorf("attachment.noPath")
	}
	info, err := os.Stat(in.Path)
	if err != nil {
		return store.Attachment{}, uimsg.Errorf("attachment.unreadable", err)
	}
	if info.IsDir() {
		return store.Attachment{}, uimsg.Errorf("attachment.folderNotSupported")
	}
	const maxSize = 64 << 20
	if info.Size() > maxSize {
		return store.Attachment{}, uimsg.Errorf("attachment.tooLarge",
			info.Size()>>20, int64(maxSize)>>20)
	}
	data, err := os.ReadFile(in.Path)
	if err != nil {
		return store.Attachment{}, uimsg.Errorf("attachment.unreadable", err)
	}

	result, err := a.extractor.Ingest(filepath.Base(in.Path), "", data)
	if err != nil {
		return store.Attachment{}, err
	}

	placement := in.Placement
	if placement == "" {
		if result.Tokens > AutoIndexThreshold {
			placement = store.PlacementIndexed
		} else {
			placement = store.PlacementInline
		}
	}

	attachment := store.Attachment{
		ID:            newID(),
		ChatID:        in.ChatID,
		Kind:          string(result.Kind),
		Filename:      filepath.Base(in.Path),
		Mime:          result.Mime,
		SizeBytes:     info.Size(),
		Pages:         result.Pages,
		TokenEstimate: result.Tokens,
		Placement:     placement,
		CreatedAt:     time.Now(),
	}
	if len(result.Warnings) > 0 {
		attachment.Warning = strings.Join(result.Warnings, " ")
	}

	// Images carry their payload rather than text, so a vision model can receive
	// them; everything else stores the extracted text.
	documentText := result.Text
	if result.Kind == ingest.KindImage {
		documentText = base64.StdEncoding.EncodeToString(data)
		attachment.TokenEstimate = 1024
	}

	document := store.Document{
		ID:           newID(),
		AttachmentID: attachment.ID,
		ChatID:       in.ChatID,
		Title:        filepath.Base(in.Path),
		Text:         documentText,
		TokenCount:   attachment.TokenEstimate,
		PageCount:    result.Pages,
		CreatedAt:    time.Now(),
	}
	if err := a.store.CreateDocument(document); err != nil {
		return store.Attachment{}, err
	}
	attachment.DocumentID = document.ID

	if placement == store.PlacementIndexed && result.Kind != ingest.KindImage {
		chunks := ingest.ChunkDocument(result.Text, ingest.DefaultChunkSpec())
		out := make([]store.Chunk, 0, len(chunks))
		for _, c := range chunks {
			out = append(out, store.Chunk{
				DocumentID: document.ID,
				ChatID:     in.ChatID,
				Ordinal:    c.Ordinal,
				Heading:    c.Heading,
				Page:       c.Page,
				Text:       c.Text,
				Tokens:     c.Tokens,
			})
		}
		if err := a.store.InsertChunks(out); err != nil {
			// Indexing is what makes the attachment reachable through
			// search_documents. Staying "indexed" after it failed would leave the
			// text in the database but invisible to the tool, so the placement falls
			// back to inline: the content still reaches the model, and the warning
			// says why it is not searchable.
			placement = store.PlacementInline
			attachment.Placement = placement
			attachment.Warning = strings.TrimSpace(attachment.Warning +
				" Indizierung fehlgeschlagen: " + err.Error())
		}
	}

	if err := a.store.CreateAttachment(attachment); err != nil {
		// The document — the whole extracted text, and the parent of its chunks — is
		// written first because the attachment references it. Losing the attachment
		// must not leave that text behind with nothing pointing at it: it is the
		// largest row in the database, and DeleteChat and DeleteAttachment both
		// exist to stop exactly this leak.
		if cleanupErr := a.store.DeleteDocument(document.ID); cleanupErr != nil {
			return store.Attachment{}, uimsg.Errorf("attachment.cleanupFailed",
				err, document.ID, cleanupErr)
		}
		return store.Attachment{}, err
	}
	a.emit(events.Attachments, map[string]any{"chatId": in.ChatID})
	return attachment, nil
}

// RemoveAttachment deletes an attachment and its extracted text.
func (a *App) RemoveAttachment(attachmentID string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	attachment, err := a.store.GetAttachment(attachmentID)
	if err != nil {
		return err
	}
	if err := a.store.DeleteAttachment(attachmentID); err != nil {
		return err
	}
	a.emit(events.Attachments, map[string]any{"chatId": attachment.ChatID})
	return nil
}

// SetAttachmentPlacement switches an attachment between inline and indexed.
func (a *App) SetAttachmentPlacement(attachmentID, placement string) (store.Attachment, error) {
	if err := a.ensure(); err != nil {
		return store.Attachment{}, err
	}
	attachment, err := a.store.GetAttachment(attachmentID)
	if err != nil {
		return store.Attachment{}, err
	}
	// An image's document text is the raw base64 of the file, so indexing it would
	// fill the search index with encoded bytes. AddAttachment never indexes one;
	// the switch has to refuse it for the same reason.
	if placement == store.PlacementIndexed && attachment.Kind == string(ingest.KindImage) {
		return store.Attachment{}, uimsg.Errorf("attachment.imageNotIndexed")
	}
	attachment.Placement = placement
	if placement == store.PlacementIndexed {
		document, err := a.store.GetDocument(attachment.DocumentID)
		if err != nil {
			// Marking the attachment indexed without its text would leave it
			// unreachable through search_documents while the UI showed it as
			// searchable.
			return store.Attachment{}, uimsg.Errorf("attachment.documentRead", err)
		}
		existing, _ := a.store.ListChunks(document.ID)
		if len(existing) == 0 {
			chunks := ingest.ChunkDocument(document.Text, ingest.DefaultChunkSpec())
			out := make([]store.Chunk, 0, len(chunks))
			for _, c := range chunks {
				out = append(out, store.Chunk{
					DocumentID: document.ID, ChatID: attachment.ChatID,
					Ordinal: c.Ordinal, Heading: c.Heading, Page: c.Page,
					Text: c.Text, Tokens: c.Tokens,
				})
			}
			if err := a.store.InsertChunks(out); err != nil {
				return store.Attachment{}, err
			}
		}
	}
	if err := a.store.UpdateAttachment(attachment); err != nil {
		return store.Attachment{}, err
	}
	a.emit(events.Attachments, map[string]any{"chatId": attachment.ChatID})
	return attachment, nil
}

// --- image generation -------------------------------------------------------

// PickFiles opens a native file dialog and returns the chosen paths.
func (a *App) PickFiles() ([]string, error) {
	if a.ctx == nil {
		return nil, uimsg.Errorf("app.notReady")
	}
	return runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Dateien zum Chat hinzufügen",
	})
}

// GetDocument returns a document including its full text.
func (a *App) GetDocument(documentID string) (store.Document, error) {
	if err := a.ensure(); err != nil {
		return store.Document{}, err
	}
	return a.store.GetDocument(documentID)
}

// ListChunks returns the indexed chunks of a document.
func (a *App) ListChunks(documentID string) ([]store.Chunk, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	return a.store.ListChunks(documentID)
}

// --- search and scraping ---------------------------------------------------

// Search runs a live web search, bypassing nothing but the provider chain.
func (a *App) Search(query string) (research.SearchOutcome, error) {
	if err := a.ensure(); err != nil {
		return research.SearchOutcome{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	outcome, err := a.currentChain().Search(ctx, query, research.SearchRequest{})
	if err != nil {
		return research.SearchOutcome{}, err
	}
	return outcome, nil
}

// Scrape fetches a URL and returns clean Markdown.
func (a *App) Scrape(url string, maxTokens int) (research.ScrapeResult, error) {
	if err := a.ensure(); err != nil {
		return research.ScrapeResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if maxTokens <= 0 {
		maxTokens = 12000
	}
	return a.currentScraper().Scrape(ctx, url, maxTokens)
}

// ScrapeIntoChat scrapes a page and attaches it to a chat as an indexed or
// inline document, which is how "read this page" becomes permanent context.
//
// The page is fetched whole and then either inlined or indexed by size, the rule
// AddAttachment follows. It used to be trimmed to maxTokens and always inlined,
// which turned a 56-page manual into its first pages with no way to read the rest.
func (a *App) ScrapeIntoChat(chatID, pageURL string, maxTokens int) (store.Attachment, error) {
	if err := a.ensure(); err != nil {
		return store.Attachment{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if maxTokens <= 0 {
		maxTokens = 12000
	}
	result, err := a.currentScraper().ScrapeDocument(ctx, pageURL)
	if err != nil {
		return store.Attachment{}, err
	}

	placement := store.PlacementInline
	if result.Tokens > AutoIndexThreshold {
		placement = store.PlacementIndexed
	} else {
		// Small enough to send whole; the budget only bites above the threshold,
		// where the text is indexed instead.
		research.TrimToTokens(&result, maxTokens, config.ModelLanguage(a.Settings().Language))
	}

	title := result.Title
	if title == "" {
		title = result.FinalURL
	}
	kind, mime := "web", "text/markdown"
	if strings.Contains(result.ContentType, "application/pdf") || result.Pages > 0 {
		kind, mime = "pdf", "application/pdf"
	}
	attachment := store.Attachment{
		ID:            newID(),
		ChatID:        chatID,
		Kind:          kind,
		Filename:      title,
		Mime:          mime,
		SizeBytes:     int64(len(result.Markdown)),
		Pages:         result.Pages,
		TokenEstimate: result.Tokens,
		Placement:     placement,
		CreatedAt:     time.Now(),
	}
	if result.Warning != "" {
		attachment.Warning = result.Warning
	}
	document := store.Document{
		ID:           newID(),
		AttachmentID: attachment.ID,
		ChatID:       chatID,
		Title:        title,
		Text:         result.Markdown,
		TokenCount:   result.Tokens,
		PageCount:    result.Pages,
		SourceURL:    result.FinalURL,
		CreatedAt:    time.Now(),
	}
	if err := a.store.CreateDocument(document); err != nil {
		return store.Attachment{}, err
	}
	attachment.DocumentID = document.ID
	if placement == store.PlacementIndexed {
		chunks := ingest.ChunkDocument(result.Markdown, ingest.DefaultChunkSpec())
		out := make([]store.Chunk, 0, len(chunks))
		for _, c := range chunks {
			out = append(out, store.Chunk{
				DocumentID: document.ID,
				ChatID:     chatID,
				Ordinal:    c.Ordinal,
				Heading:    c.Heading,
				Page:       c.Page,
				Text:       c.Text,
				Tokens:     c.Tokens,
			})
		}
		if err := a.store.InsertChunks(out); err != nil {
			if cleanupErr := a.store.DeleteDocument(document.ID); cleanupErr != nil {
				return store.Attachment{}, uimsg.Errorf("attachment.cleanupFailed",
					err, document.ID, cleanupErr)
			}
			return store.Attachment{}, err
		}
	}
	if err := a.store.CreateAttachment(attachment); err != nil {
		if cleanupErr := a.store.DeleteDocument(document.ID); cleanupErr != nil {
			return store.Attachment{}, uimsg.Errorf("attachment.cleanupFailed",
				err, document.ID, cleanupErr)
		}
		return store.Attachment{}, err
	}
	a.emit(events.Attachments, map[string]any{"chatId": chatID})
	return attachment, nil
}
