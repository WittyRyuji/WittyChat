# 运维手册 / Operations

## 备份 (Backup)

每日三件套，全部纳入同一备份窗口：

```bash
# 1. PostgreSQL（唯一真源）
docker exec wittychat-postgres-1 pg_dump -U wittychat wittychat | gzip > wittychat-$(date +%F).sql.gz

# 2. 对象存储（local 模式；s3 模式由 bucket 版本/生命周期负责）
docker run --rm -v wittychat_objdata:/data -v $PWD:/b alpine tar czf /b/objects-$(date +%F).tgz /data

# 3. maddy 状态（DKIM 私钥、队列）
docker run --rm -v wittychat_maddydata:/data -v $PWD:/b alpine tar czf /b/maddy-$(date +%F).tgz /data
```

恢复：先建空库灌 `.sql.gz`，再解 `objects-*.tgz` 到 objdata 卷。objects 按 sha256 寻址，重建后与库一致。

## 审计保留 (Audit Retention)

- `audit_events` 仅追加，**保留 180 天**（等保三级下限）。
- 清理策略由管理员决定（建议冷归档到对象存储后删除），P0 不自动删除。
- 校验：`SELECT min(created_at), max(created_at), count(*) FROM audit_events;`
- 导出：`GET /api/v1/admin/audit?format=csv`（管理员）。

## 桌面安装包 (Desktop Installer)

Wails3 产出 `bin/wittychat.exe`（已验证 11.4MB）。

- **WebView2**：Win11 预装；Win10 多数已带。缺失时引导用户装
  Evergreen Bootstrapper（微软官方 1.6MB 在线安装包），或在 NSIS
  安装器里静默执行 `MicrosoftEdgeWebview2Setup.exe /silent /install`。
- **NSIS 安装器**（可选）：`deploy/installer.nsi` 模板打包 exe +
  检查 WebView2 runtime，缺则联网拉取。
- **防火墙**：exe 只做出站 443/80 + wss，不需要入站放行。

## S3 对象存储 (S3 Object Storage)

可选：对象字节放 S3 兼容存储，DB 仍存元数据与引用。未配置即 local 模式。

| 环境变量 | 说明 |
|---|---|
| `WITTYCHAT_S3_ENDPOINT` | S3 API 端点（服务端内部地址），如 `http://localhost:3900` |
| `WITTYCHAT_S3_BUCKET` | 桶名 |
| `WITTYCHAT_S3_REGION` | 默认 `us-east-1` |
| `WITTYCHAT_S3_ACCESS_KEY` / `WITTYCHAT_S3_SECRET_KEY` | 凭据 |
| `WITTYCHAT_S3_PREFIX` | key 前缀，默认 `wittychat/` |
| `WITTYCHAT_S3_PUBLIC_ENDPOINT` | 可选：客户端可访问的预签名地址。SigV4 对 Host 签名——该端点必须带原始 Host 转发到 S3，否则签名失效。docker 部署中内部端点客户端不可达时用它 |

分片上传的客户端直 PUT 到桶，要求桶 CORS：

```json
{"CORSRules":[{"AllowedOrigins":["http://wails.localhost","*"],
 "AllowedMethods":["PUT","GET","HEAD"],"AllowedHeaders":["*"],
 "ExposeHeaders":["ETag"]}]}
```

`ExposeHeaders: ETag` 必须存在——complete 需要每个分片的 ETag，读不到则上传失败。

- **dev/自托管用 Garage**（`dxflrs/garage` v2.x，见 `deploy/docker-compose.dev.yml` + `deploy/garage/`）。MinIO 社区版已归档——Docker Hub 镜像 2026-09 被删、Quay 需认证、最后镜像未打补丁，不再使用。
- **调度周期**（worker 内置，无外部调度器）：`gc` 每 24h（含过期分片中止）、`migrate_cold` 每 24h（仅启用 S3 时）、`verify_all` 每 30 天且只扫 `location='local'` 对象（S3 自做完整性校验，全量回读耗出口流量；本地盘位腐烂才是风险）。
- **设置**（管理后台 → 安全策略）：`storage.large_max_mb`（默认 2048，S3 分片单文件上限）、`storage.cold_days`（默认 90，0 = 关闭冷迁移）、`storage.upload_max_mb`（默认 5，直传上限）、`storage.quota_bytes`。
- 分片上传字节落在 `up/<object-id>` 专属 key（非按内容寻址）；冷迁移的对象按 `key(sha)` 寻址。

