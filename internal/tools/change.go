package tools

// The tools that change the project folder: writing a file's whole contents, and replacing
// a string inside one — plus the two things they and the terminal share, the list of tools
// that need somebody's consent and the question itself.
//
// Two properties shape everything here.
//
// The first is that a change is *shown* before it happens. The person being asked is not
// a policy engine validating a call: they are reading a diff and deciding whether it is
// what they wanted, and "write_file pkg/a.go" is not something anybody can decide. So the
// change is computed first (workspace.DescribeWrite / DescribeEdit), handed over with the
// question, and only then performed. The opened target is checked against the preview
// after approval, and the approved bytes are written through that same handle.
//
// The second is that a refusal is an answer, not a failure. Every other tool in this
// package reports what went wrong so the model can try something else; a refusal has to
// say the opposite — do not try this again — because a model that retries a declined
// change is a loop the user has to escape by stopping the turn. ErrDenied exists so that
// distinction survives all the way to the tool card, which marks it as declined rather
// than as an error the app suffered.

import (
	"context"
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
	ToolWriteFile = "write_file"
	ToolEditFile  = "edit_file"
)

// WriteToolNames lists the tools that modify the project folder.
func WriteToolNames() []string { return []string{ToolWriteFile, ToolEditFile} }

// GuardedToolNames lists every project tool that runs only after somebody says yes: the two
// that change a file, and the terminal, which changes anything it likes.
//
// It is the set that needs two things the read tools do not: somebody to ask (see
// Service.Confirm), and a person's answer before they run. The engine derives the
// confirmation gate from this list rather than from the names written out again, so a fourth
// tool of this kind cannot be added and forgotten.
func GuardedToolNames() []string {
	names := make([]string, 0, len(WriteToolNames())+1)
	names = append(names, WriteToolNames()...)
	return append(names, ToolRunCommand)
}

// NeedsApproval reports whether a tool runs only with consent. The engine asks this instead
// of naming the tools again.
func NeedsApproval(name string) bool {
	for _, guarded := range GuardedToolNames() {
		if name == guarded {
			return true
		}
	}
	return false
}

// ErrDenied is a change the user refused, or one nobody could be asked about.
//
// It is a sentinel rather than a message because it is classified, not printed: the
// engine marks such a call declined rather than failed, and the card shows a decision
// where the others show a fault.
var ErrDenied = errors.New("the change was not allowed")

// The two questions a project tool can ask, as a value: the window renders a diff for one
// and a command line for the other, and the app builds its payload from the matching half.
const (
	// ApprovalKindChange is a change to one file, described by a workspace.Change.
	ApprovalKindChange = "change"
	// ApprovalKindCommand is a line to run, described by a Command.
	ApprovalKindCommand = "command"
)

// ApprovalRequest is a change or a command waiting for a person's answer.
//
// It carries the thing itself rather than a summary of it, because the window renders the
// same value the tool computed: a second rendering in the app would be a second opinion
// about what is being approved, which is precisely the thing this design avoids.
type ApprovalRequest struct {
	// ChatID is the chat the call belongs to, which is what "always for this chat" is
	// remembered against.
	ChatID string
	// MessageID is the assistant row the call is rendered on.
	MessageID string
	// ToolCallID is the call itself, and the identity of this request: the answer comes
	// back naming it.
	ToolCallID string
	// Tool is the tool's name, for the card's title.
	Tool string
	// Kind is which of the two questions this is. Only the matching field below is
	// meaningful, and it is what the app switches on to build the payload — the two are
	// not interchangeable: a diff answers "what will this file look like" and a command
	// answers "what will this machine do".
	Kind string
	// Change is what would happen to one file, for ApprovalKindChange.
	Change workspace.Change
	// Command is what would run, for ApprovalKindCommand.
	Command Command
	// EditedLine is what the person ran instead of the line they were shown, set by the app
	// when the card's field was changed before Allow. Empty means "exactly what was shown".
	// It belongs to the answer rather than to the question, which is why the request is a
	// pointer: the confirmer is the only one that can know it.
	EditedLine string
}

