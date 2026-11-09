package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/update"
	"kichat/internal/version"
)

// setVersion pins the linked version for one test and puts it back afterwards: the
// variable is what the linker sets, and every other test in this package reads it.
func setVersion(t *testing.T, v string) {
	t.Helper()
	previous := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = previous })
}

// releaseLookup answers with one release, or with an error, and counts how often it
// was asked.
func releaseLookup(t *testing.T, release update.Release, err error, calls *int) func(context.Context) (update.Release, error) {
	t.Helper()
	return func(context.Context) (update.Release, error) {
		*calls++
		return release, err
	}
}

// The whole path the About panel depends on: the running version, the published tag,
// and the page the download link opens.
func TestCheckAppUpdateOffersANewerRelease(t *testing.T) {
	setVersion(t, "0.7.16")
	calls := 0
	a := &App{}
	a.appUpdateLookup = releaseLookup(t, update.Release{
		Tag:       "0.7.17",
		URL:       "https://github.com/KaiSta/KiChat/releases/tag/0.7.17",
		Body:      "- A badge in the sidebar\n- A daily check for new releases",
		Published: "2026-11-09T20:08:57Z",
	}, nil, &calls)

	reading, err := a.CheckAppUpdate()
	if err != nil {
		t.Fatalf("CheckAppUpdate: %v", err)
	}
	if !reading.Checked || !reading.Available {
		t.Errorf("reading = %+v, want a newer release offered", reading)
	}
	if reading.Current != "0.7.16" || reading.Latest != "0.7.17" {
		t.Errorf("reading = %+v, want 0.7.16 against 0.7.17", reading)
	}
	if reading.URL != "https://github.com/KaiSta/KiChat/releases/tag/0.7.17" {
		t.Errorf("url = %q, want the release page the download link opens", reading.URL)
	}
	if reading.Error != "" {
		t.Errorf("a lookup that answered reported a reason: %q", reading.Error)
	}
	// The notes are what the panel shows before the link: a tag alone says a release
	// exists, not what it changes.
	if !strings.Contains(reading.Notes, "A badge in the sidebar") {
		t.Errorf("notes = %q, want the release's own text", reading.Notes)
	}
	if calls != 1 {
		t.Errorf("lookup was made %d times, want once", calls)
	}

	// The panel asks for the cached copy when it opens, so that has to carry the same
	// answer or the marker would never appear.
	cached := a.GetAppUpdate()
	if !cached.Available || cached.Latest != "0.7.17" {
		t.Errorf("GetAppUpdate = %+v, want the reading the check just made", cached)
	}
}

// The same release as the one running is not an update, and it still names the page:
// a user who wants to read the notes of the version they are on can.
func TestCheckAppUpdateDoesNotOfferTheRunningRelease(t *testing.T) {
	setVersion(t, "0.7.17")
	calls := 0
	a := &App{}
	a.appUpdateLookup = releaseLookup(t, update.Release{
		Tag: "0.7.17",
		URL: "https://github.com/KaiSta/KiChat/releases/tag/0.7.17",
	}, nil, &calls)

	reading, err := a.CheckAppUpdate()
	if err != nil {
		t.Fatalf("CheckAppUpdate: %v", err)
	}
	if reading.Available {
		t.Errorf("0.7.17 was offered over an installed 0.7.17: %+v", reading)
	}
	if !reading.Checked || reading.Latest != "0.7.17" {
		t.Errorf("reading = %+v, want the checked, current release", reading)
	}
}

// Asking failed is not the same answer as being up to date. The panel needs the two
// apart, and it keeps a link rather than nothing: the release list is where the
// download is either way.
func TestCheckAppUpdateSeparatesFailureFromBeingCurrent(t *testing.T) {
	setVersion(t, "0.7.17")
	calls := 0
	a := &App{}
	a.appUpdateLookup = releaseLookup(t, update.Release{}, context.DeadlineExceeded, &calls)

	reading, err := a.CheckAppUpdate()
	if err != nil {
		t.Fatalf("CheckAppUpdate: %v", err)
	}
	if !reading.Checked {
		t.Error("a failed lookup left the reading as \"not asked\", so the panel cannot say it tried")
	}
	if reading.Error == "" {
		t.Error("a failed lookup reported no reason")
	}
	if reading.Available || reading.Latest != "" {
		t.Errorf("a lookup that could not ask still named a release: %+v", reading)
	}
	if reading.URL != update.PageURL {
		t.Errorf("url = %q, want the release list as the fallback", reading.URL)
	}
}

