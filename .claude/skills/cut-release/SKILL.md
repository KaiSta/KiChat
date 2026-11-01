---
name: cut-release
description: Use when asked to cut, make, ship or publish a release ("cut 0.5.4", "make the release 0.5.3", "release x.y.z"), or when bumping VERSION and tagging.
---

# Cutting a release

A request like "cut 0.4.2" means exactly this, in this order. The number in the
request is the number everywhere.

## 1. Bump the one file

```sh
printf 'x.y.z\n' > VERSION
```

One line and a newline. A pre-release is written with `~`, not a dash:
`0.2.0~beta1`, because `~` sorts *before* the release while `0.2.0-beta.1` sorts
after it, and `apt upgrade` would happily move a pre-release onto a
newer-looking final.

## 2. One commit, `build: release x.y.z`, carrying `VERSION` and the `wails.json`
info block it rewrites — nothing else

### The message *is* the release note

Its body is not a summary of the diff — it becomes the tag's annotation, and therefore
the GitHub release page, so it is the only thing someone sees before deciding to
install this version. Write it for that reader, not for the reader of the commit.

The rules are the ones in [`commit`](../commit/SKILL.md), tightened for a page that has
no diff beside it:

- **Short, and a bullet list.** A handful of bullets, one line each, in the order a
  person cares about: what the version brings first, smaller fixes after. If an item
  needs a paragraph, it is either two bullets or it belongs in the documentation.
- **Plain language, about what the user gets**, not about how it was built. "Pictures
  can now be enlarged 4×" — not "added an upscaler provider". No file names, package
  names, measurements or internal reasoning.
- **No attribution lines.** The note is published, so nothing after the last bullet: no
  `Generated with …`, no `Co-Authored-By:`, no thanks to a model or a tool. End on the
  last real bullet.
- **A release with nothing a user can observe says so in one bullet.** "Maintenance
  only: no visible change." An empty note reads as a build that failed.
- `git log <previous bump>..HEAD` is the source to read, not text to copy: each commit
  says what changed in the code, and the note says what that means for the person
  running the app.

```
- Coding mode: a folder can now be read, edited and run with confirmation, and a long
  command can be stopped without ending the reply.
- Answers are cheaper on a hosted backend: the prompt is laid out so its beginning can be
  reused, which cuts what a long coding reply costs.
- Pictures can be enlarged, 2× or 4×.
```

What does not go in: "refactored the provider registry", "reduced prompt tokens by 30%
across twelve files", "see the commits". Those describe the work, and the note describes
the version.

Not `release: bump version to …` either: 0.4.0 is the outlier that did that, and the
subject is `build: release x.y.z`.

## 3. Verify before publishing anything

```sh
make check
make build
build/bin/kichat --version          # x.y.z (<that commit's sha>)
make deb                            # build/deb/kichat_x.y.z_<arch>.deb
dpkg-deb -I build/deb/*.deb         # Version: x.y.z
dpkg-deb -c build/deb/*.deb | grep changelog
```

`make lint-deb` needs `lintian`, which may not be installed — say so rather than
skipping it silently.

## 4. Tag that commit, annotated, with the same notes as its message

```sh
git tag -a x.y.z -F <(git log -1 --format=%B HEAD) HEAD
```

The trailing argument is the **object to tag**, not the name to create. Repeating
the version there fails with `fatal: Failed to resolve 'x.y.z' as a valid ref` —
the tag does not exist yet when the command runs; `HEAD` is step 2's bump commit,
made moments before.

Annotated, because a lightweight tag carries no notes; on the bump commit,
because a build from the tag otherwise reports the old number.

```sh
git describe --tags --exact-match HEAD   # is the tag where you think it is
```

0.4.0 was bumped without a tag and only got one after the fact, which is the
mistake this step exists for.

## 5. The tag stays local until asked for, and pushing it publishes

