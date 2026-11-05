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

- 本地开发：`docker compose -f deploy/docker-compose.dev.yml up -d onlyoffice`，服务端加 `WITTYCHAT_ONLYOFFICE_URL=http://localhost:8180 WITTYCHAT_ONLYOFFICE_JWT_SECRET=wittychat-office-dev`（或以 `WITTYCHAT_ONLYOFFICE_AUTODETECT=1` 代替 URL 显式指定）
- 文档 key 随字节 sha 滚动——保存后下次打开必拉新稿；同 key 多人打开 = 协同编辑
- 编辑保存保留上一版到 `object_versions`（审计保留期可还原/审计）；超限版本由 worker 定期清理
