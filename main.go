package main

import (
	"embed"
	"fmt"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"

	"kichat/internal/version"
)

//go:embed all:frontend/dist
var assets embed.FS

// Window and taskbar icon, embedded so it shows even when the app is not
// installed via the .deb (where the desktop would otherwise have to find the
// mark through the icon theme). The package ships the mark in the two forms a
// theme lookup can reach — the SVG in hicolor/scalable/apps and this PNG in
// /usr/share/pixmaps; INSTALL_FILES in the Makefile records why a size-named
// hicolor directory is not one of them.
//
//go:embed build/appicon.png
var appIcon []byte

func main() {
	// --version prints what Settings shows and exits, so a script or a bug report
	// can ask the binary what it is without opening a window. Only the exact
	// flags count: a stray argument must not hijack an ordinary launch.
	if wantsVersion(os.Args[1:]) {
		fmt.Println(version.String())
		return
	}

	// Create an instance of the app structure
	app := NewApp()

	// Create application with options
	err := wails.Run(&options.App{
		Title:     "KiChat",
		Width:     1440,
		Height:    900,
		MinWidth:  900,
		MinHeight: 600,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		// Wails disables the webview's context menu in production builds unless
		// this is set (a dev build gets it regardless: on Linux the menu is only
		// disabled when !debug && !EnableDefaultContextMenu). Without it a
		// selected reply has no Copy in its right-click menu and the message box
		// has no Cut/Copy/Paste — hostile for a chat app whose entire output is
		// text people copy. The menu is WebKitGTK's own, so it is themed by the
		// desktop and needs no frontend code to keep in sync.
		EnableDefaultContextMenu: true,
		// Matches --bg of the dark theme, so the window does not flash white while
		// the webview paints its first frame.
		BackgroundColour: &options.RGBA{R: 13, G: 16, B: 23, A: 1},
		// SetWindowIcon drives the taskbar icon on Linux. WebviewGpuPolicy is
		// pinned to Never: a non-nil Linux option otherwise falls back to the
		// zero value (OnDemand) and would change GPU behaviour.
		Linux: &linux.Options{
			Icon:             appIcon,
			WebviewGpuPolicy: linux.WebviewGpuPolicyNever,
		},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}

// wantsVersion reports whether the command line asked for the version.
func wantsVersion(args []string) bool {
	for _, arg := range args {
		if arg == "--version" || arg == "-version" {
			return true
		}
	}
	return false
}
