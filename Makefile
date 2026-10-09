# KiChat — build, check, run and package.
#
# This file absorbs the environment traps this project actually hit, so nobody
# has to rediscover them (see AGENTS.md): only webkit2gtk-4.1 is installed, so
# every build needs -tags webkit2_41; npm 9 cannot read a package's `libc` field,
# so a clean install silently skips the tailwind oxide native binding and vite
# then dies with "Cannot find native binding"; and `vite dev` cannot run on
# Node 18, which leaves `vite build` as the only frontend build.
#
#	make            build the app into build/bin/kichat
#	make check      gofmt, vet, go test, and the TypeScript check
#	make run        start the built app, detached, logging to /tmp
#	make deb        build a .deb installer into build/deb/
#	make help       the full list
#
SHELL := /bin/bash

APP        := kichat
WEBKIT_TAG := webkit2_41

GO    ?= go

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
comma := ,
HARDEN         ?= 1
HARDEN_GOFLAGS  = $(if $(filter 1,$(HARDEN)),-buildmode=pie,)
HARDEN_LDFLAGS  = $(if $(filter 1,$(HARDEN)),-linkmode=external -extldflags=-Wl$(comma)-z$(comma)now,)
# Resolved rather than assumed: on this machine both wails and node live outside
# the default PATH, which is why a bare `wails build` fails from a clean shell.
WAILS ?= $(shell command -v wails 2>/dev/null || echo "$$($(GO) env GOPATH)/bin/wails")
NPM   ?= $(shell command -v npm 2>/dev/null || echo "$${HOME}/.local/node/bin/npm")

BIN       := build/bin/$(APP)
NODE_DEPS := frontend/node_modules
# The libc-gated optional dependency npm 9 refuses to install. It is the canary
# for "the frontend dependencies are really usable", so it doubles as the stamp.
PEER_DEPS := $(NODE_DEPS)/@tailwindcss/oxide-linux-x64-gnu

# Packaging. VERSION is the only value to bump for a release; the package version
# is $(VERSION) with no Debian revision, which is what a self-built package wants.
VERSION   ?= 0.1.0
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

.PHONY: all help deps frontend bindings icon build check fmt vet test typecheck run \
        install uninstall deb lint-deb clean distclean

all: build

help:
	@echo "KiChat $(VERSION) — make targets:"
	@echo "  build       frontend + wails build -tags $(WEBKIT_TAG) -> $(BIN)"
	@echo "  check       gofmt, go vet, go test ./..., tsc --noEmit"
	@echo "  run         start $(BIN) detached (log: /tmp/$(APP).log)"
	@echo "  deb         build $(DEB_FILE) from a staged tree"
	@echo "  HARDEN=0    skip the Debian hardening flags, for a quick local build"
	@echo "  install     install into $(DESTDIR)$(PREFIX) (override PREFIX/DESTDIR)"
	@echo "  uninstall   remove what install put there"
	@echo "  bindings    regenerate frontend/wailsjs after a Go struct change"
	@echo "  icon        regenerate build/appicon.png and icon.ico from build/appicon.svg"
	@echo "  lint-deb    run lintian over the built package"
	@echo "  clean       remove build output (distclean also drops node_modules)"

# --- frontend ---------------------------------------------------------------

deps: $(PEER_DEPS)

frontend: deps
	cd frontend && $(NPM) run build

# Two steps on purpose. `npm install` cannot be trusted to bring in the native
# binding on npm 9 (it is a libc-gated optional dependency and npm only learned
# to read that field in v10), and declaring it in package.json or regenerating
# the lockfile was tried and fails. --no-save keeps the lockfile as the source of
# truth; the version is pinned to the one the lockfile already resolves.
$(PEER_DEPS): frontend/package.json frontend/package-lock.json
	cd frontend && $(NPM) install
	cd frontend && $(NPM) i --no-save @tailwindcss/oxide-linux-x64-gnu@4.3.3

# Run after changing a Go struct that crosses the bridge, and before tsc: the
# generated models are what the frontend types against.
bindings:
	$(WAILS) generate module
	@chmod 644 frontend/wailsjs/go/main/* frontend/wailsjs/go/models.ts frontend/wailsjs/runtime/* 2>/dev/null || true

# --- icon -------------------------------------------------------------------

# The app icon is authored once as build/appicon.svg, and everything else is
# derived from it, so the taskbar, the browser tab and the .deb never drift
# apart. rsvg-convert (librsvg2-bin) and ImageMagick (convert) are required.
icon:
	@command -v rsvg-convert >/dev/null || { echo "rsvg-convert is required (librsvg2-bin)" >&2; exit 1; }
	@command -v convert >/dev/null || { echo "convert is required (imagemagick)" >&2; exit 1; }
	rsvg-convert -w 1024 -h 1024 build/appicon.svg -o build/appicon.png
	convert build/appicon.png -define icon:auto-resize=256,128,64,48,32,24,16 build/windows/icon.ico
	@echo "regenerated build/appicon.png and build/windows/icon.ico from build/appicon.svg"

# --- build ------------------------------------------------------------------

# -trimpath keeps the build directory out of the shipped binary, which is both
# what a package should contain and what makes two checkouts of the same commit
# build the same bytes.
build: frontend
	GOFLAGS="$${GOFLAGS} $(HARDEN_GOFLAGS)" \
	$(WAILS) build -tags $(WEBKIT_TAG) -trimpath $(if $(HARDEN_LDFLAGS),-ldflags '$(HARDEN_LDFLAGS)')
	@# wails rewrites the generated bindings and the runtime files with the
	@# executable bit set; the repo tracks them 644, so without this every build
	@# leaves mode churn in `git status` that is not a change.
	@chmod 644 frontend/wailsjs/go/main/* frontend/wailsjs/go/models.ts frontend/wailsjs/runtime/* 2>/dev/null || true
	@ls -l $(BIN)

# --- checks -----------------------------------------------------------------

check: fmt vet test typecheck

fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt wants to rewrite:"; echo "$$out"; exit 1; fi
	@echo "gofmt: clean"

vet:
	$(GO) vet ./...

test:
	$(GO) test ./... -count=1

typecheck: deps
	cd frontend && $(NPM) run typecheck

# --- run --------------------------------------------------------------------

run: $(BIN)
	@# Detached on purpose: a plain background launch dies with "Ctrl+C detected.
	@# Shutting down..." whenever a tool call's timeout kills the whole process
	@# group, which looks exactly like a crash. Do not "fix" this by removing setsid.
	setsid nohup $(BIN) >/tmp/$(APP).log 2>&1 </dev/null &
	@sleep 1; echo "started $(BIN); log: /tmp/$(APP).log"

$(BIN):
	@echo "$(BIN) is missing — run 'make build' first" >&2; exit 1

# --- install / package ------------------------------------------------------

install: $(BIN)
	$(call INSTALL_FILES,$(DESTDIR))

# The 1024x1024 entry is not a path this Makefile installs any more, it is the
# one it installed before the icon moved to scalable/apps: dpkg drops it on
# upgrade, and a hand-run uninstall should not leave a file behind in a
# directory only this package ever used.
uninstall:
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
