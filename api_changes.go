package main

// The app's side of a change to a project folder: asking the person at the window whether
// it may be made, and waiting for the answer.
//
// This is the one place where a tool call stops and a person decides, so the shape of it is
// worth stating. `internal/tools` computes what a change *would* do and calls a callback;
// this file is the callback, and it does not answer on its own — it emits `chat:approval`,
// blocks the turn's own goroutine, and waits for the bound method the window calls back
// with. Two consequences fall out of that and both are wanted: a chat's turn is genuinely
// suspended while the question is on screen, so no further tool call or model round can
// slip past an unanswered change; and every other chat carries on, because the block is on
// one turn's goroutine rather than on a shared one.
//
// The answers that are not "yes" are the interesting half. A cancelled turn, a build with
// nobody to ask, and a click on a card whose turn has already moved on all have to end in
// "no", because the alternative — treating silence as consent — is how an agent writes to a
// folder nobody gave it. World-writable is not the same as allowed.

import (
	"context"
	"strings"

	"kichat/internal/events"
	"kichat/internal/tools"
	"kichat/internal/uimsg"
	"kichat/internal/workspace"
)

// The answers a window can give, as they arrive over the bridge.
const (
	// approvalOnce allows this one change.
	approvalOnce = "once"
	// approvalAlways allows this change and every later one in the same chat, which is what
	// keeps a long task from being forty identical dialogs. It lasts as long as the chat is
	// open in this process: a permission the user granted for a task they were watching,
	// remembered invisibly the next morning, would be a permission nobody remembers giving.
	approvalAlways = "always"
)

// pendingChange is one change waiting for an answer.
type pendingChange struct {
	chatID string
	// decide carries the answer. It is buffered, so an answer that arrives in the moment
	// between the question being shown and the turn being stopped is not lost in a way that
	// hangs the sender.
	decide chan approvalAnswer
}

// approvalAnswer is one person's answer to one question.
//
// It carries the decision and, for a command, the line they actually ran: the card's field is
// editable, and an answer that discarded the edit would leave the app running something the
// person had just corrected.
type approvalAnswer struct {
	decision string
	line     string
}

// confirmChange asks the window whether a change may be made, and refuses unless the answer
// is explicit.
func (a *App) confirmChange(ctx context.Context, req *tools.ApprovalRequest) error {
	// The two ways a question is already answered. The setting is the standing one — set once
	// for everyone who trusts the model with this kind of work — and the chat's own "always"
	// is the narrower one given while watching a task. Both are consulted before anything is
	// emitted, so an auto-approved call behaves exactly like a read tool: no card, no block.
	if a.autoApproved(req.Tool) || a.changesAllowed(req.ChatID) {
		return nil
	}
	if req.ToolCallID == "" {
		// Nothing identifies this request, so no answer could ever name it. That is a call
		// made without an engine behind it rather than a state the window can reach, and it
		// fails closed.
		return uimsg.Wrapf("tools.denied",
			"this change cannot be confirmed, so it was not made", tools.ErrDenied)
	}
	waiter := &pendingChange{chatID: req.ChatID, decide: make(chan approvalAnswer, 1)}
	if !a.addChangeWaiter(req.ToolCallID, waiter) {
		// The same call asking twice: a retry of a change that is already waiting on screen,
		// which one answer cannot resolve. Refused rather than queued.
		return uimsg.Wrapf("tools.denied",
			"this change is already waiting for an answer", tools.ErrDenied)
	}
	defer a.removeChangeWaiter(req.ToolCallID)

	a.emit(events.Approval, approvalPayload(*req))

	select {
	case answer := <-waiter.decide:
		// What the person actually ran, for the one question that has a field on its card.
		// The tool re-checks it — approving an edited line must not bypass the refusal that
		// produced the line in the first place.
		req.EditedLine = answer.line
		switch answer.decision {
		case approvalAlways:
			a.allowChanges(req.ChatID)
			return nil
		case approvalOnce:
			return nil
		default:
			return uimsg.Wrapf("tools.denied",
				"the user declined this change, so it was not made", tools.ErrDenied)
		}
	case <-ctx.Done():
		// The turn was stopped, or the process is shutting down, while the question was on
		// screen. Nobody said yes.
		return uimsg.Wrapf("tools.denied",
			"the turn was stopped before this change was confirmed, so it was not made",
			tools.ErrDenied)
	}
}

