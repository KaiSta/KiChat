package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// What a GGUF says about itself before a byte of it is loaded.
//
// The model panel needs three things a file name cannot give: whether the model is a
// mixture of experts at all, how many layers it has, and how many experts fire per
// token. All three are integers in the key/value table at the top of the file, so
// reading them costs one bounded read that never touches the weights.
//
// This is a second GGUF reader beside internal/images/modelmeta.go, and the split is
// deliberate. That one keeps the tensor names and the *string* metadata to identify a
// diffusion checkpoint, and skips every scalar on its way past; this one keeps the
// scalars, stops as soon as it has the four keys it wants, and never reads the tensor
// table. Sharing one would mean exporting the picture path's internals into a package
// that has nothing to do with pictures.
const (
	// ggufFirstRead is the first attempt at the header. It is an order of magnitude
	// above what these four keys need — they sit in the first few hundred bytes of
	// every published model — and the parse asks for more only when a file's
	// key/value table is longer than this.
	ggufFirstRead = 1 << 20
	// ggufHeaderLimit bounds the second attempt. A GGUF states no header length, so
	// this is a guard against a corrupt length field rather than a working figure: no
	// chat model's metadata table is anywhere near 16 MiB.
	ggufHeaderLimit = 16 << 20
	// maxGGUFKeys caps the key/value table. A chat model publishes tens of keys;
	// anything near this is not one.
	maxGGUFKeys = 4096
	// cachedProbes is how many files stay in memory, dropped whole when it fills.
	cachedProbes = 8
)

// GGUFInfo is the part of a model's header this package reads.
//
// BlockCount is the layer count and the ceiling for `--n-cpu-moe`; ExpertCount is how
// many experts one layer holds and is 0 for a dense model, which is the question the
// panel asks. ExpertUsedCount is how many of them fire per token — the number that
// makes a 20 GB file answer at 3B parameters. Zeros mean "the file did not say",
// because a header this app cannot read is not evidence of a dense model.
type GGUFInfo struct {
	Architecture    string
	BlockCount      int
	ExpertCount     int
	ExpertUsedCount int
}

// MoE reports whether the header says the model has experts. It is deliberately not
// "ExpertCount > 0 or unknown": the caller decides what an unreadable header means.
func (i GGUFInfo) MoE() bool { return i.ExpertCount > 0 }

var errGGUFTruncated = errors.New("GGUF header does not fit in the read window")

// ProbeGGUF reads one GGUF's architecture and expert layout, caching the result.
//
// The cache is keyed by path and invalidated by size and modification time, which is
// what a re-quantisation or a replaced download changes. It exists because the panel
// probes on every model-path change and the backend probes again when it builds the
// status, and a header read per call would be a surprising amount of disk for a value
// that cannot have changed between them.
func ProbeGGUF(path string) (GGUFInfo, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return GGUFInfo{}, os.ErrNotExist
	}
	info, err := os.Stat(path)
	if err != nil {
		return GGUFInfo{}, err
	}
	if info.IsDir() {
		return GGUFInfo{}, fmt.Errorf("%s is a folder, not a GGUF", path)
	}

	probes.mu.Lock()
	if entry, ok := probes.byPath[path]; ok && entry.size == info.Size() && entry.mod.Equal(info.ModTime()) {
		probes.mu.Unlock()
		return entry.info, entry.err
	}
	probes.mu.Unlock()

	read, err := readGGUFInfo(path, info.Size())

	probes.mu.Lock()
	if len(probes.byPath) >= cachedProbes {
		probes.byPath = make(map[string]cachedProbe)
	}
	probes.byPath[path] = cachedProbe{size: info.Size(), mod: info.ModTime(), info: read, err: err}
	probes.mu.Unlock()
	return read, err
}

type cachedProbe struct {
	size int64
	mod  time.Time
	info GGUFInfo
	err  error
}

var probes = struct {
	mu     sync.Mutex
	byPath map[string]cachedProbe
}{byPath: make(map[string]cachedProbe)}

// forgetGgufProbes drops the cache. Only a test needs it.
func forgetGgufProbes() {
	probes.mu.Lock()
	probes.byPath = make(map[string]cachedProbe)
	probes.mu.Unlock()
}

// readGGUFInfo reads as little of the file as will answer the question, and one more,
// bounded, window when the first one ended inside the header.
func readGGUFInfo(path string, size int64) (GGUFInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return GGUFInfo{}, err
	}
	defer file.Close()

	window, err := readGGUFWindow(file, size, ggufFirstRead)
	if err != nil {
		return GGUFInfo{}, err
	}
	info, err := parseGGUFInfo(window)
	if errors.Is(err, errGGUFTruncated) && int64(len(window)) < size && ggufHeaderLimit > ggufFirstRead {
		window, err = readGGUFWindow(file, size, ggufHeaderLimit)
		if err != nil {
			return GGUFInfo{}, err
		}
		info, err = parseGGUFInfo(window)
	}
	return info, err
}

