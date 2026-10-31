// Package migrations holds additive-only schema changes shared by SQLite and D1.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
