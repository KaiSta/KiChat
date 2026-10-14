package runtime

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Kinds of folder a model may already be in. The UI names them; the app only
// needs to know which are worth offering by default.
const (
	// FolderLMStudioConfigured is the folder LM Studio itself was pointed at. It
	// comes first because it is the one that actually holds the user's downloads:
	// the location is a setting in LM Studio, and a user with models on a second
	// drive has it pointing there while `~/.lmstudio/models` sits empty.
	FolderLMStudioConfigured = "lmstudio-configured"
	// FolderLMStudio is LM Studio's own models directory.
	FolderLMStudio = "lmstudio"
	// FolderLMStudioLegacy is where releases before 0.3.x kept them.
	FolderLMStudioLegacy = "lmstudio-legacy"
	// FolderLMStudioBundled is LM Studio's own bundled models, which exist as soon
	// as it has been started once.
	FolderLMStudioBundled = "lmstudio-bundled"
	// FolderCustom is a folder the user named.
	FolderCustom = "custom"
)

// Limits on one scan.
//
// A models folder is not bounded by anything the user controls: LM Studio nests
// each download two or three levels deep, a user can point this at a drive root
// by mistake, and a network share can hold tens of thousands of files. The walk
// therefore stops at a depth and a file count and says so, rather than freezing
// the window on a folder nobody meant to name.
const (
	ModelScanMaxDepth = 6
	ModelScanMaxFiles = 500
)

// ModelFolder is a place GGUF files may already be.
type ModelFolder struct {
	Path string `json:"path"`
	// Kind says where the folder came from, so the UI can label it.
	Kind string `json:"kind"`
	// Exists reports whether it is there right now. A folder that is not is still
	// returned: which locations the app looked in is part of the answer.
	Exists bool `json:"exists"`
}

// ModelFile is one GGUF found under a folder.
type ModelFile struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// Model is the folder the file sits in, publisher and model name, which is
	// what tells two quants of one model apart in a list.
	Model string `json:"model"`
	Size  int64  `json:"size"`
}

// ModelFolderScan is the result of looking in one folder.
type ModelFolderScan struct {
	Dir   string      `json:"dir"`
	Files []ModelFile `json:"files"`
	// Truncated says the walk stopped at a limit instead of at the end of the
	// folder, so a short list is not mistaken for a complete one.
	Truncated bool `json:"truncated"`
}

// LMStudioFolders lists where LM Studio keeps models, most likely first.
//
// The order is the whole point: on the machine this was written on,
// `~/.lmstudio/models` exists and is empty while LM Studio's own setting names a
// folder on another drive that holds six chat models. Offering the first folder
// on the list rather than the first that exists is what makes the difference
// between "no models found" and the user's actual models.
func LMStudioFolders(home string) []ModelFolder {
	folders := []ModelFolder{}
	if dir := lmStudioDownloadsFolder(home); dir != "" {
		folders = append(folders, ModelFolder{Path: dir, Kind: FolderLMStudioConfigured})
	}
	folders = append(folders,
		ModelFolder{Path: filepath.Join(home, ".lmstudio", "models"), Kind: FolderLMStudio},
		ModelFolder{Path: filepath.Join(home, ".cache", "lm-studio", "models"), Kind: FolderLMStudioLegacy},
		ModelFolder{Path: filepath.Join(home, ".lmstudio", ".internal", "bundled-models"), Kind: FolderLMStudioBundled},
	)
	for i := range folders {
		folders[i].Exists = IsDir(folders[i].Path)
	}
	return folders
}

// lmStudioDownloadsFolder reads the folder out of LM Studio's own settings file.
//
// It is read as one string field rather than as a whole schema: this is another
// application's private file, it changes shape between releases, and all that is
// wanted from it is a hint. Anything unexpected — no file, no such field, a
// value that is not a string — simply yields no folder, and the platform
// defaults below are offered instead.
func lmStudioDownloadsFolder(home string) string {
	if home == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".lmstudio", "settings.json"))
	if err != nil {
		return ""
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return ""
	}
	for _, key := range []string{"downloadsFolder", "modelsFolder", "modelsRootFolder"} {
		value, ok := settings[key].(string)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		if dir := strings.TrimSpace(value); IsDir(dir) {
			return dir
		}
	}
	return ""
}

