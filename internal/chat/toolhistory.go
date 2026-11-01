package chat

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"kichat/internal/llm"
	"kichat/internal/tools"
	"kichat/internal/uimsg"
	"kichat/internal/workspace"
)

// A tool call has to name a tool. Two things can break that: a model that mangles
// the name (Qwen and Llama builds on Groq have been seen folding an extra word into
// `function.name` — the observed failure quotes `scr scrape_url` where the request
// declared `scrape_url`), and a stored history that carries such a name forward.
//
// Groq refuses the *generated* call with HTTP 400 "attempted to call tool … which
// was not in request.tools" before a token streams, so nothing reaches the app and no
// history is written — the turn simply fails. A backend that does not validate (a
// local llama.cpp, LM Studio, Ollama) lets the same call through, and there the app
// decides: it executes the name it was given, answers `unbekanntes Tool`, and stores
// the call in the assistant row every later turn replays.
//
// This file is that decision: a mangled name is repaired where the intent is
// unambiguous, dropped where it is not, and a history that already holds a name the
// app cannot execute is demoted to plain text rather than sent as a tool call.
//
// The three sentences a repair produces are `uimsg` codes, not text: they are shown as a toast
// in the window, and a German sentence here reached an English and a Chinese window unchanged.
// They are written as literals at the call site rather than as constants, because that is what
// the dictionary test in the root package reads (`uimsg.*("code"`) — a code behind an identifier
// is a code nothing checks for a translation.
//
// droppedToolText below is the exception and stays a sentence, because it is written into a
// stored message: the transcript shows it as it was recorded, and the model reads it as part of
// the history it replays.

// The two numbers that decide how much tool material one reply keeps in the request.
//
// A tool result is the only thing in a coding reply that grows without bound: every round adds
// another file, another build log, another search, and every *later* round re-sends all of them.
// That is the quadratic term in the cost of an answer — a 25-round reply that ends at 150 000
// tokens sent several million — and no threshold fixes it, because the growth happens *inside*
// the reply, between the folds.
//
// So the newest results are kept verbatim and the older ones are replaced by a sentence saying
// what was dropped and how to get it back. The transcript is untouched: the tool card still
// shows the full record, and the model can call the tool again, which for a file read is the
// ordinary way to refresh a window anyway (the app forgets the earlier read when it drops the
// result — see Engine.buildAssembly).
//
// The budget is not a share of the window on purpose. A 1M-token model does not make a 400 000-
// token request cheaper or more useful; it only makes it possible, and this is the number that
// decides what a reply actually carries. 32 000 tokens is roughly the last half-dozen file
// windows, which is what an edit round is working from.
const (
	toolsKeptVerbatimTokens = 32_000
	// toolStubMinTokens is the size below which a result is never dropped. A short result is
	// cheap to keep and is often the one that matters — an error line, a test summary, the
	// notice that a file is unchanged — while replacing it would cost more explanation than
	// it saves.
	toolStubMinTokens = 600
)