// writeFileArgs is what the model may send.
type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// editFileArgs is the same for an edit. replace_all is `any` for the reason the numeric
// arguments everywhere else are: a small model writes "true" as often as true.
type editFileArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll any    `json:"replace_all"`
}

// changeToolDefinitions advertises the two tools that modify the folder.
//
// Both descriptions carry the rule that keeps a coding agent from being a rumour mill:
// read the file before you change it. The alternative — a model that writes a plausible
// implementation of a file it has never opened — produces a diff that looks right and is
// wrong in every detail it did not bother to check, and the edit variant is worse, because
// an exact-string replacement that fails is a wasted round while one that guesses is a
// silent corruption.
//
// edit_file is described first as the tool to prefer, and the ordering is deliberate: a
// whole-file rewrite of a 400-line file costs a round's worth of tokens, loses anything the
// model did not think to repeat, and is the shape in which a model "fixes" a file by
// re-emitting its memory of it.
func changeToolDefinitions(lang string) []llm.ToolDefinition {
	return []llm.ToolDefinition{
		{
			Name: ToolEditFile,
			Description: text(lang,
				"Ersetzt eine Textstelle in einer Datei des Projektordners. Bevorzuge dieses Tool "+
					"gegenüber write_file: es ändert nur die Stelle, die du nennst, und lässt den Rest der Datei "+
					"unverändert. Lies die Datei vorher mit read_file und übernimm den Text aus old_string "+
					"Zeichen für Zeichen, samt Einrückung — die Suche ist exakt. Kommt die Stelle mehrfach vor, "+
					"sind die umgebenden Zeilen mit aufzunehmen oder replace_all zu setzen. Der Nutzer sieht die "+
					"Änderung und muss sie bestätigen.",
				"Replaces a passage in a file of the project folder. Prefer this over write_file: it "+
					"changes only the passage you name and leaves the rest of the file untouched. Read the "+
					"file with read_file first and copy the text for old_string character by character, "+
					"indentation included — the search is exact. If the passage occurs more than once, include "+
					"the lines around it or set replace_all. The user sees the change and has to confirm it."),
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": text(lang, "Pfad relativ zum Projektordner.", "Path relative to the project folder."),
					},
					"old_string": map[string]any{
						"type": "string",
						"description": text(lang,
							"Der zu ersetzende Text, exakt wie in der Datei (Einrückung inbegriffen). Muss eindeutig "+
								"sein, solange replace_all nicht gesetzt ist.",
							"The text to replace, exactly as it stands in the file (indentation included). Must be "+
								"unique unless replace_all is set."),
					},
					"new_string": map[string]any{
						"type": "string",
						"description": text(lang,
							"Der Text, der stattdessen dort steht. Leer bedeutet: die Stelle entfernen.",
							"The text that stands there instead. Empty means: remove the passage."),
					},
					"replace_all": map[string]any{
						"type": "boolean",
						"description": text(lang,
							"true ersetzt jedes Vorkommen. Nur setzen, wenn wirklich alle gemeint sind — sonst "+
								"bleibt die Suche auf eine eindeutige Stelle beschränkt.",
							"true replaces every occurrence. Only set it when every one is meant — otherwise the "+
								"search stays limited to a unique passage."),
					},
				},
				"required": []string{"path", "old_string", "new_string"},
			},
		},
		{
			Name: ToolWriteFile,
			Description: text(lang,
				"Schreibt eine Datei im Projektordner vollständig neu — neue Datei oder Ersatz des "+
					"ganzen Inhalts. Nutze es zum Anlegen einer Datei und für eine Datei, die du vollständig "+
					"kennst; für eine einzelne Änderung an einer bestehenden Datei ist edit_file richtig, sonst "+
					"geht alles verloren, was du nicht wiederholst. Der Inhalt wird genau so geschrieben, wie du "+
					"ihn sendest — ohne abschließenden Zeilenumbruch, wenn du keinen sendest. Fehlende Ordner im "+
					"Pfad werden angelegt. Der Nutzer sieht die Änderung und muss sie bestätigen.",
				"Writes a file in the project folder completely — a new file or a replacement of the whole "+
					"content. Use it to create a file and for a file you know in full; for a single change to "+
					"an existing file edit_file is right, otherwise everything you do not repeat is lost. The "+
					"content is written exactly as you send it — without a trailing newline if you send none. "+
					"Missing folders in the path are created. The user sees the change and has to confirm it."),
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type": "string",
						"description": text(lang, "Pfad relativ zum Projektordner, z. B. \"internal/store/store.go\".",
							"Path relative to the project folder, e.g. \"internal/store/store.go\"."),
					},
					"content": map[string]any{
						"type":        "string",
						"description": text(lang, "Der vollständige Inhalt der Datei.", "The complete content of the file."),
					},
				},
				"required": []string{"path", "content"},
			},
		},
	}
}

