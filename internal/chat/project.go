package chat

import (
	"fmt"
	"strings"

	"kichat/internal/store"
	"kichat/internal/uimsg"
	"kichat/internal/workspace"
)

// The project's own instructions, as they reach the model.
//
// A coding project usually carries an AGENTS.md, and the point of reading it rather than
// asking the user to paste it into a setting is that it already exists, it is versioned
// beside the code it describes, and it changes as the project does. So it is read on
// every turn rather than once when the project was created: a prompt that keeps
// describing a build step that was removed last week is worse than one that says nothing,
// because the model will follow it and be wrong.
//
// Two wrappers, and both carry meaning. The tag separates the repository's words from the
// application's, which is what lets the system prompt say how to treat them without the
// two running together into one voice — and it is markup rather than a sentence because
// the sentence belongs in the prompt, in the user's language, while this payload is the
// same text in every language. The attribute says the file was cut, so a model that finds
// no answer to its question in what it was given can say the guidance was incomplete
// rather than concluding the project has no rule about it.
//
// The content is untrusted in the same sense a scraped page is, and more subtly: it is
// written by whoever owns the repository, which is not necessarily the person running the
// app — cloning somebody else's project is the ordinary way to get one. The system
// prompt therefore states that this file is project guidance and never a change to the
// rules about which folder may be read.
const (
	agentsOpen  = "<project_agents_md"
	agentsClose = "</project_agents_md>"
)

// projectGuidance renders the folder's guidance file for the system prompt.
//
// ok is false for a chat that is not a project, for one whose folder has gone, and for a
// project with no AGENTS.md. None of those is a failure: the two cases the prompt cares
// about are "there is guidance" and "there is none", and it says what to do about the
// second. A folder that cannot be opened is reported by the file tools, where the model
// asked to read something and deserves an answer.
func projectGuidance(c store.Chat) (guidance, detail string, ok bool) {
	if !c.IsProject() {
		return "", "", false
	}
	root, err := workspace.Open(c.Workspace)
	if err != nil {
		return "", "", false
	}
	doc, found, err := root.AgentsFile(0)
	if err != nil || !found || strings.TrimSpace(doc.Text) == "" {
		return "", "", false
	}
	var sb strings.Builder
	sb.WriteString(agentsOpen)
	fmt.Fprintf(&sb, " path=%q", doc.Path)
	if doc.Truncated {
		sb.WriteString(` truncated="true"`)
	}
	sb.WriteString(">\n")
	sb.WriteString(strings.TrimRight(doc.Text, "\n"))
	sb.WriteString("\n")
	sb.WriteString(agentsClose)

	detail = fmt.Sprintf("Projektanweisungen, %d Zeichen", len(doc.Text))
	if doc.Truncated {
		detail += ", gekürzt"
	}
	return sb.String(), detail, true
}

// The project's working tree, as the next turn's prompt says it.
//
// The gap this closes is not that the model cannot run `git status` — it can, through the
// terminal — but that it has no reason to: the transcript already says what it wrote, and
// nothing there says whether that work is staged, committed, or gone. So the state is read on
// every turn and travels beside the guidance file, as one block, exactly like the notes below.
//
// Three decisions:
//
//   - **Markup, not prose.** The system prompt exists once per interface language and this
//     payload is the same in all of them; the attributes carry the machine-readable half (branch,
//     how many changes) so the window can show it cheaply too (see context.go's segment).
//   - **It says when it was taken.** The block is a snapshot from the moment the turn was
//     assembled, and a turn that edits three files has made it wrong before it is done. Saying so
//     is what keeps the model from treating a stale "clean" as permission to assume a file is
//     untouched.
//   - **It is not authority.** A path here is a hint about where to look, not a substitute for
//     reading, and the folder's own instructions never arrive through it — that is the AGENTS.md
//     block, which has its own rule about being knowledge rather than permission.
const (
	gitOpen  = "<project_git"
	gitClose = "</project_git>"
	// maxGitBlockBytes bounds the rendered block. The entry count is capped where the state is
	// read (workspace.MaxGitEntries); this is the second bound, because a path may itself be long
	// and forty of them are not necessarily small.
	maxGitBlockBytes = 4 << 10
)

