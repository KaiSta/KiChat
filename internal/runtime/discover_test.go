package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLMStudioFoldersPrefersTheFolderItsOwnSettingNames pins the order this
// feature depends on. On the machine it was written for, `~/.lmstudio/models`
// exists and is empty while LM Studio's settings.json names a folder on another
// drive holding six chat models — so "the first folder that exists" is not the
// same question as "the first folder LM Studio would look in", and answering the
// first one reports no models on a machine full of them.
func TestLMStudioFoldersPrefersTheFolderItsOwnSettingNames(t *testing.T) {
	home := t.TempDir()
	onAnotherDrive := filepath.Join(t.TempDir(), "lmstudio")
	if err := os.MkdirAll(onAnotherDrive, 0o755); err != nil {
		t.Fatal(err)
	}
	// LM Studio's own models directory is present but empty, exactly as it is on
	// a machine whose models live elsewhere.
	if err := os.MkdirAll(filepath.Join(home, ".lmstudio", "models"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".lmstudio"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{"language":"en","downloadsFolder":"` + onAnotherDrive + `","chat":{}}`
	if err := os.WriteFile(filepath.Join(home, ".lmstudio", "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	folders := LMStudioFolders(home)
	if len(folders) == 0 {
		t.Fatal("no folders offered")
	}
	if folders[0].Path != onAnotherDrive {
		t.Fatalf("first folder is %q, want LM Studio's configured %q", folders[0].Path, onAnotherDrive)
	}
	if folders[0].Kind != FolderLMStudioConfigured || !folders[0].Exists {
		t.Fatalf("configured folder: %+v", folders[0])
	}

	byKind := map[string]ModelFolder{}
	for _, f := range folders {
		byKind[f.Kind] = f
	}
	if f, ok := byKind[FolderLMStudio]; !ok || !f.Exists {
		t.Fatalf("LM Studio's own models directory should be offered and found: %+v", byKind[FolderLMStudio])
	}
	if f, ok := byKind[FolderLMStudioLegacy]; !ok || f.Exists {
		t.Fatalf("a directory that is not there must still be listed but marked absent: %+v", f)
	}
}

// TestLMStudioFoldersSurvivesASettingsFileItCannotRead: this is another
// application's private file, so a shape this code does not expect has to cost a
// hint and not an error.
func TestLMStudioFoldersSurvivesASettingsFileItCannotRead(t *testing.T) {
	for name, body := range map[string]string{
		"not json":       "{ this is not json",
		"not a string":   `{"downloadsFolder": 42}`,
		"empty":          `{"downloadsFolder":"  "}`,
		"missing folder": `{"downloadsFolder":"/nonexistent/kichat/models"}`,
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, ".lmstudio"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".lmstudio", "settings.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			folders := LMStudioFolders(home)
			for _, f := range folders {
				if f.Kind == FolderLMStudioConfigured {
					t.Fatalf("a folder that cannot be trusted was offered anyway: %+v", f)
				}
			}
			if len(folders) == 0 {
				t.Fatal("the platform defaults must still be offered")
			}
		})
	}
}

// TestScanModelFolderFindsTheGgufsAndSkipsTheProjectors: LM Studio keeps a vision
// model's mmproj file next to it under the same extension, and llama-server refuses
// that file as a model. Offering it would present a choice that cannot work.
func TestScanModelFolderFindsTheGgufsAndSkipsTheProjectors(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, size int) string {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	want := write("lmstudio-community/Ministral-3-3B-Instruct-2512-GGUF/Ministral-3-3B-Instruct-2512-Q4_K_M.gguf", 2048)
	write("lmstudio-community/Ministral-3-3B-Instruct-2512-GGUF/mmproj-Ministral-3-3B-Instruct-2512-F16.gguf", 1024)
	write("lmstudio-community/Ministral-3-3B-Instruct-2512-GGUF/README.md", 10)
	write("lmstudio-community/Ministral-3-3B-Instruct-2512-GGUF/MMPROJ-upper.gguf", 512)
	// The other way publishers spell it: the model's name first, `mmproj` in the
	// middle. Measured on the PrismML ternary model's folder, where this shape — not
	// the `mmproj-…` one above — is what the download produces, and where a
	// prefix-only test offered a projector as a model.
	write("prism-ml/Ternary-Bonsai-2-27B-gguf/Ternary-Bonsai-2-27B-mmproj-BF16.gguf", 889)
	// A model whose name merely contains the letters is not a projector.
	mmproject := write("vendor/mmproject-7B-GGUF/mmproject-7B-Q4_K_M.gguf", 256)
	deep := write("empero-ai/Qwythos-9B-GGUF/Qwythos-9B-Q4_K_M.gguf", 4096)

	scan, err := ScanModelFolder(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, f := range scan.Files {
		got[f.Path] = f.Size
	}
	if len(got) != 3 {
		t.Fatalf("found %d files, want 3: %+v", len(got), scan.Files)
	}
	if got[want] != 2048 || got[deep] != 4096 || got[mmproject] != 256 {
		t.Fatalf("sizes wrong: %+v", scan.Files)
	}
	for _, f := range scan.Files {
		if isProjector(f.Name) {
			t.Errorf("a projector was offered as a model: %+v", f)
		}
	}

	byPath := map[string]ModelFile{}
	for _, f := range scan.Files {
		byPath[f.Path] = f
	}
	if m := byPath[want].Model; m != "lmstudio-community/Ministral-3-3B-Instruct-2512-GGUF" {
		t.Fatalf("model label is %q", m)
	}
	if scan.Truncated {
		t.Fatal("nothing was truncated")
	}
	// Sorted by model, then by name: two quants of one model stay together and the
	// list does not reshuffle between scans.
	if !strings.HasSuffix(scan.Files[0].Path, "Ministral") && scan.Files[0].Model > scan.Files[1].Model {
		t.Fatalf("not sorted: %q then %q", scan.Files[0].Model, scan.Files[1].Model)
	}
}

// TestScanModelFolderSkipsHiddenDirectories: LM Studio's housekeeping lives in
// hidden folders (`.internal`, `.cache`), and a walk that descends into them
// reports engines and bundled models as if the user had chosen them. A hidden
// folder named as the root is still scanned — that is how the bundled models are
// offered deliberately.
func TestScanModelFolderSkipsHiddenDirectories(t *testing.T) {
	root := t.TempDir()
	hidden := filepath.Join(root, ".internal", "bundled-models", "nomic")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "nomic.Q4_K_M.gguf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	scan, err := ScanModelFolder(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Files) != 0 {
		t.Fatalf("a hidden folder was descended into: %+v", scan.Files)
	}

	scan, err = ScanModelFolder(filepath.Join(root, ".internal", "bundled-models"))
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Files) != 1 {
		t.Fatalf("naming a hidden folder as the root must still list it: %+v", scan.Files)
	}
}

// TestScanModelFolderStopsAtItsLimits: a folder can be a drive root or a network
// share, so the walk reports that it gave up rather than sitting there.
func TestScanModelFolderStopsAtItsLimits(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "publisher", "model")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < ModelScanMaxFiles+5; i++ {
		name := filepath.Join(dir, "m-"+strings.Repeat("0", 4-len(itoa(i)))+itoa(i)+".gguf")
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	scan, err := ScanModelFolder(root)
	if err != nil {
		t.Fatal(err)
	}
	if !scan.Truncated {
		t.Fatal("the limit was reached but the scan does not say so")
	}
	if len(scan.Files) > ModelScanMaxFiles {
		t.Fatalf("listed %d files, over the limit of %d", len(scan.Files), ModelScanMaxFiles)
	}
}

// TestScanModelFolderDepthLimit: LM Studio nests two or three levels; more than
// that is another application's tree, not a models folder.
func TestScanModelFolderDepthLimit(t *testing.T) {
	root := t.TempDir()
	shallow := filepath.Join(root, "a", "b", "c", "deep.gguf")
	nested := root
	for _, part := range []string{"a", "b", "c"} {
		nested = filepath.Join(nested, part)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shallow, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tooDeep := root
	for i := 0; i <= ModelScanMaxDepth; i++ {
		tooDeep = filepath.Join(tooDeep, "d")
	}
	if err := os.MkdirAll(tooDeep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tooDeep, "buried.gguf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	scan, err := ScanModelFolder(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Files) != 1 {
		t.Fatalf("want only the file within the depth limit: %+v", scan.Files)
	}
}

// TestScanModelFolderOnSomethingThatIsNotThere reports the failure instead of an
// empty list, which the panel has to tell apart: "nothing here" and "that folder
// is not there" are different answers.
func TestScanModelFolderOnSomethingThatIsNotThere(t *testing.T) {
	if _, err := ScanModelFolder(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("a missing folder must be an error")
	}
	scan, err := ScanModelFolder("")
	if err != nil || len(scan.Files) != 0 {
		t.Fatalf("an empty folder is an empty answer, not a failure: %v %+v", err, scan)
	}
}

// TestMatchProjectorPairsAModelWithItsOwnProjector: a vision model is two files, and a
// server started with only the first refuses every image ("image input is not
// supported"), so the app looks for the projector itself — but only where the folder
// answers the question. The shapes below are the ones measured on this machine's LM
// Studio folder: `mmproj-<model>-F16.gguf` beside `<model>-Q4_K_M.gguf`, and the Hugging
// Face spelling `Ternary-Bonsai-2-27B-mmproj-BF16.gguf` the ternary download ships.
//
// The last cases are the ones that matter as much as the matches: a tie and a folder
// with nothing in common both answer *nothing*, because a silent pick from an arbitrary
// sort is how a model ends up loading another model's vision tower.
func TestMatchProjectorPairsAModelWithItsOwnProjector(t *testing.T) {
	cases := []struct {
		name   string
		files  []string
		model  string
		want   string
		reason string
	}{
		{
			name:   "one projector beside its model",
			files:  []string{"mmproj-Qwen3-VL-8B-Instruct-F16.gguf"},
			model:  "Qwen3-VL-8B-Instruct-Q4_K_M.gguf",
			want:   "mmproj-Qwen3-VL-8B-Instruct-F16.gguf",
			reason: "the ordinary LM Studio shape",
		},
		{
			name: "two models' projectors in one folder",
			files: []string{
				"mmproj-Qwen3-VL-4B-Instruct-F16.gguf",
				"mmproj-Qwen3-VL-8B-Instruct-F16.gguf",
			},
			model:  "Qwen3-VL-8B-Instruct-Q4_K_M.gguf",
			want:   "mmproj-Qwen3-VL-8B-Instruct-F16.gguf",
			reason: "the 8B is the model's own; the 4B sibling's shares fewer tokens",
		},
		{
			name:   "the model's name first, mmproj in the middle",
			files:  []string{"Ternary-Bonsai-2-27B-mmproj-BF16.gguf"},
			model:  "Ternary-Bonsai-2-27B-PQ2_0.gguf",
			want:   "Ternary-Bonsai-2-27B-mmproj-BF16.gguf",
			reason: "the Hugging Face spelling, whose quant differs from the model's by design",
		},
		{
			name:   "a generically named projector",
			files:  []string{"mmproj.gguf"},
			model:  "Some-Model-Q4_K_M.gguf",
			want:   "mmproj.gguf",
			reason: "one candidate is unambiguous by position alone, whatever it is called",
		},
		{
			name: "two quants of one projector",
			files: []string{
				"mmproj-Qwen3-VL-8B-Instruct-BF16.gguf",
				"mmproj-Qwen3-VL-8B-Instruct-F16.gguf",
			},
			model:  "Qwen3-VL-8B-Instruct-Q4_K_M.gguf",
			want:   "",
			reason: "a tie is the user's choice, not the sort order's",
		},
		{
			name:   "an unrelated projector",
			files:  []string{"mmproj-Llama-3-8B-F16.gguf"},
			model:  "Qwen3-VL-8B-Instruct-Q4_K_M.gguf",
			want:   "",
			reason: "nothing in common is no match, not the closest one",
		},
		{
			name:   "no projector at all",
			files:  []string{"Qwen3-VL-8B-Instruct-Q4_K_M.gguf"},
			model:  "Qwen3-VL-8B-Instruct-Q4_K_M.gguf",
			want:   "",
			reason: "a text-only folder has to answer nothing",
		},
		{
			name:   "a model whose name merely contains the letters",
			files:  []string{"mmproject-7B-Q4_K_M.gguf"},
			model:  "Qwen3-VL-8B-Instruct-Q4_K_M.gguf",
			want:   "",
			reason: "`mmproject` is not `mmproj`, the same rule the model list uses",
		},
		{
			name:   "the extension in capitals, on the projector",
			files:  []string{"mmproj-Qwen3-VL-8B-Instruct-F16.GGUF"},
			model:  "Qwen3-VL-8B-Instruct-Q4_K_M.gguf",
			want:   "mmproj-Qwen3-VL-8B-Instruct-F16.GGUF",
			reason: "the two halves are not always published by the same hand, and `.GGUF` is not a model token",
		},
		{
			name:   "the extension in capitals, on the model",
			files:  []string{"mmproj-Qwen3-VL-8B-Instruct-F16.gguf"},
			model:  "Qwen3-VL-8B-Instruct-Q4_K_M.GGUF",
			want:   "mmproj-Qwen3-VL-8B-Instruct-F16.gguf",
			reason: "the same asymmetry is harmless in this direction, and has to stay that way",
		},
		{
			name: "the model's name written with dots",
			files: []string{
				"mmproj-Qwen3-VL-8B-Instruct-F16.gguf",
			},
			model:  "Qwen3-VL-8B-Instruct.Q4_K_M.gguf",
			want:   "mmproj-Qwen3-VL-8B-Instruct-F16.gguf",
			reason: "the separators differ between two spellings of one name, and neither separator is a token",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("GGUF"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			model := filepath.Join(dir, tc.model)
			want := ""
			if tc.want != "" {
				want = filepath.Join(dir, tc.want)
			}
			if got := MatchProjector(model); got != want {
				t.Errorf("MatchProjector = %q, want %q — %s", got, want, tc.reason)
			}
		})
	}

	// A model that is not there, and no path at all, are answered rather than read:
	// the status row asks this for every configured model, including a path the user
	// typed and has not saved yet.
	if got := MatchProjector(""); got != "" {
		t.Errorf("MatchProjector(\"\") = %q, want empty", got)
	}
	if got := MatchProjector(filepath.Join(t.TempDir(), "nope", "a.gguf")); got != "" {
		t.Errorf("MatchProjector of an absent folder = %q, want empty", got)
	}
}

// TestProjectorsBesideNeedsAFolderOfItsOwn: the search is "the folder this model is
// in", so a path that names no folder has to be answered with nothing rather than read.
// A bare file name resolves against the process's own working directory — wherever the
// app happened to be started from — which is not the model's folder and can hold a
// projector of something else entirely. The working directory is moved to a folder that
// *does* hold a matching projector, so a version that read it would be caught pairing a
// file that has nothing to do with the model named.
func TestProjectorsBesideNeedsAFolderOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"mmproj-Some-Model-F16.gguf", "Some-Model-Q4_K_M.gguf"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("GGUF"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)

	if got := MatchProjector("Some-Model-Q4_K_M.gguf"); got != "" {
		t.Errorf("a bare file name was paired with %q out of the working directory", got)
	}
	if got := ProjectorsBeside("Some-Model-Q4_K_M.gguf"); len(got) != 0 {
		t.Errorf("ProjectorsBeside listed %q for a path with no folder", got)
	}
	// The same file, named by its folder, is paired — the guard is about the shape of
	// the path and not about refusing to answer.
	if got := MatchProjector(filepath.Join(dir, "Some-Model-Q4_K_M.gguf")); got != filepath.Join(dir, "mmproj-Some-Model-F16.gguf") {
		t.Errorf("MatchProjector of the same file by absolute path = %q, want the projector beside it", got)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
