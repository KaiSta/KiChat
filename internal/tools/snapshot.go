package tools

// The undo record behind a change.
//
// A coding agent edits somebody else's code, and "I would rather have the old version" is
// something people say about their own work: the model is guessing, the change looked right in
// a diff, and ten minutes later the file it rewrote is the file that used to work. Nothing in
// the tool call record can answer that, because the arguments hold what was written and never
// what was replaced.
//
// So every change carries two facts to the store: the file as this chat first found it, and the
// digest of what the app last wrote. The second is what makes a revert safe — it compares the
// digest with the file on disk, so work done in the user's own editor in the meantime is a
// refusal rather than a casualty — and the first is what the revert restores.
//
// **The record is taken before the change and written after it.** Before, because reading the
// file afterwards would record the new text as the old one. After, because a change that was
// refused, or that failed half way, must not leave a row claiming something happened. What the
// two orderings mean together is that a file this app could not read has no undo: the change
// still happens (the person allowed it and the write works), and the model is told in the
// result that it cannot be taken back, which is the honest place for that sentence.

import (
	"time"

	"kichat/internal/store"
	"kichat/internal/trace"
	"kichat/internal/workspace"
)

// preImage is what a change found when it looked at the file it is about to replace.
type preImage struct {
	text workspace.Text
	// ok is "this change can be undone".
	ok bool
	// why is the sentence the model gets when it cannot be, empty otherwise.
	why string
}

// preImageOf reads the file a change is about to touch.
//
// A file that is not there is the best case: the undo of a create is a deletion, and nothing
// has to be stored for it. A file that is too large or is not text has no undo — this app will
// not put back something it never read — and a file that cannot be read at all is reported.
func (s *Service) preImageOf(root workspace.Root, rel string) preImage {
	text, err := root.Text(rel)
	if err != nil {
		return preImage{why: " Die Änderung konnte nicht zum Rückgängigmachen gesichert werden: " + err.Error() + "."}
	}
	if text.Existed && !text.Writable {
		return preImage{why: " Die Änderung kann nicht rückgängig gemacht werden: die Datei ist keine " +
			"Textdatei oder zu groß, um sie zu sichern."}
	}
	return preImage{text: text, ok: true}
}

// remember records a change, and returns the sentence the model should see if it could not be.
//
// A failure here is about this app's bookkeeping rather than about the file: the change was
// made and the result says so. Swallowing it would leave a file the panel offers to undo and
// cannot, so the sentence travels with the result and the failure goes to the trace.
func (s *Service) remember(chatID string, pre preImage, afterSHA string) string {
	if !pre.ok {
		return pre.why
	}
	if s.Store == nil {
		return ""
	}
	err := s.Store.PutFileSnapshot(store.FileSnapshot{
		ChatID:     chatID,
		Path:       pre.text.Path,
		BeforeText: pre.text.Content,
		Existed:    pre.text.Existed,
		AfterSHA:   afterSHA,
		CreatedAt:  time.Now(),
	})
	if err == nil {
		return ""
	}
	if s.Trace != nil {
		s.Trace.Record(trace.Event{Event: "tool.snapshot", Level: trace.LevelError, Kind: trace.KindOf(err)})
	}
	return " Die Änderung ist geschrieben, konnte aber nicht zum Rückgängigmachen gesichert werden: " +
		err.Error() + "."
}
