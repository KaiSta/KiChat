package workspace

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// An outline is a cheap map of a file: every declaration this app can recognise, with the line
// it starts on, and none of the bodies.
//
// It exists because a coding chat has three expensive ways to learn the shape of a 1 900-line
// file and none of them is both cheap and exact: reading it whole (the default 500-line window
// is ~5 000 tokens, the file ~24 000), reading a window blind (cheap, but the model has no idea
// where anything is), or a second model's summary (600 tokens, and a paraphrase of the line
// numbers the next `edit_file` will depend on). An outline of `app.go` is ~1 500 tokens, exact,
// and it names the window to ask for next.
//
// It is a line scanner, not a parser. That is a deliberate trade: a parser per language is a
// dependency and a set of failure modes, while the question this answers — "where is the thing
// I want" — survives an occasional miss. What it cannot do it says: a file with no recognised
// declarations gets an empty list and a sentence pointing at the window, rather than a list of
// guesses.
const (
	// MaxOutlineBytes bounds the scan. A file can be any size, and an outline of the first
	// few megabytes is still an outline; past this the list is labelled as partial, which is
	// why the tool reads the bound rather than restating it.
	MaxOutlineBytes = 4 << 20
	// MaxOutlineDecls is how many declarations one outline lists. A generated file with ten
	// thousand entries is not a map any more, and the list says how many it left out.
	MaxOutlineDecls = 400
	// maxDeclText is how much of a declaration line travels. A signature is a name and its
	// arguments; a 300-character generic type is not read, it is skipped.
	maxDeclText = 150
)

// Outline is what one file's declarations look like.
type Outline struct {
	// Path is the workspace-relative path, the form the model asked in.
	Path string
	// Decls are the declarations found, in file order.
	Decls []Decl
	// Omitted is how many declarations were found after the list was cut at MaxOutlineDecls.
	Omitted int
	// Total is the file's length in lines, known only when the whole file was scanned: a
	// partial scan leaves it at 0 rather than at the count it stopped at, because a lower
	// bound printed as a length is worse than no number at all. The same rule Read follows.
	Total int
	// Complete is "the whole file was scanned". A file past MaxOutlineBytes is not.
	Complete bool
	// Binary is a file no model should be reading at all.
	Binary bool
}

// Decl is one declaration: the line it starts on, and its first line of text.
type Decl struct {
	Line int
	Text string
}

