package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"uuid"

	"kichat/internal/config"
	"kichat/internal/images"
	"kichat/internal/ingest"
	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/uimsg"
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

// turnKey carries the backend and model that own the turn a tool call belongs to.
//
// It travels in the context for the same reason the message id does: the service is one
// value for the whole app and two chats can run a turn at once, so a field on the service
// would be a race rather than a setting. The caption request needs both — the provider is
// resolved from the backend id, and the model id is what the caption is written by.
type turnKey struct{}

// TurnRef names the backend and model a tool call is running on.
type TurnRef struct {
	BackendID string
	ModelID   string
	// Turn is the trace's number for the turn, carried so a tool call can be read
	// with the provider rounds around it. Empty outside a traced turn.
	Turn string
	// Sources is the turn's numbering of the web sources it has read, so a search
	// result continues the numbering of the search before it instead of restarting at
	// [1]. Nil is a call outside a turn, which keeps the old per-list numbering.
	Sources *SourceList
}

// WithTurn attaches the turn's backend and model to a tool call.
func WithTurn(ctx context.Context, turn TurnRef) context.Context {
	return context.WithValue(ctx, turnKey{}, turn)
}

// TurnFrom reads the turn a tool call belongs to, false when nobody attached one — which is
// what a service exercised without an engine sees.
func TurnFrom(ctx context.Context) (TurnRef, bool) {
	turn, ok := ctx.Value(turnKey{}).(TurnRef)
	return turn, ok && turn.BackendID != ""
}

// generateImageArgs is what the model may send. Every field but the prompt is
// optional and tolerates a string where a number belongs, which is what
// decodeLoose plus the `any` type is for: small local models emit "512" as often
// as 512, and refusing the call over that would waste a whole tool round.
//
// These are the *model's* choices, and they are what the drawing uses — unless the user has
// fixed the same value in Settings, which wins (see pinInt/pinString below). The split is the
// point of the whole mechanism: a model decides per picture, and a user decides once.
type generateImageArgs struct {
	Prompt         string `json:"prompt"`
	NegativePrompt string `json:"negative_prompt"`
	Width          any    `json:"width"`
	Height         any    `json:"height"`
	Steps          any    `json:"steps"`
	Seed           any    `json:"seed"`
	Sampler        string `json:"sampler"`
	// CfgScale is the guidance scale: how hard the prompt is pushed. A float, and so
	// the one `any` here that is not read as an int — `7.5` is a scale a model may
	// well ask for, and `looseInt` would take it for 7.
	CfgScale any `json:"cfg_scale"`
}

