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

**One project per folder.** `CreateProject` refuses a folder another project already works in,
with a sentence naming that project and pointing at `/clear`. This is the same rule §7 states
from the other side — a project *is* its conversation — and the reason it is enforced rather
than documented is that two chats on one folder would each keep their own undo record and
their own history of writes to files the other believes it knows. The comparison asks the
filesystem (`os.SameFile`) rather than comparing strings, because a symlink and its target are
one folder with two names. Archived projects do not claim one: an archived chat is one the
user has finished with.

The prompt a project starts on is the user's **coding-agent preset** when they have edited it
(`codingAgentPrompt` in `app.go`), and the shipped body otherwise — the textarea under
Settings → Prompts is where those instructions are meant to live, so a rewritten one has to be
what the next project starts with.

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

**The card shows a diff, not the two files.** `workspace.CompareDiff` compares the texts line by
line and sends *hunks*: the changed lines with three unchanged ones either side, numbered on
both sides, and the unchanged span between two hunks left out entirely. So a one-line change
inside a 400-line file is eight rows — the old line, the new one, and the three around them —
where it used to be the first 16 kB of two pages that differ by one character somewhere in the
middle. The comparison trims the equal head and tail before it builds a table, which is what
keeps that ordinary case cheap; past `maxDiffCells` the middle is reported as one replacement
rather than compared. It is computed in Go because the changes panel draws the same thing, and
because a byte budget at the transport is not a shape hunks can be cut from.

**The one change with no rows says so.** A line diff splits on newlines and compares the text
between them, so adding or dropping the *final* newline of a file produces no rows at all — and
the card would then read "no line differs" about a file whose last byte is about to change. The
diff carries `NewlineOnly` for exactly that case and the card says which of the two empty diffs
it is showing. Every other difference in the bytes produces a row; a carriage return does, and
its rows are shown with the `\r` hidden rather than rendered.

**A diff that is longer than the card says so.** `maxDiffRows` and `maxDiffBytes` bound what one
change sends — a full rewrite is one enormous hunk, and the card is a preview in a fixed-height
box — and the totals beside the path count the *diff's* lines, not either file's, so they match
the rows below them. An edit's hunks carry the one span the tool call names, so a `replace_all`
that matched twelve places still reports `Occurrences` above a diff that shows one: the totals
are over every replacement, the rows are not, and the card says which is which.

**A change that cannot be applied is never put on the card.** Both writers refuse during
`Describe*`, not only during the write: a file that is not text (its preview would be a page of
replacement characters, and it has no undo to put back), a path whose parent is a file
(`MkdirAll` would fail with ENOTDIR), a replacement large enough to push the file past
`MaxWriteBytes`, and a text too large to write at all. Finding out after the click is the one
outcome the describe-then-apply split exists to prevent.

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

**A line that would discard uncommitted work is refused too.** The file tools can take a change
back, because every write keeps the text it replaced; a command cannot be given the same
treatment, and the reason is the shape of that record: it is keyed by (chat, path) and keeps the
*first* pre-image the chat saw, so a snapshot taken before `git reset --hard` would mean two
things in one row — "before this chat touched the file" and "before this command" — and the
panel has one button for both. So the promise is the narrow, honest one: the work is not lost.

`refuseDestructiveGit` reads the simple commands of the line, as `elevationRequest` does, and
recognises the forms that throw text away: `checkout -- <paths>` (with a tree-ish before the
`--`, and the dot form `git checkout .`, because a ref may not begin with a dot), a forced
checkout, `restore` of the working copy (`--staged` alone only unstages, which loses nothing),
`reset --hard`, and `clean -f` without `-n`. The scope — the pathspecs, or the whole folder — is
measured against `git status --porcelain` with those same pathspecs, so `git checkout -- b.go`
is allowed while `a.go` is dirty, and `clean` counts untracked paths while `reset --hard` counts
tracked ones and leaves untracked alone. It fires before the card is built, for the reason the
elevation refusal does — nobody should be asked to approve a line the app will not run — and
again on an edited line, because the card is a text field. On a clean tree the same line is a
no-op and runs, so the guard only ever speaks when there is something to lose.

