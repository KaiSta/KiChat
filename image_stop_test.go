package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/images"
	"kichat/internal/store"
	"kichat/internal/tools"
)

// stopTestPicture stands in for a PNG. It is not valid base64 itself, so a decode that
// silently returned its input would fail a round trip rather than pass it; nothing in the
// tool path decodes the picture, it stores the bytes it was handed.
var stopTestPicture = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x7f, 0x3c}

// slowImageServer is a WebUI that draws the first picture and, from then on, takes
// forever — which is what a 40-step draw on a CPU server amounts to. It reports whether
// the interrupt route was called and lets a test wait until a draw is really in flight,
// because "Stop worked" is only a claim about a request that had actually started.
type slowImageServer struct {
	*httptest.Server
	draws      atomic.Int32
	interrupts atomic.Int32
	// noInterrupt serves the interrupt route as absent, which is stable-diffusion.cpp.
	noInterrupt bool
	drawing     chan struct{}
	release     chan struct{}
}

func newSlowImageServer(t *testing.T, noInterrupt bool) *slowImageServer {
	t.Helper()
	s := &slowImageServer{
		noInterrupt: noInterrupt,
		drawing:     make(chan struct{}, 8),
		// Closing it from a test is the only way to let a handler that never noticed the
		// disconnect finish, so the server can be closed without waiting on a goroutine.
		release: make(chan struct{}),
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sdapi/v1/interrupt":
			if s.noInterrupt {
				http.NotFound(w, r)
				return
			}
			s.interrupts.Add(1)
			fmt.Fprint(w, `{}`)
		case "/sdapi/v1/txt2img":
			// The first draw is the setup: it has to succeed so that a picture with a
			// recipe exists to redraw later.
			if s.draws.Add(1) == 1 {
				fmt.Fprintf(w, `{"images":["%s"],"info":"{\"seed\":4242}"}`,
					base64.StdEncoding.EncodeToString(stopTestPicture))
				return
			}
			s.drawing <- struct{}{}
			// A real WebUI does not notice the client going away; it finishes the picture
			// and throws the answer at a closed socket. Waiting for either means this
			// handler holds the connection open exactly as long as it is being drawn, so
			// the client's cancellation is what has to end it.
			select {
			case <-r.Context().Done():
			case <-s.release:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		close(s.release)
		s.Server.Close()
	})
	return s
}

