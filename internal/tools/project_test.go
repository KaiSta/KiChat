package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kichat/internal/store"
	"kichat/internal/uimsg"
)

// projectService builds a store holding one project chat on a folder of files, which is
// the state the file tools exist for.
func projectService(t *testing.T, files map[string]string) (*Service, string, string) {
	t.Helper()
	folder := t.TempDir()
	for name, body := range files {
		path := filepath.Join(folder, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	project := store.Chat{
		ID: "project-1", Title: "Projekt", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Mode: store.ModeCoding, Workspace: folder,
	}
	plain := store.Chat{
		ID: "chat-1", Title: "Chat", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Mode: store.ModeChat, Workspace: folder,
	}
	for _, c := range []store.Chat{project, plain} {
		if err := st.CreateChat(c); err != nil {
			t.Fatalf("create chat %s: %v", c.ID, err)
		}
	}
	return &Service{Store: st}, "project-1", folder
}

// refusalCode is the uimsg code a failed call produced, which is what the window
// translates and what a test can assert without pinning a sentence in one language.
func refusalCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	message, ok := uimsg.Parse(err.Error())
	if !ok {
		t.Fatalf("the failure carries no code, so the window would show raw text: %v", err)
	}
	return message.Code
}

// TestReadFileNumbersTheLines is the shape a model needs to name a place later: the
// numbers it read are the numbers it will use.
func TestReadFileNumbersTheLines(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{
		"pkg/a.go": "package pkg\n\nfunc A() {}\n",
	})
	out, err := service.Execute(context.Background(), chatID, ToolReadFile,
		`{"path": "pkg/a.go"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	for _, want := range []string{"pkg/a.go", "     1| package pkg", "     3| func A() {}"} {
		if !strings.Contains(out, want) {
			t.Errorf("read_file output is missing %q:\n%s", want, out)
		}
	}
}

// TestReadFileContinuesWhereItStopped pins the trailer a long file needs: a model that
// does not know there is more will answer from the first window and be wrong about the
// second.
func TestReadFileContinuesWhereItStopped(t *testing.T) {
	var body strings.Builder
	for i := 1; i <= 40; i++ {
		body.WriteString("line\n")
	}
	service, chatID, _ := projectService(t, map[string]string{"long.txt": body.String()})

	first, err := service.Execute(context.Background(), chatID, ToolReadFile,
		`{"path": "long.txt", "limit": 10}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(first, "offset=11") {
		t.Fatalf("a truncated read does not say where to continue:\n%s", first)
	}
	second, err := service.Execute(context.Background(), chatID, ToolReadFile,
		`{"path": "long.txt", "offset": 11, "limit": 5}`)
	if err != nil {
		t.Fatalf("read_file at offset 11: %v", err)
	}
	if !strings.Contains(second, "    11| line") || strings.Contains(second, "    10| line") {
		t.Errorf("the second window does not start where the first ended:\n%s", second)
	}

	// The card's sentence, not the model's: a window of a longer file has no total to name, so
	// its template has three fields rather than four with a zero where the length goes.
	window, err := service.ExecuteWithSummary(context.Background(), chatID, ToolReadFile,
		`{"path": "long.txt", "limit": 10}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if window.Summary == nil || window.Summary.Code != "tool.sum.readPart" || len(window.Summary.Args) != 3 {
		t.Errorf("a truncated read is summarised as %+v, want tool.sum.readPart with three fields", window.Summary)
	}
}

// TestReadFileSaysWhenOneLineIsTooLongForTheWindow is the shape a read answers worst: a
// minified bundle or a one-line JSON export is a single line larger than the whole window, so the
// window never starts. That used to be reported as an empty file — the one reading that makes a
// model conclude there is nothing to look at, or write the file from nothing.
func TestReadFileSaysWhenOneLineIsTooLongForTheWindow(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{
		"bundle.min.js": strings.Repeat("x", 200<<10),
	})
	out, err := service.Execute(context.Background(), chatID, ToolReadFile, `{"path": "bundle.min.js"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if strings.Contains(out, "ist leer") {
		t.Fatalf("a file with one very long line was reported as empty:\n%s", out)
	}
	if strings.Contains(out, "Zeilen 1–0") {
		t.Errorf("the answer names an impossible range:\n%s", out)
	}
	for _, want := range []string{"Zeile 1", "Textfenster", "search_files"} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer is missing %q, so the model cannot tell what happened:\n%s", want, out)
		}
	}
}

// TestReadFileRefusesAPathOutsideTheProject is the boundary as the model meets it: it
// writes the path, so it is the one that has to be told no.
func TestReadFileRefusesAPathOutsideTheProject(t *testing.T) {
	service, chatID, folder := projectService(t, map[string]string{"a.txt": "a\n"})
	secret := filepath.Join(filepath.Dir(folder), "secret.txt")
	if err := os.WriteFile(secret, []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret.txt", "/etc/passwd", secret} {
		_, err := service.Execute(context.Background(), chatID, ToolReadFile,
			`{"path": "`+path+`"}`)
		if code := refusalCode(t, err); code != "tools.outsideWorkspace" {
			t.Errorf("read_file(%q) refused with %q, want tools.outsideWorkspace", path, code)
		}
	}
}

// TestFileToolsRefuseAChatWithNoProject covers the state the gate is meant to make
// unreachable: a call that arrives anyway is answered with what is wrong, and never with
// a file from somewhere else.
func TestFileToolsRefuseAChatWithNoProject(t *testing.T) {
	service, _, _ := projectService(t, map[string]string{"a.txt": "a\n"})
	for _, call := range []struct{ tool, args string }{
		{ToolReadFile, `{"path": "a.txt"}`},
		{ToolListDir, `{}`},
		{ToolSearchFiles, `{"query": "a"}`},
	} {
		_, err := service.Execute(context.Background(), "chat-1", call.tool, call.args)
		if code := refusalCode(t, err); code != "tools.noWorkspace" {
			t.Errorf("%s on a chat with no project refused with %q, want tools.noWorkspace", call.tool, code)
		}
	}
	// An unknown chat is the same answer rather than a crash or an empty result.
	if _, err := service.Execute(context.Background(), "ghost", ToolReadFile, `{"path":"a.txt"}`); err == nil {
		t.Error("read_file on an unknown chat succeeded")
	}
}

// TestReadFileRefusesABinaryAndReportsAMissingFile separates the two failures a model
// has to tell apart: "not a file you can read" and "not there".
func TestReadFileRefusesABinaryAndReportsAMissingFile(t *testing.T) {
	service, chatID, folder := projectService(t, map[string]string{"a.txt": "a\n"})
	if err := os.WriteFile(filepath.Join(folder, "blob.bin"), []byte{0, 1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}

	binary, err := service.Execute(context.Background(), chatID, ToolReadFile, `{"path": "blob.bin"}`)
	if err != nil {
		t.Fatalf("a binary read should be answered, not failed: %v", err)
	}
	if !strings.Contains(binary, "keine Textdatei") {
		t.Errorf("a binary file was not reported as one:\n%s", binary)
	}

	_, err = service.Execute(context.Background(), chatID, ToolReadFile, `{"path": "nope.txt"}`)
	if code := refusalCode(t, err); code != "tools.fileUnreadable" {
		t.Errorf("a missing file refused with %q, want tools.fileUnreadable", code)
	}

	// A missing argument is its own refusal, so the model is told which field to fill
	// in rather than that a path failed.
	_, err = service.Execute(context.Background(), chatID, ToolReadFile, `{}`)
	if code := refusalCode(t, err); code != "tools.pathMissing" {
		t.Errorf("an empty path refused with %q, want tools.pathMissing", code)
	}
}

// TestListDirShowsFoldersBeforeFiles is the orientation tool: the first thing a model
// does in an unknown project, and the reason it does not have to guess a layout.
func TestListDirShowsFoldersBeforeFiles(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{
		"main.go": "package main\n", "internal/deep/a.go": "package deep\n",
		"notes.md": "# Notes\n",
	})
	out, err := service.Execute(context.Background(), chatID, ToolListDir, `{}`)
	if err != nil {
		t.Fatalf("list_dir: %v", err)
	}
	if !strings.Contains(out, "internal/") {
		t.Errorf("a folder is not marked as one:\n%s", out)
	}
	if strings.Index(out, "internal/") > strings.Index(out, "main.go") {
		t.Errorf("folders do not come before files:\n%s", out)
	}
	if !strings.Contains(out, "3 Einträge") {
		t.Errorf("the listing does not say how many entries it has:\n%s", out)
	}

	// And a subfolder is reachable by name, which is how the model descends without a
	// recursive listing.
	sub, err := service.Execute(context.Background(), chatID, ToolListDir, `{"path": "internal/deep"}`)
	if err != nil {
		t.Fatalf("list_dir on a subfolder: %v", err)
	}
	if !strings.Contains(sub, "a.go") {
		t.Errorf("a subfolder listing lost its file:\n%s", sub)
	}
}

// TestSearchFilesStopsAtTheDefaultItAdvertises is the schema and the code agreeing: the
// description promises 60 hits when the model names no limit, and the workspace cap (200) is a
// bound rather than a default — a model that asked for nothing was being handed three times the
// context the tool said it would.
func TestSearchFilesStopsAtTheDefaultItAdvertises(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 80; i++ {
		files[fmt.Sprintf("f%02d.go", i)] = "needle\n"
	}
	service, chatID, _ := projectService(t, files)

	out, err := service.Execute(context.Background(), chatID, ToolSearchFiles, `{"query": "needle"}`)
	if err != nil {
		t.Fatalf("search_files: %v", err)
	}
	if !strings.Contains(out, "60 Treffer") {
		t.Fatalf("a search with no limit did not stop at the 60 the schema names:\n%s", out)
	}
	if hits := strings.Count(out, ":1: needle"); hits > 60 {
		t.Errorf("the answer carries %d hits, more than the 60 it promised", hits)
	}
}

// TestSearchFilesSaysWhenItFoundNothing pins the two readings of an empty result: the
// string is not in the project, or it was not in the part that was read. They call for
// opposite next moves, so the tool has to distinguish them.
func TestSearchFilesSaysWhenItFoundNothing(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{
		"main.go":                   "func FindThing() {}\n",
		"node_modules/pkg/index.js": "function FindThing() {}\n",
	})
	out, err := service.Execute(context.Background(), chatID, ToolSearchFiles, `{"query": "FindThing"}`)
	if err != nil {
		t.Fatalf("search_files: %v", err)
	}
	if !strings.Contains(out, "main.go:1") {
		t.Errorf("the hit in the project is missing:\n%s", out)
	}
	if strings.Contains(out, "node_modules") {
		t.Errorf("a dependency was searched:\n%s", out)
	}

	none, err := service.Execute(context.Background(), chatID, ToolSearchFiles, `{"query": "NichtsDerartiges"}`)
	if err != nil {
		t.Fatalf("search_files with no hits: %v", err)
	}
	if !strings.Contains(none, "Keine Treffer") {
		t.Errorf("an empty result does not say so plainly:\n%s", none)
	}

	_, err = service.Execute(context.Background(), chatID, ToolSearchFiles, `{"query": "  "}`)
	if code := refusalCode(t, err); code != "tools.queryMissing" {
		t.Errorf("an empty query refused with %q, want tools.queryMissing", code)
	}
}

