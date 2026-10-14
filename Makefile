# KiChat — build, check, run and package.
#
# This file absorbs the environment traps this project actually hit, so nobody
# has to rediscover them (see AGENTS.md): only webkit2gtk-4.1 is installed, so
# every Linux build needs -tags webkit2_41; npm 9 cannot read a package's `libc`
# field, so a clean install silently skips the tailwind oxide native binding and
# vite then dies with "Cannot find native binding"; and `vite dev` cannot run on
# Node 18, which leaves `vite build` as the only frontend build.
#
# Everything above is *Linux's* half of the story, and the file therefore asks the
# toolchain which platform it is building for instead of assuming one:
#
#	make            build the app for this machine, into build/bin/$(APP)
#	make windows    cross-build build/bin/kichat.exe (works from Linux and macOS)
#	make check      gofmt, vet, go test, the TypeScript check, and a
#	                GOOS=windows compile of everything
#	make run        start the built app, detached, logging to a file
#	make deb        build a .deb installer into build/deb/ (Debian hosts only)
#	make help       the full list
#
# The shell stays bash: every recipe here is POSIX sh, and several are multi-line
# pipelines a cmd.exe make would not run. A Windows build is therefore made from Git
# Bash or MSYS2 — see AGENTS.md for what such a machine needs (bash and make, node on
# PATH, and the WebView2 runtime).
SHELL := /bin/bash

APP        := kichat
WEBKIT_TAG := webkit2_41

GO    ?= go
# gofmt ships beside the compiler rather than on the PATH on a machine whose Go was
# unpacked by hand, which is this project's own diagnosis one row up in AGENTS.md.
GOFMT ?= $(shell command -v gofmt 2>/dev/null || echo "$$(dirname "$$(command -v $(GO))" 2>/dev/null)/gofmt")

# --- platform ---------------------------------------------------------------
#
# A Makefile cannot see what it is running on, so it asks the toolchain. TARGETOS is
# what the *build* is for and HOSTOS what it runs on: they differ only for
# `make windows` from Linux, which is a supported cross-build (Wails writes the
# version resource and the manifest in pure Go, so no mingw is needed for it —
# measured, see AGENTS.md).
HOSTOS   := $(shell $(GO) env GOOS)
HOSTARCH := $(shell $(GO) env GOARCH)
TARGETOS   ?= $(HOSTOS)
TARGETARCH ?= $(HOSTARCH)

# The Windows build is x64 by default even on a machine that is not, and that is a
# choice rather than a leftover: `make windows` exists to produce an executable to
# copy onto a Windows machine, nearly all of which are x64, and the architecture
# must not depend on which developer's laptop ran the cross-build. It is also the
# architecture `vet-windows` compiles, so the gate and the artifact agree on every
# host. `make windows TARGETARCH=arm64` overrides it — a command-line assignment
# beats a target's own.
WINDOWS_ARCH ?= amd64

# Windows names its binary; nothing else does.
EXE = $(if $(filter windows,$(TARGETOS)),.exe,)

# The webkit2gtk tag is Linux-only and mandatory there: this machine has
# webkit2gtk-4.1 and nothing else, and a build without the tag fails to link.
# Windows uses WebView2 and macOS WKWebView; a webkit tag on either is a tag no
# file in the build reads.
BUILD_TAGS = $(if $(filter linux,$(TARGETOS)),-tags $(WEBKIT_TAG),)

# The platform is passed explicitly rather than left to the CLI's default, because
# that default is the *host's*: a build told nothing would produce a Linux binary on
# a machine asked for a Windows one, and the difference between the two is the
# platform an archive name, a loader path and a process-stop are all keyed to.
WAILS_PLATFORM = $(TARGETOS)/$(TARGETARCH)