// isProjector reports whether a GGUF is a model projector rather than a model.
//
// `mmproj` is looked for as a *token* and not as a prefix, because publishers spell it
// two ways and only one of them starts the file name: LM Studio's own downloads are
// `mmproj-<model>-F16.gguf`, while the same file on Hugging Face carries the model's
// name first — the ternary model this app added a second llama.cpp for ships
// `Ternary-Bonsai-2-27B-mmproj-BF16.gguf`, which a prefix test offered as a model that
// llama.cpp then refuses to load ("the current context does not support logits
// computation"). Measured on that folder, on the user's own download.
func isProjector(name string) bool {
	lower := strings.ToLower(name)
	for _, token := range strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if token == "mmproj" {
			return true
		}
	}
	return false
}

// ScanModelFolder lists the GGUF files under dir.
//
// Model projectors are skipped: LM Studio keeps a vision model's `mmproj` file
// beside it under the same extension, it is often close to a gigabyte, and
// llama-server refuses it as a model with "the current context does not support
// logits computation" and similar — a file offered as a choice but unloadable is
// worse than one that is not offered at all.
func ScanModelFolder(dir string) (ModelFolderScan, error) {
	scan := ModelFolderScan{Dir: dir, Files: []ModelFile{}}
	if dir == "" {
		return scan, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return scan, err
	}
	base := abs
	info, err := os.Stat(abs)
	if err != nil {
		return scan, err
	}
	if !info.IsDir() {
		return scan, &fs.PathError{Op: "scan", Path: abs, Err: fs.ErrInvalid}
	}

	queue := []struct {
		dir   string
		depth int
	}{{abs, 0}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		entries, err := os.ReadDir(current.dir)
		if err != nil {
			// An unreadable subdirectory is not a reason to fail the whole
			// listing: the rest of the folder is still worth showing.
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			full := filepath.Join(current.dir, name)
			if entry.IsDir() {
				// Hidden folders are LM Studio's own housekeeping (`.internal`,
				// `.cache`) unless one of them was named as the root, which is
				// handled by the depth check alone.
				if strings.HasPrefix(name, ".") || current.depth >= ModelScanMaxDepth {
					continue
				}
				queue = append(queue, struct {
					dir   string
					depth int
				}{full, current.depth + 1})
				continue
			}
			if !strings.HasSuffix(strings.ToLower(name), ".gguf") {
				continue
			}
			if isProjector(name) {
				continue
			}
			fileInfo, err := entry.Info()
			if err != nil {
				continue
			}
			scan.Files = append(scan.Files, ModelFile{
				Path:  full,
				Name:  name,
				Model: relativeModel(current.dir, base),
				Size:  fileInfo.Size(),
			})
			if len(scan.Files) >= ModelScanMaxFiles {
				scan.Truncated = true
				return scan, nil
			}
		}
	}

	// Sorted by model and then by name, so two quants of one model sit together
	// and the list does not reshuffle between two scans of the same folder.
	sort.Slice(scan.Files, func(i, j int) bool {
		if scan.Files[i].Model != scan.Files[j].Model {
			return scan.Files[i].Model < scan.Files[j].Model
		}
		return scan.Files[i].Name < scan.Files[j].Name
	})
	return scan, nil
}

// fileTypeToken matches the parts of a GGUF file name that say what *kind* of file
// this is rather than which model it holds: the numeric format, the quantization, and
// the projector marker itself.
//
// They are dropped before two names are compared, because a model and its projector
// are published in different quants by design — `Qwen3-VL-8B-Instruct-Q4_K_M.gguf`
// sits beside `mmproj-Qwen3-VL-8B-Instruct-F16.gguf` — and counting them would make a
// model and its own projector look less alike than two projectors of that model look
// to each other.
var fileTypeToken = regexp.MustCompile(`^(mmproj|f\d+|bf\d+|fp\d+|i?q\d+.*)$`)

// modelTokens is the set of tokens in a GGUF file name that identify the model,
// lowercased and with the file-type tokens above removed.
//
// The extension is dropped after lowercasing, not before. The two halves of a vision
// model are not always published by the same hand — `Qwen3-VL-8B-Instruct-Q4_K_M.gguf`
// from one upload and `mmproj-Qwen3-VL-8B-Instruct-F16.GGUF` from another — and an
// extension left in place enters as the token `gguf`, which the other name does not
// have, so the containment rule below would reject the model's own projector and leave
// a vision model that silently cannot see.
func modelTokens(name string) map[string]bool {
	lower := strings.TrimSuffix(strings.ToLower(name), ".gguf")
	tokens := map[string]bool{}
	for _, token := range strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if token == "" || fileTypeToken.MatchString(token) {
			continue
		}
		tokens[token] = true
	}
	return tokens
}

