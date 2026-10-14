package store

import "time"

// Message status values. "interrupted" marks a generation the user stopped and
// "cancelled" a tool call that was abandoned; both are kept so the transcript
// stays honest about what happened. "failed" marks a reply the backend stopped
// delivering halfway through, which is a different story from a user pressing
// Stop — the text is kept, but it must not read as a finished answer.
const (
	StatusComplete    = "complete"
	StatusStreaming   = "streaming"
	StatusInterrupted = "interrupted"
	StatusFailed      = "failed"
	StatusCancelled   = "cancelled"
)

// Chat is one conversation.
type Chat struct {
	ID                 string    `json:"id"`
	Title              string    `json:"title"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
	Backend            string    `json:"backend"`
	Model              string    `json:"model"`
	SystemPrompt       string    `json:"systemPrompt"`
	SystemPromptPreset string    `json:"systemPromptPreset"`
	Temperature        float64   `json:"temperature"`
	TopP               float64   `json:"topP"`
	NumCtx             int       `json:"numCtx"`
	Archived           bool      `json:"archived"`
	AutoCompact        bool      `json:"autoCompact"`
	CompactThreshold   float64   `json:"compactThreshold"`
	KeepRecentTurns    int       `json:"keepRecentTurns"`
	SearchFirst        bool      `json:"searchFirst"`
}

// Message is one turn in a chat.
type Message struct {
	ID               string    `json:"id"`
	ChatID           string    `json:"chatId"`
	Seq              int       `json:"seq"`
	Role             string    `json:"role"`
	Content          string    `json:"content"`
	Reasoning        string    `json:"reasoning"`
	Name             string    `json:"name"`
	ToolCallsJSON    string    `json:"toolCallsJson"`
	ToolCallID       string    `json:"toolCallId"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"createdAt"`
	Backend          string    `json:"backend"`
	Model            string    `json:"model"`
	PromptTokens     int       `json:"promptTokens"`
	CompletionTokens int       `json:"completionTokens"`
	TokensPerSecond  float64   `json:"tokensPerSecond"`
	TTFTMs           int64     `json:"ttftMs"`
	GenerationMs     int64     `json:"generationMs"`
	StopReason       string    `json:"stopReason"`
	StatsSource      string    `json:"statsSource"`

	// Attachments are populated when a message is loaded, not stored inline.
	Attachments []Attachment `json:"attachments,omitempty"`
	// ToolCalls are populated when a message is loaded.
	ToolCalls []ToolCallRecord `json:"toolCalls,omitempty"`
}

// Attachment is a file the user added to a chat.
type Attachment struct {
	ID            string    `json:"id"`
	ChatID        string    `json:"chatId"`
	MessageID     string    `json:"messageId"`
	Kind          string    `json:"kind"`
	Filename      string    `json:"filename"`
	Mime          string    `json:"mime"`
	SizeBytes     int64     `json:"sizeBytes"`
	SHA256        string    `json:"sha256"`
	Pages         int       `json:"pages"`
	TokenEstimate int       `json:"tokenEstimate"`
	Placement     string    `json:"placement"`
	DocumentID    string    `json:"documentId"`
	Warning       string    `json:"warning,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	// SavedPath is where this attachment was exported to, empty when it was never saved to
	// a file. It is a fact about the *copy* rather than about the attachment: the picture
	// itself lives in the document either way, so an empty path means "the only copy is the
	// one in this database" and nothing more. That is what the pictures panel reads it for.
	SavedPath string `json:"savedPath,omitempty"`

	// Generation is the recipe a picture this app drew was made with, so it can be
	// drawn again. It is nil for a file the user attached: a photograph has no
	// prompt, and offering to redraw one would be offering to draw something
	// unrelated to it.
	Generation *GenerationRecipe `json:"generation,omitempty"`
}

