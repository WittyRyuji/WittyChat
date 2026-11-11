# Changelog / 更新记录

## 0.1.0-alpha

First public alpha preview / 首个公开 Alpha 预览。

### Included / 内容

- Windows-first Wails desktop, Go server/worker and PostgreSQL schema through migration 23.
- Messaging, group management/avatars, attachments, reactions, search and live identities.
- Mail UI, admin console, audit, storage governance, local/S3 object lifecycle.
- Native collaborative documents, sheets and slides; optional OnlyOffice integration.
- Experimental HR and Feishu/IMAP migration flows.
- Reconnect identity/message synchronization; transactional settings; core-asset and sticker retention fixes.
- AGPL license, contribution/CLA/governance/security documentation and reproducible release workflow.

### Limitations / 限制

- Not production-ready; unsigned Windows x64 binaries; WebView2 and PostgreSQL are separate prerequisites.
- Desktop defaults to localhost:8080; no friendly remote-server selector or supported upgrade UI.
- Agent runtime, OIDC, mobile and federation are not complete/included.
- Feishu integration is mock-tested only, not verified against a real tenant.
- No compliance certification, end-to-end message encryption or external security-audit claim.
- Source-build Docker topology and external mail delivery require operator configuration.

The older local `v0.1.0` development tag predates this public alpha and is not
rewritten or used as this release's source.
