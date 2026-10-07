package llm

import "testing"

// TestCustomProviderChatPathsAreNotDuplicated: a base URL ending in /v1 used to
// make the primary and the fallback chat paths identical, so a 404 retired one
// and retried the exact same URL instead of trying the other shape.
func TestCustomProviderChatPathsAreNotDuplicated(t *testing.T) {
	cases := []struct {
		baseURL string
		want    []string
	}{
		{
			baseURL: "http://host:8080",
			want:    []string{"http://host:8080/v1/chat/completions", "http://host:8080/chat/completions"},
		},
		{
			baseURL: "http://host:8080/v1",
			want:    []string{"http://host:8080/v1/chat/completions"},
		},
	}
	for _, tc := range cases {
		p := newCustomProvider("custom", "Custom", tc.baseURL, "")
		if len(p.chatPaths) != len(tc.want) {
			t.Errorf("%s: chatPaths = %v, want %v", tc.baseURL, p.chatPaths, tc.want)
			continue
		}
		for i := range tc.want {
			if p.chatPaths[i] != tc.want[i] {
				t.Errorf("%s: chatPaths[%d] = %q, want %q", tc.baseURL, i, p.chatPaths[i], tc.want[i])
			}
		}
	}
}
