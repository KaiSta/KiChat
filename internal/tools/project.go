package tools

// The project tools: what a coding chat can read out of the folder it was created on.
//
// They are a separate list from the research tools in tools.go, and deliberately so.
// Those reach a network or the chat's own index; these reach the user's disk, which
// makes them the only tools whose argument is a path — and the only ones whose safety
// rests on something outside this package. internal/workspace owns that boundary: it
// resolves every path against the project's root and refuses anything that leaves it,
// including through a symlink. Nothing here does its own filepath work, so a fourth
// tool cannot be the one that forgets.
//
// The list is reads, writes and the terminal: this file holds the ones that only look, and
// the tools that change something — a file in change.go, the machine in command.go — live
// next to the confirmation they need.

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"kichat/internal/llm"
	"kichat/internal/uimsg"
	"kichat/internal/workspace"
)

// Tool names.
const (
	ToolReadFile    = "read_file"
	ToolListDir     = "list_dir"
	ToolSearchFiles = "search_files"
)

// ProjectToolNames lists the tools that need a project folder — the reads in this file, the
// writes in change.go and the terminal in command.go.
//
// It is exported because two decisions have to be made about the same set: whether a tool is
// advertised for a request, and whether a call naming it is a tool this chat can be answered
// with at all. internal/chat derives both from this one list, so a further project tool
// cannot be added to one and forgotten in the other. The tools that change something need
// one more thing on top of a folder — somebody to ask — and that is GuardedToolNames.
func ProjectToolNames() []string {
	names := make([]string, 0, 3+len(GuardedToolNames()))
	names = append(names, ToolReadFile, ToolListDir, ToolSearchFiles)
	return append(names, GuardedToolNames()...)
}

// projectDefinitions advertises the project tools.
//
// The descriptions are the ones a model reads when it decides *whether* to reach for a
// tool at all, so each says what the tool is for rather than only what it does: a
// coding chat answers from the folder or it invents, and the phrase that keeps it
// reading is "statt zu vermuten".
//
// The other two groups are appended rather than defined here, so `ProjectToolNames`,
// `projectDefinitions` and the advertised list stay one set: a reader asking "what can a
// coding chat do to this folder" gets the same answer from all three, and the test that
// compares them cannot be satisfied by adding a tool to one list.
func projectDefinitions() []llm.ToolDefinition {
	all := make([]llm.ToolDefinition, 0, 5)
	all = append(all, reads()...)
	all = append(all, changeToolDefinitions()...)
	return append(all, commandDefinitions()...)
}