# Debian's hardening flags for the link step, on by default because leaving them
# off is the difference between a package that merely installs and one lintian
# does not warn about. Two flags, two findings: Go's default linux/amd64 build is
# a fixed-address ET_EXEC, so -buildmode=pie is what clears hardening-no-pie, and
# Go's internal linker never emits BIND_NOW, so the external linker plus -z,now is
# what clears hardening-no-bindnow. GOFLAGS is the route for -buildmode because
# the wails CLI has no flag for it (checked with `wails build --help`); -ldflags
# is passed through. Set HARDEN=0 for a quick local build.
#
# comma exists because make splits function arguments on commas, so a literal
# comma inside $(if ...) would truncate the value — that is not hypothetical, it
# silently reduced -extldflags=-Wl,-z,now to -Wl the first time.
# ...and all of it is ELF policy: `-buildmode=pie` is Go's Linux flag and `-z,now` is
# an ELF linker option that mingw and the Mach-O linker would each reject. So the
# hardening follows the *target* and is off for Windows, where the flags are not
# policy but a broken link.
comma := ,
HARDEN         ?= $(if $(filter linux,$(TARGETOS)),1,0)
HARDEN_GOFLAGS  = $(if $(filter 1,$(HARDEN)),-buildmode=pie,)
HARDEN_LDFLAGS  = $(if $(filter 1,$(HARDEN)),-linkmode=external -extldflags=-Wl$(comma)-z$(comma)now,)

# Version identity, linked into internal/version — that package's doc comment
# records why renaming it silently stops these from matching. The date is the
# commit's own date, not the build clock: a wall-clock timestamp would make two
# builds of the same commit differ and waste the -trimpath guarantee above. A
# missing git (no repo, a plain export) degrades to the package's own defaults
# rather than to an empty value.
GIT_SHA    := $(shell git rev-parse --short HEAD 2>/dev/null || true)
GIT_DATE   := $(shell git show -s --format=%cI HEAD 2>/dev/null || true)
VERSION_LDFLAGS = -X kichat/internal/version.Version=$(VERSION) \
	  -X kichat/internal/version.Commit=$(if $(GIT_SHA),$(GIT_SHA),none) \
	  -X kichat/internal/version.Date=$(if $(GIT_DATE),$(GIT_DATE),unknown)
# Resolved rather than assumed: on this machine both wails and node live outside
# the default PATH, which is why a bare `wails build` fails from a clean shell.
WAILS ?= $(shell command -v wails 2>/dev/null || echo "$$($(GO) env GOPATH)/bin/wails")
NPM   ?= $(shell command -v npm 2>/dev/null || echo "$${HOME}/.local/node/bin/npm")

BIN       = build/bin/$(APP)$(EXE)
RUNLOG    = $(if $(filter windows,$(HOSTOS)),$(TEMP)/$(APP).log,/tmp/$(APP).log)
NODE_DEPS := frontend/node_modules
# The stamp for "the frontend dependencies are really usable".
#
# On Linux that is npm's libc-gated optional dependency, which npm 9 refuses to
# install by itself and which is why the recipe below installs it by hand — a canary
# that fails when the binding is missing, which is exactly when `vite build` fails.
# npm 10 resolves the platform's own binding from the lockfile (checked: a clean
# install in an empty directory brings in @tailwindcss/oxide-linux-x64-gnu), and the
# other platforms have no libc field to gate anything — Windows' oxide-win32-x64-msvc
# has no glibc/musl choice to make. So the extra install is Linux's, and elsewhere
# npm's own hidden lockfile is the stamp: it is written by every npm 7+ install and
# missing exactly when the install did not run.
ifeq ($(HOSTOS),linux)
PEER_DEPS := $(NODE_DEPS)/@tailwindcss/oxide-linux-x64-gnu
else
PEER_DEPS := $(NODE_DEPS)/.package-lock.json
endif

# The version is authored once, in the VERSION file at the repo root, and every
# surface is derived from it: the .deb's name, control and changelog, and the
# string linked into the binary that Settings and --version show. The package
# version is $(VERSION) with no Debian revision, which is what a self-built
# package wants. `VERSION=x.y.z make ...` overrides all of it for a one-off.
VERSION   ?= $(shell cat VERSION 2>/dev/null || echo 0.0.0-dev)
DEB_ARCH  ?= $(shell dpkg --print-architecture 2>/dev/null || echo amd64)
PREFIX    ?= /usr
DESTDIR   ?=
DEB_STAGE := build/deb/$(APP)_$(VERSION)_$(DEB_ARCH)
DEB_FILE  := build/deb/$(APP)_$(VERSION)_$(DEB_ARCH).deb

