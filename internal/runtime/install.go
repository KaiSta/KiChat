// Package runtime installs and supervises a local llama.cpp server, so a model
// can be run by KiChat itself rather than by a server the user started first.
//
// Two jobs live in one package because they share one directory layout:
//
//   - Installer fetches llama-server from llama.cpp's own releases. Those archives
//     are not a single file: `llama-server` is a thin launcher whose code sits in
//     `libllama-server-impl.so`, and its siblings (`libllama.so.0.6.0`,
//     `libggml*.so`, symlinks pointing at them) have to be next to it for it to
//     start. Measured on llama-b11528-bin-ubuntu-x64.tar.gz: 61 entries, a 17 KB
//     launcher beside a 7.6 MB implementation library. So an install unpacks the
//     whole archive into one directory and runs the launcher from there.
//
//   - Server supervises one llama-server process for one model: a free port, an
//     address, readiness from /health, the log tail so a failure can be explained
//     instead of merely reported, and a shutdown that does not leave the process
//     running after the app is gone.
package runtime

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// ReleasesAPI lists llama.cpp's releases, newest first.
	ReleasesAPI = "https://api.github.com/repos/ggml-org/llama.cpp/releases"
	// DownloadBase is the URL every published asset lives under.
	DownloadBase = "https://github.com/ggml-org/llama.cpp/releases/download"

	// maxExtractBytes bounds what one archive may unpack to, so a truncated or
	// hostile archive cannot fill the disk. The measured Linux CPU archive is
	// 17.8 MB compressed, and the largest desktop build is about 590 MB.
	maxExtractBytes = 4 << 30

	// AcceleratorCPU is the build that runs everywhere: no GPU runtime required.
	AcceleratorCPU = "cpu"
	// AcceleratorVulkan uses the GPU through Vulkan, which needs a driver but no
	// vendor toolkit, so it is the portable way to get acceleration.
	AcceleratorVulkan = "vulkan"

	// userAgent identifies the client to GitHub, which refuses requests without
	// one.
	userAgent = "kichat-llama-runtime"
)

// Asset is one downloadable archive.
type Asset struct {
	Tag    string
	Name   string
	URL    string
	Size   int64
	Digest string // "sha256:<hex>", empty when the API does not report one.
}

// Progress is reported while an install runs.
type Progress struct {
	Phase string // "downloading" | "extracting"
	Done  int64
	Total int64
}

// Installer owns the runtime directory: `<dataDir>/runtime`, one subdirectory per
// llama.cpp build.
type Installer struct {
	Dir string
	// APIBase and Client are fields so the release lookup can be tested without
	// reaching GitHub.
	APIBase string
	Client  *http.Client

	// OnProgress is called as an install proceeds. It may be nil.
	OnProgress func(Progress)
}

// NewInstaller builds an installer rooted at dir.
func NewInstaller(dir string) *Installer {
	return &Installer{
		Dir:     dir,
		APIBase: ReleasesAPI,
		Client:  &http.Client{Timeout: 30 * time.Minute},
	}
}

// AssetName is the archive llama.cpp publishes for a platform, or an error naming
// what is not covered.
//
// The names are per platform rather than per operating-system convention: Linux
// is "ubuntu", Windows says "win-cpu" where the others imply the CPU build, macOS
// carries no accelerator at all (its builds already use Metal), and the extension
// follows the asset rather than the host — every desktop Unix build is .tar.gz
// while Windows is .zip, including when KiChat runs on Linux and fetches for
// Windows. Exported because the settings UI names the build it would fetch.
func AssetName(tag, goos, goarch, accel string) (string, error) {
	if accel == "" {
		accel = AcceleratorCPU
	}
	var platform, ext string
	switch goos {
	case "linux":
		arch, err := unixArch(goarch, "ubuntu")
		if err != nil {
			return "", err
		}
		platform, ext = "ubuntu-"+arch, ".tar.gz"
		if accel == AcceleratorVulkan {
			platform = "ubuntu-vulkan-" + arch
		}
	case "darwin":
		// macOS builds are Metal-enabled and published one per architecture;
		// there is no Vulkan build for them, so asking for one is an error
		// rather than a silent fallback to a build the user did not ask for.
		if accel != AcceleratorCPU {
			return "", fmt.Errorf("für macOS gibt es keinen %s-Build", accel)
		}
		switch goarch {
		case "arm64":
			platform = "macos-arm64"
		case "amd64":
			platform = "macos-x64"
		default:
			return "", fmt.Errorf("macOS auf %s wird nicht veröffentlicht", goarch)
		}
		ext = ".tar.gz"
	case "windows":
		arch, err := unixArch(goarch, "windows")
		if err != nil {
			return "", err
		}
		platform, ext = "win-cpu-"+arch, ".zip"
		if accel == AcceleratorVulkan {
			platform = "win-vulkan-" + arch
		}
	default:
		return "", fmt.Errorf("für %s wird llama.cpp nicht veröffentlicht", goos)
	}
	return fmt.Sprintf("llama-%s-bin-%s%s", tag, platform, ext), nil
}

