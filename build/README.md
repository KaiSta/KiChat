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

## Windows

The `windows` directory contains the manifest and rc files used when building with `wails build`.
These may be customised for your application. To return these files to the default state, simply delete them and
build with `wails build`.

- `icon.ico` - The icon used for the application. This is used when building using `wails build`. If you wish to
  use a different icon, simply replace this file with your own. If it is missing, a new `icon.ico` file
  will be created using the `appicon.png` file in the build directory.
- `installer/*` - The files used to create the Windows installer. These are used when building using `wails build`.
- `info.json` - Application details used for Windows builds. The data here will be used by the Windows installer,
  as well as the application itself (right click the exe -> properties -> details)
- `wails.exe.manifest` - The main application manifest file.