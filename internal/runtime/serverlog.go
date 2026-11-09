package runtime

import (
	"os"
	"path/filepath"
	"sync"
)

// MaxServerLogSize is where the file rolls over. llama-server at the trace level is
// chatty — a model load is hundreds of lines — and a crash adds a twenty-frame gdb
// backtrace to that, so this is a few loads' worth; one previous file is kept, which
// is enough for "it happened twice the same way".
const MaxServerLogSize int64 = 4 << 20

// ServerLog is the file a managed server's whole output is kept in.
//
// The supervisor's ring is a window of forty lines, and the panel shows twelve of
// them, which is the right size for "what is it doing" and the wrong size for a
// crash: on SIGABRT llama.cpp forks gdb, so the line that explains the abort is
// followed by twenty frames and gdb's notes about the sources it could not find.
// This file is the other half of that pair — every line, unrolled, on disk, so a
// report can be read after the row is gone.
//
// It is 0600 in the owner-only data directory, like the trace beside it, because a
// server's output names the model's path and the arguments it was started with.
type ServerLog struct {
	mu   sync.Mutex
	file *os.File
	path string
	size int64
}

// NewServerLog opens path for appending, creating the directory and the file. A file
// already at the limit is rolled out of the way first, the same policy kichat.log
// uses at launch. An error is returned rather than kept: a start is diagnostics-free
// without it, and the caller is the one that decides whether that is worth a line.
func NewServerLog(path string) (*ServerLog, error) {
	l := &ServerLog{path: path}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if info, err := os.Stat(path); err == nil && info.Size() >= MaxServerLogSize {
		// Windows refuses a rename onto an existing name, so the old copy goes first.
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l.file = file
	if info, err := file.Stat(); err == nil {
		l.size = info.Size()
	}
	return l, nil
}

// Path is the file being written, empty for a nil log.
func (l *ServerLog) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Write appends, rolling the file once it reaches the limit. A nil log accepts
// everything, because the ring hands it every line and a server without a file
// attached is the ordinary case in the tests.
func (l *ServerLog) Write(p []byte) (int, error) {
	if l == nil {
		return len(p), nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return len(p), nil
	}
	if l.size+int64(len(p)) > MaxServerLogSize {
		if err := l.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := l.file.Write(p)
	l.size += int64(n)
	return n, err
}

// rotateLocked moves the current file aside and starts a new one. A rename or reopen
// that fails leaves the log closed, which the ring reports once instead of losing
// the failure silently.
func (l *ServerLog) rotateLocked() error {
	_ = l.file.Close()
	l.file = nil
	_ = os.Remove(l.path + ".1")
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return err
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	l.file = file
	l.size = 0
	return nil
}

// Close closes the file. Writing afterwards is a no-op.
func (l *ServerLog) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}