// ageToolResults keeps the newest tool results of the current reply and stubs the older ones.
//
// currentReply is index-aligned with msgs and says which messages belong to the reply being
// answered: results from earlier turns are left alone, because those are what compaction folds
// and a reply has no business rewriting a previous answer's evidence.
//
// droppedReads names the files whose `read_file` result no longer reaches the model in any
// form, so the caller can let the model read them again; saved is the token difference, which
// the caller subtracts from the budget so the meter describes the request that is sent rather
// than the one that could have been.
//
// applied is index-aligned with msgs and says which results actually ran. It is what keeps an
// edit that was denied or that failed from marking a read stale: the file did not change, and a
// model told otherwise either re-reads text it already has or believes a change landed that
// never did.
func ageToolResults(msgs []llm.Message, currentReply, applied []bool) (aged []llm.Message,
	droppedReads []string, saved int) {

	arguments := map[string]string{}
	for _, m := range msgs {
		for _, call := range m.ToolCalls {
			arguments[call.ID] = call.Arguments
		}
	}

	aged = make([]llm.Message, len(msgs))
	copy(aged, msgs)

	// Re-reads go first, and before the budget is counted: the text an outdated read would
	// otherwise spend the budget on is not the text of the file any more.
	stubbed := map[int]bool{}
	saved += supersedeReads(aged, currentReply, arguments, stubbed)
	// Then the reads an edit or a rewrite in this reply has moved past. After the re-read rule,
	// because a read a newer one already covers is gone entirely and needs no marking.
	saved += markStaleReads(aged, currentReply, applied, arguments, stubbed)

	kept := 0
	for i := len(aged) - 1; i >= 0; i-- {
		if i >= len(currentReply) || !currentReply[i] || aged[i].Role != llm.RoleTool {
			continue
		}
		size := llm.EstimateTokens(aged[i].Content)
		if kept+size <= toolsKeptVerbatimTokens || size < toolStubMinTokens {
			kept += size
			continue
		}
		stub := toolStub(aged[i].Name, size)
		aged[i].Content = stub
		stubbed[i] = true
		kept += llm.EstimateTokens(stub)
		saved += size - llm.EstimateTokens(stub)
	}

	// A file is only forgotten when no copy of it is sent at all. While an intact read of it
	// remains, the tool's own "already read in this turn" answer is still true about this
	// request — and re-reading the file would cost a round to fetch text the model has.
	intact := map[string]bool{}
	for i, m := range aged {
		if stubbed[i] || i >= len(currentReply) || !currentReply[i] || m.Role != llm.RoleTool {
			continue
		}
		if window, ok := readWindowOf(m.Name, arguments[m.ToolCallID]); ok {
			intact[window.path] = true
		}
	}
	seen := map[string]bool{}
	for i, m := range aged {
		if !stubbed[i] || i >= len(currentReply) || !currentReply[i] || m.Role != llm.RoleTool {
			continue
		}
		window, ok := readWindowOf(m.Name, arguments[m.ToolCallID])
		if !ok || intact[window.path] || seen[window.path] {
			continue
		}
		seen[window.path] = true
		droppedReads = append(droppedReads, window.path)
	}
	return aged, droppedReads, saved
}

// supersedeReads leaves out every read of a file whose lines a later read in the same reply has
// covered as well, and says how many tokens that saved.
//
// A model re-reads constant-y: after an edit to check the text landed, after a failed test to
// see the line again, after a round of thinking to be sure. Each of those is a whole window in
// the request, and every later round re-sends all of them — so by the end of a reply that read
// one file five times, four of those copies are the file as it *was*, sitting beside the copy
// that is the file as it is. That is a cost, and it is also a correctness problem: a model
// holding both versions quotes whichever it reads first, and the one it just replaced is the
// one that says what it was about to fix.
//
// So the newest read of a window wins outright, without waiting for the budget to run out. A
// re-read of a *different* section is not a replacement: a 2 000-line file is read in windows,
// and the window read at line 900 is not the window read at line 1. The rule is therefore
// coverage of lines, not the path alone.
//
// Two things keep it from being destructive. It is scoped to the reply being answered, like
// the ageing below it — an earlier answer's evidence is compaction's to summarise. And a
// result under toolStubMinTokens is kept: the notice would cost about what the copy does, and
// the floor exists so that cheap results never turn into explanations.
func supersedeReads(msgs []llm.Message, currentReply []bool, arguments map[string]string,
	stubbed map[int]bool) (saved int) {

	// Walked newest-first, so what a read has to be measured against is already collected.
	newer := map[string][]readWindow{}
	for i := len(msgs) - 1; i >= 0; i-- {
		if i >= len(currentReply) || !currentReply[i] || msgs[i].Role != llm.RoleTool {
			continue
		}
		window, ok := readWindowOf(msgs[i].Name, arguments[msgs[i].ToolCallID])
		if !ok {
			continue
		}
		replaced := false
		for _, later := range newer[window.path] {
			if later.covers(window) {
				replaced = true
				break
			}
		}
		if !replaced {
			newer[window.path] = append(newer[window.path], window)
			continue
		}
		size := llm.EstimateTokens(msgs[i].Content)
		if size < toolStubMinTokens {
			continue
		}
		stub := supersededStub(window, size)
		msgs[i].Content = stub
		stubbed[i] = true
		saved += size - llm.EstimateTokens(stub)
	}
	return saved
}

// supersededStub is what the model reads in place of a read a later one replaced.
//
// It says which lines, that a newer copy is further down, and that a re-read is the way back —
// the three things a model needs to not mistake a missing window for a file it has not seen.
func supersededStub(window readWindow, tokens int) string {
	return fmt.Sprintf("[Tool-Ergebnis von %s (%s): %d Tokens. Eine spätere Lesung in dieser Antwort "+
		"hat denselben Bereich abgedeckt; weiter unten steht der aktuelle Text, hier wurde der "+
		"ältere Stand weggelassen. Brauchst du ihn doch, lies den Bereich erneut.]",
		tools.ToolReadFile, window.label(), tokens)
}

