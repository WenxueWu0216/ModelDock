//go:build !embedweb

// Package web holds the React console. Built with -tags embedweb, the
// binary embeds web/dist (run `npm run build` here first, or `make build`);
// without the tag the API serves a short note instead, so plain
// `go build ./...` and `go test ./...` never need Node.
package web

import "io/fs"

// FS reports that the console was not embedded in this build.
func FS() (fs.FS, bool) { return nil, false }
