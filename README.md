# WittyChat

AgenticIM — an agent-native enterprise IM. / 为 Agent 时代设计的精简企业 IM。

- Server: Go · Client: Wails v3 (Go + React/TS + WebView2) · DB: PostgreSQL 17
- Architecture decisions / 架构决策: [`docs/DECISIONS.md`](docs/DECISIONS.md)

## Dev quickstart / 开发快速开始

Toolchain / 工具链: Go 1.27+, Node 24 + pnpm (corepack), Docker, `wails3`,
`golangci-lint`, `migrate`.

```bash
make dev-up        # start Postgres / 启动 PG
make migrate-up    # apply migrations / 应用迁移
make build         # build server + worker / 构建
make lint          # lint / 静态检查
make test          # tests / 测试
```