// markStaleReads marks every read of a file that a later write or edit in the same reply has
// changed, and returns the token difference in the sign the caller uses: positive for a window
// it took out, negative for the sentence it added.
//
// The re-read rule only helps a model that re-reads. The other shape is the expensive one: read
// a window, edit three lines in it, edit another passage below, and never look again — the model
// now holds the file as it was for the rest of the reply, and the next thing it quotes from that
// window is text it has already replaced. Cline removes the older version of a file outright;
// what happens here depends on what the tool did, because the two are not the same amount of
// loss:
//
//   - `write_file` replaces the whole file, so every line of the earlier read is gone — and the
//     new text is already in the request, in the call's own arguments, which is how the model
//     sees its own write. The read is therefore stubbed like a superseded one: nothing is lost
//     that the request does not carry twice.
//   - `edit_file` replaces a passage, and the window around it is still exactly right. Evicting
//     it would take away the surrounding lines the next edit is copied from and force a re-read
//     round — more tokens than the note costs, spent to fetch text the model just had. The read
//     therefore stays, with one sentence in front of it saying which file changed since and
//     where the new text is.
//
// Not marked: what a `run_command` may have rewritten. A command is the tool whose effect cannot
// be enumerated (`gofmt -w .`, a code generator), but it is also the tool that usually changes
// nothing — a test run, a listing, a grep — and marking every read after every command would put
// a warning on most windows in a reply for no reason. The read memory is still dropped after one
// (`Service.forgetAllReads`), so a model that does re-read gets the disk.
func markStaleReads(msgs []llm.Message, currentReply, applied []bool, arguments map[string]string,
	stubbed map[int]bool) (saved int) {

	// One entry per file this reply has read, in the order it read them.
	type readRef struct {
		index int
		label string
	}
	reads := map[string][]readRef{}
	noted := map[int]bool{}
	for i := range msgs {
		if i >= len(currentReply) || !currentReply[i] || msgs[i].Role != llm.RoleTool {
			continue
		}
		// A result that did not run — a denied card, a failed edit — changed nothing, so it
		// neither counts as a read nor marks one.
		if i < len(applied) && !applied[i] {
			continue
		}
		call := arguments[msgs[i].ToolCallID]
		switch msgs[i].Name {
		case tools.ToolReadFile:
			file := toolPath(call)
			if file == "" {
				continue
			}
			label := file
			if window, ok := readWindowOf(msgs[i].Name, call); ok {
				label = window.label()
			}
			reads[cleanPath(file)] = append(reads[cleanPath(file)], readRef{index: i, label: label})
		case tools.ToolWriteFile, tools.ToolEditFile:
			file := toolPath(call)
			if file == "" {
				continue
			}
			whole := msgs[i].Name == tools.ToolWriteFile
			for _, ref := range reads[cleanPath(file)] {
				if stubbed[ref.index] || noted[ref.index] {
					continue
				}
				size := llm.EstimateTokens(msgs[ref.index].Content)
				if whole {
					msgs[ref.index].Content = rewrittenStub(ref.label, size)
					stubbed[ref.index] = true
				} else {
					msgs[ref.index].Content = staleNote(file) + "\n\n" + msgs[ref.index].Content
					noted[ref.index] = true
				}
				saved += size - llm.EstimateTokens(msgs[ref.index].Content)
			}
		}
	}
	return saved
}

// staleNote is the sentence a read carries once an edit has moved past it.
//
// In front of the window rather than behind it: a model copies the passage it is about to change
// while it reads, so a warning at the end of five hundred lines arrives after the mistake.
func staleNote(path string) string {
	return fmt.Sprintf("[Hinweis: dieser Stand ist von vor einer Änderung an %s in dieser Antwort. "+
		"Den geänderten Ausschnitt enthält das Ergebnis von %s weiter unten; für den aktuellen "+
		"Stand der Datei lies den Bereich erneut.]", path, tools.ToolEditFile)
}

// rewrittenStub is what the model reads in place of a read a whole-file rewrite made obsolete.
func rewrittenStub(label string, tokens int) string {
	return fmt.Sprintf("[Tool-Ergebnis von %s (%s): %d Tokens. Die Datei wurde danach in dieser "+
		"Antwort vollständig neu geschrieben; der neue Inhalt steht im Aufruf %s weiter unten. Der "+
		"alte Stand wurde nicht erneut gesendet; brauchst du ihn doch, lies den Bereich erneut.]",
		tools.ToolReadFile, label, tokens, tools.ToolWriteFile)
}