// TestReadFileOutlinesTheDeclarationsInsteadOfTheFile is the trade the outline makes: the
// declaration list arrives with the same line numbers a read prints — so the next call can cite
// one — and none of the code.
func TestReadFileOutlinesTheDeclarationsInsteadOfTheFile(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{
		"pkg/a.go": "package pkg\n\nfunc A() {\n\tprintln(\"nur im Code\")\n}\n\ntype B struct{}\n",
	})
	result, err := service.ExecuteWithSummary(context.Background(), chatID, ToolReadFile,
		`{"path": "pkg/a.go", "outline": true}`)
	if err != nil {
		t.Fatalf("read_file with outline: %v", err)
	}
	for _, want := range []string{"Umriss", "     3| func A() {", "     7| type B struct{}", "offset"} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("the outline is missing %q:\n%s", want, result.Text)
		}
	}
	if strings.Contains(result.Text, "nur im Code") {
		t.Errorf("the outline carried the body of the function:\n%s", result.Text)
	}
	if result.Summary == nil || result.Summary.Code != "tool.sum.readOutline" || len(result.Summary.Args) != 3 {
		t.Errorf("an outline is summarised as %+v, want tool.sum.readOutline with three fields", result.Summary)
	}
}

// TestOutlineIsOnlyWhenItIsAskedFor pins the spellings a model actually writes, and the one that
// must not count: `false` is a request for the text, and answering it with a map would be the
// one wrong direction.
func TestOutlineIsOnlyWhenItIsAskedFor(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{
		"pkg/a.go": "package pkg\n\nfunc A() {}\n",
	})
	for _, call := range []string{
		`{"path":"pkg/a.go","outline":true}`,
		`{"path":"pkg/a.go","outline":"true"}`,
	} {
		out, err := service.Execute(context.Background(), chatID, ToolReadFile, call)
		if err != nil {
			t.Fatalf("read_file %s: %v", call, err)
		}
		if !strings.Contains(out, "Umriss") {
			t.Errorf("read_file %s did not outline:\n%s", call, out)
		}
	}

	whole, err := service.Execute(context.Background(), chatID, ToolReadFile,
		`{"path":"pkg/a.go","outline":false}`)
	if err != nil {
		t.Fatalf("read_file with outline false: %v", err)
	}
	if strings.Contains(whole, "Umriss") {
		t.Errorf("outline false was answered with a map:\n%s", whole)
	}
	if !strings.Contains(whole, "     1| package pkg") {
		t.Errorf("outline false did not return the file:\n%s", whole)
	}
}

