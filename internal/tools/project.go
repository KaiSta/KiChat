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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
	ToolFindFiles   = "find_files"
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
	names := make([]string, 0, 4+len(GuardedToolNames()))
	names = append(names, ToolReadFile, ToolListDir, ToolSearchFiles, ToolFindFiles)
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
func projectDefinitions(lang string) []llm.ToolDefinition {
	all := make([]llm.ToolDefinition, 0, 6)
	all = append(all, reads(lang)...)
	all = append(all, changeToolDefinitions(lang)...)
	return append(all, commandDefinitions(lang)...)
}

// reads is the project tools that only look at the folder.
func reads(lang string) []llm.ToolDefinition {
	return []llm.ToolDefinition{
		findFilesDefinition(lang),
		{
			Name: ToolReadFile,
			Description: text(lang,
				"Liest eine Textdatei aus dem Projektordner und gibt sie mit Zeilennummern zurück. "+
					"Nutze dieses Tool, wenn du den genauen Inhalt einer Datei kennen musst, statt ihn aus dem "+
					"Namen oder aus dem Gedächtnis zu vermuten. Eine große Datei liest du abschnittsweise: der "+
					"letzte Aufruf nennt die Zeile, ab der es weitergeht (offset). Mit \"outline\": true "+
					"bekommst du nur die Liste der Deklarationen (Funktionen, Typen, Klassen, Überschriften) "+
					"mit ihren Zeilennummern — bei einer langen Datei ein Bruchteil der Tokens, und du "+
					"siehst, welchen Abschnitt du danach genau lesen musst. Binärdateien werden abgelehnt.",
				"Reads a text file from the project folder and returns it with line numbers. Use it when "+
					"you need a file's exact content instead of guessing it from the name or from memory. "+
					"Read a large file in sections: the last call names the line to continue from (offset). "+
					"With \"outline\": true you get only the list of declarations (functions, types, "+
					"classes, headings) with their line numbers — a fraction of the tokens for a long file, "+
					"and you can see which section to read exactly afterwards. Binary files are refused."),
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type": "string",
						"description": text(lang, "Pfad relativ zum Projektordner, z. B. \"internal/store/store.go\".",
							"Path relative to the project folder, e.g. \"internal/store/store.go\"."),
					},
					"offset": map[string]any{
						"type": "integer",
						"description": text(lang,
							"Erste Zeile (1-basiert). Weglassen: ab Zeile 1. Der Wert aus einem „weiter mit offset“-Hinweis wird hier unverändert übernommen.",
							"First line (1-based). Omit: from line 1. A value from a \"continues at offset\" note is taken over unchanged."),
					},
					"limit": map[string]any{
						"type": "integer",
						"description": text(lang, "Anzahl der Zeilen (Standard 500, höchstens 2000).",
							"Number of lines (default 500, at most 2000)."),
					},
					"outline": map[string]any{
						"type": "boolean",
						"description": text(lang,
							"true: nur die Deklarationen mit Zeilennummern, ohne Code. Der erste Blick in eine lange Datei; den Abschnitt, den du ändern willst, liest du danach mit offset/limit.",
							"true: only the declarations with line numbers, without code. The first look at a long "+
								"file; read the section you want to change afterwards with offset/limit."),
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name: ToolListDir,
			Description: text(lang,
				"Listet den Inhalt eines Ordners im Projektordner auf: erst Unterordner, dann Dateien, "+
					"jeweils alphabetisch. Nutze dieses Tool zur Orientierung, bevor du Dateien liest oder Pfade "+
					"errätst. Ohne path wird der Projektordner selbst aufgelistet; ein Ordner in der Liste wird "+
					"mit seinem Namen als path geöffnet.",
				"Lists a folder inside the project folder: subfolders first, then files, each alphabetically. "+
					"Use it to get oriented before reading files or guessing paths. Without path the project "+
					"folder itself is listed; a folder in the list is opened by passing its name as path."),
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type": "string",
						"description": text(lang, "Ordner relativ zum Projektordner. Weglassen: der Projektordner.",
							"Folder relative to the project folder. Omit: the project folder."),
					},
				},
			},
		},
		{
			Name: ToolSearchFiles,
			Description: text(lang,
				"Sucht einen Text in allen Dateien des Projektordners und liefert die Treffer als "+
					"Datei, Zeilennummer und Zeileninhalt. Die Suche ist wörtlich (kein regulärer Ausdruck) und "+
					"beachtet Groß-/Kleinschreibung nicht. Nutze sie, um eine Definition, einen Aufruf, einen "+
					"Bezeichner oder eine Zeichenkette zu finden, statt Dateien einzeln zu lesen. "+
					"Build-Ausgaben, Abhängigkeiten und .git werden nicht durchsucht.",
				"Searches for a text in every file of the project folder and returns the hits as file, "+
					"line number and line content. The search is literal (not a regular expression) and "+
					"case-insensitive. Use it to find a definition, a call, an identifier or a string instead "+
					"of reading files one by one. Build output, dependencies and .git are not searched."),
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type": "string",
						"description": text(lang, "Der gesuchte Text, z. B. \"func NewRegistry\" oder ein Dateiname.",
							"The text to search for, e.g. \"func NewRegistry\" or a file name."),
					},
					"limit": map[string]any{
						"type": "integer",
						"description": text(lang, "Höchstzahl der Treffer (1-200, Standard 60).",
							"Maximum number of hits (1-200, default 60)."),
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
func (s *Service) projectError(path string, err error) error {
	lang := s.Language()
	switch {
	case errors.Is(err, workspace.ErrOutside):
		return uimsg.Detailf("tools.outsideWorkspace",
			fmt.Sprintf(text(lang,
				"%q liegt außerhalb des Projektordners, in den diese Tools allein schauen dürfen; "+
					"nutze einen Pfad relativ zum Projektordner",
				"%q is outside the project folder, which is the only place these tools "+
					"may look; use a path relative to the project root"), path), path)
	case errors.Is(err, fs.ErrNotExist):
		return uimsg.Detailf("tools.fileUnreadable",
			fmt.Sprintf(text(lang, "%q gibt es im Projektordner nicht",
				"%q does not exist in the project folder"), path), path)
	case errors.Is(err, fs.ErrPermission):
		return uimsg.Detailf("tools.fileUnreadable",
			fmt.Sprintf(text(lang, "%q lässt sich nicht lesen: keine Berechtigung",
				"%q cannot be read: permission denied"), path), path)
	default:
		return uimsg.Detailf("tools.fileUnreadable",
			fmt.Sprintf(text(lang, "%q lässt sich nicht lesen: %v",
				"%q cannot be read: %v"), path, err), path)
	}
}

type readFileArgs struct {
	Path    string `json:"path"`
	Offset  any    `json:"offset"`
	Limit   any    `json:"limit"`
	Outline any    `json:"outline"`
}

// The shapes a read_file call can have, for the trace.
//
// They are the four the feature is watched with: `bare` is the whole-file read a model reaches
// for by default, `bounded` is a window it chose itself, `outline` is the declaration list, and
// `continuation` is a window taken from this app's own `offset=N` offer. Counting them over a
// week of real chats is how the question "does a model use the outline, or does it still read
// everything?" gets an answer instead of a guess (documentation/CODING.md §5).
const (
	ReadShapeBare         = "bare"
	ReadShapeBounded      = "bounded"
	ReadShapeOutline      = "outline"
	ReadShapeContinuation = "continuation"
)

// readShape classifies one call. The offer is read before the read itself runs, because a
// truncated read records a new offer and a call must not be classified against its own.
func (s *Service) readShape(args readFileArgs, key readKey) string {
	if looseTruth(args.Outline) {
		return ReadShapeOutline
	}
	offset, limit := looseInt(args.Offset, 0), looseInt(args.Limit, 0)
	switch {
	case offset == 0 && limit == 0:
		// Zero is what the tool resolves to "from the top, default window" — the same call as
		// naming neither, so it is classified the same way (the rule targetedRead states).
		return ReadShapeBare
	case offset > 0 && s.offeredOffset(key) == offset:
		return ReadShapeContinuation
	}
	return ReadShapeBounded
}

// readFile answers one read, and carries the call's shape for the trace.
//
// The shape is assigned in a defer because every exit has to carry it — the outline, the
// already-read sentence, the binary refusal, the file itself — and eight copies of one
// assignment is how one of them ends up missing.
func (s *Service) readFile(ctx context.Context, chatID, argsJSON string) (result Result, err error) {
	var args readFileArgs
	if err := decodeLoose(argsJSON, &args); err != nil {
		return Result{}, s.badArguments(err)
	}
	args.Path = strings.TrimSpace(args.Path)
	if args.Path == "" {
		return Result{}, uimsg.Detailf("tools.pathMissing", "the path argument is empty; name a file in the project folder")
	}
	root, err := s.workspaceFor(chatID)
	if err != nil {
		return Result{}, err
	}
	offset, limit := looseInt(args.Offset, 0), looseInt(args.Limit, 0)
	// The turn's memory of what it has read, and the stamp that says the file has not moved
	// since. Both are only used inside a turn: outside one there is no "already" to speak of,
	// and a test calling the service directly should see the file.
	//
	// What this deliberately does not answer is "did my write land". Every tool that changes
	// the folder forgets what it touched (forgetReads, forgetAllReads), so a read that follows
	// a write always returns the file — the model checking its own work gets the text, not a
	// claim about the app's memory.
	key, stamp, watch := readKey{}, readStamp{}, false
	if turn, ok := TurnFrom(ctx); ok {
		if abs, resolveErr := root.Resolve(args.Path); resolveErr == nil {
			if info, statErr := os.Stat(abs); statErr == nil {
				if k, ok := readKeyFor(chatID, turn.Turn, abs); ok {
					key, stamp, watch = k, statStamp(info, offset, limit), true
				}
			}
		}
	}
	shape := s.readShape(args, key)
	defer func() {
		if err == nil {
			result.Shape = shape
		}
	}()
	if looseTruth(args.Outline) {
		return s.outlineFile(root, args.Path)
	}
	lang := s.Language()
	if watch && s.readSeen(key, stamp) {
		return summarized(fmt.Sprintf(text(lang,
			"%s: dieser Abschnitt wurde in diesem Turn bereits gelesen und ist seither unverändert. "+
				"Wenn du den Inhalt brauchst, lies einen anderen Abschnitt (offset), sonst arbeite mit "+
				"dem, was du hast.",
			"%s: this section was already read in this turn and is unchanged since. If you need the "+
				"content, read a different section (offset); otherwise work with what you have."), args.Path),
			"tool.sum.readSame", args.Path), nil
	}
	file, err := root.Read(args.Path, offset, limit)
	if err != nil {
		return Result{}, s.projectError(args.Path, err)
	}
	if file.Binary {
		// A refusal rather than a page of replacement characters: the model would
		// otherwise spend a round reasoning about a picture, an archive or a compiled
		// object it cannot see.
		return summarized(fmt.Sprintf(text(lang,
			"%s ist keine Textdatei (sie enthält Null-Bytes) und wird nicht als Text ausgegeben. "+
				"Suche mit search_files nach dem, was du daraus brauchst.",
			"%s is not a text file (it contains NUL bytes) and is not returned as text. Use "+
				"search_files to find what you need from it."), file.Path),
			"tool.sum.readBinary", file.Path), nil
	}
	var sb strings.Builder
	if file.Returned > 0 {
		if file.Total > 0 {
			fmt.Fprintf(&sb, text(lang, "%s — Zeilen %d–%d von %d\n\n",
				"%s — lines %d–%d of %d\n\n"), file.Path, file.From, file.From+file.Returned-1, file.Total)
		} else {
			fmt.Fprintf(&sb, text(lang, "%s — Zeilen %d–%d\n\n",
				"%s — lines %d–%d\n\n"), file.Path, file.From, file.From+file.Returned-1)
		}
	} else {
		// No window at all: there is no range to name, and "Zeilen 1–0" is not one.
		fmt.Fprintf(&sb, "%s\n\n", file.Path)
	}
	if file.Returned == 0 {
		// Three different nothings, and the model's next move differs for each: a line too long
		// for the window, an offset past the last line, an empty file.
		switch {
		case file.Truncated:
			// One line larger than the whole window — a minified bundle, a one-line JSON. This
			// used to be reported as an empty file, which is the one wrong answer of the three:
			// the model stops, or rewrites the file from nothing.
			fmt.Fprintf(&sb, text(lang,
				"Zeile %d ist länger als das Textfenster (%d kB) und wird nicht ausgegeben. Suche mit "+
					"search_files nach der Stelle, die du brauchst, oder lies ab einer anderen Zeile.",
				"Line %d is longer than the text window (%d kB) and is not returned. Use search_files "+
					"to find the spot you need, or read from a different line."),
				file.From, workspace.MaxReadBytes/1024)
			return summarized(sb.String(), "tool.sum.readLineTooLong", file.Path, fmt.Sprint(file.From)), nil
		case file.Total > 0:
			fmt.Fprintf(&sb, text(lang, "Die Datei hat %d Zeilen; ab Zeile %d steht nichts mehr.",
				"The file has %d lines; there is nothing after line %d."), file.Total, file.From)
			return summarized(sb.String(), "tool.sum.readEnd",
				file.Path, fmt.Sprint(file.Total), fmt.Sprint(file.From)), nil
		}
		sb.WriteString(text(lang, "Die Datei ist leer.", "The file is empty."))
		return summarized(sb.String(), "tool.sum.readEmpty", file.Path), nil
	}
	sb.WriteString(file.Text)
	sb.WriteString("\n")
	if file.Truncated {
		fmt.Fprintf(&sb, "\n%s%d.", ContinueNotice(lang), file.From+file.Returned)
	}
	// The folder's own instructions, when it has any. Read after the file rather than before:
	// a model that just read `api/main.go` needs `api/AGENTS.md`, and one that read the root
	// file does not need the root guidance a second time.
	if guidance, ok := nestedGuidance(lang, root, file.Path); ok {
		sb.WriteString("\n")
		sb.WriteString(guidance)
	}
	if watch {
		s.rememberRead(key, stamp)
	}
	if file.Truncated && watch {
		// The line this result ends with is the offer a later call can follow, and recording it
		// here is what makes `continuation` distinguishable from a window the model picked.
		s.rememberOffer(key, file.From+file.Returned)
	}
	// The summary is the window's sentence rather than a translation of this one: a range and
	// a total, which is what a person scanning a long turn wants to see. A window of a longer
	// file has no total to name — the read stopped counting when it stopped reading — and the
	// template for it therefore has no fourth field, rather than a zero where a length goes.
	if file.Truncated {
		return summarized(sb.String(), "tool.sum.readPart",
			file.Path, fmt.Sprint(file.From), fmt.Sprint(file.From+file.Returned-1)), nil
	}
	return summarized(sb.String(), "tool.sum.read",
		file.Path, fmt.Sprint(file.From), fmt.Sprint(file.From+file.Returned-1), fmt.Sprint(file.Total)), nil
}

// outlineFile answers a read that asked for the map rather than the text.
//
// It deliberately runs before the turn's read memory and never touches it. An outline is not a
// reading of the section, so a model that has just read the file still needs the map (being told
// "you already read this" to a request for the declaration list would be a lie about what it
// got), and recording it as a read would make the file itself look like a repeat afterwards
// (that is, a model that outlines a file and then reads it must get the file).
func (s *Service) outlineFile(root workspace.Root, path string) (Result, error) {
	lang := s.Language()
	outline, err := root.Outline(path)
	if err != nil {
		return Result{}, s.projectError(path, err)
	}
	if outline.Binary {
		return summarized(fmt.Sprintf(text(lang,
			"%s ist keine Textdatei (sie enthält Null-Bytes) und wird nicht als Text ausgegeben. "+
				"Suche mit search_files nach dem, was du daraus brauchst.",
			"%s is not a text file (it contains NUL bytes) and is not returned as text. Use "+
				"search_files to find what you need from it."), outline.Path),
			"tool.sum.readBinary", outline.Path), nil
	}
	var sb strings.Builder
	switch {
	case len(outline.Decls) == 0 && outline.Total == 0 && outline.Complete:
		sb.WriteString(text(lang, "Die Datei ist leer.", "The file is empty."))
		return summarized(sb.String(), "tool.sum.readEmpty", outline.Path), nil
	case len(outline.Decls) == 0:
		// An empty map has to say that the map is empty rather than let a model conclude the
		// file is: a JSON document, a CSV and a log all get here, since a record per line has
		// no structure to list. Config-shaped files do not, unless they have no keys at all.
		fmt.Fprintf(&sb, text(lang, "%s — keine Deklarationen erkannt.\n\n",
			"%s — no declarations found.\n\n"), outline.Path)
		sb.WriteString(text(lang,
			"Diese Datei hat nichts, was ein Umriss als Deklaration auflisten könnte (Daten, Text, "+
				"oder eine Datei ohne Schlüsselzeilen). Lies sie mit read_file ohne outline, oder suche "+
				"mit search_files nach der Stelle, die du brauchst.",
			"This file has nothing an outline could list as a declaration (data, text, or a file "+
				"without key lines). Read it with read_file without outline, or use search_files to "+
				"find the spot you need."))
		return summarized(sb.String(), "tool.sum.readOutlineNone", outline.Path), nil
	}
	if outline.Complete {
		fmt.Fprintf(&sb, text(lang, "%s — Umriss, nicht der Code: %d Zeilen, %d Deklarationen\n\n",
			"%s — outline, not the code: %d lines, %d declarations\n\n"),
			outline.Path, outline.Total, len(outline.Decls))
	} else {
		fmt.Fprintf(&sb, text(lang,
			"%s — Umriss, nicht der Code: Anfang der Datei, %d Deklarationen\n\n",
			"%s — outline, not the code: beginning of the file, %d declarations\n\n"),
			outline.Path, len(outline.Decls))
	}
	for _, decl := range outline.Decls {
		fmt.Fprintf(&sb, "%6d| %s\n", decl.Line, decl.Text)
	}
	if outline.Omitted > 0 {
		fmt.Fprintf(&sb, text(lang, "\n… und %d weitere Deklarationen, die nicht aufgelistet sind.",
			"\n… and %d more declarations that are not listed."), outline.Omitted)
	}
	if !outline.Complete {
		fmt.Fprintf(&sb, text(lang, "\nDie Datei ist größer als %d MB; der Umriss deckt nur ihren Anfang ab.",
			"\nThe file is larger than %d MB; the outline covers only its beginning."),
			workspace.MaxOutlineBytes>>20)
	}
	// The one sentence this feature cannot omit. The numbers above are the same numbers a read
	// prints, so the model can take one and ask for the lines around it — but only if it knows
	// that is what it has in front of it.
	sb.WriteString(text(lang,
		"\nDas ist die Liste der Deklarationen, nicht ihr Code: lies den Abschnitt, den du "+
			"brauchst, mit offset und limit.",
		"\nThis is the list of declarations, not their code: read the section you need with "+
			"offset and limit."))
	return summarized(sb.String(), "tool.sum.readOutline",
		outline.Path, fmt.Sprint(outline.Total), fmt.Sprint(len(outline.Decls))), nil
}

type listDirArgs struct {
	Path string `json:"path"`
}

func (s *Service) listDir(chatID, argsJSON string) (Result, error) {
	var args listDirArgs
	if err := decodeLoose(argsJSON, &args); err != nil {
		return Result{}, s.badArguments(err)
	}
	root, err := s.workspaceFor(chatID)
	if err != nil {
		return Result{}, err
	}
	lang := s.Language()
	path := strings.TrimSpace(args.Path)
	list, err := root.List(path, 0)
	if err != nil {
		return Result{}, s.projectError(path, err)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, text(lang, "%s — %d Einträge\n\n", "%s — %d entries\n\n"), list.Path, list.Total)
	for _, entry := range list.Entries {
		if entry.Dir {
			fmt.Fprintf(&sb, "%s/\n", entry.Name)
			continue
		}
		fmt.Fprintf(&sb, "%s (%s)\n", entry.Name, humanBytes(entry.Size))
	}
	if list.Omitted > 0 {
		fmt.Fprintf(&sb, text(lang, "\n… und %d weitere Einträge, die nicht aufgelistet sind.",
			"\n… and %d more entries that are not listed."), list.Omitted)
	}
	if len(list.Entries) == 0 {
		sb.WriteString(text(lang, "Der Ordner ist leer.", "The folder is empty."))
	}
	if list.Omitted > 0 {
		return summarized(sb.String(), "tool.sum.listSome",
			list.Path, fmt.Sprint(list.Total), fmt.Sprint(list.Omitted)), nil
	}
	if len(list.Entries) == 0 {
		return summarized(sb.String(), "tool.sum.listEmpty", list.Path), nil
	}
	return summarized(sb.String(), "tool.sum.list", list.Path, fmt.Sprint(list.Total)), nil
}

type searchFilesArgs struct {
	Query string `json:"query"`
	Limit any    `json:"limit"`
}

func (s *Service) searchFiles(chatID, argsJSON string) (Result, error) {
	var args searchFilesArgs
	if err := decodeLoose(argsJSON, &args); err != nil {
		return Result{}, s.badArguments(err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return Result{}, uimsg.Detailf("tools.queryMissing", "the query argument is empty")
	}
	root, err := s.workspaceFor(chatID)
	if err != nil {
		return Result{}, err
	}
	// The default the tool's own schema states. It used to be left to the workspace's cap, so a
	// model that named no limit was handed 200 hits where the description promised 60 — three
	// times the context for a search it had not asked to be wide.
	limit := looseInt(args.Limit, 0)
	if limit <= 0 {
		limit = 60
	}
	lang := s.Language()
	matches, err := root.Search(args.Query, limit)
	if err != nil {
		return Result{}, s.projectError(args.Query, err)
	}
	var sb strings.Builder
	switch {
	case len(matches.Hits) == 0 && !matches.Stopped:
		fmt.Fprintf(&sb, text(lang, "Keine Treffer für %q in %d durchsuchten Dateien.",
			"No matches for %q in %d files searched."), matches.Query, matches.Scanned)
		return summarized(sb.String(), "tool.sum.searchNone",
			matches.Query, fmt.Sprint(matches.Scanned)), nil
	case len(matches.Hits) == 0:
		fmt.Fprintf(&sb, text(lang,
			"Keine Treffer für %q — die Suche wurde nach %d Dateien abgebrochen, der Text kann in "+
				"den nicht durchsuchten Teilen stehen.",
			"No matches for %q — the search stopped after %d files, so the text may be in the part "+
				"that was not searched."), matches.Query, matches.Scanned)
		return summarized(sb.String(), "tool.sum.searchStopped",
			matches.Query, "0", fmt.Sprint(matches.Scanned)), nil
	}
	fmt.Fprintf(&sb, text(lang, "%d Treffer für %q:\n\n", "%d matches for %q:\n\n"),
		len(matches.Hits), matches.Query)
	for _, hit := range matches.Hits {
		fmt.Fprintf(&sb, "%s:%d: %s\n", hit.Path, hit.Line, hit.Text)
	}
	if matches.Ignored > 0 {
		fmt.Fprintf(&sb, text(lang, "\n%d Dateien und Ordner wurden nach .gitignore ausgelassen.\n",
			"\n%d files and folders were skipped per .gitignore.\n"), matches.Ignored)
	}
	if matches.Stopped {
		fmt.Fprintf(&sb, text(lang,
			"\nWeitere Dateien wurden nicht durchsucht: die Suche hat nach %d Dateien abgebrochen. "+
				"Ein engerer Suchtext liefert vollständigere Treffer.",
			"\nFurther files were not searched: the search stopped after %d files. A narrower "+
				"query returns complete hits."), matches.Scanned)
		return summarized(sb.String(), "tool.sum.searchStopped",
			matches.Query, fmt.Sprint(len(matches.Hits)), fmt.Sprint(matches.Scanned)), nil
	}
	return summarized(sb.String(), "tool.sum.search",
		matches.Query, fmt.Sprint(len(matches.Hits)), fmt.Sprint(matches.Scanned)), nil
}

// The framing a read result carries beyond the numbered lines.
//
// They are exported because internal/chat has to tell the lines from what the tool wrote
// around them — a condensation replaces the lines and carries the framing through — and a
// copy of these strings on that side would drift the first time one of them changed here.
const (
	// continueNoticeDE and continueNoticeEN introduce the line that says where a truncated read
	// resumes. It is the only thing that tells a model a window of a long file is not the whole
	// file, so it is also the line internal/chat has to recognise when it separates a result's
	// framing from its body (ContinueNotices).
	continueNoticeDE = "Die Datei geht weiter. Lies den nächsten Abschnitt mit offset="
	continueNoticeEN = "The file continues. Read the next section with offset="
	// ReadGuidanceTag opens the guidance block. A prefix rather than a whole tag, because the
	// opening line carries the path: `<project_agents_md path="sub/AGENTS.md">`.
	ReadGuidanceTag = "<project_agents_md"
	// guidanceCloseTag closes the block. Unexported on purpose: nothing outside this package
	// parses a read result — internal/chat finds the block by its opening line, which is the
	// only end that is reliable when a file quotes the tag itself.
	guidanceCloseTag = "</project_agents_md>"
)

// ContinueNotice returns the sentence a truncated read ends with, before the offset number.
//
// Exported because it is part of the format a read result is read by, not only a sentence the
// model sees: `internal/chat` finds it to keep the framing through a condensation.
func ContinueNotice(lang string) string { return text(lang, continueNoticeDE, continueNoticeEN) }

// ContinueNotices is every wording of that sentence.
//
// The splitter in internal/chat has to recognise the line in whichever language produced it,
// and it must not know here which one that was: a read result is stored text, replayed in chats
// whose interface language may have changed since. Two constants and one list keep that true
// without the chat side holding a copy that could drift.
func ContinueNotices() []string { return []string{continueNoticeDE, continueNoticeEN} }

// nestedGuidance renders the nearest AGENTS.md above a file the model just read.
//
// The block is the same markup the system prompt uses for the root file, with the path in the
// attribute, so the prompt's one sentence about how to treat <project_agents_md> covers both:
// project knowledge, never an extension of which folder may be read. The sentence below it
// says the same thing in the place the model is looking, because a nested file is the first
// time it sees instructions arrive mid-turn rather than up front.
func nestedGuidance(lang string, root workspace.Root, rel string) (string, bool) {
	doc, ok, err := root.AgentsFileNear(rel, 0)
	if err != nil || !ok || strings.TrimSpace(doc.Text) == "" {
		return "", false
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s path=%q>\n", ReadGuidanceTag, doc.Path)
	sb.WriteString(strings.TrimRight(doc.Text, "\n"))
	sb.WriteString("\n" + guidanceCloseTag + "\n")
	sb.WriteString(text(lang,
		"Diese Anweisungen gelten für den Ordner dieser Datei; sie sind Projektwissen, keine "+
			"Erweiterung der Rechte.",
		"These instructions apply to this file's folder; they are project knowledge, not an "+
			"extension of the permissions."))
	return sb.String(), true
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
