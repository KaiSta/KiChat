package runtime

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// ggufBuilder writes the header of a GGUF the way llama.cpp's converter does: magic,
// version, two counts, then the key/value pairs. Only the parts this package reads are
// modelled; the tensor table is left out, which is also what makes these files small.
type ggufBuilder struct{ b []byte }

func (g *ggufBuilder) u32(v uint32)  { g.b = binary.LittleEndian.AppendUint32(g.b, v) }
func (g *ggufBuilder) i32(v int32)   { g.u32(uint32(v)) }
func (g *ggufBuilder) f32(v float32) { g.u32(math.Float32bits(v)) }
func (g *ggufBuilder) u64(v uint64)  { g.b = binary.LittleEndian.AppendUint64(g.b, v) }
func (g *ggufBuilder) text(s string) {
	g.u64(uint64(len(s)))
	g.b = append(g.b, s...)
}
func (g *ggufBuilder) key(k string, typ uint32) {
	g.text(k)
	g.u32(typ)
}

// ggufHeader assembles a file: a version, no tensors, and the given key/value pairs.
func ggufHeader(version uint32, kvs ...func(*ggufBuilder)) []byte {
	g := &ggufBuilder{b: []byte("GGUF")}
	g.u32(version)
	g.u64(0)
	g.u64(uint64(len(kvs)))
	for _, kv := range kvs {
		kv(g)
	}
	return g.b
}

func kvString(k, v string) func(*ggufBuilder) {
	return func(g *ggufBuilder) { g.key(k, 8); g.text(v) }
}
func kvU32(k string, v uint32) func(*ggufBuilder) {
	return func(g *ggufBuilder) { g.key(k, 4); g.u32(v) }
}
func kvI32(k string, v int32) func(*ggufBuilder) {
	return func(g *ggufBuilder) { g.key(k, 5); g.i32(v) }
}