// projectGitState renders the folder's git state for the system prompt.
//
// ok is false for a chat that is not a project, for a folder that has gone, and for a folder
// that is not a git working tree — git missing included. None of those is worth reporting: the
// block is an addition to the conversation, and a turn without it is the turn that existed
// before it.
//
// full is false from the second round of a reply on. The list is orientation — what is pending
// before the model decides anything — and it is the only part of the block that is large: forty
// paths with their line counts reach the 4 KiB cap, and re-sending that on every round of a reply
// bills it at the miss price each time (DEEPSEEK.md §5), for a list the model has by then often
// written itself. The summary keeps the branch, the count and the staleness warning, so a round
// that changes the tree still sees the change in the numbers.
func projectGitState(c store.Chat, full bool) (block, detail string, ok bool) {
	if !c.IsProject() {
		return "", "", false
	}
	root, err := workspace.Open(c.Workspace)
	if err != nil {
		return "", "", false
	}
	state, err := root.GitState()
	if err != nil {
		return "", "", false
	}
	return renderGitState(state, full), gitStateDetail(state), true
}

// renderGitState writes the block. Its one instruction is about staleness rather than about
// what to do with the list: the list is context, and telling a coding model what to conclude
// from context is how a prompt starts overriding the person.
func renderGitState(state workspace.GitState, full bool) string {
	var sb strings.Builder
	sb.WriteString(gitOpen)
	if state.Detached {
		sb.WriteString(` detached="true"`)
	} else {
		fmt.Fprintf(&sb, ` branch=%q`, state.Branch)
	}
	if state.Total > 0 {
		fmt.Fprintf(&sb, ` changed="%d"`, state.Total)
	}
	if state.Ahead > 0 {
		fmt.Fprintf(&sb, ` ahead="%d"`, state.Ahead)
	}
	if state.Behind > 0 {
		fmt.Fprintf(&sb, ` behind="%d"`, state.Behind)
	}
	sb.WriteString(">\n")
	sb.WriteString("Der Zustand des Projektordners laut git, als dieser Turn zusammengestellt wurde. " +
		"Er kann sich während des Turns ändern; was du bearbeitest, prüfst du an der Datei.\n")
	if state.Total == 0 {
		sb.WriteString("Keine Änderungen seit dem letzten Commit.\n")
	} else if !full {
		what := fmt.Sprintf("%d Änderungen", state.Total)
		if state.Total == 1 {
			what = "1 Änderung"
		}
		fmt.Fprintf(&sb, "%s seit dem letzten Commit. Die Pfade nennt dieser Block nur zu Beginn "+
			"des Turns; `git status` zeigt die aktuelle Liste.\n", what)
	} else {
		fmt.Fprintf(&sb, "Änderungen seit dem letzten Commit (%d):\n", state.Total)
		shown := 0
		for _, entry := range state.Entries {
			line := "- " + gitEntryLine(entry) + "\n"
			if sb.Len()+len(line) > maxGitBlockBytes {
				break
			}
			sb.WriteString(line)
			shown++
		}
		// One line for both ways the list can be short: the read caps the entries, and this
		// renderer caps the bytes.
		if rest := state.Total - shown; rest > 0 {
			fmt.Fprintf(&sb, "- … und %d weitere\n", rest)
		}
	}
	sb.WriteString(gitClose)
	return sb.String()
}

// gitEntryLine is one path, with what git says about it: which half of the change is staged, what
// kind of change it is, and how many lines it moved.
func gitEntryLine(entry workspace.GitEntry) string {
	if entry.Index == '?' && entry.Worktree == '?' {
		return entry.Path + ": unversioniert"
	}
	line := entry.Path + ": " + gitChangeText(entry)
	if entry.Counted {
		line += fmt.Sprintf(" +%d −%d", entry.Added, entry.Deleted)
	}
	return line
}

// gitChangeText names the change in the terms the two porcelain columns mean: the first is the
// index (what the next commit would contain), the second the working tree.
func gitChangeText(entry workspace.GitEntry) string {
	if gitConflict(entry) {
		return "Konflikt (ungeklärt)"
	}
	staged, unstaged := gitChangeKind(entry.Index), gitChangeKind(entry.Worktree)
	switch {
	case staged != "" && unstaged != "" && staged != unstaged:
		return fmt.Sprintf("gestaged (%s), nicht gestaged (%s)", staged, unstaged)
	case staged != "" && unstaged != "":
		return fmt.Sprintf("teils gestaged (%s)", staged)
	case staged != "":
		return fmt.Sprintf("gestaged (%s)", staged)
	case unstaged != "":
		return fmt.Sprintf("nicht gestaged (%s)", unstaged)
	}
	return "geändert"
}

// gitConflict reports an unmerged entry. The pairs are UU, AA, DD, AU, UA, DU and UD: either half
// is U, or both halves are the same add or delete. A plain staged addition is `A `, which is why
// the test is a pair and not a letter.
func gitConflict(entry workspace.GitEntry) bool {
	return entry.Index == 'U' || entry.Worktree == 'U' ||
		(entry.Index == 'A' && entry.Worktree == 'A') ||
		(entry.Index == 'D' && entry.Worktree == 'D')
}

