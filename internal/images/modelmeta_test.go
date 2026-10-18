package images

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The fixtures below write the real formats — a GGUF header and a safetensors JSON header —
// rather than a Go struct filled in by hand, because the point of this feature is that the
// reader agrees with what a quantiser and a repack actually emit. The measurements it is
// built on are the published FLUX 3 files named in IMAGES.md §9.5.

// writeGGUF writes a minimal but well-formed GGUF version 3 header: the given string keys,
// the given tensor names with one dimension each, and no tensor data. A file that ends after
// the header is readable for every purpose this package has.
func writeGGUF(t *testing.T, path string, kv map[string]string, tensors []string) {
	t.Helper()
	var b []byte
	b = append(b, "GGUF"...)
	b = binary.LittleEndian.AppendUint32(b, 3)
	b = binary.LittleEndian.AppendUint64(b, uint64(len(tensors)))
	b = binary.LittleEndian.AppendUint64(b, uint64(len(kv)))
	keys := make([]string, 0, len(kv))
	for key := range kv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		b = appendGGUFString(b, key)
		b = binary.LittleEndian.AppendUint32(b, ggufString)
		b = appendGGUFString(b, kv[key])
	}
	for i, name := range tensors {
		b = appendGGUFString(b, name)
		b = binary.LittleEndian.AppendUint32(b, 1)
		b = binary.LittleEndian.AppendUint64(b, 16)
		b = binary.LittleEndian.AppendUint32(b, 0) // F32
		b = binary.LittleEndian.AppendUint64(b, uint64(i)*64)
	}
	writeTestFile(t, path, b)
}

func appendGGUFString(b []byte, s string) []byte {
	b = binary.LittleEndian.AppendUint64(b, uint64(len(s)))
	return append(b, s...)
}

// writeGGUFWithScalars adds the two value kinds a real GGUF carries between its strings —
// a scalar and an array — so the reader's skipping is exercised, not just its string path.
func writeGGUFWithScalars(t *testing.T, path string, tensors []string) {
	t.Helper()
	var b []byte
	b = append(b, "GGUF"...)
	b = binary.LittleEndian.AppendUint32(b, 3)
	b = binary.LittleEndian.AppendUint64(b, uint64(len(tensors)))
	b = binary.LittleEndian.AppendUint64(b, 5)
	b = appendGGUFString(b, "general.architecture")
	b = binary.LittleEndian.AppendUint32(b, ggufString)
	b = appendGGUFString(b, "flux")
	b = appendGGUFString(b, "general.file_type")
	b = binary.LittleEndian.AppendUint32(b, 4) // uint32
	b = binary.LittleEndian.AppendUint32(b, 32)
	b = appendGGUFString(b, "general.quantization_version")
	b = binary.LittleEndian.AppendUint32(b, 2) // uint16
	b = binary.LittleEndian.AppendUint16(b, 2)
	// An array of strings, like a tokenizer's vocabulary.
	b = appendGGUFString(b, "tokenizer.ggml.tokens")
	b = binary.LittleEndian.AppendUint32(b, ggufArray)
	b = binary.LittleEndian.AppendUint32(b, ggufString)
	b = binary.LittleEndian.AppendUint64(b, 3)
	for _, token := range []string{"a", "b", "c"} {
		b = appendGGUFString(b, token)
	}
	// An array of scalars.
	b = appendGGUFString(b, "some.ints")
	b = binary.LittleEndian.AppendUint32(b, ggufArray)
	b = binary.LittleEndian.AppendUint32(b, 4) // uint32
	b = binary.LittleEndian.AppendUint64(b, 2)
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = binary.LittleEndian.AppendUint32(b, 2)
	for i, name := range tensors {
		b = appendGGUFString(b, name)
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = binary.LittleEndian.AppendUint64(b, uint64(i)*64)
	}
	writeTestFile(t, path, b)
}

// writeSafetensors writes the length-prefixed JSON header the format defines, with the
// given metadata and tensor names.
func writeSafetensors(t *testing.T, path string, meta map[string]string, tensors []string) {
	t.Helper()
	header := map[string]any{}
	if meta != nil {
		header["__metadata__"] = meta
	}
	for _, name := range tensors {
		header[name] = map[string]any{"dtype": "F32", "shape": []int{1}, "data_offsets": []int{0, 4}}
	}
	encoded, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("encode safetensors header: %v", err)
	}
	b := binary.LittleEndian.AppendUint64(nil, uint64(len(encoded)))
	b = append(b, encoded...)
	writeTestFile(t, path, b)
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	forgetModelHeaders()
}

