package main

import (
	"os"
	"strings"
	"testing"

	"kichat/internal/config"
	rt "kichat/internal/runtime"
)

// TestAcceleratorNamesAgreeEverywhere pins three lists that have to match: the
// values settings may store, the families the installer can map to a published
// archive, and the labels the picker renders. A value in one list and not another
// is either a build nobody can ask for, an option that renders as a raw id, or a
// stored setting that silently becomes the CPU build.
func TestAcceleratorNamesAgreeEverywhere(t *testing.T) {
	settings := config.Accelerators()
	if len(settings) == 0 {
		t.Fatal("config.Accelerators() is empty")
	}
	published := rt.Accelerators()
	if strings.Join(settings, ",") != strings.Join(published, ",") {
		t.Fatalf("settings %v and installer %v disagree", settings, published)
	}

	raw, err := os.ReadFile(frontendDictPath)
	if err != nil {
		t.Fatalf("read %s: %v", frontendDictPath, err)
	}
	blocks := map[string]struct{ from, to string }{
		config.LangGerman:  {"const de = {", "const en"},
		config.LangEnglish: {"const en:", "const zh"},
		config.LangChinese: {"const zh:", "const dictionaries"},
	}
	dicts := map[string]string{}
	for lang, b := range blocks {
		block, ok := dictBlock(string(raw), b.from, b.to)
		if !ok {
			t.Fatalf("%s: cannot find the %q dictionary", frontendDictPath, lang)
		}
		dicts[lang] = block
	}

	// The key is derived from the stored value the same way the picker derives it,
	// so a new family that forgets its label fails here rather than in the UI.
	for _, id := range settings {
		key := "settings.managed.accel" + strings.ToUpper(id[:1]) + id[1:]
		for lang, block := range dicts {
			if !strings.Contains(block, "'"+key+"':") {
				t.Errorf("%s: no %q entry — the picker would show a raw id in %s", frontendDictPath, key, lang)
			}
		}
	}
}

// TestDistributionNamesAgreeEverywhere pins the same three lists for the release
// sources: the ids settings may store, the ones the installer resolves to a
// repository and a tag shape, and the labels the picker renders. An id in one list
// and not another is a source nobody can select, an option that renders as a raw id,
// or a stored value that silently repairs itself to upstream.
func TestDistributionNamesAgreeEverywhere(t *testing.T) {
	configured := config.Distributions()
	if len(configured) == 0 {
		t.Fatal("config.Distributions() is empty")
	}
	installed := make([]string, 0, len(configured))
	for _, d := range rt.Distributions() {
		installed = append(installed, d.ID)
	}
	if strings.Join(configured, ",") != strings.Join(installed, ",") {
		t.Fatalf("settings %v and installer %v disagree", configured, installed)
	}
	for _, id := range configured {
		if _, ok := rt.DistributionFor(id); !ok {
			t.Errorf("settings may store %q and the installer does not know it", id)
		}
	}

	raw, err := os.ReadFile(frontendDictPath)
	if err != nil {
		t.Fatalf("read %s: %v", frontendDictPath, err)
	}
	blocks := map[string]struct{ from, to string }{
		config.LangGerman:  {"const de = {", "const en"},
		config.LangEnglish: {"const en:", "const zh"},
		config.LangChinese: {"const zh:", "const dictionaries"},
	}
	dicts := map[string]string{}
	for lang, b := range blocks {
		block, ok := dictBlock(string(raw), b.from, b.to)
		if !ok {
			t.Fatalf("%s: cannot find the %q dictionary", frontendDictPath, lang)
		}
		dicts[lang] = block
	}

	// Derived the way the picker derives it: `prism` becomes `distPrism`.
	for _, id := range configured {
		key := "settings.managed.dist" + strings.ToUpper(id[:1]) + id[1:]
		for lang, block := range dicts {
			if !strings.Contains(block, "'"+key+"':") {
				t.Errorf("%s: no %q entry — the picker would show a raw id in %s", frontendDictPath, key, lang)
			}
		}
	}
}

// TestFlashAndCacheValuesHaveLabels pins the same three-way agreement for the
// flash-attention modes and KV cache types the managed model can carry. The
// command-line values are llama.cpp's own tokens (`on`, `q8_0`), which are not
// prose, but each one still gets a translated label so the picker reads in the
// interface language.
func TestFlashAndCacheValuesHaveLabels(t *testing.T) {
	raw, err := os.ReadFile(frontendDictPath)
	if err != nil {
		t.Fatalf("read %s: %v", frontendDictPath, err)
	}
	blocks := map[string]struct{ from, to string }{
		config.LangGerman:  {"const de = {", "const en"},
		config.LangEnglish: {"const en:", "const zh"},
		config.LangChinese: {"const zh:", "const dictionaries"},
	}
	dicts := map[string]string{}
	for lang, b := range blocks {
		block, ok := dictBlock(string(raw), b.from, b.to)
		if !ok {
			t.Fatalf("%s: cannot find the %q dictionary", frontendDictPath, lang)
		}
		dicts[lang] = block
	}

	// The suffix is capitalised exactly the way the picker does it: `q8_0`
	// becomes `Q8_0`, `iq4_nl` becomes `Iq4_nl`.
	capitalise := func(id string) string { return strings.ToUpper(id[:1]) + id[1:] }
	check := func(prefix string, values []string) {
		if len(values) == 0 {
			t.Fatalf("%s: the value list is empty", prefix)
		}
		for _, id := range values {
			key := "settings.managed." + prefix + capitalise(id)
			for lang, block := range dicts {
				if !strings.Contains(block, "'"+key+"':") {
					t.Errorf("%s: no %q entry — the picker would show a raw value in %s", frontendDictPath, key, lang)
				}
			}
		}
	}
	check("flash", config.FlashAttentionModes())
	check("cache", config.CacheTypes())
	check("offload", config.OffloadModes())
}

