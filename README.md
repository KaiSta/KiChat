# KiChat

A local-first research chat desktop app. A Wails v2 Go backend with a React 19 /
TypeScript frontend, driving **LM Studio** (`:1234`) or **Ollama** (`:11434`) so long
research conversations run against local models instead of a hosted service.

Features: automatic and manual context compaction, a live context-window meter built
from *measured* prompt tokens, Brave Search with a DuckDuckGo fallback, page scraping
with an SSRF guard, file/PDF/office attachments with BM25 retrieval over the chunks,
an editable system prompt (German research default), per-message tokens/s, native tool
calling with a text-protocol fallback, and in-chat model switching with a Stop button.

## Requirements

- **Go** 1.26+ (see `go.mod`)
- **Wails CLI v2** — `go install github.com/wailsapp/wails/v2/cmd/wails@latest`
- **A chat backend** — LM Studio on `:1234` or Ollama on `:11434`
- **Linux**: GTK3 and WebKit2GTK. Systems that ship only `webkit2gtk-4.1`
  (Ubuntu 24.04+, Linux Mint 22+) must pass the `webkit2_41` build tag.
- **Node 20.19+ or 22.12+** (so npm 10+). Node 18 can build this project, but only once
  the native Tailwind binding is already installed — see *Node and npm versions* below.

## Build

```sh
wails build -tags webkit2_41   # when only webkit2gtk-4.1 is available
wails build                    # when webkit2gtk-4.0 is available
```

`wails build` installs the frontend dependencies, compiles `frontend/dist`, and only
then compiles Go, because `main.go` embeds that directory. A bare `go build ./...` on a
fresh clone therefore fails with `pattern all:frontend/dist: no matching files found`
until the frontend has been built once — that is expected, not a packaging error.

## Development

```sh
wails dev -tags webkit2_41
```

Requires Node 20.19+/22.12+ (see below). Wails additionally serves the app on
<http://localhost:34115> so the bound Go methods can be called from a browser.

## Node and npm versions

On **Node 18 / npm 9**, a clean `npm install` leaves Vite unable to start:

```
Error: Cannot find native binding. npm has a bug related to optional
dependencies (https://github.com/npm/cli/issues/4828).
```

Two separate things combine to cause this:

1. `@tailwindcss/oxide@4.3.3` declares `engines.node: ">= 20"`, so npm 9 reports
   `EBADENGINE` and warns rather than refusing.
2. Its platform packages declare `libc: "glibc"`. **npm 9 cannot evaluate the `libc`
   field**, so it classifies the perfectly matching `@tailwindcss/oxide-linux-x64-gnu`
   as an `UNMET OPTIONAL DEPENDENCY` and silently skips the binary. `libc`-aware
   optional-dependency resolution only arrived in npm 10.

The correct fix is Node 20.19+/22.12+, which also unblocks `wails dev`. If you are
stuck on Node 18, install the binding explicitly — npm honours a package requested by
name even when it cannot match it as an optional dependency:

```sh
npm i --no-save @tailwindcss/oxide-linux-x64-gnu@4.3.3   # run inside frontend/
```

Declaring that package in `package.json` does **not** help on npm 9: the optional
filter runs before the declaration is consulted, which was verified by trying it. Note
also that `--no-save` keeps it out of `package-lock.json`, so the command has to be
repeated after any change to the lockfile, and a machine that only runs
`npm install` will not pick it up.

## Where data lives

| What | Location |
| --- | --- |
| Chats, messages, documents, FTS index, settings | `~/.config/kichat/kichat.db` |
| API keys (e.g. Brave Search) | `~/.config/kichat/secrets` |

Keys are written outside the working tree and are never read from the repository.

## Tests

```sh
go test ./...                          # hermetic unit tests
KICHAT_LIVE=1 go test ./internal/...   # additionally hits a real LM Studio
```

The live tests assert that a streamed response reports a non-zero `promptTokens`. That
value is what the context meter displays as *measured*; if it ever regresses to zero the
meter silently falls back to an estimate, so the assertion is the guard for it.

## Notes

- Token accounting is backend-authoritative: LM Studio is asked for streamed usage via
  `stream_options.include_usage`. The heuristic estimator is only used to predict a
  request *before* it is sent, and the UI badges those numbers as `estimated`.
- Compaction never deletes messages: originals stay in the database and remain viewable.
- Research requires a Brave Search API key for the primary path; without one it falls
  back to DuckDuckGo, which needs no key.
