package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kichat/internal/uimsg"
)

// The comparison is numeric, at the digit boundary a textual one gets wrong, and it
// refuses the two cases where there is nothing to compare: no installed version, and
// a tag written unlike this project's releases.
func TestNewerComparesTheReleaseNumberNotTheText(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"0.7.16", "0.7.17", true},
		{"0.7.17", "0.7.17", false},
		{"0.7.17", "0.7.16", false},
		// "0.10.0" < "0.9.9" as text, which would hide the release.
		{"0.9.9", "0.10.0", true},
		{"0.10.0", "0.9.9", false},
		// A tag written with a prefix, or a shorter one, is read as the number it is.
		{"v0.7.17", "0.7.18", true},
		{"0.7", "0.7.1", true},
		{"0.7.1", "0.7", false},
		// Nothing installed is not an update: there is no build to replace.
		{"", "0.7.18", false},
		{"0.7.17", "", false},
		// A suffix this app cannot order reports the tags as different, so an
		// unusual release does not silently disable the check.
		{"0.7.17", "0.8.0-rc1", true},
		{"0.8.0-rc1", "0.8.0-rc1", false},
	}
	for _, tc := range cases {
		if got := Newer(tc.current, tc.latest); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

// The lookup itself: what the endpoint says, and that the headers GitHub requires are
// on the request. Nothing here reaches GitHub.
func TestLatestReadsTheNewestRelease(t *testing.T) {
	var gotUA, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotAccept = r.Header.Get("User-Agent"), r.Header.Get("Accept")
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     "0.7.18",
			"name":         "KiChat 0.7.18",
			"html_url":     "https://github.com/KaiSta/KiChat/releases/tag/0.7.18",
			"body":         "- A badge in the sidebar\n- A daily check for new releases",
			"published_at": "2026-11-12T10:00:00Z",
		})
	}))
	defer srv.Close()

	release, err := Latest(context.Background(), srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if release.Tag != "0.7.18" || release.Published != "2026-11-12T10:00:00Z" {
		t.Errorf("release = %+v, want the published tag and its date", release)
	}
	if release.URL != "https://github.com/KaiSta/KiChat/releases/tag/0.7.18" {
		t.Errorf("url = %q, want the release page the body named", release.URL)
	}
	// The notes travel with the tag: the panel says what changed before sending anyone
	// to the page, and a release with no body is allowed to have none.
	if !strings.Contains(release.Body, "A badge in the sidebar") {
		t.Errorf("body = %q, want the release's own notes", release.Body)
	}
	if gotUA == "" {
		t.Error("no User-Agent was sent; GitHub refuses such a request")
	}
	if gotAccept == "" {
		t.Error("no Accept header was sent")
	}
}

// A tag whose body named no page still gets one built from the tag, because the link
// is the whole point of the check and the tag is what identifies the release.
func TestLatestBuildsTheReleasePageWhenTheBodyOmitsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "0.7.18"})
	}))
	defer srv.Close()

	release, err := Latest(context.Background(), srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if want := "https://github.com/KaiSta/KiChat/releases/tag/0.7.18"; release.URL != want {
		t.Errorf("url = %q, want %q", release.URL, want)
	}
}

// A refused lookup is a reason, not an empty answer: the panel says why it could not
// ask, and the rate limit is told apart from a bad status because it is the one a
// user can wait out.
func TestLatestNamesWhyItCouldNotAsk(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header map[string]string
		want   string
	}{
		{"rate limit", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, "update.rateLimited"},
		{"missing repository", http.StatusNotFound, nil, "update.releaseStatus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			_, err := Latest(context.Background(), srv.URL, srv.Client())
			if err == nil {
				t.Fatal("a refused lookup answered with no error")
			}
			message, ok := uimsg.Parse(err.Error())
			if !ok || message.Code != tc.want {
				t.Errorf("error = %q, want the code %q", err.Error(), tc.want)
			}
		})
	}
}

// A release that named no tag cannot be ordered against anything, so it is refused
// rather than reported as the newest of nothing.
func TestLatestRefusesAReleaseWithoutATag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"name": "KiChat"})
	}))
	defer srv.Close()

	if _, err := Latest(context.Background(), srv.URL, srv.Client()); err == nil {
		t.Fatal("a release with no tag was accepted")
	}
}