## 健康检查

- `GET /healthz`（server）→ 200
- worker：日志里 `task N (kind): done` 心跳；`bg_tasks` 表应有少量 pending
- 邮件：本域内部互发直达 inbox；外发看 `mail_send` 任务日志
- OnlyOffice：浏览器能开 `ONLYOFFICE_URL/web-apps/apps/api/documents/api.js`（返回 JS 即活）

## OnlyOffice 在线编辑

附件「在线编辑」→ `/office/open`（或 `GET /api/v1/objects/{id}/office-url`）校验登录+ACL → 302 到签名 `/office/edit` 页（15min 有效，每次打开复核账号状态与 ACL）→ 编辑器经 DocsAPI 拉 `/api/v1/office/file`（容器专用签名 URL，8h）→ 保存时文档服务 POST `/api/v1/office/callback`（JWT 校验 + key 绑定 obj + 复核写权限）→ 服务端回拉成稿并原位替换对象字节，所有引用（消息/邮件附件）即见新版。

| 环境变量 | 作用 |
|---|---|
| `WITTYCHAT_ONLYOFFICE_URL` | 浏览器可达的文档服务地址（api.js 加载源）；不设 = 功能关闭 |
| `WITTYCHAT_ONLYOFFICE_JWT_SECRET` | 与容器 `JWT_SECRET` 一致；编辑配置与回调都靠它。**必填**——未配置时 office 整体禁用（保存回调覆盖对象字节，公开默认密钥等于放伪造） |
| `WITTYCHAT_ONLYOFFICE_AUTODETECT` | `=1` 时在 URL 未设的情况下探测 `localhost:8180/healthcheck`（开发便利开关，默认关） |
| `WITTYCHAT_ONLYOFFICE_CALLBACK_BASE` | 文档容器回访本服务的基址。本地 Docker Desktop：`http://host.docker.internal:8080`（默认）；compose 同网：`http://server:8080` |
| `WITTYCHAT_ONLYOFFICE_INTERNAL_URL` | 本服务回拉成稿的基址。默认同 `ONLYOFFICE_URL`；compose 内：`http://onlyoffice` |

- 本地开发：先 `python scripts/gen_dev_env.py` 生成 gitignored `deploy/dev.env`，再 `docker compose --env-file deploy/dev.env -f deploy/docker-compose.dev.yml up -d onlyoffice`；服务端 `source deploy/dev.env` 后会拿到相同的 `WITTYCHAT_ONLYOFFICE_JWT_SECRET`（或以 `WITTYCHAT_ONLYOFFICE_AUTODETECT=1` 代替 URL 显式指定）
- 文档 key 随字节 sha 滚动——保存后下次打开必拉新稿；同 key 多人打开 = 协同编辑
- 编辑保存保留上一版到 `object_versions`（审计保留期可还原/审计）；超限版本由 worker 定期清理

## 邮件搬家（IMAP）

管理员在管理后台「邮件搬家」页配置 IMAP 导入任务，用于从飞书等邮箱迁移历史邮件。

### 前置条件（飞书）
1. 飞书管理后台 → 邮箱 → 邮箱工具 → 第三方邮箱客户端：开启
2. 员工在飞书桌面端 设置 → 邮箱 → 第三方邮箱客户端登录：生成专用密码（非登录密码）

### CSV 账号上传
格式：`source_email,password,target_email`（可带表头行；重复 source_email 覆盖密码）。

### MX 切换 runbook
5. 开始搬历史邮件 → 6. 将域名 MX 切到 WittyChat → 7. 再跑一次增量补齐切换期间的邮件 → 8. 完成并清除凭据。

### 安全
- 密码用 `data/secret.key` 加密入库（`sealed_secret`）；worker 需读同一文件。
- 「完成并清除凭据」或任务闲置 30 天 → 密码 NULL 化，不可再导入。
- `tls_mode=plain` 仅允许 loopback 主机（开发/测试）。

### 并发
`WITTYCHAT_IMPORT_CONCURRENCY`（默认 2，范围 1–8）控制 `mail_import` 任务泳道，与缩略图/发信分离，避免头阻塞。

