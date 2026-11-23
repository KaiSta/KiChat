package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// --- chats -----------------------------------------------------------------

// CreateChat inserts a new chat.
func (s *Store) CreateChat(c Chat) error {
	now := time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = now
	}
	_, err := s.db.Exec(`
INSERT INTO chats (id,title,created_at,updated_at,backend,model,system_prompt,
  system_prompt_preset,temperature,top_p,num_ctx,archived,auto_compact,
  compact_threshold,keep_recent_turns,search_first,mode,workspace)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.Title, millis(c.CreatedAt), millis(c.UpdatedAt), c.Backend, c.Model,
		c.SystemPrompt, c.SystemPromptPreset, c.Temperature, c.TopP, c.NumCtx,
		boolToInt(c.Archived), boolToInt(c.AutoCompact), c.CompactThreshold,
		c.KeepRecentTurns, boolToInt(c.SearchFirst), NormaliseMode(c.Mode), c.Workspace)
	return err
}

// UpdateChat persists mutable chat fields.
func (s *Store) UpdateChat(c Chat) error {
	c.UpdatedAt = time.Now()
	res, err := s.db.Exec(`
UPDATE chats SET title=?,updated_at=?,backend=?,model=?,system_prompt=?,
  system_prompt_preset=?,temperature=?,top_p=?,num_ctx=?,archived=?,auto_compact=?,
  compact_threshold=?,keep_recent_turns=?,search_first=?,mode=?,workspace=? WHERE id=?`,
		c.Title, millis(c.UpdatedAt), c.Backend, c.Model, c.SystemPrompt,
		c.SystemPromptPreset, c.Temperature, c.TopP, c.NumCtx, boolToInt(c.Archived),
		boolToInt(c.AutoCompact), c.CompactThreshold, c.KeepRecentTurns,
		boolToInt(c.SearchFirst), NormaliseMode(c.Mode), c.Workspace, c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetChatTitle renames a chat without touching the rest of its settings.
func (s *Store) SetChatTitle(id, title string) error {
	_, err := s.db.Exec(`UPDATE chats SET title=?, updated_at=? WHERE id=?`, title, millis(time.Now()), id)
	return err
}

// DeleteChat removes a chat and everything belonging to it.
//
// The chat row carries messages, compactions and attachments away by cascade,
// but documents and chunks hold no foreign key to chats, so they are deleted
// here by hand. They are the two largest tables in this database — a document is
// the whole extracted text, a chunk a slice of it — and leaving them behind made
// every deleted chat a permanent leak: measured, a deleted chat kept its
// document and its chunk rows while dropping everything else. The transaction is
// what keeps a half-deleted chat out of reach: either the lot goes or nothing does.
func (s *Store) DeleteChat(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if s.FTS {
		if _, err := tx.Exec(`DELETE FROM chunks_fts WHERE chat_id = ?`, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM chunks WHERE chat_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM documents WHERE chat_id = ?`, id); err != nil {
		return err
	}
	// file_snapshots holds no foreign key to chats either, so the undo records of a deleted
	// chat are deleted by hand here for the same reason documents are.
	if _, err := tx.Exec(`DELETE FROM file_snapshots WHERE chat_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM chats WHERE id=?`, id); err != nil {
		return err
	}
	return s.commitAndCheckpoint(tx)
}

