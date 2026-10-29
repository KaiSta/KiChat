# Coding mode: what is left before it is ready

A readiness review of coding mode against one question: would I use this on a project the
size of KiChat? The mechanism is [CODING.md](CODING.md) and is not repeated here — this is
the work list, and it separates what was checked from what was not.

Written against `feat/coding` at `2ee29a6`: 28 commits and +16 546 lines ahead of the
released `0.6.2`, and in no tag. Re-check the defaults and line numbers before trusting any
of it; a TODO that describes a moving branch is wrong by default.

## What is already held

Short, because CODING.md carries the argument: the folder boundary is symlink-safe with no
fallback, every change goes describe → ask → diff, the terminal runs one line with stdin
closed and kills the process tree, and every write or edit keeps a pre-image in the
encrypted database that refuses to revert over somebody else's edit. About 110 tests cover
that path (workspace 52, tools 48, the project API 10), and `command_test.go` runs real
commands, including a grandchild that writes after the shell is killed.

## Blocks a big project

In the order the friction shows up on a multi-file change.

### 1. Six tool rounds per reply

`ToolIterations` defaults to 6 (`internal/config/config.go:1234`) and the Settings field caps
at **20** (`frontend/src/components/settings/GeneralTab.tsx:149`). When the budget runs out
the turn ends on tool results and asks for a new message
(`internal/chat/generate.go:436`).

Reading four files, editing two and running `make check` is already past six. Done means a
default that suits a coding project and a ceiling high enough that a refactor is not cut in
half — 20 is still tight.

### 2. 25 000 tool tokens per reply

`ToolTokenBudget` defaults to 25 000 (`internal/config/config.go:1235`), exposed in
Settings → Context (`ContextTab.tsx:106`). One `make check` failure dump plus three reads
spends it. The condensation (`chat: condense a large file read …`) relieves the read half and
does nothing for the round count above.

### 3. One file per change, one question each

A 20-file rename is 20 cards. `AutoApprove.Files` (`internal/config/config.go:105`, `:119`)
is the switch to turn on first: a file change leaves a diff in the transcript and a row in
the changes panel, a command leaves neither. Whether a project-level "apply these N edits"
shape is worth having is a product decision, not a bug — the gate is per call by design.

### 4. Deletions cannot be taken back

`rm`, `mv` and a `git checkout -- .` run through `run_command`: gated, but no diff, no
pre-image, no row in the changes panel. Every other change in this mode can be undone.
Done means a snapshot row before a command that removes or renames a tracked file — the same
`file_snapshots` machinery and the same revert path, which already knows how to refuse when
the file changed underneath it.

### 5. No git awareness

The model drives git as plain commands. Nothing tells it what changed since the last commit
and nothing warns before a line that discards work. The cheap half is a `git status`/`git
diff` summary in the prompt; the expensive half is refusing `checkout -- .` and `reset
--hard`.

### 6. Literal search only

`search_files` matches a literal string, `.gitignore`-aware, with hits clipped at 300 bytes
(`MaxHitLine`, `internal/workspace/workspace.go:538`). Regex means `grep` through the
terminal: correct, but a round and a card.

## Not verified

### The loop has never been run with a real model

Every test asserts a tool. None asserts that a model plus these tools converges: read, edit,
run `make check`, read the failure, fix. That is the property a big project depends on and
exactly what a small local model is worst at, and there is no `KICHAT_LIVE` test for this
path — `internal/tools` has none. So "goes green" currently means "the parts work", not "the
work gets done".

Done means one gated live test, per the [`live-tests`](../.claude/skills/live-tests/SKILL.md)
skill: a real model asked to make a two-file change and run the tests, asserting convergence
and that both diffs reached the gate — nothing about the model's prose.

### The boundary does not cover hardlinks

`Root.Resolve` resolves symlinks; a hardlink inside the folder pointing at a file outside it
is indistinguishable from a normal file, so a write through one leaves the folder. Every
editor has this, and it needs the person to have made the link. Recorded because CODING.md §2
says the boundary is symlink-safe — true, and it reads as stronger than it is.

## Smaller

- `MaxWriteBytes = 4 MiB` (`internal/workspace/edit.go:42`): a larger file cannot be
  described, so it can be neither diffed nor undone.
- `MaxAgentsBytes = 48 KiB` (`internal/workspace/workspace.go:57`): a dense `AGENTS.md` is
  truncated rather than refused, which the context meter does show.
- `MaxReadLines = 2000` (`workspace.go:45`) against a 500-line default read: a file over 500
  lines takes several rounds, each one a call and — from this release — its own
  condensation.
- The README's Roadmap still lists "Repo-aware coding" as *next* while CODING.md documents it
  as shipped. One of the two is wrong.
