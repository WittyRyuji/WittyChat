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
