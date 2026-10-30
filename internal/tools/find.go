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
)

// findFilesDefinition describes it.
//
// The description names the two shapes that matter — a bare name matches at any depth, a path
// with a slash is anchored — because a model that misreads that either gets nothing or gets
// the whole tree.
func findFilesDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name: ToolFindFiles,
		Description: "Sucht Dateien im Projektordner über ein Pfadmuster und liefert ihre Pfade, alphabetisch. " +
			"Ein Muster ohne Schrägstrich passt auf den Dateinamen in jeder Tiefe (\"*.go\", \"Makefile\"); " +
			"eines mit Schrägstrich gilt ab dem Projektordner (\"internal/**/*_test.go\"). `*` bleibt " +
			"innerhalb eines Ordners, `**` geht über Ordner hinweg. Nutze dieses Tool, um eine Datei zu " +
			"finden, statt Ordner einzeln aufzulisten; Build-Ausgaben, Abhängigkeiten und .gitignore " +
			"bleiben außen vor.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Pfadmuster, z. B. \"**/*.go\" oder \"internal/tools/*.go\".",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Höchstzahl der Pfade (1-500, Standard 200).",
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
		return Result{}, badArguments(err)
	}
	pattern := strings.TrimSpace(args.Pattern)
	if pattern == "" {
		return Result{}, uimsg.Detailf("tools.patternMissing",
			"the pattern argument is empty; name a pattern such as **/*.go")
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
			fmt.Sprintf("the pattern %q could not be used: %v", pattern, err), pattern)
	}
	var sb strings.Builder
	switch {
	case len(found.Paths) == 0:
		fmt.Fprintf(&sb, "Keine Datei passt auf %q (%d Dateien geprüft).", found.Pattern, found.Scanned)
		if found.Ignored > 0 {
			fmt.Fprintf(&sb, " %d Einträge wurden nach .gitignore ausgelassen.", found.Ignored)
		}
		return summarized(sb.String(), "tool.sum.find", "0", found.Pattern), nil
	}
	fmt.Fprintf(&sb, "%d Dateien für %q:\n\n", len(found.Paths), found.Pattern)
	for _, path := range found.Paths {
		sb.WriteString(path)
		sb.WriteString("\n")
	}
	if found.Truncated {
		sb.WriteString("\nEs gibt weitere Treffer; ein engeres Muster liefert eine vollständige Liste.\n")
	}
	return summarized(sb.String(), "tool.sum.find",
		fmt.Sprint(len(found.Paths)), found.Pattern), nil
}
