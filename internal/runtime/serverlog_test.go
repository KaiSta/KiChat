package runtime

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The whole output has to survive the row, and the file is the only place a
// twenty-frame backtrace fits. Reopening continues the same record, because "it
// happened twice" is read out of one file.
func TestTheServerLogAppendsToAFileOfItsOwn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "llama-test.log")
	l, err := NewServerLog(path)
	if err != nil {
		t.Fatalf("NewServerLog: %v", err)
	}
	if _, err := l.Write([]byte("one\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte("two\n")); err != nil {
		t.Fatal(err)
	}
	l.Close()

	again, err := NewServerLog(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := again.Write([]byte("three\n")); err != nil {
		t.Fatal(err)
	}
	again.Close()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "one\ntwo\nthree\n" {
		t.Errorf("file = %q, want every line across both opens", body)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("mode = %o, want 600: the output names the model and the arguments", perm)
		}
	}
}

// It rolls like the other logs and keeps one previous file: the newest output is
// always in the current file — that is where a crash's reason is — while the
// generation before it is what a second report is compared against.
func TestTheServerLogRollsAtTheLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sd-images.log")
	l, err := NewServerLog(path)
	if err != nil {
		t.Fatalf("NewServerLog: %v", err)
	}
	chunk := []byte(strings.Repeat("x", 64<<10) + "\n")
	for i := 0; i < 70; i++ { // 4.4 MiB, past the 4 MiB limit
		if _, err := l.Write(chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	l.Close()

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("no previous generation: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= MaxServerLogSize {
		t.Errorf("current file is %d bytes, want it rolled before reaching %d", info.Size(), MaxServerLogSize)
	}
	if info.Size() == 0 {
		t.Error("the current file is empty, so the newest output was lost")
	}
}

// A nil log is the ordinary case: the ring hands every line to one whether or not a
// file was attached, so the sink has to accept them.
func TestANilServerLogAcceptsEverything(t *testing.T) {
	var l *ServerLog
	n, err := l.Write([]byte("x"))
	if err != nil || n != 1 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if l.Path() != "" {
		t.Errorf("Path = %q, want empty", l.Path())
	}
	l.Close()
}