func gitChangeKind(code byte) string {
	switch code {
	case 'M':
		return "geändert"
	case 'A':
		return "neu"
	case 'D':
		return "gelöscht"
	case 'R':
		return "umbenannt"
	case 'C':
		return "kopiert"
	case 'T':
		return "Typ geändert"
	}
	return ""
}

// gitStateDetail is the line the context meter shows next to the segment: which branch, and how
// much is pending. It is short because it sits in a table row, not because it is unread.
func gitStateDetail(state workspace.GitState) string {
	where := "git"
	switch {
	case state.Detached:
		where = "git, detached"
	case state.Branch != "":
		where = "git " + state.Branch
	}
	if state.Total == 0 {
		return where + ", keine Änderungen"
	}
	return fmt.Sprintf("%s, %d Änderungen", where, state.Total)
}

// The files a person put back, as the next turn's prompt says it.
//
// Why this exists at all: a tool result in the transcript is a claim about a file — "written,
// 40 lines", "edited, 1 occurrence replaced (line 12)" — and it is replayed on every later turn.
// A revert makes those claims false while the history keeps telling them, and a model with a
// stale claim does the things it does with any fact: it quotes the line it thinks it wrote, it
// edits around a block that has moved, or it concludes its change is still in place. Nothing in
// the folder can correct that — the folder is right, the transcript is wrong — so the correction
// has to travel beside the history, once.
//
// Three properties matter and each is a decision:
//
//   - **It names files, not contents.** Sending the restored text back would be a second copy of
//     a file the model can read with one tool call, and it would grow with the file. The point is
//     to say "what you think you know about this file is out of date", which paths do.
//   - **It is one block, once.** The turn that carried it into a request clears the marker, so the
//     note does not become a permanent part of every request (Engine.revertNote, and the clear at
//     the end of Engine.generate).
//   - **It is markup, like the AGENTS.md block, and it says what to do.** A model that reads
//     "these were reverted" without an instruction treats it as news; the sentence tells it the
//     one action that fixes the staleness: read the file again before touching it.
const (
	revertedOpen  = "<project_reverted_files>"
	revertedClose = "</project_reverted_files>"
)