// unixArch maps Go's architecture names onto the ones llama.cpp uses.
func unixArch(goarch, goos string) (string, error) {
	switch goarch {
	case "amd64":
		return "x64", nil
	case "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("für %s auf %s wird llama.cpp nicht veröffentlicht", goos, goarch)
	}
}

// BinaryPath is where the launcher of one installed build lives. Windows keeps the
// .exe suffix; nothing else does.
func (i *Installer) BinaryPath(tag string) string {
	return filepath.Join(i.Dir, tag, launcherName())
}

func launcherName() string {
	if os.PathSeparator == '\\' {
		return "llama-server.exe"
	}
	return "llama-server"
}

// Find reports the newest build already unpacked in the runtime directory, and
// its launcher. An empty tag means nothing is installed.
//
// The newest is chosen by modification time rather than by comparing build
// numbers: a build tag is opaque text (`b11528` today, something else tomorrow),
// and the directory that was written most recently is the one that was installed
// most recently.
func (i *Installer) Find() (tag, binary string) {
	entries, err := os.ReadDir(i.Dir)
	if err != nil {
		return "", ""
	}
	var newest time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bin, info := i.launcherUnder(filepath.Join(i.Dir, e.Name()))
		if bin == "" {
			continue
		}
		if info.ModTime().After(newest) {
			newest, tag, binary = info.ModTime(), e.Name(), bin
		}
	}
	return tag, binary
}

// launcherUnder finds the launcher of one installed build, and its file info.
//
// The direct path is what an install produces. The second shape is looked for as
// well because an archive whose own root directory could not be unwrapped — two
// top-level entries instead of one — leaves the launcher a level deeper, and a
// build that lists its models but cannot be started would be a poor trade for
// tidiness.
func (i *Installer) launcherUnder(buildDir string) (string, os.FileInfo) {
	if info, err := os.Stat(filepath.Join(buildDir, launcherName())); err == nil && !info.IsDir() {
		return filepath.Join(buildDir, launcherName()), info
	}
	entries, err := os.ReadDir(buildDir)
	if err != nil {
		return "", nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(buildDir, e.Name(), launcherName())
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, info
		}
	}
	return "", nil
}

type releaseAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	URL    string `json:"browser_download_url"`
}

type release struct {
	Tag    string         `json:"tag_name"`
	Assets []releaseAsset `json:"assets"`
}

// Latest returns the newest release that publishes an archive for this platform.
//
// GitHub's "latest" release cannot be used for this. Since llama.cpp moved to
// versioned tags, `latest` points at a semver release (v0.6.0 when this was
// written) that carries no binaries at all — only a nightly-tag.txt naming a
// build — while the archives live on the per-build releases (`b11528`). So the
// list is scanned in the order GitHub returns it, newest first, for the first
// release that actually has our asset.
func (i *Installer) Latest(ctx context.Context, goos, goarch, accel string) (Asset, error) {
	url := i.APIBase + "?per_page=10"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Asset{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := i.Client.Do(req)
	if err != nil {
		return Asset{}, fmt.Errorf("llama.cpp-Releases abfragen: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The API answers 403 once the unauthenticated rate limit is exhausted,
		// and says so in a header. Naming it saves the user from reading a
		// generic failure as "llama.cpp is unreachable".
		if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return Asset{}, errors.New("GitHub drosselt die Anfragen (Stundenlimit erreicht) — später erneut versuchen oder einen eigenen Build-Pfad angeben")
		}
		return Asset{}, fmt.Errorf("llama.cpp-Releases abfragen: HTTP %d", resp.StatusCode)
	}

	var releases []release
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return Asset{}, fmt.Errorf("Release-Liste lesen: %w", err)
	}
	for _, r := range releases {
		want, err := AssetName(r.Tag, goos, goarch, accel)
		if err != nil {
			return Asset{}, err
		}
		for _, a := range r.Assets {
			if a.Name == want {
				return Asset{Tag: r.Tag, Name: a.Name, URL: a.URL, Size: a.Size, Digest: a.Digest}, nil
			}
		}
	}
	return Asset{}, fmt.Errorf("kein llama.cpp-Build für %s/%s (%s) in den letzten %d Releases gefunden",
		goos, goarch, accel, len(releases))
}

