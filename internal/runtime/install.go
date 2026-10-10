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
	// AcceleratorROCm is AMD's, through HIP.
	AcceleratorROCm = "rocm"
	// AcceleratorCUDA is NVIDIA's. The release ships it per CUDA suite, and the
	// newest suite that matches wins, so a user asking for "cuda" does not have to
	// know which one their driver speaks.
	AcceleratorCUDA = "cuda"
	// AcceleratorSYCL is Intel's, through oneAPI.
	AcceleratorSYCL = "sycl"
	// AcceleratorOpenVINO is Intel's other one, for CPU and integrated GPU.
	AcceleratorOpenVINO = "openvino"
	// AcceleratorOpenCL is the Adreno build, published for Windows on arm64.
	AcceleratorOpenCL = "opencl"

	// userAgent identifies the client to GitHub, which refuses requests without
	// one.
	userAgent = "kichat-llama-runtime"
)

// Accelerators lists the build families in the order a picker should offer them:
// the one that needs nothing first, then the portable GPU one, then the
// vendor-specific ones.
func Accelerators() []string {
	return []string{AcceleratorCPU, AcceleratorVulkan, AcceleratorROCm, AcceleratorCUDA, AcceleratorSYCL, AcceleratorOpenVINO, AcceleratorOpenCL}
}

// Asset is one downloadable archive.
type Asset struct {
	Tag    string
	Name   string
	URL    string
	Size   int64
	Digest string // "sha256:<hex>", empty when the API does not report one.
	// Variant is the build family: cpu, vulkan, rocm, cuda, sycl, openvino, opencl.
	Variant string
	// Suite is the version the release carries for that family (`13.4` for CUDA,
	// `10.0` for ROCm, `fp16` for SYCL), empty when the name pins nothing. It is
	// kept because it is what tells two installations of one build apart.
	Suite string
	// Companion is the runtime bundle that has to be unpacked into the same
	// directory, when the family publishes one. CUDA does: its build archive holds
	// no CUDA library at all and its backend links libcudart/libcublas, which ship
	// as a separate `cudart-llama-…` asset. Nil for the families whose runtime is
	// the system's (ROCm, Vulkan, SYCL, OpenVINO).
	Companion *Asset
}

// Dir is the folder this build installs into.
//
// The family is part of the name, not just the build number: two families of one
// release are two different installations, and folding them into one directory
// would delete the CPU build the moment a user tried the ROCm one — and download
// 600 MB again to go back.
func (a Asset) Dir() string {
	switch {
	case a.Variant == "":
		return a.Tag
	case a.Suite == "":
		return a.Tag + "-" + a.Variant
	default:
		return a.Tag + "-" + a.Variant + "-" + a.Suite
	}
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
	// SuiteCheck decides whether a build family and suite can run on this machine,
	// and why not when it cannot. It is a field so a test can answer for a machine
	// that is not this one; nil means the real check (SuiteUsable).
	SuiteCheck func(variant, suite string) (bool, string)
}

// NewInstaller builds an installer rooted at dir.
func NewInstaller(dir string) *Installer {
	return &Installer{
		Dir:     dir,
		APIBase: ReleasesAPI,
		Client:  &http.Client{Timeout: 30 * time.Minute},
	}
}

// variant is one build family llama.cpp publishes for a platform.
type variant struct {
	// ID is the name the setting stores and the picker offers.
	ID string
	// platform is the middle of the published archive name. A `*` stands where the
	// release carries a suite version (`cuda-13.4`, `rocm-10.0`, `sycl-fp16`),
	// because pinning one here would break the day the suite moves on — the app
	// asks for the family and takes the newest the release actually publishes.
	platform string
	// ext follows the asset rather than the host: every desktop Unix build is
	// .tar.gz and Windows is .zip, including when KiChat runs on Linux.
	ext string
	// runtime is the template of the companion archive that carries the libraries
	// this build does not bundle, with {tag} and {platform} filled in. Empty when
	// the family's runtime comes from the system.
	//
	// Measured off b11538: the Linux CUDA builds come with
	// `cudart-llama-b11538-bin-ubuntu-cuda-13.4-x64.tar.gz` (440 MB for 13.4,
	// 594 MB for 12.8), and the Windows ones with
	// `cudart-llama-bin-win-cuda-13.4-x64.zip` — the tag is in the Linux name and
	// not in the Windows one, which is why this is a template per row rather than
	// one name built from the platform.
	runtime string
}

