// Package dbfiles embeds the SQL migrations so the binary is self-contained.
package dbfiles

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