// revertedNotice renders one block naming every file this chat's user put back.
//
// The paths are sorted by when they were reverted, which is the order the turn's own rounds
// happened in — so a model reading it can match the note to its own history if it cares.
func revertedNotice(snaps []store.FileSnapshot) (string, bool) {
	if len(snaps) == 0 {
		return "", false
	}
	var sb strings.Builder
	sb.WriteString(revertedOpen)
	sb.WriteString("\nDer Nutzer hat diese Dateien nach deiner Änderung zurückgesetzt; sie enthalten " +
		"wieder den Stand von vor deiner Änderung:\n")
	for _, snap := range snaps {
		sb.WriteString("- ")
		sb.WriteString(snap.Path)
		if !snap.Existed {
			sb.WriteString(" (angelegt von dir, wieder entfernt)")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Deine früheren Ergebnisse zu diesen Dateien stimmen nicht mehr. Lies eine davon " +
		"erneut, bevor du sie bearbeitest oder ihren Inhalt wiedergibst.\n")
	sb.WriteString(revertedClose)
	return sb.String(), true
}

// goneDigest stands in for "this file is not there" in the marker column.
//
// A snapshot's marker is otherwise a digest of text, and the empty one means "nothing has been
// told yet". A deletion has no text to digest, and leaving the marker empty would make it news
// on every request; so it gets a value that no sha256 can produce.
const goneDigest = "-"

// The files that changed outside the app, as the next turn's prompt says it.
//
// A snapshot already carries the digest of the text this app last wrote, which makes it the only
// place the app can notice that the file has moved on without it: the user edited it in their
// own editor, a formatter ran in another window, a branch was checked out, a generator rewrote
// it. The transcript, meanwhile, still holds the tool result that says "written, 40 lines" — the
// same false claim a revert produces, arrived at by a different road, and corrected the same way:
// one block beside the history, naming the files and telling the model to read them again before
// it trusts what it remembers.
//
// Three decisions, and the first one is what keeps this from being expensive:
//
//   - **Only files this chat wrote.** Those are the ones with a digest to compare, and the ones a
//     tool result in this transcript describes. A file the model merely read and the user then
//     edited has no recorded version to differ from; the read shortcut's size-and-mtime stamp
//     (internal/tools/reads.go) catches it within a turn, and the next turn's read returns the
//     text, which is the answer that matters there.
//   - **Once per outside version, not once per turn.** The marker is the digest that was
//     announced, so a block the model ignored does not grow every request after it, while a file
//     edited again after being announced is a new fact and speaks again. A write clears the
//     marker along with the digest, because then the file is the model's own text again.
//   - **It is not a revert and does not claim to be.** The text is the user's, not the pre-image
//     this chat restored, so the note must not suggest the change was undone.
const (
	diskOpen  = "<project_changed_on_disk>"
	diskClose = "</project_changed_on_disk>"
)

// notedVersion is what the delivery marker needs to know: the outside text that was announced,
// and the digest it was compared against. The second one is what keeps the marker from being
// written over a file the same turn has since rewritten (Store.MarkFileSnapshotNoted).
type notedVersion struct{ disk, after string }

// externalNotice renders the block for the files whose outside text this turn is about to
// announce.
func externalNotice(changed []string) string {
	if len(changed) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(diskOpen)
	sb.WriteString("\nDiese Dateien wurden außerhalb dieser App geändert, seit du sie geschrieben hast:\n")
	for _, name := range changed {
		sb.WriteString("- ")
		sb.WriteString(name)
		sb.WriteString("\n")
	}
	sb.WriteString("Dein Stand von diesen Dateien ist damit überholt. Lies eine davon erneut, " +
		"bevor du sie bearbeitest oder ihren Inhalt wiedergibst.\n")
	sb.WriteString(diskClose)
	return sb.String()
}

// externalNote is what this turn has to say about files edited behind the app's back.
//
// The map is the marker the caller writes after the request that carried the note succeeded, so
// a turn that never left the app has not spent it. It is empty when there is nothing to say.
func (e *Engine) externalNote(c store.Chat) (string, map[string]notedVersion) {
	if e == nil || e.Store == nil || c.ID == "" || !c.IsProject() {
		return "", nil
	}
	snaps, err := e.Store.ListFileSnapshots(c.ID)
	if err != nil {
		// A chat that never changed a file fails here too, which is why the language is about
		// the check rather than about a fault: without the list the turn proceeds on tool
		// results that may be false, and that is the case worth filing in the trace.
		e.warn(c.ID, uimsg.Errorf("chat.gitStateFailed", err.Error()).Error())
		return "", nil
	}
	if len(snaps) == 0 {
		return "", nil
	}
	root, err := workspace.Open(c.Workspace)
	if err != nil {
		// The folder is gone or unreadable. The file tools report that where the model asks,
		// and a note about which files changed in a folder nobody can read is noise.
		return "", nil
	}
	changed := make([]string, 0, len(snaps))
	noted := make(map[string]notedVersion, len(snaps))
	for _, snap := range snaps {
		text, readErr := root.Text(snap.Path)
		if readErr != nil {
			// Unreadable now, where it was readable when it was written: there is no digest to
			// compare, and guessing at one would announce a change nobody measured.
			continue
		}
		digest := goneDigest
		if text.Existed {
			if !text.Writable {
				// Replaced by a binary or grown past what a change may touch. The undo knows the
				// same limit, and the file tools say so where it matters.
				continue
			}
			digest = workspace.Digest(text.Content)
			if digest == snap.AfterSHA {
				continue
			}
		}
		if digest == snap.NotedSHA {
			continue
		}
		name := snap.Path
		if !text.Existed {
			name += " (nicht mehr vorhanden)"
		}
		changed = append(changed, name)
		noted[snap.Path] = notedVersion{disk: digest, after: snap.AfterSHA}
	}
	note := externalNotice(changed)
	if note == "" {
		return "", nil
	}
	return note, noted
}

// revertNote is what this turn has to say about the files the user put back.
//
// It reads the marker rather than the snapshots that still stand, because those two are
// different questions: a snapshot that stands is a change the panel offers to undo, and a marker
// is a correction the model owes. A store with no markers — every turn of a project the user has
// not touched — costs one query and returns nothing to say.
//
// It is gated on the mode like every other project fact (projectGuidance): a chat with no folder
// cannot have changed a file of it, so a marker that somehow pointed at one still must not put
// paths from a folder into its prompt.
func (e *Engine) revertNote(c store.Chat) string {
	if e == nil || e.Store == nil || c.ID == "" || !c.IsProject() {
		return ""
	}
	snaps, err := e.Store.PendingReverts(c.ID)
	if err != nil {
		// Worth saying out loud: without the note the turn proceeds on a history that is
		// wrong about the folder, which is exactly the case this exists to prevent.
		e.warn(c.ID, uimsg.Errorf("chat.revertUnreadable", err.Error()).Error())
		return ""
	}
	note, ok := revertedNotice(snaps)
	if !ok {
		return ""
	}
	return note
}