// cleanPath is how two calls' spellings of one file are compared: a model writes `./app.go` and
// `app.go` for the same file, and a read it thinks is current must be recognised as the one the
// edit moved past.
func cleanPath(p string) string {
	return path.Clean(strings.TrimPrefix(filepath.ToSlash(p), "./"))
}

// toolPath is the file a call named, by any of the spellings the app accepts.
var pathArguments = []string{"path", "file_path", "file", "filename"}

func toolPath(arguments string) string {
	if arguments == "" {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return ""
	}
	return firstString(args)
}

// firstString is the first of the path arguments a call actually carries.
func firstString(args map[string]any) string {
	for _, key := range pathArguments {
		if value, ok := args[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// readWindow is the range of a file one `read_file` call covered.
//
// Taken from the arguments and not from the result text: a window that was cut by MaxReadBytes
// or by the end of the file does not carry the range its header promises, and the arguments are
// what the call asked for either way.
type readWindow struct {
	path string
	from int
	to   int
}

// covers reports whether w read at least the lines that other read.
func (w readWindow) covers(other readWindow) bool {
	return other.from >= w.from && other.to <= w.to
}

// label names the range the way the read tool's own header does.
func (w readWindow) label() string {
	if w.from == w.to {
		return fmt.Sprintf("%s, Zeile %d", w.path, w.from)
	}
	return fmt.Sprintf("%s, Zeilen %d–%d", w.path, w.from, w.to)
}

// readWindowOf is the window a read call covered, and false for any other tool.
//
// An `outline` call is not a window: it returns the declaration list with line numbers, and
// using one to evict a window would drop the text the outline exists to help you find — or
// evict the outline because a window happens to cover the same lines, which is worse.
func readWindowOf(name, arguments string) (readWindow, bool) {
	switch name {
	case tools.ToolReadFile:
	default:
		return readWindow{}, false
	}
	path := readPath(name, arguments)
	if path == "" {
		return readWindow{}, false
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return readWindow{}, false
	}
	if looseTrue(args["outline"]) {
		return readWindow{}, false
	}
	// The read tool's own resolution of the two arguments, so this cannot claim a window the
	// call did not return: no limit is DefaultReadLines, no offset is line 1, and a limit over
	// the maximum is the maximum.
	from, limit := looseNumber(args["offset"]), looseNumber(args["limit"])
	if limit <= 0 {
		limit = workspace.DefaultReadLines
	}
	if limit > workspace.MaxReadLines {
		limit = workspace.MaxReadLines
	}
	if from <= 0 {
		from = 1
	}
	return readWindow{path: path, from: from, to: from + limit - 1}, true
}

// looseNumber reads a numeric argument the way a model writes one: a number, or a number in a
// string (`"limit": "200"`), which the tool's own decoder accepts and so must not be a
// window this file cannot see.
func looseNumber(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return 0
}

// looseTrue reads a boolean argument the same way.
func looseTrue(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes", "ja":
			return true
		}
	}
	return false
}

// toolStub is what the model reads in place of a result it is no longer sent.
//
// It names the tool and the size, because "something was removed" without either is a gap the
// model has to guess at, and it says the text still exists — otherwise a model reasonably
// concludes the tool failed.
func toolStub(name string, tokens int) string {
	if name == "" {
		name = "unbekannt"
	}
	return fmt.Sprintf("[Tool-Ergebnis von %s: %d Tokens, aus Platzgründen aus dieser Anfrage "+
		"entfernt. Der Text bleibt im Verlauf gespeichert; rufe das Tool bei Bedarf erneut auf.]",
		name, tokens)
}

// readPath is the file a `read_file` call named, empty for any other tool.
//
// The alias list is shared (pathArguments) rather than written out again here: a model that
// spelled the path one way is the model whose result can be asked for again, so the aliases have
// to be read here too or that read would be dropped for good — and two lists would drift.
func readPath(name, arguments string) string {
	if name != tools.ToolReadFile {
		return ""
	}
	return toolPath(arguments)
}

// droppedToolText is what the transcript says when no call survived, so a turn
// does not end on an empty answer nobody can explain.
const droppedToolText = "[Der Modell-Aufruf „%s“ wurde verworfen: diesen Namen kennt die App nicht.]"

// toolNameSet turns names into a lookup.
func toolNameSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		if name != "" {
			set[name] = true
		}
	}
	return set
}

// definitionNames is the names of the tool definitions, in the form the repair and the
// sanitiser both want.
func definitionNames(defs []llm.ToolDefinition) []string {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	return names
}

