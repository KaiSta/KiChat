package main

// A project's changed files, and the one action a person has over them: put a file back the
// way this chat found it.
//
// Two questions are answered here, and they are different. The panel's list is "what has this
// chat done to my folder" — a question the transcript answers only in the form of a hundred
// tool cards, and with no way to tell a file that was written once from one that was rewritten
// five times. The revert is "undo that", and it exists because the model is guessing: a change
// that looked right in a diff is regularly not right, and the folder belongs to the person
// reading it, not to the agent.
//
// The record itself is kept by the tools that made each change (internal/tools/snapshot.go):
// the file as the chat first found it, and the digest of what was last written. This file only
// turns it into something to look at and to press, which is why it can be honest about the two
// cases it cannot undo — a file somebody edited outside the chat since, and a file that is not
// text.

import (
	"time"

	"kichat/internal/store"
	"kichat/internal/uimsg"
	"kichat/internal/workspace"
)

// Why a change cannot be undone, as a code the window translates.
const (
	// revertBlockedOutside is "the file changed after the app wrote it", which is the one
	// refusal that protects the user's own work: a revert is a write, and overwriting an edit
	// made in another editor is exactly the accident this check exists to prevent.
	revertBlockedOutside = "outside"
	// revertBlockedBinary is a file the app will not rewrite — binary, or larger than a change
	// may touch — so no pre-image was ever kept for it.
	revertBlockedBinary = "not-text"
)

// FileChange is one file this chat changed, as the panel lists it.
//
// It carries the same hunk diff an approval does, for the same reason: the panel promises a
// diff against the file *as it is now*, and a file that differs by one line used to arrive as
// two whole texts. Added and Removed are the diff's line counts, so the row's `−3 +1` matches
// the rows below it rather than the size of the file.
type FileChange struct {
	// Path is workspace-relative, the form the model wrote and the person reads.
	Path string `json:"path"`
	// Operation is "created" or "changed", as the chat first found the file.
	Operation string `json:"operation"`
	// Revisions is how many times this chat wrote the file.
	Revisions int `json:"revisions"`
	// Hunks is never nil: the window maps over it.
	Hunks   []workspace.Hunk `json:"hunks"`
	Added   int              `json:"added"`
	Removed int              `json:"removed"`
	// Truncated says the diff is shorter than the change.
	Truncated bool `json:"truncated"`
	// NewlineOnly says the only difference is the newline at the end of the file, which has no
	// rows in a diff: the panel would otherwise claim no line differs.
	NewlineOnly bool `json:"newlineOnly"`
	// CanRevert is "pressing undo would restore this file". False comes with a Blocked code.
	CanRevert bool `json:"canRevert"`
	// Blocked is why not: revertBlockedOutside, revertBlockedBinary, or empty.
	Blocked string `json:"blocked"`
	// ChangedAt is when this chat last wrote the file.
	ChangedAt time.Time `json:"changedAt"`
}

// ProjectFileChanges lists what a chat has changed in its folder, most recent first.
//
// A chat with no folder, or one whose folder has gone, has no changes to list rather than an
// error: the panel simply has nothing to show, and the file tools already report the missing
// folder where the model asks to read something.
func (a *App) ProjectFileChanges(chatID string) ([]FileChange, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	snapshots, err := a.store.ListFileSnapshots(chatID)
	if err != nil {
		return nil, uimsg.Errorf("project.changesFailed", err)
	}
	out := make([]FileChange, 0, len(snapshots))
	if len(snapshots) == 0 {
		// An empty slice rather than nil: a nil Go slice marshals to null, and the window
		// walks this list (see the note in AGENTS.md about runtime_status_test.go).
		return out, nil
	}
	chat, err := a.store.GetChat(chatID)
	if err != nil {
		return nil, uimsg.Errorf("project.changesFailed", err)
	}
	root, err := workspace.Open(chat.Workspace)
	if err != nil {
		return nil, uimsg.Errorf("project.folderGone", err)
	}
	for _, snap := range snapshots {
		out = append(out, describeChange(root, snap))
	}
	return out, nil
}

