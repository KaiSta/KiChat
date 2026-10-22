package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// errorCodePattern finds the codes the backend emits. internal/uimsg is the only
// producer, so the call is what names them; a message built any other way is text,
// not a code, and is left alone by design.
var errorCodePattern = regexp.MustCompile(`uimsg\.(?:Errorf|Detailf|Wrapf)\("([^"]+)"`)

// extractionCodePattern finds the codes a caller hands to extractWithBudget. They
// cannot appear at a uimsg call site because one extraction is used for two
// operations whose wording differs, so the code is chosen by the caller and travels
// as a field. Naming the fields is narrower than accepting any string literal.
var extractionCodePattern = regexp.MustCompile(`(?:Failure|Empty|Budget)Code:\s*"([^"]+)"`)

// The other half of the dictionary check, and the reason the codes exist at all: a
// code with no sentence — or with a German sentence only — reads as `i18n:{…}` or as
// German in an English or Chinese window. Walking the tree rather than listing files
// means a new bridge file is covered the moment it emits its first code.
func TestEveryBackendErrorCodeIsTranslated(t *testing.T) {
	raw, err := os.ReadFile(frontendDictPath)
	if err != nil {
		t.Fatalf("read %s: %v", frontendDictPath, err)
	}
	src := string(raw)
	dicts := map[string]string{}
	for lang, b := range map[string][2]string{
		config.LangGerman:  {"const de = {", "const en"},
		config.LangEnglish: {"const en:", "const zh"},
		config.LangChinese: {"const zh:", "const dictionaries"},
	} {
		block, ok := dictBlock(src, b[0], b[1])
		if !ok {
			t.Fatalf("%s: cannot find the %q dictionary between %q and %q", frontendDictPath, lang, b[0], b[1])
		}
		dicts[lang] = block
	}

	codes := map[string]bool{}
	err = filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// The root `tools/` is the dev harness, but internal/tools is a package
			// that reports to the window like any other. Matching by name skipped it,
			// which is how a whole package's codes went unchecked.
			if filepath.Clean(path) == "tools" {
				return fs.SkipDir
			}
			switch entry.Name() {
			case ".git", "frontend", "scratch", "build", "packaging", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, pattern := range []*regexp.Regexp{errorCodePattern, extractionCodePattern} {
			for _, match := range pattern.FindAllStringSubmatch(string(body), -1) {
				codes[match[1]] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree for uimsg codes: %v", err)
	}
	if len(codes) == 0 {
		t.Fatal("no uimsg codes found; did the bridge stop reporting through internal/uimsg?")
	}

	for _, code := range sorted(codes) {
		key := "err." + code
		for lang, block := range dicts {
			if !strings.Contains(block, "'"+key+"':") {
				t.Errorf("%s: %q has no %s sentence — the window would show the raw code", frontendDictPath, code, lang)
			}
		}
	}

	// And the other direction: a sentence no code asks for is a leftover that will
	// never be read, and the next person to touch these strings has to work out
	// whether it matters.
	dictKeyPattern := regexp.MustCompile(`'err\.([A-Za-z0-9_.]+)':`)
	for _, match := range dictKeyPattern.FindAllStringSubmatch(dicts[config.LangGerman], -1) {
		if !codes[match[1]] {
			t.Errorf("%s: 'error.%s' is translated but no Go code emits it", frontendDictPath, match[1])
		}
	}
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
