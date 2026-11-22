package research

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
)

// TestLiveDDGInterstitialMarkup fetches one real search and says what came back: whether it is the
// interstitial, and whether the parse finds a check in it. Its body is written to the gitignored
// scratch directory so a layout change can be read rather than guessed at.
//
// Live: it reaches duckduckgo.com and costs nothing but the request.
func TestLiveDDGInterstitialMarkup(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("KICHAT_LIVE=1 is required: this reaches duckduckgo.com")
	}
	query := "Bürostuhl hohe Rückenlehne Kopfstütze Netzrücken"
	provider := NewDuckDuckGoProvider("")
	form := url.Values{}
	form.Set("q", query)
	form.Set("b", "")
	form.Set("l", "de-de")
	form.Set("df", "m")

	resp, err := provider.client.postForm(context.Background(), provider.Endpoint, form, "de")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.MkdirAll("../../scratch", 0o755); err != nil {
		t.Fatalf("scratch: %v", err)
	}
	if err := os.WriteFile("../../scratch/ddg-interstitial.html", body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Logf("status=%s bytes=%d content-type=%q", resp.Status, len(body), resp.Header.Get("Content-Type"))
	t.Logf("isDDGChallenge=%v", isDDGChallenge(string(body)))
	challenge, parsed := parseDDGChallenge(string(body), query, "de", provider.Endpoint)
	t.Logf("parsed=%v tiles=%d action=%q instruction=%q fields=%d",
		parsed, len(challenge.Tiles), challenge.Action, challenge.Instruction, len(challenge.Fields))
	if !parsed {
		t.Fatalf("the live interstitial no longer parses; the body is in ../../scratch/ddg-interstitial.html")
	}

	// One tile, over the same client: this is the request that has to answer with a picture, or the
	// grid the window draws is nine broken squares.
	rememberChallenge(challenge)
	t.Cleanup(ClearPendingChallenge)
	picture, contentType, err := provider.fetchTile(context.Background(), challenge.Tiles[0].Name)
	if err != nil {
		t.Fatalf("tile %s (%s): %v", challenge.Tiles[0].Name, challenge.Tiles[0].URL, err)
	}
	t.Logf("tile %s: %d bytes of %s", challenge.Tiles[0].Name, len(picture), contentType)

	// The answer itself. Which squares hold a duck is not knowable here, so this measures whether
	// the request is readable at all: every ticked square goes back to the action, and a 400 means
	// the host could not read the answer rather than that it was wrong.
	selections := make([]string, 0, len(challenge.Tiles))
	for _, tile := range challenge.Tiles {
		selections = append(selections, tile.Name)
	}
	results, err := provider.SolveChallenge(context.Background(), selections)
	var httpErr *httpError
	switch {
	case err == nil:
		t.Logf("the answer was accepted and returned %d results", len(results))
	case errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusBadRequest:
		t.Fatalf("the host could not read the answer: %v", err)
	default:
		t.Logf("the answer was read and judged (these squares were guessed): %v", err)
	}
}
