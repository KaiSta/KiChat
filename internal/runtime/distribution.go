package runtime

import (
	"fmt"
	"strings"

	"kichat/internal/uimsg"
)

// The sources of llama-server a user can install.
//
// A source is not a flag. PrismML's fork of llama.cpp publishes its own releases
// under its own tag scheme (`prism-b10770-6684606`, where upstream says `b11541`),
// with family names of its own (`linux-cuda-*` where upstream says `ubuntu-cuda-*`,
// `win-hip-radeon-x64` for ROCm on Windows, no OpenVINO or Adreno build at all) and,
// on Linux CUDA, no companion runtime archive — those builds are 175–311 MB against
// upstream's 146 MB build plus a 420 MB bundle. The fork is where the ternary kernels
// live: its `PQ2_0` (142) and `PTQ1_0` (143) GGML types are what a ternary model such
// as Bonsai 2 is stored in, and mainline llama.cpp refuses to load them. So which
// source is in use is part of every question this package is asked — what is the
// newest build, what is installed here, is there an update — and a build tag of one
// source must never be compared with a release of the other.
const (
	// DistributionLlamaCPP is upstream ggml-org/llama.cpp, the default, and the one
	// every model this app supports runs on.
	DistributionLlamaCPP = "llamacpp"
	// DistributionPrism is PrismML's fork. It tracks upstream, so it runs the
	// models upstream does *and* the ternary ones upstream cannot load; the cost of
	// choosing it is that it publishes fewer families (measured on
	// prism-b10770-6684606: no OpenVINO, no Adreno, no Linux CUDA for arm64, and
	// macOS has an extra `-kleidiai` ARM build).
	DistributionPrism = "prism"
)

// Distribution is one source of llama-server builds: where its releases live, how
// its build tags are spelled, and what it publishes per platform.
type Distribution struct {
	// ID is the value the settings store and the pickers offer.
	ID string
	// Repo is the GitHub repository, owner and name, the releases come from. It
	// is what makes the difference between the two sources a URL and a tag shape
	// rather than a second copy of the installer.
	Repo string
	// TagPrefix is what a build tag starts with, empty for upstream's `b11541`.
	TagPrefix string
	// CommitInTag marks a tag that carries a short commit hash after the build
	// number (`prism-b10770-6684606`). Upstream's tags do not, and that difference
	// is what tells the two apart when all there is to go on is a directory name.
	CommitInTag bool
	// variants is what this source publishes for one platform and architecture.
	variants func(goos, goarch string) ([]variant, error)
}

var (
	// LlamaCPP is upstream llama.cpp.
	LlamaCPP = Distribution{
		ID:       DistributionLlamaCPP,
		Repo:     "ggml-org/llama.cpp",
		variants: upstreamVariants,
	}
	// PrismML is PrismML's fork, the one that can load a ternary model.
	PrismML = Distribution{
		ID:          DistributionPrism,
		Repo:        "PrismML-Eng/llama.cpp",
		TagPrefix:   "prism-",
		CommitInTag: true,
		variants:    prismVariants,
	}
)

// Distributions lists the sources in the order a picker offers them: upstream
// first, because it is the default and publishes the most families.
func Distributions() []Distribution {
	return []Distribution{LlamaCPP, PrismML}
}

// DefaultDistribution is what a settings blob naming no source gets, which is also
// what every installation that predates the field carries.
func DefaultDistribution() Distribution {
	return LlamaCPP
}

// DistributionFor resolves a stored id, reporting whether it is one this app knows.
// An unknown id is a refusal and not a substitution: silently installing upstream's
// build for a user who asked for the fork would take the ternary kernels away
// without saying so.
func DistributionFor(id string) (Distribution, bool) {
	id = strings.TrimSpace(id)
	for _, d := range Distributions() {
		if d.ID == id {
			return d, true
		}
	}
	return Distribution{}, false
}

// ReleasesAPI is where this source lists its releases.
func (d Distribution) ReleasesAPI() string {
	return "https://api.github.com/repos/" + d.Repo + "/releases"
}

// DownloadURL is where one asset of one release lives. It is not how an install
// normally reaches an archive — the listing carries a `browser_download_url` per
// asset, and that is used because it is what GitHub signed off on — but a listing
// that omitted the link would otherwise leave an install with a name and no address.
func (d Distribution) DownloadURL(tag, name string) string {
	return "https://github.com/" + d.Repo + "/releases/download/" + tag + "/" + name
}

// Variants is the table of what this source publishes for a platform, which is per
// platform and per architecture because the releases are.
func (d Distribution) Variants(goos, goarch string) ([]variant, error) {
	table := d.variants
	if table == nil {
		// A distribution nobody resolved — a settings blob written before this
		// field existed, a struct literal in a test — is upstream, which is what
		// the zero value's empty tag prefix already spells.
		table = LlamaCPP.variants
	}
	return table(goos, goarch)
}