// ClearChat empties a chat's conversation and keeps the chat: the folder, the prompt, the
// model, the sampler values and the title all stay, and the next turn begins the way the
// first one did.
//
// It exists because a **project is one conversation**. A chat can be answered by creating
// another one, but a second project on the same folder would be a second copy of the same
// settings that then drifts from the first, so starting over has to happen *here*.
//
// Everything the conversation accumulated goes, in one transaction, and each table is
// named for a reason:
//
//   - `messages` and their `tool_calls`. The call table holds no foreign key to messages
//     (a call outlives the message row while a turn is in flight), so a row left behind
//     would be an orphan nothing in the window could show.
//   - `compactions`. They are summaries *of* the messages being deleted; kept, they would
//     be sent as the conversation's memory on the next turn, describing a history that no
//     longer exists — the same double-rendering DeleteCompactionsFrom exists to prevent.
//   - `attachments`, with their `documents` and `chunks`. This is the one that would
//     otherwise be forgotten, and the symptom would be quiet rather than visible: an
//     attachment's extracted text is retrievable through `search_documents`, so a chat
//     that had "cleared" its context could still answer a question from the file it was
//     cleared of. The document and chunk tables hold no foreign key to chats and are
//     deleted by hand here, for the reason DeleteChat does it.
//
// The chat's `updated_at` is touched because the conversation is what the list orders on,
// and a chat that was just emptied is the one the user last worked in.
func (s *Store) ClearChat(chatID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// The index first, and by hand: chunks_fts is a virtual table, so the cascade from
	// documents does not reach its rows.
	if s.FTS {
		if _, err := tx.Exec(`DELETE FROM chunks_fts WHERE chat_id = ?`, chatID); err != nil {
			return err
		}
	}
	for _, statement := range []string{
		`DELETE FROM chunks WHERE chat_id = ?`,
		`DELETE FROM documents WHERE chat_id = ?`,
		`DELETE FROM attachments WHERE chat_id = ?`,
		`DELETE FROM tool_calls WHERE chat_id = ?`,
		// The undo records go with the history they describe: a chat that was emptied can no
		// longer say which change the record belongs to, and a revert offered from it would be
		// a button whose label nobody could check.
		`DELETE FROM file_snapshots WHERE chat_id = ?`,
		`DELETE FROM compactions WHERE chat_id = ?`,
		`DELETE FROM messages WHERE chat_id = ?`,
	} {
		if _, err := tx.Exec(statement, chatID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE chats SET updated_at=? WHERE id=?`, millis(time.Now()), chatID); err != nil {
		return err
	}
	return s.commitAndCheckpoint(tx)
}

const chatColumns = `id,title,created_at,updated_at,backend,model,system_prompt,
  system_prompt_preset,temperature,top_p,num_ctx,archived,auto_compact,
  compact_threshold,keep_recent_turns,search_first,mode,workspace`

func scanChat(row interface{ Scan(...any) error }) (Chat, error) {
	var c Chat
	var created, updated int64
	var archived, autoCompact, searchFirst int
	err := row.Scan(&c.ID, &c.Title, &created, &updated, &c.Backend, &c.Model,
		&c.SystemPrompt, &c.SystemPromptPreset, &c.Temperature, &c.TopP, &c.NumCtx,
		&archived, &autoCompact, &c.CompactThreshold, &c.KeepRecentTurns, &searchFirst,
		&c.Mode, &c.Workspace)
	if err != nil {
		return Chat{}, err
	}
	c.CreatedAt = fromMillis(created)
	c.UpdatedAt = fromMillis(updated)
	c.Archived = archived != 0
	c.AutoCompact = autoCompact != 0
	c.SearchFirst = searchFirst != 0
	// A row from a newer build, or one edited by hand, is read as the mode this build
	// knows rather than left as a value that would reach a switch with no case for it.
	c.Mode = NormaliseMode(c.Mode)
	return c, nil
}

// GetChat loads one chat.
func (s *Store) GetChat(id string) (Chat, error) {
	return scanChat(s.db.QueryRow(`SELECT `+chatColumns+` FROM chats WHERE id=?`, id))
}

// ListChats returns chats ordered by most recently updated.
func (s *Store) ListChats(includeArchived bool) ([]Chat, error) {
	q := `SELECT ` + chatColumns + ` FROM chats`
	if !includeArchived {
		q += ` WHERE archived=0`
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- messages --------------------------------------------------------------

const messageColumns = `id,chat_id,seq,role,content,reasoning,name,tool_calls_json,
  tool_call_id,status,created_at,backend,model,prompt_tokens,completion_tokens,
  tokens_per_second,ttft_ms,generation_ms,stop_reason,stats_source,cache_hit_tokens,
  sources_json`

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var created int64
	var sources string
	err := row.Scan(&m.ID, &m.ChatID, &m.Seq, &m.Role, &m.Content, &m.Reasoning,
		&m.Name, &m.ToolCallsJSON, &m.ToolCallID, &m.Status, &created, &m.Backend,
		&m.Model, &m.PromptTokens, &m.CompletionTokens, &m.TokensPerSecond,
		&m.TTFTMs, &m.GenerationMs, &m.StopReason, &m.StatsSource, &m.CacheHitTokens,
		&sources)
	if err != nil {
		return Message{}, err
	}
	m.CreatedAt = fromMillis(created)
	m.Sources = parseSources(sources)
	return m, nil
}

// sourcesJSON is how a message's sources are stored: one small JSON array, empty when
// there are none — which is every message but a researched answer.
func sourcesJSON(sources []Source) string {
	if len(sources) == 0 {
		return ""
	}
	raw, err := json.Marshal(sources)
	if err != nil {
		return ""
	}
	return string(raw)
}

// parseSources reads that array back. A row that cannot be parsed reports no sources
// rather than failing the message: a citation list is a display aid, and a chat that
// cannot be opened is worse than one whose list is missing.
func parseSources(raw string) []Source {
	if raw == "" {
		return nil
	}
	var sources []Source
	if err := json.Unmarshal([]byte(raw), &sources); err != nil {
		return nil
	}
	return sources
}

// CreateMessage inserts a message at the sequence number the caller assigned.
// Use AppendMessage when the number should be allocated instead.
func (s *Store) CreateMessage(m Message) error {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	if m.Status == "" {
		m.Status = StatusComplete
	}
	_, err := s.db.Exec(`
INSERT INTO messages (`+messageColumns+`)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.ChatID, m.Seq, m.Role, m.Content, m.Reasoning, m.Name,
		m.ToolCallsJSON, m.ToolCallID, m.Status, millis(m.CreatedAt), m.Backend,
		m.Model, m.PromptTokens, m.CompletionTokens, m.TokensPerSecond, m.TTFTMs,
		m.GenerationMs, m.StopReason, m.StatsSource, m.CacheHitTokens, sourcesJSON(m.Sources))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE chats SET updated_at=? WHERE id=?`, millis(time.Now()), m.ChatID)
	return err
}

// AppendMessage inserts a message at the next free sequence number of its chat
// and returns it with Seq filled in.
//
// The sequence number is read and written inside one transaction. Reading
// MAX(seq) and inserting it as two steps lets two writers compute the same
// number: with a single pooled connection the transaction holds that connection
// for its whole length, so nothing else can slip between the read and the write.
// A duplicate would silently reorder a conversation, and the engine relies on
// Seq to cut a fold at a turn boundary.
//
// The chat's updated_at is touched in the same transaction, which is also why a
// failure there is reported: a message stored without it would leave the chat
// list stale, and reporting the failure is the only way the caller can say so.
func (s *Store) AppendMessage(m Message) (Message, error) {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	if m.Status == "" {
		m.Status = StatusComplete
	}
	tx, err := s.db.Begin()
	if err != nil {
		return m, err
	}
	defer tx.Rollback()

	var max sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM messages WHERE chat_id=?`, m.ChatID).Scan(&max); err != nil {
		return m, err
	}
	m.Seq = 1
	if max.Valid {
		m.Seq = int(max.Int64) + 1
	}

	if _, err := tx.Exec(`
INSERT INTO messages (`+messageColumns+`)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.ChatID, m.Seq, m.Role, m.Content, m.Reasoning, m.Name,
		m.ToolCallsJSON, m.ToolCallID, m.Status, millis(m.CreatedAt), m.Backend,
		m.Model, m.PromptTokens, m.CompletionTokens, m.TokensPerSecond, m.TTFTMs,
		m.GenerationMs, m.StopReason, m.StatsSource, m.CacheHitTokens, sourcesJSON(m.Sources)); err != nil {
		return m, err
	}
	if _, err := tx.Exec(`UPDATE chats SET updated_at=? WHERE id=?`, millis(time.Now()), m.ChatID); err != nil {
		return m, err
	}
	if err := tx.Commit(); err != nil {
		return m, err
	}
	return m, nil
}

// UpdateMessage persists the mutable parts of a message — used while streaming
// and when a generation is interrupted.
func (s *Store) UpdateMessage(m Message) error {
	_, err := s.db.Exec(`
UPDATE messages SET content=?,reasoning=?,tool_calls_json=?,status=?,
  prompt_tokens=?,completion_tokens=?,tokens_per_second=?,ttft_ms=?,
  generation_ms=?,stop_reason=?,stats_source=?,backend=?,model=?,
  cache_hit_tokens=? WHERE id=?`,
		m.Content, m.Reasoning, m.ToolCallsJSON, m.Status, m.PromptTokens,
		m.CompletionTokens, m.TokensPerSecond, m.TTFTMs, m.GenerationMs,
		m.StopReason, m.StatsSource, m.Backend, m.Model, m.CacheHitTokens, m.ID)
	return err
}

// SetMessageSources stores the numbered sources an answer cites.
//
// Its own statement rather than a field on UpdateMessage, which is the streaming write:
// the sources are not known while the answer is being written, and every streamed update
// would otherwise erase the list with the empty one it carries.
func (s *Store) SetMessageSources(messageID string, sources []Source) error {
	_, err := s.db.Exec(`UPDATE messages SET sources_json=? WHERE id=?`,
		sourcesJSON(sources), messageID)
	return err
}

// GetMessage loads one message.
func (s *Store) GetMessage(id string) (Message, error) {
	return scanMessage(s.db.QueryRow(`SELECT `+messageColumns+` FROM messages WHERE id=?`, id))
}

// ListMessages returns every message of a chat in order, including attachments
// and tool-call records, so the engine never needs extra round trips.
func (s *Store) ListMessages(chatID string) ([]Message, error) {
	rows, err := s.db.Query(`SELECT `+messageColumns+` FROM messages WHERE chat_id=? ORDER BY seq`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	index := map[string]int{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		index[m.ID] = len(out)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	attachments, err := s.ListAttachments(chatID)
	if err != nil {
		return nil, err
	}
	for _, a := range attachments {
		if i, ok := index[a.MessageID]; ok {
			out[i].Attachments = append(out[i].Attachments, a)
		}
	}
	toolCalls, err := s.ListToolCalls(chatID)
	if err != nil {
		return nil, err
	}
	for _, tc := range toolCalls {
		if i, ok := index[tc.MessageID]; ok {
			out[i].ToolCalls = append(out[i].ToolCalls, tc)
		}
	}
	return out, nil
}

// DeleteMessagesFromSeq removes a chat's tail starting at seq (inclusive).
//
// Everything runs in one transaction. The tail is a message row plus its tool
// calls, its attachments and those attachments' documents — and the caller is a
// regenerate, which is a normal user action, so a failure part-way through used to
// leave a chat whose tail had been half deleted: some messages gone, their
// documents and index rows still there and unreachable.
func (s *Store) DeleteMessagesFromSeq(chatID string, seq int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id FROM messages WHERE chat_id=? AND seq>=?`, chatID, seq)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM tool_calls WHERE message_id=?`, id); err != nil {
			return err
		}
		// Each attachment owns a document, which owns its chunks and index rows.
		// The document has to go with the attachment: deleting only the row would
		// leave the extracted text unreachable, the same leak DeleteChat fixed.
		// The ids are collected before deleting because an open result set and a
		// write cannot share the transaction's one connection.
		docRows, err := tx.Query(`SELECT document_id FROM attachments WHERE message_id=?`, id)
		if err != nil {
			return err
		}
		var docIDs []string
		for docRows.Next() {
			var docID string
			if err := docRows.Scan(&docID); err != nil {
				docRows.Close()
				return err
			}
			if docID != "" {
				docIDs = append(docIDs, docID)
			}
		}
		docRows.Close()
		if err := docRows.Err(); err != nil {
			return err
		}
		for _, docID := range docIDs {
			if err := deleteDocumentTx(tx, s.FTS, docID); err != nil {
				return fmt.Errorf("Dokument %s entfernen: %w", docID, err)
			}
		}
		if _, err := tx.Exec(`DELETE FROM attachments WHERE message_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM messages WHERE id=?`, id); err != nil {
			return err
		}
	}
	return s.commitAndCheckpoint(tx)
}

// CountMessages returns how many messages a chat has.
func (s *Store) CountMessages(chatID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id=?`, chatID).Scan(&n)
	return n, err
}