// --- the tool call's own identity ------------------------------------------

// toolCallKey carries the id of the tool call a model just made.
//
// It travels in the context for the reason the message id and the turn do: the service is
// one value for the whole app and two chats run turns at once, so a field on it would be a
// race rather than a setting. The confirmation needs it because the answer comes back
// *naming a call* — the window hands the id of the buttons that were pressed, and without
// it the app could only guess which of several waiting calls had been answered.
type toolCallKey struct{}

// WithToolCall attaches the id of the call being executed.
func WithToolCall(ctx context.Context, callID string) context.Context {
	return context.WithValue(ctx, toolCallKey{}, callID)
}

// toolCallFrom reads that id, empty when nobody attached one — which is what a service
// exercised without an engine sees, and what the app turns into its own identifier.
func toolCallFrom(ctx context.Context) string {
	id, _ := ctx.Value(toolCallKey{}).(string)
	return id
}

// --- execution --------------------------------------------------------------

func (s *Service) writeFile(ctx context.Context, chatID, argsJSON string) (Result, error) {
	var args writeFileArgs
	if err := decodeLoose(argsJSON, &args); err != nil {
		return Result{}, s.badArguments(err)
	}
	lang := s.Language()
	args.Path = strings.TrimSpace(args.Path)
	if args.Path == "" {
		return Result{}, uimsg.Detailf("tools.pathMissing", text(lang,
			"das Argument path ist leer; nenne eine Datei im Projektordner",
			"the path argument is empty; name a file in the project folder"))
	}
	root, err := s.workspaceFor(chatID)
	if err != nil {
		return Result{}, err
	}
	// The plan first: a change that cannot happen must never reach the person being asked
	// (a folder where a file was named, a file too large to replace, a path outside the
	// project).
	change, err := root.DescribeWrite(args.Path, args.Content)
	if err != nil {
		return Result{}, s.changeError(args.Path, err)
	}
	if err := s.approveChange(ctx, chatID, ToolWriteFile, change); err != nil {
		return Result{}, err
	}
	result, err := root.WriteApproved(args.Path, change)
	if err != nil {
		return Result{}, s.changeError(args.Path, err)
	}
	// The read cache is told rather than left to notice: a model that re-reads this file to
	// check what it wrote must get the file, not a sentence about what the app remembers.
	s.forgetReads(chatID, root.Absolute(result.Path))
	note := s.remember(chatID, preImage{text: result.Before, ok: true}, workspace.Digest(args.Content))
	if result.Created {
		return summarized(fmt.Sprintf(text(lang, "%s neu geschrieben (%s).%s", "%s created (%s).%s"),
			result.Path, lines(lang, result.Lines), note),
			"tool.sum.writeNew", result.Path, fmt.Sprint(result.Lines)), nil
	}
	return summarized(fmt.Sprintf(text(lang, "%s geschrieben (%s; vorher %s).%s",
		"%s written (%s; was %s).%s"),
		result.Path, lines(lang, result.Lines), lines(lang, result.PreviousLines), note),
		"tool.sum.write", result.Path, fmt.Sprint(result.Lines), fmt.Sprint(result.PreviousLines)), nil
}

