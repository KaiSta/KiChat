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
	// Detail is a sentence for the one reader that is not the window: the model,
	// which receives a failed tool call as the tool's result. A code is not
	// something a model can act on — "query is missing" tells it what to fix,
	// `i18n:{"code":"tools.queryMissing"}` does not. The window ignores this
	// field and translates the code like any other.
	Detail string `json:"detail,omitempty"`
}

// Errorf builds an error whose text is a code and the arguments to put in it.
//
// Arguments are stringified the way `fmt` would print them, because the frontend
// substitutes them into `{{1}}`, `{{2}}`… and a number or a path is text by the time
// it reaches the window. A code with no sentences behind it is still a code: the
// frontend falls back to this raw text, which names the condition rather than
// silently showing nothing.
func Errorf(code string, args ...any) error {
	return build(Message{Code: code}, args)
}

// Detailf is Errorf for a failure that is also handed to the model as text.
//
// Both halves are needed because the two readers want different things: the window
// wants a sentence in its own language, and the model wants the condition in words
// it can correct a tool call from. detail is that sentence; the dictionaries hold
// the others.
func Detailf(code, detail string, args ...any) error {
	return build(Message{Code: code, Detail: detail}, args)
}

// Wrapf is Errorf with a cause kept for errors.Is. The text is still the code, so the
// window translates it like any other message; the cause stays reachable for the
// caller that tests for it, which is the one thing fmt.Errorf could do and a code
// cannot. An empty detail leaves the message with none, as Errorf does.
func Wrapf(code, detail string, cause error, args ...any) error {
	return &wrappedError{message: build(Message{Code: code, Detail: detail}, args), cause: cause}
}

type wrappedError struct {
	message error
	cause   error
}

func (e *wrappedError) Error() string { return e.message.Error() }

func (e *wrappedError) Unwrap() error { return e.cause }

// Sentence returns the text a reader that is not the window should see: the detail
// when the message carries one, the error's own text otherwise. An error from a
// package that does not use this package reads exactly as it did before.
func Sentence(err error) string {
	if err == nil {
		return ""
	}
	if message, ok := parse(err.Error()); ok && message.Detail != "" {
		return message.Detail
	}
	return err.Error()
}

// build stringifies the arguments and renders the payload both the window and the
// tests read back.
func build(message Message, args []any) error {
	message.Args = make([]string, 0, len(args))
	for _, arg := range args {
		message.Args = append(message.Args, fmt.Sprint(arg))
	}
	payload, err := json.Marshal(message)
	if err != nil {
		// Only strings go in, so this is unreachable; naming the code is still better
		// than dropping the failure entirely.
		return fmt.Errorf("%s%s", Prefix, message.Code)
	}
	return fmt.Errorf("%s%s", Prefix, payload)
}

// Parse splits an error's text back into its code and arguments.
//
// The window is the reader this format exists for, but a test in another package that
// wants to assert *which* refusal a call produced needs the same split — and pinning
// English prose in Go is what this package replaced.
func Parse(text string) (Message, bool) { return parse(text) }

// parse does the work behind Parse and Sentence: one implementation, so a test and a
// fallback reader cannot disagree about what a payload is.
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
