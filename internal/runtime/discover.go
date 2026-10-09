package runtime

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
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
			if strings.HasPrefix(strings.ToLower(name), "mmproj") {
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
