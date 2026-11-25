// Package migrations embeds the SQL migrations so the binary is self-contained
// (`tollgate migrate` needs no files on disk inside the distroless image).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
