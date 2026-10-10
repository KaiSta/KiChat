package runtime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fork's table against the names it really publishes, read off release
// prism-b10770-6684606 (26 assets, published 2026-10-09). The differences from
// upstream are the point: Linux CUDA is `linux-` and not `ubuntu-`, Windows calls
// its ROCm build `hip-radeon`, and OpenVINO and the Adreno build do not exist here.
func TestForkTableMatchesThePublishedNames(t *testing.T) {
	cases := []struct {
		goos, goarch, accel string
		want                string
	}{
		{"linux", "amd64", AcceleratorCPU, "llama-prism-b10770-6684606-bin-ubuntu-x64.tar.gz"},
		{"linux", "amd64", AcceleratorVulkan, "llama-prism-b10770-6684606-bin-ubuntu-vulkan-x64.tar.gz"},
		{"linux", "amd64", AcceleratorROCm, "llama-prism-b10770-6684606-bin-ubuntu-rocm-*-x64.tar.gz"},
		{"linux", "amd64", AcceleratorCUDA, "llama-prism-b10770-6684606-bin-linux-cuda-*-x64.tar.gz"},
		{"linux", "amd64", AcceleratorSYCL, "llama-prism-b10770-6684606-bin-ubuntu-sycl-*-x64.tar.gz"},
		{"linux", "amd64", AcceleratorOpenVINO, ""}, // no such build in the fork
		{"linux", "arm64", AcceleratorCPU, "llama-prism-b10770-6684606-bin-ubuntu-arm64.tar.gz"},
		{"linux", "arm64", AcceleratorVulkan, "llama-prism-b10770-6684606-bin-ubuntu-vulkan-arm64.tar.gz"},
		{"linux", "arm64", AcceleratorCUDA, ""}, // the fork has no arm64 CUDA for Linux
		{"darwin", "arm64", AcceleratorCPU, "llama-prism-b10770-6684606-bin-macos-arm64.tar.gz"},
		{"darwin", "amd64", AcceleratorCPU, "llama-prism-b10770-6684606-bin-macos-x64.tar.gz"},
		{"windows", "amd64", AcceleratorROCm, "llama-prism-b10770-6684606-bin-win-hip-radeon-x64.zip"},
		{"windows", "amd64", AcceleratorCUDA, "llama-prism-b10770-6684606-bin-win-cuda-*-x64.zip"},
		{"windows", "amd64", AcceleratorOpenVINO, ""},
		{"windows", "arm64", AcceleratorCUDA, "llama-prism-b10770-6684606-bin-win-cuda-*-arm64.zip"},
		{"windows", "arm64", AcceleratorVulkan, ""}, // no Vulkan build for arm64 Windows
		{"windows", "arm64", AcceleratorOpenCL, ""}, // no Adreno build in the fork
		{"freebsd", "amd64", AcceleratorCPU, ""},
	}
	const tag = "prism-b10770-6684606"
	for _, tc := range cases {
		got, err := PrismML.AssetName(tag, tc.goos, tc.goarch, tc.accel)
		if tc.want == "" {
			if err == nil {
				t.Errorf("fork %s/%s/%s = %q, want a refusal", tc.goos, tc.goarch, tc.accel, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("fork %s/%s/%s: %v", tc.goos, tc.goarch, tc.accel, err)
			continue
		}
		if got != tc.want {
			t.Errorf("fork %s/%s/%s = %q, want %q", tc.goos, tc.goarch, tc.accel, got, tc.want)
		}
	}
}

// TestAFamilyTheForkDoesNotPublishIsNotFetchedFromUpstream: the refusal has to be a
// refusal. Answering an OpenVINO request with upstream's archive would install a
// launcher without the ternary kernels while every label in the panel still said
// PrismML — the silent substitution the tables exist to prevent.
func TestAFamilyTheForkDoesNotPublishIsNotFetchedFromUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"b11541","assets":[
		  {"name":"llama-b11541-bin-ubuntu-openvino-2025.3-x64.tar.gz","size":1,"browser_download_url":"https://example.invalid/x.tar.gz"}
		]}]`)
	}))
	defer srv.Close()
	inst := &Installer{Dir: t.TempDir(), APIBase: srv.URL, Client: srv.Client()}

	if _, err := inst.Latest(context.Background(), PrismML, "linux", "amd64", AcceleratorOpenVINO); err == nil {
		t.Fatal("the fork was asked for an OpenVINO build and the lookup answered")
	}
	// And the same listing does answer the source it belongs to, so the test is not
	// passing because the fixture is broken.
	if _, err := inst.Latest(context.Background(), LlamaCPP, "linux", "amd64", AcceleratorOpenVINO); err != nil {
		t.Fatalf("upstream could not read its own listing: %v", err)
	}
}

// The whole lookup for the fork, over a listing shaped like the real one: the CPU
// archive with the android, arm64 and xcframework siblings that must not be mistaken
// for it.
func TestLatestReadsTheForksRelease(t *testing.T) {
	const body = `[{"tag_name":"prism-b10770-6684606","assets":[
	  {"name":"cudart-llama-bin-win-cuda-13.3-x64.zip","size":1,"browser_download_url":"https://example.invalid/cudart.zip"},
	  {"name":"llama-prism-b10770-6684606-bin-android-arm64.tar.gz","size":2,"browser_download_url":"https://example.invalid/android.tar.gz"},
	  {"name":"llama-prism-b10770-6684606-bin-linux-cuda-12.8-x64.tar.gz","size":3,"browser_download_url":"https://example.invalid/cuda128.tar.gz"},
	  {"name":"llama-prism-b10770-6684606-bin-linux-cuda-13.3-x64.tar.gz","size":4,"browser_download_url":"https://example.invalid/cuda133.tar.gz"},
	  {"name":"llama-prism-b10770-6684606-bin-ubuntu-arm64.tar.gz","size":5,"browser_download_url":"https://example.invalid/arm64.tar.gz"},
	  {"name":"llama-prism-b10770-6684606-bin-ubuntu-vulkan-x64.tar.gz","size":6,"browser_download_url":"https://example.invalid/vulkan.tar.gz"},
	  {"name":"llama-prism-b10770-6684606-bin-ubuntu-x64.tar.gz","size":17800388,"browser_download_url":"https://example.invalid/cpu.tar.gz"},
	  {"name":"llama-prism-b10770-6684606-xcframework.zip","size":8,"browser_download_url":"https://example.invalid/xcframework.zip"}
	]}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	inst := &Installer{
		Dir: t.TempDir(), APIBase: srv.URL, Client: srv.Client(),
		// Which CUDA suite this machine's driver speaks is a question with its own
		// tests; this one is about which archive the fork's listing yields.
		SuiteCheck: func(string, string) (bool, string) { return true, "" },
	}

	cpu, err := inst.Latest(context.Background(), PrismML, "linux", "amd64", AcceleratorCPU)
	if err != nil {
		t.Fatalf("cpu: %v", err)
	}
	if cpu.Name != "llama-prism-b10770-6684606-bin-ubuntu-x64.tar.gz" {
		t.Errorf("cpu chose %q", cpu.Name)
	}
	if cpu.Tag != "prism-b10770-6684606" || cpu.Variant != AcceleratorCPU || cpu.Suite != "" {
		t.Errorf("cpu asset = %+v", cpu)
	}
	// The directory name is what the settings record and what a later lookup has to
	// read the tag back out of.
	if cpu.Dir() != "prism-b10770-6684606-cpu" {
		t.Errorf("cpu dir = %q", cpu.Dir())
	}
	if got := PrismML.TagOfDir(cpu.Dir()); got != "prism-b10770-6684606" {
		t.Errorf("TagOfDir(%q) = %q, want the tag it was built from", cpu.Dir(), got)
	}
	if cpu.Size != 17800388 {
		t.Errorf("size = %d, want the 17,800,388 bytes the release reports", cpu.Size)
	}

	// The fork's CUDA build is not the upstream name, and it needs no companion on
	// Linux: the archive carries the runtime (175 MB for 13.3 against upstream's
	// 146 MB build plus a 420 MB bundle).
	cuda, err := inst.Latest(context.Background(), PrismML, "linux", "amd64", AcceleratorCUDA)
	if err != nil {
		t.Fatalf("cuda: %v", err)
	}
	if cuda.Name != "llama-prism-b10770-6684606-bin-linux-cuda-13.3-x64.tar.gz" || cuda.Suite != "13.3" {
		t.Errorf("cuda asset = %+v, want the newest suite of the `linux-` family", cuda)
	}
	if cuda.Dir() != "prism-b10770-6684606-cuda-13.3" {
		t.Errorf("cuda dir = %q", cuda.Dir())
	}
	if cuda.Companion != nil {
		t.Errorf("cuda companion = %+v, want none: the fork's Linux archives bundle it", cuda.Companion)
	}

	// What keeps the two sources apart is the endpoint they are listed from and
	// not the shape of an archive name: both publish `llama-<tag>-bin-…`, and the
	// fork's tag happens to start with `prism-`, so the same name would match an
	// upstream lookup pointed at the fork's repository. Two different URLs are what
	// makes that impossible, so they are what is asserted.
	if LlamaCPP.ReleasesAPI() == PrismML.ReleasesAPI() {
		t.Fatal("both sources list from the same endpoint")
	}
	if got, want := PrismML.ReleasesAPI(), "https://api.github.com/repos/PrismML-Eng/llama.cpp/releases"; got != want {
		t.Errorf("fork releases API = %q, want %q", got, want)
	}
	if !strings.HasPrefix(cpu.Name, "llama-prism-") {
		t.Errorf("fork archive %q does not carry the fork's tag", cpu.Name)
	}
}

