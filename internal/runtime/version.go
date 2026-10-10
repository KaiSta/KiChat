package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// BinaryVersion asks a llama-server what it is and returns the line it answers
// with, for example `0.6.0-dev (build 11534, commit 50e3e3e48)`.
//
// The release tag the installer wrote down is not the same thing: it is a name
// the app chose (`b11534`), it says nothing about a launcher the user supplied
// instead, and it is the *published* build rather than the one on disk after a
// user replaced or rebuilt it. Only the binary can answer what the binary is,
// which is also what makes a bug report actionable.
//
// The context carries the deadline, because a launcher that hangs (a damaged
// download, a library that stalls at load) must not hold a status call open. A
// build that prints its version and exits non-zero still counts: what was asked
// for arrived.
func BinaryVersion(ctx context.Context, binary string) (string, error) {
	if strings.TrimSpace(binary) == "" {
		return "", errors.New("kein llama-server angegeben")
	}
	cmd := exec.CommandContext(ctx, binary, "--version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// The same environment a server is started with: a HIP build cannot load its
	// backend without the ROCm library path, and a probe that answered "no
	// version" for an installed build would be reading its own environment, not
	// the build's. See serverEnv.
	cmd.Env = serverEnv(binary)
	// Cancelling the context kills the launcher, but not a child it forked — and a
	// child inherits the pipe this collects through, so Wait would sit there until
	// that child exits. Measured with a launcher that sleeps: without this the
	// probe returned after 30 s, which is exactly the pipe-holding trap the server
	// shutdown has to work around as well.
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()

	if version := parseVersionOutput(out.String()); version != "" {
		return version, nil
	}
	if runErr != nil {
		return "", fmt.Errorf("Version von %s lesen: %w", binary, runErr)
	}
	return "", fmt.Errorf("%s meldet keine Version", binary)
}

// parseVersionOutput picks the version out of what a launcher printed.
//
// It searches rather than reading the first line, because llama.cpp writes other
// things first — a warning about its build, the device list, a note about the
// missing API key — and it strips a leading `version:` label so the UI can put
// its own next to it. The whole line is kept rather than just the number: the
// build and commit in it are exactly what makes two installations of "0.6.0-dev"
// distinguishable.
func parseVersionOutput(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		index := strings.Index(lower, "version")
		if index < 0 && !strings.Contains(lower, "build") {
			continue
		}
		if index > 0 {
			line = strings.TrimSpace(line[index:])
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "version"))
		line = strings.TrimLeft(line, ": ")
		if line == "" {
			continue
		}
		return line
	}
	return ""
}
