// Package uimsg carries a user-facing message from Go as a key the frontend's
// dictionaries already hold.
//
// The bridge used to answer with German sentences, so an English or Chinese window
// showed German toasts. A bound method can only return an `error`, which Wails
// marshals as its text — there is no field to put a code in — so the code travels
// inside the text behind a prefix, and the frontend translates it and substitutes
// the arguments. Text without the prefix is shown as it is, which is what keeps a
// message from a package that has not been converted (a server's own wording, an
// HTTP status) readable rather than replaced by a code.
package uimsg

import (
	"encoding/json"
	"fmt"
)

// Prefix marks a message meant for translation. `i18n:` rather than something seen
// in ordinary text, so a sentence that happens to start with a key cannot be
// mistaken for one.
const Prefix = "i18n:"

// Message is the payload the frontend parses back out of the error text.
type Message struct {
	Code string   `json:"code"`
	Args []string `json:"args,omitempty"`
}

// Errorf builds an error whose text is a code and the arguments to put in it.
//
// Arguments are stringified the way `fmt` would print them, because the frontend
// substitutes them into `{{1}}`, `{{2}}`… and a number or a path is text by the time
// it reaches the window. A code with no sentences behind it is still a code: the
// frontend falls back to this raw text, which names the condition rather than
// silently showing nothing.
func Errorf(code string, args ...any) error {
	message := Message{Code: code, Args: make([]string, 0, len(args))}
	for _, arg := range args {
		message.Args = append(message.Args, fmt.Sprint(arg))
	}
	payload, err := json.Marshal(message)
	if err != nil {
		// Only strings go in, so this is unreachable; naming the code is still better
		// than dropping the failure entirely.
		return fmt.Errorf("%s%s", Prefix, code)
	}
	return fmt.Errorf("%s%s", Prefix, payload)
}

// parse splits a message back into its code and arguments. Unexported because only
// the test needs it: the frontend does the parsing that matters, and an exported
// Go-side parser would be a second implementation to keep in step.
func parse(text string) (Message, bool) {
	var message Message
	if len(text) <= len(Prefix) || text[:len(Prefix)] != Prefix {
		return message, false
	}
	if err := json.Unmarshal([]byte(text[len(Prefix):]), &message); err != nil {
		return Message{}, false
	}
	if message.Code == "" {
		return Message{}, false
	}
	return message, true
}