// generateImage draws the requested picture and files it in the chat.
func (s *Service) generateImage(ctx context.Context, chatID, argsJSON string) (string, error) {
	if s.Images == nil {
		return "", images.ErrNotConfigured
	}
	var args generateImageArgs
	if err := decodeLoose(argsJSON, &args); err != nil {
		return "", s.badArguments(err)
	}
	prompt := strings.TrimSpace(args.Prompt)
	if prompt == "" {
		// A picture with no prompt is not a degenerate case, it is a blank canvas:
		// the server would draw something arbitrary and charge for it.
		return "", uimsg.Detailf("tools.promptMissing", text(s.Language(),
			"das Argument prompt ist leer; ein Bild braucht eine Beschreibung",
			"the prompt argument is empty; a picture needs a description"))
	}

	cfg := s.imageSettings()
	req := images.Request{
		Prompt: prompt,
		// The draw's errors are read by the person and by the model, so they follow the
		// interface language like every other sentence in this package.
		Lang: s.Language(),
		// The model decides these, *except* where the user has fixed one in Settings — a value
		// typed there is an instruction, not a default, so it overrides what the model asked
		// for. An empty field on both sides means "send nothing", which is what leaves a
		// negative prompt configured in the user's own WebUI in force. Why the two directions
		// are not symmetrical is in pinInt/pinFloat/pinString below.
		NegativePrompt: pinString(cfg.NegativePrompt, args.NegativePrompt),
		Width:          pinInt(cfg.Width, args.Width),
		Height:         pinInt(cfg.Height, args.Height),
		Steps:          pinInt(cfg.Steps, args.Steps),
		Sampler:        pinString(cfg.Sampler, args.Sampler),
		CfgScale:       pinFloat(cfg.CfgScale, args.CfgScale),
		// The checkpoint is a setting rather than an argument: which models are
		// installed is a property of the machine, and a model that guessed a name
		// would get a silent fallback to whatever was already loaded. Which *field*
		// holds it depends on the kind — a hosted id is not a checkpoint — so the
		// answer comes from the settings rather than from this call site.
		Model: cfg.GenerationModel(),
		Seed:  seedFromValue(args.Seed),
		// The second pass is a *setting* and not an argument, for the same reason the
		// checkpoint is: it is a property of the machine (a CLI build either has
		// `--hires` or it does not, and a picture twice the size costs minutes), not a
		// choice about one picture that a model can see the hardware for.
		Hires: images.Hires{
			Enabled: cfg.HiresEnabled,
			Scale:   cfg.HiresScale,
			Denoise: cfg.HiresDenoise,
		},
	}
	// Bounded before anything is drawn *or recorded*. The recipe written below is built from
	// this request, and the schema this call answers to advertises 64–2048 and 1–150 — but a
	// model is free to invent a number, and the smallest local ones often do. A recipe that
	// named a size the provider quietly replaced would describe a picture that does not
	// exist, so the clamp happens here rather than only at the backend; see
	// images.Request.Normalised.
	req = req.Normalised()
	// The profile of the configured model fills what nobody chose and takes away what that
	// model cannot use. It runs after the clamp and before anything is drawn, so the recipe
	// records the numbers really used rather than the empty fields that produced them — and
	// so a FLUX 3 draw is 16 steps at CFG 1.0 without the user typing either.
	pipeline := images.PipelineFor(cfg.Pipeline, cfg.CLIDiffusionModel)
	var adjusted images.Adjustments
	req, adjusted = pipeline.Adjusted(req)

	// One picture at a time, and before anything is registered: a second draw under the same
	// message would otherwise cancel the first one's registration on its way to being refused.
	releaseSlot, err := s.takeDrawSlot()
	if err != nil {
		return "", err
	}
	defer releaseSlot()

	// The draw is registered under the message that asked for it, which is the row the view
	// renders the tool call on — so it is the id a Stop can be pressed next to. Without this
	// a draw the *model* started had no way of being stopped at all: every Stop in the window
	// lives under a picture, and there is no picture until the draw has finished.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	release := s.watchDraw(callingMessage(ctx), chatID, cancel)
	defer release()

	// The caption, before a single sampling step and after the draw is registered — so a
	// Stop pressed during it ends the draw before any GPU time is spent, which is the
	// cheapest cancel there is. A failed or empty caption does not fail the draw: the
	// request is drawn as typed and the tool result says the caption did not happen,
	// because refusing a picture the user asked for over a second model's stumble is the
	// worse answer.
	caption, captionErr := s.caption(ctx, chatID, pipeline.CaptioningMode(cfg.Captioning), req)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if captionErr == nil {
		req.Caption = caption.Text
	}

	result, err := s.draw(ctx, callingMessage(ctx), chatID, req)
	if err != nil {
		return "", err
	}

	// The recipe is recorded beside the picture, so "draw that again" needs no model turn
	// and no parsing of the prose summary below.
	recipe := recipeFrom(req, result, caption.Model)
	messageID := callingMessage(ctx)
	// The names this chat already shows, so a second draw of one prompt does not land under
	// the name the first one took. Redrawing has guarded this from the start (uniqueName);
	// this path did not, and it is the one a model drives — "draw that again" is a single
	// sentence to a model that will happily comply, and it produced two pictures under one
	// name, which are two pictures a user cannot tell apart in the caption or in the save
	// dialog that offers the same filename twice.
	used, err := s.usedFilenames(chatID)
	if err != nil {
		return "", uimsg.Detailf("tools.imageNamesUnreadable",
			fmt.Sprintf("the filenames already used in this chat could not be read: %v", err), err)
	}
	for i, img := range result.Kept() {
		name := uniqueName(used, imageFilename(prompt, i, img.Ext))
		if _, err := s.storeGeneratedImage(chatID, messageID, name, img, &recipe, nil); err != nil {
			// The picture is already drawn and paid for, so an error here loses it
			// entirely. Reporting the failure is still right: silently returning a
			// cheerful result would leave the user with a tool card claiming a
			// picture that exists nowhere.
			return "", uimsg.Detailf("tools.imageSaveFailed",
				fmt.Sprintf("the generated picture could not be saved: %v", err), err)
		}
		used[name] = true
	}

	return summarizeGeneration(s.Language(), req, result, caption, captionErr, pipeline, adjusted), nil
}

