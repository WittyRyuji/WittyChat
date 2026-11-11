# WittyChat — agent notes

Self-hostable enterprise IM + coding-agent platform. Go backend (`server/`, `worker/`, `core/`), Wails v3 + React 19 + Vite desktop (`desktop/`). PostgreSQL is authoritative. Comments are bilingual (English / 中文) — keep that style.

## Dev environment (Windows)
- Deps: `docker compose -f deploy/docker-compose.dev.yml up -d` (postgres `deploy-postgres-1`, onlyoffice `deploy-onlyoffice-1`).
- Shell: Git Bash. WSL bash is NOT installed — in Git Bash use `npm.cmd` / `npx.cmd` (plain `npm` hits a broken WSL shim). Set `PYTHONIOENCODING=utf-8` for python.
- Server (repo root): `go build -o wittychat-server.exe ./server`, run with
  `WITTYCHAT_ONLYOFFICE_URL=http://localhost:8180 WITTYCHAT_ONLYOFFICE_JWT_SECRET=wittychat-office-dev ./wittychat-server.exe serve`.
  Worker: `go build -o worker.exe ./worker && ./worker.exe` (thumbnails, AV, mail).
- Rebuilding an exe that is running fails on Windows — stop it first.

## Databases
- Dev DB: `wittychat`. Test DB: `wittychat_test` (Go integration tests ONLY — never point tests at the dev DB; that is how ~600 junk accounts accumulated).
- Migrations: always `./wittychat-server.exe migrate -dir up` (`-dir version` to check). Never hand-apply migration SQL — it desyncs `schema_migrations`. For the test DB prefix `WITTYCHAT_DB_URL='postgres://wittychat:wittychat@localhost:5432/wittychat_test?sslmode=disable'`.

## Gates
- Go (repo root): `go build ./...`, `go vet ./...`, `golangci-lint run ./...` (expects 0 issues; nolint only with a reason, repo convention),
  `WITTYCHAT_TEST_DB_URL='postgres://wittychat:wittychat@localhost:5432/wittychat_test?sslmode=disable' go test ./...` (DB tests skip silently without it). Use `go test -p 1 ./...` for the full suite: packages share the test DB and `TestReplaceBytesVersionsAndQuota` reads a global usage total, so parallel packages make it flaky.
- Frontend (`desktop/frontend`): `npm.cmd run build` (tsc + vite), `npm.cmd test` (vitest), `npx.cmd biome check <files>` (pre-existing style warnings exist; no new errors).
- API smoke against a running server: `python scripts/smoke_p0.py` (creates smoke-* users in the dev DB).

## Desktop build
The Wails Taskfile's `npm install` step fails here (`Cannot read properties of null (reading 'matches')`). Build manually:
`npm.cmd run build` in `desktop/frontend`, then in `desktop/`:
`go build -tags production -trimpath -buildvcs=false -ldflags="-w -s -H windowsgui" -o bin/wittychat.exe .`
Debug with CDP: launch with `WITTYCHAT_DEBUG_PORT=9222` (`WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS` is ignored). WebView2 profile: `%APPDATA%\wittychat.exe\EBWebView`.

## Resetting / seeding dev data
1. Stop server, worker, desktop app.
2. Drop + recreate `wittychat` (owner `wittychat`), `migrate -dir up`.
3. Clear `data/objects`, `data/thumbs`, `data/tmp` (keep `data/secret.key`, `data/tls`); optionally delete the EBWebView profile.
4. `./wittychat-server.exe bootstrap -email admin@wittyloong.com -name Ryuji -password <pw>`, complete first-run setup (`POST /setup/confirm`), `./wittychat-server.exe dkim -domain wittyloong.com`.
5. Start server + worker, then `python scripts/seed_dev.py` (creates Alice/Carol, DM/group/stickers/mail with image fixtures incl. EXIF orientations; manifest at `%TEMP%\wc_seed\manifest.json`). The sticker shelf is per-device localStorage (`wittychat.stickers`) — inject `manifest.stickers` manually.

## S3 (dev)
- Garage in `deploy/docker-compose.dev.yml` (`dxflrs/garage:v2.5.0`, ports 3900/3903, config `deploy/garage/`). After `compose up garage`, run `sh deploy/garage/bootstrap.sh` once (layout + bucket + key `GKDEV0DEV0DEV0DEV0DEV0`/`devsecret_devsecret_devsecret_012345`), then `compose up garage-cors` applies bucket CORS (PUT/GET/HEAD, `ExposeHeaders: ETag`).
- Server/worker env: `WITTYCHAT_S3_ENDPOINT=http://localhost:3900`, `WITTYCHAT_S3_BUCKET=wittychat`, `WITTYCHAT_S3_ACCESS_KEY`/`WITTYCHAT_S3_SECRET_KEY` as above, optional `WITTYCHAT_S3_PUBLIC_ENDPOINT` (client-reachable presign host; SigV4 signs Host — the endpoint must preserve it).
- S3 integration tests: `WITTYCHAT_TEST_S3_ENDPOINT/BUCKET/ACCESS_KEY/SECRET_KEY` (+ optional `_REGION`) plus the test DB DSN; `go test ./core/objects/` skips them silently without the env.

## Image dimensions (objects.width/height)
Sniffed at upload (`core/objects/imgmeta.go`, EXIF-orientation aware for JPEG) and by the server's background sweep (`RunDimsBackfill`, keyed on `objects.dims_checked`). Never sniff on the request path — it previously truncated downloads. Client display boxes come from `desktop/frontend/src/objdims.ts` (`FIT` caps must mirror the CSS).

## Dev IMAP (devimap)
`go run ./scripts/devimap` starts an in-process IMAP server on `127.0.0.1:1143` (plain) for testing the 邮件搬家 admin tab. Users: `zhang@wittyloong.com / zhang-app-pass` (~318 msgs incl. GBK/multipart/cid/flags) and `old@feishu-old.test / old-pass` (20 msgs).

## Dev Feishu (devfeishu)
`go build -o devfeishu.exe ./scripts/devfeishu && ./devfeishu.exe` serves a mock Feishu OpenAPI on `127.0.0.1:1180` (bot messages are logged to stderr). PUT `/api/v1/admin/feishu/app` with `base_url=http://127.0.0.1:1180` (any secret except `bad`). Directory: Ryuji + Alice (existing), dave/erin (provisioned), one resigned; each mailbox has 33 messages (INBOX 30, SENT 2, 项目/2024 1). OAuth redirects straight back as `DEVFEISHU_OAUTH_USER` (default alice). Clear the app config and `feishu_*` state afterwards.
