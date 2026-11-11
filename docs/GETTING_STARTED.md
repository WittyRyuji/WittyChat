# Getting Started / 入门

## Windows portable release / Windows 便携包

Download the archive and `SHA256SUMS.txt` from the same GitHub release.
Compare with `Get-FileHash <archive.zip> -Algorithm SHA256` before extracting.
The binaries are unsigned: Windows may warn about an unknown publisher.
Verify provenance; never disable security controls globally to bypass warnings.
Install Microsoft's WebView2 Evergreen Runtime if it is missing.

The archive includes `wittychat.exe`, `wittychat-server.exe`, `wittychat-worker.exe`,
project documents, dependency notices and build metadata. PostgreSQL is not bundled.

1. Provision a PostgreSQL 17 database and user for this instance.
2. In PowerShell, configure the backend in a writable directory:

```powershell
$env:WITTYCHAT_DB_URL = "postgres://USER:PASSWORD@localhost:5432/wittychat?sslmode=disable"
$env:WITTYCHAT_DATA_DIR = "$PWD\data"
$env:WITTYCHAT_PUBLIC_URL = "http://localhost:8080"
$env:WITTYCHAT_MAIL_DOMAIN = "example.com"
.\wittychat-server.exe migrate -dir up
.\wittychat-server.exe bootstrap -email admin@example.com -name Admin
.\wittychat-server.exe serve
```

Use TLS and appropriately encoded credentials outside local testing.
Visit `http://localhost:8080` to complete first-run setup, save the encryption key
backup, and activate the administrator with the one-time link printed by bootstrap.
The admin console is `http://localhost:8080/admin/`. There is no default password.

Run `.\wittychat-worker.exe` in another terminal with the **same database, data
directory, public URL and optional storage settings**. Then start `.\wittychat.exe`.
Stop server and worker before restoring backups or applying upgrades.

## Source build / 源码构建

Use Go 1.27.3, Node.js 24, pnpm 12.16.1 and Docker. On Windows use
`corepack.cmd`/`npm.cmd` in Git Bash where plain shims are broken.

```bash
corepack pnpm --dir admin install --frozen-lockfile
corepack pnpm --dir admin build
corepack pnpm --dir desktop/frontend install --frozen-lockfile
corepack pnpm --dir desktop/frontend build
go build -o bin/wittychat-server.exe ./server
go build -o bin/wittychat-worker.exe ./worker
cd desktop
go build -tags production -trimpath -buildvcs=false -ldflags="-w -s -H windowsgui" -o bin/wittychat.exe .
```

The admin SPA and desktop `dist` directories are generated, not committed.
Build them before compiling packages that embed them. Linux server/worker can be
built from the root with `CGO_ENABLED=0 go build ./server` and `./worker`;
the Windows desktop is the only packaged/tested GUI target for this alpha.

For browser development, run the desktop frontend Vite dev server; its `/api` and
`/ws` proxy targets default to localhost:8080. This is a development server, not
an additional production deployment.

## Remote desktop connection / 远程连接限制

There is no server selector in this alpha. The WebView uses a persistent
`wittychat.server` localStorage override, read at page load. Developers can set
it in the WebView debugging console and reload:

```javascript
localStorage.setItem("wittychat.server", "https://chat.example.com");
location.reload();
```

Changing servers also requires signing out/clearing old tokens. Debugging is
enabled only by `WITTYCHAT_DEBUG_PORT`; the port permits full session access,
must stay local, and must be disabled after troubleshooting. This workaround is
not a polished remote-deployment experience.

## Data and optional integrations / 数据与集成

Back up PostgreSQL, object bytes and `data/secret.key` together. Do not share the
key or copy development data into a release. Run migrations with the application's
CLI, never by directly executing migration SQL.

Local object storage needs no S3 configuration; large multipart uploads need S3.
OnlyOffice is disabled without its URL and JWT secret. Email beyond internal
testing requires mail transport, MX/SPF/DKIM/DMARC and operational setup.
Feishu needs a properly permissioned app and employee OAuth for personal documents.
See [Operations](../deploy/OPERATIONS.md) and [Security](../SECURITY.md).
