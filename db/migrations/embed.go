package migrations

import "embed"

// Files contains the reviewed migration set used by the production migrator.
//
//go:embed *.sql
var Files embed.FS
