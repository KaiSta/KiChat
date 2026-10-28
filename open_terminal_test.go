package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kichat/internal/ingest"
	"kichat/internal/store"
	"kichat/internal/uimsg"

	rt "kichat/internal/runtime"
)

// terminalTestApp is an App with a real store and a launcher that records its folder
// instead of starting anything: a test that opened a terminal window would be a test that
// depends on which terminal the person running the suite happens to have installed.
func terminalTestApp(t *testing.T, launch func(string) error) (*App, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := &App{store: st, extractor: ingest.New()}
	// ensure() would otherwise initialise the real ~/.config/kichat, which a test must
	// never touch — the same seam every other bridge test uses.
	a.initOnce.Do(func() {})
	a.openTerminal = launch
	return a, st
}

func terminalTestChat(t *testing.T, st *store.Store, id, mode, dir string) {
	t.Helper()
	now := time.Now()
	if err := st.CreateChat(store.Chat{
		ID: id, Title: "Projekt", CreatedAt: now, UpdatedAt: now,
		Backend: "stub", Model: "stub-model", SystemPrompt: "p", Temperature: 0.7,
		Mode: mode, Workspace: dir,
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
}

// codeOf reads back the code a binding failed with, so the assertion is about which refusal
// happened rather than about the English sentence in the dictionary.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("the call reported success")
	}
	message, ok := uimsg.Parse(err.Error())
	if !ok {
		t.Fatalf("error %q carries no code", err)
	}
	return message.Code
}

// The button on the hand-off exists for one thing: after a refusal the person has a line to
// run, and this puts them where it can be run. So what it must get right is the folder — a
// terminal opened in the app's own working directory, or in the user's home, is a terminal
// the copied line would run in the wrong place.
func TestOpenProjectTerminalOpensTheProjectsFolder(t *testing.T) {
	root := t.TempDir()
	var got string
	a, st := terminalTestApp(t, func(dir string) error {
		got = dir
		return nil
	})
	terminalTestChat(t, st, "p1", store.ModeCoding, root)

	if err := a.OpenProjectTerminal("p1"); err != nil {
		t.Fatalf("OpenProjectTerminal: %v", err)
	}
	// The resolved path, not the stored string: the folder is opened as the filesystem
	// reports it, and on macOS a temp directory is handed out as /var/… and resolved
	// through /private/var/…, so comparing against the raw argument would be a test that
	// fails for a reason nobody cares about.
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve %s: %v", root, err)
	}
	if got != want {
		t.Errorf("the terminal was opened in %q, want %q", got, want)
	}
}

// A chat that is not a project has no folder, and opening a terminal "somewhere" would be a
// window the person never asked for. The refusal is the same sentence the file tools use.
func TestOpenProjectTerminalRefusesAChatWithoutAFolder(t *testing.T) {
	a, st := terminalTestApp(t, func(dir string) error {
		t.Errorf("a terminal was opened in %q", dir)
		return nil
	})
	terminalTestChat(t, st, "c1", store.ModeChat, "")

	if code := codeOf(t, a.OpenProjectTerminal("c1")); code != "tools.noWorkspace" {
		t.Errorf("code = %q, want tools.noWorkspace", code)
	}

	if code := codeOf(t, a.OpenProjectTerminal("does-not-exist")); code != "chat.gone" {
		t.Errorf("missing chat: code = %q, want chat.gone", code)
	}
}

// The folder is checked before a terminal is started in it. A project whose directory was
// deleted — a USB stick unplugged, a folder renamed — would otherwise open a terminal in
// whatever the operating system falls back to, which looks like it worked.
func TestOpenProjectTerminalRefusesAFolderThatIsGone(t *testing.T) {
	root := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	a, st := terminalTestApp(t, func(dir string) error {
		t.Errorf("a terminal was opened in %q", dir)
		return nil
	})
	terminalTestChat(t, st, "p1", store.ModeCoding, root)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}

	if code := codeOf(t, a.OpenProjectTerminal("p1")); code != "project.badFolder" {
		t.Errorf("code = %q, want project.badFolder", code)
	}
}

// "No terminal found" and "the terminal would not start" are two sentences because they ask
// for two different things: one names a program to install, the other invites a retry. The
// launcher's own error is the only thing that distinguishes them.
func TestOpenProjectTerminalSeparatesAMissingTerminalFromAFailedOne(t *testing.T) {
	root := t.TempDir()
	a, st := terminalTestApp(t, func(dir string) error { return rt.ErrNoTerminal })
	terminalTestChat(t, st, "p1", store.ModeCoding, root)

	if code := codeOf(t, a.OpenProjectTerminal("p1")); code != "project.noTerminal" {
		t.Errorf("code = %q, want project.noTerminal", code)
	}

	a.openTerminal = func(dir string) error { return errors.New("cannot open display") }
	err := a.OpenProjectTerminal("p1")
	if code := codeOf(t, err); code != "project.terminalFailed" {
		t.Errorf("code = %q, want project.terminalFailed", code)
	}
	// The failure keeps the launcher's own words, because that is what tells somebody with
	// no display why the window did not appear.
	if !strings.Contains(err.Error(), "cannot open display") {
		t.Errorf("the message dropped the launcher's reason: %v", err)
	}
}