// describeChange is one snapshot as the panel shows it: the diff against the file as it is
// now, and whether pressing undo would be safe.
func describeChange(root workspace.Root, snap store.FileSnapshot) FileChange {
	change := FileChange{
		Path:      snap.Path,
		Operation: "changed",
		Revisions: snap.Revisions,
		ChangedAt: snap.UpdatedAt,
	}
	if !snap.Existed {
		change.Operation = "created"
	}
	current, err := root.Text(snap.Path)
	switch {
	case err != nil:
		// The file cannot be read (a permission, a folder where a file was): the diff falls
		// back to what was written, and undo is off rather than attempted blind.
		change.Blocked = revertBlockedBinary
		return withDiff(change, workspace.CompareDiff(snap.BeforeText, ""))
	case !current.Existed:
		// Gone from the disk. Undo puts it back, and the diff says so by showing every line as
		// removed — which is what the file's absence means.
		change.CanRevert = true
		return withDiff(change, workspace.CompareDiff(snap.BeforeText, ""))
	case !current.Writable:
		change.Blocked = revertBlockedBinary
	default:
		// The digest is what makes undo safe: the file is only put back while it still holds
		// what the app wrote. A file somebody edited in the meantime is left alone.
		change.CanRevert = workspace.Digest(current.Content) == snap.AfterSHA
		if !change.CanRevert {
			change.Blocked = revertBlockedOutside
		}
	}
	return withDiff(change, workspace.CompareDiff(snap.BeforeText, current.Content))
}

// withDiff fills a row's diff in, in one place so the three ways a row is built cannot
// disagree about which numbers belong to which field.
func withDiff(change FileChange, diff workspace.Diff) FileChange {
	change.Hunks = diff.Hunks
	change.Added = diff.Added
	change.Removed = diff.Removed
	change.Truncated = diff.Truncated
	change.NewlineOnly = diff.NewlineOnly
	return change
}

// RevertFileChange puts one file back the way this chat first found it.
//
// The checks are ordered by what they protect. A running turn comes first: the model may be
// writing the same file at this instant, and a revert racing it would leave whichever write
// landed last — with the panel claiming the file is restored. Then the digest, because the
// undo restores the *chat's* change and not somebody else's; a file that moved on is a
// refusal, and the sentence says where to look rather than what went wrong.
//
// A file the chat created is removed rather than emptied: an empty file is not the state the
// folder was in. Folders a write created are deliberately left, because by now they may hold
// work this chat did not put there.
func (a *App) RevertFileChange(chatID, path string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	if path == "" {
		return uimsg.Errorf("project.pathMissing")
	}
	if engine := a.currentEngine(); engine != nil {
		for _, active := range engine.ActiveChats() {
			if active == chatID {
				return uimsg.Errorf("project.revertWhileRunning")
			}
		}
	}
	chat, err := a.store.GetChat(chatID)
	if err != nil {
		return uimsg.Errorf("project.changesFailed", err)
	}
	root, err := workspace.Open(chat.Workspace)
	if err != nil {
		return uimsg.Errorf("project.folderGone", err)
	}
	snap, ok, err := a.store.GetFileSnapshot(chatID, path)
	if err != nil {
		return uimsg.Errorf("project.changesFailed", err)
	}
	if !ok || !snap.RevertedAt.IsZero() {
		// A second click on a row the first click already restored. Nothing went wrong, and
		// the panel is about to be reloaded.
		return nil
	}
	current, err := root.Text(snap.Path)
	if err != nil {
		return uimsg.Errorf("project.revertFailed", snap.Path, err)
	}
	if current.Existed && workspace.Digest(current.Content) != snap.AfterSHA {
		return uimsg.Errorf("project.revertConflict", snap.Path)
	}

	if snap.Existed {
		change, err := root.DescribeWrite(snap.Path, snap.BeforeText)
		if err != nil {
			return uimsg.Errorf("project.revertFailed", snap.Path, err)
		}
		before := change.Before()
		if before.Existed && workspace.Digest(before.Content) != snap.AfterSHA {
			return uimsg.Errorf("project.revertConflict", snap.Path)
		}
		if _, err := root.WriteApproved(snap.Path, change); err != nil {
			return uimsg.Errorf("project.revertFailed", snap.Path, err)
		}
	} else if err := root.Remove(snap.Path); err != nil {
		return uimsg.Errorf("project.revertFailed", snap.Path, err)
	}
	// The row is marked rather than deleted, and the mark is the one thing here that is about
	// the model rather than about the folder: the transcript's tool results still claim this
	// file contains what the model wrote, and the next turn has to be told otherwise or it
	// will quote a line that is no longer there and edit around one that moved. The panel
	// leaves a marked row out, so nothing offers to undo it twice.
	if err := a.store.MarkFileSnapshotReverted(chatID, snap.Path); err != nil {
		return uimsg.Errorf("project.changesFailed", err)
	}
	// A revert is a write like any other: whatever the turn remembered about this file is
	// stale from here on (internal/tools/reads.go), and the panel's own list is reloaded by
	// the caller.
	if a.toolSvc != nil {
		a.toolSvc.ForgetReads(chatID, root.Absolute(snap.Path))
	}
	return nil
}
