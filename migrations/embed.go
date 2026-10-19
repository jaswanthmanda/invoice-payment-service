// Package migrations embeds the SQL migration files so the binary
// can run them on startup with no extra files at runtime.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