// GenerationRecipe is what one text-to-image generation was made with, recorded so the
// picture can be drawn again from its own fields rather than from a prose description
// of them.
//
// It stores what the server *actually used*, not what was asked for, wherever the two
// differ. A request that left the seed open was given one by the server, and only that
// number reproduces the picture — the asked-for value was "none". A size or a step count
// the server filled in from its own settings stays 0 here, exactly as it does on the way
// in, so a redraw leaves that choice open again instead of freezing a number the user
// never picked.
//
// Seed 0 means the server reported none. Zero is not a usable seed in practice, which is
// why images.Result spells "not reported" the same way, and why a redraw of such a
// picture lets the server choose again.
type GenerationRecipe struct {
	Prompt         string `json:"prompt"`
	NegativePrompt string `json:"negativePrompt,omitempty"`
	Width          int    `json:"width,omitempty"`
	Height         int    `json:"height,omitempty"`
	Steps          int    `json:"steps,omitempty"`
	Sampler        string `json:"sampler,omitempty"`
	// CfgScale is the guidance scale the picture was drawn at, zero when nobody chose
	// one. It belongs in the recipe for the same reason the size does: it is part of
	// what makes a picture reproducible, and two pictures of one prompt that look
	// different are most often a different scale. It lives in the recipe's own JSON, so
	// an older row simply has no such key.
	CfgScale float64 `json:"cfgScale,omitempty"`
	Seed     int64   `json:"seed,omitempty"`
	// Model is the checkpoint the server reported using, falling back to the one that
	// was requested. Empty is "whatever is loaded", which is what a redraw asks for
	// again rather than naming a checkpoint the server may no longer have.
	Model string `json:"model,omitempty"`
}

// Attachment placement modes.
const (
	PlacementInline  = "inline"
	PlacementIndexed = "indexed"
)

// Document is extracted text from an attachment or a scraped page.
type Document struct {
	ID           string    `json:"id"`
	AttachmentID string    `json:"attachmentId"`
	ChatID       string    `json:"chatId"`
	Title        string    `json:"title"`
	Text         string    `json:"text"`
	TokenCount   int       `json:"tokenCount"`
	PageCount    int       `json:"pageCount"`
	SHA256       string    `json:"sha256"`
	SourceURL    string    `json:"sourceUrl"`
	CreatedAt    time.Time `json:"createdAt"`
}

// Chunk is one indexed slice of a document.
type Chunk struct {
	ID         int64  `json:"id"`
	DocumentID string `json:"documentId"`
	ChatID     string `json:"chatId"`
	Ordinal    int    `json:"ordinal"`
	Heading    string `json:"heading"`
	Page       int    `json:"page"`
	Text       string `json:"text"`
	Tokens     int    `json:"tokens"`
}

// ChunkHit is a search result.
type ChunkHit struct {
	Chunk    Chunk   `json:"chunk"`
	Document string  `json:"documentTitle"`
	Score    float64 `json:"score"`
}

// Compaction records a summarisation event. Original messages are never
// removed, so a compaction is a reading instruction over history, not a
// replacement of it.
type Compaction struct {
	ID           string    `json:"id"`
	ChatID       string    `json:"chatId"`
	Trigger      string    `json:"trigger"` // auto | manual
	FromSeq      int       `json:"fromSeq"`
	ToSeq        int       `json:"toSeq"`
	Summary      string    `json:"summary"`
	Focus        string    `json:"focus"`
	TokensBefore int       `json:"tokensBefore"`
	TokensAfter  int       `json:"tokensAfter"`
	Model        string    `json:"model"`
	Backend      string    `json:"backend"`
	CreatedAt    time.Time `json:"createdAt"`
}

// Tool call status values.
const (
	ToolPending   = "pending"
	ToolOK        = "ok"
	ToolError     = "error"
	ToolCancelled = "cancelled"
)

// ToolCallRecord is the persisted outcome of one tool invocation.
type ToolCallRecord struct {
	ID         string    `json:"id"`
	MessageID  string    `json:"messageId"`
	ChatID     string    `json:"chatId"`
	Name       string    `json:"name"`
	ArgsJSON   string    `json:"argsJson"`
	ResultJSON string    `json:"resultJson"`
	Status     string    `json:"status"`
	DurationMs int64     `json:"durationMs"`
	Error      string    `json:"error"`
	CreatedAt  time.Time `json:"createdAt"`
}

// SearchCacheEntry caches a search provider response.
type SearchCacheEntry struct {
	QueryHash    string    `json:"queryHash"`
	Provider     string    `json:"provider"`
	Query        string    `json:"query"`
	ResponseJSON string    `json:"responseJson"`
	CreatedAt    time.Time `json:"createdAt"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

// ScrapeCacheEntry caches extracted page content.
type ScrapeCacheEntry struct {
	URLHash   string    `json:"urlHash"`
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	ContentMD string    `json:"contentMd"`
	Tokens    int       `json:"tokens"`
	FetchedAt time.Time `json:"fetchedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}