// reads is the project tools that only look at the folder.
func reads() []llm.ToolDefinition {
	return []llm.ToolDefinition{
		{
			Name: ToolReadFile,
			Description: "Liest eine Textdatei aus dem Projektordner und gibt sie mit Zeilennummern zurück. " +
				"Nutze dieses Tool, wenn du den genauen Inhalt einer Datei kennen musst, statt ihn aus dem " +
				"Namen oder aus dem Gedächtnis zu vermuten. Eine große Datei liest du abschnittsweise: der " +
				"letzte Aufruf nennt die Zeile, ab der es weitergeht (offset). Binärdateien werden abgelehnt.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Pfad relativ zum Projektordner, z. B. \"internal/store/store.go\".",
					},
					"offset": map[string]any{
						"type":        "integer",
						"description": "Erste Zeile (1-basiert). Weglassen: ab Zeile 1. Der Wert aus einem „weiter mit offset“-Hinweis wird hier unverändert übernommen.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Anzahl der Zeilen (Standard 500, höchstens 2000).",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name: ToolListDir,
			Description: "Listet den Inhalt eines Ordners im Projektordner auf: erst Unterordner, dann Dateien, " +
				"jeweils alphabetisch. Nutze dieses Tool zur Orientierung, bevor du Dateien liest oder Pfade " +
				"errätst. Ohne path wird der Projektordner selbst aufgelistet; ein Ordner in der Liste wird " +
				"mit seinem Namen als path geöffnet.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Ordner relativ zum Projektordner. Weglassen: der Projektordner.",
					},
				},
			},
		},
		{
			Name: ToolSearchFiles,
			Description: "Sucht einen Text in allen Dateien des Projektordners und liefert die Treffer als " +
				"Datei, Zeilennummer und Zeileninhalt. Die Suche ist wörtlich (kein regulärer Ausdruck) und " +
				"beachtet Groß-/Kleinschreibung nicht. Nutze sie, um eine Definition, einen Aufruf, einen " +
				"Bezeichner oder eine Zeichenkette zu finden, statt Dateien einzeln zu lesen. " +
				"Build-Ausgaben, Abhängigkeiten und .git werden nicht durchsucht.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Der gesuchte Text, z. B. \"func NewRegistry\" oder ein Dateiname.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Höchstzahl der Treffer (1-200, Standard 60).",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

// workspaceFor resolves the folder the calling chat works in.
//
// Every failure here is reported rather than worked around. There is no fallback to the
// app's own working directory or to the user's home: a file tool that quietly read from
// wherever KiChat happens to be running would be the exact failure the folder boundary
// exists to prevent, and it would be invisible because the answer would look plausible.
func (s *Service) workspaceFor(chatID string) (workspace.Root, error) {
	if s.Store == nil {
		return workspace.Root{}, uimsg.Detailf("tools.noWorkspace",
			"this chat has no project folder, so there are no files to read")
	}
	chat, err := s.Store.GetChat(chatID)
	if err != nil {
		return workspace.Root{}, uimsg.Detailf("tools.noWorkspace",
			fmt.Sprintf("this chat could not be read, so its project folder is unknown: %v", err))
	}
	if !chat.IsProject() {
		return workspace.Root{}, uimsg.Detailf("tools.noWorkspace",
			"this chat has no project folder, so there are no files to read")
	}
	root, err := workspace.Open(chat.Workspace)
	if err != nil {
		return workspace.Root{}, uimsg.Detailf("tools.workspaceGone",
			fmt.Sprintf("the project folder %s cannot be opened: %v", chat.Workspace, err))
	}
	return root, nil
}

// projectError turns a failure from internal/workspace into a message both readers can
// use: the window translates the code, and the model is handed the sentence, which
// names the path it should correct.
//
// The two refusals are separated because they call for different next moves. A path
// outside the folder is not something to retry with the same shape — the model has to
// stop and use a path inside the project — while a file that could not be read is
// usually a typo or a folder. Telling them apart in one generic message would leave the
// model to guess, and the wrong guess is another refused call.
func projectError(path string, err error) error {
	switch {
	case errors.Is(err, workspace.ErrOutside):
		return uimsg.Detailf("tools.outsideWorkspace",
			fmt.Sprintf("%q is outside the project folder, which is the only place these tools "+
				"may look; use a path relative to the project root", path), path)
	case errors.Is(err, fs.ErrNotExist):
		return uimsg.Detailf("tools.fileUnreadable",
			fmt.Sprintf("%q does not exist in the project folder", path), path)
	case errors.Is(err, fs.ErrPermission):
		return uimsg.Detailf("tools.fileUnreadable",
			fmt.Sprintf("%q cannot be read: permission denied", path), path)
	default:
		return uimsg.Detailf("tools.fileUnreadable",
			fmt.Sprintf("%q cannot be read: %v", path, err), path)
	}
}

type readFileArgs struct {
	Path   string `json:"path"`
	Offset any    `json:"offset"`
	Limit  any    `json:"limit"`
}

func (s *Service) readFile(chatID, argsJSON string) (string, error) {
	var args readFileArgs
	if err := decodeLoose(argsJSON, &args); err != nil {
		return "", badArguments(err)
	}
	args.Path = strings.TrimSpace(args.Path)
	if args.Path == "" {
		return "", uimsg.Detailf("tools.pathMissing", "the path argument is empty; name a file in the project folder")
	}
	root, err := s.workspaceFor(chatID)
	if err != nil {
		return "", err
	}
	file, err := root.Read(args.Path, looseInt(args.Offset, 0), looseInt(args.Limit, 0))
	if err != nil {
		return "", projectError(args.Path, err)
	}
	if file.Binary {
		// A refusal rather than a page of replacement characters: the model would
		// otherwise spend a round reasoning about a picture, an archive or a compiled
		// object it cannot see.
		return fmt.Sprintf("%s ist keine Textdatei (sie enthält Null-Bytes) und wird nicht als "+
			"Text ausgegeben. Suche mit search_files nach dem, was du daraus brauchst.", file.Path), nil
	}
	var sb strings.Builder
	if file.Total > 0 {
		fmt.Fprintf(&sb, "%s — Zeilen %d–%d von %d\n\n", file.Path, file.From, file.From+file.Returned-1, file.Total)
	} else {
		fmt.Fprintf(&sb, "%s — Zeilen %d–%d\n\n", file.Path, file.From, file.From+file.Returned-1)
	}
	if file.Returned == 0 {
		// An offset past the last line, or an empty file. Saying which it is keeps the
		// model from retrying the same read.
		if file.Total > 0 {
			fmt.Fprintf(&sb, "Die Datei hat %d Zeilen; ab Zeile %d steht nichts mehr.", file.Total, file.From)
		} else {
			sb.WriteString("Die Datei ist leer.")
		}
		return sb.String(), nil
	}
	sb.WriteString(file.Text)
	sb.WriteString("\n")
	if file.Truncated {
		fmt.Fprintf(&sb, "\nDie Datei geht weiter. Lies den nächsten Abschnitt mit offset=%d.",
			file.From+file.Returned)
	}
	return sb.String(), nil
}

type listDirArgs struct {
	Path string `json:"path"`
}

func (s *Service) listDir(chatID, argsJSON string) (string, error) {
	var args listDirArgs
	if err := decodeLoose(argsJSON, &args); err != nil {
		return "", badArguments(err)
	}
	root, err := s.workspaceFor(chatID)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(args.Path)
	list, err := root.List(path, 0)
	if err != nil {
		return "", projectError(path, err)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s — %d Einträge\n\n", list.Path, list.Total)
	for _, entry := range list.Entries {
		if entry.Dir {
			fmt.Fprintf(&sb, "%s/\n", entry.Name)
			continue
		}
		fmt.Fprintf(&sb, "%s (%s)\n", entry.Name, humanBytes(entry.Size))
	}
	if list.Omitted > 0 {
		fmt.Fprintf(&sb, "\n… und %d weitere Einträge, die nicht aufgelistet sind.", list.Omitted)
	}
	if len(list.Entries) == 0 {
		sb.WriteString("Der Ordner ist leer.")
	}
	return sb.String(), nil
}

type searchFilesArgs struct {
	Query string `json:"query"`
	Limit any    `json:"limit"`
}

func (s *Service) searchFiles(chatID, argsJSON string) (string, error) {
	var args searchFilesArgs
	if err := decodeLoose(argsJSON, &args); err != nil {
		return "", badArguments(err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", uimsg.Detailf("tools.queryMissing", "the query argument is empty")
	}
	root, err := s.workspaceFor(chatID)
	if err != nil {
		return "", err
	}
	matches, err := root.Search(args.Query, looseInt(args.Limit, 0))
	if err != nil {
		return "", projectError(args.Query, err)
	}
	var sb strings.Builder
	switch {
	case len(matches.Hits) == 0 && !matches.Stopped:
		fmt.Fprintf(&sb, "Keine Treffer für %q in %d durchsuchten Dateien.", matches.Query, matches.Scanned)
	case len(matches.Hits) == 0:
		fmt.Fprintf(&sb, "Keine Treffer für %q — die Suche wurde nach %d Dateien abgebrochen, der Text kann "+
			"in den nicht durchsuchten Teilen stehen.", matches.Query, matches.Scanned)
	default:
		fmt.Fprintf(&sb, "%d Treffer für %q:\n\n", len(matches.Hits), matches.Query)
		for _, hit := range matches.Hits {
			fmt.Fprintf(&sb, "%s:%d: %s\n", hit.Path, hit.Line, hit.Text)
		}
		if matches.Stopped {
			fmt.Fprintf(&sb, "\nWeitere Dateien wurden nicht durchsucht: die Suche hat nach %d Dateien "+
				"abgebrochen. Ein engerer Suchtext liefert vollständigere Treffer.", matches.Scanned)
		}
	}
	return sb.String(), nil
}

// humanBytes renders a size the way a listing should show it: never as a bare count of
// bytes, which a model reads as a number rather than as a size.
func humanBytes(size int64) string {
	switch {
	case size < 1024:
		return fmt.Sprintf("%d B", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.1f kB", float64(size)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	}
}
