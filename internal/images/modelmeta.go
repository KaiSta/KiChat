package images

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// What a weights file says about itself before a single byte of it is loaded.
//
// The header is the one place a checkpoint cannot be lied about by its name: a rename, a
// re-quantisation or a folder called "models" leaves it alone. Both formats the image
// backends accept carry it — GGUF in a key/value table and a tensor table, safetensors in
// its JSON header — and reading it costs one bounded read that never touches the weights:
// 26–41 kB for the published files, against 11 GB of tensors.
const (
	// headerLimit bounds what is read to identify a file. The published FLUX 3 headers are
	// 26.5 kB as GGUF and 40.5 kB as safetensors (measured with these readers on the files
	// named in IMAGES.md §9.5.1), so this is a guard against a corrupt or hostile length
	// field rather than a working figure.
	headerLimit = 8 << 20
	// ggufFirstRead is the first attempt at a GGUF header. It is an order of magnitude above
	// every published one — 26.5 kB for FLUX 3 — and the parse asks for more when a file's
	// key/value table is longer than this.
	ggufFirstRead = 256 << 10
	// safetensorsLimit is separate because a safetensors file states its header length:
	// the number is read from the file and must not be able to ask for a 4 GB allocation.
	safetensorsLimit = 16 << 20
	// maxHeaderTensors caps the table kept in memory. Every known image checkpoint has
	// hundreds of entries; anything near this is not a diffusion model.
	maxHeaderTensors = 20000
	// maxTensorName bounds one name. GGUF and safetensors names are ASCII identifiers of
	// tens of bytes; a long "name" is a corrupt file.
	maxTensorName = 256
	// cachedHeaders is how many files stay in memory. Two or three diffusion models are
	// configured at once at most, and the cache is dropped whole when it fills.
	cachedHeaders = 8
)

// modelHeader is the part of a weights file this package reads: the tensor names, which
// identify the architecture, and the string metadata beside them.
//
// Format is "gguf", "safetensors", or empty for a file that is neither. Tensors is never
// nil when the file was read successfully, and empty when the header was: a format nobody
// here knows is not an error, it is a file whose pipeline has to come from its name.
//
// Meta is the header's human-readable half. It does not decide anything: none of the
// published FLUX 3 files names itself, and one conversion carries no keys at all
// (IMAGES.md §9.5.1). It is kept because it is what an unknown future checkpoint would be
// most likely to say, and because the test reads it to keep that claim checkable.
type modelHeader struct {
	Format  string
	Meta    map[string]string
	Tensors []string
}

// errHeaderTruncated reports that the read window ended inside the header. It is not a
// failure to report to a user — the caller falls back to the file name — so it is
// distinguished only so a test can tell "unreadable file" from "readable, unknown layout".
var errHeaderTruncated = errors.New("model header does not fit in the read window")

// inspectModel reads a weights file's header and caches the result.
//
// The cache is keyed by path and invalidated by size and modification time, which is what
// a re-quantisation or a replaced download changes. It exists because the pipeline is
// resolved on every settings change and on several call paths inside one draw, and a
// header read per call would be a surprising amount of disk for a value that cannot have
// changed between them.
func inspectModel(path string) (modelHeader, error) {
	if path == "" {
		return modelHeader{}, os.ErrNotExist
	}
	info, err := os.Stat(path)
	if err != nil {
		return modelHeader{}, err
	}
	if info.IsDir() {
		return modelHeader{}, fmt.Errorf("%s ist ein Ordner", path)
	}

	modelHeaders.mu.Lock()
	if entry, ok := modelHeaders.byPath[path]; ok && entry.size == info.Size() && entry.mod.Equal(info.ModTime()) {
		modelHeaders.mu.Unlock()
		return entry.header, entry.err
	}
	modelHeaders.mu.Unlock()

	header, err := readModelHeader(path, info.Size())

	modelHeaders.mu.Lock()
	if len(modelHeaders.byPath) >= cachedHeaders {
		modelHeaders.byPath = make(map[string]cachedHeader)
	}
	modelHeaders.byPath[path] = cachedHeader{size: info.Size(), mod: info.ModTime(), header: header, err: err}
	modelHeaders.mu.Unlock()
	return header, err
}

type cachedHeader struct {
	size   int64
	mod    time.Time
	header modelHeader
	err    error
}

var modelHeaders = struct {
	mu     sync.Mutex
	byPath map[string]cachedHeader
}{byPath: make(map[string]cachedHeader)}

