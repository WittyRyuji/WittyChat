# ASVS L2 走查 (OWASP Application Security Verification Standard — Level 2)

P0 范围内的自查记录；每项标注实现位置。

## V2 认证 (Authentication)

- [x] Argon2id 密码散列（PHC 格式，内存/迭代参数内嵌）— `core/auth/password.go`
- [x] 登录限速：同IP+账号硬锁、按账号渐进延迟、原子预留防并发竞态、Argon2并发上限 — `core/auth/limiter.go`
- [x] TOTP（RFC 6238），secret 用 AES-256-GCM 加密入库 — `core/auth/totp.go`
- [x] 激活链接一次性、72h 过期、哈希存储 — `core/auth/store.go`
- [x] 会话 30 天闲置过期、改密全踢（cred_version）— `core/auth/sessions.go`
- [x] 密码最低 15 字符（可后台配置） — `core/auth`

## V3 会话管理 (Session)

- [x] Bearer token 高熵随机（32B）、哈希入库不存明文 — `core/auth`
- [x] 登出即吊销 — `POST /auth/logout`
- [x] 设备会话列表可查可踢 — `GET /auth/sessions`
- [x] WS 握手带 token，断开即清 — `server/realtime`

## V4 访问控制 (Access Control)

- [x] 会话成员才能读写消息/下载附件 — `core/im` `core/objects.Get`
- [x] 管理员接口 requireAdmin 中间件 — `server/httpapi/admin.go`
- [x] 对象属主或会话成员 ACL；签名 URL 作为持有即授权（时效+签名）
- [x] 群角色 owner/admin/member 权限矩阵 — `core/im/group.go`

## V5 输入验证 (Validation)

- [x] huma 入参 schema 校验（枚举/长度/UUID 格式） — `server/httpapi`
- [x] 上传大小上限（默认 5MB，可配置 `upload_max_mb`）+ 流式不落内存
- [x] sha256 强校验 dedup，multipart complete 校验 size
- [x] 邮件主题/正文长度、地址格式 — `core/mail`

## V7 错误处理与日志 (Error Handling & Logging)

- [x] 审计事件仅追加表 `audit_events`，管理操作全覆盖（建用户/停用/交接/助理增删改）
- [x] 管理后台查看审计自带审计（自审计）
- [x] 前端不暴露内部错误细节；服务端日志无敏感值

## V8 数据保护 (Data Protection)

- [x] TOTP secret AES-256-GCM 加密存储
- [x] 传输全走 TLS（生产 nginx 443 only，http 强制跳转）
- [x] 附件文件名 RFC5987 编码，下载带 Content-Disposition
- [x] Markdown 渲染 DOMPurify 白名单消毒 — `desktop/frontend/src/md.tsx`

## V9 通信安全 (Communications)

- [x] 生产仅 TLS1.2+，安全 cipher 套件 — `deploy/nginx.conf`
- [x] SMTP 出站 STARTTLS（对端支持时升级） — `core/mail/smtp.go`
- [x] WS 生产环境 wss:// — nginx `/ws` upgrade 代理

## V10 恶意代码 (Malicious Software)

- [x] AV 扫描接口 `objects.Scanner`（NoopScanner 默认，可插 clamd）— `core/objects/maint.go`
- [x] 检出即隔离（status=deleted + 文件移除）+ 日志
- [x] 前端 DOMPurify 防 XSS（见 V8）

## V12 文件与资源 (Files & Resources)

- [x] 上传文件名仅作元数据，存储按 sha256 寻址（无路径穿越面）
- [x] 下载统一走 ACL 检查；签名 URL 15min 有效
- [x] 缩略图服务端生成，不信任客户端提交的图片

## V13 API 安全 (API)

- [x] OpenAPI 由 huma 生成，契约即代码
- [x] JSON 全边界；multipart 仅上传端点
- [x] WS 仅推送轻量提醒，数据仍走鉴权 REST

## 已知待办（P0 后）

- [ ] CSP/完整安全响应头矩阵（当前 X-Content-Type-Options/X-Frame-Options/Referrer-Policy）
- [ ] 真实 AV 引擎接入（接口已冻结）
- [ ] OIDC/SSO 外部身份源
- [ ] mTLS（如需内网组件隔离）