// Outline lists the declarations in a text file.
//
// The whole file is read here even though the result is small, and that is the point of the
// feature: the cost this saves is the model's context, not this process's memory.
func (r Root) Outline(rel string) (Outline, error) {
	abs, err := r.Resolve(rel)
	if err != nil {
		return Outline{}, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return Outline{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Outline{}, err
	}
	if info.IsDir() {
		return Outline{}, fmt.Errorf("%s is a folder, not a file", r.Relative(abs))
	}

	out := Outline{Path: r.Relative(abs)}
	// The cap is on the reader rather than on the loop so a single line larger than the whole
	// window — a minified bundle — cannot be buffered past it.
	limited := &io.LimitedReader{R: f, N: MaxOutlineBytes}
	reader := bufio.NewReaderSize(limited, 64<<10)
	if sniff, err := reader.Peek(maxSniffBytes); err == nil || errors.Is(err, io.EOF) {
		if isBinary(sniff) {
			out.Binary = true
			return out, nil
		}
	}

	syntax := syntaxFor(filepath.Base(abs))
	line, inFence := 0, false
	for {
		raw, readErr := reader.ReadString('\n')
		if raw == "" && readErr != nil {
			break
		}
		line++
		text := strings.TrimRight(raw, "\r\n")
		if syntax.fenced && (strings.HasPrefix(text, "```") || strings.HasPrefix(text, "~~~")) {
			// A heading inside a fenced block is an example of a heading, not a section of
			// this document.
			inFence = !inFence
			if readErr != nil {
				break
			}
			continue
		}
		if inFence {
			if readErr != nil {
				break
			}
			continue
		}
		if syntax.matches(text) {
			if len(out.Decls) < MaxOutlineDecls {
				shown := text
				if syntax.cutValue {
					shown = keyOf(text)
				}
				// The leading whitespace is kept: in Python or Rust an indented declaration is
				// a member of the block above it, and a list that printed it flush left would
				// say a method was module-level. Trailing spaces carry nothing.
				out.Decls = append(out.Decls, Decl{Line: line, Text: shorten(strings.TrimRight(shown, " \t"))})
			} else {
				out.Omitted++
			}
		}
		if readErr != nil {
			break
		}
	}
	// A file that ended cleanly reports its length; one the cap cut reports none, and the
	// caller says the outline is partial rather than pretending the file is this long.
	if limited.N > 0 {
		out.Complete = true
		out.Total = line
	}
	return out, nil
}

// shorten keeps a declaration to a length a reader takes in, marking that it was cut rather
// than ending a signature mid-argument with no sign of it.
func shorten(text string) string {
	runes := []rune(text)
	if len(runes) <= maxDeclText {
		return text
	}
	return string(runes[:maxDeclText]) + "…"
}

// declSyntax is what one language family's declarations look like to a line scanner.
type declSyntax struct {
	// rules are matched against a line with its line ending removed. Every one of them is
	// anchored at the start of the line, so a declaration *mentioned* in a sentence — a prose
	// line that says `func Foo` — is not listed as one.
	rules []*regexp.Regexp
	// signature is the C-family definition shape, which needs the text before the argument
	// list judged in code: the difference between `int main(void) {` and `if (x) {` lives
	// there, and Go's regexp has no lookahead to express it (cSignaturePrefix).
	signature *regexp.Regexp
	// cutValue is "list the key, not what is beside it" — the generic fallback's second half,
	// because an unrecognised file is where a password or an API key sits and this text is
	// copied into the model's context.
	cutValue bool
	// fenced is "``` opens a block whose lines are examples". Markdown only, where `#` is
	// both a heading and, inside a code sample, a comment that would otherwise be listed.
	fenced bool
}

// matches reports whether a line starts a declaration.
func (s declSyntax) matches(line string) bool {
	for _, rule := range s.rules {
		if rule.MatchString(line) {
			return true
		}
	}
	if s.signature != nil {
		if parts := s.signature.FindStringSubmatch(line); parts != nil {
			return cSignaturePrefix(parts[1])
		}
	}
	return false
}

// cSignature matches the tail of a C-family definition: an argument list, then the qualifiers
// and initialiser list that may follow it, ending in an opening brace.
//
// The part before the arguments is captured rather than described, because the pattern that
// separates `int main(void) {` from `if (x) {` is about *that* text and RE2 has no lookahead.
var cSignature = regexp.MustCompile(
	`^\s*(.+)\(([^;{}]*)\)\s*` +
		`(?:const\s*)?(?:noexcept\s*)?(?:override\s*)?(?:final\s*)?` +
		`(?::[^;{}]*)?` +
		`(?:->\s*[\w:<>,\s\*&]+)?` +
		`\{\s*$`)

// cSignaturePrefix reports whether the text before a parameter list is a return type and a
// name rather than the head of a statement.
//
// Five decisions, each of which is a line in the corpus of TestOutlineCatchesCFunctions:
//
//   - **A statement has no return type.** `if`, `while`, `printf` and `foreach` arrive as the
//     name alone, so a prefix with neither a space nor a `::` is refused. That one test is what
//     makes `if (x) {` and `for (int i = 0; …) {` impossible to list.
//   - **A definition starts with a letter.** `} else if (y) {` and `#define FOO(x) {` begin with
//     punctuation, and a preprocessor line is never a definition.
//   - **`=` means an assignment**, which is what a lambda `auto f = [](int x) {` and a closure
//     are; the parenthesised thing after it is the value, not a signature.
//   - **`operator` is stripped first**, so `bool operator==(…)` and `void *operator new(…)` are
//     not mistaken for assignments by the rule above.
//   - **A short list of keywords is refused anyway**, for the shapes the first rule cannot see:
//     `else if (y) {` has a space, and `struct point {` is a declaration of another kind (the
//     keyword rules already list it).
func cSignaturePrefix(prefix string) bool {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || !isLetter(prefix[0]) {
		return false
	}
	if i := strings.Index(prefix, "operator"); i >= 0 {
		prefix = prefix[:i] + "operator"
	}
	if !strings.Contains(prefix, " ") && !strings.Contains(prefix, "::") {
		return false
	}
	if strings.Contains(prefix, "=") {
		return false
	}
	first := prefix
	if i := strings.IndexAny(prefix, " \t"); i >= 0 {
		first = prefix[:i]
	}
	switch first {
	case "if", "for", "while", "switch", "else", "do", "case", "default", "return",
		"goto", "break", "continue", "typedef", "using", "namespace", "struct", "class",
		"enum", "union", "try", "catch", "delete", "new", "sizeof", "static_assert":
		return false
	}
	return true
}

func isLetter(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// keyOf keeps the name in front of the first separator and drops the rest: `type: deployment`
// becomes `type:`, `POSTGRES_PASSWORD=hunter2` becomes `POSTGRES_PASSWORD=`, and a Makefile's
// `SHELL := /bin/bash` becomes `SHELL :=`.
//
// The separator stays because `type:` reads as a key while `type` reads as a word, and the
// value goes because this is shown for files whose value is not structure — the generic
// fallback, which is where secrets are kept, and a Makefile, whose lines otherwise list a
// dozen `$(shell …)` invocations instead of its targets. The compound operators (`:=`, `?=`,
// `+=`, `::=`) are kept whole, because `SHELL :` reads as a truncated typo.
func keyOf(text string) string {
	text = strings.TrimSpace(text)
	i := strings.IndexAny(text, ":=")
	if i < 0 {
		return text
	}
	end := i + 1
	if end < len(text) && text[end] == '=' {
		end++
	}
	if i > 0 && (text[i-1] == '?' || text[i-1] == '+' || text[i-1] == ':') {
		i--
	}
	return strings.TrimRight(text[:end], " \t")
}

// syntaxFor picks the rules for one file: by its name for the files whose name *is* the
// format, then by its extension, and otherwise the generic fallback below.
//
// The name comes first because `Makefile`, `Dockerfile` and `go.mod` have no extension to
// read — and because they are the three files a coding chat asks about that used to get an
// empty answer for no reason at all.
func syntaxFor(name string) declSyntax {
	lower := strings.ToLower(name)
	switch {
	case lower == "makefile", lower == "gnu makefile", strings.HasSuffix(lower, ".mk"),
		strings.HasSuffix(lower, ".make"):
		// A target (`all: build test`) or a variable (`GO ?= go`). A recipe line starts with a
		// tab, so it cannot match either rule — which is what keeps a 200-line Makefile's
		// outline to its dozen real entry points.
		//
		// The character after the separator is spelled out rather than excluded with a
		// lookahead, which Go's regexp does not have: `[^=]` is the same "not another `=`"
		// the generic rules use, and it keeps `VAR := x` from reading as a target.
		//
		// The values stay, unlike the generic fallback's: a target's prerequisites are what
		// the graph is made of, and `windows: TARGETOS := windows` and `windows: build` are
		// different declarations of the same target — which a cut to `windows:` would print
		// three times as if it were a bug. Measured on this checkout's 673-line Makefile,
		// 14 709 tokens whole against 1 498 for its 91 lines.
		return declSyntax{rules: []*regexp.Regexp{
			regexp.MustCompile(`^[A-Za-z0-9_.%$()/-]+\s*:(?:[^=]|$)`),
			regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\s*[:?+]?=(?:[^=]|$)`),
		}}
	case lower == "dockerfile", lower == "containerfile", strings.HasSuffix(lower, ".dockerfile"):
		// The instruction keywords, which are the whole structure a Dockerfile has. A
		// continuation line begins with `&&` or an indent, and a comment with `#`.
		return declSyntax{rules: []*regexp.Regexp{
			regexp.MustCompile(`(?i)^(from|arg|env|label|expose|user|workdir|copy|add|run|volume|entrypoint|cmd|healthcheck|shell|stopsignal|onbuild)\b`),
		}}
	case lower == "go.mod", lower == "go.work":
		// The directives, not their contents: a 60-line go.mod becomes module, go and the
		// three block headers, which is what a reader navigates by. The requirement lines
		// inside `require (` are indented and therefore skipped.
		return declSyntax{rules: []*regexp.Regexp{
			regexp.MustCompile(`^(module|go|toolchain|require|replace|exclude|retract|use)\b`),
		}}
	}

	switch strings.ToLower(filepath.Ext(name)) {
	// A record or a pattern per line: an outline of one is the file. Keeping the empty answer
	// for these is what stops a `.log` from listing every `INFO:` in it — and a lockfile from
	// listing four hundred package names that the manifest above it already has.
	case ".json", ".jsonl", ".ndjson", ".csv", ".tsv", ".log", ".txt", ".lock", ".sum", ".map":
		return declSyntax{}
	case ".go":
		return declSyntax{rules: []*regexp.Regexp{
			regexp.MustCompile(`^func `),
			regexp.MustCompile(`^type `),
			// A grouped block, not the single-line form: `var errFoo = errors.New(…)` is a
			// value somebody will find by its use, while `var (` is a table worth seeing.
			regexp.MustCompile(`^(var|const) \($`),
		}}
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".vue", ".svelte", ".astro":
		return declSyntax{rules: []*regexp.Regexp{
			regexp.MustCompile(`^(export\s+)?(default\s+)?(async\s+)?function\b`),
			regexp.MustCompile(`^(export\s+)?(default\s+)?(abstract\s+)?class\b`),
			regexp.MustCompile(`^(export\s+)?(declare\s+)?(interface|type|enum|namespace)\b`),
			// A top-level const or let: the component, the store, the handler. Indented ones
			// are local variables, which is why this rule is not `^\s*`.
			regexp.MustCompile(`^(export\s+)?(const|let)\s+[A-Za-z_$][\w$]*\s*[:=]`),
		}}
	case ".py", ".pyi", ".rb":
		// Indented too, on purpose: a method in a class is a declaration, and its line
		// number is what the next read needs even though the outline does not show the
		// nesting it sits in.
		return declSyntax{rules: []*regexp.Regexp{
			regexp.MustCompile(`^\s*(async\s+)?def\s`),
			regexp.MustCompile(`^\s*class\s`),
		}}
	case ".rs":
		// Indented as well as flush left, because Rust's methods live inside an `impl` block:
		// a rule anchored at column 0 lists the block and none of what is in it.
		return declSyntax{rules: []*regexp.Regexp{
			regexp.MustCompile(`^\s*(pub(\([^)]*\))?\s+)?(async\s+)?(unsafe\s+)?(fn|struct|enum|trait|impl|mod|type|const|static)\b`),
			regexp.MustCompile(`^\s*macro_rules!`),
		}}
	case ".sh", ".bash", ".zsh":
		return declSyntax{rules: []*regexp.Regexp{
			regexp.MustCompile(`^(function\s+)?[A-Za-z_][\w-]*\s*\(\)`),
		}}
	case ".md", ".mdx", ".markdown":
		return declSyntax{fenced: true, rules: []*regexp.Regexp{
			regexp.MustCompile(`^#{1,6}\s`),
		}}
	case ".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".cs", ".java", ".kt", ".kts",
		".swift", ".scala", ".m", ".mm", ".php", ".dart", ".sol", ".lua", ".ex", ".exs",
		".erl", ".hs", ".ml", ".clj":
		return declSyntax{
			rules: []*regexp.Regexp{
				regexp.MustCompile(`^\s*(?:(?:export|public|private|protected|internal|static|final|abstract|virtual|override|inline|async|extern|const|local|partial|open|suspend|fun))\s+\w`),
				regexp.MustCompile(`^\s*(?:func|fn|def|class|struct|interface|enum|trait|impl|module|namespace|protocol|record|type|function)\b`),
			},
			// The third shape, and the reason this family needed one: C and C++ write a
			// definition as a return type, a name and a brace — `int main(void) {` — with no
			// keyword the two rules above can anchor on.
			signature: cSignature,
		}
	}
	// Anything else — a `.yaml`, a `.toml`, an `.ini`, a `.conf`, a file with no extension at
	// all — gets the structural lines of a config-shaped file. `cutValue` is the second half of
	// that answer: the generic fallback lists *keys*, never what is beside them, because an
	// unrecognised file is exactly where a password or an API key lives and this text is
	// copied into the model's context (documentation/CODING.md §5).
	return declSyntax{cutValue: true, rules: []*regexp.Regexp{
		// `key: value`, but not `key::` or `key:=`.
		regexp.MustCompile(`^[A-Za-z_][\w.-]*\s*:(?:[^:=]|$)`),
		// `key = value`, `key += value`, `KEY=value`.
		regexp.MustCompile(`^[A-Za-z_][\w.-]*\s*[:+]?=(?:[^=]|$)`),
		// `[section]`, the INI and TOML shape.
		regexp.MustCompile(`^\[[A-Za-z0-9_.-]+\]`),
	}}
}
