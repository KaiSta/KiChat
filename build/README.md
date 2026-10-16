# Build Directory

The build directory is used to house all the build files and assets for your application. 

The structure is:

* bin - Output directory
* darwin - macOS specific files
* windows - Windows specific files

## Icon

The mark is authored once as `appicon.svg`, and `make icon` derives the rest from it
(that target is what needs `rsvg-convert` and ImageMagick's `convert`):

- `appicon.png` - 1024 px. `wails build` embeds it as the window and taskbar icon
  (`main.go`), and `icon.ico` is resized from it.
- `windows/icon.ico` - the icon the Windows build and its installer use.

What `make install` copies into a prefix is a different pair, because an icon-theme
lookup follows its own rules:

- `$PREFIX/share/icons/hicolor/scalable/apps/kichat.svg` - one file that answers every
  request a desktop makes, 16 px through 1024 px.
- `$PREFIX/share/pixmaps/kichat.png` - the unthemed base directory, for lookups that
  predate the icon theme.

`packaging/kichat.desktop` names both through the basename they share, `Icon=kichat`.
The comment on `INSTALL_FILES` in the Makefile records why the directory named after the
PNG's real pixel size is deliberately not used.

## Mac

The `darwin` directory holds files specific to Mac builds.
These may be customised and used as part of the build. To return these files to the default state, simply delete them
and
build with `wails build`.

The directory contains the following files:

- `Info.plist` - the main plist file used for Mac builds. It is used when building using `wails build`.
- `Info.dev.plist` - same as the main plist file but used when building using `wails dev`.

`make darwin` and `make dmg` are the targets that consume them, and both need a macOS host:
the Wails packager is called with `runtime.GOOS` and its darwin path is cgo against AppKit
and WebKit, so there is no darwin cross-build the way there is a Windows one. From
`make darwin` Wails writes `build/bin/kichat.app` — the bundle name is the project `name`
in `wails.json`, and `outputfilename` names the executable inside `Contents/MacOS/`.

**There is no tracked `iconfile.icns`, and none is needed.** Wails encodes
`Contents/Resources/iconfile.icns` from `build/appicon.png` itself, with a pure-Go ICNS
encoder (`github.com/jackmordaunt/icns`) rather than `iconutil`, so `make icon` has nothing
darwin-specific to do and the icon cannot drift from the one Windows and Linux get. That is
the tool's behaviour as read from its source, not something measured on a Mac here.

The plist is a **template**: Wails resolves its `{{.Info.*}}` placeholders against the `info`
block of `wails.json` and writes the result into the bundle. Next to macOS, that block is what
the Windows version resource is rendered from, which is why `make version-info` maintains it
from `VERSION` instead of either platform getting a number to type in by hand. `CFBundleVersion`
and `CFBundleShortVersionString` both come from `Info.ProductVersion`, so both carry the
* numeric* half of a Debian pre-release (`0.2.0~beta1` -> `0.2.0`).

**The bundle identifier is still Wails' placeholder.** It reads
`com.wails.{{safeBundleID .Name}}`, which is what `wails init` writes — a name in Wails' own
reverse-DNS namespace rather than this project's. It is deliberately left alone here rather
than guessed at, but it is a thing to set before distributing: Launch Services, the Keychain
and (later) notarization all key off it, and changing it after a release makes the new bundle
a different app to the system. One line in each plist.

`make dmg` then wraps that bundle into `build/dmg/kichat_<version>_<arch>.dmg` with `hdiutil`
(part of macOS), holding `KiChat.app` beside an `/Applications` symlink so dragging across is
the install. The staged copy is renamed from Wails' `kichat.app` because Finder shows the
directory name; the executable and the identity come from `Info.plist`, which is untouched.
Wails ad-hoc signs the bundle (`codesign --sign -`, which is not a Developer ID and not
notarization), so a downloaded copy is quarantined and needs
`xattr -dr com.apple.quarantine /Applications/KiChat.app` before Gatekeeper will open it.

## Windows

The `windows` directory contains the manifest and rc files used when building with `wails build`.
These may be customised for your application. To return these files to the default state, simply delete them and
build with `wails build`.

- `icon.ico` - The icon used for the application. This is used when building using `wails build`. If you wish to
  use a different icon, simply replace this file with your own. If it is missing, a new `icon.ico` file
  will be created using the `appicon.png` file in the build directory.
- `installer/*` - The files used to create the Windows installer. `make nsis` is the target that
  consumes them (`wails build -nsis` underneath, so `make windows` alone does not need NSIS).
- `installer/project.nsi` - Source. The installer's pages, shortcuts and uninstall section; a `!define`
  put at the top of it wins over the generated one beside it.
- `installer/wails_tools.nsh` - **Generated, and not tracked.** Wails rewrites it on every `-nsis` run
  from the template inside the tool, resolving its `{{.Info.*}}` placeholders against `wails.json` in
  place — so a committed copy is a template `makensis` cannot use and every packaging run dirties it.
  `installer/tmp/` (the WebView2 bootstrapper it embeds) is ignored for the same reason.
- `info.json` - Application details used for Windows builds. The data here will be used by the Windows installer,
  as well as the application itself (right click the exe -> properties -> details). This file and
  `wails.exe.manifest` are templates too, but Wails renders them elsewhere rather than overwriting them,
  so they stay as committed — the `{{.Info.*}}` placeholders are not meant to be filled in by hand.
- `wails.exe.manifest` - The main application manifest file.