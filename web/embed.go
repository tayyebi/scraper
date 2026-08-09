// Package web holds the Operator Console's static assets.
//
// The embed lives here rather than in internal/adapters/console because
// go:embed cannot reach outside its own directory tree. Keeping the assets at
// web/console and the embed beside them is the only arrangement that lets the
// files stay editable as plain files while still compiling into the binary.
package web

import "embed"

// Console holds index.html, style.css and app.js.
//
// There is no build step and no node_modules: three files, served as written.
// That is a deliberate constraint, not a limitation -- a console that needs a
// toolchain to change is a console nobody changes.
//
//go:embed console
var Console embed.FS
