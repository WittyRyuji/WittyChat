# Contributing / 贡献指南

感谢参与 WittyChat。请先阅读 [README](README.md)、[架构决策](docs/DECISIONS.md)
和 [行为准则](CODE_OF_CONDUCT.md)。安全漏洞请走 [私密披露](SECURITY.md)，不要公开可利用细节。

## Workflow / 协作流程

1. 缺陷请附版本、系统、复现步骤和脱敏日志；较大的设计/依赖变更先开 Issue。
2. 从最新 `main` 建功能分支，保持补丁聚焦；不要提交运行数据、私钥或真实凭据。
3. Go 使用 gofmt/gofumpt 和 golangci-lint；前端使用 Biome。
   遵循现有风格，必要注释和用户文档保持中英可理解。
4. 提交 PR，解释行为变化、风险、验证命令和结果。
   建议英文 conventional-commit 标题，正文可用中文。
5. 确认 [CLA](CLA.md)，由维护者核实后合并。不要使用 `pull_request_target`
   执行外部贡献的任意代码，也不要向 fork PR 暴露密钥。

Contributors retain copyright. Only submit work you are entitled to license,
including employer approval where needed. Do not paste incompatible third-party
code, private customer data or generated code whose provenance cannot be reviewed.

## Local checks / 本地检查

详见 [构建指南](docs/GETTING_STARTED.md)。先安装锁定的前端依赖并构建 admin SPA。

```bash
go build ./...
go vet ./...
golangci-lint run ./...
WITTYCHAT_TEST_DB_URL='postgres://wittychat:wittychat@localhost:5432/wittychat_test?sslmode=disable' go test -p 1 ./...
corepack pnpm --dir desktop/frontend test
corepack pnpm --dir desktop/frontend build
corepack pnpm --dir admin build
```

**只能用独立测试数据库。** 集成测试未设置 `WITTYCHAT_TEST_DB_URL` 会跳过；
不要把“跳过”当作通过。各包共享测试库，全量测试使用 `-p 1`。
S3 测试需要单独的测试桶和 `WITTYCHAT_TEST_S3_*`，不得指向生产存储。
对权限、同步、水印、幂等和数据生命周期的修改，应补有行为意义的回归覆盖。

## Review / 审核

- 文档区分已实现、实验性与规划；不要宣称没有做过的生产或真实租户验证。
- 迁移通过内置 migrate 命令执行，不手动应用 SQL，不修改已经发布的迁移。
- 新依赖应说明用途、版本、安全记录和许可证，并更新锁文件及第三方说明。
- 避免无关格式化、重写历史或夹带大文件。PR 需说明新增依赖和数据库升级影响。

维护者审核 CLA 和 PR 后才接受贡献；仓库模板本身不等于自动签约或自动审核。