`git push origin x.y.z` is a separate request, like any other push — nothing here
pushes a tag on its own. It is also the act that *ships*: `.github/workflows/release.yml`
triggers on a tag whose name starts with a digit, and it does four things.

1. **Verify.** The tag must be annotated, equal to `VERSION`, and reachable from
   `master`. Any of the three failing ends the run before a build starts, so a
   mistyped tag or an unbumped `VERSION` publishes nothing.
2. **Build, on the host each package needs.** `make deb` on Ubuntu 24.04,
   `make nsis` on Windows after installing NSIS, `make dmg` on macOS.
3. **Check each artifact against the tag**, because a build that succeeded is not
   a package that says the right thing: `dpkg-deb --field Version`, the Setup.exe's
   `ProductVersion` resource, and `CFBundleShortVersionString` read out of the
   bundle *inside* the mounted image. A missing or mismatched file fails the run.
4. **Publish** `kichat_<version>_<arch>.deb`, `kichat-<arch>-installer.exe` and
   `kichat_<version>_universal.dmg` on a GitHub Release titled `KiChat <version>`,
   whose notes are the tag's annotation — the same text as the bump commit's body,
   so the page and the tag cannot disagree.

The three builds are separate jobs and the release job needs all of them: a failed
build means no release page at all, never a partial one. Re-running the workflow
for the same tag (`workflow_dispatch` with the tag as its input, or Re-run jobs)
uploads over the existing release and edits its notes; the tag is never moved.
Deleting a release is `gh release delete <version>` — the tag stays.

What the repo's own config changes is what an asked-for push carries:
`.git/config` sets `push.followTags = true`, so a plain `git push origin master`
also sends every *annotated* tag that is missing from the remote and reachable
from the branch. Measured against a scratch remote: a plain push carried 0.3.0
through 0.5.2 and none of the lightweight tags, and after deleting 0.5.2 from that
remote, pushing the already-current branch returned it — `* [new tag] 0.5.2 ->
0.5.2`, with no branch line at all. The setting is a repair for a tag an earlier
push left behind, not only a habit.

**Lightweight tags are never carried.** `0.1.0`, `0.2.0`, `0.2.1` and `v0.4.0` are
lightweight, so those need `git push origin <tag>` said out loud. `v0.4.0` exists
only on the remote while this checkout's `0.4.0` is a different, annotated tag on
the same commit — a plain push carries `0.4.0` and can never carry `v0.4.0`.

The setting lives in `.git/config`, which is not cloned: a fresh checkout wants
`git config push.followTags true`, and `git config --get push.followTags` is the
check when a tag seems to have gone missing.

**That makes a plain push a publishing act.** Since a pushed tag builds and
publishes, `git push origin master` with an already-cut tag still local also
releases it. That is the intent — the tag was cut to be released — but it means
step 3's checks belong before the tag is created, not before it is pushed: by the
time `git push` runs there is nothing left to catch.

**A published tag is never moved or deleted.** A mistake is the next patch number.
A tag that went missing is *repaired* rather than rewritten: an annotated tag on
the bump commit, whose message says it is late and why and summarises the changes
since the previous tag, closes the hole in `git describe` without moving anything.
Only for a version that really was released, and never an invented note.

## Done when

- `VERSION`, `wails.json`'s `comments`/`productVersion`, the binary's `--version`
  and the `.deb`'s control all agree.
- The bump commit carries `VERSION` and nothing else, and its body is the short,
  user-facing bullet list that the release page will show.
- The annotated tag sits on that commit (`git describe --tags --exact-match HEAD`).
- Nothing was pushed; the tag is local until the user asks.
- Once it is pushed: the release page carries exactly five assets — a `.deb` and a
  Setup.exe for amd64 and for arm64, and the universal `.dmg` — and each one reports
  the tag's version in the form its platform shows it (`.deb` Version, installer
  ProductVersion, bundle CFBundleShortVersionString).
