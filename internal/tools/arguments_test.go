package tools

// The other half of a borrowed tool name: its arguments.
//
// A model trained on another harness writes `read` with `{"file_path": "main.go"}` where this
// app's `read_file` takes `path`. internal/chat resolves the name (`toolAliases`), and a call
// that arrives with the right name and an unreadable argument object is refused with "the path
// argument is empty" — the user is told the call ran and the model is told it named nothing, so
// it either repeats the same shape or gives up. These tests pin the table that closes that gap,
// and the rules that keep it from swallowing a call it cannot improve.

import (
	"context"
	"strings"
	"testing"
)

// TestABorrowedFieldNameStillReadsTheFile is the report end to end: `cat` with `file_path`
// reaches `read_file`, and the file is returned rather than "the path argument is empty".
func TestABorrowedFieldNameStillReadsTheFile(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{"notes.txt": "alpha\nbeta\n"})

	out, err := service.Execute(context.Background(), chatID, ToolReadFile,
		`{"file_path":"notes.txt"}`)
	if err != nil {
		t.Fatalf("read_file with file_path: %v", err)
	}
	if !strings.Contains(out, "alpha") {
		t.Errorf("the borrowed field name did not reach the tool:\n%s", out)
	}
}

// TestThisAppsOwnFieldWinsOverABorrowedOne: a call that carries both keeps the one the schema
// named. Guessing between two paths would be the one thing this table must not do.
func TestThisAppsOwnFieldWinsOverABorrowedOne(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{"own.txt": "own\n", "other.txt": "other\n"})

	out, err := service.Execute(context.Background(), chatID, ToolReadFile,
		`{"path":"own.txt","file_path":"other.txt"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "own") || strings.Contains(out, "other") {
		t.Errorf("the borrowed field overrode the declared one:\n%s", out)
	}
}

// TestTheBorrowedFieldsOfEachProjectTool covers the tools the alias table in internal/chat can
// hand a call to, because that is what makes the pair one repair: every borrowed name there has
// a target whose fields a model from the same convention writes differently.
func TestTheBorrowedFieldsOfEachProjectTool(t *testing.T) {
	service, chatID, folder := writeService(t, map[string]string{
		"src/main.go": "package main\n\nfunc main() {}\n",
	}, &asked{})
	ctx := WithToolCall(WithCallingMessage(context.Background(), "msg-1"), "call-1")

	// `ls {"dir": …}` → list_dir
	out, err := service.Execute(ctx, chatID, ToolListDir, `{"dir":"src"}`)
	if err != nil {
		t.Fatalf("list_dir with dir: %v", err)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("list_dir did not read the borrowed folder argument:\n%s", out)
	}

	// `grep {"pattern": …}` → search_files, whose own field is query.
	out, err = service.Execute(ctx, chatID, ToolSearchFiles, `{"pattern":"func main"}`)
	if err != nil {
		t.Fatalf("search_files with pattern: %v", err)
	}
	if !strings.Contains(out, "src/main.go") {
		t.Errorf("search_files did not read the borrowed pattern argument:\n%s", out)
	}

	// `glob {"glob": …}` → find_files, whose own field is pattern.
	out, err = service.Execute(ctx, chatID, ToolFindFiles, `{"glob":"**/*.go"}`)
	if err != nil {
		t.Fatalf("find_files with glob: %v", err)
	}
	if !strings.Contains(out, "src/main.go") {
		t.Errorf("find_files did not read the borrowed glob argument:\n%s", out)
	}

	// `edit {"file_path": …, "old_str": …, "new_str": …}` → edit_file.
	if _, err := service.Execute(ctx, chatID, ToolEditFile,
		`{"file_path":"src/main.go","old_str":"func main() {}","new_str":"func main() { run() }"}`); err != nil {
		t.Fatalf("edit_file with the borrowed fields: %v", err)
	}
	if got := readProjectFile(t, folder, "src/main.go"); !strings.Contains(got, "func main() { run() }") {
		t.Errorf("edit_file did not read the borrowed fields:\n%s", got)
	}

	// `write {"file_path": …}` → write_file.
	if _, err := service.Execute(ctx, chatID, ToolWriteFile,
		`{"file_path":"src/extra.go","content":"package main\n"}`); err != nil {
		t.Fatalf("write_file with file_path: %v", err)
	}
	if got := readProjectFile(t, folder, "src/extra.go"); !strings.Contains(got, "package main") {
		t.Errorf("write_file did not read the borrowed path:\n%s", got)
	}
}

// TestACallTheTableCannotImproveIsLeftAlone: the table is a rename, not a repair. A body that
// is not a JSON object, a tool with no entry and an object with none of the borrowed keys all
// come back untouched, so the tool's own decoder stays the one that reports the problem.
func TestACallTheTableCannotImproveIsLeftAlone(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{ToolReadFile, `{"path":"a.go"}`},
		{ToolReadFile, `not json at all`},
		{ToolReadFile, `["a.go"]`},
		{ToolReadFile, ``},
		{ToolSearchWeb, `{"file_path":"a.go"}`},
	} {
		if got := withAliasedArguments(tc.name, tc.in); got != tc.in {
			t.Errorf("withAliasedArguments(%s, %q) = %q, want it unchanged", tc.name, tc.in, got)
		}
	}
}