It is a guard and not a boundary, and the limits are named rather than hidden: `bash -c "git
reset --hard"`, an alias, `cd sub && git reset --hard`, `git -C ../elsewhere …` and a line with a
quoted path are all left alone — the first two because the words are not the command, the third
because the pathspecs are relative to another folder, the fourth because the scope would be
measured in the wrong tree, and the last because the word split cannot size it. What it buys is
the routine mistake, which is the one that actually happens.

**The line stays copyable on the card**, for every command rather than only a refused one: a
command worth refusing is a command worth copying, and a button that appears only once something
broke is one nobody knows is there. The card copies the unedited string the tool was given, not
a rendering of it, so pasting it into a terminal runs what the model asked for. For a refusal
that is the whole recovery path: the sentence says to run it yourself, and the line is right
there.

**The question's line is editable, and the edit is what runs.** A typo in a path, a flag the
model guessed, `-run TestRegistry` where the whole suite was meant: the person reading the card
is the one who knows, and declining to retype the line in the composer costs the turn a round
and loses the folder, the timeout and the shell the tool chose. So the field on the card is the
answer, not a quotation of it, and the edit travels back with the answer
(`ResolveToolApprovalWith`, `ApprovalRequest.EditedLine`) rather than being spliced into the
tool's arguments somewhere. Two consequences are load-bearing:

- **The refusal runs again on the edited line.** `elevationRequest` is checked after the answer,
  on what was actually approved, because the field is a text box and the one thing an edit must
  not do is slip past the check that runs before the question. Pinned by
  `TestAnEditedLineCannotSlipPastTheElevationRefusal`.
- **An emptied field is not an answer.** *Allow* is disabled while the line is blank, so there
  is no path that runs `""` and reports the shell's complaint as the tool's failure.

The card says which text ran: while the field differs from the model's line, a sentence above
the folder note says the person's version is what runs, offers *reset the line*, and repeats
that the administrator check applies to it too.

**And the line is handed back where the person types.** A refusal also raises a card above the
composer — *run it yourself*, the line, a copy button, an open-terminal button, a close button —
because the transcript's card sits several screens above the composer and the next move after a
refusal is to type. It is recognised by the message's **code** (`tools.needsElevation`), never
by its sentence: the sentence is translated, and which refusal this is has to survive a language
switch. It is keyed by chat and set from the event rather than derived from the history, so a
project opened tomorrow does not offer a hand-off for a refusal it answered yesterday, while one
that is still waiting is found again after switching away. Three steps on one card — open a
terminal, run the line, paste the output back — because copying alone leaves the answer exactly
where it was: the model has no way to know the command ran. Dismissing it loses nothing, since
the transcript's card keeps the line.

**The terminal is the user's, and the app only points it at the folder.** *Open terminal* calls
`OpenProjectTerminal`, which resolves the chat's folder, checks it still exists, and starts the
program the person already uses through `internal/runtime`'s launcher — `gnome-terminal`,
`konsole`, `xfce4-terminal`, `kitty`, `alacritty`, `foot`, `terminator`, `xterm`, `urxvt` on
Linux in that order; `open -a Terminal` on macOS, which takes the folder as a document and so
needs no AppleScript permission; Windows Terminal, else the shell `ComSpec` names, with
`CREATE_NEW_CONSOLE`, whose working directory is set on the process rather than through a
command line. Four things follow, and each was the reason for a decision:

- **No embedded terminal.** A terminal inside this window would have to answer a password
  prompt, which is the exact thing the refusal above exists because the app cannot do.
