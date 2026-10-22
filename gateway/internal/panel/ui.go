package panel

import (
	"embed"
	"io/fs"
)

// The frontend (panel/ in the repository) is built into ui/ by `make panel`.
//
//go:embed all:ui
var uiFiles embed.FS

// UI returns the built frontend.
func UI() fs.FS {
	sub, _ := fs.Sub(uiFiles, "ui")
	return sub
}