// The GPU offload modes are spelled twice on purpose — the settings package owns the
// value a blob may store, the runtime package owns the flag value it becomes — so the
// two have to be held together, and each word has to have a label in all three
// dictionaries. A mode the runtime does not know is a picker entry that silently
// means "auto"; a mode without a label is an option that renders as a raw id.
func TestOffloadModesAgreeEverywhere(t *testing.T) {
	modes := config.OffloadModes()
	if len(modes) == 0 {
		t.Fatal("config.OffloadModes() is empty")
	}
	if config.OffloadAll != rt.OffloadAll {
		t.Errorf("settings spell the full offload %q and the runtime %q", config.OffloadAll, rt.OffloadAll)
	}
	if config.OffloadOff != rt.OffloadOff {
		t.Errorf("settings spell the CPU-only offload %q and the runtime %q", config.OffloadOff, rt.OffloadOff)
	}
	// The word the server's own default is stored under is not passed as a flag, which
	// is the whole reason it may differ from llama.cpp's spelling (`auto` for the
	// count `""` the runtime sends). It only has to be one of the modes.
	if !config.IsOffload(config.OffloadAuto) {
		t.Errorf("%q is not accepted as an offload value", config.OffloadAuto)
	}

	raw, err := os.ReadFile(frontendDictPath)
	if err != nil {
		t.Fatalf("read %s: %v", frontendDictPath, err)
	}
	blocks := map[string]struct{ from, to string }{
		config.LangGerman:  {"const de = {", "const en"},
		config.LangEnglish: {"const en:", "const zh"},
		config.LangChinese: {"const zh:", "const dictionaries"},
	}
	for lang, b := range blocks {
		block, ok := dictBlock(string(raw), b.from, b.to)
		if !ok {
			t.Fatalf("%s: cannot find the %q dictionary", frontendDictPath, lang)
		}
		for _, mode := range modes {
			key := "settings.managed.offload" + strings.ToUpper(mode[:1]) + mode[1:]
			if !strings.Contains(block, "'"+key+"':") {
				t.Errorf("%s: no %q entry — the picker would show a raw value in %s", frontendDictPath, key, lang)
			}
		}
		// The row that reports what the server did, and the one for a model that stayed
		// on the CPU, are the other half of the feature: without them the reading has
		// nowhere to appear.
		for _, key := range []string{"settings.managed.offload", "settings.managed.offloadHint", "settings.managed.offloadReport", "settings.managed.offloadCpu"} {
			if !strings.Contains(block, "'"+key+"':") {
				t.Errorf("%s: no %q entry in %s", frontendDictPath, key, lang)
			}
		}
	}
}

// TestInstallUsesThePickedBuild pins the fix for "I picked ROCm and the model still
// ran on the CPU": the Build picker edits the settings draft, so the download must
// be told the family rather than reading the saved one, which is the previous
// selection. An empty or unknown pick falls back instead of becoming a URL. The
// source picker is the same rule one level up, and its fallback matters more: the
// wrong family is a slower build, the wrong source is a llama.cpp that cannot load
// the model the source was chosen for.
func TestInstallUsesThePickedBuild(t *testing.T) {
	cases := []struct{ picked, stored, want string }{
		{"rocm", "cpu", "rocm"},
		{"vulkan", "cpu", "vulkan"},
		{"  cuda  ", "cpu", "cuda"},
		{"", "rocm", "rocm"},
		{"   ", "cpu", "cpu"},
		{"cuda-99", "vulkan", "vulkan"},
	}
	for _, c := range cases {
		if got := installAccelerator(c.picked, c.stored); got != c.want {
			t.Errorf("installAccelerator(%q, %q) = %q, want %q", c.picked, c.stored, got, c.want)
		}
	}

	sources := []struct{ picked, stored, want string }{
		{config.DistPrism, config.DistLlamaCPP, config.DistPrism},
		{"  " + config.DistLlamaCPP + "  ", config.DistPrism, config.DistLlamaCPP},
		{"", config.DistPrism, config.DistPrism},
		{"ik_llama", config.DistLlamaCPP, config.DistLlamaCPP},
	}
	for _, c := range sources {
		if got := installDistribution(c.picked, c.stored); got != c.want {
			t.Errorf("installDistribution(%q, %q) = %q, want %q", c.picked, c.stored, got, c.want)
		}
	}
}

// A stored source this app does not know is upstream, and the picker's draft is
// resolved the same way: nothing in the panel may name a source that has no
// repository behind it.
func TestAnUnknownSourceIsUpstream(t *testing.T) {
	if got := resolvedDistribution("").ID; got != config.DistLlamaCPP {
		t.Errorf("an empty source resolved to %q", got)
	}
	if got := resolvedDistribution("llama.cpp").ID; got != config.DistLlamaCPP {
		t.Errorf("an unknown source resolved to %q", got)
	}
	if got := resolvedDistribution(config.DistPrism).ID; got != config.DistPrism {
		t.Errorf("the fork resolved to %q", got)
	}
	if got := distributionFor(config.Default()).ID; got != config.DistLlamaCPP {
		t.Errorf("the shipped settings resolved to %q", got)
	}
}
