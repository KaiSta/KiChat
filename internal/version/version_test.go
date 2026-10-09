package version

import (
	"strings"
	"testing"
)

// A plain `go test` build carries no linker flags, so these are the values an
// unversioned build shows. Empty strings would render the Settings row as a
// blank line and the CLI as an empty answer, so the defaults are load-bearing.
func TestDefaultsAreNonEmpty(t *testing.T) {
	if Version == "" || Commit == "" || Date == "" {
		t.Fatalf("defaults must not be empty: version=%q commit=%q date=%q", Version, Commit, Date)
	}
}

// The pair is what makes a bug report actionable: a version alone does not say
// which commit it was built from. The default build has no commit, so one is
// supplied here the way the linker would.
func TestStringPairsVersionWithCommit(t *testing.T) {
	original := Commit
	Commit = "cf920ba"
	defer func() { Commit = original }()

	if got, want := String(), Version+" (cf920ba)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// An unversioned build has no commit to name, and a dangling "()" would look
// like a lost value rather than the honest absence of one.
func TestStringOmitsMissingCommit(t *testing.T) {
	original := Commit
	Commit = "none"
	defer func() { Commit = original }()

	if got := String(); strings.Contains(got, "(") {
		t.Fatalf("String() = %q, must not render a missing commit", got)
	}
}
