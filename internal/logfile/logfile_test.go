package logfile

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenAppendsToTheSameFile(t *testing.T) {
	dir := t.TempDir()

	first, closeFirst := Open(dir)
	first.Print("first launch")
	closeFirst()

	second, closeSecond := Open(dir)
	second.Print("second launch")
	closeSecond()

	raw, err := os.ReadFile(filepath.Join(dir, Name))
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	text := string(raw)
	for _, want := range []string{"first launch", "second launch"} {
		if !strings.Contains(text, want) {
			t.Errorf("the log is missing %q:\n%s", want, text)
		}
	}
}

// The roll is what keeps the file bounded. It has to keep the previous content
// rather than truncate it: the run that filled the log is usually the run before
// the failure someone is reporting.
func TestOpenRollsAFullFileAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, Name)
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), MaxSize), 0o644); err != nil {
		t.Fatalf("seed a full log: %v", err)
	}

	logger, closeLog := Open(dir)
	logger.Print("after the roll")
	closeLog()

	previous, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("the full log was not kept as %s.1: %v", Name, err)
	}
	if len(previous) != MaxSize {
		t.Errorf("the rolled file is %d bytes, want the %d it had", len(previous), MaxSize)
	}

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the new log: %v", err)
	}
	if !strings.Contains(string(current), "after the roll") {
		t.Errorf("the new log does not carry the launch that followed the roll:\n%s", current)
	}
	if len(current) >= MaxSize {
		t.Errorf("the new log is %d bytes, so the roll did not start it over", len(current))
	}
}

// A read-only or otherwise unusable config directory must not take the message
// with it: the whole point is to report something that has already gone wrong.
func TestOpenFallsBackWhenTheDirectoryCannotBeUsed(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("create the blocking file: %v", err)
	}

	logger, closeLog := Open(blocker)
	if logger == nil {
		t.Fatal("Open returned no logger for an unusable directory")
	}
	logger.Print("still recorded")
	closeLog()
}
