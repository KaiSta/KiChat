package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/images"
	"kichat/internal/store"
)

// pictureBytes stands in for a PNG. It is not valid base64 itself, so a decode
// that silently returned its input would fail the round-trip assertion rather
// than pass it.
var pictureBytes = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x7f, 0x3c}

type fakeImageProvider struct {
	got    images.Request
	result images.Result
	err    error
	calls  int
	// delay makes a draw take measurable time, which is the only way a test can tell
	// that DurationMs was measured by the caller rather than left at zero.
	delay time.Duration
}

func (f *fakeImageProvider) ID() string                   { return "fake" }
func (f *fakeImageProvider) Name() string                 { return "Fake SD" }
func (f *fakeImageProvider) Health(context.Context) error { return nil }
func (f *fakeImageProvider) Models(context.Context) ([]images.Model, error) {
	return nil, nil
}
func (f *fakeImageProvider) Samplers(context.Context) ([]string, error) { return nil, nil }
func (f *fakeImageProvider) Generate(_ context.Context, req images.Request) (images.Result, error) {
	f.calls++
	f.got = req
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.err != nil {
		return images.Result{}, f.err
	}
	return f.result, nil
}

// newImageService builds a tool service over a real store in a temp directory,
// which is the only way to check what actually landed in the attachments table.
func newImageService(t *testing.T, provider images.Provider, cfg config.ImageSettings) (*Service, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	chatID := "chat-1"
	if err := st.CreateChat(store.Chat{ID: chatID, Title: "Bilder", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	settings := config.Settings{Images: cfg, Language: config.LangGerman}
	service := &Service{
		Store:    st,
		Images:   provider,
		Settings: func() config.Settings { return settings },
	}
	return service, st, chatID
}

func onePicture() images.Result {
	mime, ext := images.SniffImage(pictureBytes, images.MimePNG, "png")
	return images.Result{
		Images: []images.Image{{Data: pictureBytes, Mime: mime, Ext: ext}},
		Seed:   4242,
		Model:  "sdxl.safetensors",
		Info:   `{"seed":4242}`,
	}
}

// TestGenerateImageFilesThePictureOnTheCallingMessage is the whole feature in one
// test: the picture has to end up as an inline image attachment of the assistant
// message that asked for it, because that is what the frontend renders and what a
// redraw reads the recipe from. It is not sent back to the model: an image part
// cannot travel on an assistant turn (GROQ.md §6).
func TestGenerateImageFilesThePictureOnTheCallingMessage(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{
		Width: 768, Height: 512, Steps: 25, Sampler: "Euler a",
	})

	ctx := WithCallingMessage(context.Background(), "msg-1")
	out, err := service.Execute(ctx, chatID, ToolGenerateImage,
		`{"prompt":"a red fox in deep snow"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
	// A value fixed in Settings is what gets drawn, whether or not the model said anything
	// about that field — there is no other source for these numbers here, since the call
	// carried a prompt and nothing else.
	if provider.got.Width != 768 || provider.got.Height != 512 || provider.got.Steps != 25 {
		t.Errorf("settings were not applied: %+v", provider.got)
	}
	if provider.got.Sampler != "Euler a" {
		t.Errorf("sampler = %q, want the configured one", provider.got.Sampler)
	}

	attachments, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(attachments))
	}
	got := attachments[0]
	if got.MessageID != "msg-1" {
		t.Errorf("messageId = %q, want msg-1: an unlinked attachment is rendered nowhere", got.MessageID)
	}
	if got.Kind != "image" {
		t.Errorf("kind = %q, want image", got.Kind)
	}
	if got.Mime != "image/png" {
		t.Errorf("mime = %q, want image/png", got.Mime)
	}
	if got.SizeBytes != int64(len(pictureBytes)) {
		t.Errorf("sizeBytes = %d, want %d", got.SizeBytes, len(pictureBytes))
	}
	if got.Placement != store.PlacementInline {
		t.Errorf("placement = %q, want inline — a picture is not indexed text", got.Placement)
	}
	if got.Filename != "a-red-fox-in-deep-snow-1.png" {
		t.Errorf("filename = %q, want the prompt as a slug", got.Filename)
	}

	// The payload has to survive as base64 in the document, which is the shape
	// an attachment's payload is stored in.
	document, err := st.GetDocument(got.DocumentID)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(document.Text)
	if err != nil {
		t.Fatalf("document text is not base64: %v", err)
	}
	if string(decoded) != string(pictureBytes) {
		t.Errorf("stored payload = %v, want the generated bytes", decoded)
	}

	// The model that called this tool has not seen the picture. A result that
	// invites it to describe what it drew produces an invented description, which
	// reads as a broken feature rather than an absent one.
	if !strings.Contains(out, "nicht beschreiben") {
		t.Errorf("the tool result does not forbid describing the picture:\n%s", out)
	}
	if !strings.Contains(out, "Seed: 4242") {
		t.Errorf("the seed is missing, so the picture cannot be reproduced:\n%s", out)
	}
}

// A server can hand back more pictures than one message should ever show. The client asks for
// one (`n: 1`, see sdRequest.N), but a build that ignores that key exists, and a WebUI left on
// a 4×4 batch answers one call with sixteen pictures — sixteen attachments no control in the
// window was built for, and sixteen pictures already paid for in GPU seconds. The first four
// are kept and the rest is *said out loud*, because a model told only about the kept ones would
// answer questions about a batch it cannot see.
func TestOneAnswerFilesAtMostTheBound(t *testing.T) {
	mime, ext := images.SniffImage(pictureBytes, images.MimePNG, "png")
	const extra = 2
	result := images.Result{Seed: 4242, Model: "sdxl.safetensors", Info: `{"seed":4242}`}
	for i := 0; i < images.MaxPicturesPerDraw+extra; i++ {
		result.Images = append(result.Images, images.Image{Data: pictureBytes, Mime: mime, Ext: ext})
	}
	provider := &fakeImageProvider{result: result}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{})

	out, err := service.Execute(WithCallingMessage(context.Background(), "msg-1"), chatID,
		ToolGenerateImage, `{"prompt":"a batch"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	attachments, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(attachments) != images.MaxPicturesPerDraw {
		t.Fatalf("attachments = %d, want the bound of %d", len(attachments), images.MaxPicturesPerDraw)
	}
	if want := fmt.Sprintf("%d Bild(er) wurden erzeugt", images.MaxPicturesPerDraw); !strings.Contains(out, want) {
		t.Errorf("the model was not told how many pictures the chat now shows:\n%s", out)
	}
	if want := fmt.Sprintf("%d Bild(er) mehr", extra); !strings.Contains(out, want) {
		t.Errorf("the pictures that were not filed were dropped in silence:\n%s", out)
	}
}

// One draw at a time, and the refusal is a sentence rather than a second process. The service
// asks the app's slot before it registers anything — that ordering is what keeps a refused
// draw from cancelling the running one's registration — and the slot has to come back out
// however the draw ended, or one failure would wedge the feature for the rest of the session.
func TestASecondDrawIsRefusedWhileOneHoldsTheSlot(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, _, chatID := newImageService(t, provider, config.ImageSettings{})
	busy := false
	service.TryDraw = func() (func(), bool) {
		if busy {
			return nil, false
		}
		busy = true
		return func() { busy = false }, true
	}

	ctx := WithCallingMessage(context.Background(), "msg-1")
	if _, err := service.Execute(ctx, chatID, ToolGenerateImage, `{"prompt":"a fox"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if busy {
		t.Fatal("the slot was still held after the draw finished")
	}

	// A failed draw releases it too, which is the half a release guarded by success would get
	// wrong: one blown request would leave the feature refusing every later picture.
	provider.err = errors.New("the server is on fire")
	if _, err := service.Execute(ctx, chatID, ToolGenerateImage, `{"prompt":"a fox"}`); !errors.Is(err, provider.err) {
		t.Fatalf("err = %v, want the provider's own failure", err)
	}
	if busy {
		t.Fatal("a failed draw left the slot held")
	}

	// Somebody else is drawing: the call is refused, and the provider is never asked.
	busy = true
	if _, err := service.Execute(ctx, chatID, ToolGenerateImage, `{"prompt":"a fox"}`); !errors.Is(err, images.ErrDrawInProgress) {
		t.Fatalf("err = %v, want ErrDrawInProgress", err)
	}
	if provider.calls != 2 {
		t.Errorf("provider calls = %d, want 2: a refused draw must not reach the server", provider.calls)
	}
}

// The rule this feature is built on, in the direction it is not obvious from: a value the user
// fixed in Settings wins over the model's argument. A model deciding 8 steps for a sketch and
// 40 for a portrait is the feature working; a model overruling the step count, size or sampler
// someone typed in Settings is an instruction being ignored by something that cannot see the
// machine, the model or the licence it was bought with.
func TestASettingTheUserFixedBeatsTheModelsArgument(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, _, chatID := newImageService(t, provider, config.ImageSettings{
		Width: 768, Height: 512, Steps: 25, Sampler: "Euler a", CfgScale: 4.5, NegativePrompt: "blurry, text",
	})

	if _, err := service.Execute(context.Background(), chatID, ToolGenerateImage,
		`{"prompt":"a fox","width":1024,"height":1024,"steps":8,"sampler":"DDIM","cfg_scale":12,"negative_prompt":"extra fingers"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.got.Width != 768 || provider.got.Height != 512 {
		t.Errorf("size = %dx%d, want the fixed 768x512", provider.got.Width, provider.got.Height)
	}
	if provider.got.Steps != 25 {
		t.Errorf("steps = %d, want the fixed 25", provider.got.Steps)
	}
	if provider.got.Sampler != "Euler a" {
		t.Errorf("sampler = %q, want the fixed one", provider.got.Sampler)
	}
	if provider.got.CfgScale != 4.5 {
		t.Errorf("cfg scale = %v, want the fixed 4.5", provider.got.CfgScale)
	}
	if provider.got.NegativePrompt != "blurry, text" {
		t.Errorf("negative = %q, want the fixed one", provider.got.NegativePrompt)
	}
}

// And the other direction, which is the one a user meets by default: an empty field is the
// model's to decide, so its numbers and its words are what reach the provider. A test of the
// pinning half alone would also pass for a builder that dropped the model's arguments entirely.
func TestTheModelDecidesEveryFieldTheUserLeftEmpty(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, _, chatID := newImageService(t, provider, config.ImageSettings{})

	if _, err := service.Execute(context.Background(), chatID, ToolGenerateImage,
		`{"prompt":"a fox","width":"640","height":384,"steps":12,"sampler":"euler_a","cfg_scale":7.5,"negative_prompt":"extra fingers"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.got.Width != 640 || provider.got.Height != 384 || provider.got.Steps != 12 {
		t.Errorf("the model's numbers were not used: %+v", provider.got)
	}
	// The guidance scale is the one number here that is *not* whole, and it is the reason
	// the arguments are decoded loosely at all: `7.5` reaching the provider as `7` would
	// draw a different picture than the one the model asked for and the recipe records.
	if provider.got.CfgScale != 7.5 {
		t.Errorf("cfg scale = %v, want the model's own 7.5", provider.got.CfgScale)
	}
	if provider.got.Sampler != "euler_a" || provider.got.NegativePrompt != "extra fingers" {
		t.Errorf("the model's own arguments were not used: %+v", provider.got)
	}
}

// A fixed field does not take the others with it. The two sides are per field rather than per
// kind of value, which is what lets a user pin a size and leave the step count to the model.
func TestAFixedFieldLeavesTheRestToTheModel(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, _, chatID := newImageService(t, provider, config.ImageSettings{Steps: 30})

	if _, err := service.Execute(context.Background(), chatID, ToolGenerateImage,
		`{"prompt":"a fox","width":640,"height":896,"steps":4,"sampler":"euler","cfg_scale":2,"negative_prompt":"blurry"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.got.Steps != 30 {
		t.Errorf("steps = %d, want the fixed 30", provider.got.Steps)
	}
	if provider.got.CfgScale != 2 {
		t.Errorf("cfg scale = %v, want the model's own 2", provider.got.CfgScale)
	}
	if provider.got.Width != 640 || provider.got.Height != 896 || provider.got.Sampler != "euler" || provider.got.NegativePrompt != "blurry" {
		t.Errorf("the fields the user left empty were not the model's: %+v", provider.got)
	}
}

// A model that emits "512" where a number belongs must not cost a tool round:
// small local models do it constantly, which is what decodeLoose is for.
func TestGenerateImageToleratesNumbersAsStrings(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, _, chatID := newImageService(t, provider, config.ImageSettings{})

	if _, err := service.Execute(context.Background(), chatID, ToolGenerateImage,
		`{"prompt":"a fox","width":"640","steps":"30","cfg_scale":"5.5","seed":"7"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.got.Width != 640 || provider.got.Steps != 30 {
		t.Errorf("string numbers were dropped: %+v", provider.got)
	}
	// A string that parses as a float rather than as an int: `Atoi` would return nothing for
	// "5.5" and the scale would silently be the backend's own.
	if provider.got.CfgScale != 5.5 {
		t.Errorf("cfg scale = %v, want the string's own 5.5", provider.got.CfgScale)
	}
	if provider.got.Seed == nil || *provider.got.Seed != 7 {
		t.Errorf("seed = %v, want 7", provider.got.Seed)
	}
}

func TestGenerateImageRefusesAnEmptyPrompt(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{})

	if _, err := service.Execute(context.Background(), chatID, ToolGenerateImage,
		`{"prompt":"   "}`); err == nil {
		t.Fatal("an empty prompt was accepted; the server would draw something arbitrary")
	}
	if provider.calls != 0 {
		t.Error("a generation was attempted with no prompt")
	}
	attachments, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(attachments) != 0 {
		t.Errorf("attachments = %d, want none", len(attachments))
	}
}

func TestGenerateImageRequiresAConfiguredServer(t *testing.T) {
	service, _, chatID := newImageService(t, nil, config.ImageSettings{})
	_, err := service.Execute(context.Background(), chatID, ToolGenerateImage, `{"prompt":"x"}`)
	if !errors.Is(err, images.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

// A server failure must not leave a half-written attachment behind: the picture
// was never produced, so a row claiming one would render a broken image.
func TestGenerateImageReportsAServerFailureAndWritesNothing(t *testing.T) {
	provider := &fakeImageProvider{err: errors.New("kein Modell geladen")}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{})

	if _, err := service.Execute(context.Background(), chatID, ToolGenerateImage,
		`{"prompt":"a fox"}`); err == nil {
		t.Fatal("a server failure was reported as success")
	}
	attachments, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(attachments) != 0 {
		t.Errorf("attachments = %d, want none after a failed generation", len(attachments))
	}
	documents, err := st.ListDocuments(chatID)
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(documents) != 0 {
		t.Errorf("documents = %d, want none: the payload was written for a picture that does not exist", len(documents))
	}
}

func TestGenerateImageIsAdvertised(t *testing.T) {
	service := &Service{}
	found := false
	for _, def := range service.Definitions() {
		if def.Name == ToolGenerateImage {
			found = true
		}
	}
	if !found {
		t.Fatal("generate_image is not among the tool definitions")
	}
	// The unconfigured case has to be answerable without a server, because the
	// engine filters on the provider being nil.
	if _, err := service.Execute(context.Background(), "c", ToolGenerateImage, `{"prompt":"x"}`); err == nil {
		t.Error("an unconfigured service answered a generation request")
	}
}

// TestTheImageToolAsksForAnExplicitRequest is the model-facing half of the
// feature, and it is asserted rather than assumed because it is the whole of what
// stops the tool being used for things it cannot draw.
//
// Measured before this contract existed: two small local models answered
// "Zeichne ein UML-Klassendiagramm für eine Bibliothek" with a generate_image
// call, because the description said the tool is for "ein Bild, eine Illustration,
// eine Grafik oder ein Foto" and never said that only an explicit request is a
// reason to draw. The permissive sentence is asserted absent so an edit cannot
// quietly restore it; the three places a model reads the tool — the native
// definition, the text-protocol hint, and the native system-prompt rule — are
// each checked for the restriction, since a model on one path never sees the
// other two.
func TestTheImageToolAsksForAnExplicitRequest(t *testing.T) {
	var description string
	for _, def := range inGerman(&Service{}).Definitions() {
		if def.Name == ToolGenerateImage {
			description = def.Description
		}
	}
	if description == "" {
		t.Fatal("generate_image has no description")
	}

	allowed := "das Werkzeug darf auf eine ausdrückliche Bitte hin zeichnen"
	forbidden := "ohne ausdrückliche Bitte darf es nicht von selbst zeichnen, und Diagramme gehören in Text"
	texts := []struct {
		name string
		text string
	}{
		{name: "native description", text: description},
		{name: "text protocol hint", text: ImageProtocolHint(config.LangGerman)},
		{name: "native system rule", text: ImageUseHint(config.LangGerman)},
	}
	for _, tc := range texts {
		if !strings.Contains(tc.text, "ausdrücklich") {
			t.Errorf("%s does not require an explicit request (%s):\n%s", tc.name, allowed, tc.text)
		}
		if !strings.Contains(tc.text, "Diagramm") {
			t.Errorf("%s does not send diagrams to text (%s):\n%s", tc.name, forbidden, tc.text)
		}
	}
	// The native description is the one that can also be read as a licence: it
	// has to rule out an unprompted call in words, not only require a request.
	if !strings.Contains(description, "nicht von dir aus") {
		t.Errorf("the description does not forbid drawing unprompted (%s):\n%s", forbidden, description)
	}
	if strings.Contains(description, "haben möchte") {
		t.Errorf("the permissive \"haben möchte\" wording is back, which is what made a diagram prompt draw a picture:\n%s",
			description)
	}
}

func TestImageFilenameSlug(t *testing.T) {
	cases := []struct {
		prompt string
		want   string
		why    string
	}{
		{"a red fox in deep snow", "a-red-fox-in-deep-snow-1.png", "the words survive"},
		{"  spaced   out  ", "spaced-out-1.png", "runs of punctuation collapse to one dash"},
		{"Füchse im Schnee", "füchse-im-schnee-1.png", "umlauts are letters, not noise"},
		{"一只红色的狐狸", "一只红色的狐狸-1.png", "a Chinese prompt is a normal prompt here"},
		{"!!!", "bild-1.png", "a prompt with nothing usable still needs a name"},
	}
	for _, tc := range cases {
		if got := imageFilename(tc.prompt, 0, "png"); got != tc.want {
			t.Errorf("imageFilename(%q) = %q, want %q (%s)", tc.prompt, got, tc.want, tc.why)
		}
	}
	// A 300-character prompt must not become a 300-character filename.
	long := imageFilename(strings.Repeat("wort ", 80), 0, "png")
	if len([]rune(long)) > 60 {
		t.Errorf("filename is %d runes long, want it bounded", len([]rune(long)))
	}
	if !strings.HasSuffix(long, "-1.png") {
		t.Errorf("truncation ate the index: %q", long)
	}
	// The index is what keeps several pictures from one prompt apart.
	if got := imageFilename("a fox", 1, "png"); got != "a-fox-2.png" {
		t.Errorf("second picture named %q", got)
	}
	// The extension follows the payload: a WebUI configured for JPEG must not
	// produce a JPEG called .png, which is a file whose type and name disagree.
	if got := imageFilename("a fox", 0, "jpg"); got != "a-fox-1.jpg" {
		t.Errorf("jpeg picture named %q", got)
	}
	// An empty extension falls back to what the route returns by default.
	if got := imageFilename("a fox", 0, ""); got != "a-fox-1.png" {
		t.Errorf("extension-less picture named %q", got)
	}
}

// TestSniffImage covers the format the server actually sent, which is not a
// constant: the WebUI returns whatever its `samples_format` says.
func TestSniffImage(t *testing.T) {
	cases := []struct {
		name     string
		data     []byte
		wantMime string
		wantExt  string
	}{
		{"png", pictureBytes, "image/png", "png"},
		{"jpeg", []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F'}, "image/jpeg", "jpg"},
		{"gif", []byte("GIF89a\x01\x00"), "image/gif", "gif"},
		// A payload nothing recognises keeps the fallback rather than being refused:
		// the bytes are what the user asked for.
		{"unknown", []byte{0x00, 0x01, 0x02, 0x03}, "image/png", "png"},
		{"empty", nil, "image/png", "png"},
	}
	for _, tc := range cases {
		mime, ext := images.SniffImage(tc.data, images.MimePNG, "png")
		if mime != tc.wantMime || ext != tc.wantExt {
			t.Errorf("%s: sniffed %q/%q, want %q/%q", tc.name, mime, ext, tc.wantMime, tc.wantExt)
		}
	}
}

func TestSeedFromValue(t *testing.T) {
	// A missing seed, the WebUI's own -1 and a value that is not a number all
	// mean the same thing: let the server choose. Zero would mean the opposite.
	for _, value := range []any{nil, float64(-1), "abc", ""} {
		if got := seedFromValue(value); got != nil {
			t.Errorf("seedFromValue(%v) = %v, want nil", value, got)
		}
	}
	got := seedFromValue(float64(1234))
	if got == nil || *got != 1234 {
		t.Errorf("seedFromValue(1234) = %v, want 1234", got)
	}
}

// firstAttachment returns the chat's only picture, failing the test if it is not alone.
func firstAttachment(t *testing.T, st *store.Store, chatID string) store.Attachment {
	t.Helper()
	attachments, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(attachments))
	}
	return attachments[0]
}

// TestGenerateImageRecordsWhatTheServerActuallyUsed is what makes a redraw possible at all,
// and it is asserted against the values the *server* reported rather than the ones the
// request carried.
//
// A request that leaves the seed open is given one by the server, and only that number
// reproduces the picture — "no seed" is not a recipe. The checkpoint is the same story: the
// server knows which one it loaded, and a recipe naming the requested one instead would
// draw something else while claiming to repeat the picture.
func TestGenerateImageRecordsWhatTheServerActuallyUsed(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{
		Width: 768, Height: 512, Steps: 25, Sampler: "Euler a", Model: "requested.safetensors",
	})

	ctx := WithCallingMessage(context.Background(), "msg-1")
	if _, err := service.Execute(ctx, chatID, ToolGenerateImage,
		`{"prompt":"a red fox in deep snow","negative_prompt":"blurry"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	recipe := firstAttachment(t, st, chatID).Generation
	if recipe == nil {
		t.Fatal("the picture was stored without a recipe, so it can never be drawn again")
	}
	if recipe.Prompt != "a red fox in deep snow" || recipe.NegativePrompt != "blurry" {
		t.Errorf("prompt fields = %q / %q, want what was drawn", recipe.Prompt, recipe.NegativePrompt)
	}
	if recipe.Width != 768 || recipe.Height != 512 || recipe.Steps != 25 || recipe.Sampler != "Euler a" {
		t.Errorf("the configured settings are missing from the recipe: %+v", recipe)
	}
	// onePicture reports seed 4242 and the checkpoint sdxl.safetensors; the request carried
	// no seed and named a different checkpoint.
	if recipe.Seed != 4242 {
		t.Errorf("seed = %d, want the one the server reported", recipe.Seed)
	}
	if recipe.Model != "sdxl.safetensors" {
		t.Errorf("model = %q, want the checkpoint the server reported using", recipe.Model)
	}
}

// TestGenerateImageAsksForTheSecondPassConfiguredInSettings pins the settings-to-request
// direction and the recipe it leaves behind. The picture here is the deliberately
// undecodable fixture, so the verification that follows a draw cannot answer — the point of
// this test is that the pass was asked for and recorded, not that a server honoured it.
// DurationMs is asserted from a provider that actually sleeps, because a number measured
// nowhere would otherwise still read as zero and look plausible.
func TestGenerateImageAsksForTheSecondPassConfiguredInSettings(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture(), delay: 5 * time.Millisecond}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{
		Width: 768, Height: 512, Steps: 20,
		HiresEnabled: true, HiresScale: 1.5, HiresDenoise: 0.4,
	})

	ctx := WithCallingMessage(context.Background(), "msg-1")
	if _, err := service.Execute(ctx, chatID, ToolGenerateImage,
		`{"prompt":"a red fox in deep snow"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !provider.got.Hires.Enabled {
		t.Fatal("the second pass was configured but never asked for")
	}
	if provider.got.Hires.Scale != 1.5 || provider.got.Hires.Denoise != 0.4 {
		t.Errorf("hires = %+v, want scale 1.5 and denoise 0.4", provider.got.Hires)
	}
	// The base size is what the request carries; the final size is its product.
	if provider.got.Width != 768 || provider.got.Height != 512 {
		t.Errorf("base size = %dx%d, want the configured 768x512", provider.got.Width, provider.got.Height)
	}
	if w, h := provider.got.OutputSize(); w != 1152 || h != 768 {
		t.Errorf("the request's own final size = %dx%d, want 1152x768", w, h)
	}

	recipe := firstAttachment(t, st, chatID).Generation
	if recipe == nil {
		t.Fatal("the picture was stored without a recipe")
	}
	if !recipe.Hires || recipe.HiresScale != 1.5 || recipe.HiresDenoise != 0.4 {
		t.Errorf("recipe lost the second pass: %+v", recipe)
	}
	if recipe.Width != 768 || recipe.Height != 512 {
		t.Errorf("recipe must keep the base size, not the product: %+v", recipe)
	}
	if recipe.DurationMs < 5 {
		t.Errorf("DurationMs = %d, want at least the 5 ms the provider slept", recipe.DurationMs)
	}
}

// pngOfSize encodes a real PNG of the given size, which is what makes the size check
// answerable: the ordinary fixture is a signature and a handful of bytes, so `images.Size`
// refuses it and verification silently has nothing to compare.
func pngOfSize(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatalf("encode %dx%d png: %v", width, height, err)
	}
	return buf.Bytes()
}

// TestGenerateImageRefusesASecondPassThatDidNotEnlargeThePicture is the A1111 failure this
// check exists for: the route accepts `enable_hr` and answers 200 with a base-size picture
// when the server cannot honour it. Filing that under a recipe claiming an enlargement would
// make every redraw ask for a second pass again and record the same lie, so nothing is filed
// at all and the error names both sizes.
func TestGenerateImageRefusesASecondPassThatDidNotEnlargeThePicture(t *testing.T) {
	base := pngOfSize(t, 768, 512)
	provider := &fakeImageProvider{result: images.Result{
		Images: []images.Image{{Data: base, Mime: images.MimePNG, Ext: "png"}},
		Seed:   7,
	}}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{
		Width: 768, Height: 512,
		HiresEnabled: true, HiresScale: 1.5, HiresDenoise: 0.4,
	})

	ctx := WithCallingMessage(context.Background(), "msg-1")
	_, err := service.Execute(ctx, chatID, ToolGenerateImage, `{"prompt":"a red fox"}`)
	if err == nil {
		t.Fatal("a base-size picture was accepted for a second-pass recipe")
	}
	if !errors.Is(err, images.ErrHiresUnsupported) {
		t.Fatalf("error = %v, want it to wrap ErrHiresUnsupported", err)
	}
	for _, want := range []string{"768x512", "1152x768"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s, which is the whole diagnosis: %v", want, err)
		}
	}
	if attachments, _ := st.ListAttachments(chatID); len(attachments) != 0 {
		t.Errorf("a refused second pass filed %d attachment(s)", len(attachments))
	}
}

// TestGenerateImageFilesASecondPassThatDeliveredTheAlignedSize is the accepting half of the
// check above, with the rounding that made it necessary: 520 × 1.25 is 650, `OutputSize`
// aligns that up to 656, and both backends are asked for 656 exactly. A picture at that size
// is filed, and the recipe keeps the 520×520 base it was drawn from.
func TestGenerateImageFilesASecondPassThatDeliveredTheAlignedSize(t *testing.T) {
	finished := pngOfSize(t, 656, 656)
	provider := &fakeImageProvider{result: images.Result{
		Images: []images.Image{{Data: finished, Mime: images.MimePNG, Ext: "png"}},
		Seed:   4242,
	}}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{
		Width: 520, Height: 520,
		HiresEnabled: true, HiresScale: 1.25, HiresDenoise: 0.4,
	})

	ctx := WithCallingMessage(context.Background(), "msg-1")
	if _, err := service.Execute(ctx, chatID, ToolGenerateImage, `{"prompt":"a red fox"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if w, h := provider.got.OutputSize(); w != 656 || h != 656 {
		t.Errorf("the request's own final size = %dx%d, want the aligned 656x656", w, h)
	}
	recipe := firstAttachment(t, st, chatID).Generation
	if recipe == nil || !recipe.Hires || recipe.Width != 520 || recipe.Height != 520 {
		t.Fatalf("the filed recipe does not describe the second pass from its base size: %+v", recipe)
	}
}

// TestGenerateImageFilesASecondPassThatTheBackendAlignedFurther covers the other half of the
// rounding, and the one stable-diffusion.cpp actually performs: the app asks for 656×656, but
// `GenerationRequest::align_image_size` rounds a target up to `vae_scale_factor *
// diffusion_model_down_factor` — 64 for a UNet — so the picture comes back 704×704. That is
// the second pass at the size the build can draw, not a refusal.
func TestGenerateImageFilesASecondPassThatTheBackendAlignedFurther(t *testing.T) {
	finished := pngOfSize(t, 704, 704)
	provider := &fakeImageProvider{result: images.Result{
		Images: []images.Image{{Data: finished, Mime: images.MimePNG, Ext: "png"}},
		Seed:   4242,
	}}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{
		Width: 520, Height: 520,
		HiresEnabled: true, HiresScale: 1.25, HiresDenoise: 0.4,
	})

	ctx := WithCallingMessage(context.Background(), "msg-1")
	if _, err := service.Execute(ctx, chatID, ToolGenerateImage, `{"prompt":"a red fox"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	recipe := firstAttachment(t, st, chatID).Generation
	if recipe == nil || !recipe.Hires {
		t.Fatalf("the filed recipe does not describe the second pass: %+v", recipe)
	}
}

// TestRegenerateKeepsTheRecordedSecondPass is the redraw direction: a recipe that asks for
// the second pass must ask for it again, and one without it must stay an ordinary draw. The
// undecodable fixture is deliberate here too — a redraw of a second-pass recipe cannot be
// checked against a size it never had in the fixture.
func TestRegenerateKeepsTheRecordedSecondPass(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{})
	ctx := WithCallingMessage(context.Background(), "msg-1")
	if _, err := service.Execute(ctx, chatID, ToolGenerateImage,
		`{"prompt":"a red fox","width":512,"height":512}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	original := firstAttachment(t, st, chatID)

	withHires := *original.Generation
	withHires.Hires = true
	withHires.HiresScale = 1.75
	withHires.HiresDenoise = 0.3
	if _, err := service.Regenerate(context.Background(), original.ID, withHires); err != nil {
		t.Fatalf("Regenerate(with hires): %v", err)
	}
	if !provider.got.Hires.Enabled || provider.got.Hires.Scale != 1.75 || provider.got.Hires.Denoise != 0.3 {
		t.Errorf("a recorded second pass was not asked for again: %+v", provider.got.Hires)
	}

	withoutHires := withHires
	withoutHires.Hires = false
	withoutHires.HiresScale = 0
	withoutHires.HiresDenoise = 0
	if _, err := service.Regenerate(context.Background(), original.ID, withoutHires); err != nil {
		t.Fatalf("Regenerate(without hires): %v", err)
	}
	if provider.got.Hires.Enabled {
		t.Errorf("a recipe without a second pass asked for one: %+v", provider.got.Hires)
	}
}

// TestRegenerateDrawsTheRecipeItIsHanded covers the three controls that share this one call.
// They differ only in the recipe the panel builds, so a mistake in any of them would be
// invisible to a test that exercised one: a repeat has to pin the recorded seed, a variation
// has to clear it and keep the prompt, and an edit has to change what is drawn.
func TestRegenerateDrawsTheRecipeItIsHanded(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{})
	ctx := WithCallingMessage(context.Background(), "msg-1")
	if _, err := service.Execute(ctx, chatID, ToolGenerateImage,
		`{"prompt":"a red fox","width":640,"steps":20}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	original := firstAttachment(t, st, chatID)

	// A repeat: the stored recipe, handed straight back.
	if _, err := service.Regenerate(context.Background(), original.ID, *original.Generation); err != nil {
		t.Fatalf("Regenerate(repeat): %v", err)
	}
	if provider.got.Seed == nil || *provider.got.Seed != 4242 {
		t.Errorf("a repeat must pin the recorded seed, got %v", provider.got.Seed)
	}
	if provider.got.Prompt != "a red fox" || provider.got.Width != 640 || provider.got.Steps != 20 {
		t.Errorf("a repeat must reuse the recorded recipe, got %+v", provider.got)
	}

	// A variation: the stored recipe with the seed cleared, which is what lets the server pick
	// a new one. This server reports 4242 again, so the two redraws collide on seed — exactly
	// the collision the names have to survive.
	variation := *original.Generation
	variation.Seed = 0
	if _, err := service.Regenerate(context.Background(), original.ID, variation); err != nil {
		t.Fatalf("Regenerate(variation): %v", err)
	}
	if provider.got.Seed != nil {
		t.Errorf("a variation must let the server choose, got seed %d", *provider.got.Seed)
	}
	if provider.got.Prompt != "a red fox" {
		t.Errorf("a variation must keep the prompt, got %q", provider.got.Prompt)
	}

	// An edit: the same picture, drawn again with the fields the user changed. The prompt,
	// the size and the steps are all here because those are what the panel's editor offers and
	// what the request has to carry through untouched.
	edited := *original.Generation
	edited.Prompt = "a blue heron in reeds, watercolour"
	edited.Width = 896
	edited.Height = 1152
	edited.Steps = 40
	if _, err := service.Regenerate(context.Background(), original.ID, edited); err != nil {
		t.Fatalf("Regenerate(edit): %v", err)
	}
	if provider.got.Prompt != "a blue heron in reeds, watercolour" {
		t.Errorf("the edited prompt did not reach the server: %q", provider.got.Prompt)
	}
	if provider.got.Width != 896 || provider.got.Height != 1152 || provider.got.Steps != 40 {
		t.Errorf("an edited size or step count was lost on the way: %+v", provider.got)
	}

	attachments, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(attachments) != 4 {
		t.Fatalf("attachments = %d, want the original plus three draws", len(attachments))
	}
	names := map[string]bool{}
	for _, a := range attachments {
		// An attachment on no message renders nowhere, which would make the redraw cost GPU
		// seconds and show the user nothing.
		if a.MessageID != "msg-1" {
			t.Errorf("%s is on message %q, want the message the original is on", a.Filename, a.MessageID)
		}
		// A redraw keeps its own recipe, so it can itself be redrawn.
		if a.Generation == nil {
			t.Errorf("%s was stored without a recipe", a.Filename)
		}
		if names[a.Filename] {
			t.Errorf("two pictures share the name %q, so they cannot be told apart", a.Filename)
		}
		names[a.Filename] = true
	}
	if !names["a-red-fox-1-s4242.png"] {
		t.Errorf("no redraw carries the seed in its name: %v", names)
	}
}

// TestRegenerateRefusesAPictureWithoutARecipe covers the pictures the control must not touch.
// An uploaded photo, a scraped page and a picture drawn before recipes were recorded all
// have no prompt of their own, and redrawing one would draw something unrelated to it.
func TestRegenerateRefusesAPictureWithoutARecipe(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{})
	if err := st.CreateAttachment(store.Attachment{
		ID: "uploaded", ChatID: chatID, MessageID: "msg-1", Kind: "image",
		Filename: "holiday.png", Mime: "image/png", TokenEstimate: 1024,
		Placement: store.PlacementInline, DocumentID: "doc-1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}

	_, err := service.Regenerate(context.Background(), "uploaded",
		store.GenerationRecipe{Prompt: "anything at all"})
	if err == nil {
		t.Fatal("an uploaded picture was redrawn")
	}
	if !strings.Contains(err.Error(), "holiday.png") {
		t.Errorf("the refusal does not name the picture it is about: %v", err)
	}
	if provider.calls != 0 {
		t.Errorf("a provider call was made for a picture with no recipe: %d", provider.calls)
	}
}

// TestRegenerateNeedsAConfiguredServer keeps the off state honest on the new path too: the
// feature being switched off has to refuse before it reads anything.
func TestRegenerateNeedsAConfiguredServer(t *testing.T) {
	service, _, _ := newImageService(t, nil, config.ImageSettings{})
	if _, err := service.Regenerate(context.Background(), "any",
		store.GenerationRecipe{Prompt: "anything"}); !errors.Is(err, images.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

// TestRedrawFilenameTellsTheRedrawsApart is the visible half of the collision: the name is
// what the caption and the save dialog both show.
func TestRedrawFilenameTellsTheRedrawsApart(t *testing.T) {
	// The seed goes in, because it is the number that reproduces the picture and the one
	// thing that differs from the original.
	if got := redrawFilename("a red fox", 4242, "png", 0); got != "a-red-fox-1-s4242.png" {
		t.Errorf("redrawFilename = %q", got)
	}
	// The index still separates several pictures from one call.
	if got := redrawFilename("a red fox", 4242, "png", 1); got != "a-red-fox-2-s4242.png" {
		t.Errorf("second picture of a redraw = %q", got)
	}
	// A server that reported no seed leaves no number to add, and the plain name stands.
	if got := redrawFilename("a red fox", 0, "png", 0); got != "a-red-fox-1.png" {
		t.Errorf("a seedless redraw = %q", got)
	}
	// The extension follows the payload here too, so a JPEG server does not produce a JPEG
	// named .png.
	if got := redrawFilename("a red fox", 7, "jpg", 0); got != "a-red-fox-1-s7.jpg" {
		t.Errorf("a JPEG redraw = %q", got)
	}
}

// TestUniqueNameBumpsACollision is the second line of defence, for the case the seed alone
// cannot solve: the same recipe drawn twice with the same seed reports the same number.
func TestUniqueNameBumpsACollision(t *testing.T) {
	used := map[string]bool{"a-fox-1-s4242.png": true}
	if got := uniqueName(used, "a-fox-1.png"); got != "a-fox-1.png" {
		t.Errorf("a free name was changed: %q", got)
	}
	if got := uniqueName(used, "a-fox-1-s4242.png"); got != "a-fox-1-s4242-2.png" {
		t.Errorf("a taken name = %q, want a numbered variant", got)
	}
	used["a-fox-1-s4242-2.png"] = true
	if got := uniqueName(used, "a-fox-1-s4242.png"); got != "a-fox-1-s4242-3.png" {
		t.Errorf("a name taken twice = %q", got)
	}
	// The extension stays where it belongs, including one the slug would not produce.
	if got := uniqueName(map[string]bool{"x.webp": true}, "x.webp"); got != "x-2.webp" {
		t.Errorf("a webp collision = %q", got)
	}
}

// TestAnEditedRecipeLeavesTheOriginalAlone keeps the transcript honest. A picture records how
// it was actually made, and editing a recipe is a statement about the *next* draw rather than
// a correction of the last one: if the edit rewrote the original, the recipe beside a picture
// would stop describing that picture, and a later redraw would silently use something the
// server never did.
func TestAnEditedRecipeLeavesTheOriginalAlone(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{})
	ctx := WithCallingMessage(context.Background(), "msg-1")
	if _, err := service.Execute(ctx, chatID, ToolGenerateImage,
		`{"prompt":"a red fox","width":640,"steps":20}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	original := firstAttachment(t, st, chatID)
	before := *original.Generation

	edited := before
	edited.Prompt = "something else entirely"
	edited.Width = 128
	edited.Steps = 7
	edited.Seed = 99
	if _, err := service.Regenerate(context.Background(), original.ID, edited); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}

	reread, err := st.GetAttachment(original.ID)
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if reread.Generation == nil {
		t.Fatal("the original lost its recipe")
	}
	if *reread.Generation != before {
		t.Errorf("the edit rewrote the original's recipe:\n got %+v\nwant %+v", *reread.Generation, before)
	}
	// And the new picture carries what it was really drawn with, so it can be edited again.
	attachments, err := st.ListAttachments(chatID)
	if err != nil || len(attachments) != 2 {
		t.Fatalf("ListAttachments = %d, %v", len(attachments), err)
	}
	fresh := attachments[1]
	if fresh.Generation == nil || fresh.Generation.Prompt != "something else entirely" {
		t.Errorf("the new picture's recipe does not describe the new picture: %+v", fresh.Generation)
	}
}

// TestRegenerateRefusesAnEmptyPromptBeforeDrawing is the editor's most likely failure: the
// user clears the prompt box and presses Draw. The button is disabled for it, but a request
// can arrive from anywhere, and a prompt-less generation is not a degenerate case — it is a
// blank canvas the server draws something arbitrary on and charges for.
func TestRegenerateRefusesAnEmptyPromptBeforeDrawing(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{})
	ctx := WithCallingMessage(context.Background(), "msg-1")
	if _, err := service.Execute(ctx, chatID, ToolGenerateImage, `{"prompt":"a red fox"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	original := firstAttachment(t, st, chatID)
	provider.calls = 0

	_, err := service.Regenerate(context.Background(), original.ID,
		store.GenerationRecipe{Prompt: "   ", Width: 512})
	if err == nil {
		t.Fatal("a blank prompt was drawn")
	}
	if provider.calls != 0 {
		t.Errorf("the server was asked to draw with no prompt: %d calls", provider.calls)
	}
}

// TestRegenerateClampsTheStepCount covers the one field a user types as a raw number. The
// bounds are the settings' own, and they matter more here than there: a stored recipe came
// from this app, but an edited one comes from a box the user filled in, and a local server
// does not refuse a 100000-step request — it attempts it, for hours.
func TestRegenerateClampsTheStepCount(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{})
	ctx := WithCallingMessage(context.Background(), "msg-1")
	if _, err := service.Execute(ctx, chatID, ToolGenerateImage, `{"prompt":"a red fox"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	original := firstAttachment(t, st, chatID)

	recipe := *original.Generation
	recipe.Steps = 100000
	if _, err := service.Regenerate(context.Background(), original.ID, recipe); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	if provider.got.Steps != config.MaxImageSteps {
		t.Errorf("steps = %d, want the bound %d", provider.got.Steps, config.MaxImageSteps)
	}

	// A negative count is "the server's own" rather than an error, the same as everywhere else
	// a size or a step count can be left empty.
	recipe.Steps = -5
	if _, err := service.Regenerate(context.Background(), original.ID, recipe); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	if provider.got.Steps != 0 {
		t.Errorf("steps = %d, want 0 (the server's own)", provider.got.Steps)
	}
}

// interruptibleProvider is the fake above plus the one method that makes a provider an
// images.Interrupter. It is a separate type rather than a field on the fake on purpose:
// the interesting half of the pair is the provider that *cannot* be asked, and a method
// that was always present would make that case impossible to write down.
type interruptibleProvider struct {
	fakeImageProvider
	interruptErr   error
	interruptCalls int
}

func (p *interruptibleProvider) Interrupt(context.Context) error {
	p.interruptCalls++
	if p.interruptErr != nil {
		return p.interruptErr
	}
	return nil
}

// TestInterruptDrawReportsWhetherTheServerCouldBeAsked pins the three answers the caller
// has to tell apart. They are not one error with three texts: "there is no server" is a
// configuration the user can fix, "this server has no interrupt route" is a fact about
// stable-diffusion.cpp and holds nothing to fix, and "the server was asked and refused" is
// the only one that is a failure. Collapsing them would either hide a real refusal or
// report a failure on every stop a stable-diffusion.cpp user makes.
func TestInterruptDrawReportsWhetherTheServerCouldBeAsked(t *testing.T) {
	t.Run("no server configured", func(t *testing.T) {
		asked, err := (&Service{}).InterruptDraw(context.Background())
		if asked {
			t.Error("a service with no provider reported that it asked one")
		}
		if !errors.Is(err, images.ErrNotConfigured) {
			t.Errorf("err = %v, want ErrNotConfigured", err)
		}
	})

	t.Run("a provider that cannot interrupt is not a failure", func(t *testing.T) {
		// The base fake has no Interrupt method, which is what a ComfyUI client would
		// look like here: able to draw, unable to be stopped.
		service, _, _ := newImageService(t, &fakeImageProvider{}, config.ImageSettings{})
		asked, err := service.InterruptDraw(context.Background())
		if asked {
			t.Error("a provider without Interrupt was reported as asked")
		}
		if err != nil {
			t.Errorf("err = %v, want nil — a server that cannot be stopped is not broken", err)
		}
	})

	t.Run("an interruptible provider is asked", func(t *testing.T) {
		provider := &interruptibleProvider{fakeImageProvider: fakeImageProvider{result: onePicture()}}
		service, _, _ := newImageService(t, provider, config.ImageSettings{})
		asked, err := service.InterruptDraw(context.Background())
		if !asked {
			t.Error("an interruptible provider was not reported as asked")
		}
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		if provider.interruptCalls != 1 {
			t.Errorf("interrupt calls = %d, want 1", provider.interruptCalls)
		}
	})

	t.Run("the provider's refusal travels", func(t *testing.T) {
		refusal := errors.New("404 Not Found: /sdapi/v1/interrupt")
		provider := &interruptibleProvider{
			fakeImageProvider: fakeImageProvider{result: onePicture()},
			interruptErr:      refusal,
		}
		service, _, _ := newImageService(t, provider, config.ImageSettings{})
		asked, err := service.InterruptDraw(context.Background())
		if !asked {
			t.Error("a provider that was asked and refused was not reported as asked")
		}
		if !errors.Is(err, refusal) {
			t.Errorf("err = %v, want the provider's own refusal", err)
		}
	})
}

// TestGenerateImageBoundsTheModelsOwnNumbers is the hole this closes: the tool's schema
// advertises 64–2048 pixels and 1–150 steps, and a model is free to ignore it — the small
// local models this app is built for are the ones that most often do. Nothing bounded the
// numbers on this path before, so a `generate_image` call could ask a WebUI for 100000
// steps (hours of sampling, attempted rather than refused) or a CLI build for a 20000-pixel
// latent (tens of gigabytes).
//
// The recipe is asserted too, and that is the subtler half: it is written from the request,
// so a request that was silently clamped by the provider would leave a recipe describing a
// picture that was never drawn — "draw that again" would then produce something else.
func TestGenerateImageBoundsTheModelsOwnNumbers(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{})
	ctx := WithCallingMessage(context.Background(), "msg-1")

	out, err := service.Execute(ctx, chatID, ToolGenerateImage,
		`{"prompt":"a red fox","width":20000,"height":63,"steps":100000,"cfg_scale":250}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if provider.got.Width != config.MaxImageDimension || provider.got.Height != config.MinImageDimension {
		t.Errorf("the provider was asked for %dx%d, want the bounds %dx%d",
			provider.got.Width, provider.got.Height, config.MaxImageDimension, config.MinImageDimension)
	}
	if provider.got.Steps != config.MaxImageSteps {
		t.Errorf("the provider was asked for %d steps, want the bound %d", provider.got.Steps, config.MaxImageSteps)
	}
	if provider.got.CfgScale != config.MaxImageCfgScale {
		t.Errorf("the provider was asked for cfg scale %v, want the bound %v",
			provider.got.CfgScale, config.MaxImageCfgScale)
	}

	got := firstAttachment(t, st, chatID)
	if got.Generation == nil {
		t.Fatal("the picture has no recipe")
	}
	if got.Generation.Width != provider.got.Width || got.Generation.Height != provider.got.Height ||
		got.Generation.Steps != provider.got.Steps {
		t.Errorf("the recipe describes %dx%d at %d steps while the provider drew %dx%d at %d",
			got.Generation.Width, got.Generation.Height, got.Generation.Steps,
			provider.got.Width, provider.got.Height, provider.got.Steps)
	}
	// The reply the model reads repeats the numbers it asked for, so a bound applied
	// behind its back would have it describe a picture nobody drew.
	if strings.Contains(out, "100000") || strings.Contains(out, "20000") || strings.Contains(out, "250") {
		t.Errorf("the summary quotes a number that was not used:\n%s", out)
	}
}

// A guidance scale nobody can read — a string, a word, "nan" — is "nobody made a
// decision" rather than a failed call, and the draw still happens. `strconv.ParseFloat`
// accepts "nan", and a NaN that reached the payload would fail the marshalling of the whole
// request: the one field a model garbled would cost the user their picture.
func TestAGarbledGuidanceScaleLeavesTheChoiceToTheBackend(t *testing.T) {
	for _, args := range []string{
		`{"prompt":"a fox","cfg_scale":"nan"}`,
		`{"prompt":"a fox","cfg_scale":"hoch"}`,
		`{"prompt":"a fox","cfg_scale":null}`,
	} {
		provider := &fakeImageProvider{result: onePicture()}
		service, _, chatID := newImageService(t, provider, config.ImageSettings{})
		if _, err := service.Execute(context.Background(), chatID, ToolGenerateImage, args); err != nil {
			t.Fatalf("Execute(%s): %v", args, err)
		}
		if provider.got.CfgScale != 0 {
			t.Errorf("%s: cfg scale = %v, want 0 (the backend's own)", args, provider.got.CfgScale)
		}
	}
}

// TestTwoDrawsOfOnePromptGetDistinctFilenames is the invariant redrawing has always kept and
// the tool path did not: `uniqueName` guards the redraw, and this path — which a model drives
// by simply being asked "draw that again" — wrote the same slug and the same index twice.
// Two pictures under one name are two pictures a user cannot tell apart: the caption under
// each one is the filename, and the save dialog offers the same name for both.
func TestTwoDrawsOfOnePromptGetDistinctFilenames(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, st, chatID := newImageService(t, provider, config.ImageSettings{})
	ctx := WithCallingMessage(context.Background(), "msg-1")

	for i := 0; i < 2; i++ {
		if _, err := service.Execute(ctx, chatID, ToolGenerateImage,
			`{"prompt":"a red fox in deep snow"}`); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	attachments, err := st.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(attachments) != 2 {
		t.Fatalf("attachments = %d, want 2", len(attachments))
	}
	if attachments[0].Filename == attachments[1].Filename {
		t.Errorf("both pictures are named %q; the second draw needs a name of its own",
			attachments[0].Filename)
	}
	// The first keeps the plain name, so the common case is unchanged and a numbered
	// variant only appears when there is something to avoid.
	if attachments[0].Filename != "a-red-fox-in-deep-snow-1.png" {
		t.Errorf("first filename = %q, want the plain slug", attachments[0].Filename)
	}
}

// progressiveProvider is a backend that can say what it is doing while it draws, which is
// the optional capability the reporting hangs on.
type progressiveProvider struct {
	fakeImageProvider
	reports []images.Progress
	// sink is called from inside the draw, so a test can assert on what the *service* did
	// with a report rather than only on what the provider produced.
	sink func(images.Progress)
}

func (p *progressiveProvider) GenerateWithProgress(ctx context.Context, req images.Request, report func(images.Progress)) (images.Result, error) {
	p.got = req
	p.calls++
	if p.err != nil {
		return images.Result{}, p.err
	}
	for _, step := range []int{1, 2} {
		progress := images.Progress{Step: step, Steps: 2, Preview: pictureBytes, PreviewMime: images.MimePNG}
		p.reports = append(p.reports, progress)
		report(progress)
		if p.sink != nil {
			p.sink(progress)
		}
	}
	return p.result, nil
}

// TestADrawIsWatchedAndStoppableUnderTheMessageThatAskedForIt is the half of progress that is
// not about pixels: a draw the *model* started has no picture to hang a Stop button under
// until it has finished, so it is registered under the message the tool call is rendered on
// — and that is the id the view has, which is what makes the button reachable at all.
//
// The release is asserted because a registration that outlives its draw is a Stop button for
// something that is not running, and the chat id because a progress report without one cannot
// be delivered to the chat that asked.
func TestADrawIsWatchedAndStoppableUnderTheMessageThatAskedForIt(t *testing.T) {
	provider := &progressiveProvider{fakeImageProvider: fakeImageProvider{result: onePicture()}}
	service, _, chatID := newImageService(t, provider, config.ImageSettings{})

	var registeredID, registeredChat string
	released := 0
	service.Draws = func(id, chat string, cancel context.CancelFunc) func() {
		registeredID, registeredChat = id, chat
		if cancel == nil {
			t.Error("a registered draw arrived with no way to cancel it, so Stop could not end it")
		}
		return func() { released++ }
	}
	var progressIDs []string
	var reports []images.Progress
	service.Progress = func(id, chat string, p images.Progress) {
		progressIDs = append(progressIDs, id)
		reports = append(reports, p)
		if chat != chatID {
			t.Errorf("progress reported for chat %q, want %q", chat, chatID)
		}
	}

	ctx := WithCallingMessage(context.Background(), "msg-42")
	if _, err := service.Execute(ctx, chatID, ToolGenerateImage, `{"prompt":"a red fox"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if registeredID != "msg-42" {
		t.Errorf("the draw was registered under %q, want the calling message id", registeredID)
	}
	if registeredChat != chatID {
		t.Errorf("registered chat = %q, want %q", registeredChat, chatID)
	}
	if released != 1 {
		t.Errorf("release called %d times, want exactly 1 — a registration that outlives its draw leaves a Stop button for nothing", released)
	}
	if len(progressIDs) != 3 {
		t.Fatalf("progress reports = %d, want the start report plus the 2 the provider sent", len(progressIDs))
	}
	// The first report announces the draw before the backend has anything to say. It is what
	// makes the row — and the Stop button — exist on a backend that never reports on its
	// own, so it is deliberately empty of pixels.
	if len(reports[0].Preview) != 0 || reports[0].Step != 0 {
		t.Errorf("the start report = %+v, want an empty one that only announces the draw", reports[0])
	}
	for _, p := range reports[1:] {
		if len(p.Preview) == 0 || p.PreviewMime == "" {
			t.Errorf("report %+v carries no usable preview", p)
		}
	}
	for _, id := range progressIDs {
		if id != "msg-42" {
			t.Errorf("a report was filed under %q, want the registered id", id)
		}
	}
}

// TestADrawWithNobodyWatchingStillDraws is the fallback the optional interface needs: a
// service with no app around it — which is what every test in this package is — and a backend
// that cannot report at all must both simply draw. A missing capability is not a broken
// backend, and a draw that failed because nothing was listening would be the worst version of
// that mistake.
func TestADrawWithNobodyWatchingStillDraws(t *testing.T) {
	t.Run("the backend cannot report", func(t *testing.T) {
		provider := &fakeImageProvider{result: onePicture()}
		service, _, chatID := newImageService(t, provider, config.ImageSettings{})
		// The draw is still *registered*, and that is not a contradiction: registering is
		// about being stoppable, which this backend can be, and reporting is about being
		// watchable, which it cannot. What this backend cannot supply is any report of its
		// own; the one report that does arrive is the service's own announcement of the
		// draw, which is what gives the window a row to put a Stop on.
		registered := 0
		service.Draws = func(string, string, context.CancelFunc) func() {
			registered++
			return func() {}
		}
		var reports []images.Progress
		service.Progress = func(_, _ string, p images.Progress) {
			reports = append(reports, p)
		}
		out, err := service.Execute(WithCallingMessage(context.Background(), "msg-1"), chatID, ToolGenerateImage,
			`{"prompt":"a red fox"}`)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "erzeugt") {
			t.Errorf("result = %q, want the picture reported as drawn", out)
		}
		if provider.calls != 1 {
			t.Errorf("provider calls = %d, want 1", provider.calls)
		}
		if registered != 1 {
			t.Errorf("draws registered = %d, want 1: a draw a Stop cannot reach is the failure this registration exists for", registered)
		}
		if len(reports) != 1 {
			t.Errorf("reports = %d, want exactly the one start report: a backend that cannot report still has to announce its draw so the Stop is reachable", len(reports))
		} else if reports[0].Step != 0 || len(reports[0].Preview) != 0 {
			t.Errorf("start report = %+v, want a zero report with no preview", reports[0])
		}
	})

	t.Run("nobody is watching", func(t *testing.T) {
		provider := &progressiveProvider{fakeImageProvider: fakeImageProvider{result: onePicture()}}
		service, _, chatID := newImageService(t, provider, config.ImageSettings{})
		// Draws and Progress are nil, which is how the service is built outside the app.
		if _, err := service.Execute(WithCallingMessage(context.Background(), "msg-1"), chatID, ToolGenerateImage,
			`{"prompt":"a red fox"}`); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if provider.calls != 1 {
			t.Errorf("provider calls = %d, want 1", provider.calls)
		}
	})
}

func TestGenerateRejectsEmptyBackendResult(t *testing.T) {
	for _, result := range []images.Result{{}, {Images: []images.Image{{}}}} {
		provider := &fakeImageProvider{result: result}
		service, st, chatID := newImageService(t, provider, config.ImageSettings{})
		if _, err := service.Execute(WithCallingMessage(t.Context(), "msg-1"), chatID, ToolGenerateImage, `{"prompt":"a fox"}`); err == nil {
			t.Fatal("empty result reported as a successful image")
		}
		attachments, err := st.ListAttachments(chatID)
		if err != nil || len(attachments) != 0 {
			t.Fatalf("attachments = %v, err = %v", attachments, err)
		}
	}
}

func TestStoppedCaptionDoesNotStartDrawing(t *testing.T) {
	provider := &fakeImageProvider{result: onePicture()}
	service, _, chatID := newImageService(t, provider, config.ImageSettings{Captioning: images.CaptioningAuto})
	ctx, cancel := context.WithCancel(WithCallingMessage(t.Context(), "msg-1"))
	service.Caption = func(context.Context, images.CaptionRequest) (images.CaptionResult, error) {
		cancel()
		return images.CaptionResult{}, context.Canceled
	}
	_, err := service.Execute(ctx, chatID, ToolGenerateImage, `{"prompt":"a fox"}`)
	if !errors.Is(err, context.Canceled) || provider.calls != 0 {
		t.Fatalf("err = %v, draw calls = %d; want cancellation without a draw", err, provider.calls)
	}
}