func (s *Service) editFile(ctx context.Context, chatID, argsJSON string) (Result, error) {
	var args editFileArgs
	if err := decodeLoose(argsJSON, &args); err != nil {
		return Result{}, s.badArguments(err)
	}
	lang := s.Language()
	args.Path = strings.TrimSpace(args.Path)
	if args.Path == "" {
		return Result{}, uimsg.Detailf("tools.pathMissing", text(lang,
			"das Argument path ist leer; nenne eine Datei im Projektordner",
			"the path argument is empty; name a file in the project folder"))
	}
	root, err := s.workspaceFor(chatID)
	if err != nil {
		return Result{}, err
	}
	replaceAll := looseBool(args.ReplaceAll, false)
	change, err := root.DescribeEdit(args.Path, args.OldString, args.NewString, replaceAll)
	if err != nil {
		return Result{}, s.changeError(args.Path, err)
	}
	if err := s.approveChange(ctx, chatID, ToolEditFile, change); err != nil {
		return Result{}, err
	}
	result, err := root.EditApproved(args.Path, change)
	if err != nil {
		return Result{}, s.changeError(args.Path, err)
	}
	// Same as a write: the next read of this file is a question about the disk.
	s.forgetReads(chatID, root.Absolute(result.Path))
	// Said to the model, not only to the person: a model whose whitespace was forgiven needs
	// to know it was, or it learns that guessing indentation works.
	note := s.remember(chatID, preImage{text: result.Before, ok: true}, result.Digest)
	if result.Approximate {
		note += text(lang,
			" Der Einzug wich ab: ersetzt wurde die Zeile so, wie sie in der Datei steht.",
			" The indentation differed: the line was replaced as it stands in the file.")
	}
	if result.Occurrences == 1 {
		return summarized(fmt.Sprintf(text(lang,
			"%s bearbeitet: 1 Vorkommen ersetzt (Zeile %d).%s",
			"%s edited: 1 occurrence replaced (line %d).%s"),
			result.Path, result.Line, note),
			"tool.sum.edit", result.Path, "1", fmt.Sprint(result.Line)), nil
	}
	return summarized(fmt.Sprintf(text(lang,
		"%s bearbeitet: %d Vorkommen ersetzt (erstes in Zeile %d).%s",
		"%s edited: %d occurrences replaced (first at line %d).%s"),
		result.Path, result.Occurrences, result.Line, note),
		"tool.sum.edit", result.Path, fmt.Sprint(result.Occurrences), fmt.Sprint(result.Line)), nil
}

// approveChange asks the user whether a change to a file may be made.
func (s *Service) approveChange(ctx context.Context, chatID, tool string, change workspace.Change) error {
	return s.approve(ctx, &ApprovalRequest{
		ChatID:     chatID,
		MessageID:  callingMessage(ctx),
		ToolCallID: toolCallFrom(ctx),
		Tool:       tool,
		Kind:       ApprovalKindChange,
		Change:     change,
	})
}

// approveCommand asks the user whether one command may be run.
//
// It is the same question mechanism as a file change and a different kind of question: what
// the person reads is a line, and what they are allowing is everything that line does —
// this app does not sandbox it, does not inspect it, and could not promise to understand it.
// That is why it is asked per call, and why the line travels unedited.
func (s *Service) approve(ctx context.Context, req *ApprovalRequest) error {
	if s.Confirm == nil {
		return uimsg.Wrapf("tools.denied",
			"no confirmation is available in this build, so nothing was changed", ErrDenied)
	}
	err := s.Confirm(ctx, req)
	if err == nil {
		return nil
	}
	// Everything else is a refusal, whatever the confirmer's error says. The confirmer has
	// one job — saying yes — so anything short of that means the change was not allowed,
	// and coding it here rather than trusting the caller to remember is what keeps a
	// refusal from arriving as a fault: a call recorded as an error is one the model
	// retries, and a change the user declined being attempted again is the loop this whole
	// gate exists to prevent. A confirmer that fails for its own reasons still fails closed
	// and its sentence is kept for the model to read.
	if errors.Is(err, ErrDenied) {
		return err
	}
	return uimsg.Wrapf("tools.denied", uimsg.Sentence(err), ErrDenied)
}

