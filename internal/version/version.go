// Package version carries the build's version identity.
//
// The values are set at link time by the Makefile's -ldflags -X entries, which
// is why the linker path (kichat/internal/version) must stay stable: a rename
// silently stops the flags from matching and leaves every build reporting "dev".
// They live in their own package rather than in main so any package can read
// them without importing main.
//
// A plain `go build` leaves the defaults below. They are non-empty on purpose:
// an unversioned local build should still answer the version question honestly
// instead of rendering blank.
package version

var (
	// Version is the release identifier, e.g. "0.1.0".
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "none"
	// Date is the commit's own date, not the build wall clock. Deriving it from
	// the commit is what keeps two builds of the same commit byte-identical,
	// which -trimpath otherwise guarantees.
	Date = "unknown"
)

// String renders the version for the CLI and for logs, e.g. "0.1.0 (cf920ba)".
// It degrades to the bare version when the linker set no commit.
func String() string {
	if Commit == "" || Commit == "none" {
		return Version
	}
	return Version + " (" + Commit + ")"
}
