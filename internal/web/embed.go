package web

import (
	"embed"
	"io/fs"
)

//go:embed static/*
var embeddedFiles embed.FS

// StaticFS returns the embedded static web UI filesystem rooted at static/.
func StaticFS() fs.FS {
	sub, err := fs.Sub(embeddedFiles, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