// approvalPayload is the event the window renders the question from.
//
// The question is sent as fields rather than as a pre-rendered sentence because the window
// decides how to show it: which side of a diff is which, what is added and what is removed,
// how much to put on screen. A sentence built here would be the app's idea of a diff in one
// language, and it would be the second opinion about what is being approved — the thing the
// whole preview-then-apply split exists to avoid.
//
// `kind` says which of the two questions this is, and exactly one of `change` and `command`
// carries it. Both keys are always present so the window's type has the shape it reads: the
// other one is null, which in TypeScript is the same statement as "this question has no
// diff", made by a compiler rather than by a comment.
func approvalPayload(req tools.ApprovalRequest) map[string]any {
	payload := map[string]any{
		"approvalId": req.ToolCallID,
		"chatId":     req.ChatID,
		"messageId":  req.MessageID,
		"toolCallId": req.ToolCallID,
		"tool":       req.Tool,
		"kind":       req.Kind,
	}
	if req.Kind == tools.ApprovalKindCommand {
		payload["change"] = nil
		payload["command"] = commandFields(req.Command)
		return payload
	}
	payload["change"] = changeFields(req.Change)
	payload["command"] = nil
	return payload
}

// changeFields is one file's change as the card reads it.
func changeFields(change workspace.Change) map[string]any {
	dirs := change.CreatesDirs
	if dirs == nil {
		// A nil Go slice marshals to JSON null, and the window walks this list. "No folders
		// are created" is an empty list, and sending null turns an ordinary change into a
		// render error.
		dirs = []string{}
	}
	return map[string]any{
		"path":        change.Path,
		"operation":   change.Operation,
		"before":      change.Before,
		"after":       change.After,
		"added":       change.Added,
		"removed":     change.Removed,
		"truncated":   change.Truncated,
		"createsDirs": dirs,
	}
}

// commandFields is one command as the card reads it: the line itself, where it runs, how
// long it may take, and which shell interprets it.
func commandFields(command tools.Command) map[string]any {
	return map[string]any{
		"line":       command.Line,
		"dir":        command.Dir,
		"timeoutSec": command.TimeoutSec,
		"shell":      command.Shell,
	}
}

// ResolveToolApproval answers a change waiting for the person at the window.
//
// decision is one of "once", "always", or anything else for no. It reports whether the
// answer reached a waiting call: a second click on a card, or one that arrives after the
// turn was stopped, is not an error worth a toast — the user did what they meant and the
// turn had already moved on.
func (a *App) ResolveToolApproval(approvalID, decision string) bool {
	return a.resolveApproval(approvalID, approvalAnswer{decision: decision})
}

// ResolveToolApprovalWith answers a question and says what to run instead of the line shown.
//
// A separate binding rather than a third argument on the old one: the two-argument call is what
// a change and every other caller uses, and widening its arity would have made every existing
// call site pass a string it does not have. command is ignored for a change.
func (a *App) ResolveToolApprovalWith(approvalID, decision, command string) bool {
	return a.resolveApproval(approvalID, approvalAnswer{decision: decision, line: strings.TrimSpace(command)})
}

func (a *App) resolveApproval(approvalID string, answer approvalAnswer) bool {
	a.changesMu.Lock()
	waiter, ok := a.changes[approvalID]
	a.changesMu.Unlock()
	if !ok {
		return false
	}
	select {
	case waiter.decide <- answer:
		return true
	default:
		// Answering the same question twice: the first answer is the one that counts.
		return false
	}
}

// autoApproved reports whether the user has switched this tool's confirmation off.
//
// It matches by *tool name* and answers for the two kinds the setting distinguishes: the
// tools that write a file, and the tool that runs a command. A guarded tool that is neither
// keeps asking, which is the direction that has to be safe — a tool added later must not
// inherit a permission somebody granted for the tools that existed when they granted it.
func (a *App) autoApproved(tool string) bool {
	auto := a.Settings().AutoApprove
	switch tool {
	case tools.ToolWriteFile, tools.ToolEditFile:
		return auto.Files
	case tools.ToolRunCommand:
		return auto.Commands
	}
	return false
}

// --- the waiting changes ----------------------------------------------------

func (a *App) addChangeWaiter(id string, waiter *pendingChange) bool {
	a.changesMu.Lock()
	defer a.changesMu.Unlock()
	if a.changes == nil {
		a.changes = map[string]*pendingChange{}
	}
	if _, there := a.changes[id]; there {
		return false
	}
	a.changes[id] = waiter
	return true
}

func (a *App) removeChangeWaiter(id string) {
	a.changesMu.Lock()
	delete(a.changes, id)
	a.changesMu.Unlock()
}

// changesAllowed reports whether the user has allowed every change in this chat.
func (a *App) changesAllowed(chatID string) bool {
	a.changesMu.Lock()
	defer a.changesMu.Unlock()
	return a.allowedChanges[chatID]
}

// allowChanges remembers "always" for one chat.
func (a *App) allowChanges(chatID string) {
	a.changesMu.Lock()
	defer a.changesMu.Unlock()
	if a.allowedChanges == nil {
		a.allowedChanges = map[string]bool{}
	}
	a.allowedChanges[chatID] = true
}

// forgetChanges drops what was remembered about a chat, which is what deleting one means:
// a permission travels with the conversation it was given for.
func (a *App) forgetChanges(chatID string) {
	a.changesMu.Lock()
	delete(a.allowedChanges, chatID)
	a.changesMu.Unlock()
}
