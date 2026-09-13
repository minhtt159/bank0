// Package webstatic embeds the operator-console static assets (CSS/JS) so the
// single Go binary serves them without touching disk. Served under /static/ on
// the portal surface (public — the login page needs the stylesheet too).
package webstatic

import "embed"

// htmx.min.js is vendored (htmx 4.0.0, from unpkg.com/htmx.org@4.0.0/dist) and
// served same-origin instead of from a CDN: the operator console moves money, so
// it must not pull a script over the network from a third party with no integrity
// guarantee (or depend on CDN availability). Bump deliberately, like the generators.
// sha384-BvJpBiO8Kh31EqtJe5DRIeWrHWnCGkwytKs9NKFi86Hhw96dEqdEMzZDeK9iEGTc
//
//go:embed console.css console.js htmx.min.js
var FS embed.FS