// TestAnOutlineOfADataFileSaysThereIsNone is the half an empty list owes the model: a JSON file
// has no declarations to list, and the answer has to point at the window rather than leave a
// model concluding that the file is empty.
func TestAnOutlineOfADataFileSaysThereIsNone(t *testing.T) {
	service, chatID, _ := projectService(t, map[string]string{
		"data.json": "{\n  \"type\": \"thing\"\n}\n",
	})
	result, err := service.ExecuteWithSummary(context.Background(), chatID, ToolReadFile,
		`{"path":"data.json","outline":true}`)
	if err != nil {
		t.Fatalf("read_file with outline: %v", err)
	}
	if !strings.Contains(result.Text, "keine Deklarationen erkannt") || !strings.Contains(result.Text, "search_files") {
		t.Errorf("an empty outline does not say what to do instead:\n%s", result.Text)
	}
	if result.Summary == nil || result.Summary.Code != "tool.sum.readOutlineNone" {
		t.Errorf("an empty outline is summarised as %+v, want tool.sum.readOutlineNone", result.Summary)
	}
	if strings.Contains(result.Text, "Die Datei ist leer") {
		t.Errorf("a non-empty file was reported as empty:\n%s", result.Text)
	}
}

// TestAGoneProjectFolderIsReported covers the folder moving or being deleted between
// messages: the tools have to say the folder is unusable rather than read somewhere else.
func TestAGoneProjectFolderIsReported(t *testing.T) {
	service, chatID, folder := projectService(t, map[string]string{"a.txt": "a\n"})
	if err := os.RemoveAll(folder); err != nil {
		t.Fatal(err)
	}
	_, err := service.Execute(context.Background(), chatID, ToolReadFile, `{"path": "a.txt"}`)
	if code := refusalCode(t, err); code != "tools.workspaceGone" {
		t.Errorf("a deleted folder refused with %q, want tools.workspaceGone", code)
	}
	// The chat is still a project, so the gate keeps offering the tools: the failure is
	// about the folder, and hiding the tools would leave the user with no way to be told.
	chat, err := service.Store.GetChat(chatID)
	if err != nil {
		t.Fatal(err)
	}
	if !chat.IsProject() {
		t.Error("a project stopped being one because its folder went away")
	}
}

// TestTheProjectToolNamesAreTheOnesThisFileDefines keeps the exported list the engine
// filters by from drifting from the tools themselves.
func TestTheProjectToolNamesAreTheOnesThisFileDefines(t *testing.T) {
	defined := map[string]bool{}
	for _, def := range projectDefinitions() {
		defined[def.Name] = true
	}
	names := ProjectToolNames()
	if len(names) != len(defined) {
		t.Fatalf("ProjectToolNames lists %d tools but the definitions hold %d: %v", len(names), len(defined), names)
	}
	for _, name := range names {
		if !defined[name] {
			t.Errorf("ProjectToolNames names %q, which is not defined here", name)
		}
	}
	// And every one of them is reachable through the one list the request is built from.
	service, _, _ := projectService(t, nil)
	all := map[string]bool{}
	for _, def := range service.Definitions() {
		all[def.Name] = true
	}
	for _, name := range names {
		if !all[name] {
			t.Errorf("%q is not in the advertised definitions, so no request can call it", name)
		}
	}
}