// caption asks the loaded model for the caption the backend will draw from, when the
// settings ask for one.
//
// Three things are deliberately not a caption: no callback (a service without an engine),
// the switch off, and a request that is already a caption. The last one is the drift guard —
// BFL's prompt asks for 100-350 words, so a request that long has been written by a model
// already, and expanding an expansion moves the picture away from what was asked for.
func (s *Service) caption(ctx context.Context, chatID, mode string, req images.Request) (images.CaptionResult, error) {
	if s.Caption == nil || mode != images.CaptioningAuto {
		return images.CaptionResult{}, nil
	}
	if images.IsCaption(req.Prompt) {
		return images.CaptionResult{}, nil
	}
	if s.Progress != nil {
		// Reported before the call rather than during it: the wait belongs to the model,
		// and a row that says so is what keeps seconds of silence from reading as a hang.
		s.Progress(callingMessage(ctx), chatID, images.Progress{Phase: images.PhaseCaptioning})
	}
	return s.Caption(ctx, images.CaptionRequest{
		Prompt: req.Prompt,
		Width:  req.Width,
		Height: req.Height,
	})
}

// summarizeGeneration is what the model reads back. It is written for the model,
// not for the user — the picture itself is in the transcript — and it has one job
// above all: stopping the model from describing what it drew. The chat model that
// called this tool has not seen the picture, and a reply that invents its contents
// reads as a broken feature rather than as an absent capability.
func summarizeGeneration(lang string, req images.Request, result images.Result, caption images.CaptionResult, captionErr error, pipeline images.Pipeline, adjusted images.Adjustments) string {
	var sb strings.Builder
	kept := len(result.Kept())
	fmt.Fprintf(&sb, text(lang, "%d Bild(er) wurden erzeugt und im Chat angezeigt.\n",
		"%d image(s) were created and shown in the chat.\n"), kept)
	if dropped := len(result.Images) - kept; dropped > 0 {
		// Said out loud rather than dropped in silence: the server drew them, the chat does
		// not show them, and a model that was told only about the kept ones would answer a
		// question about a batch it cannot see.
		fmt.Fprintf(&sb, text(lang,
			"Der Server hat %d Bild(er) mehr geliefert; sie wurden nicht übernommen.\n",
			"The server returned %d more image(s); they were not kept.\n"), dropped)
	}
	fmt.Fprintf(&sb, "Prompt: %s\n", req.Prompt)
	// Named by model and length rather than quoted: a 350-word caption is not something the
	// model needs to read back, and putting it in the result would replay it on every later
	// turn. What the model does need is to know that the picture was drawn from words
	// somebody else wrote, so it does not promise the user that it drew their sentence.
	if req.Caption != "" {
		fmt.Fprintf(&sb, text(lang,
			"Bildbeschreibung: von %s, %d Wörter — gezeichnet wurde die Beschreibung, nicht der "+
				"Prompt\n",
			"Caption: by %s, %d words — the caption was drawn, not the prompt\n"),
			firstNonEmptyString(caption.Model, text(lang, "dem geladenen Modell", "the loaded model")),
			images.WordCount(req.Caption))
	} else if captionErr != nil {
		fmt.Fprintf(&sb, text(lang,
			"Bildbeschreibung fehlgeschlagen (%s) — gezeichnet wurde der Prompt wie eingegeben\n",
			"Caption failed (%s) — the prompt was drawn as typed\n"), captionErr)
	}
	// What the profile removed is said out loud: a negative prompt that is sent and then
	// ignored looks like a broken setting, and a second pass that quietly did not run
	// looks like a worse picture for no reason. Both lines appear only when the request
	// really carried the field, because "this model has no negative prompt" is noise on a
	// draw that never asked for one.
	if adjusted.DroppedNegative {
		fmt.Fprintf(&sb, text(lang, "Negativ-Prompt: von %s nicht unterstützt — nicht gesendet\n",
			"Negative prompt: not supported by %s — not sent\n"), pipeline.Name)
	}
	if adjusted.DroppedHires {
		fmt.Fprintf(&sb, text(lang, "Zweite Stufe: für %s nicht freigegeben — nicht gezeichnet\n",
			"Second pass: not enabled for %s — not drawn\n"), pipeline.Name)
	}
	if req.NegativePrompt != "" {
		fmt.Fprintf(&sb, text(lang, "Negativ-Prompt: %s\n", "Negative prompt: %s\n"), req.NegativePrompt)
	}
	if req.Width > 0 && req.Height > 0 {
		fmt.Fprintf(&sb, text(lang, "Größe: %dx%d", "Size: %dx%d"), req.Width, req.Height)
		if req.Steps > 0 {
			fmt.Fprintf(&sb, text(lang, ", %d Schritte", ", %d steps"), req.Steps)
		}
		sb.WriteString("\n")
	}
	// The final size is what the model has to call the picture: the base size above is
	// where the first pass started, and a model told only that would describe a smaller
	// picture than the one in the transcript.
	if req.Hires.Enabled {
		outputWidth, outputHeight := req.OutputSize()
		fmt.Fprintf(&sb, text(lang, "Zweite Stufe: ×%s, Denoise %s", "Second pass: ×%s, denoise %s"),
			strconv.FormatFloat(req.Hires.Scale, 'f', -1, 64),
			strconv.FormatFloat(req.Hires.Denoise, 'f', -1, 64))
		if outputWidth > 0 && outputHeight > 0 {
			fmt.Fprintf(&sb, " → %dx%d", outputWidth, outputHeight)
		}
		sb.WriteString("\n")
	}
	// Reported for the same reason the size and the step count are: the values drawn with are
	// not necessarily the ones the model asked for — a setting the user fixed wins — and a
	// model that describes the picture it just made has to be able to name what was really used.
	if req.Sampler != "" {
		fmt.Fprintf(&sb, "Sampler: %s\n", req.Sampler)
	}
	// The guidance scale for the same reason, and with one more purpose: two pictures of one
	// prompt that look different are most often a different scale, so a model asked to redraw
	// something has to be able to see the one that was used.
	if req.CfgScale > 0 {
		fmt.Fprintf(&sb, text(lang, "CFG-Scale: %s\n", "CFG scale: %s\n"),
			strconv.FormatFloat(req.CfgScale, 'f', -1, 64))
	}
	// The seed is the one number worth reporting even when the request left it
	// open: the server chose one, and quoting it is what lets the user ask for the
	// same picture again.
	if result.Seed != 0 {
		fmt.Fprintf(&sb, "Seed: %d\n", result.Seed)
	}
	if result.Model != "" {
		fmt.Fprintf(&sb, text(lang, "Modell: %s\n", "Model: %s\n"), result.Model)
	}
	sb.WriteString(text(lang,
		"\nDu siehst das Bild selbst NICHT und darfst seinen Inhalt nicht beschreiben. Sage kurz, "+
			"dass es erzeugt wurde, und nenne bei Bedarf den Prompt oder den Seed. Wenn der Nutzer "+
			"das Bild ändern will, rufe generate_image mit einem angepassten Prompt erneut auf.",
		"\nYou do NOT see the image yourself and must not describe its contents. Say briefly that "+
			"it was created, and name the prompt or the seed if needed. If the user wants to change "+
			"the image, call generate_image again with an adjusted prompt."))
	return sb.String()
}

