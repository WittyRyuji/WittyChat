# WittyChat

[![CI 状态](https://img.shields.io/github/actions/workflow/status/WittyRyuji/WittyChat/ci.yml?branch=main&style=flat-square&logo=github&label=CI)](https://github.com/WittyRyuji/WittyChat/actions/workflows/ci.yml)
[![最新版本（含预发布）](https://img.shields.io/github/v/release/WittyRyuji/WittyChat?include_prereleases&sort=semver&style=flat-square&label=release)](https://github.com/WittyRyuji/WittyChat/releases)
[![开源许可证](https://img.shields.io/github/license/WittyRyuji/WittyChat?style=flat-square&color=blue)](LICENSE)
[![Windows x64](https://img.shields.io/badge/platform-Windows%20x64-0078D4?style=flat-square)](docs/GETTING_STARTED.md)

**沟通、邮件、协作文档，以及面向 Agent 的企业工作空间。**

An open-source, self-hostable enterprise workspace built for the agent era.

[English](README.md) · [下载 Alpha](https://github.com/WittyRyuji/WittyChat/releases) ·
[贡献指南](CONTRIBUTING.md) · [架构决策](docs/DECISIONS.md)

> **0.1.0-alpha：早期预览，不是生产就绪版本。**
> API、数据库和界面仍会变化；请使用测试数据并做好备份。
> 目前提供未签名的 Windows x64 便携包，需要 Microsoft Edge WebView2。

## 为什么做 WittyChat

把日常团队协作放回一个精简、可控、可自托管的界面。核心保持 AGPL-3.0 开源，
可选商业模块与核心隔离；不把所有网页应用塞进一个内嵌工作台。
第三方网页服务交给系统浏览器，原生聊天、邮件和文档留在应用内。

## 当前可以做什么

| 模块 | Alpha 中的实现 |
| --- | --- |
| 即时通信 | 私聊、公开/私有群、Markdown、引用回复、编辑/撤回、搜索、表情回应、已读回执、在线状态、附件与群头像 |
| 邮件 | 收件箱、写信、文件夹、星标、附件及后台迁移任务；对外投递需要额外配置邮件基础设施和 DNS |
| 协作文档 | 原生文档、表格、演示，Yjs 实时同步，会话分享与权限检查；Office 附件可选接入 OnlyOffice |
| 管理后台 | 账号开通/停用、激活链接、安全和存储策略、附件管理、审计、邮件/飞书迁移 |
| 存储 | 本地磁盘或 S3 兼容存储、去重、分片上传、缩略图、配额与后台维护 |
| HR | 基础申请与审批入口，仍属实验功能 |
| Agent / Zen | 数据结构、接口和界面入口；完整 ACP/MCP Agent 运行时尚未交付 |

飞书迁移支持通讯录建号、应用凭据邮件迁移，以及员工授权导入个人文档。
**当前仅对模拟飞书服务做过端到端验证，尚未验证真实租户。**
普通邮箱仍可使用 IMAP 迁移。

## 架构

```text
Windows desktop: Wails v3 + React 19 + TypeScript + WebView2
                         |
                 HTTP API / WebSocket
                         |
               Go server + background worker
                         |
           PostgreSQL 17 + local objects / S3
```

PostgreSQL 是权威数据源；桌面端有 SQLite 缓存层。聊天以会话操作日志和水印同步，
实时通知用于提醒客户端补拉；协作文档通过 Yjs 更新日志持久化。
邮件、对象存储和 OnlyOffice 是独立的集成边界。

## 快速体验

从 [Releases](https://github.com/WittyRyuji/WittyChat/releases) 下载 Windows x64 压缩包，
校验 `SHA256SUMS.txt` 后解压。包内包含桌面端、服务端、worker 和使用说明；
不包含数据库、WebView2、默认账号、密钥或用户数据。

本版桌面端默认连接 `http://localhost:8080`，尚无面向普通用户的服务器地址配置界面。
首次体验建议在本机同时运行服务端。远程部署与当前限制见 [入门指南](docs/GETTING_STARTED.md)。

### 从源码运行

需要 Go **1.27.3**、Node.js **24**、pnpm **12.16.1**、Wails CLI **3.0.0**、Docker，以及 Windows 桌面构建环境。
以下命令在仓库根目录运行（Git Bash 下使用 `npm.cmd` / `corepack.cmd`）。

```bash
git clone git@github.com:WittyRyuji/WittyChat.git
cd WittyChat
python scripts/gen_dev_env.py
set -a; . deploy/dev.env; set +a
docker compose --env-file deploy/dev.env -f deploy/docker-compose.dev.yml up -d postgres
corepack pnpm --dir admin install --frozen-lockfile
corepack pnpm --dir admin build
go build -o bin/wittychat-server.exe ./server
go build -o bin/wittychat-worker.exe ./worker
./bin/wittychat-server.exe migrate -dir up
./bin/wittychat-server.exe bootstrap -email admin@wittyloong.com -name Admin -password adminpass
./bin/wittychat-server.exe serve
```

另开终端运行 `./bin/wittychat-worker.exe`。应用测试账号使用 `adminpass` / `alicepass`
这类固定弱口令，仅用于本机开发；数据库、Garage 和 OnlyOffice 的服务密钥由
`deploy/dev.env` 在本机随机生成且不会提交。不要把开发配置用于正式部署。

桌面端构建、环境变量、测试数据库和备份说明见：

- [入门与构建](docs/GETTING_STARTED.md)
- [运维与迁移](deploy/OPERATIONS.md)
- [安全披露](SECURITY.md)
- [更新记录](CHANGELOG.md)
- [发布说明与流程](docs/RELEASING.md)

## 路线图与边界

下一阶段重点是实际 Agent 运行时、服务器地址配置、安装/升级体验和真实迁移验证。
OIDC/SSO、移动端、跨实例联邦、完整离线体验和云端 Agent 属于后续规划，
不能视作本版承诺。详细设计见 [DECISIONS](docs/DECISIONS.md)；
其中阶段表是产品规划，不等同于已完成清单。

本项目没有宣称通过 ISO 27001、SOC 2 或任何合规认证；没有外部安全审计保证。
Alpha 不适用于关键业务或受监管数据的唯一存储。

## 开源与贡献

本仓库项目代码按 **GNU AGPL v3.0 only** 发布，另有标注的第三方内容除外。
修改后通过网络向用户提供服务时，请履行 AGPL 第 13 条等对应源码提供义务；
这不是“禁止商业使用”的许可证。

参见 [LICENSE](LICENSE)、[贡献指南](CONTRIBUTING.md)、[CLA](CLA.md)、
[核心开源承诺](GOVERNANCE.md) 和 [第三方说明](THIRD_PARTY_NOTICES.md)。
欢迎提交问题、文档、测试与代码。贡献者保留版权；贡献接受需维护者审核与 CLA 确认，
目前没有自动执行 CLA/分支保护的保证。