// toolAliases are the names *other* tool conventions use for tools this app ships.
//
// They exist because a model that has been trained or prompted on another harness writes
// `bash` where this app's request declares `run_command` — a name it got exactly right, in
// a vocabulary this app never taught it. The repair below is about a name the model mangled
// (`scr scrape_url`); this table is about a name it borrowed, and the two arrive through
// the same door, so they are resolved in one place.
//
// Two rules keep it from becoming a way around the tool list. A target resolves only when
// this request actually offers it, so an alias can never hand a model a tool the turn
// withheld — `bash` in a research chat, where `run_command` is not offered, stays an
// unknown name. And a name with more than one plausible target is not listed at all: `find`
// could mean find_files or search_files, and a wrong tool with the right arguments is the
// expensive shape of that mistake.
var toolAliases = map[string][]string{
	"bash":       {tools.ToolRunCommand},
	"shell":      {tools.ToolRunCommand},
	"sh":         {tools.ToolRunCommand},
	"terminal":   {tools.ToolRunCommand},
	"command":    {tools.ToolRunCommand},
	"run":        {tools.ToolRunCommand},
	"read":       {tools.ToolReadFile},
	"cat":        {tools.ToolReadFile},
	"write":      {tools.ToolWriteFile},
	"edit":       {tools.ToolEditFile},
	"patch":      {tools.ToolEditFile},
	"ls":         {tools.ToolListDir},
	"list":       {tools.ToolListDir},
	"grep":       {tools.ToolSearchFiles},
	"rg":         {tools.ToolSearchFiles},
	"glob":       {tools.ToolFindFiles},
	"fetch":      {tools.ToolScrapeURL},
	"browse":     {tools.ToolScrapeURL},
	"web_search": {tools.ToolSearchWeb},
}

// repairToolName answers the name a model's mangled (or borrowed) one probably meant, or
// "" when nothing can be said with confidence. The second value says the name was an alias,
// which only changes the sentence the user is shown.
//
// The rule is deliberately conservative. An exact name wins; a name that differs only
// in case or surrounding whitespace resolves; a name whose word-tokens contain
// exactly one known tool name resolves to it; and a name from another convention's
// vocabulary resolves through toolAliases. Two candidates mean the model meant
// something this function cannot know, and a wrong tool with the right arguments is
// worse than a refused call — `generate_image` handed a scrape's URL as its prompt is
// the expensive shape of that mistake.
//
// `known` is every tool the app can execute, which is wider than what a request offers. An
// alias is therefore also resolved against `offered` — the tools this request declares — and
// only there: a borrowed `bash` must not reach a `run_command` the request withheld (a project
// with nobody to ask), and `fetch` must not reach a `scrape_url` whose scraper is switched off.
// A nil `offered` means "no restriction", which is what replaying a stored history wants: that
// call was executed, so the tool it ran as is the answer.
func repairToolName(name string, known, offered map[string]bool) (string, bool) {
	trimmed := strings.TrimSpace(strings.Trim(name, "`\"'"))
	if trimmed == "" {
		return "", false
	}
	if known[trimmed] {
		return trimmed, false
	}
	lower := strings.ToLower(trimmed)
	for candidate := range known {
		if strings.ToLower(candidate) == lower {
			return candidate, false
		}
	}
	words := strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_'
	})
	match := ""
	for _, word := range words {
		if !known[word] {
			continue
		}
		if match != "" && match != word {
			return "", false
		}
		match = word
	}
	if match != "" {
		return match, false
	}
	for _, target := range toolAliases[lower] {
		if !known[target] {
			continue
		}
		if offered != nil && !offered[target] {
			continue
		}
		return target, true
	}
	return "", false
}

