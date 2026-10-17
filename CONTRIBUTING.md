# Contributing to KiChat

KiChat is a single-author project that is happy to take contributions:
bug reports, translations, backend adapters, documentation fixes, and the
features on the roadmap.

## Before your first pull request: the CLA

Every contribution is covered by [CLA.md](CLA.md), which grants the maintainer
the rights needed to keep one licensing position as the project grows while
leaving your work available to everyone under Apache-2.0. **You keep the
copyright.**

In the pull request that carries your first contribution, comment exactly:

> I have read the KiChat Contributor License Agreement (CLA.md) and I agree to
> it on my own behalf.

A maintainer records it in [CLA-signatures.md](CLA-signatures.md). If you
contribute as part of your job, ask for the corporate form first — your
employer signs once and lists the accounts it authorises. Write to
kai.stadtmueller@live.de for that, or open an issue if you prefer.

## Getting set up

```sh
make build     # build for this machine
make run       # build and launch
make check     # gofmt, vet (linux/windows/darwin), tests, tsc
```

`make` resolves `wails` and `npm` itself. The one trap worth knowing before
your first build: if the machine only has webkit2gtk-4.1 (Ubuntu 24.04+, Mint
22+), the `webkit2_41` build tag is required — `make build` adds it, a raw
`wails build` needs `-tags webkit2_41`. [README.md](README.md) has the full
platform notes and [AGENTS.md](AGENTS.md) the environment traps measured on
this project.

There is a browser harness for UI work, with no backend and no model:

```sh
make frontend
go run ./tools/uiharness      # http://127.0.0.1:8791/
```

## What a good pull request looks like

- **One coherent change.** The commit subject is `area: description` —
  lowercase, imperative, describing the behaviour change rather than the files
  (`chat: make compaction work with a reasoning model`). The body says what was
  wrong, what you measured, what changed, and which checks stayed green.
- **Tests pin behaviour, not implementation.** A test should fail if the user
  or the model would notice a difference. Live tests that touch metered
  services are gated by `KICHAT_LIVE=1` and must never run in the default set.
- **Never commit a key.** Pass credentials in the environment; the app stores
  them through `internal/secret`, and tests use `secret.NewIsolated` so the
  suite never touches the real keyring or `~/.config/kichat`.
- **User-facing strings go through [`frontend/src/lib/i18n.ts`](frontend/src/lib/i18n.ts)**
  in all three languages (English, German, Chinese). A hardcoded label is a
  bug.
- **A vendor fact belongs in its system document** — [`GROQ.md`](GROQ.md),
  [`NVIDIA.md`](NVIDIA.md), [`MISTRAL.md`](MISTRAL.md),
  [`DEEPSEEK.md`](DEEPSEEK.md), [`LLAMACPP.md`](LLAMACPP.md),
  [`IMAGES.md`](IMAGES.md), [`RESEARCH.md`](RESEARCH.md) — not duplicated in
  the README.
- **After changing a Go struct that crosses the bridge**, build before
  type-checking the frontend: `wails build` regenerates
  `frontend/wailsjs/go/models.ts`.
- **After changing dependencies**, regenerate the notices:
  `go run ./tools/notices` rewrites
  [`THIRD-PARTY-NOTICES.md`](THIRD-PARTY-NOTICES.md).

## Licensing of what you send

By contributing you agree that the project may distribute your contribution
under Apache-2.0 and under the terms described in [CLA.md](CLA.md). Only send
work you have the right to send; if a file carries someone else's licence, say
so in the pull request.

## Reporting a bug

Include the version (`kichat --version` or Settings → About), the platform, the
backend and model, and what you expected instead. A log is worth more than a
description: the app writes nothing anywhere you would have to hunt for — the
interesting output is what the terminal showed when it was started.

Security issues should go to kai.stadtmueller@live.de rather than into a public
issue.