- **A program, not a generic launcher.** `x-terminal-emulator` is a symlink to whatever the
  distribution chose, and the flags it accepts follow that choice: `--working-directory` on a
  build that is xterm opens in the user's home and reports success, and even `-e` is not
  interchangeable (the rxvt family takes the rest of the line as argv, the gnome-terminal
  family one string). So it is not in the list at all. Naming the programs costs a list and
  buys an answer that is either right or absent; a terminal nobody named falls through to
  "no terminal was found" instead of a window that flashes an error.
- **The folder is one argument, never part of a script.** The two rxvt-family terminals are
  given `cd "$1" && exec "${SHELL:-/bin/sh}"` with the folder as `$1`, so a directory named
  `a; rm -rf ~` is still a directory. A missing folder is refused before anything starts,
  because a terminal opened somewhere else looks like it worked.
- **A start that fails immediately is reported as a failure.** A terminal given a flag it does
  not know prints its usage and exits non-zero, and `Start` reports success for that — the
  process started, it just did not open a window. So `OpenTerminal` watches the first 500 ms
  and returns the exit error when there is one; a terminal still running after that (or one
  that handed off to an instance already running and exited 0) is the outcome asked for.

The window keeps it open afterwards: it is started in its own process group and not waited for,
so a shell somebody types in all afternoon is not owned by a turn. `ErrNoTerminal` has its own
sentence ("no terminal was found") because it asks for something different than a start that
failed — a program to install rather than a retry. `open_terminal_test.go` pins all four cases
with a recorded launcher, so the suite never opens a window; the launcher's own startup watch
has no automated test, because making one reach the real `Start` path would mean putting a fake
terminal on `PATH`.

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

**One command can be stopped without stopping the answer.** The turn's Stop cancels everything
and abandons the reply, which is the wrong price for a `go test` started with the wrong flags.
So a command registers itself on the way in (`Service.beginCommand`, keyed by chat and tool
call, because two chats can run the same line) and the card offers *stop the command* while the
call is still `pending`. The stop cancels that call's own context — a child of the turn's, so
Stop still reaches it — and the tool answers with the output so far plus "you stopped this,
the output is incomplete", as a *result*: the model reads it and decides, the turn carries on,
and nothing is marked as a fault. `proc.KillTree` ends the tree, not the shell alone.

## 5. Reading the folder, and the shortcut that must not lie

`read_file`, `list_dir`, `search_files` and `find_files` are the read side; `find_files` exists
so a model can ask "which tests exist" in one call instead of walking a tree a level at a time,
and `search_files`/`find_files` both skip what `.gitignore` excludes (counted in the result, so
an empty answer does not read as an empty folder).

**A hit is a window, not a line.** `search_files` returns at most `MaxHitLine` bytes per hit,
taken *around* the match with an ellipsis on whichever side was cut (`workspace.clipHit`). A
line is the unit a reader thinks in, but a minified bundle or a generated JSON is one line of
megabytes: returning it whole spends the answer's tool budget on a single hit, and cutting from
the start hides the very text that made the line a hit. The same bound is why a `read_file`
whose line is larger than the whole 80 kB window answers *"line 1 is longer than the window,
search for it"* instead of *"the file is empty"* — the window never starts, and the second
sentence is the one reading that makes a model rewrite a file it thinks has nothing in it.

**A repeat read of an unchanged range is answered with a sentence** rather than the file
(`internal/tools/reads.go`): a model checking whether it still remembers a file it read four
rounds ago does not need it again. The memory is per turn and per chat, bounded, and dropped
when the turn ends.

**What arrives instead of a large file may be a summary.** With a condensation model chosen in
Settings → Tool results, a `read_file` result above the threshold is condensed before the
model reads it; the notice names the way back (`offset`/`limit`, which is never condensed) and
the framing — the continuation line and the nearest `AGENTS.md` — is carried through verbatim.
The rules live with the mechanism, in [COMPACT.md §17.6](COMPACT.md), because they belong to
the condensation rather than to the read.

