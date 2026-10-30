# WittyChat P0 开发计划

P0 出口目标：**激活 → 登录 → IM + 附件 + 邮件 + 管理后台**全链路可用；
Agent 接口与数据结构就位（不接真 Agent）；一套 Docker 部署跑通。
依据：[`DECISIONS.md`](DECISIONS.md)。

---

## Phase 1 — 竖切链路（进行中）

打通最难的：schema → 认证 → op 流消息通道 → 同步 → 最小界面 → 附件骨架。

- [x] M1 仓库骨架 + 工具链（depguard 边界已验证）
- [x] M2 初始 schema（16 表）+ dev PG
- [x] M3 认证链路：CLI 首个 Admin → Admin 建用户出激活链接（72h）→ 激活页设密码 → 邮箱密码登录（argon2id/限速/锁定）→ 设备会话（30d 闲置、改密全踢）→ 2FA(TOTP) 结构与 API
- [x] M4 IM 核心：会话（私聊/公开群/私有群）、`conv_ops` op 流（message/edit/recall/member）、`client_msg_id` 幂等、2000 字校验、目录比对 + 按会话补差额 API、WS 仅提醒推送、已读位置、暂态（输入中/在线）内存通道、audit hook 落 `audit_events`
- [x] M5 最小前端：激活页、登录页、会话列表、消息流（虚拟滚动）、输入框（`n/2000`、引用回复）、断线补差额、水印去重、待发送队列
- [x] M6 附件最小路径 + worker 骨架：`objects` + ≤5MB 直传本地盘 + 成员鉴权下载 + 任务表消费（缩略图跑通流水线）+ Wails 壳打包验证

**出口**：两个客户端互发消息实时到达；断线重连补差额不丢不重（集成测试覆盖）。

## Phase 2 — IM 补全 + 管理后台 v1 ✅

- IM：群管理（建群/改名/邀请/踢人/转让、公开群搜索加入）、@所有人权限、已读回执开关生效、撤回时限从设置读、消息搜索（PG 全文 body）、未读数聚合、通知中心占位
- 管理后台 v1（`admin/` SPA）：用户列表/开通/激活链接复制/停用/离职交接给 Admin、安全策略（密码规则/2FA/锁定/超时）、审计日志查看器（查看行为自审计）
- 设置三层生效：workspace_settings → 消息上限/撤回时限/已读开关 逐层读取

**出口**：管理员全程网页开通账号；群功能齐；私聊互发可见已读。 ✅ 实测通过（建群/公开群加入/改名/角色/@all/中文搜索/未读聚合/设置热生效/审计自审）。
## Phase 3 — 存储补全 ✅

- 大文件预签名直传 S3（multipart，init/complete + dedup 短路）✅、SHA-256 秒传 ✅、短时效签名下载 URL（15min）✅ —— `core/objects/s3.go`、`sign.go`
- worker：图片缩略图 ✅、孤儿对象 GC（7 天，含未引用 ready 对象）✅、冷迁移（90 天未访问 → S3）✅、哈希校验 ✅、AV 插件接口 `Scanner`（NoopScanner 默认，检出即隔离）✅ —— `core/objects/maint.go`
- 工作区配额：`storage_quota_mb` 超限拒收（HTTP 507）✅、`upload_max_mb` 生效 ✅、`/objects/quota` 查询 ✅；近满告警 UI 留 P1（接通知中心）
- SQLite 缓存层 ✅：`desktop/cache`（convs/ops/watermarks/outbox/kv，测试通过）；前端启动重放待发接线留 P1

**出口**：S3 直传不占服务端带宽 ✅（presigned PUT）；同文件秒传 ✅；超配额被拒 ✅；冷数据迁移后 `Open` 按 location 透明取回 ✅。

## Phase 4 — 邮件 ✅（内部链路实测；外域互发待公网验证）

