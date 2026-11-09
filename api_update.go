package main

// The app's side of "is there a newer KiChat?".
//
// The answer is a release tag and the page the five downloads are on — nothing here
// installs anything. A .deb is placed by `apt install`, a Windows build by its
// installer, and the macOS .dmg by a drag and one `xattr` line, so an app that tried
// to replace itself would be reimplementing three package managers on a build it
// cannot verify. The link is the honest half of the feature; see internal/update for
// the lookup and the comparison.
//
// The reading is cached on the App for the same reason the llama.cpp one is: it costs
// a request to a rate-limited API, and the answer changes when a release is published.
// Settings asks for the cached copy when the panel opens and spends a request only
// when one has not been made yet, or when the user clicks Check again.

import (
	"context"
	"strings"
	"time"

	"kichat/internal/update"
	"kichat/internal/version"
)

// AppUpdate is the answer to "is there a newer KiChat?", as Settings -> About reads
// it. "Up to date" and "not asked" are different statements, which is what Checked
// separates: only one of them is a claim about the newest release.
type AppUpdate struct {
	Checked bool `json:"checked"`
	// Available marks a newer release than the one running.
	Available bool `json:"available"`
	// Current is the running build's version, and Latest the newest published tag.
	Current string `json:"current,omitempty"`
	Latest  string `json:"latest,omitempty"`
	// URL is the newest release's page. It is filled even when no update is
	// available, so a user who wants to read the notes can, and it falls back to the
	// release list rather than to nothing.
	URL string `json:"url,omitempty"`
	// Published is when that release went up, as GitHub reports it.
	Published string `json:"publishedAt,omitempty"`
	// Notes is a bounded preview of the release page's own text, so the panel can say
	// what changed before the Download button. It is a preview on purpose: the release
	// page renders the whole thing with its links, and a notes box in Settings that
	// carried a whole changelog would be a worse reader than the page it links to.
	Notes string `json:"notes,omitempty"`
	// DevBuild marks a build that is not a release at all — `go build` leaves the
	// version as "dev" — so there is no tag to compare and nothing to offer. It is a
	// field rather than an error because nothing went wrong: a local build is what it
	// says it is.
	DevBuild bool `json:"devBuild,omitempty"`
	// Error is why no newest release could be established — no network, a rate
	// limit, a refused status. It is kept apart from Available=false because
	// "current" is a claim this app must not make when it could not ask.
	Error string `json:"error,omitempty"`
}

// appUpdateTimeout bounds the lookup. It is a JSON document of a few kilobytes, so a
// request that has not answered in ten seconds is not worth the panel waiting for.
const appUpdateTimeout = 10 * time.Second

// CheckAppUpdate asks GitHub for the newest release and reports whether it is newer
// than this build.
//
// It does not call ensure(): which build is running is a question that has to answer
// even while the history is locked — that is exactly when someone checks whether a
// newer version fixes what they are looking at. A failed lookup is reported in the
// reading rather than as an error, because it is a row in the panel and not a failure
// of the call.
func (a *App) CheckAppUpdate() (AppUpdate, error) {
	current := strings.TrimSpace(version.Version)
	if current == "" || current == "dev" {
		// A development build is not a release, so no release is newer than it.
		// Asking would spend a request to report a comparison with no meaning.
		reading := AppUpdate{Checked: true, Current: current, DevBuild: true}
		a.setAppUpdate(reading)
		return reading, nil
	}

	// Bounded by the app's own context, not a fresh one: a lookup still in flight
	// when the window closes is answered by the store's own shutdown rather than left
	// to outlive it. Tests call this on an App whose ctx is nil.
	parent := a.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, appUpdateTimeout)
	defer cancel()

	lookup := a.appUpdateLookup
	if lookup == nil {
		lookup = defaultAppUpdateLookup
	}
	release, err := lookup(ctx)
	if err != nil {
		reading := AppUpdate{Checked: true, Current: current, URL: update.PageURL, Error: err.Error()}
		a.setAppUpdate(reading)
		return reading, nil
	}
	reading := AppUpdate{
		Checked:   true,
		Current:   current,
		Latest:    release.Tag,
		URL:       release.URL,
		Published: release.Published,
		Notes:     previewNotes(release.Body),
		Available: update.Newer(current, release.Tag),
	}
	a.setAppUpdate(reading)
	return reading, nil
}

// notesPreviewRunes bounds the notes the bridge carries. A release body is written for
// a page and can be long; the panel shows a few lines and links to the rest, so sending
// more than this would be bytes nobody reads on every check.
const notesPreviewRunes = 1200

// previewNotes trims a release body to the part the panel shows, on a rune boundary so
// a multi-byte character is never cut in half.
func previewNotes(body string) string {
	body = strings.TrimSpace(body)
	runes := []rune(body)
	if len(runes) <= notesPreviewRunes {
		return body
	}
	return strings.TrimSpace(string(runes[:notesPreviewRunes])) + "…"
}

// defaultAppUpdateLookup is the real lookup, and the seam a test replaces.
func defaultAppUpdateLookup(ctx context.Context) (update.Release, error) {
	return update.Latest(ctx, "", nil)
}

// GetAppUpdate reports the last reading without spending a request. Zero means no
// check has been made in this session, which the panel distinguishes from being
// current.
func (a *App) GetAppUpdate() AppUpdate {
	a.appUpdateMu.Lock()
	defer a.appUpdateMu.Unlock()
	return a.appUpdate
}

func (a *App) setAppUpdate(reading AppUpdate) {
	a.appUpdateMu.Lock()
	a.appUpdate = reading
	a.appUpdateMu.Unlock()
}