func readGGUFWindow(file *os.File, size, n int64) ([]byte, error) {
	if n > size {
		n = size
	}
	data := make([]byte, n)
	got, err := file.ReadAt(data, 0)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return data[:got], nil
}

// GGUF metadata value types, as the format defines them.
const (
	ggufTypeString = 8
	ggufTypeArray  = 9
)

// The keys are namespaced by architecture in the file (`qwen3moe.block_count`), so the
// parse keeps only the suffix and resolves the prefix at the end: a file whose
// architecture key comes last reads the same as one whose key comes first.
var ggufNumberKeys = []string{".block_count", ".expert_count", ".expert_used_count"}

func parseGGUFInfo(data []byte) (GGUFInfo, error) {
	if len(data) < 4 || string(data[:4]) != "GGUF" {
		return GGUFInfo{}, fmt.Errorf("not a GGUF file")
	}
	cursor := ggufCursor{data: data, off: 4}
	version, err := cursor.u32()
	if err != nil {
		return GGUFInfo{}, err
	}
	// Versions 2 and 3 share this layout; 1 predates it and no current model uses it.
	if version < 2 || version > 3 {
		return GGUFInfo{}, fmt.Errorf("GGUF version %d is not read", version)
	}
	// The tensor count is not needed, and its table is after the key/value table.
	if _, err := cursor.u64(); err != nil {
		return GGUFInfo{}, err
	}
	kvCount, err := cursor.u64()
	if err != nil {
		return GGUFInfo{}, err
	}
	if kvCount > maxGGUFKeys {
		return GGUFInfo{}, fmt.Errorf("%d metadata keys are not a chat model", kvCount)
	}

	numbers := map[string]uint64{}
	architecture := ""
	for i := uint64(0); i < kvCount; i++ {
		key, err := cursor.str()
		if err != nil {
			return GGUFInfo{}, err
		}
		typ, err := cursor.u32()
		if err != nil {
			return GGUFInfo{}, err
		}
		if typ == ggufTypeString {
			value, err := cursor.str()
			if err != nil {
				return GGUFInfo{}, err
			}
			if key == "general.architecture" {
				architecture = value
			}
			continue
		}
		value, isNumber, err := cursor.value(typ)
		if err != nil {
			return GGUFInfo{}, err
		}
		if isNumber {
			for _, suffix := range ggufNumberKeys {
				if strings.HasSuffix(key, suffix) {
					numbers[key] = value
					break
				}
			}
		}
		// A mixture-of-experts file has all four keys — the expert_used_count comes
		// after expert_count, so all of them are required before stopping or the "how
		// many fire per token" answer is lost. The tokenizer arrays that follow are the
		// largest part of the header, which is what makes stopping worth doing; a dense
		// model never publishes expert keys and reads to the end, bounded by the window.
		if architecture != "" &&
			numbers[architecture+".block_count"] > 0 &&
			numbers[architecture+".expert_count"] > 0 &&
			numbers[architecture+".expert_used_count"] > 0 {
			break
		}
	}

	prefix := architecture
	if prefix == "" {
		// No architecture key: take it from whichever namespaced key was found, so a
		// header this app only partly understands still answers the layer question.
		for key := range numbers {
			prefix = strings.TrimSuffix(key, ".block_count")
			if prefix != key {
				break
			}
			prefix = ""
		}
	}
	return GGUFInfo{
		Architecture:    prefix,
		BlockCount:      int(numbers[prefix+".block_count"]),
		ExpertCount:     int(numbers[prefix+".expert_count"]),
		ExpertUsedCount: int(numbers[prefix+".expert_used_count"]),
	}, nil
}

// ggufCursor is a bounds-checked little-endian cursor over the read window. Every
// method returns errGGUFTruncated rather than panicking when the window ends first,
// which is what makes a partially read file harmless.
type ggufCursor struct {
	data []byte
	off  int
}

func (c *ggufCursor) skipN(n uint64) error {
	if n > uint64(len(c.data)-c.off) {
		return errGGUFTruncated
	}
	c.off += int(n)
	return nil
}

func (c *ggufCursor) u32() (uint32, error) {
	if len(c.data)-c.off < 4 {
		return 0, errGGUFTruncated
	}
	v := binary.LittleEndian.Uint32(c.data[c.off:])
	c.off += 4
	return v, nil
}

func (c *ggufCursor) u64() (uint64, error) {
	if len(c.data)-c.off < 8 {
		return 0, errGGUFTruncated
	}
	v := binary.LittleEndian.Uint64(c.data[c.off:])
	c.off += 8
	return v, nil
}

func (c *ggufCursor) str() (string, error) {
	n, err := c.u64()
	if err != nil {
		return "", err
	}
	if n > uint64(len(c.data)-c.off) {
		return "", errGGUFTruncated
	}
	s := string(c.data[c.off : c.off+int(n)])
	c.off += int(n)
	return s, nil
}