// Install downloads and unpacks one asset, and returns the launcher to run.
//
// The archive is verified before it is touched: GitHub reports a sha256 per asset
// and this refuses a download that does not match it. Extraction happens into a
// temporary directory beside the final one and is renamed into place only when it
// is complete, so an interrupted install leaves nothing that looks installed.
func (i *Installer) Install(ctx context.Context, asset Asset) (string, error) {
	if asset.Tag == "" || asset.URL == "" {
		return "", errors.New("kein Build zum Installieren")
	}
	if err := os.MkdirAll(i.Dir, 0o755); err != nil {
		return "", fmt.Errorf("Laufzeitverzeichnis anlegen: %w", err)
	}

	format, err := archiveFormatFor(asset.Name)
	if err != nil {
		return "", err
	}
	archive, err := i.download(ctx, asset)
	if err != nil {
		return "", err
	}
	defer os.Remove(archive)

	final := filepath.Join(i.Dir, asset.Tag)
	staging := final + ".installing"
	if err := os.RemoveAll(staging); err != nil {
		return "", err
	}
	i.report(Progress{Phase: "extracting"})
	if err := extract(archive, format, staging); err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("Archiv entpacken: %w", err)
	}
	if err := flattenSingleRoot(staging); err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("Archivwurzel auflösen: %w", err)
	}
	// A previous install of the same build is replaced wholesale: mixing files
	// from two archives is how a stale library gets loaded.
	if err := os.RemoveAll(final); err != nil {
		return "", err
	}
	if err := os.Rename(staging, final); err != nil {
		os.RemoveAll(staging)
		return "", fmt.Errorf("Build an seinen Platz verschieben: %w", err)
	}

	binary := i.BinaryPath(asset.Tag)
	if err := os.Chmod(binary, 0o755); err != nil {
		return "", fmt.Errorf("llama-server ausführbar machen: %w", err)
	}
	i.report(Progress{Phase: "done"})
	return binary, nil
}

// download fetches the archive into the runtime directory and verifies its
// digest, returning the file it landed in.
func (i *Installer) download(ctx context.Context, asset Asset) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := i.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Build herunterladen: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Build herunterladen: HTTP %d", resp.StatusCode)
	}

	total := asset.Size
	if total == 0 {
		total = resp.ContentLength
	}
	path := filepath.Join(i.Dir, asset.Name+".part")
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("Download-Datei anlegen: %w", err)
	}

	sum := sha256.New()
	written, err := copyWithProgress(io.MultiWriter(f, sum), resp.Body, total, func(done int64) {
		i.report(Progress{Phase: "downloading", Done: done, Total: total})
	})
	closeErr := f.Close()
	if err != nil {
		os.Remove(path)
		return "", fmt.Errorf("Build herunterladen: %w", err)
	}
	if closeErr != nil {
		os.Remove(path)
		return "", closeErr
	}
	if total > 0 && written != total {
		// A truncated download that still ends cleanly would otherwise unpack
		// into a broken install.
		os.Remove(path)
		return "", fmt.Errorf("Download unvollständig: %d von %d Bytes", written, total)
	}

	if want := normaliseDigest(asset.Digest); want != "" {
		got := hex.EncodeToString(sum.Sum(nil))
		if !strings.EqualFold(got, want) {
			os.Remove(path)
			return "", fmt.Errorf("Prüfsumme stimmt nicht: erwartet %s, erhalten %s", want, got)
		}
	}
	return path, nil
}

// normaliseDigest turns the API's "sha256:<hex>" into the bare hex it compares
// against, and answers empty for anything else: an algorithm this code does not
// compute must not be treated as a match.
func normaliseDigest(digest string) string {
	const prefix = "sha256:"
	if !strings.HasPrefix(strings.ToLower(digest), prefix) {
		return ""
	}
	return strings.TrimSpace(digest[len(prefix):])
}

// copyWithProgress copies while reporting, so a 500 MB download has a visible
// progress rather than a frozen window. Reporting is throttled to whole
// percentages: a callback per 32 KB chunk would spend more time in the UI than in
// the copy.
func copyWithProgress(dst io.Writer, src io.Reader, total int64, report func(int64)) (int64, error) {
	buf := make([]byte, 64<<10)
	var written, lastPercent int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return written, werr
			}
			written += int64(n)
			if total > 0 {
				if p := written * 100 / total; p != lastPercent {
					lastPercent = p
					report(written)
				}
			}
		}
		if err == io.EOF {
			report(written)
			return written, nil
		}
		if err != nil {
			return written, err
		}
	}
}