# Every file the package owns, in one place: a plain `make install` and the
# staged copy inside the .deb must never drift apart, so both call this.
#
# The icon is installed where the icon-theme algorithm can reach it, which is
# not the same thing as "in the directory named after its pixel size". Measured
# on this system (GTK 3.24 against hicolor 0.17): a PNG in
# hicolor/1024x1024/apps — its true pixel size, and what lintian's
# icon-size-and-directory-name-mismatch asks for — is invisible to
# Gtk.IconTheme.lookup_icon() at *every* size. 16, 22, 24, 32, 48, 64, 96, 128,
# 256, 512 and 1024 all come back NULL, because the lookup only walks the
# directories hicolor/index.theme declares, and that file lists no 1024x1024
# directory at all (grep: 0 hits) — a 1024 px raster's declared home is
# 512x512@2/apps, not a size no theme defines. The only reason the old layout
# still showed a mark is GTK's legacy /usr/share/pixmaps fallback, and that is
# not a themed directory: Cinnamon's menu applet swaps an app icon it cannot
# find in a theme for application-x-executable (commit e83f7786, "no themed
# icon available for the app"), so an install that races the app-list rebuild
# can leave the menu showing a gear for as long as that button lives, while the
# window list — built later, and through the pixmaps path — shows the real one.
#
# An SVG under hicolor/scalable/apps answers every size from a single file,
# which is what the spec recommends ("an svg icon in
# $prefix/share/icons/hicolor/scalable/apps means most desktops will have one
# icon that works for all sizes") and what mintinstall, Mint's own installer,
# ships. Measured here: with the SVG in scalable/apps, lookups for 16 through
# 1024 all resolve to it, even with the pixmaps copy present, because themes are
# searched before unthemed directories. The pixmaps copy stays for
# implementations that predate the theme lookup — /usr/share/pixmaps is a base
# directory of the same spec rather than a theme.
INSTALL_FILES = install -D -m 0755 $(BIN) $(1)$(PREFIX)/bin/$(APP) && \
	install -D -m 0644 packaging/kichat.desktop $(1)$(PREFIX)/share/applications/$(APP).desktop && \
	install -d -m 0755 $(1)$(PREFIX)/share/man/man1 && \
	gzip -9nc packaging/kichat.1 > $(1)$(PREFIX)/share/man/man1/$(APP).1.gz && \
	chmod 0644 $(1)$(PREFIX)/share/man/man1/$(APP).1.gz && \
	install -D -m 0644 build/appicon.svg $(1)$(PREFIX)/share/icons/hicolor/scalable/apps/$(APP).svg && \
	install -D -m 0644 build/appicon.png $(1)$(PREFIX)/share/pixmaps/$(APP).png

.PHONY: all help deps frontend bindings icon build windows check fmt vet vet-windows test \
        typecheck run install uninstall deb lint-deb version-info clean distclean

all: build

help:
	@echo "KiChat $(VERSION) — make targets ($(HOSTOS)/$(HOSTARCH) host, $(TARGETOS)/$(TARGETARCH) target):"
	@echo "  build       frontend + wails build $(BUILD_TAGS) -> $(BIN)"
	@echo "  windows     the same for windows/$(WINDOWS_ARCH) -> build/bin/$(APP).exe (cross-builds)"
	@echo "  check       gofmt, go vet, go test ./..., tsc --noEmit, GOOS=windows compile"
	@echo "  run         start $(BIN) detached (log: $(RUNLOG))"
	@echo "  deb         build $(DEB_FILE) from a staged tree (Debian hosts)"
	@echo "  HARDEN=0    skip the Debian hardening flags, for a quick local build"
	@echo "  install     install into $(DESTDIR)$(PREFIX) (override PREFIX/DESTDIR)"
	@echo "  uninstall   remove what install put there"
	@echo "  bindings    regenerate frontend/wailsjs after a Go struct change"
	@echo "  icon        regenerate build/appicon.png and icon.ico from build/appicon.svg"
	@echo "  lint-deb    run lintian over the built package"
	@echo "  VERSION=x   build as that version without touching VERSION"
	@echo "  clean       remove build output (distclean also drops node_modules)"

# --- frontend ---------------------------------------------------------------

deps: $(PEER_DEPS)

frontend: deps
	cd frontend && $(NPM) run build

ifeq ($(HOSTOS),linux)
# Two steps on purpose. `npm install` cannot be trusted to bring in the native
# binding on npm 9 (it is a libc-gated optional dependency and npm only learned
# to read that field in v10), and declaring it in package.json or regenerating
# the lockfile was tried and fails. --no-save keeps the lockfile as the source of
# truth; the version is pinned to the one the lockfile already resolves. A package
# that is not this platform's is in the tree but not loadable, and vite then fails
# with "Cannot find native binding" — so the binding is named for the platform the
# install runs on, and only Linux needs naming at all.
$(PEER_DEPS): frontend/package.json frontend/package-lock.json
	cd frontend && $(NPM) install
	cd frontend && $(NPM) i --no-save @tailwindcss/oxide-linux-x64-gnu@4.3.3