// The tensor names the published files carry, measured with the readers in this file against
// the files named in IMAGES.md §9.5: the distilled checkpoint has two more, ref_time_in.
var (
	flux3KleinTensors = []string{
		"content_mode_blocks.image.0.attn_out.weight",
		"content_mode_blocks.image.1.attn_out.weight",
		"txt_mode_blocks.txt.0.k_proj.weight",
		"single_blocks.0.mlp_in.weight",
		"ref_time_in.in_layer.weight",
		"ref_time_in.out_layer.weight",
		"txt_in.weight",
	}
	flux3BaseTensors = []string{
		"content_mode_blocks.image.0.attn_out.weight",
		"content_mode_blocks.image.1.attn_out.weight",
		"txt_mode_blocks.txt.0.k_proj.weight",
		"single_blocks.0.mlp_in.weight",
		"txt_in.weight",
	}
	flux1Tensors = []string{
		"double_blocks.0.img_attn.qkv.weight",
		"single_blocks.0.mlp_in.weight",
		"txt_in.weight",
		"time_in.in_layer.weight",
	}
)

// TestAGGUFFHeaderIsRead pins the reader against the shape a quantiser writes: strings,
// scalars and arrays in the key/value table, then the tensor table.
func TestAGGUFFHeaderIsRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weights.gguf")
	writeGGUFWithScalars(t, path, flux3KleinTensors)

	header, err := inspectModel(path)
	if err != nil {
		t.Fatalf("inspectModel: %v", err)
	}
	if header.Format != "gguf" {
		t.Errorf("format = %q, want gguf", header.Format)
	}
	if got := header.Meta["general.architecture"]; got != "flux" {
		t.Errorf("general.architecture = %q, want flux", got)
	}
	if len(header.Tensors) != len(flux3KleinTensors) {
		t.Errorf("read %d tensors, want %d: %v", len(header.Tensors), len(flux3KleinTensors), header.Tensors)
	}
	if header.Tensors[0] != flux3KleinTensors[0] {
		t.Errorf("first tensor = %q, want %q", header.Tensors[0], flux3KleinTensors[0])
	}
}

// TestASafetensorsHeaderIsRead is the other half of "its own metadata": a safetensors file
// states the header length and keeps the metadata in a reserved key.
func TestASafetensorsHeaderIsRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.safetensors")
	writeSafetensors(t, path, map[string]string{"model": "f3-image-small", "dtype": "bfloat16"}, flux3KleinTensors)

	header, err := inspectModel(path)
	if err != nil {
		t.Fatalf("inspectModel: %v", err)
	}
	if header.Format != "safetensors" {
		t.Errorf("format = %q, want safetensors", header.Format)
	}
	if got := header.Meta["model"]; got != "f3-image-small" {
		t.Errorf("metadata model = %q, want f3-image-small", got)
	}
	if len(header.Tensors) != len(flux3KleinTensors) {
		t.Errorf("read %d tensors, want %d", len(header.Tensors), len(flux3KleinTensors))
	}
	for _, name := range header.Tensors {
		if name == "__metadata__" {
			t.Error("the metadata block was reported as a tensor")
		}
	}
}

// TestACorruptOrForeignFileIsAnErrorNotAGuess: a file this package cannot read has to say so,
// so the caller can decide to trust the name instead. Reporting an empty header with no
// error would turn "unreadable" into "no tensors", which is a different claim.
func TestACorruptOrForeignFileIsAnErrorNotAGuess(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]byte{
		"a text file":        []byte("this is not a model\n"),
		"a truncated gguf":   []byte("GGUF\x03\x00\x00\x00\x01\x00"),
		"a huge json header": append(binary.LittleEndian.AppendUint64(nil, 1<<62), '{', '}'),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "file.bin")
			writeTestFile(t, path, data)
			if header, err := inspectModel(path); err == nil {
				t.Fatalf("read %q as a model header: %+v", data, header)
			}
		})
	}
}

// TestTheCacheFollowsTheFile: a file replaced by a different one at the same path must be
// read again. Size and modification time are what the cache trusts, which is what a
// re-quantisation changes.
func TestTheCacheFollowsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weights.gguf")
	writeGGUF(t, path, nil, flux3KleinTensors)
	if header, err := inspectModel(path); err != nil || len(header.Tensors) != len(flux3KleinTensors) {
		t.Fatalf("first read: %d tensors, err %v", len(header.Tensors), err)
	}

	writeGGUF(t, path, nil, flux1Tensors)
	header, err := inspectModel(path)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(header.Tensors) != len(flux1Tensors) || header.Tensors[0] != flux1Tensors[0] {
		t.Errorf("the cache served the replaced file: %v", header.Tensors)
	}
}

// TestAMissingFileIsNotAnErrorToReport: nothing configured is the state every fresh install
// is in, and it must not read as a failed file.
func TestAMissingFileIsNotAnErrorToReport(t *testing.T) {
	if _, err := inspectModel(""); err == nil {
		t.Error("an empty path was read as a model")
	}
	if _, err := inspectModel(filepath.Join(t.TempDir(), "absent.gguf")); err == nil {
		t.Error("a missing file was read as a model")
	}
}
