# Coding mode

A conversation in this app is either a chat or a **project**. A project is a conversation
about a folder: it is created with a folder path, it carries the coding prompt, it is given
the file tools and the terminal, and it is listed separately from chats. Everything a project
can do to that folder passes one gate — a person reading the question and answering it.

This document records the decisions behind that mode, what is measured, and what is
deliberately not done. Where a number is stated it came from a run on this machine; where a
statement is a vendor's, it says so.

## 1. What makes a conversation a project

`store.Chat.Mode` holds `chat` or `coding`, and `store.Chat.Workspace` holds the folder.
`store.Chat.IsProject` asks **both**: `Mode == ModeCoding && Workspace != ""`. Half a project
is a chat — a coding chat whose folder was never set has nothing to work in, and a folder
without the mode must not see the file tools.

The mode is stored rather than derived from the prompt preset, because a preset is text the
user can edit, copy or delete, and the *tool set* must not change when somebody retypes a
textarea.

The frontend asks the same question (`App.tsx`, the sidebar's two groups) so the window
cannot disagree with the gate about which rows are projects. `IsProject` on the Go side is the
one that decides what runs.

## 2. The folder boundary

`internal/workspace` owns it. `Root.Resolve` resolves every path the model writes against the
project root and refuses anything that leaves it, including through a symlink; `Root.Read`,
`List`, `Search`, `DescribeWrite`/`Write` and `DescribeEdit`/`Edit` are the only operations
there are, and none of them does its own path work.

Nothing falls back. A chat with no folder, a folder that has been deleted, a path outside it —
each is a refusal with a code the window translates, never a read from wherever the app
happens to be running. `workspaceFor` in `internal/tools` is where that is enforced for every
tool at once.

## 3. The confirmation gate

Every tool that changes something goes through `Service.Confirm`, which the app wires to
`App.confirmChange` (`api_changes.go`). The shape of it:

- `tools.NeedsApproval` decides *which* tools are gated, from `GuardedToolNames` — the two
  file writers and `run_command`. The engine derives both the advertised set and the executed
  set from that list, so a fourth tool of this kind cannot be added and forgotten.
- The tool computes what it *would* do and asks. `workspace.DescribeWrite`/`DescribeEdit` exist
  for that alone: a person cannot consent to "write_file pkg/a.go", and asking after the write
  is not consent at all. The change is then **re-derived** when it is performed, because a
  preview that travelled through a window and came back must never be what gets applied.
- The question is an event (`chat:approval`), and the turn's own goroutine blocks on it. No
  further tool call or model round can pass an unanswered question, and every other chat keeps
  running, because the block is one turn's goroutine.
- The answer comes back through the bound `ResolveToolApproval`, naming the tool call's id.

**It fails closed.** A cancelled turn, a request with no tool call id to name, a double answer,
and a build with no confirmer at all every end in "not allowed". A confirmer's error is
classified as a refusal whatever it says (`Service.approve`), because a call recorded as a
*fault* is one the model retries, and a declined change being attempted again is the loop this
gate exists to prevent.

**"Always in this chat"** is remembered per chat id, in memory, for the life of the process. A
permission given for a task somebody was watching, remembered invisibly the next morning, is a
permission nobody remembers giving. It is dropped when the chat is deleted.

**The standing answer** is the other kind of yes, and unlike the one above it is *set* rather
than given. Settings → Coding agent holds two switches — `AutoApprove.Files` for `write_file`
and `edit_file`, `AutoApprove.Commands` for `run_command` — and both are off in `Default()`.
`App.autoApproved` answers by **kind, not by name**: a guard tool the switch does not name
keeps asking, so a tool added later cannot inherit a permission somebody granted for the tools
that existed when they granted it. It is consulted before the event is emitted, so an
auto-approved call behaves exactly like a read tool: no card, no blocked turn.

What is given up is the *reading*, and that is the whole of the trade:

| | With the question | Auto-approved |
| --- | --- | --- |
| A file change | the diff on the card, before it is written | written first; the diff is read afterwards, in the transcript |
| A command | the line, the folder, the shell and the limit, before it runs | runs first; only the output is left |

So the two switches are separate, and the reason is not a preference: a file change is a diff
somebody can read afterwards and a commit they can revert, while a command runs with their own
rights, can leave the folder, and leaves nothing to review. Trusting the model with the files
says nothing about the machine. The project header says which of the two is on — the badge
reads *writes without asking*, *commands without asking* or both — because a header that still
said "with confirmation" would be the one place in the window that lies about what the model
may do to the folder.

The two answers are additive rather than alternatives: a chat that was granted "always" needs
no setting, and a setting on makes the per-chat answer irrelevant. Deleting a chat drops only
the per-chat one.

**The card offers the switch itself**, because the standing answer used to be discoverable
only by someone who already knew the tab existed. Under the three buttons there is one link —
*stop asking about file changes*, or *stop asking about commands*, named for the card it sits
on — which turns the matching switch on **and** allows the change on screen. Four decisions
keep that from being a trap:

| | Why |
| --- | --- |
| It is a link, not a fourth button | It is not another answer to *this* question, it is the switch that makes there not be a next one. A friendly-looking fourth button in the same row is how somebody grants a permission they did not mean to. |
| The setting is saved *before* the answer | A failed save leaves the card on screen. The other order would close the question and report a permission that was never stored — the one combination that is worse than either. |
| It grants only its own kind | The two switches are separate for the reasons above; a click about a file change must not start running commands unread. |
| The tooltip says it outlives everything and where to undo it | A permission granted in passing is one nobody remembers giving, so it says *in every project, and after a restart* and names Settings → Coding agent. The toast after the click repeats where to undo it. |

What it answers with is `once` for the waiting call, not `always`: the switch covers every
later call, and pretending the current one was auto-approved would make the record say
something that did not happen.

## 4. The terminal

`run_command` runs **one line** in the project folder, and it is the tool whose effect cannot be
previewed — `rm -rf build` is its own description. So the card shows everything needed to judge
it: the line, the folder, the shell that interprets it, and how long it may take.

**The shell, per platform.** This is the decision most worth reviewing:

| Platform | Shell | Why |
| --- | --- | --- |
| Unix (Linux, macOS) | `/bin/sh -c` | POSIX guarantees the path; `make check`, `go test ./...` and every README line mean what sh means. A developer's interactive shell brings aliases, `setopt` behaviour and `**` globs that a model cannot know and a user cannot see. |
| Windows | `cmd.exe /C` | Always present at one path (PowerShell exists as two executables and may be on no PATH); its quoting is what `npx`, `npm` and `go` document; `/flags` are not parsed as paths; `&&` chains. |

A command that needs another shell can ask for it by name — `bash -c "…"`,
`powershell -NoProfile -Command "…"` — which is itself a line this shell runs, and the choice
then appears on the card instead of being hidden in a setting. It is not a setting: a shell the
user cannot see on the card is a shell they cannot review.

**Administrator rights are refused, not attempted.** The app runs every line with the user's
own rights, with stdin closed and no terminal, so a password prompt has nowhere to go: `sudo`,
`doas` and `pkexec` fail at once, and on Windows the UAC window opens outside the app entirely.
Left alone that failure arrives *after* the person has read and allowed the line — a card, a
click, then a refusal they could have been told about first. So `elevationRequest` reads the
simple commands of the line and refuses one that starts with `sudo`, `doas`, `pkexec`, `su`,
`runas` or `gsudo` (after leading `NAME=value` assignments and an `env`), with
`tools.needsElevation` and a sentence that names the way forward.

It is a hint and not a boundary, and saying so is the point: `env sudo`, `sh -c "sudo …"` and a
Makefile that calls sudo all get past it. The boundary is that there is no terminal to prompt in,
which holds whatever shape the request takes. What the parser buys is a saved click and an
instruction instead of a mystery failure — and it does not fire on a line that merely mentions
the word (`grep sudo README.md`, `echo sudo`, `sudoku`), because a false refusal has no override.

**The line stays copyable on the card**, for every command rather than only a refused one: a
command worth refusing is a command worth copying, and a button that appears only once something
broke is one nobody knows is there. The card copies the unedited string the tool was given, not
a rendering of it, so pasting it into a terminal runs what the model asked for. For a refusal
that is the whole recovery path: the sentence says to run it yourself, and the line is right
there.

**And the line is handed back where the person types.** A refusal also raises a card above the
composer — *run it yourself*, the line, a copy button, a close button — because the transcript's
card sits several screens above the composer and the next move after a refusal is to type. It is
recognised by the message's **code** (`tools.needsElevation`), never by its sentence: the
sentence is translated, and which refusal this is has to survive a language switch. It is keyed
by chat and set from the event rather than derived from the history, so a project opened
tomorrow does not offer a hand-off for a refusal it answered yesterday, while one that is still
waiting is found again after switching away. Two steps on one card — copy the line, run it,
paste the output back — because copying alone leaves the answer exactly where it was: the model
has no way to know the command ran. Dismissing it loses nothing, since the transcript's card
keeps the line.

**A failed call says why without being opened.** The tool card shows its error under the header
instead of only behind the expand toggle. That block used to be inside it, which meant the one
part of a card a person has to read to act on — why it failed — was the part they had to know to
look for.

**Not sandboxed, and not pretending to be.** The process runs with the user's own rights and
can leave the folder, start anything, and reach the network. The gate is a person reading the
line, and everything else in this file is arranged around that being possible. Pretending
otherwise — a path check, a command allowlist — would be a promise this app cannot keep, and a
user who trusted it would be worse off than one who reads the card.

**What is bounded:**

| Bound | Value | Why |
| --- | --- | --- |
| Output kept | 32 KiB from the start, 32 KiB from the end, the middle dropped and counted | A build says what it is doing first and what failed last. Keeping one end loses the story or its conclusion. |
| Default timeout | 120 s | Sized for a test suite or a build on a slow machine. Too short is worse than too long: it turns a working command into a mystery. |
| Maximum timeout | 600 s | Beyond that it is not a command, it is a service, and a service belongs in a terminal the user owns. |
| stdin | closed | A command that stops to ask a question gets EOF immediately — `git commit` without `-m` complains instead of a two-minute silence. |
| `WaitDelay` | 5 s | Bounds the wait *after* the process is gone: a grandchild inheriting the pipes is what keeps `Wait` from returning. |

**The timeout kills the tree.** `go test` is the parent of the test binaries it builds; `make`
is the parent of every compiler it starts. Killing the shell alone leaves those writing to the
project folder after the result said the command was stopped, and holding the pipes the call is
reading. So the command starts in its own process group and the kill addresses the negative
pid (`syscall.Kill(-pid, SIGKILL)`); on Windows there is no group to address, so `taskkill /T
/F` walks the parent chain, with the direct kill as the fallback. `internal/runtime` and
`internal/images` carry the same helpers for the servers they supervise; this is the third
copy, and the cleanup — one small package for all three — is noted in `command_unix.go` rather
than done in the change that added a tool.

**The output is reported while it runs.** A command that prints nothing until it ends is
indistinguishable from one that hung — the card says "running", the phase line says "tool",
and two minutes pass — so `chat:tool-output` carries the output as it arrives and the card
shows it under its own heading, scrolled to the newest line.

Three decisions, all of them sizing the feature against the build it exists for:

| | Choose | Why |
| --- | --- | --- |
| What travels | the output **so far**, not a delta | The card replaces one string instead of accumulating one, so the window needs no cap of its own; and the backend bounds it once. |
| How much | the last 8 KiB (`commandStreamBytes`) | The end is where a build's errors arrive. The finished result still keeps both ends — this is only the view while it runs. |
| How often | at most every 150 ms (`commandStreamInterval`) | A compiler writes thousands of small chunks; one message per chunk would cost more than the build. The command's end sends what the last interval held, so the final line is never the one missing. |

The report names the call it belongs to (`toolCallId`), not the chat, so two calls in a row
cannot inherit each other's text, and the window drops the entry when the call's own record
arrives — the record carries the whole output, and a second copy beside it would be shown
twice. Nothing is reported when nobody is watching, which is what a test-built service is.

## 5. Starting over: `/clear`

A chat can be answered by creating another one. A project cannot: **a project is its
conversation**, so a second one on the same folder would be a second copy of the settings
that make it that project — the folder, the prompt, the model — and the two would drift.
Starting over therefore happens *in* the project, with `/clear` typed in the composer, which
calls `App.ClearChat` and leaves everything that is not the conversation in place: the folder,
the prompt, the model, the sampler values and the title.

What goes is the conversation and everything it accumulated. `Store.ClearChat` does it in one
transaction, and two of the four things it removes are the ones a naive clear would forget:

| Removed | Why it has to go |
| --- | --- |
| `messages` and their `tool_calls` | The call table holds no foreign key to messages, so a row left behind is an orphan nothing in the window can show. |
| `compactions` | They are summaries of the messages being deleted. Kept, they would be sent as the conversation's *memory* on the next turn, describing a history that no longer exists. |
| `attachments`, `documents`, `chunks` | An attachment's extracted text is retrievable through `search_documents`. Kept, a chat that had "cleared" its context could still answer a question out of the file it was cleared of — silently, and with a correct-looking answer. |
| the measurement and the per-chat "always allow" | Both live in memory, not in the database: the measured prompt size describes messages that are gone, and the permission was an answer about a conversation that has ended. |

It is **refused while the chat is answering** (`err.chat.turnRunning`, the sentence a second
send already gets). A running turn appends its reply and its tool results as it goes, so a
clear underneath it would delete those messages and then watch new ones arrive — a chat that
is neither cleared nor whole. Stop ends the turn, and the same command then works.

The command is matched **exactly** (`/clear`, case-insensitive, no arguments) so a message
that merely begins with a slash still travels as a message, and it works in any chat rather
than only in projects: the sentence is the same either way, and a rule that depended on the
mode would be one more thing to remember. The habit it needs is one line in the composer —
`/clear empties the chat` — because a command nobody knows about is a feature nobody has.

For a project this is also the answer to "the context is full": the context is the
conversation plus the folder's `AGENTS.md`, so clearing it starts with an empty history that
still carries the project's own guidance.

## 6. What a project's prompt carries

`internal/chat/project.go` injects the folder's `AGENTS.md` as its own budget segment
(`SegmentProject`, labelled with the file name), read from the project folder on each turn and
memoised on the file's size and modification time. The block is charged to `budget.Memory`, so
it appears in the context meter and in what a compaction has to fit.

It is the *project's* file that is read, never this repository's: a project points at its own
folder, and a project whose folder has no `AGENTS.md` gets no block at all. The everyday cost
is therefore that file's tokens — hundreds for a typical file, thousands for a dense one — and
the Context Inspector shows the real count for the folder actually opened. The cap is a share
of the model's window, because a flat byte count cannot express the rule that matters: this
file must never dominate a small model's context.

## 7. Watching it without the app

`make frontend && go run ./tools/uiharness` renders the whole mode against a mocked bridge: the
sidebar's two groups, a project's folder and permission note, and a confirmation card in its
four file shapes and its command shape. `?approval=command` is the terminal's question —
pressing *Allow* or *Decline* finishes the turn and leaves the tool card reading `done` or
`declined`; *Allow* also shows the output arriving under the card before the record replaces
it, which is the one place the live view can be watched without running a build.

The link under the three buttons is the standing answer, and the card is where it can be
pressed: it closes the card, finishes the turn, and changes the project header's note to what
now runs unread — which is how the save is observable at all, since the Settings tab is not on
screen. `?autoapprove=files|commands|both` is the setting already in effect: the coding tab's
switches on, and the project header's note instead of the one about confirmation. It cannot
show the absence of the card — the harness emits the question itself, so `?approval=…` still
renders one — because *whether the app asks* is the Go side, pinned by
`TestAStandingAnswerSkipsTheFileQuestion`. The flag table is in the
[`verify-ui`](../.claude/skills/verify-ui/SKILL.md) skill.

The harness proves rendering, state flow and event wiring. It proves nothing about the backend:
neither the shell, nor the timeout, nor the tree kill is exercised there. Those are covered by
`internal/tools/command_test.go`, which runs real commands — including one whose grandchild
tries to write a file after the shell has been killed.
