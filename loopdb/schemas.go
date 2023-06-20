package loopdb

import (
	"embed"
)

//go:embed sqlc/migrations/*.up.sql
var sqlSchemas embed.FS