// A development build is not a release: there is no tag to compare, so nothing is
// offered and no request is spent finding that out.
func TestADevBuildIsNotComparedWithARelease(t *testing.T) {
	setVersion(t, "dev")
	calls := 0
	a := &App{}
	a.appUpdateLookup = releaseLookup(t, update.Release{Tag: "0.7.17"}, nil, &calls)

	reading, err := a.CheckAppUpdate()
	if err != nil {
		t.Fatalf("CheckAppUpdate: %v", err)
	}
	if !reading.Checked || !reading.DevBuild {
		t.Errorf("reading = %+v, want the dev build marked as such", reading)
	}
	if reading.Available || reading.Latest != "" {
		t.Errorf("a development build was offered a release: %+v", reading)
	}
	if calls != 0 {
		t.Errorf("a development build spent %d requests, want none", calls)
	}
}

// The daily check waits out the day: a look that has just happened is not repeated
// because the app started again.
func TestTheDailyCheckWaitsOutTheDay(t *testing.T) {
	setVersion(t, "0.7.16")
	calls := 0
	a := &App{settings: config.Default()}
	a.settings.Updates.CheckDaily = true
	a.settings.Updates.LastCheckUnix = time.Now().Unix()
	a.appUpdateLookup = releaseLookup(t, update.Release{Tag: "0.7.17"}, nil, &calls)

	a.startUpdateCheck()
	// The not-due path starts no goroutine at all, so there is nothing to wait for.
	if calls != 0 {
		t.Errorf("a check made %d requests although one had just been made", calls)
	}
}

// A switch that is off looks at nothing, whatever the clock says.
func TestTheDailyCheckDoesNothingWhenSwitchedOff(t *testing.T) {
	setVersion(t, "0.7.16")
	calls := 0
	a := &App{settings: config.Default()}
	a.appUpdateLookup = releaseLookup(t, update.Release{Tag: "0.7.17"}, nil, &calls)

	a.startUpdateCheck()
	if calls != 0 {
		t.Errorf("a switched-off daily check made %d requests", calls)
	}
}

// A day after the last look it asks, once, and the reading it leaves behind is what the
// sidebar badge and the About row are set from.
func TestTheDailyCheckLooksWhenADayHasPassed(t *testing.T) {
	setVersion(t, "0.7.16")
	asked := make(chan struct{}, 1)
	a := &App{settings: config.Default()}
	a.settings.Updates.CheckDaily = true
	a.settings.Updates.LastCheckUnix = time.Now().Add(-25 * time.Hour).Unix()
	a.appUpdateLookup = func(context.Context) (update.Release, error) {
		asked <- struct{}{}
		return update.Release{
			Tag: "0.7.17",
			URL: "https://github.com/KaiSta/KiChat/releases/tag/0.7.17",
		}, nil
	}

	a.startUpdateCheck()
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the daily check never asked")
	}

	// The goroutine continues after the request; the cached reading is what the window
	// reads, so that is what has to arrive.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if reading := a.GetAppUpdate(); reading.Available && reading.Latest == "0.7.17" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the check ran but the reading is %+v", a.GetAppUpdate())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The notes preview is trimmed and bounded, and cut on a rune boundary: a release body
// is written for a page and can be long, and half a multi-byte character is not a
// preview of anything.
func TestUpdateNotesAreBoundedAndCutOnACharacter(t *testing.T) {
	if got := previewNotes("  - a fix\n"); got != "- a fix" {
		t.Errorf("previewNotes trimmed to %q, want the body without its whitespace", got)
	}
	long := strings.Repeat("ü", notesPreviewRunes+50)
	got := previewNotes(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a body of %d runes was not marked as a preview", len([]rune(long)))
	}
	if n := len([]rune(strings.TrimSuffix(got, "…"))); n != notesPreviewRunes {
		t.Errorf("preview kept %d runes, want %d", n, notesPreviewRunes)
	}
}

// Before any check the reading is empty, and that is not "current": the panel shows
// the Check button rather than a claim it has no basis for.
func TestNoReadingMeansNotChecked(t *testing.T) {
	a := &App{}
	if reading := a.GetAppUpdate(); reading.Checked {
		t.Errorf("a fresh App reported a reading: %+v", reading)
	}
}
