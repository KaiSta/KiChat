// Command uiharness serves the built KiChat frontend as a plain web page with a
// mocked Wails bridge, so the UI can be driven in a normal browser.
//
// Why this exists: KiChat is a native Wails window. Its UI is not served over
// HTTP, and the generated bindings (window.go.main.App...) are injected by the Go
// runtime into the webview. Opening frontend/dist in a browser therefore shows a
// bootstrap failure, not the app. This harness injects mock-wails.js before the
// app bundle, and the mock answers every bound call from in-memory fixtures — so
// rendering, streaming, settings, markdown and the error boundary can all be
// exercised without a Go backend, a loaded model, or X11 automation.
//
// It is a development tool: it never makes a network call and is not part of the
// shipped app.
//
//	go run ./tools/uiharness
//	go run ./tools/uiharness -dir frontend/dist -addr 127.0.0.1:8791
//
// The frontend must be built first (vite build, or any wails build).
//
// mock-wails.js is embedded into this binary, so a running server picks up a mock
// edit only after a restart: `go run` rebuilds, a previously built binary does
// not. Use go run, or rebuild before restarting, when changing the fixtures.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

//go:embed mock-wails.js
var mockBridge string

// banner makes a harness page unmistakable next to the real app, including in a
// screenshot taken much later.
const banner = `<style>
#__harness-badge{position:fixed;right:8px;bottom:8px;z-index:99999;padding:4px 8px;
border-radius:6px;background:#7c3aed;color:#fff;font:600 11px system-ui,sans-serif;
letter-spacing:.04em;box-shadow:0 2px 8px rgba(0,0,0,.4)}
</style>
<div id="__harness-badge" title="Served by tools/uiharness with a mocked Wails bridge — no Go backend, no real model">MOCK BRIDGE</div>
<script src="/__mock-wails.js"></script>
`

func main() {
	dir := flag.String("dir", "frontend/dist", "directory holding the built frontend")
	addr := flag.String("addr", "127.0.0.1:8791", "listen address")
	flag.Parse()

	if _, err := os.Stat(filepath.Join(*dir, "index.html")); err != nil {
		log.Fatalf("cannot read %s: %v\nBuild the frontend first (wails build, or cd frontend && npm run build).",
			filepath.Join(*dir, "index.html"), err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           &handler{dir: *dir},
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("KiChat UI harness — mock bridge, no backend")
	log.Printf("  sending %s", *dir)
	log.Printf("  open http://%s/", *addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

type handler struct {
	dir string
}

// page reads the built index.html and injects the mock bridge into it. The
// modules are deferred, so a classic script in <head> always runs first and
// window.go exists before the app bundle touches it.
func (h *handler) page() (string, error) {
	path := filepath.Join(h.dir, "index.html")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", path, err)
	}
	patched := strings.Replace(string(raw), "</head>", banner+"</head>", 1)
	if patched == string(raw) {
		return "", fmt.Errorf("%s has no </head>; the mock bridge cannot be injected", path)
	}
	return patched, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "/index.html":
		// Read per request rather than once at startup: the page names a hashed
		// bundle, so an index.html captured before a rebuild points the browser at
		// the previous one and an edited UI appears not to have changed at all.
		// Restarting is what the embedded mock needs; a frontend rebuild is not.
		page, err := h.page()
		if err != nil {
			log.Printf("%-4s %s -> 500: %v", r.Method, r.URL.Path, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// A stale harness page is worse than none: the mock changes as the UI does.
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, page)
		log.Printf("%-4s %s (index, mock injected)", r.Method, r.URL.Path)
		return
	case "/__mock-wails.js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, mockBridge)
		return
	}

	clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	full := filepath.Join(h.dir, filepath.FromSlash(clean))
	// Refuse anything that escaped the served directory.
	if rel, err := filepath.Rel(h.dir, full); err != nil || strings.HasPrefix(rel, "..") {
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(full); err != nil {
		http.NotFound(w, r)
		log.Printf("%-4s %s -> 404", r.Method, r.URL.Path)
		return
	}
	http.ServeFile(w, r, full)
}
