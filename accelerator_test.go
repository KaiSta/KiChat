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