// A build directory says which source it came from, and only a name that is a tag
// of the source answering gets a tag back. The two shapes cannot be confused, which
// is what lets a runtime folder hold builds of both.
func TestOnlyTheOwningSourceReadsADirectoryName(t *testing.T) {
	cases := []struct {
		dist Distribution
		dir  string
		want string
	}{
		{LlamaCPP, "b11541", "b11541"},
		{LlamaCPP, "b11541-cpu", "b11541"},
		{LlamaCPP, "b11541-vulkan", "b11541"},
		{LlamaCPP, "b11541-vulkan-10.0", "b11541"},
		{LlamaCPP, "b11541-cuda-13.4", "b11541"},
		{LlamaCPP, "b11541-sycl-fp16", "b11541"},
		{LlamaCPP, "b11541-vulkan-10.0.installing", "b11541"},
		{LlamaCPP, "prism-b10770-6684606-cpu", ""},
		{LlamaCPP, "", ""},
		{LlamaCPP, "llama-server", ""},

		{PrismML, "prism-b10770-6684606", "prism-b10770-6684606"},
		{PrismML, "prism-b10770-6684606-cpu", "prism-b10770-6684606"},
		{PrismML, "prism-b10770-6684606-cuda-13.3", "prism-b10770-6684606"},
		{PrismML, "prism-b10770-6684606-vulkan-7.2", "prism-b10770-6684606"},
		{PrismML, "prism-b10770-6684606-sycl-fp16", "prism-b10770-6684606"},
		{PrismML, "prism-b10754-2459f68-cpu", "prism-b10754-2459f68"},
		{PrismML, "b11541-vulkan", ""},
		{PrismML, "prism", ""},
		{PrismML, "prism-b10770", ""},
		// The staging directory of an interrupted install is named after the build
		// it was going to be, so the tag is legible in it too. Whether such a
		// directory is a *build* is not this function's question — and asking this
		// one is not enough to answer it either, because a crash late in an install
		// leaves a launcher in there: Find refuses that name outright and Builds
		// marks it incomplete, which is what
		// TestAStagingDirectoryWithALauncherIsStillAnInterruptedInstall pins.
		{PrismML, "prism-b10770-6684606-cpu.installing", "prism-b10770-6684606"},
	}
	for _, tc := range cases {
		if got := tc.dist.TagOfDir(tc.dir); got != tc.want {
			t.Errorf("%s.TagOfDir(%q) = %q, want %q", tc.dist.ID, tc.dir, got, tc.want)
		}
	}
}

