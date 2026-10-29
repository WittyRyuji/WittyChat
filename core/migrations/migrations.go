// Package migrations embeds the SQL migration files so the server binary can
// apply them itself (server migrate) without shipping separate files.
//
// migrations 包内嵌迁移 SQL，使 server 二进制可自行执行迁移（server migrate），
// 无需随包分发 SQL 文件。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