// variantsFor is the measured table of what llama.cpp publishes, read off release
// b11535 (36 assets). It is per platform and per architecture because the
// releases are: ROCm, SYCL and OpenVINO are x64 only, the CUDA arm64 build exists
// where Vulkan's ARM one does, and macOS publishes a single Metal-enabled build
// per architecture with no accelerator to choose.
func variantsFor(goos, goarch string) ([]variant, error) {
	switch goos {
	case "linux":
		switch goarch {
		case "amd64":
			return []variant{
				{AcceleratorCPU, "ubuntu-x64", ".tar.gz", ""},
				{AcceleratorVulkan, "ubuntu-vulkan-x64", ".tar.gz", ""},
				{AcceleratorROCm, "ubuntu-rocm-*-x64", ".tar.gz", ""},
				{AcceleratorCUDA, "ubuntu-cuda-*-x64", ".tar.gz", "cudart-llama-{tag}-bin-{platform}"},
				{AcceleratorSYCL, "ubuntu-sycl-*-x64", ".tar.gz", ""},
				{AcceleratorOpenVINO, "ubuntu-openvino-*-x64", ".tar.gz", ""},
			}, nil
		case "arm64":
			return []variant{
				{AcceleratorCPU, "ubuntu-arm64", ".tar.gz", ""},
				{AcceleratorVulkan, "ubuntu-vulkan-arm64", ".tar.gz", ""},
				{AcceleratorCUDA, "ubuntu-cuda-*-arm64", ".tar.gz", "cudart-llama-{tag}-bin-{platform}"},
			}, nil
		}
		return nil, fmt.Errorf("für Linux auf %s wird llama.cpp nicht veröffentlicht", goarch)
	case "darwin":
		switch goarch {
		case "arm64":
			return []variant{{AcceleratorCPU, "macos-arm64", ".tar.gz", ""}}, nil
		case "amd64":
			return []variant{{AcceleratorCPU, "macos-x64", ".tar.gz", ""}}, nil
		}
		return nil, fmt.Errorf("macOS auf %s wird nicht veröffentlicht", goarch)
	case "windows":
		switch goarch {
		case "amd64":
			return []variant{
				{AcceleratorCPU, "win-cpu-x64", ".zip", ""},
				{AcceleratorVulkan, "win-vulkan-x64", ".zip", ""},
				{AcceleratorROCm, "win-rocm-*-x64", ".zip", ""},
				{AcceleratorCUDA, "win-cuda-*-x64", ".zip", "cudart-llama-bin-{platform}"},
				{AcceleratorSYCL, "win-sycl-x64", ".zip", ""},
				{AcceleratorOpenVINO, "win-openvino-*-x64", ".zip", ""},
			}, nil
		case "arm64":
			return []variant{
				{AcceleratorCPU, "win-cpu-arm64", ".zip", ""},
				{AcceleratorVulkan, "win-vulkan-arm64", ".zip", ""},
				{AcceleratorCUDA, "win-cuda-*-arm64", ".zip", "cudart-llama-bin-{platform}"},
				{AcceleratorOpenCL, "win-opencl-adreno-arm64", ".zip", ""},
			}, nil
		}
		return nil, fmt.Errorf("für Windows auf %s wird llama.cpp nicht veröffentlicht", goarch)
	}
	return nil, fmt.Errorf("für %s wird llama.cpp nicht veröffentlicht", goos)
}

