// Package migrations embeds the goose SQL migrations so they can be applied from
// the application binary itself (no goose CLI needed in-cluster).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
