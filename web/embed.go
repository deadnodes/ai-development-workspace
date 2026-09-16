package web

import (
	"embed"
	"io/fs"
)

//go:embed static/*
var files embed.FS

// FS contains the complete browser application, embedded in the server binary.
var FS, _ = fs.Sub(files, "static")
