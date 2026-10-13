package main

import (
	"os"
	"strings"
	"testing"

	"kichat/internal/config"
	"kichat/internal/images"
)

// frontendDictPath is the frontend's dictionary file.
//
// The settings screen is the one place where the backend and the dictionaries
// have to agree on names: a built-in preset is shown under `preset.<id>` and a
// template variable under the key it carries, and a name that exists on one side
// only renders as the backend's own untranslated text — an English screen with a
// German line in it. This test reads the dictionaries rather than importing them,
// the same way internal/events checks the frontend's event map, so it stays part
// of `go test ./...` without a Node toolchain.
const frontendDictPath = "frontend/src/lib/i18n.ts"

// dictBlock returns the source of one dictionary, from its declaration to the
// next one. Checking a block rather than the whole file is what makes "this key
// is translated in German but missing in Chinese" a failure instead of a pass.
func dictBlock(src, from, to string) (string, bool) {
	start := strings.Index(src, from)
	if start < 0 {
		return "", false
	}
	rest := src[start+len(from):]
	end := strings.Index(rest, to)
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

func TestEveryTranslatedNameExistsInEveryDictionary(t *testing.T) {
	raw, err := os.ReadFile(frontendDictPath)
	if err != nil {
		t.Fatalf("read %s: %v", frontendDictPath, err)
	}
	src := string(raw)

	// The declarations move as the file is edited, so a missing one is reported
	// as the source having changed shape rather than as a missing translation.
	blocks := map[string]struct{ from, to string }{
		config.LangGerman:  {"const de = {", "const en"},
		config.LangEnglish: {"const en:", "const zh"},
		config.LangChinese: {"const zh:", "const dictionaries"},
	}
	dicts := map[string]string{}
	for lang, b := range blocks {
		block, ok := dictBlock(src, b.from, b.to)
		if !ok {
			t.Fatalf("%s: cannot find the %q dictionary between %q and %q", frontendDictPath, lang, b.from, b.to)
		}
		dicts[lang] = block
	}

	// Everything the backend hands the settings screen with a key attached.
	var keys []string
	for _, p := range config.DefaultPresets(config.LangEnglish) {
		keys = append(keys, "preset."+p.ID)
	}
	for _, v := range (&App{}).GetPromptVariables() {
		if v.Key == "" {
			t.Errorf("template variable %q carries no i18n key", v.Name)
			continue
		}
		keys = append(keys, v.Key)
	}
	// And the sentences the image backend attaches to a published stable-diffusion.cpp
	// build. They travel as interface *keys* for the same reason the template variables
	// do — the backend knows the condition, the dictionary knows the language — so a key
	// missing from one dictionary is an English line in a German panel. Every platform's
	// list is walked, because a note that only exists on Windows is still a sentence a
	// Windows user reads.
	for _, platform := range [][2]string{{"linux", "amd64"}, {"windows", "amd64"}, {"darwin", "arm64"}} {
		for _, b := range images.SDBuilds(platform[0], platform[1]) {
			if b.NoteKey != "" {
				keys = append(keys, b.NoteKey)
			}
		}
	}

	if len(keys) == 0 {
		t.Fatal("no translated names to check; did the backend stop naming them?")
	}

	for _, key := range keys {
		for lang, block := range dicts {
			if !strings.Contains(block, "'"+key+"':") {
				t.Errorf("%s: no %q entry — an interface in %s would show the backend's own text for it", frontendDictPath, key, lang)
			}
		}
	}
}
