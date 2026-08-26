// Package web holds the single-page application `vaultd serve` serves
// (SPEC §13).
//
// The assets are compiled into the binary, so the daemon needs nothing on disk
// and the UI cannot drift out of step with the API it talks to. They are
// hand-written rather than bundled: a Node toolchain in the build would buy
// nothing here — four screens and no dependencies — and would cost `go build
// ./...` its self-sufficiency, which the release story (a static CGO-free
// binary) depends on. Nothing is loaded from a CDN.
package web

import "embed"

// Assets is the SPA: one page, one stylesheet, one script.
//
//go:embed index.html app.js style.css
var Assets embed.FS