- maddy 进 compose ✅（dev :1025/:1143/:1587，prod :25/:587/:993）；DKIM/反垃圾由 maddy 承担；DNS（SPF/DKIM/DMARC/PTR）部署指引见 `deploy/OPERATIONS.md`
- 入站链路：原始 .eml 入 objects → worker `mail_parse` 解析（小解析器，multipart 取 text/plain）→ 本地收件人投递 inbox + `raw_object_id` 回链 ✅
- 出站：`SendMX`（net/smtp + LookupMX + STARTTLS 升级）任务 `mail_send` 逐域投递；内部收件人 `Deliver` 直投不过 SMTP ✅
- 客户端邮件区（左侧栏视图切换）：收件箱/已发/草稿/垃圾/归档/回收站 + 写信（正文 + 附件经对象存储）+ 回复（Re:+In-Reply-To）+ trgm/FTS 搜索 ✅；自定义文件夹 UI 留 P1（schema 支持 custom）
- 别名地址 CRUD（`mail_addresses` primary/alias）✅；建用户自动配主地址 + 老用户首发补配 ✅

**出口**：内部邮件即时达 ✅（冒烟实测）；外发代码路径就绪，真实外域互发需公网 MX 环境验证；原文 .eml 只落 objects ✅。

## Phase 5 — Agent-ready 表面 + 管理后台 v2 ✅（核心面；UI/预设留 P1）

- 助理 CRUD ✅（`core/agents` + `/api/v1/assistants`：runtime acp|api、adapter、model、instructions、scopes、session_level、auto_send；status 恒 disabled）
- MCP 工具面契约 ✅ `shared/proto.Tools`（im.send/read/search、objects.upload/fetch、mail.send/search、approval.request，含 scope + JSON Schema）
- ACP 类型 ✅ `shared/proto`（SessionConfig/MCPServerSpec/PromptContentBlock/AgentEvent 手写子集；官方 schema 全量生成留 P1）
- 审批/代发：`ApprovalRequest` 类型冻结 + `/api/v1/approvals` 占位（list 空、decide 501）；审批 UI 留 P1；助理 CUD 审计 hook ✅
- 管理后台 v2：审计导出 ✅ `/admin/audit/export` CSV；存储/邮件策略经 workspace_settings 键；等保预设一键应用 + 分层只读展示留 P1

**出口**：助理能建能配（不运行）✅；对外契约冻结进 shared/proto ✅。

## Phase 6 — 部署打包 + 加固 + 验收 ✅

- 生产 compose ✅：`deploy/docker-compose.prod.yml` + `nginx.conf` + Dockerfile.server/worker + `.env.example` + `deploy/OPERATIONS.md`（备份/恢复/健康检查）
- 桌面端打包 ✅：`wails3 build` exe 已验证；`deploy/installer.nsi` 含 WebView2 bootstrapper 检测静默安装；防火墙说明入 OPERATIONS
- 加固走查 ✅ `docs/ASVS-L2.md` 逐条自查；审计 180d 保留（仅追加表，不自动删）
- E2E ✅ `scripts/smoke_p0.py` **22/22**：激活→消息→附件+签名URL→内部邮件→停用拒登
- 性能抽查 ✅ `scripts/perf_p0.py`：10k 消息首屏 12ms、1k op 差额 81ms、1009 会话目录 31ms

**出口**：compose 文件齐备（干净机器部署未实测）；exe 已构建（NSIS 模板未编译）；冒烟全绿 ✅。

---

## 依赖与并行

```text
Phase1(竖切) → Phase2(IM补全) → Phase3(存储) → Phase4(邮件) → Phase6(部署)
Phase5 的数据结构与契约从 Phase1 起随做随补，可在 Phase3 后集中收尾。
管理后台 v1 依赖 M3 认证；客户端本地 SQLite 缓存依赖 M4 同步 API。
```

## 风险与对策

- **maddy 成熟度**：Phase4 先做入站投递最小验证，不过关退 Postfix（架构不变，换 compose 服务）
- **Wails v3 新发布**：锁 3.0.x，绑定生成入 Makefile；出问题回退点已留（e05da20 后的每步可独立验）
- **npm 源慢**：`.npmrc` 配 npmmirror（本次已感知，顺手配）
- **WebView2 缺失**：安装包带 bootstrapper（wails3 自带生成任务）
- **范围蔓延**：P0 不做——线程、访客、OIDC、文档预览、视频预览、IMAP、网页邮箱、多邮件域、邮件标签、个人配额、LAN 直传、联邦、真实 Agent