func (i *Installer) report(p Progress) {
	if i.OnProgress != nil {
		i.OnProgress(p)
	}
}

// archiveFormat names the container an archive is in.
type archiveFormat int

const (
	formatZip archiveFormat = iota + 1
	formatTarGz
)

// archiveFormatFor picks the format from the asset's published name. Windows
// builds ship .zip and every desktop Unix build ships .tar.gz, so the name decides
// rather than the host.
//
// It is decided from the published name and not from the file on disk, because the
// file on disk is the download in progress and is deliberately called something
// else until it has been verified: deriving the format from that name made every
// install fail with "unknown archive format: …tar.gz.part".
func archiveFormatFor(name string) (archiveFormat, error) {
	switch {
	case strings.HasSuffix(name, ".zip"):
		return formatZip, nil
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		return formatTarGz, nil
	default:
		return 0, fmt.Errorf("unbekanntes Archivformat: %s", filepath.Base(name))
	}
}

// flattenSingleRoot lifts away the directory an archive wraps its contents in.
//
// Every published archive puts everything inside a directory named after the build
// (`llama-b11528/`), so an install that kept it would place the launcher at
// `<tag>/llama-b11528/llama-server` — one level below every path this package
// builds, including the one the launcher is made executable at. Unwrapping it here,
// once, is what keeps BinaryPath and Find honest. An archive shaped differently is
// left alone, and Find copes with that case by looking a level deeper.
func flattenSingleRoot(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return nil
	}
	inner := filepath.Join(dir, entries[0].Name())
	moved := dir + ".root"
	if err := os.Rename(inner, moved); err != nil {
		return err
	}
	// The outer directory is empty now, so removing it cannot lose anything.
	if err := os.Remove(dir); err != nil {
		return err
	}
	return os.Rename(moved, dir)
}

// extract unpacks an archive into dst.
func extract(archive string, format archiveFormat, dst string) error {
	switch format {
	case formatZip:
		return extractZip(archive, dst)
	case formatTarGz:
		return extractTarGz(archive, dst)
	default:
		return fmt.Errorf("unbekanntes Archivformat: %s", filepath.Base(archive))
	}
}

func extractTarGz(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		path, err := entryPath(dst, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// The published archives rely on these: `libllama-common.so.0` is a
			// symlink to the versioned file, and the launcher loads it by the
			// short name. A link pointing outside the install is dropped rather
			// than created.
			if err := symlink(hdr.Linkname, path, dst); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > maxExtractBytes {
				return fmt.Errorf("Archiv zu groß: mehr als %d Bytes", int64(maxExtractBytes))
			}
			if err := writeFile(path, tr, hdr.Size, fileMode(hdr.Mode)); err != nil {
				return err
			}
		}
	}
}

func extractZip(archive, dst string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()

	var total int64
	for _, f := range zr.File {
		path, err := entryPath(dst, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		total += int64(f.UncompressedSize64)
		if total > maxExtractBytes {
			rc.Close()
			return fmt.Errorf("Archiv zu groß: mehr als %d Bytes", int64(maxExtractBytes))
		}
		// A zip carries no executable bit worth trusting, but a launcher that is
		// not executable on Unix cannot start; the install chmods it anyway.
		err = writeFile(path, rc, int64(f.UncompressedSize64), 0o755)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// symlink creates a link inside root, refusing a target that points outside it.
func symlink(target, path, root string) error {
	if filepath.IsAbs(target) {
		return nil
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(target)))
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_ = os.Remove(path)
	return os.Symlink(target, path)
}

// writeFile streams one entry into place.
func writeFile(path string, r io.Reader, size int64, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(r, size))
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func fileMode(mode int64) os.FileMode {
	m := os.FileMode(mode).Perm()
	if m == 0 {
		return 0o644
	}
	return m
}

// entryPath resolves an archive entry inside root, refusing anything that would
// land outside it. Every unpacker needs this: an entry named `../../.bashrc` or
// an absolute path is otherwise written wherever it likes.
func entryPath(root, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	clean = strings.TrimPrefix(clean, string(filepath.Separator))
	if clean == "." || clean == "" {
		return "", fmt.Errorf("Archiv-Eintrag ohne Namen")
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("Archiv-Eintrag verlässt das Ziel: %s", name)
	}
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("Archiv-Eintrag mit absolutem Pfad: %s", name)
	}
	return filepath.Join(root, clean), nil
}
