package events

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// frontendEVPath is the frontend's own copy of these names. The two must agree
// in both directions: a name the backend emits but the frontend ignores is a
// dead event, and a name the frontend subscribes to but the backend never emits
// is a listener that can never fire.
const frontendEVPath = "../../frontend/src/lib/api.ts"

var evEntry = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9_]+):\s*'([^']+)',`)

// parseFrontendEV extracts the EV object from api.ts. Parsing the source rather
// than importing it keeps this test runnable without a Node toolchain, which is
// the whole reason it can be part of `go test ./...`.
func parseFrontendEV(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(frontendEVPath)
	if err != nil {
		t.Fatalf("read %s: %v", frontendEVPath, err)
	}
	source := string(raw)

	start := strings.Index(source, "export const EV = {")
	if start < 0 {
		t.Fatalf("%s: no `export const EV = {` block found; did the frontend move it?", frontendEVPath)
	}
	end := strings.Index(source[start:], "} as const")
	if end < 0 {
		t.Fatalf("%s: EV block is not closed with `} as const`", frontendEVPath)
	}
	block := source[start : start+end]

	out := map[string]string{}
	for _, m := range evEntry.FindAllStringSubmatch(block, -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatalf("%s: EV block parsed to zero entries", frontendEVPath)
	}
	return out
}

func TestFrontendEventMapMatchesConstants(t *testing.T) {
	frontend := parseFrontendEV(t)

	known := map[string]bool{}
	for _, name := range All() {
		known[name] = true
		if _, ok := frontend[name]; !ok {
			// Reverse lookup by value: the frontend key need not match the Go
			// identifier, but the wire name must be present.
			found := false
			for _, v := range frontend {
				if v == name {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("backend emits %q but the frontend EV map does not subscribe to it", name)
			}
		}
	}

	for key, value := range frontend {
		if !known[value] {
			t.Errorf("frontend EV.%s = %q is not a known event name; the backend never emits it", key, value)
		}
	}

	// Two Go constants sharing one wire name would make All() ambiguous.
	seen := map[string]string{}
	for _, name := range All() {
		seen[name] = name
	}
	if len(seen) != len(All()) {
		t.Errorf("All() contains duplicate event names: %v", All())
	}
}

// literalEmit matches an emit call whose first argument is a string literal,
// which is how the compaction event came to be misspelled. Every call site must
// pass a constant from this package instead.
var literalEmit = regexp.MustCompile(`\.(?:emit|Emit)\(\s*["` + "`" + `]`)

func TestNoRawEventLiteralsAtEmitSites(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// This package defines the names instead of emitting them.
		if strings.Contains(filepath.ToSlash(path), "/internal/events/") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if literalEmit.MatchString(line) {
				offenders = append(offenders, formatLocation(path, i+1, line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("emit called with a raw string literal; use an internal/events constant:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func formatLocation(path string, line int, text string) string {
	rel := filepath.ToSlash(path)
	if i := strings.Index(rel, "kiChat/"); i >= 0 {
		rel = rel[i+len("kiChat/"):]
	}
	return rel + ":" + itoa(line) + ": " + strings.TrimSpace(text)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
