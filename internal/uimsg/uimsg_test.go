package uimsg

import "testing"

func TestErrorfCarriesTheCodeAndItsArguments(t *testing.T) {
	err := Errorf("attachment.noStoredImage", "photo.png", 3)
	message, ok := parse(err.Error())
	if !ok {
		t.Fatalf("Errorf produced text the frontend cannot parse: %q", err.Error())
	}
	if message.Code != "attachment.noStoredImage" {
		t.Errorf("code is %q, want attachment.noStoredImage", message.Code)
	}
	want := []string{"photo.png", "3"}
	if len(message.Args) != len(want) {
		t.Fatalf("args are %v, want %v", message.Args, want)
	}
	for i := range want {
		if message.Args[i] != want[i] {
			t.Errorf("arg %d is %q, want %q", i, message.Args[i], want[i])
		}
	}
}

func TestErrorfWithoutArgumentsStaysParseable(t *testing.T) {
	message, ok := parse(Errorf("image.serverNotRunning").Error())
	if !ok || message.Code != "image.serverNotRunning" || len(message.Args) != 0 {
		t.Fatalf("got %+v (ok=%v), want the code and no arguments", message, ok)
	}
}

// Everything the frontend shows that did not come from a code — an OS error, a
// server's own wording — must stay text rather than be mistaken for a message.
func TestPlainTextIsNotAMessage(t *testing.T) {
	for _, text := range []string{
		"permission denied",
		"",
		"i18n:",
		"i18n:not json",
		`i18n:{"args":["x"]}`,
		`i18n:{"code":""}`,
	} {
		if message, ok := parse(text); ok {
			t.Errorf("%q parsed as %+v, want it left as text", text, message)
		}
	}
}