// --- attachments -----------------------------------------------------------

// CreateAttachment inserts an attachment record.
func (s *Store) CreateAttachment(a Attachment) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	generation, err := marshalRecipe(a.Generation)
	if err != nil {
		return fmt.Errorf("Rezept von %s ablegen: %w", a.Filename, err)
	}
	derivation, err := marshalDerivation(a.Derivation)
	if err != nil {
		return fmt.Errorf("Herkunft von %s ablegen: %w", a.Filename, err)
	}
	_, err = s.db.Exec(`
INSERT INTO attachments (id,chat_id,message_id,kind,filename,mime,size_bytes,
  sha256,pages,token_estimate,placement,document_id,warning,created_at,generation_json,
  derivation_json,project_path)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.ChatID, a.MessageID, a.Kind, a.Filename, a.Mime, a.SizeBytes,
		a.SHA256, a.Pages, a.TokenEstimate, a.Placement, a.DocumentID,
		a.Warning, millis(a.CreatedAt), generation, derivation, a.ProjectPath)
	return err
}

// marshalDerivation encodes an optional derivation, with the same empty-string spelling
// marshalRecipe uses: a nil pointer is "this picture was not derived from anything".
func marshalDerivation(d *Derivation) (string, error) {
	if d == nil {
		return "", nil
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// marshalRecipe encodes an optional generation recipe. An empty string is "there is
// none", which is what a nil recipe means: an attached file has no prompt, and a row
// that says nothing is better than one carrying an empty object the reader has to
// interpret.
func marshalRecipe(recipe *GenerationRecipe) (string, error) {
	if recipe == nil {
		return "", nil
	}
	raw, err := json.Marshal(recipe)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// UpdateAttachment persists mutable attachment fields.
func (s *Store) UpdateAttachment(a Attachment) error {
	_, err := s.db.Exec(`
UPDATE attachments SET message_id=?,pages=?,token_estimate=?,placement=?,
  document_id=?,warning=? WHERE id=?`,
		a.MessageID, a.Pages, a.TokenEstimate, a.Placement, a.DocumentID, a.Warning, a.ID)
	return err
}

// DeleteAttachment removes an attachment and the document it owns.
//
// The document goes first and its error is returned rather than dropped: chunks
// cascade from documents and those two tables hold the bulk of the data, so a
// silently ignored failure here leaves a document nothing can reach — the same
// leak DeleteChat had. Deleting the document first also means a failure leaves
// the attachment row in place, so the operation can simply be repeated. A row
// that is already gone is not an error: deleting twice must not report one.
//
// The two deletions are one transaction, which is what makes that "repeated"
// promise true: without it a failure between them leaves the attachment pointing
// at a document that is already gone, and repeating the call would delete the row
// while the chunks it owned stayed behind.
func (s *Store) DeleteAttachment(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var docID string
	err = tx.QueryRow(`SELECT document_id FROM attachments WHERE id=?`, id).Scan(&docID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("Anhang %s lesen: %w", id, err)
	}
	if docID != "" {
		if err := deleteDocumentTx(tx, s.FTS, docID); err != nil {
			return fmt.Errorf("Dokument %s entfernen: %w", docID, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM attachments WHERE id=?`, id); err != nil {
		return err
	}
	return s.commitAndCheckpoint(tx)
}