else
# npm 10+ reads the lockfile's platform packages itself, so there is no second step to
# take here. The stamp is npm's own hidden lockfile, which every install writes.
$(PEER_DEPS): frontend/package.json frontend/package-lock.json
	cd frontend && $(NPM) install
endif

# Run after changing a Go struct that crosses the bridge, and before tsc: the
# generated models are what the frontend types against.
bindings:
	$(WAILS) generate module
	@chmod 644 frontend/wailsjs/go/main/* frontend/wailsjs/go/models.ts frontend/wailsjs/runtime/* 2>/dev/null || true

# --- icon -------------------------------------------------------------------

# The app icon is authored once as build/appicon.svg, and everything else is
# derived from it, so the taskbar, the browser tab and the .deb never drift
# apart. rsvg-convert (librsvg2-bin) and ImageMagick are required — the latter by
# either of its names: `convert` is ImageMagick 6 and `magick` is 7, and Windows
# installs are 7, which ships `magick` and no `convert` unless the installer was
# asked for the legacy commands.
IM_CONVERT ?= $(shell command -v magick 2>/dev/null || command -v convert 2>/dev/null)
icon:
	@command -v rsvg-convert >/dev/null || { echo "rsvg-convert is required (librsvg2-bin)" >&2; exit 1; }
	@command -v $(IM_CONVERT) >/dev/null || { echo "ImageMagick is required (the 'magick' or 'convert' command)" >&2; exit 1; }
	rsvg-convert -w 1024 -h 1024 build/appicon.svg -o build/appicon.png
	$(IM_CONVERT) build/appicon.png -define icon:auto-resize=256,128,64,48,32,24,16 build/windows/icon.ico
	@echo "regenerated build/appicon.png and build/windows/icon.ico from build/appicon.svg"

# --- build ------------------------------------------------------------------

# The Windows file properties are not linked in, they are *rendered*: Wails fills
# build/windows/info.json and wails.exe.manifest with the `info` block of wails.json
# before the Go build, and there is no flag to override it (checked: `wails build
# --help` has none). So the version Windows shows in a file's Properties would be a
# second place to type it in, and a build that forgot would ship an executable whose
# resource says 1.0.0 — which is what the default is (measured on the first
# cross-built kichat.exe: ProductVersion 1.0.0, LegalCopyright "Copyright.........",
# manifest assemblyIdentity version="1.0.0.0"). This step is what keeps the promise
# that the version is authored once in VERSION.
#
# Two details of the format. Windows version fields are numbers with up to four parts,
# so `0.4.0` is written as it stands and the manifest's fourth part is appended by
# Wails; a Debian pre-release (`0.2.0~beta1`, the form AGENTS.md requires) has no
# numeric spelling, so the field gets its numeric head and the full string goes into
# Comments, which is free text. And the rewrite is a pipeline rather than `sed -i`,
# because BSD sed wants an argument after -i and GNU sed does not.
VERSION_NUMERIC = $(firstword $(subst ~, ,$(VERSION)))
VERSION_INFO_COMMENT = KiChat $(VERSION) - local-first LLM research chat, all rights reserved
version-info:
	@if grep -q '"productVersion": "$(VERSION_NUMERIC)"' wails.json && \
	    grep -q '"comments": "$(VERSION_INFO_COMMENT)"' wails.json; then \
		true; \
	else \
		sed -e 's|"productVersion": "[^"]*"|"productVersion": "$(VERSION_NUMERIC)"|' \
		    -e 's|"comments": "[^"]*"|"comments": "$(VERSION_INFO_COMMENT)"|' \
		    wails.json > wails.json.tmp && mv wails.json.tmp wails.json; \
		echo "wails.json: info block set to $(VERSION) (Windows resource version $(VERSION_NUMERIC)) — commit it with the release"; \
	fi

# -trimpath keeps the build directory out of the shipped binary, which is both
# what a package should contain and what makes two checkouts of the same commit
# build the same bytes. The guaranteed byte-identical rebuild is the .deb's, but
# the flag is passed for every target: a Windows binary whose symbols name the
# builder's home directory is not better for the difference.
build: frontend version-info
	GOFLAGS="$${GOFLAGS} $(HARDEN_GOFLAGS)" \
	$(WAILS) build -platform $(WAILS_PLATFORM) $(BUILD_TAGS) -trimpath -ldflags '$(VERSION_LDFLAGS) $(HARDEN_LDFLAGS)'
	@# wails rewrites the generated bindings and the runtime files with the
	@# executable bit set; the repo tracks them 644, so without this every build
	@# leaves mode churn in `git status` that is not a change.
	@chmod 644 frontend/wailsjs/go/main/* frontend/wailsjs/go/models.ts frontend/wailsjs/runtime/* 2>/dev/null || true
	@ls -l $(BIN)

# A Windows build of the same commit, without a Windows machine. Wails writes the
# executable's version resource, its manifest and its icon in pure Go, and neither
# Wails nor this app needs cgo, so `-platform windows/amd64` links here (measured:
# a 22 MB PE32+ GUI executable). The frontend is the same bundle, and the target is
# the one place the difference between the two platforms is spelled out.
windows: TARGETOS := windows
windows: TARGETARCH := $(WINDOWS_ARCH)
windows: build

# --- checks -----------------------------------------------------------------

check: fmt vet vet-windows test typecheck

fmt:
	@out="$$($(GOFMT) -l .)"; if [ -n "$$out" ]; then echo "gofmt wants to rewrite:"; echo "$$out"; exit 1; fi
	@echo "gofmt: clean"

vet:
	$(GO) vet ./...

# What keeps the Windows half honest, on a machine that has no Windows. `go vet`
# type-checks *and* compiles the test files, which is where a `//go:build windows` file
# and every `runtime.GOOS == "windows"` branch in a test live: a file that stops
# compiling for Windows fails here, on Linux, instead of on a user's machine. It does
# not need the Windows SDK and costs well under a second (measured: 0.86 s, exit 0, on
# this machine).
vet-windows:
	GOOS=windows GOARCH=$(WINDOWS_ARCH) $(GO) vet ./...
	@echo "vet: windows/$(WINDOWS_ARCH) compiles"

test:
	$(GO) test ./... -count=1

typecheck: deps
	cd frontend && $(NPM) run typecheck

# --- run --------------------------------------------------------------------

run: $(BIN)
ifeq ($(HOSTOS),windows)
	@# `start` is the Windows way to launch detached: the app is a GUI executable, so
	@# a shell that waits for it would hold the terminal until the window is closed.
	@cmd //c start "" "$(BIN)"
	@echo "started $(BIN)"
else
	@# Detached on purpose: a plain background launch dies with "Ctrl+C detected.
	@# Shutting down..." whenever a tool call's timeout kills the whole process
	@# group, which looks exactly like a crash. Do not "fix" this by removing setsid.
	setsid nohup $(BIN) >$(RUNLOG) 2>&1 </dev/null &
	@sleep 1; echo "started $(BIN); log: $(RUNLOG)"
endif

$(BIN):
	@echo "$(BIN) is missing — run 'make build' first" >&2; exit 1

# --- install / package ------------------------------------------------------
#
# These targets are Debian's: a .deb, a lintian run, and a filesystem layout
# (/usr/share/applications, the icon theme, a man page) that only Unix has. A machine
# with no package manager is not a machine without a binary — `make build` and
# `make windows` are those two cases — so each of them says what it needs rather than
# failing part-way through an `install -D` or a dpkg-deb that is not there.
define need-linux
	@if [ "$(HOSTOS)" != "linux" ]; then echo "$(1) builds a Debian package and needs a Linux host (this is $(HOSTOS)); use 'make build' for this machine or 'make windows' for a Windows binary" >&2; exit 1; fi
endef

install: $(BIN)
	$(call need-linux,install)
	$(call INSTALL_FILES,$(DESTDIR))

# The 1024x1024 entry is not a path this Makefile installs any more, it is the
# one it installed before the icon moved to scalable/apps: dpkg drops it on
# upgrade, and a hand-run uninstall should not leave a file behind in a
# directory only this package ever used.
uninstall:
	$(call need-linux,uninstall)
	rm -f $(DESTDIR)$(PREFIX)/bin/$(APP) \
	      $(DESTDIR)$(PREFIX)/share/applications/$(APP).desktop \
	      $(DESTDIR)$(PREFIX)/share/man/man1/$(APP).1.gz \
	      $(DESTDIR)$(PREFIX)/share/icons/hicolor/scalable/apps/$(APP).svg \
	      $(DESTDIR)$(PREFIX)/share/icons/hicolor/1024x1024/apps/$(APP).png \
	      $(DESTDIR)$(PREFIX)/share/pixmaps/$(APP).png \
	      $(DESTDIR)$(PREFIX)/share/lintian/overrides/$(APP) \
	      $(DESTDIR)$(PREFIX)/share/doc/$(APP)/copyright \
	      $(DESTDIR)$(PREFIX)/share/doc/$(APP)/changelog.gz

# The .deb is the same install, staged. Depends is not guessed: it is the set of
# libraries the binary names in its own DT_NEEDED, with the t64 alternatives
# because Ubuntu 24.04 renamed several of them, plus the libc floor the version
# symbols ask for (GLIBC_2.34, measured with readelf --version-info).
deb: build
	$(call need-linux,deb)
	rm -rf $(DEB_STAGE)
	$(call INSTALL_FILES,$(CURDIR)/$(DEB_STAGE))
	@mkdir -p $(DEB_STAGE)/DEBIAN
	@install -D -m 0644 packaging/debian/copyright $(DEB_STAGE)$(PREFIX)/share/doc/$(APP)/copyright
	@# Justifications for the info tags this package would otherwise re-report on
	@# every check. Only a .deb ships it: it is a fact about the package, not
	@# something a plain `make install` of a working tree should spray into
	@# /usr/local/share.
	@install -D -m 0644 packaging/debian/kichat.lintian-overrides $(DEB_STAGE)$(PREFIX)/share/lintian/overrides/$(APP)
	@# A native package has to carry a changelog, and its absence is the one error
	@# lintian reports. Generating it means it cannot claim a version the package
	@# does not have; -9n keeps it byte-stable for a given commit.
	@{ echo "$(APP) ($(VERSION)) unstable; urgency=medium"; echo; \
	   echo "  * Built from $(VERSION) at $(shell git rev-parse --short HEAD 2>/dev/null || echo 'an unversioned tree')."; \
	   echo; \
	   echo " -- Kai Stadtmueller <kai.stadtmueller@live.de>  $$(date -R)"; } \
	 | gzip -9n > $(DEB_STAGE)$(PREFIX)/share/doc/$(APP)/changelog.gz
	@# The redirect above cannot set a mode, and the umask decides it otherwise.
	@chmod 0644 $(DEB_STAGE)$(PREFIX)/share/doc/$(APP)/changelog.gz
	@# md5sums is what lets `dpkg -V kichat` check the installed files later.
	@cd $(DEB_STAGE) && find . -type f ! -path './DEBIAN/*' -printf '%P\0' \
	 | sort -z | xargs -0 md5sum > DEBIAN/md5sums
	@sed -e 's|@VERSION@|$(VERSION)|' \
	     -e 's|@ARCH@|$(DEB_ARCH)|' \
	     -e "s|@INSTALLED_SIZE@|$$(du -sk $(DEB_STAGE) | cut -f1)|" \
	     packaging/debian/control.in > $(DEB_STAGE)/DEBIAN/control
	dpkg-deb --root-owner-group --build $(DEB_STAGE) $(DEB_FILE)
	@echo
	@dpkg-deb --info $(DEB_FILE)
	@echo "built $(DEB_FILE) — install with: sudo apt install ./$(DEB_FILE)"

# Two lintian runs on purpose. The first is raw and only reports: a self-built
# package is allowed to carry findings a Debian upload is not, so it does not
# decide the exit status. The second applies this package's own overrides, which
# normally only take effect once the .deb is installed, and it IS a gate — after
# the justified tags are suppressed there should be nothing left, so a new
# finding fails the target instead of scrolling past. --suppress-tags-from-file
# takes bare tag names, one per line (verified: override-style lines come back as
# "Unknown tag"), so the names are extracted from the real overrides file rather
# than kept in a second list that could drift away from it.
lint-deb: $(DEB_FILE)
	$(call need-linux,lint-deb)
	@lintian --display-info $(DEB_FILE) || true
	@echo
	@echo "--- with $(APP)'s own overrides applied (this is the gate):"
	@sed -n 's/^[^#]*:[[:space:]]*\([a-z][a-z0-9-]*\).*/\1/p' packaging/debian/kichat.lintian-overrides \
	 | sort -u > build/deb/suppressed-tags.txt
	lintian --display-info --suppress-tags-from-file build/deb/suppressed-tags.txt $(DEB_FILE)

clean:
	rm -rf build/bin build/deb

distclean: clean
	rm -rf frontend/dist $(NODE_DEPS)
