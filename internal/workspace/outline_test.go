package workspace

// The outline, and the one thing it must not do: pretend.
//
// Every test here is about the shape of the answer rather than about a parser being complete.
// A declaration list is worth 1 500 tokens instead of 24 000 only if the lines it names are the
// lines a read will land on, so the assertions are on line numbers and on the code that must
// *not* be in the list.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// outlineRoot writes one file into a fresh folder and opens it as a project.
func outlineRoot(t *testing.T, name, body string) Root {
	t.Helper()
	folder := t.TempDir()
	path := filepath.Join(folder, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := Open(folder)
	if err != nil {
		t.Fatalf("open %s: %v", folder, err)
	}
	return root
}

// TestOutlineListsGoDeclarationsWithTheirLines is the whole feature in one file: the functions
// and types are named, the comment that mentions one is not a declaration, and a line inside a
// body is not either.
func TestOutlineListsGoDeclarationsWithTheirLines(t *testing.T) {
	root := outlineRoot(t, "demo.go", `package demo

// func commented() — ein Beispiel im Kommentar
var (
	errOne = errors.New("eins")
)

type Thing struct {
	Name string
}

func (t *Thing) Method() {
	if true {
		return
	}
}

func Plain() {}
`)

	outline, err := root.Outline("demo.go")
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	if !outline.Complete || outline.Total != 18 {
		t.Errorf("Complete=%v Total=%d, want a complete scan of 18 lines", outline.Complete, outline.Total)
	}
	want := []Decl{
		{Line: 4, Text: "var ("},
		{Line: 8, Text: "type Thing struct {"},
		{Line: 12, Text: "func (t *Thing) Method() {"},
		{Line: 18, Text: "func Plain() {}"},
	}
	if len(outline.Decls) != len(want) {
		t.Fatalf("declarations = %+v, want %+v", outline.Decls, want)
	}
	for i, decl := range outline.Decls {
		if decl != want[i] {
			t.Errorf("declaration %d = %+v, want %+v", i, decl, want[i])
		}
	}
}

// TestOutlineKeepsHeadingsOutOfCodeFences is why markdown is a family rather than the generic
// rules: `#` is a heading at the top and a comment inside a sample, and an outline that listed
// the sample would send the model to the wrong line.
func TestOutlineKeepsHeadingsOutOfCodeFences(t *testing.T) {
	root := outlineRoot(t, "notes.md", "# Titel\n\n```go\n# kein Titel\n```\n\n## Abschnitt\n")

	outline, err := root.Outline("notes.md")
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	if len(outline.Decls) != 2 || outline.Decls[0].Line != 1 || outline.Decls[1].Line != 7 {
		t.Fatalf("headings = %+v, want lines 1 and 7", outline.Decls)
	}
}

// TestOutlineOfADataFileIsEmptyAndSaysSo is the honest half: JSON has no declaration list worth
// inventing, so the answer is nothing at all — the tool's sentence then points at the window
// instead of handing over a list of `type:` keys.
func TestOutlineOfADataFileIsEmptyAndSaysSo(t *testing.T) {
	root := outlineRoot(t, "data.json", "{\n  \"type\": \"thing\",\n  \"class\": \"x\"\n}\n")

	outline, err := root.Outline("data.json")
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	if len(outline.Decls) != 0 {
		t.Errorf("a JSON file produced declarations: %+v", outline.Decls)
	}
	if !outline.Complete || outline.Total != 4 {
		t.Errorf("Complete=%v Total=%d, want a complete scan of 4 lines", outline.Complete, outline.Total)
	}
}

// TestOutlineRefusesABinary keeps one refusal from the read tool: a compiled object or a picture
// is not a file whose declarations can be listed as text.
func TestOutlineRefusesABinary(t *testing.T) {
	root := outlineRoot(t, "blob.bin", "func A() {}\x00binary")

	outline, err := root.Outline("blob.bin")
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	if !outline.Binary {
		t.Error("a file with a NUL byte was not reported as binary")
	}
}

// TestOutlineCutsItsListAndCountsTheRest keeps a generated file from spending the window on a
// list nobody reads, and says how much of it was left out.
func TestOutlineCutsItsListAndCountsTheRest(t *testing.T) {
	var body strings.Builder
	body.WriteString("package demo\n")
	for i := 0; i <= MaxOutlineDecls; i++ {
		body.WriteString("func F" + string(rune('a'+i%26)) + "() {}\n")
	}
	root := outlineRoot(t, "many.go", body.String())

	outline, err := root.Outline("many.go")
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	if len(outline.Decls) != MaxOutlineDecls {
		t.Errorf("declarations = %d, want the cap %d", len(outline.Decls), MaxOutlineDecls)
	}
	if outline.Omitted != 1 {
		t.Errorf("Omitted = %d, want 1", outline.Omitted)
	}
}

// TestOutlineReadsEachLanguageFamily is what makes "it works for more than Go" a claim the
// repository checks rather than one the docs make. One file per family, and the exact lines it
// lists: the declaration it must catch, the member it must keep indented, and — for YAML and SQL
// — the empty answer, which is the family list refusing to invent one.
func TestOutlineReadsEachLanguageFamily(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []int
	}{
		{
			name: "a.ts",
			body: "import { x } from './x'\n\nexport interface Point {\n  x: number\n}\n\n" +
				"export type Id = string\n\nexport function distance(a: Point, b: Point): number {\n" +
				"  const inner = (p: Point) => p.x\n  return inner(a) - inner(b)\n}\n\n" +
				"export class Shape {\n  private name: string\n\n  area(): number {\n    return 0\n  }\n}\n\n" +
				"const handler = async (req: unknown) => {\n  return req\n}\n\nexport default Shape\n",
			// A class method at an indent is not listed; the class is, and so is a top-level const.
			want: []int{3, 7, 9, 14, 22},
		},
		{
			name: "b.py",
			body: "import os\n\nCONSTANT = 3\n\n@dataclass\nclass Store:\n    def __init__(self, path):\n" +
				"        self.path = path\n\n    def read(self, name):\n        return open(name).read()\n\n" +
				"def top_level(a, b):\n    def nested():\n        return 1\n    return a + b\n\n" +
				"async def fetch(url):\n    return url\n",
			// Methods and a nested function are listed, because their indentation is carried.
			want: []int{6, 7, 10, 13, 14, 18},
		},
		{
			name: "c.rs",
			body: "use std::fmt;\n\npub struct Config {\n    pub path: String,\n}\n\nimpl Config {\n" +
				"    pub fn new(path: &str) -> Self {\n        Self { path: path.into() }\n    }\n\n" +
				"    fn helper(&self) {}\n}\n\nfn main() {\n    println!(\"hi\");\n}\n\n" +
				"pub(crate) const LIMIT: usize = 10;\n",
			// The methods inside `impl`, which a rule anchored at column 0 missed entirely.
			want: []int{3, 7, 8, 12, 15, 19},
		},
		{
			name: "d.c",
			body: "#include <stdio.h>\n\nstruct point {\n    int x;\n    int y;\n};\n\n" +
				"static int helper(int a) {\n    return a * 2;\n}\n\nint main(void) {\n" +
				"    printf(\"%d\\n\", helper(1));\n    return 0;\n}\n",
			// The third rule: `int main(void) {` has no keyword to anchor on, so it is caught by
			// its shape — a return type, a name, arguments and a brace.
			want: []int{3, 8, 12},
		},
		{
			name: "e.java",
			body: "package demo;\n\npublic class Main {\n    private int count;\n\n" +
				"    public static void main(String[] args) {\n        System.out.println(\"hi\");\n    }\n\n" +
				"    private int count() {\n        return count;\n    }\n}\n",
			want: []int{3, 4, 6, 10},
		},
		{
			name: "f.sh",
			body: "#!/bin/bash\nset -euo pipefail\n\nbuild() {\n  go build ./...\n}\n\nrun() {\n  ./kichat \"$@\"\n}\n\nbuild\n",
			want: []int{4, 8},
		},
		{
			name: "g.md",
			body: "# Title\n\nSome prose.\n\n```go\n# not a heading\nfunc example() {}\n```\n\n## Second\n\n### Third\n",
			want: []int{1, 10, 12},
		},
		// A record per line is not a declaration, so the empty answer stays the right one for
		// data even now that other unknown files get the generic fallback.
		{name: "i.sql", body: "CREATE TABLE chats (id TEXT PRIMARY KEY);\nSELECT * FROM chats;\n", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := outlineRoot(t, tc.name, tc.body)
			outline, err := root.Outline(tc.name)
			if err != nil {
				t.Fatalf("outline: %v", err)
			}
			var got []int
			for _, decl := range outline.Decls {
				got = append(got, decl.Line)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("lines %v, want %v (from %+v)", got, tc.want, outline.Decls)
			}
			for i, line := range tc.want {
				if got[i] != line {
					t.Errorf("line %d = %d, want %d (from %+v)", i, got[i], line, outline.Decls)
				}
			}
		})
	}
}