// pinInt is a value the user fixed in Settings, or — when they fixed nothing — what the model
// asked for. Zero means "nobody made this decision", which is the same reading an empty field
// has in the request builder: nothing is sent, and the server's own default applies.
//
// The direction is deliberately the opposite of what a "default" would do. A model choosing
// 8 steps for a sketch and 40 for a portrait is the feature working; a model *overriding* the
// step count a user typed in Settings is the user's own instruction being ignored by
// something that cannot see the machine, the model or the licence they paid for. So the
// argument wins only where the field is empty, and a recipe records what was really drawn
// rather than what was asked for either way.
func pinInt(pinned int, asked any) int {
	if pinned > 0 {
		return pinned
	}
	return looseInt(asked, 0)
}

// pinFloat is pinInt for the guidance scale, the one field here that is not a whole number:
// a scale fixed in Settings wins, an empty field leaves it to the model, and `7.5` is read
// as the float it is rather than truncated.
func pinFloat(pinned float64, asked any) float64 {
	if pinned > 0 {
		return pinned
	}
	return looseFloat(asked, 0)
}

// pinString is pinInt for the two text fields: a configured value is fixed, an empty one
// leaves the choice to the model.
func pinString(pinned, asked string) string {
	if trimmed := strings.TrimSpace(pinned); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(asked)
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
// Reusing that shape is the whole point: the frontend renders a message's
// attachments, and an inline attachment of a *user* turn still reaches the request
// as an image part. A generated picture belongs to the assistant turn that asked
// for it, and no image part may travel there — the OpenAI-compatible schema has
// none, and a backend that validates the shape refuses the whole request
// (GROQ.md §6). The model reads its own call and the tool result instead; the
// picture is stored, shown, saved and redrawn from the transcript.
func (s *Service) storeGeneratedImage(chatID, messageID, filename string, img images.Image, recipe *store.GenerationRecipe, derivation *store.Derivation) (store.Attachment, error) {
	if s.Store == nil {
		return store.Attachment{}, uimsg.Detailf("tools.noStore", "no storage is available, so the picture cannot be filed")
	}
	now := time.Now()
	// The estimate is the app's shared one for an image payload, so the context
	// meter prices a generated picture exactly as it prices an attached one.
	tokens := llm.EstimateImageTokens(1)
	attachment := store.Attachment{
		ID:            uuid.New().String(),
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
		// Nil for a picture that was not enlarged from another one. The two fields are
		// exclusive in practice: an upscale has provenance, a draw has a prompt.
		Derivation: derivation,
	}
	document := store.Document{
		ID:           uuid.New().String(),
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
// The guidance scale is recorded as the request carries it, after the clamp and after a
// Settings pin won: it is a number this app sent, and no backend reports one back that would
// disagree. Only the server's *own* fallback — a `txt_cfg` living in the model file — is
// never seen, and a recipe claiming a number there would be inventing one.
//
// The effective value wins wherever the request and the answer disagree. A request that
// left the seed open was given one by the server, and only that number reproduces the
// picture — the requested value was "none". The checkpoint follows the same rule, because
// the server knows which one it really loaded, and a recipe that named the wrong one would
// draw something else while claiming to repeat.
func recipeFrom(req images.Request, result images.Result, captionModel string) store.GenerationRecipe {
	recipe := store.GenerationRecipe{
		// The effective prompt is what reproduces the picture: with a caption, that is the
		// caption. The user's own words are recorded beside it so the card can say what was
		// asked for, and a redraw replays the caption at no extra model call.
		Prompt:         req.EffectivePrompt(),
		NegativePrompt: req.NegativePrompt,
		Width:          req.Width,
		Height:         req.Height,
		Steps:          req.Steps,
		Sampler:        req.Sampler,
		CfgScale:       req.CfgScale,
		Model:          firstNonEmptyString(result.Model, req.Model),
		DurationMs:     result.DurationMs,
	}
	if req.Caption != "" {
		recipe.SourcePrompt = req.Prompt
		recipe.CaptionModel = captionModel
	}
	if req.Hires.Enabled {
		recipe.Hires = true
		recipe.HiresScale = req.Hires.Scale
		recipe.HiresDenoise = req.Hires.Denoise
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
func requestFromRecipe(recipe store.GenerationRecipe, lang string) images.Request {
	req := images.Request{
		Lang:           lang,
		Prompt:         recipe.Prompt,
		NegativePrompt: recipe.NegativePrompt,
		Width:          recipe.Width,
		Height:         recipe.Height,
		Steps:          recipe.Steps,
		Sampler:        recipe.Sampler,
		CfgScale:       recipe.CfgScale,
		Model:          recipe.Model,
		Hires: images.Hires{
			Enabled: recipe.Hires,
			Scale:   recipe.HiresScale,
			Denoise: recipe.HiresDenoise,
		},
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
		return store.Attachment{}, uimsg.Detailf("tools.noStore", "no storage is available, so the picture cannot be redrawn")
	}
	source, err := s.Store.GetAttachment(attachmentID)
	if err != nil {
		return store.Attachment{}, err
	}
	if source.Generation == nil {
		// An uploaded file, a scraped page, or a picture drawn before the recipe was
		// recorded. Naming the reason beats drawing something unrelated to it.
		return store.Attachment{}, uimsg.Detailf("tools.noRecipe",
			fmt.Sprintf("%s has no recipe; only a picture KiChat drew itself can be redrawn", source.Filename),
			source.Filename)
	}
	if strings.TrimSpace(recipe.Prompt) == "" {
		// A picture with no prompt is a blank canvas: the server draws something arbitrary
		// and charges for it. The editor disables the button for this, but a request can
		// arrive from anywhere.
		return store.Attachment{}, uimsg.Detailf("tools.emptyPrompt", "the stored recipe has an empty prompt")
	}
	req := requestFromRecipe(recipe, s.Language())
	// A redraw is drawn by the pipeline too: a stored recipe that asked for a second pass
	// under a model that has none would otherwise draw it once here and never again.
	pipeline := images.PipelineFor(s.imageSettings().Pipeline, s.imageSettings().CLIDiffusionModel)
	req, _ = pipeline.Adjusted(req)

	// This draw is *not* registered here, and that is deliberate: its caller is the bound
	// RegenerateImage, which has already registered it under this same attachment. Both
	// registering would mean the second registration cancelling the first — the newest
	// recipe wins, by design — and the first holds the very context this call is running
	// under, so the redraw would cancel itself the moment it started. The id is the
	// attachment because that is what the view's own controls sit under.
	result, err := s.draw(ctx, attachmentID, source.ChatID, req)
	if err != nil {
		return store.Attachment{}, err
	}
	if len(result.Images) == 0 {
		return store.Attachment{}, images.ErrNoModel
	}

	// What the *server* used, which becomes the new picture's own recipe — not the recipe
	// that was asked for, since only the server's seed reproduces the result and an editor
	// may well have left the seed open.
	// No caption here: a redraw replays the stored prompt, which for a captioned picture is
	// the caption itself. Asking for a new one would expand an expansion on every redraw.
	effective := recipeFrom(req, result, "")
	// The names this chat already shows, so a second redraw of the same seed does not land
	// under the name the first one took.
	used, err := s.usedFilenames(source.ChatID)
	if err != nil {
		return store.Attachment{}, err
	}
	var first store.Attachment
	for i, img := range result.Kept() {
		name := uniqueName(used, redrawFilename(req.Prompt, effective.Seed, img.Ext, i))
		attachment, err := s.storeGeneratedImage(source.ChatID, source.MessageID, name, img, &effective, nil)
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

// takeDrawSlot takes the app-wide draw slot when one is wired and reports the busy sentinel
// when another picture is already being drawn.
//
// One picture at a time is a property of the hardware rather than politeness: every backend
// here draws on the same GPU, the two HTTP kinds queue (or refuse) a second request behind
// the first, and the CLI kind pays a *model load* per draw — two at once is the same
// checkpoint in memory twice, which turns a working setup into an out-of-memory failure. It
// also makes Stop unambiguous: with two draws under way, an interrupt meant for one of them
// is sent to whichever the server happens to be working on.
//
// Nil means nothing is coordinating — a service built without an app, which is what the tests
// do — so the draw runs as it always did rather than waiting for a slot nobody owns.
func (s *Service) takeDrawSlot() (func(), error) {
	if s.TryDraw == nil {
		return func() {}, nil
	}
	release, ok := s.TryDraw()
	if !ok || release == nil {
		return nil, images.ErrDrawInProgress
	}
	return release, nil
}

// watchDraw registers a draw that is about to start, when somebody is watching. The
// returned function is never nil, so a caller can defer it without a condition.
func (s *Service) watchDraw(id, chatID string, cancel context.CancelFunc) func() {
	if s.Draws == nil || id == "" {
		return func() {}
	}
	release := s.Draws(id, chatID, cancel)
	if release == nil {
		return func() {}
	}
	// Announce the draw at once, before the backend has anything of its own to say.
	// The window keys the row — and with it the Stop button — off the draw id, and
	// only a report creates that row; a backend that cannot report progress (an
	// sd-cli build without `--preview`, a server without the progress route) would
	// otherwise leave a model-started draw unstoppable for its whole length even
	// though it is registered and cancellable. This zero report is that first
	// report, and it is also what makes the release emit its `done`.
	if s.Progress != nil {
		s.Progress(id, chatID, images.Progress{})
	}
	return release
}

// draw runs one generation, reporting it while it runs when the backend can and somebody
// asked to hear about it.
//
// A backend that cannot report is not a broken backend — the interface is optional for the
// same reason the interrupt one is — so this falls back to a plain Generate and the draw is
// simply silent. Nothing about the result depends on the reports having happened.
func (s *Service) draw(ctx context.Context, id, chatID string, req images.Request) (images.Result, error) {
	if err := ctx.Err(); err != nil {
		return images.Result{}, err
	}
	start := time.Now()
	var result images.Result
	var err error
	if progressive, ok := s.Images.(images.Progressive); ok && s.Progress != nil && id != "" {
		result, err = progressive.GenerateWithProgress(ctx, req, func(p images.Progress) {
			s.Progress(id, chatID, p)
		})
	} else {
		result, err = s.Images.Generate(ctx, req)
	}
	// Measured here rather than in the backends so both a tool draw and a redraw record
	// the same thing: the wall time a user waited, which is what a recipe can promise
	// about the next draw. A failed draw's number is dropped with its result.
	result.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return images.Result{}, err
	}
	if len(result.Images) == 0 {
		return images.Result{}, images.ErrNoModel
	}
	for _, image := range result.Kept() {
		if len(image.Data) == 0 {
			return images.Result{}, uimsg.Detailf("tools.emptyImage", "the image backend returned an empty picture")
		}
	}
	if err := verifyHiresOutput(req, result); err != nil {
		return result, err
	}
	return result, nil
}

// widestHiresAlignment is the coarsest spatial grid stable-diffusion.cpp aligns a target to:
// `GenerationRequest::align_image_size` uses `vae_scale_factor * diffusion_model_down_factor`,
// and the widest image case is a VAE factor of 8 with a down factor of 32 (256). A1111 only
// pads to its latent stride of 8. The app's own `OutputSize` is an 8-grid prediction, so a
// backend may return a size up to one grid step above it — 656 can come back as 704 from
// sd.cpp's UNet path (8 × 8) — and that is alignment, not a failed second pass.
const widestHiresAlignment = 256

// verifyHiresOutput answers "did the second pass actually happen?" from the picture that came
// back rather than from the request that went out.
//
// It has to be asked. The A1111 route accepts `enable_hr` without saying whether it honoured
// it — a server built without hires support, or one whose own settings disagree, answers 200
// with a base-size picture — and filing that under a recipe that claims two passes would tell
// the next redraw to reproduce something that was never drawn. The size is read from the PNG
// header, so the check costs no decode; formats Go cannot read (WebP) leave the question
// unanswered rather than failed, because refusing a picture over the format it arrived in
// would be a worse answer than trusting the server once.
//
// The requested target is a lower bound, not an equality: `OutputSize` is the app's 8-grid
// prediction, while sd.cpp aligns the target again to a grid as coarse as 256. The upper bound
// is that same prediction rounded up to the widest grid, so a server that ignored the target
// and used a much larger scale is still refused. A base-size answer is below the lower bound
// for every allowed scale (the minimum is 1.25), which is the failure this check exists for.
//
// The error names the expected range and the delivered size because that is the diagnosis,
// and wraps ErrHiresUnsupported so a caller can tell this apart from a server that broke.
func verifyHiresOutput(req images.Request, result images.Result) error {
	if !req.Hires.Enabled {
		return nil
	}
	wantWidth, wantHeight := req.OutputSize()
	if wantWidth <= 0 || wantHeight <= 0 {
		// The base size was the server's own, so there is no product to compare against.
		return nil
	}
	maxWidth := (wantWidth + widestHiresAlignment - 1) / widestHiresAlignment * widestHiresAlignment
	maxHeight := (wantHeight + widestHiresAlignment - 1) / widestHiresAlignment * widestHiresAlignment
	for _, img := range result.Kept() {
		width, height, ok := images.Size(img.Data)
		if !ok {
			continue
		}
		if width < wantWidth || height < wantHeight || width > maxWidth || height > maxHeight {
			return uimsg.Wrapf("tools.hiresSize",
				fmt.Sprintf("the second drawing pass returned %dx%d instead of %dx%d to %dx%d",
					width, height, wantWidth, wantHeight, maxWidth, maxHeight),
				images.ErrHiresUnsupported, width, height, wantWidth, wantHeight, maxWidth, maxHeight)
		}
	}
	return nil
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
