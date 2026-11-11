# WittyChat 0.1.0-alpha

首个公开 Alpha：可自托管的企业消息、邮件与协作文档工作空间。
First public alpha of the Windows-first, self-hostable enterprise workspace.

## Downloads / 下载

- `WittyChat-0.1.0-alpha-windows-amd64.zip`: unsigned portable desktop + server + worker.
- `SHA256SUMS.txt`: SHA-256 checksum for the archive.
- GitHub source archives: source for the exact tagged version, including build scripts.

Requires Windows x64, WebView2, and a separately provisioned PostgreSQL 17 database.
Read the included `docs/GETTING_STARTED.md` before running. No default credentials
or user data are included. Default desktop server: `http://localhost:8080`.

## Highlights / 内容

Direct/group chat, live identities and group avatars, reactions/search/attachments,
mail/admin/audit, local/S3 storage, collaborative documents/sheets/slides, optional
OnlyOffice, and experimental HR and Feishu/IMAP migration.
Recent fixes cover atomic settings saves, offline catch-up, core-asset protection,
sticker retention and shared/retrying avatar requests.

## Important limits / 重要限制

This is **not production-ready**. Use test data and backups. Binaries are unsigned;
there is no supported installer/update channel or friendly remote-server selector.
Full Agent/ACP/MCP runtime, OIDC, mobile clients and federation are not included.
Feishu migration has been tested with a mock service only, not a real tenant.
External mail delivery and production infrastructure need operator configuration.
No external security audit or compliance certification is claimed.

The core is AGPL-3.0-only, with separate third-party notices. See the README,
LICENSE, CONTRIBUTING, CLA, SECURITY and CHANGELOG in the tagged source.
