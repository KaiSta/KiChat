package main

import (
	"encoding/json"
	"strings"
	"testing"

	"kichat/internal/uimsg"
)

// refusalOf returns the message code and arguments a bound method refused with.
//
// The sentence itself lives in the frontend dictionaries now (see internal/uimsg and
// translateError), so a test that wants to assert *which* refusal a call produced
// reads the code and the arguments it carries — the part the window actually shows —
// rather than pinning German or English prose in Go.
func refusalOf(t *testing.T, err error) (string, []string) {
	t.Helper()
	text := err.Error()
	payload, ok := strings.CutPrefix(text, uimsg.Prefix)
	if !ok {
		t.Fatalf("refusal %q carries no message code, so the window would show it untranslated", text)
	}
	var message struct {
		Code string   `json:"code"`
		Args []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(payload), &message); err != nil || message.Code == "" {
		t.Fatalf("refusal %q does not parse as a message: %v", text, err)
	}
	return message.Code, message.Args
}
