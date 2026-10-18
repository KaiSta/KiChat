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
	// Derivation records that this picture was enlarged from another attachment. It is
	// not a generation recipe: an upscaled copy has no prompt of its own, and giving it
	// one would make Redraw offer to invent a different picture. Keeping the two apart
	// is what lets an upscaled photo say where it came from without pretending it was
	// drawn.
	Derivation *Derivation `json:"derivation,omitempty"`
}

// Derivation is how a derived picture was made: which attachment it came from, by
// which method, and at what scale.
//
// It is stored rather than inferred so the card can answer "what is this, and why is
// it larger?" after a restart, and so two enlargements of one picture can be told
// apart. The source id is the attachment, not a filename: names are made unique per
// chat and a file may have been renamed since.
type Derivation struct {
	SourceAttachmentID string `json:"sourceAttachmentId"`
	SourceWidth        int    `json:"sourceWidth,omitempty"`
	SourceHeight       int    `json:"sourceHeight,omitempty"`
	// Method is "lanczos" or "esrgan".
	Method string `json:"method"`
	// Model is the Real-ESRGAN weights, empty for a resample.
	Model string `json:"model,omitempty"`
	// Scale is the enlargement per side, recorded from the pixels rather than from the
	// request so a capped target reports what really happened.
	Scale      float64 `json:"scale,omitempty"`
	Sharpen    float64 `json:"sharpen,omitempty"`
	DurationMs int64   `json:"durationMs,omitempty"`
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
	// Prompt is the words the picture was drawn from. For a caption-trained model that is
	// the caption rather than the request, which is why a redraw replays it instead of
	// asking for a new one.
	Prompt string `json:"prompt"`
	// SourcePrompt is what the user asked for when a caption was drawn from instead of it,
	// empty when the two are the same. A picture has two true descriptions — the request,
	// and the words the model was really given — and recording only one of them makes
	// either the card or the redraw lie.
	SourcePrompt string `json:"sourcePrompt,omitempty"`
	// CaptionModel is the model that wrote the caption, empty when none did. Without it a
	// card cannot answer "why does this look nothing like my prompt?", which is the
	// question a caption is most often the answer to.
	CaptionModel   string `json:"captionModel,omitempty"`
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
	// Hires says a second sampling pass ran, and the two numbers are what it ran with.
	// They live beside the base Width and Height rather than replacing them, because a
	// redraw has to ask for the same base and the same enlargement — recording only the
	// output size would draw a picture that is already large and then enlarge it again.
	Hires        bool    `json:"hires,omitempty"`
	HiresScale   float64 `json:"hiresScale,omitempty"`
	HiresDenoise float64 `json:"hiresDenoise,omitempty"`
	// DurationMs is how long the draw took, when the caller measured it. It is recorded so
	// the picture's card can show what it cost — both passes included, which is the best
	// predictor a redraw has — and a recipe from before this field simply carries zero.
	DurationMs int64 `json:"durationMs,omitempty"`
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
	Trigger      string    `json:"trigger"` // auto | manual | too_large
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
