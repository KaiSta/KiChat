// Package images turns a text prompt into picture bytes through an image server
// the user runs themselves.
//
// It is deliberately separate from internal/llm. A chat provider streams tokens
// into a conversation and is asked for one model at a time; an image server takes
// one prompt and hands back finished bytes, has no conversation state, and is
// configured with a size, a step count and a checkpoint rather than a context
// window. Making llm.Provider cover both would oblige every chat backend to
// pretend it can draw and every image backend to pretend it can chat, and the
// settings and the UI would have to carry fields that are meaningless to one
// side. The two meet in exactly one place — the tool that turns a request into
// rows in the attachments table — so only that shared shape is reused.
package images

import (
	"context"
	"errors"
	"fmt"
)

// Request is one text-to-image generation, normalised across servers.
type Request struct {
	Prompt         string
	NegativePrompt string
	Width          int
	Height         int
	Steps          int
	Sampler        string
	// Seed is the seed to draw with. nil asks for a fresh random one, which the
	// *provider* picks and reports back rather than leaving the choice to the
	// server: only the seed that was used makes a picture reproducible, and a
	// server that picks one and does not report it has produced a picture the user
	// can never ask for again. See Result.Seed.
	Seed *int64
	// Model names the checkpoint to use for this request only. Empty means the
	// server's own current selection, and is the right default: a user who set up
	// their WebUI has already chosen a model, and overriding it with a guess would
	// quietly ignore that.
	Model string
}

// Image is one generated picture together with the format it arrived in.
type Image struct {
	Data []byte
	Mime string
	// Ext is the file extension for Mime, without the dot: "png", "jpg", "webp".
	//
	// It exists because the format is not a constant. A WebUI returns whatever its
	// `samples_format` setting says, and a user who chose JPEG to save disk space
	// would otherwise get a JPEG named `.png` and offered in a `*.png` dialog — a
	// file that opens in most viewers only because they sniff.
	Ext string
}

// Sentinels for the formats a WebUI can be configured to return. Anything else
// keeps its bytes and is reported as PNG, which is what the route returns by
// default and the only format every build agrees on.
const (
	MimePNG  = "image/png"
	MimeJPEG = "image/jpeg"
	MimeWebP = "image/webp"
)

// SniffImage identifies a picture from its leading bytes and reports its MIME
// type and extension. An unrecognised payload keeps the given fallback rather
// than being rejected: the bytes are what the user asked for, and refusing to
// store a picture because its header is unfamiliar is a worse answer than
// storing it under the format the route usually returns.
func SniffImage(data []byte, fallbackMime, fallbackExt string) (string, string) {
	switch {
	case len(data) >= 8 && string(data[1:4]) == "PNG":
		return MimePNG, "png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return MimeJPEG, "jpg"
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return MimeWebP, "webp"
	case len(data) >= 6 && string(data[0:3]) == "GIF":
		return "image/gif", "gif"
	}
	return fallbackMime, fallbackExt
}

// Result is what one generation produced.
type Result struct {
	Images []Image
	// Seed is the seed that reproduces this picture: the one the server reported
	// using, or — when the server reports nothing — the one this app chose and sent.
	// 0 means no seed is known, which is why a seed this app picks is never 0. The
	// distinction matters downstream: a recipe with no seed can only be redrawn as a
	// variation, while a recipe with one draws the same picture again.
	Seed int64
	// Model is the checkpoint the server reported using, empty when it did not
	// name one.
	Model string
	// Info is the server's own report about the run, kept verbatim for the tool
	// result: when a picture comes out wrong the sampler, the seed and the
	// scheduler it actually used are what a bug report needs.
	Info string
}

// Model is a checkpoint the server offers.
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Type says what kind of model this is where the server distinguishes them.
	Type string `json:"type,omitempty"`
}

