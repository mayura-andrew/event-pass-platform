// Package web holds the SaaS pages (html/template) and their static files,
// embedded so the binary serves them without PUBLIC_DIR. The current build's
// PWA stays in public/.
package web

import "embed"

//go:embed templates static
var FS embed.FS
