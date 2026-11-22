package research

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

// TestLiveDDGInterstitialMarkup fetches one real search and says what came back: whether it is the
// interstitial, and whether the parse finds a check in it. Its body is written to the gitignored
// scratch directory so a layout change can be read rather than guessed at.
//
// The answer is posted too, and its reply is captured whole with a one-screen summary beside it.
// That is the measurement the wording depends on: a verdict (the host read the ticks and drew
// another puzzle) and an automation refusal (the answer was never treated as an answer) both come
// back as check markup, so what separates them is recorded rather than assumed — whether the squares
// are new, what the modal says, and what the answer's request claimed about where it came from.
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
	t.Logf("parsed=%v tiles=%d action=%q instruction=%q fields=%d page=%q",
		parsed, len(challenge.Tiles), challenge.Action, challenge.Instruction, len(challenge.Fields),
		challenge.Page)
	switch {
	case !parsed && isDDGChallenge(string(body)):
		t.Fatalf("the live interstitial no longer parses; the body is in ../../scratch/ddg-interstitial.html")
	case !parsed:
		// The identity is trusted at this moment: a results page came back instead of a check, so
		// there is nothing to answer. Other tests cover reading a page that is not a check.
		t.Skip("the host served results, not a check — nothing to answer right now; " +
			"the body is in ../../scratch/ddg-interstitial.html")
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

	// The answer itself. Which squares hold a duck is not knowable here, so this measures what the
	// host does with a readable answer. Every square is ticked, so a verdict must be a refusal of
	// the ticks rather than of the request; the reply is kept whole next to a summary of it.
	answerForm := url.Values{}
	for name, value := range challenge.Fields {
		answerForm.Set(name, value)
	}
	for _, tile := range challenge.Tiles {
		answerForm.Set(tile.Name, "on")
	}
	from, _ := url.Parse(challenge.Page)
	answer, postErr := provider.client.postFormFromPage(context.Background(), challenge.Action,
		answerForm, "de", from)
	if postErr != nil {
		t.Fatalf("posting the answer: %v", postErr)
	}
	defer answer.Body.Close()
	reply, readErr := io.ReadAll(io.LimitReader(answer.Body, 4<<20))
	if readErr != nil {
		t.Fatalf("reading the answer's reply: %v", readErr)
	}
	if err := os.WriteFile("../../scratch/ddg-answer.html", reply, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	next, isCheck := parseDDGChallenge(string(reply), query, "de", challenge.Action)
	modal := modalText(string(reply))
	summary := strings.Join([]string{
		"request: POST " + challenge.Action,
		"  Origin:      " + from.String(),
		"  Referer:     " + from.String(),
		"  Sec-Fetch-Site: " + formFetchSite(parseURLOrFail(t, challenge.Action), from),
		"status: " + answer.Status,
		"location: " + answer.Header.Get("Location"),
		"cookies: " + strings.Join(answer.Header.Values("Set-Cookie"), " | "),
		"modal: " + modal,
	}, "\n") + "\n"
	if err := os.WriteFile("../../scratch/ddg-answer.txt", []byte(summary), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Logf("answer status=%s bytes=%d location=%q check-again=%v",
		answer.Status, len(reply), answer.Header.Get("Location"), isCheck)
	t.Logf("answer modal=%q", modal)
	switch {
	case answer.StatusCode == http.StatusBadRequest:
		t.Fatalf("the host could not read the answer at all: %s", answer.Status)
	case !isCheck:
		t.Logf("the reply is not a check: the answer was handled as a page request, not as an answer")
	case sameTiles(challenge, next):
		t.Logf("the same %d squares came back: the answer was not taken — a refusal, not a verdict",
			len(next.Tiles))
	default:
		t.Logf("a different set of %d squares came back: the host read and judged the answer",
			len(next.Tiles))
	}
}

// modalText is every word the check's modal says, in reading order. The capture needs it because
// the instruction alone does not say whether an answer was refused.
func modalText(body string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return ""
	}
	var parts []string
	doc.Find(".anomaly-modal__title, .anomaly-modal__description, .anomaly-modal__instructions").
		Each(func(_ int, selection *goquery.Selection) {
			if text := cleanText(selection.Text()); text != "" {
				parts = append(parts, text)
			}
		})
	return strings.Join(parts, " / ")
}

// sameTiles reports whether two checks are the same puzzle: the squares are named by the hash of
// the picture they carry, so the same names are the same squares.
func sameTiles(a, b Challenge) bool {
	if len(a.Tiles) != len(b.Tiles) || len(a.Tiles) == 0 {
		return false
	}
	left := make([]string, 0, len(a.Tiles))
	for _, tile := range a.Tiles {
		left = append(left, tile.Name)
	}
	right := make([]string, 0, len(b.Tiles))
	for _, tile := range b.Tiles {
		right = append(right, tile.Name)
	}
	sort.Strings(left)
	sort.Strings(right)
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// parseURLOrFail parses a URL the test already holds as text.
func parseURLOrFail(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed
}
