package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The four names below are the published ones for b11538: two CUDA suites for Linux
// x64, each with the runtime bundle that carries the libraries the build archive
// itself does not contain.
const cudaReleaseJSON = `[
  {"tag_name":"b11538","assets":[
    {"name":"llama-b11538-bin-ubuntu-cuda-12.8-x64.tar.gz","size":172000000,
     "browser_download_url":"https://example.invalid/cuda128.tar.gz"},
    {"name":"llama-b11538-bin-ubuntu-cuda-13.4-x64.tar.gz","size":152800000,
     "browser_download_url":"https://example.invalid/cuda134.tar.gz"},
    {"name":"cudart-llama-b11538-bin-ubuntu-cuda-12.8-x64.tar.gz","size":594400000,
     "browser_download_url":"https://example.invalid/rt128.tar.gz"},
    {"name":"cudart-llama-b11538-bin-ubuntu-cuda-13.4-x64.tar.gz","size":440200000,
     "browser_download_url":"https://example.invalid/rt134.tar.gz"}
  ]}
]`

// suiteCheckStub answers for a machine that is not this one.
func suiteCheckStub(verdicts map[string]struct {
	usable bool
	why    string
}) func(string, string) (bool, string) {
	return func(variant, suite string) (bool, string) {
		if v, ok := verdicts[suite]; ok {
			return v.usable, v.why
		}
		return true, ""
	}
}

// TestLatestPicksASuiteThisMachineCanRun is the driver-compatibility fix: both
// CUDA suites are published, the newest is not always the one the machine's driver
// can initialise, and llama.cpp reports no device — and no error — when it cannot.
func TestLatestPicksASuiteThisMachineCanRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, cudaReleaseJSON)
	}))
	defer srv.Close()

	inst := &Installer{
		Dir: t.TempDir(), APIBase: srv.URL, Client: srv.Client(),
		SuiteCheck: suiteCheckStub(map[string]struct {
			usable bool
			why    string
		}{
			"13.4": {false, "der NVIDIA-Treiber ist 550, CUDA 13 braucht mindestens 580"},
			"12.8": {true, ""},
		}),
	}
	asset, err := inst.Latest(context.Background(), LlamaCPP, "linux", "amd64", AcceleratorCUDA)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if asset.Name != "llama-b11538-bin-ubuntu-cuda-12.8-x64.tar.gz" {
		t.Errorf("asset = %q, want the 12.8 build the driver can run", asset.Name)
	}
	if asset.Suite != "12.8" || asset.Dir() != "b11538-cuda-12.8" {
		t.Errorf("suite = %q, dir = %q; want 12.8 and b11538-cuda-12.8", asset.Suite, asset.Dir())
	}
	// The runtime bundle has to be the one of the same suite: the 13.4 backend
	// links libcudart.so.13, which the 12.8 bundle does not carry.
	if asset.Companion == nil {
		t.Fatal("no runtime bundle was found for a CUDA build")
	}
	if asset.Companion.Name != "cudart-llama-b11538-bin-ubuntu-cuda-12.8-x64.tar.gz" {
		t.Errorf("companion = %q, want the 12.8 runtime bundle", asset.Companion.Name)
	}
}

// A release whose suites all fail the check is refused instead of downloaded: 153 MB
// of a backend that cannot load, followed by a model that silently runs on the CPU,
// is the outcome this avoids.
func TestLatestRefusesAFamilyWithNoUsableSuite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, cudaReleaseJSON)
	}))
	defer srv.Close()

	const reason = "der NVIDIA-Treiber ist 470, CUDA 12 braucht mindestens 525"
	inst := &Installer{
		Dir: t.TempDir(), APIBase: srv.URL, Client: srv.Client(),
		SuiteCheck: suiteCheckStub(map[string]struct {
			usable bool
			why    string
		}{
			"13.4": {false, reason},
			"12.8": {false, reason},
		}),
	}
	_, err := inst.Latest(context.Background(), LlamaCPP, "linux", "amd64", AcceleratorCUDA)
	if err == nil {
		t.Fatal("a CUDA install was offered on a machine whose driver supports neither suite")
	}
	for _, want := range []string{"cuda", reason} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

