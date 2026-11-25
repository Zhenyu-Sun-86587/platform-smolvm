package migrations

import "embed"

// Files contains the ordered forward-only registry migrations.
//
//go:embed *.sql
var Files embed.FS
