package chat

import "fmt"

// Continuing a reply the output limit cut short.
//
// A backend stops a generation at the model's output ceiling and says so in its stop reason
// (`llm.Truncated`). What the app then has is half an answer: a sentence that ends mid-word, or a
// tool call whose arguments are not JSON. The turn used to end there — the row was stored as
// complete, because nothing had failed, and the user was left to ask again and pay for the text
// they already had.
//
// So the reply is continued instead. The text already produced stays in the history, and the
// round after it carries one note asking for the rest. Three attempts, because a model that hits
// the same ceiling three times in a row is not going to be saved by a fourth ask — it has a shape
// it keeps producing at that length — while one or two is what turns "it just stops" into a
// finished answer.
//
// This is not a bigger output budget. Raising the limit is the user's decision (`Sampler.MaxTokens`),
// and it is the better fix when the answer genuinely needs more room; the note says so when the
// attempts run out. What this does is the smaller thing that helps most: ask for the remaining
// part, briefly, instead of throwing the first part away.

// maxTruncatedRetries is how many times one reply may be asked to carry on. A continuation is a
// model round like any other, so it is also counted against the turn's round budget rather than
// on top of it.
const maxTruncatedRetries = 3

// continuationNote is the request-tail note that asks for the rest of a reply.
//
// In the tail and not in the system prompt for the reason the clock is there: it exists only for
// the rounds that follow a truncation, so in message zero it would invalidate the cache of the
// whole history twice — once when it arrived, once when it left. It is the one note sent as a
// message of its own, a user turn right after the cut-off reply, because a request whose last
// message is an assistant message is a shape some vendors refuse — and because that is where the
// model reads "the text above stops; carry on from there".
//
// It asks for the two things that help. "Continue where you stopped" is the whole point: asked
// again without that, a model restarts from the top and bills the same text twice. "Be briefer"
// is what makes the rest fit — the first attempt spent the entire output budget, so a second one
// in the same style reaches the same wall. And a cut-off tool call is named with its fix, because
// that is the failure the model can actually act on: a smaller window, a smaller change.
func continuationNote(attempt int) string {
	return fmt.Sprintf("<output_limit>Deine letzte Antwort wurde am Ausgabelimit des Modells "+
		"abgeschnitten (Versuch %d von %d). Setze genau an der Stelle fort, an der sie aufgehört hat, "+
		"und fasse dich dabei kürzer: keine Wiederholung des schon Gesagten, weniger Prosa. Wurde ein "+
		"Werkzeugaufruf abgeschnitten, sende ihn erneut mit kleineren Argumenten — kleinerer Bereich, "+
		"kleinere Änderung.</output_limit>", attempt, maxTruncatedRetries)
}