// ProjectorsBeside lists the projector GGUFs in the folder a model file lives in,
// sorted by name so two scans of one folder agree.
//
// Only that one folder is read. It is where a download puts the pair — LM Studio
// beside each model, Hugging Face beside each quant — and a walk of the whole tree
// would pair a model with a projector belonging to a different model three levels
// away.
//
// A path that names no folder is answered with nothing rather than read: a bare file
// name resolves against the *process's* working directory, which is wherever the app
// happened to be started from and has nothing to do with where the model is. Every
// other path this app handles is absolute — the file dialog and the folder scan both
// produce ones that are — so nothing that can actually be loaded is given up by it.
func ProjectorsBeside(modelPath string) []string {
	modelPath = strings.TrimSpace(modelPath)
	if modelPath == "" || !filepath.IsAbs(modelPath) {
		return []string{}
	}
	dir := filepath.Dir(modelPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{}
	}
	found := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(name), ".gguf") || !isProjector(name) {
			continue
		}
		found = append(found, filepath.Join(dir, name))
	}
	sort.Strings(found)
	return found
}

// MatchProjector names the projector that belongs to a model file, or "" when the
// folder holds none, holds one that belongs to a different model, or holds several that
// fit equally well.
//
// Two rules, and the name decides both. A projector whose name carries model tokens that
// this model's name does not contain is another model's, however alone it is in the
// folder: `mmproj-Llama-3-8B-F16.gguf` beside a Qwen3-VL-8B shares the size and nothing
// else, and a vision tower that does not fit the model it is loaded with is worse than
// none — so the whole name has to be contained, not merely to overlap. A projector whose
// name carries no identity at all (`mmproj.gguf`, `mmproj-f16.gguf`) contradicts nothing
// and is kept, which is what a hand-assembled folder looks like. What remains is decided
// by shared tokens, which is how the projector of `Qwen3-VL-8B-Instruct-Q4_K_M.gguf` is
// told from a 4B sibling's in the same folder.
//
// A tie is reported as *no answer* rather than broken by name order. Two projectors that
// fit one model equally well are two quants of one file, and a silent pick from an
// arbitrary sort is how a model ends up loading a vision tower nobody chose — which is a
// 500 on the next image rather than an error anyone can read.
//
// The comparison is case-insensitive throughout, which is why the file names are folded
// before their tokens are taken: `.GGUF` is a spelling of the extension and not part of
// any model's name.
func MatchProjector(modelPath string) string {
	candidates := ProjectorsBeside(modelPath)
	if len(candidates) == 0 {
		return ""
	}
	want := modelTokens(filepath.Base(modelPath))
	kept, scores := []string{}, []int{}
	for _, candidate := range candidates {
		tokens := modelTokens(filepath.Base(candidate))
		score := 0
		for token := range tokens {
			if want[token] {
				score++
			}
		}
		if !subsetOf(tokens, want) {
			continue
		}
		kept, scores = append(kept, candidate), append(scores, score)
	}
	if len(kept) == 0 {
		return ""
	}
	if len(kept) == 1 {
		return kept[0]
	}
	best, bestScore, tied := "", 0, false
	for i, candidate := range kept {
		switch {
		case scores[i] > bestScore:
			best, bestScore, tied = candidate, scores[i], false
		case scores[i] == bestScore:
			// Reached by the first candidate too, when every score is 0: several
			// nameless projectors fit equally well, so the answer stays empty.
			tied = true
		}
	}
	if bestScore == 0 || tied {
		return ""
	}
	return best
}

// subsetOf reports whether every token of sub is also in super, which is the test that
// rules out another model's projector without ruling out a nameless one.
func subsetOf(sub, super map[string]bool) bool {
	for token := range sub {
		if !super[token] {
			return false
		}
	}
	return true
}

// relativeModel names the folder a file was found in, relative to the root, so a
// row can say `lmstudio-community/LFM2.5-2.6B-GGUF` instead of repeating the
// whole path.
func relativeModel(dir, base string) string {
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == "." {
		return filepath.Base(dir)
	}
	return rel
}

// IsDir reports whether path names a directory that is there right now, which is
// what turns a candidate location into one worth offering.
func IsDir(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
