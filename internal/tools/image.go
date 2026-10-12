package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"kichat/internal/config"
	"kichat/internal/images"
	"kichat/internal/ingest"
	"kichat/internal/llm"
	"kichat/internal/store"
)

// ToolGenerateImage is the one tool that does not read anything: it costs GPU
// minutes on the user's own machine and is therefore the only tool whose
// availability is worth gating on configuration rather than on a credential.
const ToolGenerateImage = "generate_image"

// callingMessageKey carries the assistant message that requested a tool call.
//
// An attachment is rendered as part of the message it belongs to — the frontend
// reads `message.attachments` — so a generated picture has to be linked to the
// assistant turn that asked for it. A tool call is only told its chat, and the
// service is shared between chats, so the message id travels in the context of
// the one call rather than in a field that two concurrent turns could overwrite.
type callingMessageKey struct{}

// WithCallingMessage attaches the requesting message id to a tool call.
func WithCallingMessage(ctx context.Context, messageID string) context.Context {
	return context.WithValue(ctx, callingMessageKey{}, messageID)
}

// callingMessage returns the requesting message id, empty when there is none.
func callingMessage(ctx context.Context) string {
	id, _ := ctx.Value(callingMessageKey{}).(string)
	return id
}

// generateImageArgs is what the model may send. Every field but the prompt is
// optional and tolerates a string where a number belongs, which is what
// decodeLoose plus the `any` type is for: small local models emit "512" as often
// as 512, and refusing the call over that would waste a whole tool round.
type generateImageArgs struct {
	Prompt         string `json:"prompt"`
	NegativePrompt string `json:"negative_prompt"`
	Width          any    `json:"width"`
	Height         any    `json:"height"`
	Steps          any    `json:"steps"`
	Seed           any    `json:"seed"`
	Sampler        string `json:"sampler"`
}

// generateImage draws the requested picture and files it in the chat.
func (s *Service) generateImage(ctx context.Context, chatID, argsJSON string) (string, error) {
	if s.Images == nil {
		return "", images.ErrNotConfigured
	}
	var args generateImageArgs
	if err := decodeLoose(argsJSON, &args); err != nil {
		return "", fmt.Errorf("ungültige Argumente: %w", err)
	}
	prompt := strings.TrimSpace(args.Prompt)
	if prompt == "" {
		// A picture with no prompt is not a degenerate case, it is a blank canvas:
		// the server would draw something arbitrary and charge for it.
		return "", fmt.Errorf("prompt fehlt — ein Bild braucht eine Beschreibung")
	}

	cfg := s.imageSettings()
	req := images.Request{
		Prompt: prompt,
		// The tool's own argument wins over the configured default, and an empty
		// one on both sides means "send nothing", which is what leaves a negative
		// prompt configured in the user's own WebUI in force.
		NegativePrompt: firstNonEmptyString(args.NegativePrompt, cfg.NegativePrompt),
		Width:          looseInt(args.Width, cfg.Width),
		Height:         looseInt(args.Height, cfg.Height),
		Steps:          looseInt(args.Steps, cfg.Steps),
		Sampler:        firstNonEmptyString(args.Sampler, cfg.Sampler),
		// The checkpoint is a setting rather than an argument: which models are
		// installed is a property of the machine, and a model that guessed a name
		// would get a silent fallback to whatever was already loaded.
		Model: cfg.Model,
		Seed:  seedFromValue(args.Seed),
	}

	result, err := s.Images.Generate(ctx, req)
	if err != nil {
		return "", err
	}

	messageID := callingMessage(ctx)
	for i, img := range result.Images {
		if _, err := s.storeGeneratedImage(chatID, messageID, imageFilename(prompt, i, img.Ext), img); err != nil {
			// The picture is already drawn and paid for, so an error here loses it
			// entirely. Reporting the failure is still right: silently returning a
			// cheerful result would leave the user with a tool card claiming a
			// picture that exists nowhere.
			return "", fmt.Errorf("das erzeugte Bild konnte nicht gespeichert werden: %w", err)
		}
	}

	return summarizeGeneration(req, result), nil
}

// summarizeGeneration is what the model reads back. It is written for the model,
// not for the user — the picture itself is in the transcript — and it has one job
// above all: stopping the model from describing what it drew. The chat model that
// called this tool has not seen the picture, and a reply that invents its contents
// reads as a broken feature rather than as an absent capability.
func summarizeGeneration(req images.Request, result images.Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d Bild(er) wurden erzeugt und im Chat angezeigt.\n", len(result.Images))
	fmt.Fprintf(&sb, "Prompt: %s\n", req.Prompt)
	if req.NegativePrompt != "" {
		fmt.Fprintf(&sb, "Negativ-Prompt: %s\n", req.NegativePrompt)
	}
	if req.Width > 0 && req.Height > 0 {
		fmt.Fprintf(&sb, "Größe: %dx%d", req.Width, req.Height)
		if req.Steps > 0 {
			fmt.Fprintf(&sb, ", %d Schritte", req.Steps)
		}
		sb.WriteString("\n")
	}
	// The seed is the one number worth reporting even when the request left it
	// open: the server chose one, and quoting it is what lets the user ask for the
	// same picture again.
	if result.Seed != 0 {
		fmt.Fprintf(&sb, "Seed: %d\n", result.Seed)
	}
	if result.Model != "" {
		fmt.Fprintf(&sb, "Modell: %s\n", result.Model)
	}
	sb.WriteString("\nDu siehst das Bild selbst NICHT und darfst seinen Inhalt nicht beschreiben. " +
		"Sage kurz, dass es erzeugt wurde, und nenne bei Bedarf den Prompt oder den Seed. " +
		"Wenn der Nutzer das Bild ändern will, rufe generate_image mit einem angepassten " +
		"Prompt erneut auf.")
	return sb.String()
}

