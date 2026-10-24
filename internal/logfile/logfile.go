// Package logfile records the launch itself.
//
// A desktop executable has no terminal: started from a menu, a .desktop entry or
// Explorer, its stderr goes nowhere. That left the failure a user is most likely
// to meet — the window simply never appearing — with no trace anywhere, so a bug
// report could not say what the program had done before it stopped. This writes
// the launch, and why it ended, into the data directory beside the database.
package logfile

import (
	"log"
	"os"
	"path/filepath"
)

const (
	// Name is the file itself, inside the data directory.
	Name = "kichat.log"

	// MaxSize is where the log rolls over. Small on purpose: this records launches
	// and startup failures, not conversations, so a megabyte is months of ordinary
	// use and a larger file would only be kept to sit unread. One previous file is
	// enough for "it failed twice the same way", which is the case a report brings.
	MaxSize = 1 << 20
)

// Open returns a logger appending to dir/Name, rolling the file over once it has
// passed MaxSize, and a function that closes the file.
//
// A directory that cannot be created or opened is not a reason to lose the
// message: the logger writes to stderr instead, so a build launched from a shell
// still shows the failure on a machine whose config directory is read-only. The
// caller always gets a usable logger, because every caller is on its way to
// reporting something that has already gone wrong.
func Open(dir string) (*log.Logger, func()) {
	stderr := log.New(os.Stderr, "", log.LstdFlags)
	// Owner-only, matching the database that will live beside this file: on a fresh
	// install this is the first thing to create the data directory, so it must not be
	// the one place that leaves it world-readable.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return stderr, func() {}
	}

	path := filepath.Join(dir, Name)
	if info, err := os.Stat(path); err == nil && info.Size() >= MaxSize {
		// A rename that fails leaves the file in place, and the append below then
		// keeps growing it. That is a worse log than a rolled one, and a better
		// outcome than refusing to record anything.
		_ = os.Rename(path, path+".1")
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return stderr, func() {}
	}
	return log.New(file, "", log.LstdFlags), func() { _ = file.Close() }
}