// A mixture-of-experts header, with the tokenizer array after the keys it needs: the
// parse stops at the last key and must never walk that array.
func TestGGUFProbeReadsTheExpertLayout(t *testing.T) {
	data := ggufHeader(3,
		kvString("general.architecture", "qwen3moe"),
		kvString("general.name", "Qwen3.6-35B-A3B"),
		kvU32("qwen3moe.block_count", 48),
		kvU32("qwen3moe.expert_count", 256),
		kvU32("qwen3moe.expert_used_count", 8),
		func(g *ggufBuilder) {
			g.key("tokenizer.ggml.tokens", 9)
			g.u32(8)
			g.u64(3)
			g.text("a")
			g.text("b")
			g.text("c")
		},
	)
	info, err := parseGGUFInfo(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.Architecture != "qwen3moe" || info.BlockCount != 48 || info.ExpertCount != 256 || info.ExpertUsedCount != 8 {
		t.Errorf("info = %+v, want qwen3moe/48/256/8", info)
	}
	if !info.MoE() {
		t.Error("a header with 256 experts reports dense")
	}
}

// A dense model publishes no expert keys at all, which is the answer the panel uses to
// hide the row — 0 means "read, and there are none".
func TestGGUFProbeTellsADenseModelFromAMixture(t *testing.T) {
	info, err := parseGGUFInfo(ggufHeader(3,
		kvString("general.architecture", "llama"),
		kvU32("llama.block_count", 32),
		kf32("llama.rope.freq_base", 10000),
		kvString("tokenizer.ggml.model", "gpt2"),
	))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.Architecture != "llama" || info.BlockCount != 32 {
		t.Errorf("info = %+v, want llama/32", info)
	}
	if info.ExpertCount != 0 || info.MoE() {
		t.Errorf("a dense header reports %d experts", info.ExpertCount)
	}
}

func kf32(k string, v float32) func(*ggufBuilder) {
	return func(g *ggufBuilder) { g.key(k, 6); g.f32(v) }
}

// Some converters write the counts as int32 rather than uint32, and the architecture
// key is not guaranteed to come first. Both have to read the same.
func TestGGUFProbeDoesNotDependOnKeyOrderOrIntegerWidth(t *testing.T) {
	info, err := parseGGUFInfo(ggufHeader(3,
		kvI32("qwen3moe.block_count", 40),
		kvI32("qwen3moe.expert_count", 128),
		kvI32("qwen3moe.expert_used_count", 8),
		kvString("general.architecture", "qwen3moe"),
	))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.Architecture != "qwen3moe" || info.BlockCount != 40 || info.ExpertCount != 128 || info.ExpertUsedCount != 8 {
		t.Errorf("info = %+v, want qwen3moe/40/128/8", info)
	}
}

// An architecture this app has never heard of still answers the layer question, because
// the keys are namespaced by whatever the file says.
func TestGGUFProbeReadsAnUnknownArchitecture(t *testing.T) {
	info, err := parseGGUFInfo(ggufHeader(3,
		kvString("general.architecture", "futuremoe"),
		kvU32("futuremoe.block_count", 61),
		kvU32("futuremoe.expert_count", 64),
	))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.Architecture != "futuremoe" || info.BlockCount != 61 || info.ExpertCount != 64 {
		t.Errorf("info = %+v, want futuremoe/61/64", info)
	}
}

func TestGGUFProbeRefusesWhatItCannotRead(t *testing.T) {
	full := ggufHeader(3,
		kvString("general.architecture", "llama"),
		kvU32("llama.block_count", 32),
	)
	if _, err := parseGGUFInfo([]byte("NOPE")); err == nil {
		t.Error("a file that does not start with GGUF was read")
	}
	if _, err := parseGGUFInfo(full[:len(full)-3]); err == nil {
		t.Error("a truncated header was read as a complete one")
	}
	if _, err := parseGGUFInfo(ggufHeader(1, kvU32("llama.block_count", 32))); err == nil {
		t.Error("a version-1 header was read")
	}
	// A corrupt key count must be refused rather than walked.
	g := &ggufBuilder{b: []byte("GGUF")}
	g.u32(3)
	g.u64(0)
	g.u64(maxGGUFKeys + 1)
	if _, err := parseGGUFInfo(g.b); err == nil {
		t.Error("an absurd key count was accepted")
	}
}

// The file path is what the panel calls, and a replaced file must not keep answering
// with the previous probe: a re-quantised model has a different size.
func TestGGUFProbeReadsTheFileAndDropsAStaleEntry(t *testing.T) {
	forgetGgufProbes()
	path := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(path, ggufHeader(3,
		kvString("general.architecture", "qwen3moe"),
		kvU32("qwen3moe.block_count", 48),
		kvU32("qwen3moe.expert_count", 256),
	), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := ProbeGGUF(path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.ExpertCount != 256 {
		t.Errorf("ExpertCount = %d, want 256", info.ExpertCount)
	}

	// A different file at the same path, one byte longer, must be read again.
	replaced := append(ggufHeader(3,
		kvString("general.architecture", "llama"),
		kvU32("llama.block_count", 32),
	), 0)
	if err := os.WriteFile(path, replaced, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err = ProbeGGUF(path)
	if err != nil {
		t.Fatalf("probe after replace: %v", err)
	}
	if info.Architecture != "llama" || info.ExpertCount != 0 {
		t.Errorf("a replaced file still answered %+v", info)
	}
}

// The suggestion is a starting point, and the direction it errs in is the one that
// cannot fail: more experts in RAM than strictly needed.
func TestSuggestExpertLayers(t *testing.T) {
	const gb = int64(1) << 30
	cases := []struct {
		name       string
		blockCount int
		model      int64
		free       int64
		want       int
	}{
		{"fits, nothing to suggest", 48, 10 * gb, 20 * gb, 0},
		{"a 20.4 GB model on 12 GB of card", 48, 20400 << 20, 12 * gb, 20},
		{"just over: at least one layer", 10, 100 * gb, 99 * gb, 1},
		{"no card reported", 48, 20 * gb, 0, 0},
		{"no header", 0, 20 * gb, 8 * gb, 0},
		{"never more than the model has", 4, 100 * gb, 1 * gb, 4},
	}
	for _, tc := range cases {
		if got := SuggestExpertLayers(tc.blockCount, tc.model, tc.free); got != tc.want {
			t.Errorf("%s: SuggestExpertLayers(%d, %d, %d) = %d, want %d",
				tc.name, tc.blockCount, tc.model, tc.free, got, tc.want)
		}
	}
}

// A dense model is never told to move experts. The arithmetic above is about layers, and
// an oversized dense file satisfies it, so the header has to be the thing that says no:
// both the panel's prefill and the chat's picker otherwise turn the number into
// `--n-cpu-moe` on a model whose expert count is zero.
func TestDenseModelGetsNoExpertSuggestion(t *testing.T) {
	const gb = int64(1) << 30
	dense := GGUFInfo{Architecture: "llama", BlockCount: 32}
	if got := SuggestForModel(dense, 20*gb, 4*gb); got != 0 {
		t.Errorf("a dense model that does not fit got %d layers, want no suggestion", got)
	}
	// The same arithmetic on a mixture answers, so the zero above is the expert count
	// talking rather than a share that came out empty.
	moe := GGUFInfo{Architecture: "qwen3moe", BlockCount: 48, ExpertCount: 256, ExpertUsedCount: 8}
	if got := SuggestForModel(moe, 20*gb, 4*gb); got == 0 {
		t.Error("a mixture that does not fit got no suggestion at all")
	}
}

// Multi-token prediction is declared by the converter in one metadata value, and only a
// file that carries the heads can be started with the flag: llama.cpp rejects it on a
// model without them, so the suggestion and the start both ask this question first.
func TestGGUFProbeFindsTheMTPHeads(t *testing.T) {
	data := ggufHeader(3,
		kvString("general.architecture", "qwen35moe"),
		kvU32("qwen35moe.block_count", 48),
		kvU32("qwen35moe.expert_count", 256),
		kvU32("qwen35moe.expert_used_count", 8),
		kvU32("qwen35moe.nextn_predict_layers", 1),
		// The tokenizer array follows, and the read must never need it: the MTP count is
		// architecture metadata, and the parse stops at the section's first key. Before
		// that boundary existed the parse stopped once it had the expert trio, which is
		// why the heads have to be declared after it here.
		func(g *ggufBuilder) {
			g.key("tokenizer.ggml.tokens", 9)
			g.u32(3)
			g.u64(3)
			g.text("a")
			g.text("b")
			g.text("c")
		},
	)
	path := filepath.Join(t.TempDir(), "mtp.gguf")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	forgetGgufProbes()
	info, err := ProbeGGUF(path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.MTPLayers != 1 || !info.MTP() {
		t.Errorf("MTP layers = %d, want the one the header declares", info.MTPLayers)
	}
	if got := SuggestMTPDraftTokens(info); got != 3 {
		t.Errorf("suggested drafts = %d, want 3", got)
	}
	if got := SuggestMTPConfidence(info); got != 0.75 {
		t.Errorf("suggested floor = %v, want 0.75", got)
	}

	// A file without the key is not told to speculate: the row does not appear and the
	// start sends no flag.
	dense := ggufHeader(3, kvString("general.architecture", "llama"), kvU32("llama.block_count", 32))
	plain := filepath.Join(t.TempDir(), "plain.gguf")
	if err := os.WriteFile(plain, dense, 0o644); err != nil {
		t.Fatal(err)
	}
	forgetGgufProbes()
	info, err = ProbeGGUF(plain)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.MTP() || SuggestMTPDraftTokens(info) != 0 || SuggestMTPConfidence(info) != 0 {
		t.Errorf("a file without MTP heads reports %d heads, %d drafts and floor %v",
			info.MTPLayers, SuggestMTPDraftTokens(info), SuggestMTPConfidence(info))
	}
}