// The failure this whole shape exists for: compared as text, a fork tag against a
// directory name of the other source reads as "newer" on every poll — which is what
// the old helper did, because it cut a directory name at the first dash, so
// `prism-b10770-6684606-cpu` reduced to `prism` and every comparison against `prism`
// came out as "different, therefore newer". An installed fork build would have been
// offered its own build as an update, forever.
func TestAForkBuildIsNotOfferedItsOwnBuildAsAnUpdate(t *testing.T) {
	const tag = "prism-b10770-6684606"
	installed := PrismML.TagOfDir("prism-b10770-6684606-cpu")
	if installed != tag {
		t.Fatalf("installed tag = %q, want %q", installed, tag)
	}
	if PrismML.NewerBuild(tag, installed) {
		t.Error("the build on disk was offered as an update over itself")
	}
	// And a name of the other source parses to no tag at all, so an upstream
	// comparison has nothing to say about a fork build.
	if got := LlamaCPP.TagOfDir("prism-b10770-6684606-cpu"); got != "" {
		t.Errorf("upstream read a fork directory as %q", got)
	}
	if LlamaCPP.NewerBuild(tag, LlamaCPP.TagOfDir("prism-b10770-6684606-cpu")) {
		t.Error("a fork build on disk was offered as an upstream update")
	}
}

// The numeric comparison, on both tag shapes: the digit boundary is the case a
// textual comparison gets wrong, and it applies to the fork's counter as much as to
// upstream's.
func TestNewerBuildPerSource(t *testing.T) {
	cases := []struct {
		dist              Distribution
		latest, installed string
		want              bool
	}{
		{LlamaCPP, "b11541", "b11535", true},
		{LlamaCPP, "b11535", "b11541", false},
		{LlamaCPP, "b1000", "b999", true},
		{LlamaCPP, "b11541", "", false},
		{LlamaCPP, "v0.6.0", "b11535", true},

		{PrismML, "prism-b10771-abcdef0", "prism-b10770-6684606", true},
		{PrismML, "prism-b10770-6684606", "prism-b10770-6684606", false},
		{PrismML, "prism-b10769-6684606", "prism-b10770-6684606", false},
		{PrismML, "prism-b1000-abcdef0", "prism-b999-abcdef0", true},
		{PrismML, "prism-b10770-6684606", "", false},
	}
	for _, tc := range cases {
		if got := tc.dist.NewerBuild(tc.latest, tc.installed); got != tc.want {
			t.Errorf("%s.NewerBuild(%q, %q) = %v, want %v", tc.dist.ID, tc.latest, tc.installed, got, tc.want)
		}
	}
}

