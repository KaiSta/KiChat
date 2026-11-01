---
name: build-platforms
description: Use when building an artifact (Linux, Windows, macOS, .deb, NSIS Setup.exe, .dmg), changing the Makefile's build or packaging recipes, or debugging a build that produced nothing or the wrong artifact.
---

# Building for a platform

The `Makefile` is the entry point; every target resolves `wails`, `npm` and its own
tools from `PATH` with fallbacks.

| Command | Produces |
| --- | --- |
| `make build` | `build/bin/kichat` (or `kichat.app` on macOS) |
| `make windows` | `build/bin/kichat.exe`, cross-built from Linux or macOS |
| `make nsis` | `build/bin/kichat-<arch>-installer.exe` |
| `make darwin` | `build/bin/kichat.app`, universal, **only on a Mac** |
| `make dmg` | `build/dmg/kichat_<version>_<arch>.dmg` |
| `make deb` | `build/deb/kichat_<version>_<arch>.deb` |
| `make install` / `make uninstall` | into `$(PREFIX)` (default `/usr`), Linux only |
| `make vet-arm64` | compiles the linux/arm64 and windows/arm64 halves on any host |

The architecture is a variable rather than a constant: `WINDOWS_ARCH` (default
`amd64`) and `DEB_ARCH` (from `dpkg --print-architecture`, default `amd64`) decide what
is built and what the artifact is called, so the same recipes make the arm64 pair.

The Debian-only targets (`install`, `uninstall`, `deb`, `lint-deb`) say so and exit
on another host, rather than failing part-way through an `install -D` Windows does
not have.

## This machine, measured

| Tool | Here | Consequence |
| --- | --- | --- |
| Go | 1.27.3 | `go.mod` says `go 1.27.0`, which is also the toolchain CI installs, so the two agree |
| Wails CLI | 2.15.0 | fine |
| Node / npm | 22.23.4 / 12.2.1, in `/usr/bin` | on `PATH`; the `~/.local/node/bin` fallback is unused here |
| webkit2gtk | 4.1 (2.52.6) only | the `webkit2_41` tag above |

## What derives from `VERSION`

`make build` links the number into `internal/version` (`VERSION_LDFLAGS`) and rewrites
`wails.json`'s `info` block, which is where the Windows resource and the macOS bundle
version come from. So Settings → About and `kichat --version` cannot disagree with
`VERSION` unless one of these breaks:

- **The linker path is the contract.** `-X kichat/internal/version.Version=…` stops
  matching the moment that package or variable is renamed, and every build then silently
  reports `dev` — a wrong answer, not an error.
- **The Windows version resource is rendered, not linked**, and no `wails` flag overrides it:
  `make version-info` is what keeps it in step, and `productVersion` has to stay numeric
  (see the Windows section).
- **The build date is the commit's date, never the build clock.** A wall-clock stamp would
  break the byte-identical rebuilds `-trimpath` otherwise guarantees.

## The traps that produce a wrong answer rather than an error

- **`webkit2_41`.** Only webkit2gtk-4.1 exists on Ubuntu 24.04+/Mint 22+, so a raw
  `wails build` fails to *link*. `make build` adds the tag; a hand-run build needs
  `-tags webkit2_41`.
- **`node` missing from `PATH`.** npm is a script with `#!/usr/bin/env node`, so a
  path to npm is not enough. It bit on this machine for a while, when Node lived in
  `~/.local/node/bin` and nothing exported it, and the Makefile still falls back to that
  directory when `npm` is not found. The fix is a current Node on `PATH`, not a Makefile
  change: `vite dev` needs ≥ 20.19 / ≥ 22.12 (below that Vite 7 dies with
  `crypto.hash is not a function`), and npm 9 cannot install Tailwind's native binding
  at all, because `libc`-aware optional dependencies arrived in npm 10. The full history,
  including what upgrading the toolchain does and does not change, is in
  [`README.md`](../../../README.md).
- **The frontend is embedded.** `main.go` has `go:embed all:frontend/dist`, so a
  bare `go build ./...` on a fresh clone fails with `pattern all:frontend/dist: no
  matching files found`. That is expected; `make build` builds the frontend first.
- **Wails exits 0 with nothing built.** `wails build -platform darwin/...` on Linux
  prints `WARNING  Crosscompiling to Mac not currently supported.` and returns 0,
  leaving `build/bin` untouched. That is why `make darwin` and `make dmg` check the
  host themselves and exit 2 with a sentence.
