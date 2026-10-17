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

Its body is the release note: what changed **since the previous bump commit**,
written as prose from `git log <previous bump>..HEAD` with the measured numbers in
it, not a list of files. Open with the standing paragraph the 0.3.x releases and
0.4.1 use — `VERSION` is the one authored number, so a release is this line and
nothing else, and the tag has to sit on the commit that carries it — then the
changes.

Not `release: bump version to …`: 0.4.0 is the outlier that did that.

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

## 5. The tag stays local until asked for

`git push origin x.y.z` is a separate request, like any other push — nothing here
pushes a tag on its own.

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

**A published tag is never moved or deleted.** A mistake is the next patch number.
A tag that went missing is *repaired* rather than rewritten: an annotated tag on
the bump commit, whose message says it is late and why and summarises the changes
since the previous tag, closes the hole in `git describe` without moving anything.
Only for a version that really was released, and never an invented note.

## Done when

- `VERSION`, `wails.json`'s `comments`/`productVersion`, the binary's `--version`
  and the `.deb`'s control all agree.
- The bump commit carries `VERSION` and nothing else, and its body reads as prose.
- The annotated tag sits on that commit (`git describe --tags --exact-match HEAD`).
- Nothing was pushed; the tag is local until the user asks.
