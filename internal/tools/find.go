package tools

// Finding files by name, as distinct from reading them or searching inside them.
//
// Why it is its own tool rather than an argument of list_dir: the question a model asks is
// "where is the file that does X" or "which tests exist", and both are answered by a pattern
// over the whole tree. `list_dir` walks one level at a time, so a deep tree costs a dozen
// rounds and a lot of context, and `search_files` needs a string that is inside the file the
// model has not found yet.

import (
	"fmt"
	"strings"

	"kichat/internal/llm"
	"kichat/internal/uimsg"
	"kichat/internal/workspace"
)

// findFilesDefinition describes it.
//
// The description names the two shapes that matter — a bare name matches at any depth, a path
// with a slash is anchored — because a model that misreads that either gets nothing or gets
// the whole tree.
func findFilesDefinition(lang string) llm.ToolDefinition {
	return llm.ToolDefinition{
		Name: ToolFindFiles,
		Description: text(lang,
			"Sucht Dateien im Projektordner über ein Pfadmuster und liefert ihre Pfade, alphabetisch. "+
				"Ein Muster ohne Schrägstrich passt auf den Dateinamen in jeder Tiefe (\"*.go\", \"Makefile\"); "+
				"eines mit Schrägstrich gilt ab dem Projektordner (\"internal/**/*_test.go\"). `*` bleibt "+
				"innerhalb eines Ordners, `**` geht über Ordner hinweg. Nutze dieses Tool, um eine Datei zu "+
				"finden, statt Ordner einzeln aufzulisten; Build-Ausgaben, Abhängigkeiten und .gitignore "+
				"bleiben außen vor.",
			"Searches the project folder for files by a path pattern and returns their paths, "+
				"alphabetically. A pattern without a slash matches the file name at any depth (\"*.go\", "+
				"\"Makefile\"); one with a slash is anchored at the project folder "+
				"(\"internal/**/*_test.go\"). `*` stays inside one folder, `**` crosses folders. Use it to "+
				"find a file instead of listing folders one by one; build output, dependencies and "+
				".gitignore stay out."),
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type": "string",
					"description": text(lang, "Pfadmuster, z. B. \"**/*.go\" oder \"internal/tools/*.go\".",
						"Path pattern, e.g. \"**/*.go\" or \"internal/tools/*.go\"."),
				},
				"limit": map[string]any{
					"type": "integer",
					"description": text(lang, "Höchstzahl der Pfade (1-500, Standard 200).",
						"Maximum number of paths (1-500, default 200)."),
				},
			},
			"required": []string{"pattern"},
		},
	}
}

type findFilesArgs struct {
	Pattern string `json:"pattern"`
	Limit   any    `json:"limit"`
}

func (s *Service) findFiles(chatID, argsJSON string) (Result, error) {
	var args findFilesArgs
	if err := decodeLoose(argsJSON, &args); err != nil {
		return Result{}, s.badArguments(err)
	}
	lang := s.Language()
	pattern := strings.TrimSpace(args.Pattern)
	if pattern == "" {
		return Result{}, uimsg.Detailf("tools.patternMissing", text(lang,
			"das Argument pattern ist leer; nenne ein Muster wie **/*.go",
			"the pattern argument is empty; name a pattern such as **/*.go"))
	}
	root, err := s.workspaceFor(chatID)
	if err != nil {
		return Result{}, err
	}
	limit := looseInt(args.Limit, 0)
	if limit <= 0 {
		limit = 200
	}
	found, err := root.Find(pattern, limit)
	if err != nil {
		return Result{}, uimsg.Detailf("tools.badPattern",
			fmt.Sprintf(text(lang, "das Muster %q ließ sich nicht verwenden: %v",
				"the pattern %q could not be used: %v"), pattern, err), pattern)
	}
	return formatFound(lang, found), nil
}

func formatFound(lang string, found workspace.Found) Result {
	var sb strings.Builder
	switch {
	case len(found.Paths) == 0:
		if found.ScanLimit {
			fmt.Fprintf(&sb, text(lang,
				"Keine Treffer für %q unter %d geprüften Dateien; die Suche wurde vor dem Ende abgebrochen.",
				"No matches for %q among %d files checked; the search stopped before reaching the end."),
				found.Pattern, found.Scanned)
			return summarized(sb.String(), "tool.sum.find", "0", found.Pattern)
		}
		fmt.Fprintf(&sb, text(lang, "Keine Datei passt auf %q (%d Dateien geprüft).",
			"No file matches %q (%d files checked)."), found.Pattern, found.Scanned)
		if found.Ignored > 0 {
			fmt.Fprintf(&sb, text(lang, " %d Einträge wurden nach .gitignore ausgelassen.",
				" %d entries were skipped per .gitignore."), found.Ignored)
		}
		return summarized(sb.String(), "tool.sum.find", "0", found.Pattern)
	}
	fmt.Fprintf(&sb, text(lang, "%d Dateien für %q:\n\n", "%d files for %q:\n\n"),
		len(found.Paths), found.Pattern)
	for _, path := range found.Paths {
		sb.WriteString(path)
		sb.WriteString("\n")
	}
	if found.ScanLimit {
		sb.WriteString(text(lang,
			"\nDie Suche wurde vor dem Ende abgebrochen; weitere Treffer sind möglich.\n",
			"\nThe search stopped before reaching the end; there may be more matches.\n"))
	} else if found.Truncated {
		sb.WriteString(text(lang,
			"\nEs gibt weitere Treffer; ein engeres Muster liefert eine vollständige Liste.\n",
			"\nThere are more matches; a narrower pattern returns a complete list.\n"))
	}
	return summarized(sb.String(), "tool.sum.find",
		fmt.Sprint(len(found.Paths)), found.Pattern)
}