// Which builds a lookup sees, with both sources installed side by side: the two are
// separate installations, and a lookup for one must not answer with the other —
// both are called `llama-server`, so the mistake would be invisible until a ternary
// model failed to load.
func TestFindSeesOnlyTheAskedSourcesBuilds(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b11541-cpu", "prism-b10770-6684606-cpu"} {
		build := filepath.Join(dir, name)
		if err := os.MkdirAll(build, 0o755); err != nil {
			t.Fatal(err)
		}
		writeLauncher(t, build)
	}
	inst := &Installer{Dir: dir}

	// Find answers with the directory name, which is what the settings record and
	// what TagOfDir then reads a tag out of.
	if tag, binary := inst.Find(LlamaCPP); tag != "b11541-cpu" || binary == "" {
		t.Errorf("Find(upstream) = %q, %q; want the upstream build", tag, binary)
	}
	if tag, binary := inst.Find(PrismML); tag != "prism-b10770-6684606-cpu" || binary == "" {
		t.Errorf("Find(fork) = %q, %q; want the fork build", tag, binary)
	}
	// A source with nothing installed answers with nothing rather than with the
	// other source's newest build: "installed" has to mean "installed for this
	// source", or the panel offers to start a launcher that cannot load the model.
	if err := os.RemoveAll(filepath.Join(dir, "prism-b10770-6684606-cpu")); err != nil {
		t.Fatal(err)
	}
	if tag, _ := inst.Find(PrismML); tag != "" {
		t.Errorf("Find(fork) = %q with only upstream's build on disk", tag)
	}
}

// The distributions a settings blob may name, and the family list each of them
// offers: what a picker is filled from.
func TestDistributionsResolveAndPublishWhatTheyHave(t *testing.T) {
	if got := DefaultDistribution().ID; got != DistributionLlamaCPP {
		t.Errorf("default distribution = %q, want upstream", got)
	}
	if list := Distributions(); len(list) != 2 || list[0].ID != DistributionLlamaCPP || list[1].ID != DistributionPrism {
		t.Errorf("Distributions() = %+v, want upstream first and the fork second", list)
	}
	for _, id := range []string{DistributionLlamaCPP, DistributionPrism} {
		if _, ok := DistributionFor(id); !ok {
			t.Errorf("DistributionFor(%q) did not resolve", id)
		}
	}
	// An id this app does not know is a refusal, not a substitution.
	if _, ok := DistributionFor("ik_llama"); ok {
		t.Error("an unknown distribution resolved to something")
	}
	if _, ok := DistributionFor(""); ok {
		t.Error("an empty distribution resolved to something")
	}

	// Upstream keeps every family it had; the fork offers a subset, which is what
	// the picker has to show for it.
	upstream := strings.Join(LlamaCPP.PublishedAccelerators("linux", "amd64"), ",")
	if upstream != "cpu,vulkan,rocm,cuda,sycl,openvino" {
		t.Errorf("upstream linux/amd64 = %q", upstream)
	}
	fork := strings.Join(PrismML.PublishedAccelerators("linux", "amd64"), ",")
	if fork != "cpu,vulkan,rocm,cuda,sycl" {
		t.Errorf("fork linux/amd64 = %q, want no OpenVINO", fork)
	}
	if got := strings.Join(PrismML.PublishedAccelerators("windows", "arm64"), ","); got != "cpu,cuda" {
		t.Errorf("fork windows/arm64 = %q", got)
	}
	if got := PrismML.PublishedAccelerators("freebsd", "amd64"); len(got) != 0 {
		t.Errorf("fork freebsd = %v, want nothing offered", got)
	}

	// Every family either source publishes has to be a value the settings may store,
	// or a build could be installed that nothing can ask for again. Accelerators() is
	// that value space and is the union, not one source's answer.
	known := strings.Join(Accelerators(), ",")
	for _, d := range Distributions() {
		for _, goos := range []string{"linux", "darwin", "windows"} {
			for _, accel := range d.PublishedAccelerators(goos, "amd64") {
				if !strings.Contains(","+known+",", ","+accel+",") {
					t.Errorf("%s publishes %s on %s, which Accelerators() does not list", d.ID, accel, goos)
				}
			}
		}
	}
}
