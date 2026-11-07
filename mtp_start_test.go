package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"kichat/internal/config"
)

// The MTP flags are only legal on a file that carries the heads, and a stored count
// outlives the file it was chosen for: a re-download, a re-quantisation, or a path the
// user pointed at a different model. The start therefore asks the header, and the pair of
// tests here pins both answers — the flag on a file that declares the heads, and silence
// on a header that cannot vouch for them.
func TestAStoredMTPCountReachesAFileThatDeclaresTheHeads(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the command line is read from /proc")
	}
	models := t.TempDir()
	model := writeMTPModel(t, models)
	_, argv := startManagedForArgv(t, config.ManagedModel{
		ModelPath:     model,
		MTP:           map[string]int{model: 3},
		MTPConfidence: map[string]float64{model: 0.75},
	})

	if !hasFlagValue(argv, "--spec-type", "draft-mtp") {
		t.Errorf("the running server's command line is %v, want --spec-type draft-mtp", argv)
	}
	if !hasFlagValue(argv, "--spec-draft-n-max", "3") {
		t.Errorf("the command line is %v, want --spec-draft-n-max 3", argv)
	}
	if !hasFlagValue(argv, "--spec-draft-p-min", "0.75") {
		t.Errorf("the command line is %v, want --spec-draft-p-min 0.75", argv)
	}
}

// Undecided is not off: a file the user has never tuned starts with the recommendation,
// which is what makes the chat's picker agree with the panel's suggestion.
func TestAFileWithHeadsAndNoStoredChoiceStartsWithTheRecommendation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the command line is read from /proc")
	}
	model := writeMTPModel(t, t.TempDir())
	_, argv := startManagedForArgv(t, config.ManagedModel{ModelPath: model})

	if !hasFlagValue(argv, "--spec-type", "draft-mtp") {
		t.Errorf("an undecided file's command line is %v, want the recommendation on", argv)
	}
	if !hasFlagValue(argv, "--spec-draft-n-max", "3") || !hasFlagValue(argv, "--spec-draft-p-min", "0.75") {
		t.Errorf("the command line is %v, want 3 drafts with a 0.75 floor", argv)
	}
}

// And the one that has to stay possible: a stored 0 is the user saying off, so unticking the
// box in the panel is respected instead of being read as "nobody has decided".
func TestAStoredOffKeepsTheMTPFlagsOff(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the command line is read from /proc")
	}
	model := writeMTPModel(t, t.TempDir())
	_, argv := startManagedForArgv(t, config.ManagedModel{
		ModelPath: model,
		MTP:       map[string]int{model: 0},
	})

	if hasFlagValue(argv, "--spec-type", "draft-mtp") {
		t.Errorf("a stored off was overridden: %v", argv)
	}
	if hasFlagValue(argv, "--spec-draft-n-max", "3") || hasFlagValue(argv, "--spec-draft-p-min", "0.75") {
		t.Errorf("the recommendation reached a file the user turned off: %v", argv)
	}
}

// The other half: a count left on a file whose header has no MTP heads must not become a
// flag. llama.cpp does not ignore `--spec-type draft-mtp` there — it refuses the start —
// so the stored setting has to lose to the header rather than the other way round.
func TestAStoredMTPCountNeverReachesAFileWithoutHeads(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the command line is read from /proc")
	}
	models := t.TempDir()
	// `GGUF` and nothing else: a header that does not parse, which is the state of a
	// half-finished download the user pointed the entry at.
	model := writeModelFile(t, models)
	_, argv := startManagedForArgv(t, config.ManagedModel{
		ModelPath:     model,
		MTP:           map[string]int{model: 3},
		MTPConfidence: map[string]float64{model: 0.75},
	})

	if hasFlagValue(argv, "--spec-type", "draft-mtp") {
		t.Errorf("a stored count reached a file whose header has no MTP heads: %v", argv)
	}
	if hasFlagValue(argv, "--spec-draft-n-max", "3") || hasFlagValue(argv, "--spec-draft-p-min", "0.75") {
		t.Errorf("the MTP count or floor reached an unreadable file: %v", argv)
	}
}

// writeMTPModel writes the smallest header the probe accepts that still declares one MTP
// head, in the shape llama.cpp's converter writes: the architecture string, then the
// namespaced scalars.
func writeMTPModel(t *testing.T, dir string) string {
	t.Helper()
	var b []byte
	u32 := func(v uint32) { b = binary.LittleEndian.AppendUint32(b, v) }
	u64 := func(v uint64) { b = binary.LittleEndian.AppendUint64(b, v) }
	str := func(s string) { u64(uint64(len(s))); b = append(b, s...) }
	kv := func(k string, v uint32) { str(k); u32(4); u32(v) }

	b = append(b, "GGUF"...)
	u32(3) // version
	u64(0) // no tensors: the header alone answers the question
	u64(4) // metadata keys
	str("general.architecture")
	u32(8) // string
	str("qwen35moe")
	kv("qwen35moe.block_count", 48)
	kv("qwen35moe.expert_count", 256)
	kv("qwen35moe.nextn_predict_layers", 1)

	path := filepath.Join(dir, "mtp-weights.gguf")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