// Provider is an image generation backend.
type Provider interface {
	// ID is the stable key used in settings and in the tool result.
	ID() string
	// Name is the human-readable backend name.
	Name() string
	// Health returns nil when the server is reachable.
	Health(ctx context.Context) error
	// Models lists the checkpoints the server can be asked for.
	Models(ctx context.Context) ([]Model, error)
	// Samplers lists the sampler names the server accepts. It is separate from
	// Models because the two failure modes differ: an empty sampler list means
	// "let the server choose", while an empty model list means there is nothing
	// to draw with at all.
	Samplers(ctx context.Context) ([]string, error)
	// Generate draws one or more pictures. Cancelling ctx aborts it.
	Generate(ctx context.Context, req Request) (Result, error)
}

// Interrupter is a provider that can stop work it has already been handed.
//
// It is a separate interface rather than a method on Provider because not every
// image server has a way to do it: AUTOMATIC1111 and Forge answer
// `POST /sdapi/v1/interrupt`, while stable-diffusion.cpp's server cancels by job id
// and has no such route at all. A provider that cannot interrupt simply does not
// implement this — a truer statement than an Interrupt method that always fails.
//
// The caller must not depend on it for ending a draw. Cancelling the request's
// context is what ends the wait, and it works against every server; interrupting is
// what stops the *server* burning GPU minutes on a picture nobody will see, and is
// therefore worth attempting and never worth requiring.
type Interrupter interface {
	// Interrupt asks the server to abandon the generation it is running, and returns
	// its error when the server was asked and refused (a 404 from a server without
	// the route, a dead connection).
	Interrupt(ctx context.Context) error
}

// DefaultBaseURL is the address every server this package talks to listens on when
// nothing was configured. It is AUTOMATIC1111's own default, which its Forge fork and
// stable-diffusion.cpp's sd-server share, and it is offered as the starting value in
// Settings rather than probed: nothing is contacted until the feature is switched on.
//
// It lives here rather than in the settings package because the two must not drift apart —
// the settings that offer an address and the client that dials it describe one fact — and
// because this package's own live test connects to it, which a constant copied into two
// packages cannot do without an import cycle.
const DefaultBaseURL = "http://127.0.0.1:7860"

// ErrUnavailable marks a server that could not be reached.
var ErrUnavailable = errors.New("image backend unavailable")

// ErrNotConfigured marks a request made with no image server configured.
var ErrNotConfigured = errors.New("no image backend is configured")

// ErrCancelled marks a draw the user stopped.
//
// It is its own error because it is the one generation failure that is not a failure:
// the picture was not drawn, nothing was charged for it, and the state the UI reports
// is "nothing happened" rather than "something broke". Without a distinct value the
// caller can only see a context error through an HTTP client's own wrapping, and the
// user's own click reads as a crash.
var ErrCancelled = errors.New("das Zeichnen wurde abgebrochen")

// ErrNoModel marks a generation request against a server that has no checkpoint
// loaded. It is its own error because the fix is specific and the message a bare
// HTTP failure produces does not name it: a WebUI with no model installed answers
// with a 404 on its own API rather than a sentence about the missing model.
var ErrNoModel = errors.New("the image server has no model loaded")

// HTTPError carries a server response failure with enough detail to be useful in
// the UI. It mirrors llm.HTTPError rather than sharing it: the two packages are
// independent consumers of unrelated services, and exporting one package's
// internal error type so the other can decorate it would couple them for a
// twenty-line struct. What matters is that both redact credentials and both
// expose Retryable, which they do.
type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
	URL        string
}

func (e *HTTPError) Error() string {
	body := e.Body
	if len(body) > 400 {
		body = body[:400] + "…"
	}
	return fmt.Sprintf("%s: %s %s", e.URL, e.Status, body)
}

// Retryable reports whether the failure is worth retrying. An image server that
// is busy loading a checkpoint answers 503; one that is still starting refuses the
// connection outright, which is a transport error rather than an HTTP one.
func (e *HTTPError) Retryable() bool {
	return e.StatusCode == 429 || e.StatusCode >= 500
}
