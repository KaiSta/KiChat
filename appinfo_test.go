package main

import (
	"testing"

	"kichat/internal/version"
)

// The Settings screen reads the version through this binding, so it has to hand
// back exactly the values the linker set — a mismatch would show a version the
// running binary does not have. It also must not need initialisation: someone
// whose startup failed is precisely who asks which build they are on.
func TestGetAppInfoReportsTheLinkedVersion(t *testing.T) {
	got := (&App{}).GetAppInfo()
	if got.Version != version.Version || got.Commit != version.Commit || got.Date != version.Date {
		t.Fatalf("AppInfo %+v does not match linked version=%q commit=%q date=%q",
			got, version.Version, version.Commit, version.Date)
	}
	if got.Platform == "" || got.GoVersion == "" {
		t.Fatalf("AppInfo %+v must name the platform and the runtime", got)
	}
}
