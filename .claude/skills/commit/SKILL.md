---
name: commit
description: Use when committing work in this repository — writing the subject and body, choosing an area, staging files, splitting a session into commits, or writing the `build: release x.y.z` commit.
---

# Committing

The subject is `area: description` — lowercase, imperative, and it describes the
behaviour change, not the files. It is short: one line, and it reads as a sentence rather
than as a list of the modules that were touched.

Areas in use: `chat`, `llm`, `ui`, `research`, `docs`, `store`, `build`, `config`,
`api`, `settings`, `secret`, `ingest`, `tools`, `icon`. A new area is fine when the
change belongs nowhere else.

A second effect joins with "and":

```
chat: make compaction work with a reasoning model
research: report a refused search, and keep tests out of the keyring
icon: draw the mark once, derive every surface from it, and make the taskbar one show
```

## Short, in plain language

For the subject and the body both: **plain language, never jargon for its own sake.** No
headings, no checklist, no "as per", no sentence that only exists to sound thorough, and
no body that narrates the diff line by line — that is the diff said twice. If a change
needs a paragraph of justification, the paragraph belongs in the subsystem document and
the commit points at it.

## The body

Prose, not a changelog: the failure or limitation that motivated the change, what was
measured, what changed and why, and which checks stayed green. Numbers go in where they
exist; a body that only lists the files is a stub.

## No attribution lines

The message ends on its last real line. Nothing after the body: no `Generated with …`, no
`Co-Authored-By:`, no AI-assistant or tool trailer, whoever or whatever wrote the change.
The history is the work, not the machinery that typed it.

## One commit is one coherent change

Files are staged explicitly, never `git add -A`. A session that produced three
observable behaviours is three commits, because a regression in one should not hide
inside the others.

Each commit must be a tree that compiles and passes **on its own**. When the changes
share a file, that means staging each commit's version of it through the index while
the working tree keeps the final, tested content — or checking each commit out into a
throwaway worktree and running the tests there — rather than leaving a pile of partial
hunks that only build when applied together.

## The exception: `build: release x.y.z`

Its body is the user-facing bullet list that
[`cut-release`](../cut-release/SKILL.md) step 2 describes, because it is published as
the release note rather than read as a diff summary.

## Always `--no-pager`

`git log`, `git diff` and `git show` open a pager that blocks a non-interactive shell —
it looks hung, and the output lands in a temp file instead of the result.

## Working in a shared checkout

Inspect `git status` and the branch before a consequential git operation. Never
discard, overwrite, stash, stage or commit changes you did not make; when ownership is
ambiguous, leave the work uncommitted and say what remains.