// imageSettings reads the current image configuration. It goes through a function
// rather than being copied into the service at build time so that a settings save
// takes effect on the next tool call instead of on the next restart.
func (s *Service) imageSettings() config.ImageSettings {
	if s.Settings == nil {
		return config.ImageSettings{}
	}
	return s.Settings().Images
}

// storeGeneratedImage writes one picture as an attachment of the message that
// asked for it, in exactly the shape an uploaded image has: a Document whose text
// is the base64 payload, and an inline image attachment pointing at it.
//
// Reusing that shape is the whole point. The assembly already turns an inline
// image attachment back into an image part of the request, so a picture KiChat
// generated can be handed to a vision model on a later turn without a single line
// of new plumbing — and the frontend already renders a message's attachments.
func (s *Service) storeGeneratedImage(chatID, messageID, filename string, img images.Image) (store.Attachment, error) {
	if s.Store == nil {
		return store.Attachment{}, fmt.Errorf("kein Speicher verfügbar")
	}
	now := time.Now()
	// The estimate is the app's shared one for an image payload, so the context
	// meter prices a generated picture exactly as it prices an attached one.
	tokens := llm.EstimateImageTokens(1)
	attachment := store.Attachment{
		ID:            uuid.NewString(),
		ChatID:        chatID,
		MessageID:     messageID,
		Kind:          string(ingest.KindImage),
		Filename:      filename,
		Mime:          img.Mime,
		SizeBytes:     int64(len(img.Data)),
		TokenEstimate: tokens,
		Placement:     store.PlacementInline,
		CreatedAt:     now,
	}
	document := store.Document{
		ID:           uuid.NewString(),
		AttachmentID: attachment.ID,
		ChatID:       chatID,
		Title:        filename,
		Text:         base64.StdEncoding.EncodeToString(img.Data),
		TokenCount:   tokens,
		CreatedAt:    now,
	}
	if err := s.Store.CreateDocument(document); err != nil {
		return store.Attachment{}, err
	}
	attachment.DocumentID = document.ID
	if err := s.Store.CreateAttachment(attachment); err != nil {
		// The document is the parent, and it holds the largest row this call
		// writes. Losing the attachment must not leave that payload behind with
		// nothing pointing at it; this is the same rule AddAttachment follows.
		_ = s.Store.DeleteDocument(document.ID)
		return store.Attachment{}, err
	}
	return attachment, nil
}

// seedFromValue reads a seed the model supplied. A missing seed, a negative one —
// which is the WebUI's own spelling of "random" — and a value that is not a number
// all mean the same thing here: let the server choose.
func seedFromValue(value any) *int64 {
	switch v := value.(type) {
	case nil:
		return nil
	case float64:
		if v < 0 {
			return nil
		}
		n := int64(v)
		return &n
	case string:
		return images.ParseSeed(v)
	}
	return nil
}

// imageFilename builds a name that says what the picture is. A generated file is
// saved by the user under this name, so "a-red-fox-in-snow-1.png" is worth ten
// lines of slugging where "image-1.png" would leave them guessing which of six
// files is which.
//
// The extension comes from the payload rather than from a constant: a WebUI can be
// configured to answer with JPEG, and naming that file `.png` produces a file whose
// type and name disagree.
func imageFilename(prompt string, index int, ext string) string {
	if strings.TrimSpace(ext) == "" {
		// Nothing was sniffable, so the format the route returns by default is used.
		ext = "png"
	}
	slug := slugify(prompt)
	if slug == "" {
		slug = "bild"
	}
	// Long prompts are common and a 300-character filename is hostile on every
	// platform that has to hold it.
	if runes := []rune(slug); len(runes) > 48 {
		slug = strings.Trim(string(runes[:48]), "-")
	}
	return fmt.Sprintf("%s-%d.%s", slug, index+1, ext)
}

// slugify keeps letters and digits and collapses everything else into single
// dashes. It is unicode-aware on purpose: a German or Chinese prompt is a normal
// prompt here, and a byte-oriented filter would reduce "ein Fuchs im Schnee" to
// "ein-fuchs-im-schnee" but "Füchse" to "f-chse".
func slugify(value string) string {
	var sb strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			sb.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && sb.Len() > 0 {
				sb.WriteRune('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(sb.String(), "-")
}
