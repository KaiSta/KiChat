# Coding mode: what is left before it is ready

A readiness review of coding mode against one question: would I use this on a project the size
of KiChat? The mechanism is [CODING.md](CODING.md) and is not repeated here — this is the work
list, and it separates what was checked from what was not.

Written against `feat/coding` at `c093673`: 30 commits and +16 672/−599 lines over the released
`0.6.2`, not in any tag, with this review's own two changes (the raised tool budgets and the git
block) still uncommitted. Re-check the defaults and line numbers before trusting any of it; a
TODO that describes a moving branch is wrong by default.

## What is already held

Short, because CODING.md carries the argument: the folder boundary is symlink-safe with no
fallback, every change goes describe → ask → diff, the terminal runs one command with stdin
closed (120 s default, 600 s ceiling) and kills the process tree, and every write or edit keeps a
pre-image in the encrypted database that refuses to revert over somebody else's edit. About 122
tests cover that path — 56 in `internal/workspace`, 55 across the project tools
(`project_test.go`, `command_test.go`, `gitguard_test.go`, `change_test.go`, `snapshot_test.go`,
`reads_test.go`), 10 in the project API (`create_project_test.go`, `project_files_test.go`), and
one chat test for the git block; the changes panel, the terminal hand-off and `/clear` add 16 at
the root. `command_test.go` runs real commands, including a grandchild that writes after the
shell is killed.

Three things this list used to open with are now held, and it matters what they do and do not
reach:

- **A project replies on its own figures: 60 rounds and 120 000 tool-result tokens**, against
  a chat's 24 and 60 000 (`config.DefaultCodingToolIterations`, `DefaultCodingToolTokenBudget`,
  `DefaultToolIterations`, `DefaultToolTokenBudget`; `MaxToolIterations` is the ceiling both
  round fields are repaired against). Reading four files, editing two and running `make check`
  fits. They are separate figures because sharing them was the bug: a chat set to 10 rounds
  cut a project at 10, and the shared token budget was two whole reads of a file the size of
  `app.go` (24 429 tokens each) with most of the project's rounds still to come
  (`chat.maxToolRounds` and `chat.maxToolResultTokens` pick by mode). Both are still cut to
  the room the next request actually has, so a small local window is unaffected; and a
  settings row that already stores 6 — or an absent token field — is repaired the same way,
  so a fresh install and an upgraded one both get the project's figures.
- **The working tree travels with the prompt.** A project whose folder is a git repository gets
  `<project_git>` on every turn: branch (or detached), ahead/behind, and every pending path with
  the staged half, the kind of change and its line counts — 40 entries and 4 KiB at most, charged
  as `SegmentGit` ("Git-Status") in the meter. It is silent for a folder that is not a repository
  and for a machine without git; the probe never writes and never runs what the repository
  configures (CODING.md §8).
- **A line that would discard uncommitted work is refused before it is asked about.** `git
  checkout -- <paths>`, `git checkout .`, a forced checkout, `git restore`, `git reset --hard`
  and `git clean -f` are measured against the pending paths in their own pathspecs, and refused
  with `tools.destructiveGit` when there is work to lose; on a clean tree the same line runs.
  It is not undoable by design — the undo record belongs to a file and keeps the chat's first
  pre-image, so it cannot represent "the working tree just before this command" — so the promise
  is that the work is not lost rather than that the loss can be taken back (CODING.md §4).
- **A large file can be mapped instead of read.** `read_file` with `"outline": true` lists the
  declarations it recognises with their line numbers and no bodies — `app.go` measures 24 429
  tokens whole against 1 597 as an outline (1 939 lines, 79 declarations), and the numbers are the
  ones a read prints, so the second call is a window. It is a line scanner rather than a parser, says so when it finds nothing, and is
  exempt from the condensation (CODING.md §5, COMPACT.md §17.6). Families come from the
  extension, `Makefile`/`Dockerfile`/`go.mod` from the name, and any other config-shaped file from
  its keys with the values cut — which is the rule that also keeps a `.env` out of the prompt.
  What it does not do is read the text: the round after it is still a real read.

## Blocks a big project

In the order the friction shows up on a multi-file change.

### 1. One file per change, one question each

A 20-file rename is 20 cards. `AutoApprove.Files` (`internal/config/config.go:121`, `:123`) is the
switch to turn on first: a file change leaves a diff in the transcript and a row in the changes
panel, a command leaves neither. Whether a project-level "apply these N edits" shape is worth
having is a product decision, not a bug — the gate is per call by design.

### 2. Deletions and moves cannot be taken back

`rm` and `mv` run through `run_command`: gated, but no diff, no pre-image, no row in the changes
panel. Every other change in this mode can be undone, and git's own discarding commands are now
refused when their paths are dirty (above) — the shell's are not. Done means a snapshot row
before a command that removes or renames a tracked file — the same `file_snapshots` machinery and
the same revert path, which already knows how to refuse when the file changed underneath it.

### 3. Literal search only

`search_files` matches a literal string, `.gitignore`-aware, with hits clipped at 300 bytes
(`MaxHitLine`, `internal/workspace/workspace.go:538`). Regex means `grep` through the terminal:
correct, but a round and a card.

## Not verified

### The loop has never been run with a real model

Every test asserts a tool. None asserts that a model plus these tools converges: read, edit, run
`make check`, read the failure, fix. That is the property a big project depends on and exactly
what a small local model is worst at, and there is no `KICHAT_LIVE` test for this path —
`internal/tools` has none. So "goes green" currently means "the parts work", not "the work gets
done", and nobody has watched a model use the new git block, or meet the new
destructive-command refusal and take the hint.

Done means one gated live test, per the [`live-tests`](../.claude/skills/live-tests/SKILL.md)
skill: a real model asked to make a two-file change and run the tests, asserting convergence and
that both diffs reached the gate — nothing about the model's prose.

### The boundary does not cover hardlinks

`Root.Resolve` resolves symlinks (`workspace.go:170`); a hardlink inside the folder pointing at a
file outside it is indistinguishable from a normal file, so a write through one leaves the folder.
Every editor has this, and it needs the person to have made the link. Recorded because CODING.md
§2 says the boundary is symlink-safe — true, and it reads as stronger than it is.

## Smaller

- `MaxWriteBytes = 4 MiB` (`internal/workspace/edit.go:42`): a larger file cannot be described, so
  it can be neither diffed nor undone.
- `MaxAgentsBytes = 48 KiB` (`internal/workspace/workspace.go:57`): a dense `AGENTS.md` is
  truncated rather than refused, which the context meter does show.
- `MaxReadLines = 2000` (`workspace.go:45`) against a 500-line default read (`workspace.go:364`):
  a file over 500 lines takes several rounds, each one a call and — from this release — its own
  condensation. The outline makes the *first* of those a fraction of a file, but reading the text
  an edit is built from is still however many rounds its length needs.
- The git probe runs on **every tool round**, not once per turn: about 11 ms per git call on this
  checkout (measured, 20 runs; two calls when the tree is dirty). A project folder that is not a
  repository pays a failing `git rev-parse` per round. A per-turn or short-TTL cache is the
  obvious fix if a large repository ever makes it visible.
- The README's Roadmap still lists "Repo-aware coding" as *next* (`README.md:799`) while
  CODING.md documents it as shipped. One of the two is wrong.
