// Package web carries the built React frontend inside the binary.
//
// `npm run build` writes to web/dist (see vite.config.ts). The directory is
// committed empty so that `go build` works on a fresh clone — a binary built
// without a frontend build still serves the API, it just has no UI.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// Assets returns the built frontend rooted at web/dist. ok is false when the
// binary was built without running `npm run build` first.
func Assets() (assets fs.FS, ok bool) {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, false
	}
	return sub, true
}
