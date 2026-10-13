package tools

import (
	"context"
	"encoding/base64"
	"errors"
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
	// Bounded before anything is drawn *or recorded*. The recipe written below is built from
	// this request, and the schema this call answers to advertises 64–2048 and 1–150 — but a
	// model is free to invent a number, and the smallest local ones often do. A recipe that
	// named a size the provider quietly replaced would describe a picture that does not
	// exist, so the clamp happens here rather than only at the backend; see
	// images.Request.Normalised.
	req = req.Normalised()

	result, err := s.Images.Generate(ctx, req)
	if err != nil {
		return "", err
	}

	// The recipe is recorded beside the picture, so "draw that again" needs no model turn
	// and no parsing of the prose summary below.
	recipe := recipeFrom(req, result)
	messageID := callingMessage(ctx)
	// The names this chat already shows, so a second draw of one prompt does not land under
	// the name the first one took. Redrawing has guarded this from the start (uniqueName);
	// this path did not, and it is the one a model drives — "draw that again" is a single
	// sentence to a model that will happily comply, and it produced two pictures under one
	// name, which are two pictures a user cannot tell apart in the caption or in the save
	// dialog that offers the same filename twice.
	used, err := s.usedFilenames(chatID)
	if err != nil {
		return "", fmt.Errorf("die vergebenen Bildnamen konnten nicht gelesen werden: %w", err)
	}
	for i, img := range result.Images {
		name := uniqueName(used, imageFilename(prompt, i, img.Ext))
		if _, err := s.storeGeneratedImage(chatID, messageID, name, img, &recipe); err != nil {
			// The picture is already drawn and paid for, so an error here loses it
			// entirely. Reporting the failure is still right: silently returning a
			// cheerful result would leave the user with a tool card claiming a
			// picture that exists nowhere.
			return "", fmt.Errorf("das erzeugte Bild konnte nicht gespeichert werden: %w", err)
		}
		used[name] = true
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
func (s *Service) storeGeneratedImage(chatID, messageID, filename string, img images.Image, recipe *store.GenerationRecipe) (store.Attachment, error) {
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
		// Nil for a picture with no recorded recipe, which is what keeps an uploaded
		// photo from offering to redraw itself.
		Generation: recipe,
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

// recipeFrom records what a generation was made with, so the picture can be drawn again
// from its own fields rather than from the prose summary the model reads.
//
// The effective value wins wherever the request and the answer disagree. A request that
// left the seed open was given one by the server, and only that number reproduces the
// picture — the requested value was "none". The checkpoint follows the same rule, because
// the server knows which one it really loaded, and a recipe that named the wrong one would
// draw something else while claiming to repeat.
func recipeFrom(req images.Request, result images.Result) store.GenerationRecipe {
	recipe := store.GenerationRecipe{
		Prompt:         req.Prompt,
		NegativePrompt: req.NegativePrompt,
		Width:          req.Width,
		Height:         req.Height,
		Steps:          req.Steps,
		Sampler:        req.Sampler,
		Model:          firstNonEmptyString(result.Model, req.Model),
	}
	switch {
	case result.Seed != 0:
		recipe.Seed = result.Seed
	case req.Seed != nil:
		recipe.Seed = *req.Seed
	}
	return recipe
}

// requestFromRecipe turns a recipe into a request.
//
// Seed 0 is "let the server choose", which is both what a picture whose server never
// reported a seed asks for again and how the panel's editor spells an emptied seed box.
//
// Every number is bounded by the same call the tool's own path uses, and that is not
// belt-and-braces: a stored recipe came from this app, but an *edited* one comes from a box
// the user typed into, and a local server does not refuse a 100000-step request — it
// attempts it, for hours. Which bound, and why zero and negative mean "the server's own",
// is limits.go's business rather than this function's.
func requestFromRecipe(recipe store.GenerationRecipe) images.Request {
	req := images.Request{
		Prompt:         recipe.Prompt,
		NegativePrompt: recipe.NegativePrompt,
		Width:          recipe.Width,
		Height:         recipe.Height,
		Steps:          recipe.Steps,
		Sampler:        recipe.Sampler,
		Model:          recipe.Model,
	}
	if recipe.Seed > 0 {
		seed := recipe.Seed
		req.Seed = &seed
	}
	return req.Normalised()
}

// Regenerate draws a picture from a recipe and files the result beside the one it was
// redrawn from — the same chat and the same message, so it appears next to its source
// rather than somewhere the user has to go looking for it.
//
// This is the one path into image generation that does not go through a chat model. A recipe
// is data rather than a request in words, so nothing has to be asked of the model to use it,
// and the user gets the picture without paying for a turn. What it costs is real GPU time,
// which is why it is a control the user presses and not something the app does on its own.
//
// The recipe passed in is the whole of what gets drawn, not a patch over the stored one. A
// zero field is therefore "the server's own", exactly as it is in Settings and in the tool's
// own arguments — which is what makes an emptied box in the panel's editor mean what it
// looks like it means. The three controls are the same call: redrawing passes the stored
// recipe back unchanged, a variation passes it with the seed cleared, and the editor passes
// whatever the user typed over the prefilled fields.
//
// The source attachment's own recipe is never modified. A picture records how it was
// actually made, and an edit is a statement about the next draw rather than a correction of
// the last one — so the transcript keeps telling the truth about the picture above it.
func (s *Service) Regenerate(ctx context.Context, attachmentID string, recipe store.GenerationRecipe) (store.Attachment, error) {
	if s.Images == nil {
		return store.Attachment{}, images.ErrNotConfigured
	}
	if s.Store == nil {
		return store.Attachment{}, fmt.Errorf("kein Speicher verfügbar")
	}
	source, err := s.Store.GetAttachment(attachmentID)
	if err != nil {
		return store.Attachment{}, err
	}
	if source.Generation == nil {
		// An uploaded file, a scraped page, or a picture drawn before the recipe was
		// recorded. Naming the reason beats drawing something unrelated to it.
		return store.Attachment{}, fmt.Errorf(
			"%s hat kein Rezept: nur ein von KiChat erzeugtes Bild lässt sich erneut zeichnen",
			source.Filename)
	}
	if strings.TrimSpace(recipe.Prompt) == "" {
		// A picture with no prompt is a blank canvas: the server draws something arbitrary
		// and charges for it. The editor disables the button for this, but a request can
		// arrive from anywhere.
		return store.Attachment{}, errors.New("der Bild-Prompt ist leer")
	}
	req := requestFromRecipe(recipe)

	result, err := s.Images.Generate(ctx, req)
	if err != nil {
		return store.Attachment{}, err
	}
	if len(result.Images) == 0 {
		return store.Attachment{}, images.ErrNoModel
	}

	// What the *server* used, which becomes the new picture's own recipe — not the recipe
	// that was asked for, since only the server's seed reproduces the result and an editor
	// may well have left the seed open.
	effective := recipeFrom(req, result)
	// The names this chat already shows, so a second redraw of the same seed does not land
	// under the name the first one took.
	used, err := s.usedFilenames(source.ChatID)
	if err != nil {
		return store.Attachment{}, err
	}
	var first store.Attachment
	for i, img := range result.Images {
		name := uniqueName(used, redrawFilename(req.Prompt, effective.Seed, img.Ext, i))
		attachment, err := s.storeGeneratedImage(source.ChatID, source.MessageID, name, img, &effective)
		if err != nil {
			return store.Attachment{}, err
		}
		used[name] = true
		if i == 0 {
			first = attachment
		}
	}
	return first, nil
}

// InterruptDraw asks the image server to abandon the picture it is drawing.
//
// Two answers, and both are facts the caller needs to tell apart rather than collapse
// into one error: whether the provider can be asked at all (a server that cannot —
// stable-diffusion.cpp has no interrupt route — is not a failure, and holds nothing
// for a user to fix), and whether asking it failed (a 404 on a route the server does
// not have, or a connection that went away).
//
// The wait is not this call's business. The caller has already cancelled the draw's
// context, which is what ends it on every server; this only exists so the server does
// not spend the next few minutes drawing a picture nobody will see.
func (s *Service) InterruptDraw(ctx context.Context) (bool, error) {
	if s.Images == nil {
		return false, images.ErrNotConfigured
	}
	interrupter, ok := s.Images.(images.Interrupter)
	if !ok {
		return false, nil
	}
	return true, interrupter.Interrupt(ctx)
}

// usedFilenames is the set of names a chat's pictures already carry.
func (s *Service) usedFilenames(chatID string) (map[string]bool, error) {
	attachments, err := s.Store.ListAttachments(chatID)
	if err != nil {
		return nil, err
	}
	used := make(map[string]bool, len(attachments))
	for _, a := range attachments {
		used[a.Filename] = true
	}
	return used, nil
}

// uniqueName returns name, or the first numbered variant of it the chat does not already
// use. Two pictures under one name are two pictures a user cannot tell apart — in the
// caption under each one and in the name the save dialog offers — and redrawing the same
// recipe twice is an ordinary thing to do, not an edge case.
func uniqueName(used map[string]bool, name string) string {
	if !used[name] {
		return name
	}
	base, ext := name, ""
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		base, ext = name[:dot], name[dot:]
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d%s", base, n, ext)
		if !used[candidate] {
			return candidate
		}
	}
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

// redrawFilename names a redrawn picture so it is distinguishable from the one it redrew
// and from its own siblings.
//
// The plain name would repeat the first generation's exactly — same slug, same index — and
// two files under one name make both the caption under the picture and the save dialog
// ambiguous. The seed goes in because it is the number that reproduces a picture and the
// one thing that differs between a redraw and what it redrew; the index keeps several
// pictures from one redraw apart.
func redrawFilename(prompt string, seed int64, ext string, index int) string {
	name := imageFilename(prompt, index, ext)
	if seed == 0 {
		// No seed was reported, so there is no number to tell them apart by. It is still a
		// separate attachment with its own id.
		return name
	}
	dot := strings.LastIndex(name, ".")
	if dot < 0 {
		return name
	}
	return name[:dot] + fmt.Sprintf("-s%d", seed) + name[dot:]
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
