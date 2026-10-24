package store

import (
	"errors"
	"fmt"
	"time"
)

// ChatBundle is one chat with everything that belongs to it: the rows a reader needs
// to render it, continue it, search it, and read its documents again.
//
// It is the unit an archive is made of and the unit an import writes, so a chat can
// never arrive half-copied: either the row, its messages, its compactions, its tool
// calls, its attachments, its documents and its index rows all land, or none does.
//
// What it deliberately leaves out is the caches (search_cache, scrape_cache,
// digest_cache): they hold second copies of pages the chat already quotes, they are
// regenerable, and they are the bulk of a database's size. A backup that carried them
// would be mostly cache.
type ChatBundle struct {
	Chat        Chat             `json:"chat"`
	Messages    []Message        `json:"messages"`
	Compactions []Compaction     `json:"compactions"`
	ToolCalls   []ToolCallRecord `json:"toolCalls"`
	Attachments []Attachment     `json:"attachments"`
	Documents   []Document       `json:"documents"`
	Chunks      []Chunk          `json:"chunks"`
}

// LoadChatBundle gathers a chat and its rows for export.
func (s *Store) LoadChatBundle(chatID string) (ChatBundle, error) {
	chat, err := s.GetChat(chatID)
	if err != nil {
		return ChatBundle{}, fmt.Errorf("load chat %s: %w", chatID, err)
	}
	messages, err := s.ListMessages(chatID)
	if err != nil {
		return ChatBundle{}, fmt.Errorf("load messages of %s: %w", chatID, err)
	}
	// ListMessages attaches each message's attachments and tool calls because the
	// engine reads them from there. The bundle carries both lists once, at the top, so
	// the per-message copies go: keeping them would store every attachment twice and
	// let the two disagree after a hand edit.
	for i := range messages {
		messages[i].Attachments = nil
		messages[i].ToolCalls = nil
	}
	compactions, err := s.ListCompactions(chatID)
	if err != nil {
		return ChatBundle{}, fmt.Errorf("load compactions of %s: %w", chatID, err)
	}
	toolCalls, err := s.ListToolCalls(chatID)
	if err != nil {
		return ChatBundle{}, fmt.Errorf("load tool calls of %s: %w", chatID, err)
	}
	attachments, err := s.ListAttachments(chatID)
	if err != nil {
		return ChatBundle{}, fmt.Errorf("load attachments of %s: %w", chatID, err)
	}
	documents, err := s.chatDocuments(chatID)
	if err != nil {
		return ChatBundle{}, fmt.Errorf("load documents of %s: %w", chatID, err)
	}
	chunks := []Chunk{}
	for _, d := range documents {
		got, err := s.ListChunks(d.ID)
		if err != nil {
			return ChatBundle{}, fmt.Errorf("load chunks of %s: %w", d.ID, err)
		}
		chunks = append(chunks, got...)
	}
	return ChatBundle{
		Chat:        chat,
		Messages:    messages,
		Compactions: compactions,
		ToolCalls:   toolCalls,
		Attachments: attachments,
		Documents:   documents,
		Chunks:      chunks,
	}, nil
}

