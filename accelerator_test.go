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
}

// TestInstallUsesThePickedBuild pins the fix for "I picked ROCm and the model still
// ran on the CPU": the Build picker edits the settings draft, so the download must
// be told the family rather than reading the saved one, which is the previous
// selection. An empty or unknown pick falls back instead of becoming a URL.
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
}
