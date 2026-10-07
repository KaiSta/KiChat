package llm

import (
	"strings"
	"testing"
)

// TestImageDataURLSniffsTheMediaType keeps a JPEG from being declared a PNG, which
// servers that validate the image's declared type would reject.
func TestImageDataURLSniffsTheMediaType(t *testing.T) {
	cases := []struct {
		name   string
		b64    string
		prefix string
	}{
		{"png", "iVBORw0KGgoAAAANSUhEUg", "data:image/png;base64,"},
		{"jpeg", "/9j/4AAQSkZJRgABAQ", "data:image/jpeg;base64,"},
		{"gif", "R0lGODlhAQABAIAA", "data:image/gif;base64,"},
		{"webp", "UklGRgAAAABXRUJQ", "data:image/webp;base64,"},
		{"unknown falls back to png", "AAAAuknownpayload", "data:image/png;base64,"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := imageDataURL(tc.b64)
			if !strings.HasPrefix(got, tc.prefix) {
				t.Errorf("got %q, want prefix %q", got, tc.prefix)
			}
			if !strings.HasSuffix(got, tc.b64) {
				t.Errorf("payload was not preserved in %q", got)
			}
		})
	}
}