// variantFor picks one family out of the table, refusing what this platform does
// not publish rather than substituting a build the user did not ask for.
func variantFor(goos, goarch, accel string) (variant, error) {
	if accel == "" {
		accel = AcceleratorCPU
	}
	variants, err := variantsFor(goos, goarch)
	if err != nil {
		return variant{}, err
	}
	for _, v := range variants {
		if v.ID == accel {
			return v, nil
		}
	}
	return variant{}, fmt.Errorf("für %s/%s gibt es keinen %s-Build", goos, goarch, accel)
}

// PublishedAccelerators lists the families this platform can actually fetch, in
// the order the picker offers them, so the panel cannot offer a build that does
// not exist.
//
// A platform with none answers with an empty list rather than nil: this feeds a
// JSON array, and a nil slice marshals to `null`.
func PublishedAccelerators(goos, goarch string) []string {
	variants, err := variantsFor(goos, goarch)
	if err != nil {
		return []string{}
	}
	ids := make([]string, 0, len(variants))
	for _, v := range variants {
		ids = append(ids, v.ID)
	}
	return ids
}

// AssetName is the archive name the app asks for, or an error naming what is not
// covered. A `*` stands for the suite version a release carries (see variant).
func AssetName(tag, goos, goarch, accel string) (string, error) {
	v, err := variantFor(goos, goarch, accel)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("llama-%s-bin-%s%s", tag, v.platform, v.ext), nil
}

// matchAssetName reports whether a published name is this variant, and returns the
// segment the pattern left open — the suite version — or "" for an exact name.
func matchAssetName(pattern, name string) (string, bool) {
	p := strings.Split(pattern, "-")
	n := strings.Split(name, "-")
	if len(p) != len(n) {
		return "", false
	}
	open := ""
	for i := range p {
		if p[i] == "*" {
			open = n[i]
			continue
		}
		if p[i] != n[i] {
			return "", false
		}
	}
	return open, true
}

// preferSuite reports whether a is a better answer than the current best for one
// variant. A release carries several suites of the same family (cuda-12.8 and
// cuda-13.4), and the newest is the one a user asking for "cuda" means. A segment
// that is not a version — SYCL's fp16/fp32 — leaves the API's own order alone,
// which is alphabetical and therefore stable.
func preferSuite(candidate, best string) bool {
	if best == "" {
		return true
	}
	c, cok := dottedVersion(candidate)
	b, bok := dottedVersion(best)
	if !cok || !bok {
		return false
	}
	for i := 0; i < len(c) && i < len(b); i++ {
		if c[i] != b[i] {
			return c[i] > b[i]
		}
	}
	return len(c) > len(b)
}

// dottedVersion reads a version-like segment (`13.4`) into numbers. Anything else
// is not a version, which the caller has to be able to tell apart from zero.
func dottedVersion(s string) ([]int, bool) {
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				return nil, false
			}
			n = n*10 + int(r-'0')
		}
		out = append(out, n)
	}
	return out, true
}

// BinaryPath is where the launcher of one installed build lives — the directory
// name an Asset.Dir() produced. Windows keeps the .exe suffix; nothing else does.
func (i *Installer) BinaryPath(dir string) string {
	return filepath.Join(i.Dir, dir, launcherName())
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

// Launcher reports the llama-server of one named build directory, whether or not
// it is the newest one there. An empty or unknown name answers false, so a caller
// that has a recorded build in mind can fall back to Find when it is gone.
//
// BinaryPath is enough here because an install flattens the archive's wrapper
// directory; a build left a level deeper by an unusual archive is still reachable
// through Find.
func (i *Installer) Launcher(dir string) (string, bool) {
	if strings.TrimSpace(dir) == "" {
		return "", false
	}
	path := i.BinaryPath(dir)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path, true
	}
	return "", false
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
	chosen, err := variantFor(goos, goarch, accel)
	if err != nil {
		return Asset{}, err
	}
	for _, r := range releases {
		want := fmt.Sprintf("llama-%s-bin-%s%s", r.Tag, chosen.platform, chosen.ext)
		best := -1
		suite := ""
		refused := ""
		for index, a := range r.Assets {
			got, ok := matchAssetName(want, a.Name)
			if !ok {
				continue
			}
			// A suite this machine cannot run is not a candidate, however new it
			// is: llama.cpp reports no device for a mismatched driver or runtime
			// and no error either, so the model would quietly run on the CPU.
			if usable, why := i.suiteCheck(chosen.ID, got); !usable {
				refused = why
				continue
			}
			if best < 0 || preferSuite(got, suite) {
				best, suite = index, got
			}
		}
		if best < 0 {
			if refused != "" {
				return Asset{}, fmt.Errorf("kein %s-Build für diese Maschine möglich — %s", chosen.ID, refused)
			}
			continue
		}
		a := r.Assets[best]
		asset := Asset{
			Tag: r.Tag, Name: a.Name, URL: a.URL, Size: a.Size, Digest: a.Digest,
			Variant: chosen.ID, Suite: suite,
		}
		asset.Companion = i.companionFor(r, chosen, suite)
		return asset, nil
	}
	return Asset{}, fmt.Errorf("kein llama.cpp-Build für %s/%s (%s) in den letzten %d Releases gefunden",
		goos, goarch, accel, len(releases))
}