// The bundle is left out when the machine's own CUDA runtime already answers for the
// sonames this build wants — and only then. Asserted against this machine instead of
// a fixed verdict, so the test says what it means on any machine.
func TestLatestFindsTheRuntimeBundleUnlessTheMachineHasOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, cudaReleaseJSON)
	}))
	defer srv.Close()

	inst := &Installer{
		Dir: t.TempDir(), APIBase: srv.URL, Client: srv.Client(),
		SuiteCheck: suiteCheckStub(map[string]struct {
			usable bool
			why    string
		}{"13.4": {true, ""}}),
	}
	asset, err := inst.Latest(context.Background(), LlamaCPP, "linux", "amd64", AcceleratorCUDA)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if cudaRuntimePresent("13.4") {
		if asset.Companion != nil {
			t.Errorf("companion = %q, want none: this machine's own CUDA runtime answers for it", asset.Companion.Name)
		}
		return
	}
	if asset.Companion == nil {
		t.Error("no runtime bundle was fetched on a machine without its own CUDA runtime")
	}
}

// TestInstallUnpacksTheRuntimeBundleBesideTheLauncher is the layout the loader
// demands: the archives carry rpath `$ORIGIN`, so libcudart/libcublas have to land in
// the build directory itself rather than in a subdirectory of it.
func TestInstallUnpacksTheRuntimeBundleBesideTheLauncher(t *testing.T) {
	build := readFile(t, tarGz(t, map[string]string{
		"llama-b1/llama-server":            "#!/bin/sh\ntrue\n",
		"llama-b1/libllama-server-impl.so": "impl",
		"llama-b1/libggml-cuda.so":         "cuda",
	}, nil))
	// Shaped like the real bundle: one wrapper directory, the runtime at its top
	// and a nested directory beside it, so the merge has to keep the layout.
	bundle := readFile(t, tarGz(t, map[string]string{
		"cudart-libs/libcudart.so.13":       "cudart",
		"cudart-libs/lib/libcublas.so.13":   "cublas",
		"cudart-libs/lib/libcublasLt.so.13": "cublaslt",
	}, nil))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "rt") {
			w.Write(bundle)
			return
		}
		w.Write(build)
	}))
	defer srv.Close()

	dir := t.TempDir()
	progress := []Progress{}
	inst := &Installer{
		Dir: dir, APIBase: srv.URL, Client: srv.Client(),
		OnProgress: func(p Progress) { progress = append(progress, p) },
	}
	sum := sha256.Sum256(build)
	runtimeSum := sha256.Sum256(bundle)
	asset := Asset{
		Tag: "b1", Name: "llama-b1-bin-ubuntu-cuda-13.4-x64.tar.gz", URL: srv.URL + "/build.tar.gz",
		Size: int64(len(build)), Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Variant: AcceleratorCUDA, Suite: "13.4",
		Companion: &Asset{
			Tag: "b1", Name: "cudart-llama-b1-bin-ubuntu-cuda-13.4-x64.tar.gz", URL: srv.URL + "/rt.tar.gz",
			Size: int64(len(bundle)), Digest: "sha256:" + hex.EncodeToString(runtimeSum[:]),
			Variant: AcceleratorCUDA, Suite: "13.4",
		},
	}
	binary, err := inst.Install(context.Background(), asset)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	buildDir := filepath.Join(dir, "b1-cuda-13.4")
	for _, want := range []string{
		"llama-server", "libllama-server-impl.so", "libggml-cuda.so",
		"libcudart.so.13", filepath.Join("lib", "libcublas.so.13"), filepath.Join("lib", "libcublasLt.so.13"),
	} {
		if _, err := os.Stat(filepath.Join(buildDir, want)); err != nil {
			t.Errorf("%s was not unpacked beside the launcher: %v", want, err)
		}
	}
	if binary != filepath.Join(buildDir, "llama-server") {
		t.Errorf("binary = %q, want the launcher in the build directory", binary)
	}
	// Nothing of either archive is left behind: they are hundreds of megabytes and
	// nothing reads them again.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "b1-cuda-13.4" {
			t.Errorf("leftover in the runtime directory: %s", entry.Name())
		}
	}
	// The progress has one total for the whole install, so the bar of a 153 MB
	// build plus a 440 MB runtime does not jump back to nothing in the middle.
	var sawDownload bool
	var lastDone int64
	for _, p := range progress {
		if p.Phase == "downloading" {
			sawDownload = true
			if p.Total != asset.Size+asset.Companion.Size {
				t.Fatalf("progress total = %d, want the size of both archives", p.Total)
			}
			if p.Done < lastDone {
				t.Fatalf("progress went backwards: %d after %d", p.Done, lastDone)
			}
			lastDone = p.Done
		}
	}
	if !sawDownload {
		t.Error("no download progress was reported")
	}
	if lastDone != asset.Size+asset.Companion.Size {
		t.Errorf("final progress = %d of %d, want the whole install", lastDone, asset.Size+asset.Companion.Size)
	}
}

