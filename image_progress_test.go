package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"kichat/internal/events"
	"kichat/internal/images"
)

// TestTheProgressEventCarriesWhatTheWindowReads pins the one interface in this feature that
// neither side's compiler checks. The window subscribes by name and reads its fields by key;
// a renamed field is not an error anywhere — it is a progress row that quietly stops
// appearing while every draw still works, which is the failure that is hardest to notice.
//
// The preview is asserted as a `data:` URL because that is not a style choice: the webview
// loads from an embedded assets handler, and a path would be blocked by the same origin rule
// that stops any other local file from being read.
func TestTheProgressEventCarriesWhatTheWindowReads(t *testing.T) {
	payload := drawProgressPayload("att-1", "chat-1", images.Progress{
		Step: 3, Steps: 12, Pass: 2, Passes: 2, Preview: []byte{1, 2, 3}, PreviewMime: images.MimeJPEG,
	}, false)

	for _, key := range []string{"drawId", "chatId", "step", "steps", "pass", "passes", "done", "preview"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("the payload has no %q; the window reads it by that name", key)
		}
	}
	// A second pass restarts the step counter, so a report that did not say which pass
	// it belonged to would read as progress going backwards.
	if payload["pass"] != 2 || payload["passes"] != 2 {
		t.Errorf("the pass did not survive: %+v", payload)
	}
	if payload["drawId"] != "att-1" || payload["chatId"] != "chat-1" {
		t.Errorf("the payload does not say which draw it belongs to: %+v", payload)
	}
	if payload["step"] != 3 || payload["steps"] != 12 {
		t.Errorf("the fraction did not survive: %+v", payload)
	}
	want := "data:" + images.MimeJPEG + ";base64," + base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	if payload["preview"] != want {
		t.Errorf("preview = %v, want %s", payload["preview"], want)
	}

	// A report with no picture in it — which is what a server without a preview route gives
	// — must not carry an empty or half-built data URL: the window would try to paint it and
	// show a broken image.
	stepsOnly := drawProgressPayload("att-1", "chat-1", images.Progress{Step: 4, Steps: 12}, false)
	if _, ok := stepsOnly["preview"]; ok {
		t.Errorf("a report without a preview carries one anyway: %+v", stepsOnly)
	}
	// An ordinary draw reports neither field, and the window reads that as one pass
	// rather than as a missing part of the sentence.
	if stepsOnly["pass"] != 1 || stepsOnly["passes"] != 1 {
		t.Errorf("an ordinary draw did not normalise to one pass: %+v", stepsOnly)
	}
	// And the same rule for a preview with no format: the bytes are unusable without one.
	noMime := drawProgressPayload("att-1", "chat-1", images.Progress{Preview: []byte{1}}, false)
	if _, ok := noMime["preview"]; ok {
		t.Errorf("a preview without a format was sent as a data URL: %+v", noMime)
	}

	// A caption is a phase rather than a step: the counter has not started, so "step 0 of
	// 0" would be a progress bar pretending to have begun. The phase is a key of its own,
	// and an ordinary report must not carry one — a stale value would leave the row saying
	// "captioning" through the whole draw.
	captioning := drawProgressPayload("att-1", "chat-1", images.Progress{Phase: images.PhaseCaptioning}, false)
	if captioning["phase"] != images.PhaseCaptioning {
		t.Errorf("the captioning phase did not survive: %+v", captioning)
	}
	if _, ok := stepsOnly["phase"]; ok {
		t.Errorf("an ordinary step report carries a phase: %+v", stepsOnly)
	}

	// The end of a draw is the same event with `done` set, and nothing else in it: it exists
	// so a progress row cannot outlive the draw it describes.
	end := drawProgressPayload("att-1", "chat-1", images.Progress{}, true)
	if end["done"] != true {
		t.Errorf("the end-of-draw payload does not say so: %+v", end)
	}
	if _, ok := end["preview"]; ok {
		t.Errorf("the end-of-draw payload carries a picture: %+v", end)
	}
}

// TestTheProgressEventIsDeclaredOnBothSidesOfTheBridge is the same rule internal/events exists
// for: the Go constant and the frontend's event map are two spellings of one name, and the
// package reads the frontend's file to check them. This asserts the new event is really in
// that list, so a listener in the window cannot be left waiting for a name nothing emits.
func TestTheProgressEventIsDeclaredOnBothSidesOfTheBridge(t *testing.T) {
	found := false
	for _, name := range events.All() {
		if name == events.ImageProgress {
			found = true
		}
	}
	if !found {
		t.Fatal("image:progress is not in events.All(), so the frontend's map check cannot see it")
	}
	if !strings.HasPrefix(events.ImageProgress, "image:") {
		t.Errorf("event name = %q, want an image: prefix", events.ImageProgress)
	}
	if _, err := json.Marshal(drawProgressPayload("a", "c", images.Progress{}, false)); err != nil {
		t.Fatalf("the payload does not marshal: %v", err)
	}
}