// forgetModelHeaders drops the cache. Only a test needs it: nothing else can make a file at
// the same path change under an unchanged size and modification time.
func forgetModelHeaders() {
	modelHeaders.mu.Lock()
	modelHeaders.byPath = make(map[string]cachedHeader)
	modelHeaders.mu.Unlock()
}

// readModelHeader reads as little of the file as will identify it: the format's own
// length fields where they exist, and a first attempt of ggufFirstRead for GGUF, whose
// header length is not stated anywhere and only the parse can reveal it is short.
func readModelHeader(path string, size int64) (modelHeader, error) {
	file, err := os.Open(path)
	if err != nil {
		return modelHeader{}, err
	}
	defer file.Close()

	switch {
	case size >= 8:
		prefix := make([]byte, 8)
		if _, err := io.ReadFull(file, prefix); err != nil {
			return modelHeader{}, err
		}
		if string(prefix[:4]) == "GGUF" {
			data, err := readWindow(file, size, ggufFirstRead)
			if err != nil {
				return modelHeader{}, err
			}
			header, err := parseGGUF(data)
			if errors.Is(err, errHeaderTruncated) && int64(len(data)) < size && int64(len(data)) < headerLimit {
				// A header bigger than the first window: read the full, still bounded,
				// window once rather than giving up on a file that is merely verbose.
				data, err = readWindow(file, size, headerLimit)
				if err != nil {
					return modelHeader{}, err
				}
				header, err = parseGGUF(data)
			}
			return header, err
		}
		// safetensors states its header length in the eight bytes before the JSON.
		length := binary.LittleEndian.Uint64(prefix)
		if length == 0 || length > safetensorsLimit {
			return modelHeader{}, fmt.Errorf("%s: safetensors-Kopf von %d Bytes wird nicht gelesen", path, length)
		}
		data, err := readWindow(file, size, 8+int64(length))
		if err != nil {
			return modelHeader{}, err
		}
		if len(data) < 8 || data[8] != '{' {
			return modelHeader{}, fmt.Errorf("%s ist keine GGUF- oder safetensors-Datei", path)
		}
		return parseSafetensors(data)
	default:
		return modelHeader{}, errHeaderTruncated
	}
}