// A corrupt runtime bundle must not produce a half-installed build: the digest is
// checked before anything is unpacked, and nothing is left in the runtime directory.
func TestInstallRefusesACorruptRuntimeBundle(t *testing.T) {
	build := readFile(t, tarGz(t, map[string]string{"llama-b1/llama-server": "#!/bin/sh\ntrue\n"}, nil))
	bundle := readFile(t, tarGz(t, map[string]string{"cudart-libs/libcudart.so.13": "cudart"}, nil))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "rt") {
			w.Write(bundle)
			return
		}
		w.Write(build)
	}))
	defer srv.Close()

	dir := t.TempDir()
	inst := &Installer{Dir: dir, APIBase: srv.URL, Client: srv.Client()}
	sum := sha256.Sum256(build)
	asset := Asset{
		Tag: "b1", Name: "llama-b1-bin-ubuntu-cuda-13.4-x64.tar.gz", URL: srv.URL + "/build.tar.gz",
		Size: int64(len(build)), Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Variant: AcceleratorCUDA, Suite: "13.4",
		Companion: &Asset{
			Tag: "b1", Name: "cudart-llama-b1-bin-ubuntu-cuda-13.4-x64.tar.gz", URL: srv.URL + "/rt.tar.gz",
			Size: int64(len(bundle)), Digest: "sha256:" + strings.Repeat("ab", 32),
			Variant: AcceleratorCUDA, Suite: "13.4",
		},
	}
	if _, err := inst.Install(context.Background(), asset); err == nil {
		t.Fatal("a runtime bundle with a wrong digest was installed")
	} else if !strings.Contains(err.Error(), "Prüfsumme") {
		t.Errorf("error = %q, want the digest named", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b1-cuda-13.4")); !os.IsNotExist(err) {
		t.Errorf("a build directory was left behind after a failed install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b1-cuda-13.4.installing")); !os.IsNotExist(err) {
		t.Errorf("a staging directory was left behind after a failed install: %v", err)
	}
}

// A release that reports no size must still not drive the bar past 100%: the two
// archives are then reported one at a time rather than against a total nobody knows.
func TestInstallProgressStaysWithinItsTotal(t *testing.T) {
	build := readFile(t, tarGz(t, map[string]string{"llama-b1/llama-server": "#!/bin/sh\ntrue\n"}, nil))
	bundle := readFile(t, tarGz(t, map[string]string{"cudart-libs/libcudart.so.13": "cudart"}, nil))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "rt") {
			w.Write(bundle)
			return
		}
		w.Write(build)
	}))
	defer srv.Close()

	dir := t.TempDir()
	var exceeded []string
	inst := &Installer{
		Dir: dir, APIBase: srv.URL, Client: srv.Client(),
		OnProgress: func(p Progress) {
			if p.Total > 0 && p.Done > p.Total {
				exceeded = append(exceeded, fmt.Sprintf("%d of %d", p.Done, p.Total))
			}
		},
	}
	asset := Asset{
		Tag: "b1", Name: "llama-b1-bin-ubuntu-cuda-13.4-x64.tar.gz", URL: srv.URL + "/build.tar.gz",
		Variant: AcceleratorCUDA, Suite: "13.4",
		Companion: &Asset{
			Tag: "b1", Name: "cudart-llama-b1-bin-ubuntu-cuda-13.4-x64.tar.gz", URL: srv.URL + "/rt.tar.gz",
			Variant: AcceleratorCUDA, Suite: "13.4",
		},
	}
	if _, err := inst.Install(context.Background(), asset); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(exceeded) != 0 {
		t.Errorf("progress exceeded its total: %v", exceeded)
	}
}

// A family without a companion asset is installed as it is: ROCm's runtime is the
// system's, and asking for a bundle that does not exist must not break the install.
func TestLatestLeavesFamiliesWithoutABundleAlone(t *testing.T) {
	const body = `[{"tag_name":"b11538","assets":[
	  {"name":"llama-b11538-bin-ubuntu-rocm-10.0-x64.tar.gz","size":244900000,
	   "browser_download_url":"https://example.invalid/rocm.tar.gz"}]}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	inst := &Installer{
		Dir: t.TempDir(), APIBase: srv.URL, Client: srv.Client(),
		SuiteCheck: func(string, string) (bool, string) { return true, "" },
	}
	asset, err := inst.Latest(context.Background(), LlamaCPP, "linux", "amd64", AcceleratorROCm)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if asset.Companion != nil {
		t.Errorf("companion = %+v, want none for a family whose runtime is the system's", asset.Companion)
	}
	if asset.Dir() != "b11538-rocm-10.0" {
		t.Errorf("dir = %q, want the ROCm build", asset.Dir())
	}
}
