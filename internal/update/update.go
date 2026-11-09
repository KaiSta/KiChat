// Package update answers one question: has a newer KiChat been released?
//
// It reads the project's own GitHub release list, which is also where the five
// downloads live, so the answer carries the release page as well as the tag — the
// window turns that into a link instead of the app trying to install anything. A
// .deb waits for `apt install`, a Windows build for its installer, and the macOS
// .dmg for a drag and a `xattr` line; three package managers reimplemented badly is
// not what a download link is for.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"kichat/internal/uimsg"
)

// Repo is the project's repository, owner and name.
const Repo = "KaiSta/KiChat"

// PageURL is the release list a user is sent to when no single release could be
// named — an unreachable lookup is not a reason to show no link at all.
const PageURL = "https://github.com/" + Repo + "/releases"

// LatestURL is the one release a check asks for. GitHub's `latest` is the newest
// non-draft, non-prerelease release, which is exactly what a tag of this project
// is: the release workflow triggers on `[0-9]*` and every such release carries the
// installers.
const LatestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// userAgent names this app to GitHub, which refuses a request that has none.
const userAgent = "kichat-update-check"

// Release is the part of a published release this app has any use for.
type Release struct {
	Tag string `json:"tag_name"`
	URL string `json:"html_url"`
	// Body is the release page's own text — the workflow writes it as a short bullet
	// list for users — so the panel can say what changed before sending anyone to the
	// page. It may be empty; GitHub permits a release with no notes.
	Body      string `json:"body"`
	Published string `json:"published_at"`
}

// Latest reads the newest published release from endpoint, or from LatestURL when
// endpoint is empty. A nil client is http.DefaultClient; the caller's context bounds
// the request, which is how the window keeps a slow lookup from holding the panel
// open.
func Latest(ctx context.Context, endpoint string, client *http.Client) (Release, error) {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = LatestURL
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return Release{}, uimsg.Errorf("update.releaseQuery", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The API answers 403 once the unauthenticated rate limit is exhausted and
		// says so in a header. Naming it keeps a quota from reading as "the release
		// page is down" — the same distinction the llama.cpp lookup makes.
		if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return Release{}, uimsg.Errorf("update.rateLimited")
		}
		return Release{}, uimsg.Errorf("update.releaseStatus", resp.StatusCode)
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return Release{}, uimsg.Errorf("update.releaseRead", err)
	}
	if strings.TrimSpace(release.Tag) == "" {
		// A release with no tag cannot be ordered or named, so it is not an answer.
		return Release{}, uimsg.Errorf("update.noTag")
	}
	if strings.TrimSpace(release.URL) == "" {
		// The tag is enough to build the page address, and the link is the point of
		// the whole check: a body that named no `html_url` still gets one.
		release.URL = "https://github.com/" + Repo + "/releases/tag/" + release.Tag
	}
	return release, nil
}

// Newer reports whether latest is a later release than current.
//
// The two tags are this project's own (`0.7.17`), so the comparison is the number
// and not the text: `0.10.0` sorts below `0.9.9` as strings, which would hide a
// release on every version whose first component passes a digit boundary. A tag
// that cannot be read as a number falls back to "different means newer" — the
// window prints both tags, so the user sees what they would get, and refusing over
// an unusual tag would hide the real ones.
//
// No current version is not an update: there is no build to replace — and a
// development build is refused by the caller rather than here, because it is not a
// release at all.
func Newer(current, latest string) bool {
	current, latest = strings.TrimSpace(current), strings.TrimSpace(latest)
	if current == "" || latest == "" {
		return false
	}
	c, cok := parseVersion(current)
	l, lok := parseVersion(latest)
	if cok && lok {
		for i := 0; i < len(c) || i < len(l); i++ {
			var a, b int
			if i < len(c) {
				a = c[i]
			}
			if i < len(l) {
				b = l[i]
			}
			if a != b {
				return b > a
			}
		}
		return false
	}
	return current != latest
}

// parseVersion reads `0.7.17` — and `v0.7.17`, because a tag written with a prefix
// is still the same release — as its numeric components. Anything else (a branch
// name, a `-rc1` suffix) is refused rather than guessed at, which is what sends
// Newer to its textual fallback.
func parseVersion(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(v), "v"), "V")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return nil, false
	}
	numbers := make([]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, false
		}
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				return nil, false
			}
			n = n*10 + int(r-'0')
		}
		numbers = append(numbers, n)
	}
	return numbers, true
}

// Describe renders a release for a log line. It exists so a reader of the start
// trace can tell which build was involved without a struct dump.
func (r Release) Describe() string {
	return fmt.Sprintf("%s (%s)", r.Tag, r.URL)
}