**A big file has a fourth answer: ask for the map.** `read_file` with `"outline": true` lists
the declarations it recognises with the line each starts on, and no bodies: `app.go` is 1 939
lines, 79 declarations, **24 429 tokens whole and 1 597 as an outline** — and the numbers in it
are the numbers a read prints, so the next call is `offset=N&limit=M` for the forty lines that
matter. (Measured on this checkout with `llm.EstimateTokens` over the file and over the list,
not estimated.) It is a line
scanner rather than a parser (`workspace.Root.Outline`), which is a trade: it can miss a shape it
does not know, so it says when it found nothing instead of listing guesses, and a log or a data
file gets *no declarations recognised* plus a pointer at the window. Four rules keep it honest —
the families are chosen by extension (Go, JS/TS, Python/Ruby, Rust, shell, C-like, and Markdown
headings outside code fences); leading indentation is kept, so a Python method or a Rust `impl`
member reads as one instead of looking module-level; what it cannot see it does not invent — a JS
class method is not listed, and JSON and SQL get an empty list and the "no declarations"
sentence, all pinned per family in `TestOutlineReadsEachLanguageFamily`.

What is left over gets two answers rather than none, because "no declarations recognised" was
true of the scanner and useless to the reader: `Makefile`, `Dockerfile`/`Containerfile` and
`go.mod`/`go.work` are matched **by name**, since their name is their format, and everything else
gets a generic one. On this checkout: the 673-line `Makefile` is **14 709 tokens whole and
1 498 as a map of its 91 targets and variables** — a recipe line starts with a tab, so none of
the commands are in it — and the 61-line `go.mod` is **1 080 against 26**, because it lists the
directives (`module`, `go`, `require`) and not the requirement lines they open. A Dockerfile
lists its instruction keywords instead. The rest — a `.yaml`, a `.toml`, an `.ini`, a `.conf`, a
file with no extension — lists `key:`/`key =`/`[section]` lines, so a Kubernetes manifest or a
`tox.ini` gives a map instead of nothing. **That fallback keeps the key and drops the value**
(`keyOf`): it runs on files this app does not know, which is where a password or an API key is
kept, and the list goes into the model's context — `POSTGRES_PASSWORD=hunter2` arrives as
`POSTGRES_PASSWORD=`, pinned by `TestAGenericOutlineKeepsTheKeyAndNotTheValue`. The Makefile is
the one that keeps its values, and deliberately: `windows: TARGETOS := windows` and
`windows: build` are different declarations of the same target, which a cut to `windows:` would
print three times as if it were a bug. A record or a pattern per line is still refused —
`.json`, `.jsonl`/`.ndjson`, `.csv`/`.tsv`, `.log`, `.txt`, and the generated `.lock`/`.sum`/
`.map`, since a lockfile's four hundred package names are a list the manifest above it already
contains. `TestOutlineFallsBackForUnrecognisedFiles` holds all of these shapes.

C and C++ needed their own rule because their definitions carry no keyword: `int main(void) {` is recognised by *shape* — a return type,
a name, an argument list and a brace — and refused when it is a statement, which is the same
shape `if (x) {` and `for (int i = 0; …) {` have. The refusal is where the work is: the text
before the argument list has to look like a type and a name (`cSignaturePrefix`), a lambda or
assignment is refused on its `=`, `operator==` is exempted from that refusal, and a leading
non-letter refuses `} else if (…)` and a preprocessor line. `TestOutlineCatchesCFunctions`
holds 11 definitions that must be listed and 19 lines that must not be, because a false
positive there sends the model to a line that is not a declaration. What that rule still
misses: the brace on its own line (Allman style), a macro wrapper such as `TEST_F(A, B) {`, a
definition split across lines, and a prototype (`int f(void);`) — so a header whose functions
are only declared lists its structs and enums and none of its functions; it stops at 400 declarations and 4 MB scanned and says how
much it left out; it reads the whole
file, because the cost it saves is the model's context and not this process's memory; and it
never touches the turn's read memory, because a map is not a reading of the section — a model
that outlines a file and then asks for it gets the file, and one that has read the file and then
asks for the map has not "already read" it. It is also exempt from the condensation below, which
is a rule stated in [COMPACT.md §17.6](COMPACT.md).

