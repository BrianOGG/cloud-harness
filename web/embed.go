// Package web contains the browser app built by Vite.
package web

import "embed"

// Dist holds the production assets under dist/.
//
//go:embed all:dist
var Dist embed.FS