### 失败台账
单封失败记入 `mail_import_failures`（uidvalidity+uid 主键、attempts、error），`last_uid` 照常前进；下次运行先重试台账行，`msgs_failed` 按活跃行数重算。详见任务详情的失败列表。

## 飞书整体迁移 / Feishu whole-tenant migration

后台「飞书迁移」页一键完成：同步飞书通讯录 → 为缺账号的员工建号并由飞书机器人私信激活链接 →
用应用凭据给每个人搬邮件（不需要员工密码）→ 导入应用可见的全部知识库 → 机器人通知每位员工
在桌面端「一键导入我的飞书文档」。之后持续增量同步邮件，直到管理员点「完成迁移」。

**管理员准备（一次）**
1. 飞书开放平台创建企业自建应用，添加机器人能力，可用范围设为全部成员。
2. 开通权限并发布版本：
   - 通讯录（权限范围：全部成员）：`contact:contact.base:readonly`、`contact:user.base:readonly`、`contact:user.email:readonly`
   - 邮箱（邮件数据权限范围：全部成员）：`mail:user_mailbox.message:readonly`、`mail:user_mailbox.message.body:read`、
     `mail:user_mailbox.message.subject:read`、`mail:user_mailbox.message.address:read`、`mail:user_mailbox.folder:read`
   - 云文档：`docs:document:export`、`drive:drive:readonly`、`wiki:wiki:readonly`；要搬的知识库把应用加为成员
   - 机器人：`im:message:send_as_bot`
3. 安全设置 → 重定向 URL：添加后台显示的回调地址（`$WITTYCHAT_PUBLIC_URL/api/v1/feishu/oauth/callback`）。
   `WITTYCHAT_PUBLIC_URL` 必须是员工浏览器能打开的地址，激活链接也用它。
4. 后台「飞书迁移」填 App ID / Secret（保存时向飞书验证；Secret 用 `data/secret.key` 加密存库，worker 需同一份密钥）。

**运行**
1. 「预览人员」：同步通讯录，看匹配情况（按企业邮箱匹配已有账号，大小写不敏感，含邮件别名）。
2. 「开始迁移」：后台 worker 执行全部步骤；页面每 5 秒刷新，逐人显示账号、激活链接、邮件进度和个人文档授权。
3. 任何时候把域名 MX 切到 WittyChat；持续同步每 30 分钟补一次新邮件，每天刷新一次通讯录（新入职自动建号、搬邮件）。
4. 确认无误后点「完成迁移」：每人再跑一轮全量补齐，跑完自动停止同步。

**邮件细节**
- 飞书 `batch_get` 返回原始 MIME 时走与入站邮件相同的解析并保存原文；否则按字段重建（主题、收发件人、正文、附件、
  Message-ID / 回复链），原始邮件头不保留。
- 文件夹：收件箱、已发送、垃圾、归档按系统文件夹对应，草稿进草稿箱，自定义文件夹按层级建成 `项目/2024` 这类文件夹；
  未读、星标保留。
- 增量轮次遇到整页都已导入就停止该文件夹；首轮和「完成迁移」那一轮走全量。单封失败进台账，下一轮先重试。
- 应用没有某人邮箱的数据权限时，该人显示"应用无权读取该邮箱"，补权限后在人员表点「重搬邮件」。

**个人文档（员工一键）**
- 应用读不到员工「我的空间」，所以需要员工点一次「一键导入我的飞书文档」，在浏览器里同意授权。
- 飞书账号邮箱必须与 WittyChat 账号一致，否则拒绝；服务端只保存加密的刷新令牌，以员工身份导入
  「我的空间」「我的文档库」里**本人是所有者**的文档和表格；别人放进来的显示"不是你的文档"。
- 再次授权会沿用原任务，已导入的不重复。也可以「按链接导入」单篇文档或文件夹。

**限制**
- 幻灯片、思维笔记、白板等列为"不支持的类型"；超大附件走下载链接（飞书限 1 次/秒）。
- 开发联调：`go run ./scripts/devfeishu`（127.0.0.1:1180 的模拟飞书），后台把 base_url 指向它
  （仅回环地址允许非官方 base_url）。
