// Package web embeds the browser client into the binary.
package web

import "embed"

//go:embed index.html app.js style.css
var Files embed.FS