**How it gets read is recorded, not guessed.** Every `read_file` call leaves one `tool.call` in
the diagnostic trace with `phase` set to one of four words: `bare` (a whole-file read),
`bounded` (a window the model chose), `outline`, or `continuation` — a window taken from this
app's own `offset=N` offer, which no argument can tell apart from a model guessing at a line
(`tools.Result.Shape`, `ReadShape*` in `internal/tools/project.go`; the offer is kept per turn
beside the read memory in `internal/tools/reads.go` and dropped by a write or a command, because
a write moves the lines it names). Settings → About shows the trace's path; a session is then
one line of shell — `grep` rather than `jq`, because `jq` is not installed on the machine this
was written on:

```sh
# in the folder Settings → About names
grep '"tool":"read_file"' kichat-trace.jsonl | grep -o '"phase":"[a-z]*"' | cut -d'"' -f4 | sort | uniq -c
```

The filter by tool is not decoration: `turn.end` writes `complete`, `failed` or `interrupted`
and a search writes `cached` into the same field. A read that failed writes no phase at all, so
the four counts need not add up to the number of read cards in the transcript.

The trace has no content field and no path field, so what comes back is a distribution and
nothing else — which is the whole point: this is the measurement the outline was shipped on. If the counts stay
dominated by `bare`, the tool description needs work before the feature does.

**What it must never answer is "did my write land".** The cache is therefore *told* about
changes rather than left to notice them from size and modification time: `write_file` and
`edit_file` forget the path they touched, and `run_command` forgets the whole chat's memory,
because a command can rewrite anything (`gofmt -w .`) and the app cannot enumerate that. Size
and mtime remain the optimistic half — they catch a file changed outside the app — but they are
deliberately not the guarantee, because a filesystem with coarse timestamps or a same-length
replacement defeats both. `TestAReadAfterAWriteReturnsTheFile` gives the write the same byte
length and puts the modification time back, so only the invalidation can make it pass.

**A subdirectory's `AGENTS.md` arrives with the file that needs it.** `read_file` appends the
nearest `AGENTS.md` above the file it just read (`workspace.AgentsFileNear`, root excluded —
that one is already in the system prompt), in the same `<project_agents_md>` markup, with one
sentence saying it is project knowledge and not an extension of what may be read. The nearest
file wins rather than a stack of them: a rule written in `api/` already knows about the root's,
and three documents that disagree are worse than the one the person meant.

## 6. Undoing a change

A model's change is only safe to accept if it can be taken back, so every `write_file` and
`edit_file` records what it replaced. Three decisions:

| | Choose | Why |
| --- | --- | --- |
| Where | the database (`file_snapshots`, migration 12) | The database is encrypted at rest; a copy of somebody's source in a plain folder beside it would be the one thing the encryption did not cover. It also travels with the chat, and is deleted with it. |
| What | the text *before the chat's first change*, and the digest of what was last written | The question people ask is "put this file back the way you found it", not "undo round seven but keep round eight" — the model usually takes several rounds to get a file right. One row per (chat, file), so ten edits store one copy. |
| When | read before the change, written after it | Read before, because reading afterwards records the new text as the old one. Written after, because a change that was refused or failed must not leave a row claiming something happened. |

The digest is what makes the undo safe rather than eager: `RevertFileChange` compares it with
the file on disk and **refuses when they differ** (`project.revertConflict`), because a revert
is a write and overwriting an edit somebody made in their own editor is exactly the accident a
one-click undo would cause. A file the chat *created* is removed rather than emptied — an empty
file is not the state the folder was in — and folders a write created are left, because by now
they may hold work the chat did not put there. A running turn is refused too: the model might
be writing that file at this instant. A file that is binary or larger than a change may touch
has no undo at all, and the panel says so instead of offering a button that could only fail.

