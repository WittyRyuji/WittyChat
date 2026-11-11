# WittyChat

**A self-hostable enterprise workspace for the agent era.**

[简体中文](README.md) · [Releases](https://github.com/WittyRyuji/WittyChat/releases) ·
[Contributing](CONTRIBUTING.md) · [Architecture](docs/DECISIONS.md)

WittyChat brings team messaging, email, collaborative documents and future coding-agent
workflows into one Windows-first application. The core is AGPL-3.0-only; optional
commercial modules are intended to remain separate. External web integrations open
in the system browser rather than a generic embedded workbench.

## Status: 0.1.0-alpha

This is an **early preview, not a production-ready release**. APIs, schemas and UI
may change. Use test data and maintain backups. Windows x64 binaries are unsigned
and require Microsoft Edge WebView2. The archive includes desktop, server and worker
binaries, but no database, credentials, default account or user data.

Implemented areas include direct/group messaging, attachments, reactions, search,
live user identities, mail UI, administration/audit, local/S3-compatible storage,
native Yjs document/sheet/slide collaboration, and experimental HR workflows.
OnlyOffice is an optional external service.

Feishu migration supports account provisioning, app-credential mail migration and
employee-authorized personal-document import. It has only been tested against a
mock Feishu service, **not a real tenant**. IMAP migration remains available.

Agent/Zen currently provides interfaces, data structures and UI entry points:
the complete ACP/MCP agent runtime is **not implemented**. OIDC, mobile clients,
federation and cloud agents are roadmap items, not included features.

## Architecture and setup

- Go server and background worker; PostgreSQL 17 is authoritative.
- Wails v3, React 19, TypeScript and WebView2 desktop; SQLite cache layer.
- Conversation operation logs and watermarks; Yjs update logs for documents.
- Local object files or S3-compatible storage; separate mail/OnlyOffice integrations.

See [Getting Started](docs/GETTING_STARTED.md) for release-binary setup and source
build commands. Toolchain: Go 1.27.3, Node.js 24, pnpm 12.16.1, Docker.
Build the admin SPA **before** compiling the Go server: it embeds generated assets.

The desktop currently defaults to `http://localhost:8080`; a user-friendly remote
server selector is not yet available. Use a local server for the simplest preview.
External email requires additional SMTP/MX/DNS setup. The production Compose file
is a reference topology, not a turnkey, security-audited deployment.

## Project documents

- [Operations and migration](deploy/OPERATIONS.md)
- [Contributing](CONTRIBUTING.md), [CLA](CLA.md), [governance](GOVERNANCE.md)
- [Security reporting](SECURITY.md), [code of conduct](CODE_OF_CONDUCT.md)
- [Changelog](CHANGELOG.md), [release process](docs/RELEASING.md)
- [Third-party notices](THIRD_PARTY_NOTICES.md)

## License

Project code is licensed under **GNU Affero General Public License v3.0 only**,
except components with their own notices. Commercial use is allowed subject to the
license; modified network deployments must comply with AGPL's corresponding-source
obligations. See [LICENSE](LICENSE). There is no warranty or certification claim.
Contributors retain copyright; maintainers require review and explicit CLA acceptance.
