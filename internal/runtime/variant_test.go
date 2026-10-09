package runtime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAssetNameCoversEveryPublishedFamily pins the table against the names
// llama.cpp actually publishes, read off release b11535 (36 assets). The
// versioned families are asked for with a `*` where the release carries a suite,
// because pinning `cuda-13.4` here would break the day the suite moves on.
func TestAssetNameCoversEveryPublishedFamily(t *testing.T) {
	cases := []struct {
		goos, goarch, accel string
		want                string
	}{
		{"linux", "amd64", AcceleratorCPU, "llama-b11535-bin-ubuntu-x64.tar.gz"},
		{"linux", "amd64", AcceleratorVulkan, "llama-b11535-bin-ubuntu-vulkan-x64.tar.gz"},
		{"linux", "amd64", AcceleratorROCm, "llama-b11535-bin-ubuntu-rocm-*-x64.tar.gz"},
		{"linux", "amd64", AcceleratorCUDA, "llama-b11535-bin-ubuntu-cuda-*-x64.tar.gz"},
		{"linux", "amd64", AcceleratorSYCL, "llama-b11535-bin-ubuntu-sycl-*-x64.tar.gz"},
		{"linux", "amd64", AcceleratorOpenVINO, "llama-b11535-bin-ubuntu-openvino-*-x64.tar.gz"},
		{"linux", "arm64", AcceleratorROCm, ""}, // not published for arm64
		{"linux", "arm64", AcceleratorVulkan, "llama-b11535-bin-ubuntu-vulkan-arm64.tar.gz"},
		{"darwin", "arm64", AcceleratorCPU, "llama-b11535-bin-macos-arm64.tar.gz"},
		{"darwin", "arm64", AcceleratorVulkan, ""}, // macOS builds already use Metal
		{"windows", "amd64", AcceleratorROCm, "llama-b11535-bin-win-rocm-*-x64.zip"},
		{"windows", "amd64", AcceleratorOpenVINO, "llama-b11535-bin-win-openvino-*-x64.zip"},
		{"windows", "arm64", AcceleratorOpenCL, "llama-b11535-bin-win-opencl-adreno-arm64.zip"},
		{"windows", "arm64", AcceleratorROCm, ""}, // no ROCm for arm64 Windows
	}
	for _, tc := range cases {
		got, err := AssetName("b11535", tc.goos, tc.goarch, tc.accel)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%s/%s/%s = %q, want a refusal", tc.goos, tc.goarch, tc.accel, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s/%s/%s: %v", tc.goos, tc.goarch, tc.accel, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s/%s/%s = %q, want %q", tc.goos, tc.goarch, tc.accel, got, tc.want)
		}
	}
}

// TestLatestTakesTheNewestSuiteOfTheFamily: a release publishes several CUDA
// suites at once, and a user asking for "cuda" means the newest one their driver
// can speak — not whichever happens to come first in the API's list, and never the
// `cudart-` bundle, which is a different asset with the runtime folded in.
func TestLatestTakesTheNewestSuiteOfTheFamily(t *testing.T) {
	const body = `[{"tag_name":"b11535","assets":[
	  {"name":"cudart-llama-b11535-bin-ubuntu-cuda-13.4-x64.tar.gz","size":1,"browser_download_url":"https://example.invalid/cudart.tar.gz"},
	  {"name":"llama-b11535-bin-ubuntu-cuda-12.8-x64.tar.gz","size":2,"browser_download_url":"https://example.invalid/cuda128.tar.gz"},
	  {"name":"llama-b11535-bin-ubuntu-cuda-13.4-x64.tar.gz","size":3,"browser_download_url":"https://example.invalid/cuda134.tar.gz"},
	  {"name":"llama-b11535-bin-ubuntu-rocm-10.0-x64.tar.gz","size":4,"browser_download_url":"https://example.invalid/rocm.tar.gz"},
	  {"name":"llama-b11535-bin-ubuntu-x64.tar.gz","size":5,"browser_download_url":"https://example.invalid/cpu.tar.gz"}
	]}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	inst := &Installer{Dir: t.TempDir(), APIBase: srv.URL, Client: srv.Client()}

	cuda, err := inst.Latest(context.Background(), "linux", "amd64", AcceleratorCUDA)
	if err != nil {
		t.Fatalf("cuda: %v", err)
	}
	if cuda.Name != "llama-b11535-bin-ubuntu-cuda-13.4-x64.tar.gz" {
		t.Errorf("cuda chose %q, want the 13.4 suite", cuda.Name)
	}
	if cuda.Variant != AcceleratorCUDA || cuda.Suite != "13.4" {
		t.Errorf("cuda asset = %+v, want variant cuda with suite 13.4", cuda)
	}
	if cuda.Dir() != "b11535-cuda-13.4" {
		t.Errorf("cuda dir = %q", cuda.Dir())
	}

	rocm, err := inst.Latest(context.Background(), "linux", "amd64", AcceleratorROCm)
	if err != nil {
		t.Fatalf("rocm: %v", err)
	}
	if rocm.Name != "llama-b11535-bin-ubuntu-rocm-10.0-x64.tar.gz" || rocm.Suite != "10.0" {
		t.Errorf("rocm asset = %+v, want the 10.0 build", rocm)
	}

	// The plain CPU build must not be picked up by a GPU family, and vice versa:
	// the two names differ by one segment.
	cpu, err := inst.Latest(context.Background(), "linux", "amd64", AcceleratorCPU)
	if err != nil {
		t.Fatalf("cpu: %v", err)
	}
	if cpu.Name != "llama-b11535-bin-ubuntu-x64.tar.gz" || cpu.Suite != "" {
		t.Errorf("cpu asset = %+v", cpu)
	}
	if cpu.Dir() != "b11535-cpu" {
		t.Errorf("cpu dir = %q", cpu.Dir())
	}
}

// TestLatestRefusesAFamilyThisPlatformDoesNotPublish: the picker is filled from
// PublishedAccelerators, so this is the second line of defence — a stored setting
// from another machine must not turn into a request for an archive that is not
// there.
func TestLatestRefusesAFamilyThisPlatformDoesNotPublish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"b11535","assets":[{"name":"llama-b11535-bin-macos-arm64.tar.gz"}]}]`)
	}))
	defer srv.Close()
	inst := &Installer{Dir: t.TempDir(), APIBase: srv.URL, Client: srv.Client()}
	if _, err := inst.Latest(context.Background(), "darwin", "arm64", AcceleratorROCm); err == nil {
		t.Fatal("ROCm on macOS must be refused rather than answered with something else")
	}
}