The record goes when it is no longer true: `/clear` and deleting the chat delete all of them —
an undo offered for a change the history no longer describes is a button nobody can check. A
revert is the one case that does not delete the row.

**A revert is news to the model, not only to the folder.** A tool result in the transcript is a
claim about a file — "written, 40 lines", "edited, 1 occurrence replaced (line 12)" — and it is
replayed on every later turn. Put the file back and those claims become false while the
history keeps making them, and that is the one failure the folder cannot correct: the folder is
right, the transcript is wrong. So a revert marks the row (`file_snapshots.reverted_at`) instead
of deleting it, the panel leaves a marked row out, and the next turn's prompt carries a
`<project_reverted_files>` block naming each file with one sentence: your earlier results about
these are out of date, read one again before you edit it or quote its contents. Three
properties, each a decision:

| | Choose | Why |
| --- | --- | --- |
| What it carries | paths, not contents | Sending the restored text back would be a second copy of a file the model can read with one call, and it grows with the file. The point is "what you think you know is stale". |
| How often | once | The marker is cleared after the request that carried it has reached the model, so the note does not become a permanent part of every request. A turn that failed before that point keeps the marker, so the correction is not lost with it. |
| Who gets it | a project, gated on its mode like the guidance | A chat with no folder cannot have changed a file of one; a research conversation whose store somehow holds a marker still gets no paths from a folder. |

A later write of the same file clears the marker rather than starting a second row: the file is
the model's work again, so there is nothing stale to report. The pre-image is not touched by any
of this — the text to put back stays the text from before the chat first touched the file.

**The other way a tool result goes stale is the user's own editor.** The snapshot already holds
the digest of the text the app last wrote, so a file whose digest has moved on was changed
somewhere the app cannot see — an editor, a formatter in another window, a checkout that
replaced the tree — while the transcript still says the model's text is in there. The next
turn's prompt therefore carries a `<project_changed_on_disk>` block, built at the start of the
turn the same way the revert note is, naming each file (and saying when one is gone). It is
*not* the revert note with another trigger, and the wording keeps them apart: nothing was
undone, the text on disk is the person's own, and a model told otherwise would go looking for a
version nobody restored.

| | Choose | Why |
| --- | --- | --- |
| Whose files | the ones this chat wrote | Those are the ones with a recorded digest to compare against, and the ones a tool result in this transcript describes. A file the model merely read and the user then edited has no recorded version to differ from; within a turn the read shortcut's size-and-mtime stamp catches it, and the next read returns the text, which is the answer that matters there. |
| How often | once per outside version | `file_snapshots.noted_sha` (migration 14) holds the digest that was announced, so a block the model ignored does not grow every request after it, while a file edited again after being announced is a new fact and speaks again. A write clears the marker with the digest, because then the file is the model's text again. |
| Where it is checked | once, at the start of the turn, against the folder | The alternative — checking on every read — cannot help: the model has to be told *before* it decides what to do, which is the same reason the revert note is assembled up front rather than added to a tool result. |

What it costs is one query and one read per file the chat ever wrote, at the start of a turn
whose project has changed files at all; a chat whose folder is untouched by anyone reads the
same files it wrote and finds the digests equal. A file that is now binary or too large to
compare is skipped rather than announced — there is no digest to compare, and guessing at one
would report a change nobody measured.