// chatDocuments is ListDocuments with the text included.
//
// The listing deliberately leaves `text` out — it is the whole extracted document, and
// a chat list does not need it. An archive does: it is the prose the model read, and the
// base64 bytes of every picture this app generated.
func (s *Store) chatDocuments(chatID string) ([]Document, error) {
	rows, err := s.db.Query(`SELECT id,attachment_id,chat_id,title,text,token_count,page_count,
  sha256,source_url,created_at FROM documents WHERE chat_id=? ORDER BY created_at`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		var d Document
		var created int64
		if err := rows.Scan(&d.ID, &d.AttachmentID, &d.ChatID, &d.Title, &d.Text,
			&d.TokenCount, &d.PageCount, &d.SHA256, &d.SourceURL, &created); err != nil {
			return nil, err
		}
		d.CreatedAt = fromMillis(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

// SaveChatBundle writes a chat bundle over whatever this database holds under the same
// chat id, in one transaction.
//
// It replaces rather than appends: the caller has already decided that the incoming
// copy is the one to keep, and a merge would have to renumber sequence numbers two
// machines allocated independently. Replacing is what makes an import idempotent — the
// same archive applied twice leaves the database as the archive describes it.
//
// Two values are not carried across. Attachment.SavedPath is a path on the machine the
// archive was written on, so importing it would make the pictures panel claim a file
// the user does not have. Everything else — ids, sequence numbers and timestamps —
// is preserved, because the import policy compares timestamps and because a chat's
// message ids are referenced by its compactions and tool calls.
func (s *Store) SaveChatBundle(b ChatBundle) error {
	if b.Chat.ID == "" {
		return errors.New("chat bundle has no id")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Every row of the chat that may already be here. tool_calls has no foreign key,
	// which makes an explicit delete the only thing that removes it; documents and
	// chunks hold no reference to chats either, for the reason DeleteChat gives.
	deletes := []string{
		`DELETE FROM tool_calls WHERE chat_id=?`,
		`DELETE FROM attachments WHERE chat_id=?`,
		`DELETE FROM compactions WHERE chat_id=?`,
		`DELETE FROM messages WHERE chat_id=?`,
		`DELETE FROM chunks WHERE chat_id=?`,
		`DELETE FROM documents WHERE chat_id=?`,
		`DELETE FROM chats WHERE id=?`,
	}
	if s.FTS {
		if _, err := tx.Exec(`DELETE FROM chunks_fts WHERE chat_id=?`, b.Chat.ID); err != nil {
			return fmt.Errorf("clear index of %s: %w", b.Chat.ID, err)
		}
	}
	for _, statement := range deletes {
		if _, err := tx.Exec(statement, b.Chat.ID); err != nil {
			return fmt.Errorf("clear chat %s: %w", b.Chat.ID, err)
		}
	}

	c := b.Chat
	created, updated := millis(c.CreatedAt), millis(c.UpdatedAt)
	if _, err := tx.Exec(`
INSERT INTO chats (id,title,created_at,updated_at,backend,model,system_prompt,
  system_prompt_preset,temperature,top_p,num_ctx,archived,auto_compact,
  compact_threshold,keep_recent_turns,search_first)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.Title, created, updated, c.Backend, c.Model, c.SystemPrompt,
		c.SystemPromptPreset, c.Temperature, c.TopP, c.NumCtx, boolToInt(c.Archived),
		boolToInt(c.AutoCompact), c.CompactThreshold, c.KeepRecentTurns,
		boolToInt(c.SearchFirst)); err != nil {
		return fmt.Errorf("write chat %s: %w", c.ID, err)
	}

	for _, m := range b.Messages {
		// The chat id is normalised rather than trusted: a bundle is a file that may
		// have been edited, and a row naming another chat must not be able to write
		// itself into it.
		status := m.Status
		if status == "" {
			status = StatusComplete
		}
		if _, err := tx.Exec(`
INSERT INTO messages (`+messageColumns+`)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			m.ID, c.ID, m.Seq, m.Role, m.Content, m.Reasoning, m.Name, m.ToolCallsJSON,
			m.ToolCallID, status, millis(m.CreatedAt), m.Backend, m.Model,
			m.PromptTokens, m.CompletionTokens, m.TokensPerSecond, m.TTFTMs,
			m.GenerationMs, m.StopReason, m.StatsSource, sourcesJSON(m.Sources)); err != nil {
			return fmt.Errorf("write message %s: %w", m.ID, err)
		}
	}

	for _, k := range b.Compactions {
		if _, err := tx.Exec(`
INSERT INTO compactions (id,chat_id,trigger,from_seq,to_seq,summary,focus,
  tokens_before,tokens_after,model,backend,created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			k.ID, c.ID, k.Trigger, k.FromSeq, k.ToSeq, k.Summary, k.Focus,
			k.TokensBefore, k.TokensAfter, k.Model, k.Backend, millis(k.CreatedAt)); err != nil {
			return fmt.Errorf("write compaction %s: %w", k.ID, err)
		}
	}

	for _, t := range b.ToolCalls {
		if _, err := tx.Exec(`
INSERT INTO tool_calls (id,message_id,chat_id,name,args_json,result_json,status,duration_ms,error,
  source_tokens,context_tokens,condensed,withheld,created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			t.ID, t.MessageID, c.ID, t.Name, t.ArgsJSON, t.ResultJSON, t.Status,
			t.DurationMs, t.Error, t.SourceTokens, t.ContextTokens,
			boolToInt(t.Condensed), boolToInt(t.Withheld), millis(t.CreatedAt)); err != nil {
			return fmt.Errorf("write tool call %s: %w", t.ID, err)
		}
	}

	for _, a := range b.Attachments {
		generation, err := marshalRecipe(a.Generation)
		if err != nil {
			return fmt.Errorf("recipe of %s: %w", a.Filename, err)
		}
		derivation, err := marshalDerivation(a.Derivation)
		if err != nil {
			return fmt.Errorf("derivation of %s: %w", a.Filename, err)
		}
		if _, err := tx.Exec(`
INSERT INTO attachments (id,chat_id,message_id,kind,filename,mime,size_bytes,
  sha256,pages,token_estimate,placement,document_id,warning,created_at,generation_json,
  saved_path,derivation_json)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'',?)`,
			a.ID, c.ID, a.MessageID, a.Kind, a.Filename, a.Mime, a.SizeBytes,
			a.SHA256, a.Pages, a.TokenEstimate, a.Placement, a.DocumentID,
			a.Warning, millis(a.CreatedAt), generation, derivation); err != nil {
			return fmt.Errorf("write attachment %s: %w", a.ID, err)
		}
	}

	for _, d := range b.Documents {
		if _, err := tx.Exec(`
INSERT INTO documents (id,attachment_id,chat_id,title,text,token_count,page_count,
  sha256,source_url,created_at)
VALUES (?,?,?,?,?,?,?,?,?,?)`,
			d.ID, d.AttachmentID, c.ID, d.Title, d.Text, d.TokenCount, d.PageCount,
			d.SHA256, d.SourceURL, millis(d.CreatedAt)); err != nil {
			return fmt.Errorf("write document %s: %w", d.ID, err)
		}
	}

	// Chunks are re-inserted rather than reproduced: the id is a rowid this database
	// allocated, and the incoming number belongs to the database the archive was
	// written on, where it may well name a different row.
	for _, k := range b.Chunks {
		res, err := tx.Exec(`
INSERT INTO chunks (document_id,chat_id,ordinal,heading,page,text,tokens)
VALUES (?,?,?,?,?,?,?)`,
			k.DocumentID, c.ID, k.Ordinal, k.Heading, k.Page, k.Text, k.Tokens)
		if err != nil {
			return fmt.Errorf("write chunk %d of %s: %w", k.Ordinal, k.DocumentID, err)
		}
		if !s.FTS {
			continue
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO chunks_fts(rowid,text,heading,chat_id,doc_id) VALUES (?,?,?,?,?)`,
			id, k.Text, k.Heading, c.ID, k.DocumentID); err != nil {
			return fmt.Errorf("index chunk %d of %s: %w", k.Ordinal, k.DocumentID, err)
		}
	}

	return tx.Commit()
}

// BundleSummary is the part of a bundle an import shows before it writes anything.
type BundleSummary struct {
	ChatID    string    `json:"chatId"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updatedAt"`
	Messages  int       `json:"messages"`
	// Attachments counts the files the chat carries, pictures included, so a preview
	// can say what an import costs in bytes rather than in rows.
	Attachments int   `json:"attachments"`
	Bytes       int64 `json:"bytes"`
}

// Summary describes a bundle without writing it.
func (b ChatBundle) Summary() BundleSummary {
	var bytes int64
	for _, d := range b.Documents {
		bytes += int64(len(d.Text))
	}
	return BundleSummary{
		ChatID:      b.Chat.ID,
		Title:       b.Chat.Title,
		UpdatedAt:   b.Chat.UpdatedAt,
		Messages:    len(b.Messages),
		Attachments: len(b.Attachments),
		Bytes:       bytes,
	}
}