// PruneExpiredCaches deletes expired rows from the web caches and the condensation
// cache, and reports how many it removed.
//
// Reads already treat an expired row as a miss (GetSearchCache, GetScrapeCache,
// GetDigestCache), so this changes no result — it stops the tables from growing for
// the whole lifetime of an installation. Nothing else ever deletes from them, and a
// scrape row holds an entire extracted page, which makes them the fastest-growing
// rows in a research workflow. Open calls this, so every start trims the backlog.
func (s *Store) PruneExpiredCaches() (int64, error) {
	now := millis(time.Now())
	var removed int64
	for _, table := range []string{"search_cache", "scrape_cache", "digest_cache"} {
		res, err := s.db.Exec("DELETE FROM "+table+" WHERE expires_at < ?", now)
		if err != nil {
			return removed, fmt.Errorf("%s aufräumen: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return removed, err
		}
		removed += n
	}
	return removed, nil
}

const attachmentColumns = `id,chat_id,message_id,kind,filename,mime,size_bytes,
  sha256,pages,token_estimate,placement,document_id,warning,created_at,generation_json,
  saved_path,derivation_json,project_path`

func scanAttachment(row interface{ Scan(...any) error }) (Attachment, error) {
	var a Attachment
	var created int64
	var generation, derivation string
	err := row.Scan(&a.ID, &a.ChatID, &a.MessageID, &a.Kind, &a.Filename, &a.Mime,
		&a.SizeBytes, &a.SHA256, &a.Pages, &a.TokenEstimate, &a.Placement,
		&a.DocumentID, &a.Warning, &created, &generation, &a.SavedPath, &derivation,
		&a.ProjectPath)
	if err != nil {
		return Attachment{}, err
	}
	a.CreatedAt = fromMillis(created)
	// A recipe that will not parse fails the read rather than being dropped. It is
	// JSON this app wrote, so a value that does not decode means something else edited
	// the row — and quietly treating that as "no recipe" would make the picture
	// silently un-repeatable, which is the one thing this column exists to prevent.
	// config.Load treats its own stored JSON the same way.
	if strings.TrimSpace(generation) != "" {
		var recipe GenerationRecipe
		if err := json.Unmarshal([]byte(generation), &recipe); err != nil {
			return Attachment{}, fmt.Errorf("Rezept von Anhang %s lesen: %w", a.ID, err)
		}
		a.Generation = &recipe
	}
	// A derivation is decoded by the same rule, and for the same reason: a value this
	// app wrote that no longer parses means the row was edited elsewhere, and reporting
	// the picture as "not derived" would lose the only record of where it came from.
	if strings.TrimSpace(derivation) != "" {
		var derived Derivation
		if err := json.Unmarshal([]byte(derivation), &derived); err != nil {
			return Attachment{}, fmt.Errorf("Herkunft von Anhang %s lesen: %w", a.ID, err)
		}
		a.Derivation = &derived
	}
	return a, nil
}

// GetAttachment loads one attachment.
func (s *Store) GetAttachment(id string) (Attachment, error) {
	return scanAttachment(s.db.QueryRow(`SELECT `+attachmentColumns+` FROM attachments WHERE id=?`, id))
}

// ListAttachments returns a chat's attachments oldest first.
func (s *Store) ListAttachments(chatID string) ([]Attachment, error) {
	rows, err := s.db.Query(`SELECT `+attachmentColumns+` FROM attachments WHERE chat_id=? ORDER BY created_at`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AllAttachments returns every attachment in the database, newest last.
//
// It exists for the questions that are about the store as a whole rather than about one
// chat — how much of it pictures take up, and which of them nothing outside the database
// holds a copy of. Aggregating those in SQL would mean the store knowing what an image is
// (`ingest.KindImage`), which is a fact about how the app reads a file rather than about
// how a row is stored; the caller that already knows that classification does the
// counting instead.
func (s *Store) AllAttachments() ([]Attachment, error) {
	rows, err := s.db.Query(`SELECT ` + attachmentColumns + ` FROM attachments ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAttachmentSavedPath records where an attachment was exported to.
//
// A separate statement rather than a field on UpdateAttachment: that one persists the
// fields a reader may edit (placement, warning, the document a re-read produced), and an
// export is not an edit of the attachment — it is a note that a copy now exists elsewhere.
// Passing an empty path is allowed and means the copy is gone as far as this app knows.
func (s *Store) SetAttachmentSavedPath(id, path string) error {
	_, err := s.db.Exec(`UPDATE attachments SET saved_path=? WHERE id=?`, path, id)
	return err
}

// --- documents and chunks --------------------------------------------------

// CreateDocument inserts a document.
func (s *Store) CreateDocument(d Document) error {
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now()
	}
	_, err := s.db.Exec(`
INSERT INTO documents (id,attachment_id,chat_id,title,text,token_count,page_count,sha256,source_url,created_at)
VALUES (?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.AttachmentID, d.ChatID, d.Title, d.Text, d.TokenCount,
		d.PageCount, d.SHA256, d.SourceURL, millis(d.CreatedAt))
	return err
}

// GetDocument loads a document.
func (s *Store) GetDocument(id string) (Document, error) {
	var d Document
	var created int64
	err := s.db.QueryRow(`SELECT id,attachment_id,chat_id,title,text,token_count,page_count,sha256,source_url,created_at
FROM documents WHERE id=?`, id).
		Scan(&d.ID, &d.AttachmentID, &d.ChatID, &d.Title, &d.Text, &d.TokenCount,
			&d.PageCount, &d.SHA256, &d.SourceURL, &created)
	if err != nil {
		return Document{}, err
	}
	d.CreatedAt = fromMillis(created)
	return d, nil
}

// DocumentBySource returns the newest document a chat holds for a source URL, with
// its text.
//
// It exists because a scraped page is two things at once: a tool result the model has
// just been handed, and a document the chat indexed. The result carries an excerpt, so
// the part that condenses it needs the document itself to summarise all of it. The
// newest row wins because a page scraped twice has two, and the one just written is the
// text the model just read.
func (s *Store) DocumentBySource(chatID, sourceURL string) (Document, bool, error) {
	if chatID == "" || sourceURL == "" {
		return Document{}, false, nil
	}
	var d Document
	var created int64
	err := s.db.QueryRow(`SELECT id,attachment_id,chat_id,title,text,token_count,page_count,sha256,source_url,created_at
FROM documents WHERE chat_id=? AND source_url=? ORDER BY created_at DESC, rowid DESC LIMIT 1`, chatID, sourceURL).
		Scan(&d.ID, &d.AttachmentID, &d.ChatID, &d.Title, &d.Text, &d.TokenCount,
			&d.PageCount, &d.SHA256, &d.SourceURL, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, false, nil
	}
	if err != nil {
		return Document{}, false, err
	}
	d.CreatedAt = fromMillis(created)
	return d, true, nil
}

// ListDocuments returns a chat's documents without their full text.
func (s *Store) ListDocuments(chatID string) ([]Document, error) {
	rows, err := s.db.Query(`SELECT id,attachment_id,chat_id,title,token_count,page_count,sha256,source_url,created_at
FROM documents WHERE chat_id=? ORDER BY created_at`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		var d Document
		var created int64
		if err := rows.Scan(&d.ID, &d.AttachmentID, &d.ChatID, &d.Title, &d.TokenCount,
			&d.PageCount, &d.SHA256, &d.SourceURL, &created); err != nil {
			return nil, err
		}
		d.CreatedAt = fromMillis(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDocument removes a document and its chunks and index entries.
//
// The index rows and the document row go in one transaction: the chunks cascade
// from the document, so a failure between the two would leave fts rows naming
// chunks that no longer exist while the document — and the chunks it still owns —
// stayed behind.
func (s *Store) DeleteDocument(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteDocumentTx(tx, s.FTS, id); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteDocumentTx is deleteDocument inside a caller's transaction, so a deletion
// that is part of a bigger one — a message tail, an attachment — commits or rolls
// back with it rather than on its own.
func deleteDocumentTx(tx *sql.Tx, fts bool, id string) error {
	if fts {
		if _, err := tx.Exec(`DELETE FROM chunks_fts WHERE doc_id=?`, id); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`DELETE FROM documents WHERE id=?`, id)
	return err
}

// InsertChunks stores chunks and indexes them for full-text search.
func (s *Store) InsertChunks(chunks []Chunk) error {
	if len(chunks) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range chunks {
		res, err := tx.Exec(`
INSERT INTO chunks (document_id,chat_id,ordinal,heading,page,text,tokens)
VALUES (?,?,?,?,?,?,?)`,
			c.DocumentID, c.ChatID, c.Ordinal, c.Heading, c.Page, c.Text, c.Tokens)
		if err != nil {
			return err
		}
		if !s.FTS {
			continue
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO chunks_fts(rowid,text,heading,chat_id,doc_id) VALUES (?,?,?,?,?)`,
			id, c.Text, c.Heading, c.ChatID, c.DocumentID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListChunks returns a document's chunks in order.
func (s *Store) ListChunks(documentID string) ([]Chunk, error) {
	rows, err := s.db.Query(`SELECT id,document_id,chat_id,ordinal,heading,page,text,tokens
FROM chunks WHERE document_id=? ORDER BY ordinal`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chunk{}
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.ChatID, &c.Ordinal, &c.Heading,
			&c.Page, &c.Text, &c.Tokens); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChunksInRange returns chunks of a document between two ordinals, used by the
// read_pages tool to expand a slice of a document on demand.
func (s *Store) ChunksInRange(documentID string, fromOrdinal, toOrdinal int) ([]Chunk, error) {
	rows, err := s.db.Query(`SELECT id,document_id,chat_id,ordinal,heading,page,text,tokens
FROM chunks WHERE document_id=? AND ordinal>=? AND ordinal<=? ORDER BY ordinal`,
		documentID, fromOrdinal, toOrdinal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chunk{}
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.ChatID, &c.Ordinal, &c.Heading,
			&c.Page, &c.Text, &c.Tokens); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SearchChunks runs an FTS5 BM25 query. An empty chatID searches every chat.
func (s *Store) SearchChunks(chatID, query string, limit int) ([]ChunkHit, error) {
	if !s.FTS {
		return nil, fmt.Errorf("full-text search is unavailable in this build")
	}
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 8
	}
	q := `SELECT c.id,c.document_id,c.chat_id,c.ordinal,c.heading,c.page,c.text,c.tokens,
  COALESCE(d.title,''), bm25(chunks_fts)
FROM chunks_fts f
JOIN chunks c ON c.id = f.rowid
LEFT JOIN documents d ON d.id = c.document_id
WHERE chunks_fts MATCH ?`
	args := []any{match}
	if chatID != "" {
		q += ` AND f.chat_id = ?`
		args = append(args, chatID)
	}
	q += ` ORDER BY bm25(chunks_fts) LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChunkHit{}
	for rows.Next() {
		var h ChunkHit
		if err := rows.Scan(&h.Chunk.ID, &h.Chunk.DocumentID, &h.Chunk.ChatID,
			&h.Chunk.Ordinal, &h.Chunk.Heading, &h.Chunk.Page, &h.Chunk.Text,
			&h.Chunk.Tokens, &h.Document, &h.Score); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ftsQuery turns free text into a safe FTS5 MATCH expression. FTS5 has its own
// operator syntax, so bare user text must be quoted term by term to avoid
// syntax errors on characters like '-', '"' or '*'.
func ftsQuery(query string) string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return !(r == '_' || r == '-' || r == '.' || r == '+' || r == '#' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') || r > 127)
	})
	terms := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.Trim(f, "-.+")
		if len(f) < 2 {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
		if len(terms) >= 12 {
			break
		}
	}
	if len(terms) == 0 {
		return ""
	}
	return strings.Join(terms, " OR ")
}

// --- compactions -----------------------------------------------------------

// CreateCompaction records a compaction event.
func (s *Store) CreateCompaction(c Compaction) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	_, err := s.db.Exec(`
INSERT INTO compactions (id,chat_id,trigger,from_seq,to_seq,summary,focus,
  tokens_before,tokens_after,model,backend,created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.ChatID, c.Trigger, c.FromSeq, c.ToSeq, c.Summary, c.Focus,
		c.TokensBefore, c.TokensAfter, c.Model, c.Backend, millis(c.CreatedAt))
	return err
}

// ListCompactions returns a chat's compactions oldest first.
func (s *Store) ListCompactions(chatID string) ([]Compaction, error) {
	rows, err := s.db.Query(`SELECT id,chat_id,trigger,from_seq,to_seq,summary,focus,
  tokens_before,tokens_after,model,backend,created_at
FROM compactions WHERE chat_id=? ORDER BY to_seq`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Compaction{}
	for rows.Next() {
		var c Compaction
		var created int64
		if err := rows.Scan(&c.ID, &c.ChatID, &c.Trigger, &c.FromSeq, &c.ToSeq,
			&c.Summary, &c.Focus, &c.TokensBefore, &c.TokensAfter, &c.Model,
			&c.Backend, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = fromMillis(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCompactionsFrom removes every compaction of a chat whose range ends at
// or after toSeq.
//
// Compactions are cumulative: each one is summarised on top of the memory in
// force when it ran, so a later compaction embeds the content of the earlier
// ones. Dropping the middle of that chain would leave the prompt asserting a
// summary of messages that are back in the live context, sending the same text
// twice, so the stale tail goes with it in a single statement.
func (s *Store) DeleteCompactionsFrom(chatID string, toSeq int) error {
	_, err := s.db.Exec(`DELETE FROM compactions WHERE chat_id=? AND to_seq>=?`, chatID, toSeq)
	return err
}

// --- tool calls ------------------------------------------------------------

// UpsertToolCall inserts or updates a tool call record.
func (s *Store) UpsertToolCall(tc ToolCallRecord) error {
	if tc.CreatedAt.IsZero() {
		tc.CreatedAt = time.Now()
	}
	_, err := s.db.Exec(`
INSERT INTO tool_calls (id,message_id,chat_id,name,args_json,result_json,status,duration_ms,error,
  source_tokens,context_tokens,condensed,withheld,created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET result_json=excluded.result_json,status=excluded.status,
  duration_ms=excluded.duration_ms,error=excluded.error,
  source_tokens=excluded.source_tokens,context_tokens=excluded.context_tokens,
  condensed=excluded.condensed,withheld=excluded.withheld`,
		tc.ID, tc.MessageID, tc.ChatID, tc.Name, tc.ArgsJSON, tc.ResultJSON,
		tc.Status, tc.DurationMs, tc.Error, tc.SourceTokens, tc.ContextTokens,
		boolToInt(tc.Condensed), boolToInt(tc.Withheld), millis(tc.CreatedAt))
	return err
}

// ListToolCalls returns a chat's tool calls oldest first.
func (s *Store) ListToolCalls(chatID string) ([]ToolCallRecord, error) {
	rows, err := s.db.Query(`SELECT id,message_id,chat_id,name,args_json,result_json,
  status,duration_ms,error,source_tokens,context_tokens,condensed,withheld,created_at
FROM tool_calls WHERE chat_id=? ORDER BY created_at`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ToolCallRecord{}
	for rows.Next() {
		var tc ToolCallRecord
		var created int64
		var condensed, withheld int
		if err := rows.Scan(&tc.ID, &tc.MessageID, &tc.ChatID, &tc.Name, &tc.ArgsJSON,
			&tc.ResultJSON, &tc.Status, &tc.DurationMs, &tc.Error, &tc.SourceTokens,
			&tc.ContextTokens, &condensed, &withheld, &created); err != nil {
			return nil, err
		}
		tc.Condensed = condensed != 0
		tc.Withheld = withheld != 0
		tc.CreatedAt = fromMillis(created)
		out = append(out, tc)
	}
	return out, rows.Err()
}

// --- file snapshots --------------------------------------------------------

// PutFileSnapshot records one change to one file, keeping the first pre-image.
//
// The insert carries the file as this chat found it, and a later change to the same file only
// replaces what the app last wrote and counts itself. That is what makes "put this file back"
// one button however many rounds the model took, and it keeps the row small: a turn that edits
// the same file ten times stores one copy of the text it replaced, not ten.
func (s *Store) PutFileSnapshot(snap FileSnapshot) error {
	if snap.CreatedAt.IsZero() {
		snap.CreatedAt = time.Now()
	}
	if snap.UpdatedAt.IsZero() {
		snap.UpdatedAt = snap.CreatedAt
	}
	if snap.Revisions < 1 {
		snap.Revisions = 1
	}
	// A change after a revert clears the marker: the file is the model's work again, so
	// there is nothing to warn its next turn about. The pre-image is not touched — the text
	// to put back stays the text from before this chat first touched the file.
	//
	// The outside-change marker is cleared here for the same reason: the digest it named is
	// no longer the file, and a note already delivered about some older text must not stop
	// the next one. Otherwise a file edited, announced, written by the model, and then
	// hand-edited back to the announced version would match the stale marker and be silent.
	_, err := s.db.Exec(`
INSERT INTO file_snapshots (chat_id,path,before_text,existed,after_sha,revisions,created_at,updated_at,reverted_at,noted_sha)
VALUES (?,?,?,?,?,?,?,?,0,'')
ON CONFLICT(chat_id,path) DO UPDATE SET after_sha=excluded.after_sha,
  revisions=file_snapshots.revisions+1, updated_at=excluded.updated_at,
  reverted_at=0, noted_sha=''`,
		snap.ChatID, snap.Path, snap.BeforeText, boolToInt(snap.Existed), snap.AfterSHA,
		snap.Revisions, millis(snap.CreatedAt), millis(snap.UpdatedAt))
	return err
}

// GetFileSnapshot loads one file's undo record.
func (s *Store) GetFileSnapshot(chatID, path string) (FileSnapshot, bool, error) {
	row := s.db.QueryRow(`SELECT chat_id,path,before_text,existed,after_sha,revisions,
  reverted_at,noted_sha,created_at,updated_at FROM file_snapshots WHERE chat_id=? AND path=?`, chatID, path)
	snap, err := scanFileSnapshot(row)
	if err == sql.ErrNoRows {
		return FileSnapshot{}, false, nil
	}
	if err != nil {
		return FileSnapshot{}, false, err
	}
	return snap, true, nil
}

// ListFileSnapshots returns a chat's changed files, most recently changed first — the order a
// panel wants, because the file a turn just wrote is the one the person is looking at.
//
// A file the user put back is not one of them: the change it describes is no longer in the
// folder, so listing it would offer an undo for something that already happened. Its row lives
// on until the marker is cleared, because the model still has to be told (see PendingReverts).
func (s *Store) ListFileSnapshots(chatID string) ([]FileSnapshot, error) {
	return s.fileSnapshots(`SELECT chat_id,path,before_text,existed,after_sha,revisions,
  reverted_at,noted_sha,created_at,updated_at FROM file_snapshots
WHERE chat_id=? AND reverted_at=0 ORDER BY updated_at DESC, path`, chatID)
}

// MarkFileSnapshotNoted records that a turn has told the model this outside version of a file.
//
// Only the digest, because only the question "is this the same outside text I already announced"
// needs answering. A file the model then writes resets it (PutFileSnapshot), and a version edited
// again afterwards has a different digest and is news in its own right.
//
// after is the digest the note was built against, and the update is conditional on it. The model
// may rewrite the file during the very turn that carried the note: by then the row holds the new
// text and a cleared marker, and stamping the old outside version over it would make a later edit
// that happens to restore that same text look like one already announced.
func (s *Store) MarkFileSnapshotNoted(chatID, path, sha, after string) error {
	_, err := s.db.Exec(`UPDATE file_snapshots SET noted_sha=?
WHERE chat_id=? AND path=? AND reverted_at=0 AND after_sha=?`, sha, chatID, path, after)
	return err
}

// PendingReverts returns the files this chat's user put back, oldest first, whose marker has not
// been cleared yet.
//
// It exists because of what the history says. A tool result in the transcript is a claim about a
// file — "written, 40 lines" — and after a revert that claim is false; the model would reason
// from text that is no longer in the folder, quote it, or edit around a line that moved. So the
// next turn is told, and ClearReverts forgets the marker once that turn has had the note.
func (s *Store) PendingReverts(chatID string) ([]FileSnapshot, error) {
	return s.fileSnapshots(`SELECT chat_id,path,before_text,existed,after_sha,revisions,
  reverted_at,noted_sha,created_at,updated_at FROM file_snapshots
WHERE chat_id=? AND reverted_at>0 ORDER BY reverted_at, path`, chatID)
}

// MarkFileSnapshotReverted records that a file was put back. The row stays — the fact that the
// model's own results are now wrong is worth keeping — and the marker is what makes it a note
// for the next turn rather than an entry in the panel.
func (s *Store) MarkFileSnapshotReverted(chatID, path string) error {
	_, err := s.db.Exec(`UPDATE file_snapshots SET reverted_at=?
WHERE chat_id=? AND path=? AND reverted_at=0`, millis(time.Now()), chatID, path)
	return err
}

// ClearReverts drops the markers a turn has already told the model about.
func (s *Store) ClearReverts(chatID string) error {
	_, err := s.db.Exec(`DELETE FROM file_snapshots WHERE chat_id=? AND reverted_at>0`, chatID)
	return err
}

func (s *Store) fileSnapshots(query string, chatID string) ([]FileSnapshot, error) {
	rows, err := s.db.Query(query, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileSnapshot{}
	for rows.Next() {
		snap, scanErr := scanFileSnapshot(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

func scanFileSnapshot(row interface{ Scan(...any) error }) (FileSnapshot, error) {
	var snap FileSnapshot
	var existed int
	var reverted, created, updated int64
	if err := row.Scan(&snap.ChatID, &snap.Path, &snap.BeforeText, &existed, &snap.AfterSHA,
		&snap.Revisions, &reverted, &snap.NotedSHA, &created, &updated); err != nil {
		return FileSnapshot{}, err
	}
	snap.Existed = existed != 0
	if reverted > 0 {
		snap.RevertedAt = fromMillis(reverted)
	}
	snap.CreatedAt = fromMillis(created)
	snap.UpdatedAt = fromMillis(updated)
	return snap, nil
}

// --- caches ----------------------------------------------------------------

// PutSearchCache stores a search response.
func (s *Store) PutSearchCache(e SearchCacheEntry) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	_, err := s.db.Exec(`
INSERT INTO search_cache (query_hash,provider,query,response_json,created_at,expires_at)
VALUES (?,?,?,?,?,?)
ON CONFLICT(query_hash) DO UPDATE SET provider=excluded.provider,query=excluded.query,
  response_json=excluded.response_json,created_at=excluded.created_at,expires_at=excluded.expires_at`,
		e.QueryHash, e.Provider, e.Query, e.ResponseJSON, millis(e.CreatedAt), millis(e.ExpiresAt))
	return err
}

// GetSearchCache returns a cached search response if it is still valid.
func (s *Store) GetSearchCache(queryHash string) (SearchCacheEntry, bool) {
	var e SearchCacheEntry
	var created, expires int64
	err := s.db.QueryRow(`SELECT query_hash,provider,query,response_json,created_at,expires_at
FROM search_cache WHERE query_hash=?`, queryHash).
		Scan(&e.QueryHash, &e.Provider, &e.Query, &e.ResponseJSON, &created, &expires)
	if err != nil {
		return SearchCacheEntry{}, false
	}
	if time.Now().After(fromMillis(expires)) {
		return SearchCacheEntry{}, false
	}
	e.CreatedAt = fromMillis(created)
	e.ExpiresAt = fromMillis(expires)
	return e, true
}

// PutScrapeCache stores extracted page content.
func (s *Store) PutScrapeCache(e ScrapeCacheEntry) error {
	if e.FetchedAt.IsZero() {
		e.FetchedAt = time.Now()
	}
	_, err := s.db.Exec(`
INSERT INTO scrape_cache (url_hash,url,title,content_md,tokens,fetched_at,expires_at)
VALUES (?,?,?,?,?,?,?)
ON CONFLICT(url_hash) DO UPDATE SET url=excluded.url,title=excluded.title,
  content_md=excluded.content_md,tokens=excluded.tokens,fetched_at=excluded.fetched_at,
  expires_at=excluded.expires_at`,
		e.URLHash, e.URL, e.Title, e.ContentMD, e.Tokens, millis(e.FetchedAt), millis(e.ExpiresAt))
	return err
}

// GetScrapeCache returns cached page content if still valid.
func (s *Store) GetScrapeCache(urlHash string) (ScrapeCacheEntry, bool) {
	var e ScrapeCacheEntry
	var fetched, expires int64
	err := s.db.QueryRow(`SELECT url_hash,url,title,content_md,tokens,fetched_at,expires_at
FROM scrape_cache WHERE url_hash=?`, urlHash).
		Scan(&e.URLHash, &e.URL, &e.Title, &e.ContentMD, &e.Tokens, &fetched, &expires)
	if err != nil {
		return ScrapeCacheEntry{}, false
	}
	if time.Now().After(fromMillis(expires)) {
		return ScrapeCacheEntry{}, false
	}
	e.FetchedAt = fromMillis(fetched)
	e.ExpiresAt = fromMillis(expires)
	return e, true
}

// PutDigestCache stores a condensed tool result.
func (s *Store) PutDigestCache(e DigestCacheEntry) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	_, err := s.db.Exec(`
INSERT INTO digest_cache (key,model,source_tokens,summary,digest_tokens,created_at,expires_at)
VALUES (?,?,?,?,?,?,?)
ON CONFLICT(key) DO UPDATE SET model=excluded.model,source_tokens=excluded.source_tokens,
  summary=excluded.summary,digest_tokens=excluded.digest_tokens,
  created_at=excluded.created_at,expires_at=excluded.expires_at`,
		e.Key, e.Model, e.SourceTokens, e.Summary, e.DigestTokens,
		millis(e.CreatedAt), millis(e.ExpiresAt))
	return err
}

// GetDigestCache returns a condensation if it has not expired.
func (s *Store) GetDigestCache(key string) (DigestCacheEntry, bool) {
	var e DigestCacheEntry
	var created, expires int64
	err := s.db.QueryRow(`SELECT key,model,source_tokens,summary,digest_tokens,created_at,expires_at
FROM digest_cache WHERE key=?`, key).
		Scan(&e.Key, &e.Model, &e.SourceTokens, &e.Summary, &e.DigestTokens, &created, &expires)
	if err != nil {
		return DigestCacheEntry{}, false
	}
	if time.Now().After(fromMillis(expires)) {
		return DigestCacheEntry{}, false
	}
	e.CreatedAt = fromMillis(created)
	e.ExpiresAt = fromMillis(expires)
	return e, true
}

// --- settings and counters -------------------------------------------------

// GetSetting reads a JSON setting.
func (s *Store) GetSetting(key string, out any) (bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value_json FROM settings WHERE key=?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return false, fmt.Errorf("decode setting %q: %w", key, err)
	}
	return true, nil
}

// PutSetting writes a JSON setting.
func (s *Store) PutSetting(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
INSERT INTO settings (key,value_json) VALUES (?,?)
ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json`, key, string(raw))
	return err
}

// IncrCounter bumps a counter for the given period and returns the new value.
func (s *Store) IncrCounter(key, period string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var value int64
	var existingPeriod string
	err = tx.QueryRow(`SELECT value,period FROM counters WHERE key=?`, key).Scan(&value, &existingPeriod)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		value = 1
		if _, err := tx.Exec(`INSERT INTO counters (key,value,period) VALUES (?,?,?)`, key, value, period); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, err
	default:
		if existingPeriod != period {
			value = 0
		}
		value++
		if _, err := tx.Exec(`UPDATE counters SET value=?,period=? WHERE key=?`, value, period, key); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return value, nil
}

// Counter returns the current counter value for a period, or zero.
func (s *Store) Counter(key, period string) int64 {
	var value int64
	var existingPeriod string
	if err := s.db.QueryRow(`SELECT value,period FROM counters WHERE key=?`, key).
		Scan(&value, &existingPeriod); err != nil {
		return 0
	}
	if existingPeriod != period {
		return 0
	}
	return value
}

// --- per-model overrides ---------------------------------------------------

// ModelOverrides returns every stored per-model override keyed by
// "backend/model".
func (s *Store) ModelOverrides() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT backend,model,settings_json FROM model_overrides`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var backend, model, raw string
		if err := rows.Scan(&backend, &model, &raw); err != nil {
			return nil, err
		}
		out[backend+"/"+model] = raw
	}
	return out, rows.Err()
}

// PutModelOverride stores a per-model override. Passing an override with no
// values clears the row, so "reset to backend defaults" is expressible.
func (s *Store) PutModelOverride(backend, model string, ov any) error {
	raw, err := json.Marshal(ov)
	if err != nil {
		return err
	}
	if string(raw) == "{}" || string(raw) == "null" {
		return s.DeleteModelOverride(backend, model)
	}
	_, err = s.db.Exec(`
INSERT INTO model_overrides (backend,model,settings_json) VALUES (?,?,?)
ON CONFLICT(backend,model) DO UPDATE SET settings_json=excluded.settings_json`,
		backend, model, string(raw))
	return err
}

// DeleteModelOverride removes a per-model override.
func (s *Store) DeleteModelOverride(backend, model string) error {
	_, err := s.db.Exec(`DELETE FROM model_overrides WHERE backend=? AND model=?`, backend, model)
	return err
}

// LoadModelOverrides decodes every stored override into a map keyed by
// "backend/model", so settings can be hydrated from the database on startup.
func (s *Store) LoadModelOverrides() (map[string]string, error) {
	return s.ModelOverrides()
}
