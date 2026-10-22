package main

import "testing"

// The bridge opens a URL in the user's browser on click. Two of the three callers
// pass text a model or a scraped page produced, so this is the one place a scheme
// the OS would act on has to be refused.
func TestOnlyWebURLsReachTheOperatingSystem(t *testing.T) {
	cases := []struct {
		target string
		want   bool
		why    string
	}{
		{"https://example.com/a?b=c", true, "an ordinary citation"},
		{"http://127.0.0.1:1234/v1", true, "a local server's address"},
		{"  https://example.com/path  ", true, "whitespace around a pasted link"},
		{"HTTPS://EXAMPLE.COM", true, "a scheme with capitals"},
		{"https://", false, "no host to open"},
		{"file:///etc/passwd", false, "a local file, in whatever program claims it"},
		{"file://host/share/secret.txt", false, "a file on a network share"},
		{"smb://server/share", false, "a Windows file-sharing handler"},
		{"javascript:alert(1)", false, "script the webview must never run"},
		{"mailto:someone@example.com", false, "a mail handler this app never needs"},
		{"example.com", false, "no scheme at all"},
		{"", false, "empty"},
	}
	for _, tc := range cases {
		if got := isWebURL(tc.target); got != tc.want {
			t.Errorf("isWebURL(%q) = %v, want %v (%s)", tc.target, got, tc.want, tc.why)
		}
	}
}
