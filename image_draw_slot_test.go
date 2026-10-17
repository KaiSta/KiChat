package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/images"
)

// alwaysDrawing is a WebUI that has no picture to give: every txt2img request is held until
// the client gives up. It is the state the settings panel's Stop exists for — a first draw
// that would otherwise run for the whole bound — and it cannot be the shared slow server,
// whose *first* draw has to succeed so that a picture with a recipe exists.
func alwaysDrawing(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sdapi/v1/progress":
			// Reported while the draw runs, the way a real server does; nothing asserts on
			// it, but a server that 404s here is a different fixture (`?nointerrupt=1`
			// territory) and would make the draw report "unsupported" instead of progress.
			fmt.Fprint(w, `{"progress":0.1,"state":{"sampling_step":1,"sampling_steps":20}}`)
		case "/sdapi/v1/txt2img":
			started <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-release:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv, started
}

// One draw at a time is a property of the *app*, and this is the half that matters most: a
// refused second draw must leave the running one alone. Taking the slot before registerDraw
// is what makes that true — a draw that registered first and was refused afterwards would
// have cancelled the registration of the picture the user was already waiting for, so
// "please wait" would have cost them the thing it asked them to wait for.
func TestASecondDrawIsRefusedWithoutEndingTheOneRunning(t *testing.T) {
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

	running := make(chan error, 1)
	go func() {
		_, err := a.RegenerateImage(original.ID, *original.Generation)
		running <- err
	}()
	select {
	case <-server.drawing:
	case <-time.After(5 * time.Second):
		t.Fatal("the first redraw never reached the server")
	}

	if _, err := a.RegenerateImage(original.ID, *original.Generation); !errors.Is(err, images.ErrDrawInProgress) {
		t.Fatalf("second draw err = %v, want ErrDrawInProgress", err)
	}
	// The first one is still registered and still stoppable. This is the assertion the
	// ordering exists for: registering the refused draw would have replaced this entry, and
	// Stop would then report nothing to stop while the picture was still being drawn.
	if got := a.StopImageDraw(original.ID); !got.WasDrawing {
		t.Fatal("the refused draw took the running one's registration with it — Stop found nothing")
	}
	select {
	case err := <-running:
		if !errors.Is(err, images.ErrCancelled) {
			t.Errorf("the running draw ended with %v, want the cancellation sentinel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the running draw never ended after Stop")
	}

	// And the slot is free again, which is what keeps one refusal — or one failed draw — from
	// wedging image generation for the rest of the session.
	release, ok := a.tryDraw()
	if !ok {
		t.Fatal("the draw slot was never released")
	}
	release()
}

// The settings panel's test draw is a draw like any other, and it has to be stoppable: on the
// one-shot kind it loads the model before it draws anything, which is minutes on a CPU, and
// the panel used to sit on a running button with nothing to press. The id is the fixed one the
// backend registers it under, because a test has no attachment to name it by.
func TestTheSettingsTestDrawCanBeStopped(t *testing.T) {
	srv, started := alwaysDrawing(t)
	settings := config.Default()
	settings.Images.Enabled = true
	settings.Images.BaseURL = srv.URL

	a := newSettingsTestApp(t, settings, t.TempDir())
	// A bound call needs the app's lifetime context; there is no Wails runtime here.
	a.ctx = context.Background()
	a.rebuildServices(settings)

	done := make(chan error, 1)
	go func() {
		_, err := a.TestImageBackend()
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the test draw never reached the server")
	}

	result := a.StopImageDraw(imageTestDrawID)
	if !result.WasDrawing {
		t.Errorf("Stop found nothing under %q while the test draw was in flight", imageTestDrawID)
	}
	select {
	case err := <-done:
		if !errors.Is(err, images.ErrCancelled) {
			t.Fatalf("err = %v, want ErrCancelled — a stopped test is not a broken server", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the test draw is still waiting after Stop")
	}

	release, ok := a.tryDraw()
	if !ok {
		t.Fatal("the draw slot was not released by the stopped test")
	}
	release()
}
