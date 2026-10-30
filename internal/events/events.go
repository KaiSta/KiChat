// Package events names every Wails event the backend emits.
//
// They live here rather than as literals at the call sites because the failure
// mode is silent: a misspelled name does not break the build, it just leaves a
// listener that never fires. That happened once — a manual compaction emitted
// "chat:chats-changed" while the frontend subscribed to "chats:changed", so the
// chat list quietly stopped refreshing. The frontend's own map is pinned to
// these constants by events_test.go, which reads frontend/src/lib/api.ts.
package events

const (
	// Delta and Reasoning stream a reply's text and thinking as it arrives.
	Delta     = "chat:delta"
	Reasoning = "chat:reasoning"
	// Start announces a new assistant message; Done closes a turn.
	Start = "chat:start"
	Done  = "chat:done"
	// Message carries a persisted message, used for user turns, tool results
	// and seeded search results.
	Message = "chat:message"
	// Tool reports a tool call being started and finished.
	Tool = "chat:tool"
	// Status carries a phase such as "searching", "tool" or "compacting".
	Status = "chat:status"
	// Error and Warning report a failed turn and a degraded-but-usable one.
	Error   = "chat:error"
	Warning = "chat:warning"
	// Budget republishes the context accounting for the meter.
	Budget = "chat:budget"
	// Compaction and Title report background changes to a chat.
	Compaction = "chat:compaction"
	Title      = "chat:title"
	// Attachments reports that a chat's attachments changed.
	Attachments = "chat:attachments"
	// ToolOutput carries a running tool's output while it is still producing it. It is
	// its own event rather than a Status or a repeated Tool: it is high-frequency and
	// keyed to one call, and a Tool event carries a *record* — a status and a result —
	// which is the opposite of "this is not finished yet".
	ToolOutput = "chat:tool-output"
	// Approval carries a change to a project folder that is waiting for the user's
	// answer. The turn that asked is blocked until ResolveToolApproval answers it, which
	// is why it cannot ride on Status: a phase string tells the window what is happening,
	// and this one asks it to decide something before anything else can.
	Approval = "chat:approval"
	// ImageProgress carries one report from a picture being drawn: a step counter and,
	// usually, a small preview of the draw so far. It is its own event rather than a Status
	// because it is high-frequency and structured — a draw reports once per step, and a
	// phase string would mean re-sending the whole phase on every one of them.
	ImageProgress = "image:progress"
	// ChatsChanged reports that the chat list itself changed.
	ChatsChanged = "chats:changed"
)

// All lists every event name, so a test can check the set as a whole rather
// than only the names it happens to remember.
func All() []string {
	return []string{
		Delta, Reasoning, Start, Done, Message, Tool, Status,
		Error, Warning, Budget, Compaction, Title, Attachments, ChatsChanged,
		ImageProgress, Approval, ToolOutput,
	}
}