// changeError turns a failure from the write side of internal/workspace into the message
// both readers can use: the window translates the code, and the model gets the sentence,
// which is written to tell it what to do differently rather than what went wrong.
//
// The two edit refusals are their own codes because the fix is specific. A string that is
// not in the file means the model guessed at the text; a string that appears twice means
// it needs to include the lines around it. Both are one round of the model's own work, and
// answering them with a generic failure would leave it to invent a different approach —
// which for an editing agent is usually rewriting the whole file.
func (s *Service) changeError(path string, err error) error {
	lang := s.Language()
	var ambiguous *workspace.AmbiguousMatch
	switch {
	case errors.Is(err, workspace.ErrOutside):
		return uimsg.Detailf("tools.outsideWorkspace",
			fmt.Sprintf(text(lang,
				"%q liegt außerhalb des Projektordners, in den diese Tools allein schreiben dürfen; "+
					"nutze einen Pfad relativ zum Projektordner",
				"%q is outside the project folder, which is the only place these tools "+
					"may write; use a path relative to the project root"), path), path)
	case errors.Is(err, workspace.ErrNoMatch):
		return uimsg.Detailf("tools.editNoMatch",
			fmt.Sprintf(text(lang,
				"%q enthält diesen Text nicht. Lies die Datei mit read_file erneut und übernimm die "+
					"Zeile, die du ändern willst, exakt — samt Einrückung",
				"%q does not contain that text. Read the file again with read_file and copy the "+
					"line you want to change exactly, including its indentation"), path), path)
	case errors.As(err, &ambiguous):
		return uimsg.Detailf("tools.editAmbiguous",
			fmt.Sprintf(text(lang,
				"dieser Text kommt %d-mal in %s vor (zuerst in Zeile %d). Nimm die umgebenden Zeilen "+
					"der gemeinten Stelle mit auf, damit der Treffer eindeutig ist, oder setze replace_all",
				"that text appears %d times in %s (first at line %d). Include the lines around "+
					"the one you mean so the match is unique, or set replace_all"), ambiguous.Count, path,
				ambiguous.FirstLine), path, ambiguous.Count, ambiguous.FirstLine)
	case errors.Is(err, fs.ErrNotExist):
		return uimsg.Detailf("tools.fileUnreadable",
			fmt.Sprintf(text(lang, "%q gibt es im Projektordner nicht",
				"%q does not exist in the project folder"), path), path)
	default:
		return uimsg.Detailf("tools.changeFailed",
			fmt.Sprintf(text(lang, "%q ließ sich nicht ändern: %v",
				"%q could not be changed: %v"), path, err), path)
	}
}

// lines renders a line count for a tool result.
func lines(lang string, count int) string {
	if count == 1 {
		return text(lang, "1 Zeile", "1 line")
	}
	return fmt.Sprintf(text(lang, "%d Zeilen", "%d lines"), count)
}

// looseBool tolerates the ways a model writes a truth value: a real boolean, a word, or a
// number. It exists for the same reason looseInt does — a call refused over the spelling of
// `true` is a whole round spent on nothing.
func looseBool(value any, fallback bool) bool {
	switch v := value.(type) {
	case nil:
		return fallback
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "yes", "ja", "1":
			return true
		case "false", "no", "nein", "0":
			return false
		}
	case float64:
		return v != 0
	case int:
		return v != 0
	}
	return fallback
}