// TestPublishedAcceleratorsPerPlatform is what the settings picker is built from,
// so an entry that is offered but not published is a dead option.
func TestPublishedAcceleratorsPerPlatform(t *testing.T) {
	cases := map[string][]string{
		"linux/amd64":   {AcceleratorCPU, AcceleratorVulkan, AcceleratorROCm, AcceleratorCUDA, AcceleratorSYCL, AcceleratorOpenVINO},
		"linux/arm64":   {AcceleratorCPU, AcceleratorVulkan, AcceleratorCUDA},
		"darwin/arm64":  {AcceleratorCPU},
		"darwin/amd64":  {AcceleratorCPU},
		"windows/amd64": {AcceleratorCPU, AcceleratorVulkan, AcceleratorROCm, AcceleratorCUDA, AcceleratorSYCL, AcceleratorOpenVINO},
		"windows/arm64": {AcceleratorCPU, AcceleratorVulkan, AcceleratorCUDA, AcceleratorOpenCL},
	}
	for platform, want := range cases {
		parts := strings.Split(platform, "/")
		got := PublishedAccelerators(parts[0], parts[1])
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s = %v, want %v", platform, got, want)
		}
	}
	if got := PublishedAccelerators("freebsd", "amd64"); len(got) != 0 {
		t.Errorf("freebsd = %v, want nothing offered", got)
	}
}

// TestPreferSuite: the comparison has to work on versions and leave everything
// else alone, because the wildcard segment is a version for CUDA and ROCm and a
// flavour name for SYCL.
func TestPreferSuite(t *testing.T) {
	cases := []struct {
		candidate, best string
		want            bool
	}{
		{"13.4", "12.8", true},
		{"12.8", "13.4", false},
		{"10.0", "9.9", true},
		{"fp16", "fp32", false}, // not a version: keep the release's own order
		{"anything", "", true},  // the first match always wins over nothing
	}
	for _, tc := range cases {
		if got := preferSuite(tc.candidate, tc.best); got != tc.want {
			t.Errorf("preferSuite(%q, %q) = %v, want %v", tc.candidate, tc.best, got, tc.want)
		}
	}
}

// TestMatchAssetName: the wildcard stands for exactly one segment, which is what
// keeps `ubuntu-x64` and `ubuntu-vulkan-x64` — and the `cudart-` bundle, which has
// one segment more — apart.
func TestMatchAssetName(t *testing.T) {
	pattern := "llama-b11535-bin-ubuntu-cuda-*-x64.tar.gz"
	cases := map[string]string{
		"llama-b11535-bin-ubuntu-cuda-13.4-x64.tar.gz":        "13.4",
		"llama-b11535-bin-ubuntu-cuda-12.8-x64.tar.gz":        "12.8",
		"llama-b11535-bin-ubuntu-x64.tar.gz":                  "",
		"llama-b11535-bin-ubuntu-rocm-10.0-x64.tar.gz":        "",
		"cudart-llama-b11535-bin-ubuntu-cuda-13.4-x64.tar.gz": "",
		"llama-b11534-bin-ubuntu-cuda-13.4-x64.tar.gz":        "",
	}
	for name, want := range cases {
		got, ok := matchAssetName(pattern, name)
		if want == "" {
			if ok {
				t.Errorf("matchAssetName(%q) = %q, want no match", name, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("matchAssetName(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
	suffix, ok := matchAssetName("llama-b11535-bin-ubuntu-x64.tar.gz", "llama-b11535-bin-ubuntu-x64.tar.gz")
	if !ok || suffix != "" {
		t.Errorf("an exact pattern must match with no open segment: %q %v", suffix, ok)
	}
}
