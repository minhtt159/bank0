// Package webstatic embeds the operator-console static assets (CSS/JS) so the
// single Go binary serves them without touching disk. Served under /static/ on
// the portal surface (public — the login page needs the stylesheet too).
package webstatic

import "embed"

//go:embed console.css console.js
var FS embed.FS
