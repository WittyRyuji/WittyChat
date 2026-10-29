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
- [ ] M5 最小前端：激活页、登录页、会话列表、消息流（虚拟滚动）、输入框（`n/2000`、引用回复）、断线补差额、水印去重、待发送队列
- [ ] M6 附件最小路径 + worker 骨架：`objects` + ≤5MB 直传本地盘 + 成员鉴权下载 + 任务表消费（缩略图跑通流水线）+ Wails 壳打包验证

**出口**：两个客户端互发消息实时到达；断线重连补差额不丢不重（集成测试覆盖）。

## Phase 2 — IM 补全 + 管理后台 v1

- IM：群管理（建群/改名/邀请/踢人/转让、公开群搜索加入）、@所有人权限、已读回执开关生效、撤回时限从设置读、消息搜索（PG 全文 body）、未读数聚合、通知中心占位
- 管理后台 v1（`admin/` SPA）：用户列表/开通/激活链接复制/停用/离职交接给 Admin、安全策略（密码规则/2FA/锁定/超时）、审计日志查看器（查看行为自审计）
- 设置三层生效：workspace_settings → 消息上限/撤回时限/已读开关 逐层读取

**出口**：管理员全程网页开通账号；群功能齐；私聊互发可见已读。

## Phase 3 — 存储补全

- 大文件预签名直传 S3（multipart）、SHA-256 秒传去重生效、短时效下载 URL
- worker：图片缩略图、孤儿对象 GC（7 天）、按热度+年龄冷迁移、哈希校验、AV 插件接口（仅接口）
- 工作区配额 + 近满告警与上传拦截
- 离线外发队列在桌面端落 SQLite（客户端缓存层 v1：会话/消息/水印/待发）

**出口**：1GB 文件走 S3 不占服务端带宽；同文件秒传；超配额被拒；冷数据透明回迁。

## Phase 4 — 邮件

- maddy 进 compose：入站 :25（STARTTLS）、DKIM 签名、rspamd 挂载点；部署文档含 SPF/DKIM/DMARC/PTR
- 入站链路：maddy → 核心服务 → 原文分段入 objects + 解析入 PG + 附件入统一对象表（后台任务流式解析）
- 出站：SMTP 客户端直连 MX（go-smtp），内部收件人直投不过 SMTP
- 客户端邮件区（左侧栏）：收件箱/已发/草稿/垃圾/归档 + 自定义文件夹、写信（Markdown/附件）、回复链、附件名入搜索索引
- 别名地址、内部邮件同模型

**出口**：对外收发邮件走通（真实外域互发验证）；内部邮件即时达；原文不落明文内存外任何地方。

## Phase 5 — Agent-ready 表面 + 管理后台 v2

- 助理 CRUD（runtime=acp|api、adapter、model、instructions、scopes、session_level L1-L4、auto_send）——配置全开，运行时禁用占位
- MCP 工具面契约：`im.read / im.send / mail.read / mail.send / objects.read / zen.session.*` 的 JSON Schema 定义进 `shared/proto`（P1 直接实现）
- ACP 类型从官方 Schema 生成进 `shared/proto`（代码先备好）
- 审批/代发 UI 占位与 hook 审计事件完整覆盖（审批动作/副作用/会话启止/权限变更）
- 管理后台 v2：存储策略（阈值/配额/S3）、邮件策略、等保三级预设一键应用、审计导出、系统/工作区设置分层只读展示

**出口**：助理能建能配（不运行）；所有对外契约冻结进 shared/proto。

## Phase 6 — 部署打包 + 加固 + 验收

- 生产 compose：nginx（单入口 TLS）+ server + worker + PG + maddy + 可选 minio(S3)；数据卷与备份文档
- 桌面端打包：wails3 build 安装包 + WebView2 bootstrapper + 防火墙规则（为 P2 LAN 直传预留文档）
- 加固走查：ASVS L2 清单（headers/CSRF/限速/密钥加密存储/日志脱敏）、审计日志 180d 保留验证
- E2E：激活→发消息→传附件→收发邮件→停用账号 全链路冒烟
- 性能抽查：10k 消息会话虚拟滚动、断线补 1k op 差额耗时、1000 会话目录比对

**出口**：干净机器 `compose up` 起整套；桌面安装包可装可跑；冒烟全绿。

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