// suiteCheck asks the real machine whether a family and suite can run here, unless
// a test has answered for it.
func (i *Installer) suiteCheck(variant, suite string) (bool, string) {
	if i.SuiteCheck != nil {
		return i.SuiteCheck(variant, suite)
	}
	return SuiteUsable(variant, suite)
}

// companionFor finds the runtime bundle that belongs to a build, when the family
// publishes one and this machine does not already provide those libraries.
//
// It is looked up by the name the release itself uses rather than by pattern, so a
// build and its runtime cannot end up from different suites: the CUDA 13 backend
// links libcudart.so.13, which only the 13.4 bundle carries.
func (i *Installer) companionFor(r release, chosen variant, suite string) *Asset {
	if chosen.runtime == "" || suite == "" {
		return nil
	}
	if chosen.ID == AcceleratorCUDA && cudaRuntimePresent(suite) {
		// A CUDA toolkit on this machine already answers for these sonames, and the
		// bundle is hundreds of megabytes.
		return nil
	}
	name := companionName(chosen.runtime, r.Tag, chosen.platform, suite) + chosen.ext
	for _, a := range r.Assets {
		if a.Name == name {
			return &Asset{
				Tag: r.Tag, Name: a.Name, URL: a.URL, Size: a.Size, Digest: a.Digest,
				Variant: chosen.ID, Suite: suite,
			}
		}
	}
	// A release without a bundle is used as it is: the machine may have its own
	// CUDA toolkit, and when it does not, the panel names the libraries that are
	// missing instead of the install failing late.
	return nil
}