// TestOutlineFallsBackForUnrecognisedFiles pins the generic answer and the three files whose
// *name* is their format. Before this, a Makefile, a Dockerfile and a go.mod all got the "no
// declarations recognised" sentence — true of the scanner and useless to a reader, since each
// of them has an obvious structure.
func TestOutlineFallsBackForUnrecognisedFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []int
	}{
		{
			name: "Makefile",
			body: "# comment\nGO ?= go\nbuild: clean\n\t$(GO) build ./...\ntest: build\n" +
				"\t$(GO) test ./...\n.PHONY: build test\n",
			// A recipe line starts with a tab, so three of the seven lines are structure.
			want: []int{2, 3, 5, 7},
		},
		{
			name: "Dockerfile",
			body: "FROM golang:1.27 AS build\n\n# comment\nRUN go build ./... \\\n    && echo done\n" +
				"COPY --from=build /out /app\nENTRYPOINT [\"/app/kichat\"]\n",
			want: []int{1, 4, 6, 7},
		},
		{
			name: "go.mod",
			body: "module kichat\n\ngo 1.27.0\n\nrequire (\n\tgithub.com/x/y v1.0.0\n)\n\n" +
				"replace foo => bar\n",
			// The directives, not the requirement list they open.
			want: []int{1, 3, 5, 9},
		},
		{name: "app.yaml", body: "type: deployment\nspec:\n  replicas: 3\n", want: []int{1, 2}},
		{
			name: "app.ini",
			body: "[server]\nhost = 0.0.0.0\nport = 8080\n\n[db]\npassword = hunter2\n",
			want: []int{1, 2, 3, 5, 6},
		},
		{name: "data.json", body: "{\"name\": \"x\"}\n", want: nil},
		{name: "build.log", body: "INFO: start\nERROR: boom\n", want: nil},
		// Generated files: their keys are a list of dependencies the manifest already has.
		{name: "Cargo.lock", body: "[[package]]\nname = \"serde\"\nversion = \"1.0\"\n", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := outlineRoot(t, tc.name, tc.body)
			outline, err := root.Outline(tc.name)
			if err != nil {
				t.Fatalf("outline: %v", err)
			}
			var got []int
			for _, decl := range outline.Decls {
				got = append(got, decl.Line)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("lines %v, want %v (from %+v)", got, tc.want, outline.Decls)
			}
			for i, line := range tc.want {
				if got[i] != line {
					t.Errorf("line %d = %d, want %d (from %+v)", i, got[i], line, outline.Decls)
				}
			}
		})
	}
}

