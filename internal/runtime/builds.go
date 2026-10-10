package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Build is one llama.cpp build in the installer's directory.
//
// The list exists because a build is not small and nothing removes the one an update
// replaces: measured against b11535, the Linux CPU archive is 17.8 MB, ROCm 244.9 MB,
// and CUDA 593 MB once its 440 MB runtime bundle is unpacked beside the launcher. Two
// CUDA updates therefore leave more than a gigabyte of archives nobody will run
// again, and no part of the app used to say so.
type Build struct {
	// Dir is the directory name, which is also what the settings record as the
	// installed tag: Asset.Dir writes `b11541`, `b11541-vulkan` or
	// `b11541-vulkan-10.0`.
	Dir string
	// SizeBytes is what the directory occupies, measured by walking it. It is the
	// whole footprint, not the archive's: a companion runtime is part of the build
	// once it has been merged in, and it is the larger part of a CUDA install.
	SizeBytes int64
	// InstalledAt is when the build landed — the launcher's modification time, which
	// is the only timestamp an extracted archive leaves behind that means anything.
	InstalledAt time.Time
	// Incomplete marks what an interrupted install left behind: the staging
	// directory this package extracts into and removes when it finishes. It is
	// always safe to remove, and it is listed rather than swept up because a crash
	// in the middle of a 600 MB download otherwise leaves a directory nobody can
	// account for.
	Incomplete bool
}

// Builds lists the builds in the runtime directory, newest first.
//
// A directory counts as a build when a launcher is in it, which is what separates a
// build from a folder someone else put there; the leftovers of an interrupted install
// are listed as incomplete. Everything else is left alone on purpose — this app
// removes what it wrote, and a directory it cannot account for is not its to delete.
func (i *Installer) Builds() []Build {
	entries, err := os.ReadDir(i.Dir)
	if err != nil {
		return []Build{}
	}
	out := make([]Build, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		build := Build{Dir: name}
		binary, info := i.launcherUnder(filepath.Join(i.Dir, name))
		switch {
		// The leftover is asked about first, because it is the leftover whatever is
		// in it: a crash after the archive was flattened leaves a launcher in the
		// staging directory, and reporting that as an ordinary build would hide the
		// one directory nothing else will ever clean up. It also must not be offered
		// to a start — Find skips it for the same reason.
		case isInstallLeftover(name):
			build.Incomplete = true
			switch {
			case info != nil:
				build.InstalledAt = info.ModTime()
			default:
				if entryInfo, err := entry.Info(); err == nil {
					build.InstalledAt = entryInfo.ModTime()
				}
			}
		case binary != "":
			build.InstalledAt = info.ModTime()
		default:
			continue
		}
		build.SizeBytes = dirSize(filepath.Join(i.Dir, name))
		out = append(out, build)
	}
	// Newest first, with the name as the tie-breaker: two builds installed in the
	// same second — a script, a test — still come out in a stable order.
	sort.Slice(out, func(a, b int) bool {
		if !out[a].InstalledAt.Equal(out[b].InstalledAt) {
			return out[a].InstalledAt.After(out[b].InstalledAt)
		}
		return out[a].Dir > out[b].Dir
	})
	return out
}

// isInstallLeftover reports whether a directory name is one this package writes while
// an install runs and removes when it finishes. A crash in between leaves it, and
// that is the case the cleanup list is for.
//
// The marker is searched for rather than anchored at the end, because an install
// writes three names of this kind and only one of them ends in `.installing`: the
// staging directory is `<build>.installing`, `flattenSingleRoot` renames the
// archive's inner root to `<build>.installing.root`, and a companion runtime unpacks
// into `<build>.installing.runtime`. A crash between those renames leaves a
// directory holding the full, startable build under the `.root` name — Find would
// pick it as the newest build and the cleanup list would present it as a complete
// one, which is exactly what the suffix test did before. No real build directory
// contains `.installing`, so a substring is safe here.
func isInstallLeftover(name string) bool {
	return strings.Contains(name, ".installing")
}

// RemoveBuild deletes one installed build, named the way Builds reported it.
//
// The name is checked rather than trusted: it arrives from the UI, and `..` or an
// absolute path would otherwise hand this function a directory that is not a build at
// all. A directory holding no launcher is refused for the same reason, unless it is
// the leftover of an interrupted install — this removes what it wrote, nothing else.
func (i *Installer) RemoveBuild(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("kein Build angegeben")
	}
	if dir != filepath.Base(dir) || dir == "." || dir == ".." {
		return fmt.Errorf("kein Build-Verzeichnisname: %s", dir)
	}
	path := filepath.Join(i.Dir, dir)
	if launcher, _ := i.launcherUnder(path); launcher == "" && !isInstallLeftover(dir) {
		return fmt.Errorf("in %s liegt kein llama.cpp-Build", dir)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("Build entfernen: %w", err)
	}
	return nil
}

// dirSize is what one build occupies on disk.
//
// A symlink is counted as the link it is: the archives are full of them (every
// `libllama.so.0` points at a versioned file beside it), and following one would count
// the same bytes twice. An entry that cannot be read leaves the total approximate,
// which is the right trade for a number the panel prints.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