// newImageDrawApp is an app with image generation switched on against the given server,
// and one picture already in the transcript — drawn through the real tool path, so it
// carries the recipe a redraw needs.
func newImageDrawApp(t *testing.T, server *slowImageServer) (*App, string) {
	t.Helper()
	settings := config.Default()
	settings.Images.Enabled = true
	settings.Images.BaseURL = server.URL

	a := newSettingsTestApp(t, settings, t.TempDir())
	// A bound call needs the app's lifetime context; there is no Wails runtime here.
	a.ctx = context.Background()
	a.rebuildServices(settings)

	// The row goes in through the store rather than through the bound CreateChat: that
	// method emits the `chats:changed` event, and an event needs the Wails runtime's own
	// context, which a test does not have.
	chatID := "chat-draw"
	if err := a.store.CreateChat(store.Chat{ID: chatID, Title: "Bilder", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	ctx := tools.WithCallingMessage(t.Context(), "msg-1")
	if _, err := a.currentToolService().Execute(ctx, chatID, tools.ToolGenerateImage,
		`{"prompt":"a red fox in deep snow"}`); err != nil {
		t.Fatalf("generate_image: %v", err)
	}
	return a, chatID
}

// TestStopImageDrawEndsTheWaitAndAsksTheServer is the whole feature: a draw that a CPU
// server would spend minutes on is abandoned at the click, and the server is told to stop
// burning those minutes rather than being left to finish a picture nobody will ever see.
//
// The first half is what the user feels, and it is measured rather than assumed: the call
// has to *return*, with the cancellation sentinel rather than a transport failure, well
// inside the ten-minute bound it would otherwise be held to. The second half is a request
// the server sees.
func TestStopImageDrawEndsTheWaitAndAsksTheServer(t *testing.T) {
	server := newSlowImageServer(t, false)
	a, chatID := newImageDrawApp(t, server)

	attachments, err := a.store.ListAttachments(chatID)
	if err != nil || len(attachments) != 1 {
		t.Fatalf("attachments = %d, err = %v", len(attachments), err)
	}
	original := attachments[0]
	if original.Generation == nil {
		t.Fatal("the setup picture carries no recipe, so there is nothing to redraw")
	}

	type outcome struct {
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		_, err := a.RegenerateImage(original.ID, *original.Generation)
		done <- outcome{err: err}
	}()

	select {
	case <-server.drawing:
	case <-time.After(5 * time.Second):
		t.Fatal("the redraw never reached the server")
	}

	started := time.Now()
	result := a.StopImageDraw(original.ID)
	if !result.WasDrawing {
		t.Error("Stop reported nothing to stop while a draw was in flight")
	}
	if !result.Interrupted {
		t.Errorf("the server with the interrupt route was not asked to stop: %+v", result)
	}
	if result.InterruptError != "" {
		t.Errorf("interruptError = %q, want empty on a server that answered", result.InterruptError)
	}

	select {
	case got := <-done:
		if !errors.Is(got.err, images.ErrCancelled) {
			t.Errorf("err = %v, want the cancellation sentinel — a user's own click is not a server failure", got.err)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Errorf("the draw took %s to unwind after Stop", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the draw is still waiting after Stop — the click did not reach it")
	}

	if got := server.interrupts.Load(); got != 1 {
		t.Errorf("interrupt requests = %d, want 1", got)
	}
	// Nothing was drawn, so nothing may have been filed: a stopped draw leaves the
	// transcript exactly as it was.
	after, err := a.store.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(after) != 1 {
		t.Errorf("attachments after a stopped draw = %d, want the original alone", len(after))
	}
}

// TestStopImageDrawIsNotAFailureOnAServerWithoutTheRoute is the stable-diffusion.cpp case,
// and the one that decides whether the interrupt may be attempted at all: a 404 from a
// server that has no such route must not turn the stop into an error. The wait is over
// either way — that is what the button is for — and the refusal is reported as a fact
// rather than raised as a problem the user cannot do anything about.
func TestStopImageDrawIsNotAFailureOnAServerWithoutTheRoute(t *testing.T) {
	server := newSlowImageServer(t, true)
	a, chatID := newImageDrawApp(t, server)

	attachments, err := a.store.ListAttachments(chatID)
	if err != nil || len(attachments) != 1 {
		t.Fatalf("attachments = %d, err = %v", len(attachments), err)
	}
	original := attachments[0]

	done := make(chan error, 1)
	go func() {
		_, err := a.RegenerateImage(original.ID, *original.Generation)
		done <- err
	}()
	select {
	case <-server.drawing:
	case <-time.After(5 * time.Second):
		t.Fatal("the redraw never reached the server")
	}

	result := a.StopImageDraw(original.ID)
	if !result.WasDrawing {
		t.Error("Stop reported nothing to stop while a draw was in flight")
	}
	if result.Interrupted {
		t.Error("a server that answered 404 was reported as interrupted")
	}
	if result.InterruptError == "" {
		t.Error("the refused interrupt was dropped rather than reported")
	}

	select {
	case err := <-done:
		if !errors.Is(err, images.ErrCancelled) {
			t.Errorf("err = %v, want the cancellation sentinel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the draw is still waiting after Stop")
	}
}

// TestStopImageDrawWithNothingRunningSaysSo covers the second click — and the click that
// lands after the picture arrived. Both are ordinary, and both have to be answerable
// without inventing a stop that did not happen: the UI says nothing at all for them, which
// it can only do if the backend distinguishes "stopped it" from "there was nothing there".
func TestStopImageDrawWithNothingRunningSaysSo(t *testing.T) {
	server := newSlowImageServer(t, false)
	a, chatID := newImageDrawApp(t, server)

	attachments, err := a.store.ListAttachments(chatID)
	if err != nil || len(attachments) != 1 {
		t.Fatalf("attachments = %d, err = %v", len(attachments), err)
	}

	// A picture that was never redrawn, and an id that does not exist at all.
	for _, id := range []string{attachments[0].ID, "att-does-not-exist"} {
		result := a.StopImageDraw(id)
		if result.WasDrawing {
			t.Errorf("Stop(%q) claimed a draw was in flight", id)
		}
		if result.Interrupted || result.InterruptError != "" {
			t.Errorf("Stop(%q) = %+v, want nothing attempted", id, result)
		}
	}
	if got := server.interrupts.Load(); got != 0 {
		t.Errorf("interrupt requests = %d, want 0: nothing was in flight", got)
	}
}