- **`makensis` missing is also exit 0.** Wails prints `Cannot create installer:
  makensis not found` and succeeds. `make nsis` checks up front and names the
  package to install on each platform.

## Windows

Cross-builds from Linux or macOS and needs no mingw: Wails writes the PE version
resource, the manifest and the icon in pure Go, and nothing here needs cgo.
Measured: a 22 MB PE32+ **GUI** executable (subsystem 2, no console window) at
`build/bin/kichat.exe`. The platform is `windows/amd64` whatever the host is;
`make windows TARGETARCH=arm64` overrides it.

`make windows WINDOWS_ARCH=arm64` is the same cross-build and needs no ARM runner:
nothing on the Windows path is cgo, and Wails writes the PE resource, the manifest and
the icon in pure Go. Measured on an amd64 Linux host: 21.7 MB, `PE32+ executable (GUI)
Aarch64`. `make nsis WINDOWS_ARCH=arm64` then renders an arm64-only installer from the
same script — Wails passes `-DARG_WAILS_ARM64_BINARY` and the OutFile carries the arch —
so the two installers sit beside each other as `kichat-amd64-installer.exe` and
`kichat-arm64-installer.exe`. The executable is measured; **the arm64 installer's
makensis run is not** (there is no makensis on this machine).

The version resource comes from `build/windows/info.json`, whose values are
`wails.json`'s `info` block — there is **no flag to override it**, so `make
version-info` (a prerequisite of `build` and `windows`) rewrites `productVersion`
and `comments` from `VERSION` and the build prints that it did. Commit the synced
`wails.json` with the release.

`make nsis` wraps the exe in a Setup.exe. Two knobs describe what Windows does,
not cosmetics: `WEBVIEW2=download|embed|browser|error` (passed through as a build
tag, so the *app* acts on a machine with no runtime) and
`INSTALL_SCOPE=machine|user`.

The installer's version is not a fourth place to type it in: the script renders it
from the same `wails.json` info block the exe's resource comes from. That is also
why `version-info` has to keep `productVersion` **numeric** — the script's
`VIProductVersion "${INFO_PRODUCTVERSION}.0"` will not compile a pre-release
string, so `0.2.0~beta1` goes in `comments` and `0.2.0` in the number.

**The installer is not byte-reproducible, and the reason is worth knowing.** NSIS
records each embedded file's timestamp and `wails build -nsis` rewrites
`kichat.exe` immediately before `makensis` runs, so the installer follows the
build clock even though the executable does not. Measured: pinning the exe's bytes
*and* mtime makes two runs identical; touching only the mtime changes them.

`build/windows/installer/wails_tools.nsh` is **generated** by Wails on every
`-nsis` run and is gitignored; `project.nsi` beside it is source, and defines put
there win over the generated ones.

If `makensis` is not installed, it can be taken out of unpacked `nsis` and
`nsis-common` debs with `NSISDIR` pointed at them — no root needed, and that is how
the installer here was measured (makensis 3.09).

Windows needs the WebView2 runtime to open a window at all (present on Windows 11
and on 10 from 21H2). The `-webview2` flag only decides what the installer does
about a machine that lacks it.

## macOS

**Cannot be built on Linux.** Wails packages darwin bundles only for the host it
runs on (its darwin layer is cgo against AppKit and WebKit), and it does not treat
the attempt as a failure — see the exit-0 trap above.

`make darwin` produces the bundle `build/bin/kichat.app` (the name is the project
`name` in `wails.json`; no bare binary is left behind). `Contents/Info.plist` is
`build/darwin/Info.plist` with `wails.json`'s `info` block rendered in, and
`iconfile.icns` is encoded from `build/appicon.png` by a pure-Go ICNS encoder — not
by `iconutil`, which is why no `.icns` is tracked. The default arch is `universal`
(both slices, combined with `lipo`); `DARWIN_ARCH=arm64` builds one.

The recipe reads the version back out of the built plist with
`/usr/libexec/PlistBuddy` and fails if it disagrees with `VERSION` — the same
read-it-back-out the `nsis` target does.

`make dmg` uses `hdiutil` (part of macOS, nothing to install) to write a compressed
read-only image holding `KiChat.app` beside an `/Applications` symlink. The staged
copy is renamed because Finder shows the *directory* name; nothing inside the
bundle changes. `MACOS_APPNAME=kichat` restores Wails' own name if a Mac objects.

Wails signs darwin bundles **ad hoc** (`codesign --sign -`), neither a Developer ID
nor notarization, so a downloaded copy needs
`xattr -dr com.apple.quarantine /Applications/KiChat.app`.

`LSMinimumSystemVersion` is left at Wails' `10.13.0`; the arm64 slice needs 11.0,
so 11 is the effective floor on Apple Silicon. The bundle identifier is still
Wails' placeholder (`com.wails.{{safeBundleID .Name}}`) — the one line to set
before distributing, because Launch Services, the Keychain and notarization all
key off it.

Two macOS specifics in the app itself: `internal/secret` reaches the Keychain
through go-keyring's `/usr/bin/security` CLI, so it needs no code and the probe
does not prompt (the item it asks for does not exist yet); and `main.go` sets
`Mac: &mac.Options{DisableEscapeExitsFullscreen: true}` because the app spends
Escape on stopping a generation and cancelling a chat rename, which AppKit would
otherwise consume to leave fullscreen — leaving neither handler able to fire.

## Debian

`make deb` stages the same `INSTALL_FILES` into `build/deb/…`, then adds
`packaging/debian/copyright`, `THIRD-PARTY-NOTICES.md`, the lintian overrides and a
generated `changelog.gz`, and writes `md5sums`. `Depends` is not guessed: it is the
set of libraries the binary names in its own `DT_NEEDED`, with `t64` alternatives
for the Ubuntu 24.04 rename, plus the libc floor the version symbols ask for.

**arm64 needs an arm64 host.** The Linux build is cgo against webkit2gtk, so an arm64
link needs the arm64 headers and an arm64 compiler; on an x64 host the arch64 assembly
reaches gcc and is rejected — measured: `gcc_arm64.S:65: Error: no such instruction:
ldp x23,x24,[sp,`. So `make deb` on `ubuntu-24.04-arm` is how
`kichat_<version>_arm64.deb` is made; `DEB_ARCH` reads `dpkg --print-architecture`, the
`Depends` names exist for arm64, and the only recipe change needed was the Tailwind
binding, which is now named from the host arch (`NPM_ARCH`) rather than a literal
`x64` — the literal installed an x64 binary on an arm64 host and stamped on it, so the
canary stopped being a canary. `make vet-arm64` compiles the half on a machine that
cannot run it.

The .deb is the artifact with a **byte-identical rebuild**: `-trimpath` and a
commit-date-derived build stamp are what make that true. The NSIS installer is not,
for the timestamp reason above.

## What CI runs, and what ships

CI runs these targets rather than a second copy of the commands:
[`ci.yml`](../../../.github/workflows/ci.yml) has `make check` and `make build` on Linux
(the only platform that links, and the one the `webkit2_41` tag exists for), `make windows`
on Windows and `make darwin` on macOS. A job that restated a recipe would be free to drift
from it, so a change to the build belongs in the Makefile and is then picked up by every
job.

No job uploads an artifact. **A pushed tag is what ships**:
[`release.yml`](../../../.github/workflows/release.yml) runs `make deb`, `make nsis` and
`make dmg` for the tag, checks each artifact reports its version, and attaches the three to
a GitHub Release whose notes are the tag's annotation — [`cut-release`](../cut-release/SKILL.md)
has the consequences, including that a plain `git push` carries local annotated tags.

## Verifying an artifact, never assuming

```sh
file build/bin/kichat.exe                 # PE32+ GUI, subsystem 2
file build/bin/kichat.exe                 # add WINDOWS_ARCH=arm64: PE32+ GUI Aarch64
dpkg-deb --field build/deb/*.deb Architecture   # amd64 or arm64, which the name must match
dpkg-deb -c build/deb/*.deb               # the files, including the notices
dpkg-deb -I build/deb/*.deb               # Version, Depends, Description
/usr/libexec/PlistBuddy -c 'Print CFBundleShortVersionString' build/bin/kichat.app/Contents/Info.plist
makensis -V4 …                            # NSIS's own report of what it embedded
```

`make check` compiles the Windows and macOS halves (`GOOS=windows go vet ./...`,
`GOOS=darwin go vet ./...`) on any host, so a build-tagged file cannot rot here.
Runtime behaviour of those platforms is not claimed anywhere in this repo, because
none was observed.
