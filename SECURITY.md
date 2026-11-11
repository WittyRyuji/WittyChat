# Security / 安全

## Supported versions / 支持范围

Only the latest alpha receives best-effort fixes. Alpha is not a production support
contract; older prereleases may not be patched. No external security audit or
ISO/SOC certification is claimed.

## Report privately / 私密披露

Use GitHub's [private vulnerability reporting](https://github.com/WittyRyuji/WittyChat/security/advisories/new)
if enabled. If unavailable, contact the repository owner through the public contact
methods on [their GitHub profile](https://github.com/WittyRyuji) to arrange a private
channel **before sending sensitive details**. Do not put exploit credentials,
customer records, session tokens or private keys in public Issues.

Include the affected version/commit, deployment, impact, reproduction and a minimal
sanitized example. Coordinated disclosure is appreciated; there is no guaranteed
response time or automatic bounty.

## Deployment checklist / 部署检查

- Use HTTPS, correct `WITTYCHAT_PUBLIC_URL`, strong unique secrets and restricted DB/S3 access.
- Never expose development Compose defaults or WebView2 debugging ports publicly.
- Back up PostgreSQL, object bytes **and `data/secret.key`** together. Losing the key
  can make encrypted credentials unreadable. Server and worker need the same key.
- Keep session tokens out of screenshots/logs; review who can administer/audit the workspace.
- OnlyOffice requires a private matching JWT secret; expose no unauthenticated callback.
- Do not rely on alpha as the sole copy of important or regulated data.
- Treat Feishu/IMAP credentials as tenant-wide sensitive access and use test tenants first.

仓库中的开发密码和模拟服务凭据不是生产秘密，严禁直接用于生产。
