// The single-session default, end to end. The flag itself is pinned in the runtime
// package; what only this test can see is that a start nobody configured reaches exec
// with `--parallel 1` — a field resolved and then dropped between the settings blob and
// the command line is the mistake a test of Options cannot catch.
package main

import (
	"runtime"
	"testing"

	"kichat/internal/config"
)

func TestAManagedStartAsksForOneSlotByDefault(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the command line is read from /proc")
	}
	_, argv := startManagedForArgv(t, config.ManagedModel{
		ModelPath: writeModelFile(t, t.TempDir()),
	})

	if !hasFlagValue(argv, "--parallel", "1") {
		t.Errorf("the running command line is %v, want --parallel 1", argv)
	}
}