// value reads one metadata value, reporting whether it is an integer and returning it
// when it is. Everything else is consumed by its own length so the cursor stays in
// step: the keys wanted here are written as uint32 by llama.cpp's converter and as
// int32 by some others, so both carry a number.
func (c *ggufCursor) value(typ uint32) (uint64, bool, error) {
	switch typ {
	case 0: // uint8
		if err := c.skipN(1); err != nil {
			return 0, false, err
		}
		return uint64(c.data[c.off-1]), true, nil
	case 1: // int8
		if err := c.skipN(1); err != nil {
			return 0, false, err
		}
		return uint64(int64(int8(c.data[c.off-1]))), true, nil
	case 2: // uint16
		if err := c.skipN(2); err != nil {
			return 0, false, err
		}
		return uint64(binary.LittleEndian.Uint16(c.data[c.off-2:])), true, nil
	case 3: // int16
		if err := c.skipN(2); err != nil {
			return 0, false, err
		}
		return uint64(int64(int16(binary.LittleEndian.Uint16(c.data[c.off-2:])))), true, nil
	case 4: // uint32
		v, err := c.u32()
		return uint64(v), err == nil, err
	case 5: // int32
		v, err := c.u32()
		return uint64(int64(int32(v))), err == nil, err
	case 6: // float32
		return 0, false, c.skipN(4)
	case 7: // bool
		return 0, false, c.skipN(1)
	case ggufTypeString:
		_, err := c.str()
		return 0, false, err
	case 10: // uint64
		v, err := c.u64()
		return v, err == nil, err
	case 11: // int64
		v, err := c.u64()
		return uint64(int64(v)), err == nil, err
	case 12: // float64
		return 0, false, c.skipN(8)
	case ggufTypeArray:
		return 0, false, c.skipArray()
	default:
		return 0, false, fmt.Errorf("GGUF value type %d is not read", typ)
	}
}

// skipArray walks an array by its element type and count. The count is read rather
// than trusted, so a corrupt length cannot run past the window.
func (c *ggufCursor) skipArray() error {
	element, err := c.u32()
	if err != nil {
		return err
	}
	count, err := c.u64()
	if err != nil {
		return err
	}
	if size, fixed := ggufFixedSize(element); fixed {
		if count > uint64(len(c.data)-c.off)/size {
			return errGGUFTruncated
		}
		return c.skipN(count * size)
	}
	if element != ggufTypeString {
		return fmt.Errorf("GGUF array element type %d is not read", element)
	}
	for i := uint64(0); i < count; i++ {
		if _, err := c.str(); err != nil {
			return err
		}
	}
	return nil
}

// ggufFixedSize is the width of a fixed-size GGUF value, and false for strings and
// arrays.
func ggufFixedSize(typ uint32) (uint64, bool) {
	switch typ {
	case 0, 1, 7:
		return 1, true
	case 2, 3:
		return 2, true
	case 4, 5, 6:
		return 4, true
	case 10, 11, 12:
		return 8, true
	default:
		return 0, false
	}
}

// SuggestExpertLayers is a starting point for `--n-cpu-moe N`: the share of the file
// that does not fit in the free device memory, applied to the layer count.
//
// It treats the whole file as shareable, which over-offloads a little — the KV cache
// and the attention weights are on the card either way. That is the safe direction for
// a first try, because too much in RAM costs speed while too little costs an
// out-of-memory start; the panel says so rather than presenting it as a measurement.
// 0 means "nothing has to leave": the file fits, so there is nothing to suggest.
func SuggestExpertLayers(blockCount int, modelBytes, freeBytes int64) int {
	if blockCount <= 0 || modelBytes <= 0 {
		return 0
	}
	// No device memory reported is not "no memory": a CPU-only build or a missing
	// runtime cannot say, and suggesting all of it would be inventing an answer.
	if freeBytes <= 0 || modelBytes <= freeBytes {
		return 0
	}
	share := float64(modelBytes-freeBytes) / float64(modelBytes)
	n := int(float64(blockCount)*share + 0.999999)
	return min(max(n, 1), blockCount)
}

// SuggestForModel is SuggestExpertLayers for a header that has been read.
//
// A dense model has no expert tensors, so there is nothing to place and nothing to
// suggest, whatever its size: the flags would be null operations at best. A number here
// would reach `--n-cpu-moe` from the panel, which fills an undecided file with the
// suggestion, and from the chat's picker, which does the same at a first start — a
// setting on a model that has no experts to move.
func SuggestForModel(info GGUFInfo, modelBytes, freeBytes int64) int {
	if info.ExpertCount <= 0 {
		return 0
	}
	return SuggestExpertLayers(info.BlockCount, modelBytes, freeBytes)
}