// resolveToolCalls repairs a mangled name where the intent is unambiguous, and decides
// what happens to a call no tool answers.
//
// known is every tool the app can execute and offered the ones this request declares; a repaired
// name resolves within the first, a borrowed one within both (repairToolName has the argument).
//
// dropUnknown is the one thing the two tool modes disagree about, and it follows where
// the name came from. A native call is generated against a `tools` array the vendor
// validates: either the request never reached the app (Groq answers 400 before a token
// streams) or it reached a backend that tolerates any name (llama.cpp), and the app is
// then the only check there is — so the call is dropped, because executing a tool on a
// guessed name is worse than refusing it.
//
// A text-protocol call is written into prose by a model the app itself asked to write
// it, and no vendor sees the name. There the error is the useful answer: it names what
// the model asked for, the model can correct itself on the next round, and a history
// that still carries the name is demoted to text when it is replayed (sanitiseToolHistory).
func resolveToolCalls(calls []llm.ToolCall, known, offered []string, dropUnknown bool) (kept []llm.ToolCall,
	notes []string, dropped []string) {

	names := toolNameSet(known)
	offeredNames := toolNameSet(offered)
	for _, call := range calls {
		if names[call.Name] {
			kept = append(kept, call)
			continue
		}
		if repaired, aliased := repairToolName(call.Name, names, offeredNames); repaired != "" {
			// chat.aliasedTool is the borrowed name (`bash` where this app says
			// `run_command`), chat.renamedTool the mangled one. They are two sentences because
			// "the model had mangled the name" is wrong about a name it wrote exactly right.
			if aliased {
				notes = append(notes, uimsg.Errorf("chat.aliasedTool", call.Name, repaired).Error())
			} else {
				notes = append(notes, uimsg.Errorf("chat.renamedTool", call.Name, repaired).Error())
			}
			call.Name = repaired
			kept = append(kept, call)
			continue
		}
		if !dropUnknown {
			kept = append(kept, call)
			continue
		}
		notes = append(notes, uimsg.Errorf("chat.droppedTool", call.Name).Error())
		dropped = append(dropped, call.Name)
	}
	return kept, notes, dropped
}

// renderToolCallText is how a native call reads once it is history rather than a
// request field: the text protocol's own convention, which every model in the app has
// been taught.
func renderToolCallText(call llm.ToolCall) string {
	args := call.Arguments
	if args == "" {
		args = "{}"
	}
	return fmt.Sprintf("[Tool-Aufruf: %s %s]", call.Name, args)
}

// renderToolResultText marks a result whose call is no longer part of the request.
// The result itself is kept — it may be the only record of what a page said — but it
// travels as a user turn, because a `tool` message without the call it answers is the
// one shape a chat template rejects outright.
func renderToolResultText(name, content string) string {
	label := name
	if label == "" {
		label = "unbekannt"
	}
	return fmt.Sprintf("[Tool-Ergebnis von %s]\n%s", label, content)
}

// sanitiseToolHistory rewrites an assembled history so that no tool call in it names
// a tool the app cannot execute, and returns the token delta the rewrite costs so the
// budget stays honest.
//
// Only *unknown* names are touched. A known tool that is not offered in this
// particular request — `search_web` while the search providers are down, `read_pages`
// on a store without the index — is left alone: the app can still answer that call,
// and rewriting a history because a setting is momentarily different would lose the
// record of what was actually read.
//
// Messages are in conversation order, so a call is seen before the result that answers
// it; a result whose call was never seen is demoted for the same reason.
//
// A repaired call takes its result's name with it. The tool turn carries the name it ran under
// (Ollama sends it as `tool_name`), so a call replayed as `run_command` beside a result named
// `bash` is a pair that disagrees about which tool ran — and for a call dropped before this
// repair existed, the stored result still says the name was unknown.
func sanitiseToolHistory(msgs []llm.Message, known map[string]bool) ([]llm.Message, int) {
	delta := 0
	kept := map[string]bool{}
	// The name each call was repaired to, so the result answering it travels under that name.
	repaired := map[string]string{}
	for i := range msgs {
		m := &msgs[i]
		before := llm.EstimateTokens(m.Content)

		if m.Role == llm.RoleTool {
			if !kept[m.ToolCallID] {
				m.Role = llm.RoleUser
				m.Content = renderToolResultText(m.Name, m.Content)
				m.Name, m.ToolCallID = "", ""
				delta += llm.EstimateTokens(m.Content) - before
			} else if name := repaired[m.ToolCallID]; name != "" {
				m.Name = name
			}
			continue
		}
		if len(m.ToolCalls) == 0 {
			continue
		}
		callKept := make([]llm.ToolCall, 0, len(m.ToolCalls))
		for _, call := range m.ToolCalls {
			name := call.Name
			if !known[name] {
				name, _ = repairToolName(name, known, nil)
			}
			if name == "" {
				m.Content = joinText(m.Content, fmt.Sprintf(droppedToolText, call.Name))
				continue
			}
			if name != call.Name {
				repaired[call.ID] = name
			}
			call.Name = name
			kept[call.ID] = true
			callKept = append(callKept, call)
		}
		m.ToolCalls = callKept
		delta += llm.EstimateTokens(m.Content) - before
	}
	return msgs, delta
}
