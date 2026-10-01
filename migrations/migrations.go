// Package migrations holds the SQL schema files, applied in file-name order.
package migrations

import "embed"

//go:embed *.sql
var Files embed.FS