**The panel** (`ProjectChanges.tsx`, beside the folder badge in a project's header) answers
"what has this chat done to my folder" — the question the transcript answers only as a hundred
tool cards, in which a file rewritten four times appears four times with nothing saying the
fourth replaced the first three. It lists one row per file with a diff against the file as it is
now, how many times the chat wrote it, and either *put back* or the reason it cannot be. It is
loaded with the chat and refreshed when a write lands, so the header's count is the folder's
state rather than a hint that something happened once.

The list and the diff are built in Go (`ProjectFileChanges`), not in the window: a row carries
the same bounded hunk diff an approval does, computed against the file as it is now, so a chat
with fifty changed files sends fifty small diffs rather than fifty files through the bridge.

## 7. Starting over: `/clear`

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

## 8. What a project's prompt carries

`internal/chat/project.go` injects the folder's `AGENTS.md` as its own budget segment
(`SegmentProject`, labelled with the file name), read from the project folder on each turn and
memoised on the file's size and modification time. The block is charged to `budget.Memory`, so
it appears in the context meter and in what a compaction has to fit.

It is the *project's* file that is read, never this repository's: a project points at its own
folder, and a project whose folder has no `AGENTS.md` gets no block at all. The everyday cost
is therefore that file's tokens — hundreds for a typical file, thousands for a dense one — and
the Context Inspector shows the real count for the folder actually opened.

The cap is a flat **48 KiB** (`workspace.MaxAgentsBytes`), measured in bytes of the file and cut
back to the last complete line that fits, with `truncated="true"` on the block so a model that
finds no answer in what it was given can say the guidance was incomplete instead of concluding
the project has no rule about it. It is not a share of the model's window: what the
block costs against that window is the meter's subject, since it is charged to `budget.Memory`
and a compaction has to fit it like any other segment.

Beside it, when the folder is a git working tree, travels the **working tree's state**
(`SegmentGit`, labelled `Git-Status`): the branch (or `detached="true"`), how far it is ahead or
behind its upstream, and every pending path with the half of it that is staged, the kind of
change and its line counts. It answers the question the transcript cannot — is the earlier work
finished, staged, or already committed — without spending a tool round and a confirmation card
on `git status`, and it is what the model reads before it decides that a file it remembers is
still unmodified.

It is read from `git status --porcelain=v1 --branch` and `git diff HEAD --numstat`, and the
`--porcelain` format reports paths from the *repository* root, so the repository prefix is
stripped and anything outside the project folder is dropped: a project that is a subdirectory of
a larger repository is given paths relative to its own folder, never the surrounding tree's.
Three properties are load-bearing. It never writes (`GIT_OPTIONAL_LOCKS=0`, because `git status`
otherwise refreshes the index) and it never executes anything the repository configures
(`core.fsmonitor=false`, `--no-ext-diff`, `--no-textconv`) — a cloned project is somebody else's
config, and this runs unprompted, unlike a command the user allows. It is bounded twice: one
three-second deadline covers every call, `MaxGitEntries` (40) and a 4 KiB render cap bound the
list, and whatever is left out is counted. And it fails silently: a plain folder, a machine
without git and a probe that ran out of time all return `ErrNoGit`, which is the absence of a
block rather than a warning. Measured about **11 ms per git call** on this checkout (390 files,
20 runs); the assembly is rebuilt on every tool round, so a long turn pays that per round, which
is the number to revisit if the probe ever becomes noticeable on a large repository.

## 9. Watching it without the app

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

The project fixture also carries what the changes panel reads (`ProjectFileChanges`,
`RevertFileChange`): three rows — a changed file with a diff, a created one, and a binary that
cannot be undone — so all three states are on screen, and pressing *put back* removes the row
and drops the header's count. The card's line is a real field under `?approval=command`: typing
into it and pressing *Allow* answers through `ResolveToolApprovalWith`, which the harness logs,
so a caller that quietly used the plain binding would show up as a missing line in the console.

What the harness cannot show is the *stopping* of a running command: the fixture's commands
finish immediately, so there is never a `pending` call to press, and `StopToolCommand` answers
`false`. That path is `TestARunningCommandCanBeStoppedWithoutEndingTheTurn`, which starts a real
sleep and asserts the stop is a result rather than a fault.

The harness proves rendering, state flow and event wiring. It proves nothing about the backend:
neither the shell, nor the timeout, nor the tree kill is exercised there. Those are covered by
`internal/tools/command_test.go`, which runs real commands — including one whose grandchild
tries to write a file after the shell has been killed.
