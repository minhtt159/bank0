// Package webstatic embeds the operator-console static assets (CSS/JS) so the
// single Go binary serves them without touching disk. Served under /static/ on
// the portal surface (public — the login page needs the stylesheet too).
package webstatic

import "embed"

// htmx.min.js is vendored (htmx 2.0.3, from unpkg.com/htmx.org@2.0.3/dist) and
// served same-origin instead of from a CDN: the operator console moves money, so
// it must not pull a script over the network from a third party with no integrity
// guarantee (or depend on CDN availability). Bump deliberately, like the generators.
// sha384-0895/pl2MU10Hqc6jd4RvrthNlDiE9U1tWmX7WRESftEDRosgxNsQG/Ze9YMRzHq
//
//go:embed console.css console.js htmx.min.js
var FS embed.FS
