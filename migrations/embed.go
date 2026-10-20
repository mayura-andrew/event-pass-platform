// Package migrations embeds the SQL migrations so cmd/migrate and the tests
// apply exactly the files in this directory.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