// companionName fills the open suite segment into a platform pattern and the tag
// into a name template, which is what addresses a companion archive by its
// published name. The tag is in the Linux CUDA bundle's name and not in the
// Windows one, hence the template per platform row.
func companionName(template, tag, platform, suite string) string {
	concrete := strings.Replace(platform, "*", suite, 1)
	return strings.NewReplacer("{tag}", tag, "{platform}", concrete).Replace(template)
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
	// Both archives are reported against one total when the release named their
	// sizes, so a CUDA install — 153 MB of build plus 440 MB of runtime — shows one
	// bar that moves instead of two that each start over. A size the API did not
	// give leaves the total at zero, which each download then answers on its own:
	// a bar that reports one archive at a time rather than a percentage above 100.
	total := int64(0)
	if asset.Size > 0 {
		total = asset.Size
		if asset.Companion != nil && asset.Companion.Size > 0 {
			total += asset.Companion.Size
		}
	}
	archive, written, err := i.download(ctx, asset, 0, total)
	if err != nil {
		return "", err
	}
	defer os.Remove(archive)

	var companionArchive string
	if asset.Companion != nil {
		// The bundle's progress starts where the build's ended. The offset is the
		// measured size of the first download rather than the API's figure, so a
		// release that reported no size still cannot make the bar jump backwards;
		// and with no combined total the two are reported one after the other
		// instead, which is why the offset is dropped in that case.
		base := int64(0)
		if total > 0 {
			base = written
		}
		path, _, err := i.download(ctx, *asset.Companion, base, total)
		if err != nil {
			return "", err
		}
		companionArchive = path
		defer os.Remove(path)
	}

	final := filepath.Join(i.Dir, asset.Dir())
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
	if companionArchive != "" {
		if err := i.addRuntimeBundle(companionArchive, format, staging); err != nil {
			os.RemoveAll(staging)
			return "", fmt.Errorf("CUDA-Laufzeit entpacken: %w", err)
		}
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

	binary := i.BinaryPath(asset.Dir())
	if err := os.Chmod(binary, 0o755); err != nil {
		return "", fmt.Errorf("llama-server ausführbar machen: %w", err)
	}
	i.report(Progress{Phase: "done"})
	return binary, nil
}

// addRuntimeBundle unpacks a companion archive into the build directory itself.
//
// The libraries have to land *beside* the launcher rather than in a subdirectory of
// their own: the archives are built with rpath `$ORIGIN`, so that directory is the
// only place the loader looks without an environment variable to help it.
func (i *Installer) addRuntimeBundle(archive string, format archiveFormat, buildDir string) error {
	sub := buildDir + ".runtime"
	if err := os.RemoveAll(sub); err != nil {
		return err
	}
	defer os.RemoveAll(sub)
	if err := extract(archive, format, sub); err != nil {
		return err
	}
	if err := flattenSingleRoot(sub); err != nil {
		return err
	}
	return mergeInto(sub, buildDir)
}

// mergeInto moves the contents of one unpacked directory into another, replacing
// files that are already there. Directories are merged rather than replaced, so a
// bundle that nests its libraries keeps its layout.
func mergeInto(from, into string) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		src := filepath.Join(from, entry.Name())
		dst := filepath.Join(into, entry.Name())
		if !entry.IsDir() {
			_ = os.Remove(dst)
			if err := os.Rename(src, dst); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := mergeInto(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// download fetches the archive into the runtime directory and verifies its
// digest, returning the file it landed in and how much was written. Its progress is
// reported against the whole install: base is what was already downloaded, total the
// size of everything that install will fetch.
func (i *Installer) download(ctx context.Context, asset Asset, base, total int64) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := i.Client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("Build herunterladen: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("Build herunterladen: HTTP %d", resp.StatusCode)
	}

	// expected is this archive's own size, which is what the completeness check
	// below compares against. total is the whole install's, which is what the
	// progress bar shows while two archives are being fetched.
	expected := asset.Size
	if expected == 0 {
		expected = resp.ContentLength
	}
	if total <= 0 {
		total = expected
	}
	path := filepath.Join(i.Dir, asset.Name+".part")
	f, err := os.Create(path)
	if err != nil {
		return "", 0, fmt.Errorf("Download-Datei anlegen: %w", err)
	}

	sum := sha256.New()
	written, err := copyWithProgress(io.MultiWriter(f, sum), resp.Body, total, func(done int64) {
		i.report(Progress{Phase: "downloading", Done: base + done, Total: total})
	})
	closeErr := f.Close()
	if err != nil {
		os.Remove(path)
		return "", 0, fmt.Errorf("Build herunterladen: %w", err)
	}
	if closeErr != nil {
		os.Remove(path)
		return "", 0, closeErr
	}
	if expected > 0 && written != expected {
		// A truncated download that still ends cleanly would otherwise unpack
		// into a broken install.
		os.Remove(path)
		return "", 0, fmt.Errorf("Download unvollständig: %d von %d Bytes", written, expected)
	}

	if want := normaliseDigest(asset.Digest); want != "" {
		got := hex.EncodeToString(sum.Sum(nil))
		if !strings.EqualFold(got, want) {
			os.Remove(path)
			return "", 0, fmt.Errorf("Prüfsumme stimmt nicht: erwartet %s, erhalten %s", want, got)
		}
	}
	return path, written, nil
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