// TestAGenericOutlineKeepsTheKeyAndNotTheValue is the security half of the fallback: it runs for
// files this app does not recognise, which is where a password or a token is kept, and the list
// is copied into the model's context. `password = hunter2` must arrive as `password =`.
func TestAGenericOutlineKeepsTheKeyAndNotTheValue(t *testing.T) {
	root := outlineRoot(t, ".env", "POSTGRES_PASSWORD=hunter2\nOPENAI_API_KEY=sk-live-123\nDEBUG=true\n")

	outline, err := root.Outline(".env")
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	if len(outline.Decls) != 3 {
		t.Fatalf("declarations = %+v, want the three keys", outline.Decls)
	}
	want := []string{"POSTGRES_PASSWORD=", "OPENAI_API_KEY=", "DEBUG="}
	for i, decl := range outline.Decls {
		if decl.Text != want[i] {
			t.Errorf("declaration %d = %q, want %q", i, decl.Text, want[i])
		}
	}
}

// TestOutlineCatchesCFunctions is the rule for C and C++ in both directions, because a
// false positive here is worse than a miss: an outline that lists `if (x) {` as a declaration
// sends the model to a line that is not a declaration, while a missed definition only costs the
// window it would have asked for anyway.
func TestOutlineCatchesCFunctions(t *testing.T) {
	file := "int main(void) {\n" +
		"void *alloc(size_t n) {\n" +
		"MyType Foo::bar(int x) {\n" +
		"char **split(const char *s, char c) {\n" +
		"static int helper(int a) {\n" +
		"Foo::Foo(int x) : a(x), b(0) {\n" +
		"bool operator==(const Foo &o) const {\n" +
		"auto compute(int x) -> int {\n" +
		"template <typename T> T max(T a, T b) {\n" +
		"public static void Main(string[] args) {\n" +
		"    int inner(int y) {\n" +
		"if (x) {\n" +
		"for (int i = 0; i < n; i++) {\n" +
		"while (n > 0) {\n" +
		"switch (x) {\n" +
		"} else if (y) {\n" +
		"else if (y) {\n" +
		"do {\n" +
		"auto f = [](int x) {\n" +
		"case 1: {\n" +
		"#define FOO(x) {\n" +
		"typedef struct {\n" +
		"x = a < b;\n" +
		"catch (const std::exception &e) {\n" +
		"foreach (var x in xs) {\n" +
		"if constexpr (x) {\n" +
		"return std::sort(a, b, [](int x, int y) {\n" +
		"int main(void)\n" +
		"} while (x);\n" +
		"printf(\"%d\\n\", x);\n"
	root := outlineRoot(t, "sample.c", file)

	outline, err := root.Outline("sample.c")
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	var got []int
	for _, decl := range outline.Decls {
		got = append(got, decl.Line)
	}
	want := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	if len(got) != len(want) {
		t.Fatalf("lines %v, want %v (from %+v)", got, want, outline.Decls)
	}
	for i, line := range want {
		if got[i] != line {
			t.Errorf("line %d = %d, want %d (from %+v)", i, got[i], line, outline.Decls)
		}
	}
}

// TestOutlineRefusesAFolder pins the same answer Read gives, so a model that asks for the
// outline of a directory is corrected rather than handed an empty list.
func TestOutlineRefusesAFolder(t *testing.T) {
	root := outlineRoot(t, "pkg/a.go", "package pkg\n")

	if _, err := root.Outline("pkg"); err == nil {
		t.Fatal("outline of a folder succeeded")
	}
}