// variantFor picks one family out of this source's table, refusing what the source
// does not publish rather than substituting a build the user did not ask for. That
// refusal is what keeps "Prism brings no OpenVINO build" from turning into an
// upstream download while the panel still says Prism.
func (d Distribution) variantFor(goos, goarch, accel string) (variant, error) {
	if accel == "" {
		accel = AcceleratorCPU
	}
	variants, err := d.Variants(goos, goarch)
	if err != nil {
		return variant{}, err
	}
	for _, v := range variants {
		if v.ID == accel {
			return v, nil
		}
	}
	return variant{}, uimsg.Errorf("runtime.noVariant", goos, goarch, d.ID, accel)
}

// PublishedAccelerators lists the families this source publishes for a platform, in
// the order the picker offers them, so the panel cannot offer a build that does not
// exist.
//
// A platform with none answers with an empty list rather than nil: this feeds a JSON
// array, and a nil slice marshals to `null`.
func (d Distribution) PublishedAccelerators(goos, goarch string) []string {
	variants, err := d.Variants(goos, goarch)
	if err != nil {
		return []string{}
	}
	ids := make([]string, 0, len(variants))
	for _, v := range variants {
		ids = append(ids, v.ID)
	}
	return ids
}

// AssetName is the archive name this source publishes for one family, or an error
// naming what it does not cover. A `*` stands for the suite version a release
// carries (see variant).
//
// The template is the same for both sources — `llama-<tag>-bin-<platform><ext>`,
// measured on `llama-b11541-bin-ubuntu-x64.tar.gz` and on
// `llama-prism-b10770-6684606-bin-ubuntu-x64.tar.gz` — which is why the tag and the
// platform carry the difference and this function does not.
func (d Distribution) AssetName(tag, goos, goarch, accel string) (string, error) {
	v, err := d.variantFor(goos, goarch, accel)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("llama-%s-bin-%s%s", tag, v.platform, v.ext), nil
}

// IsBuildTag reports whether a string is a build tag of this source, which is what
// separates a build directory from any other directory in the runtime folder.
//
// The two shapes are `b11541` (upstream) and `prism-b10770-6684606` (the fork), and
// neither is a substring test: a directory is a build of this source only when the
// whole shape matches, so an upstream folder can never be reported as a fork build
// and the other way round.
func (d Distribution) IsBuildTag(tag string) bool {
	_, ok := d.buildNumber(tag)
	return ok
}

// buildNumber reads the number a build tag of this source carries (`b11541` and
// `prism-b10770-6684606` both become 11541 and 10770).
func (d Distribution) buildNumber(tag string) (int, bool) {
	rest, ok := strings.CutPrefix(tag, d.TagPrefix)
	if !ok {
		return 0, false
	}
	parts := strings.Split(rest, "-")
	want := 1
	if d.CommitInTag {
		want = 2
	}
	if len(parts) != want {
		return 0, false
	}
	name, digits, ok := strings.Cut(parts[0], "b")
	if !ok || name != "" || digits == "" {
		return 0, false
	}
	n := 0
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	if d.CommitInTag && !isCommitHash(parts[1]) {
		return 0, false
	}
	return n, true
}

// isCommitHash reports whether a tag segment looks like the short commit hash a
// fork's tag carries. Seven characters is what `prism-b10770-6684606` and its three
// predecessors were measured at; the range is wider because the width of a commit
// abbreviation is a choice of the tag writer and not of this app.
func isCommitHash(s string) bool {
	if len(s) < 6 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// TagOfDir reduces an installation directory name to the build tag it was made
// from, or answers empty when the name is not this source's build at all.
//
// Asset.Dir names directories `b11541`, `b11541-vulkan`, `b11541-vulkan-10.0` and
// `prism-b10770-6684606-cuda-12.4`, so the tag is the *front* of the name and the
// family and suite follow it. Reading from the right — try the whole name, then one
// segment less — needs no list of families to be kept in step with the tables above.
//
// It is also what decides whether an installed build belongs to the source the user
// selected: a directory whose name is not a tag of this distribution is not a build
// this app would start for it.
func (d Distribution) TagOfDir(dir string) string {
	parts := strings.Split(dir, "-")
	for n := len(parts); n > 0; n-- {
		if tag := strings.Join(parts[:n], "-"); d.IsBuildTag(tag) {
			return tag
		}
	}
	return ""
}

// NewerBuild reports whether a published tag is newer than an installed one of this
// same source.
//
// The two are compared as numbers and not as text: the counters are `11541` today,
// and `b1000` sorts below `b999` as text the day a counter passes a digit boundary —
// an update offered to a user who is already past it. A tag this source cannot read a
// number out of falls back to "different means newer": the panel prints both tags, so
// the user sees what they would get, and refusing an update over an unusual tag would
// hide the real ones.
//
// Nothing installed is not an update, and a tag of the other source is not a
// comparison at all — which is the difference this function exists to keep: compared
// as text, `prism-b10770-6684606` against a directory named `prism` reads as "a newer
// build is available" on every poll, for the build that is already installed.
func (d Distribution) NewerBuild(latest, installed string) bool {
	if installed == "" {
		return false
	}
	l, lok := d.buildNumber(latest)
	i, iok := d.buildNumber(installed)
	if lok && iok {
		return l > i
	}
	return latest != installed
}