// readWindow returns the first n bytes of the file, or all of it when it is shorter. It is
// the only read this package does, and n is always one of the two limits above.
func readWindow(file *os.File, size, n int64) ([]byte, error) {
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

// --- GGUF -------------------------------------------------------------------

// GGUF's header is a version, two counts, a key/value table and a tensor table. Only the
// string values are kept: they are the human-readable part (general.architecture, model,
// dtype) and every scalar or array in between is skipped by its own length.
func parseGGUF(data []byte) (modelHeader, error) {
	r := headerReader{data: data, off: 4}
	version, err := r.u32()
	if err != nil {
		return modelHeader{}, err
	}
	// Versions 2 and 3 share this layout; 1 predates it and a diffusion model has never
	// been published in it.
	if version < 2 || version > 3 {
		return modelHeader{}, fmt.Errorf("GGUF-Version %d wird nicht gelesen", version)
	}
	tensorCount, err := r.u64()
	if err != nil {
		return modelHeader{}, err
	}
	kvCount, err := r.u64()
	if err != nil {
		return modelHeader{}, err
	}
	if tensorCount > maxHeaderTensors {
		return modelHeader{}, fmt.Errorf("%d Tensoren sind kein Diffusionsmodell", tensorCount)
	}

	header := modelHeader{Format: "gguf", Meta: map[string]string{}, Tensors: []string{}}
	for i := uint64(0); i < kvCount; i++ {
		key, err := r.str()
		if err != nil {
			return modelHeader{}, err
		}
		typ, err := r.u32()
		if err != nil {
			return modelHeader{}, err
		}
		if typ == ggufString {
			value, err := r.str()
			if err != nil {
				return modelHeader{}, err
			}
			header.Meta[key] = value
			continue
		}
		if err := r.skip(typ); err != nil {
			return modelHeader{}, err
		}
	}
	for i := uint64(0); i < tensorCount; i++ {
		name, err := r.str()
		if err != nil {
			return modelHeader{}, err
		}
		dims, err := r.u32()
		if err != nil {
			return modelHeader{}, err
		}
		if err := r.skipN(uint64(dims) * 8); err != nil {
			return modelHeader{}, err
		}
		// Tensor type, then the offset of its data in the file.
		if err := r.skipN(4 + 8); err != nil {
			return modelHeader{}, err
		}
		if len(name) <= maxTensorName {
			header.Tensors = append(header.Tensors, name)
		}
	}
	return header, nil
}

// GGUF value type ids, as the format defines them. They are used for skipping only; the
// strings are the values this package reads.
const (
	ggufString = 8
	ggufArray  = 9
)

// headerReader is a bounds-checked little-endian cursor over the read window. Every method
// returns errHeaderTruncated rather than panicking when the window ends first, which is
// what makes a partially read file harmless.
type headerReader struct {
	data []byte
	off  int
}

func (r *headerReader) skipN(n uint64) error {
	if n > uint64(len(r.data)-r.off) {
		return errHeaderTruncated
	}
	r.off += int(n)
	return nil
}

func (r *headerReader) u32() (uint32, error) {
	if len(r.data)-r.off < 4 {
		return 0, errHeaderTruncated
	}
	v := binary.LittleEndian.Uint32(r.data[r.off:])
	r.off += 4
	return v, nil
}

func (r *headerReader) u64() (uint64, error) {
	if len(r.data)-r.off < 8 {
		return 0, errHeaderTruncated
	}
	v := binary.LittleEndian.Uint64(r.data[r.off:])
	r.off += 8
	return v, nil
}

func (r *headerReader) str() (string, error) {
	n, err := r.u64()
	if err != nil {
		return "", err
	}
	if n > uint64(len(r.data)-r.off) {
		return "", errHeaderTruncated
	}
	s := string(r.data[r.off : r.off+int(n)])
	r.off += int(n)
	return s, nil
}

// skip advances over a value of the given type without keeping it. Array lengths are read
// rather than trusted, and an array of strings is the only case that has to walk its
// elements — which it does by consuming each one, so it cannot run longer than the window.
func (r *headerReader) skip(typ uint32) error {
	switch typ {
	case 0, 1, 7: // uint8, int8, bool
		return r.skipN(1)
	case 2, 3: // uint16, int16
		return r.skipN(2)
	case 4, 5, 6: // uint32, int32, float32
		return r.skipN(4)
	case 10, 11, 12: // uint64, int64, float64
		return r.skipN(8)
	case ggufString:
		_, err := r.str()
		return err
	case ggufArray:
		element, err := r.u32()
		if err != nil {
			return err
		}
		count, err := r.u64()
		if err != nil {
			return err
		}
		if size, fixed := ggufScalarSize(element); fixed {
			if count > uint64(len(r.data)-r.off)/uint64(size) {
				return errHeaderTruncated
			}
			return r.skipN(count * uint64(size))
		}
		if element != ggufString {
			return fmt.Errorf("GGUF-Arraytyp %d wird nicht gelesen", element)
		}
		for i := uint64(0); i < count; i++ {
			if _, err := r.str(); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("GGUF-Werttyp %d wird nicht gelesen", typ)
	}
}

// ggufScalarSize is the width of a fixed-size GGUF value, and false for strings and arrays.
func ggufScalarSize(typ uint32) (uint64, bool) {
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

// --- safetensors ------------------------------------------------------------

// safetensors is a length-prefixed JSON object: eight little-endian bytes, then that many
// bytes of {"__metadata__": {...}, "tensor.name": {"dtype": …, "shape": …}}. The tensor
// names are the keys and are what identifies the layout, exactly as in a GGUF.
func parseSafetensors(data []byte) (modelHeader, error) {
	length := binary.LittleEndian.Uint64(data[:8])
	if length == 0 || length > safetensorsLimit {
		return modelHeader{}, fmt.Errorf("safetensors-Kopf von %d Bytes wird nicht gelesen", length)
	}
	if length > uint64(len(data)-8) {
		return modelHeader{}, errHeaderTruncated
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data[8:8+length], &raw); err != nil {
		return modelHeader{}, fmt.Errorf("safetensors-Kopf ist kein JSON: %w", err)
	}

	header := modelHeader{Format: "safetensors", Meta: map[string]string{}, Tensors: []string{}}
	if metadata, ok := raw["__metadata__"]; ok {
		var values map[string]string
		// A metadata block is a flat string map by the format's definition. One that is
		// not is ignored rather than fatal: the tensors still identify the model.
		if err := json.Unmarshal(metadata, &values); err == nil {
			for key, value := range values {
				header.Meta[key] = value
			}
		}
		delete(raw, "__metadata__")
	}
	for name := range raw {
		if len(name) > maxTensorName {
			continue
		}
		header.Tensors = append(header.Tensors, name)
		if len(header.Tensors) > maxHeaderTensors {
			return modelHeader{}, fmt.Errorf("mehr als %d Tensoren sind kein Diffusionsmodell", maxHeaderTensors)
		}
	}
	return header, nil
}
