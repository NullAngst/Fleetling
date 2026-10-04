// Package web embeds the templates and static assets into the binary.
//
// static/dist is produced by esbuild from web/src (see the Dockerfile and
// the CI workflow). A plain `go build` without that step still works; the
// pages just load without their JavaScript.
package web

import "embed"

//go:embed templates static
var FS embed.FS
