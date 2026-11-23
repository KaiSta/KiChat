package main

import (
	"os"
	"strings"
	"testing"

	"kichat/internal/update"
)

// TestLiveAppUpdate is the one thing the fixtures cannot say: that the endpoint, the
// headers and the JSON field names in internal/update still agree with what GitHub
// serves today, and that the reading the panel receives is coherent.
//
// It is opt-in because it spends a request out of GitHub's 60-an-hour anonymous budget:
//
//	KICHAT_LIVE=1 go test ./ -run TestLiveAppUpdate -v
//
// The version under test is the one in VERSION, so the answer is also what the About
// row will show about the release being cut: "latest is X" either matches it or is
// behind it.
func TestLiveAppUpdate(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") == "" {
		t.Skip("KICHAT_LIVE is not set")
	}
	raw, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatalf("VERSION could not be read: %v", err)
	}
	setVersion(t, strings.TrimSpace(string(raw)))

	reading, err := (&App{}).CheckAppUpdate()
	if err != nil {
		t.Fatalf("CheckAppUpdate returned an error instead of a reading: %v", err)
	}
	if !reading.Checked {
		t.Fatal("the reading does not say a check was made")
	}
	if reading.Error != "" {
		t.Fatalf("the real lookup failed: %s (link %s)", reading.Error, reading.URL)
	}
	if reading.Latest == "" {
		t.Fatalf("the lookup answered but named no release: %+v", reading)
	}
	if !strings.Contains(reading.URL, "/releases/") {
		t.Errorf("the reading points at %q, which is not a release page", reading.URL)
	}
	if reading.Published == "" {
		t.Errorf("release %s carries no publication date: %+v", reading.Latest, reading)
	}
	if got, want := reading.Available, update.Newer(reading.Current, reading.Latest); got != want {
		t.Errorf("Available is %v for %s against %s, want %v", got, reading.Current, reading.Latest, want)
	}
	t.Logf("running %s, latest published %s (%s), notes %d bytes, available %v",
		reading.Current, reading.Latest, reading.Published, len(reading.Notes), reading.Available)
}
