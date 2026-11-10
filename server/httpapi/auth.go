package httpapi

// Auth + session + TOTP endpoints.
// 认证、设备会话、二次验证接口。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
	"wittychat/core/audit"
	"wittychat/core/auth"
	"wittychat/core/settings"
	"wittychat/shared/apitypes"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

func registerAuth(api huma.API, d *Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "login", Method: http.MethodPost,
		Path: "/api/v1/auth/login", Summary: "Log in with email + password / 邮箱密码登录",
		Tags: []string{"auth"},
	}, d.login)

	huma.Register(api, huma.Operation{
		OperationID: "logout", Method: http.MethodPost,
		Path: "/api/v1/auth/logout", Summary: "Log out (revoke this session) / 登出（吊销本会话）",
		Tags: []string{"auth"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.logout)

	huma.Register(api, huma.Operation{
		OperationID: "me", Method: http.MethodGet,
		Path: "/api/v1/auth/me", Summary: "Current user + session / 当前用户与会话",
		Tags: []string{"auth"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.me)

	huma.Register(api, huma.Operation{
		OperationID: "activate-info", Method: http.MethodGet,
		Path: "/api/v1/auth/activate", Summary: "Peek an activation/reset link / 预览激活或重置链接",
		Tags: []string{"auth"},
	}, d.activateInfo)

	huma.Register(api, huma.Operation{
		OperationID: "activate", Method: http.MethodPost,
		Path: "/api/v1/auth/activate", Summary: "Consume link, set password / 消费链接并设密码",
		Tags: []string{"auth"},
	}, d.activate)

	huma.Register(api, huma.Operation{
		OperationID: "list-sessions", Method: http.MethodGet,
		Path: "/api/v1/sessions", Summary: "My device sessions / 我的设备会话",
		Tags: []string{"auth"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.listSessions)

	huma.Register(api, huma.Operation{
		OperationID: "revoke-session", Method: http.MethodDelete,
		Path: "/api/v1/sessions/{id}", Summary: "Kick a device / 踢掉某台设备",
		Tags: []string{"auth"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.revokeSession)

	huma.Register(api, huma.Operation{
		OperationID: "totp-enroll", Method: http.MethodPost,
		Path: "/api/v1/auth/totp/enroll", Summary: "Start TOTP enrollment / 开始绑定 TOTP",
		Tags: []string{"auth"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.totpEnroll)

	huma.Register(api, huma.Operation{
		OperationID: "totp-confirm", Method: http.MethodPost,
		Path: "/api/v1/auth/totp/confirm", Summary: "Confirm TOTP code → enable / 校验首个验证码并启用",
		Tags: []string{"auth"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.totpConfirm)

	huma.Register(api, huma.Operation{
		OperationID: "totp-disable", Method: http.MethodPost,
		Path: "/api/v1/auth/totp/disable", Summary: "Disable TOTP (password required) / 关闭 TOTP（需密码）",
		Tags: []string{"auth"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.totpDisable)
}

// ---------- handlers ----------

type Headers struct {
	Authorization string `header:"Authorization"`
	XForwardedFor string `header:"X-Forwarded-For"`
	Cookie        string `header:"Cookie"`
	XWCDevice     string `header:"X-WC-Device"`
}

// loginOut carries the Set-Cookie for the known-device marker.
// loginOut 额外带 Set-Cookie 写"已知设备"标记。
type loginOut struct {
	Body      apitypes.LoginResponse
	SetCookie *string `header:"Set-Cookie"`
}

// dummyPHC is a valid argon2id string (same cost params) verified against
// when the email doesn't exist — equalizing response timing so account
// enumeration can't ride the fast-path. 有效格式的 argon2id 串（同成本
// 参数），邮箱不存在时拿它做校验——抹平时序，账号枚举蹭不到快路径。
const dummyPHC = "$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0c2FsdHNhbHQ$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// limitIPKey normalizes a client IP for throttle keys: IPv6 collapses to
// its /64 — providers hand whole prefixes, so per-address keys rotate
// for free without it. 限速键的 IP 归一化：IPv6 归并到 /64——运营商
// 按前缀分配，不合并的话换地址零成本绕过。
func limitIPKey(ip net.IP) string {
	if ip == nil {
		return "?"
	}
	if ip.To4() == nil {
		if v := ip.To16(); v != nil {
			return "net6:" + v.Mask(net.CIDRMask(64, 128)).String()
		}
	}
	return ip.String()
}

// kdFromRequest reads the device marker from the X-WC-Device header
// (desktop/API clients) or the wc_kd cookie (web). 读取设备标记：
// 桌面/API 走请求头，网页走 cookie。
func kdFromRequest(in *struct {
	Headers
	Body apitypes.LoginRequest
},
) string {
	if in.XWCDevice != "" {
		return in.XWCDevice
	}
	for _, part := range strings.Split(in.Cookie, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(part), "wc_kd="); ok {
			return v
		}
	}
	return ""
}

func (d *Deps) login(ctx context.Context, in *struct {
	Headers
	Body apitypes.LoginRequest
},
) (*loginOut, error) {
	ip := clientIPCtx(ctx)
	// Throttle on two keys: email+IP (stops one source hammering one
	// account — per-IP-per-account hard limit, IPv6 folded to /64) and
	// email alone (distributed attempts at one account — a rotated XFF
	// can't dodge it). 双键限速：账号+来源防单源刷猜（硬锁，IPv6 按
	// /64 归并），仅账号防分布式打同一账号（换 IP 绕不过）。
	email := strings.ToLower(strings.TrimSpace(in.Body.Email))
	key := email + "|" + limitIPKey(ip)
	acctKey := email + "|*"

	// Policy from workspace settings so admin changes take effect without
	// a restart. The account window is long — the old 5-minute reset let
	// a sprayer keep a steady ~37 guesses/5min forever; a day-long decay
	// makes sustained spraying progressively slower instead.
	// 每次登录读取 security.lock_* 设置，改动即时生效。账号窗口拉长——
	// 原先 5 分钟清零让喷洒者永远保持 ~37 次/5 分的稳定速率；按天衰减
	// 让持续喷洒越喷越慢。
	th := int(d.Settings.Int(ctx, settings.KeySecurityLockN, settings.DefLockThreshold))
	lm := time.Duration(d.Settings.Int(ctx, settings.KeySecurityLockMin, settings.DefLockMinutes)) * time.Minute
	d.Limiter.Configure(th, 5*time.Minute, lm)
	d.AcctLimiter.Configure(th*2, 24*time.Hour, lm)

	// Atomic check+count — always, known device or not: the device marker
	// only skips the ACCOUNT queue, never the per-source hard lock, so a
	// leaked marker can't multiply guesses beyond what one source could
	// do anyway. / 原子检查+计数——无论是否已知设备：设备标记只豁免账号
	// 排队，从不豁免按来源硬锁——标记泄露也不能放大单源可猜次数。
	if !d.Limiter.Attempt(key) {
		return nil, huma.Error429TooManyRequests("too many failed attempts, try later / 失败过多，稍后再试")
	}

	u, err := d.Store.UserByEmail(ctx, email)
	if err != nil {
		d.AcctLimiter.Fail(acctKey)
		_, _ = d.verifyPassword(ctx, in.Body.Password, dummyPHC) // timing parity
		return nil, huma.Error401Unauthorized("invalid credentials")
	}
	hash, totpEnabled, credVersion, err := d.Store.PasswordAuth(ctx, u.ID)
	if err != nil {
		return nil, err
	}

	// A previously-issued device token skips the account queue (and only
	// that). 已签发的设备令牌只跳过账号排队。
	if !d.Store.KnownDevice(ctx, u.ID, kdFromRequest(in), credVersion) {
		wait, ok := d.AcctLimiter.Schedule(acctKey, 20*time.Second)
		if !ok {
			return nil, huma.Error429TooManyRequests("too many failed attempts, try later / 失败过多，稍后再试")
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
	}

	ok, err := d.verifyPassword(ctx, in.Body.Password, hash)
	if err != nil {
		return nil, err
	}
	if !ok {
		d.AcctLimiter.Fail(acctKey)
		d.Audit.Log(ctx, &u.ID, audit.ActorUser, "auth.login.fail", u.Email, nil, ip)
		return nil, huma.Error401Unauthorized("invalid credentials")
	}
	// Status checked only after the password verifies — answering 403
	// up front let anyone probe account existence/state without the
	// password. / 账号状态放到密码校验之后才查——前置 403 让任何人
	// 不用密码就能探测账号是否存在/已激活。
	if u.Status != auth.StatusActive {
		return nil, huma.Error403Forbidden("account not active / 账号未激活或已停用")
	}
	if totpEnabled {
		if in.Body.TOTP == "" {
			return &loginOut{
				Body: apitypes.LoginResponse{RequiresTOTP: true},
			}, nil
		}
		if !d.Store.TOTPVerify(ctx, u.ID, in.Body.TOTP, d.SecretKey) {
			d.AcctLimiter.Fail(acctKey)
			return nil, huma.Error401Unauthorized("invalid totp code")
		}
	}

	token, sess, err := d.Store.CreateSession(ctx, u.ID, in.Body.DeviceName, in.Body.DeviceKind, credVersion)
	if err != nil {
		return nil, err
	}
	// Reset only this ip+email pair — a success proves THIS source. The
	// account key keeps its failure count: an owner's login must not
	// hand a distributed sprayer a fresh counter every time.
	// 只清这个 ip+账号键——成功只证明本来源可信。账号键的失败计数保留：
	// 属主登录不能把分布式攻击攒的计数清零。
	d.Limiter.Reset(key)
	d.Audit.Log(ctx, &u.ID, audit.ActorUser, "auth.login", u.Email, nil, ip)

	devTok, _ := d.Store.IssueDevice(ctx, u.ID, credVersion)
	var setCookie *string
	if devTok != "" {
		c := "wc_kd=" + devTok + "; Path=/; HttpOnly; SameSite=Lax; Max-Age=7776000"
		if strings.HasPrefix(d.PublicURL, "https://") {
			c += "; Secure"
		}
		setCookie = &c
	}
	return &loginOut{
		Body: apitypes.LoginResponse{
			SessionToken: token,
			DeviceToken:  devTok,
			User:         publicUser(u),
			ExpiresAt:    sess.ExpiresAt,
		},
		SetCookie: setCookie,
	}, nil
}

func (d *Deps) logout(ctx context.Context, in *struct {
	Headers
},
) (*struct{}, error) {
	sess, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	_ = d.Store.RevokeSession(ctx, u.ID, sess.ID)
	d.docRevalidateUser(ctx, u.ID)
	d.Hub.KickSession(u.ID, sess.ID) // this device only / 仅断开本设备连接
	d.Audit.Log(ctx, &u.ID, audit.ActorUser, "auth.logout", u.Email, nil, clientIPCtx(ctx))
	return &struct{}{}, nil
}

func (d *Deps) me(ctx context.Context, in *struct {
	Headers
},
) (*struct{ Body apitypes.LoginResponse }, error) {
	sess, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	return &struct{ Body apitypes.LoginResponse }{
		Body: apitypes.LoginResponse{User: publicUser(u), ExpiresAt: sess.ExpiresAt},
	}, nil
}

func (d *Deps) activateInfo(ctx context.Context, in *struct {
	Token string `query:"token" required:"true"`
},
) (*struct{ Body apitypes.ActivateInfoResponse }, error) {
	email, name, expiresAt, _, err := d.Store.PeekLink(ctx, in.Token)
	if err != nil {
		return nil, huma.Error400BadRequest("link invalid or expired / 链接无效或已过期")
	}
	return &struct{ Body apitypes.ActivateInfoResponse }{
		Body: apitypes.ActivateInfoResponse{Email: email, DisplayName: name, ExpiresAt: expiresAt},
	}, nil
}

func (d *Deps) activate(ctx context.Context, in *struct {
	Headers
	Body apitypes.ActivateRequest
},
) (*struct{ Body apitypes.LoginResponse }, error) {
	userID, kind, err := d.Store.ConsumeLink(ctx, in.Body.Token)
	if err != nil {
		return nil, huma.Error400BadRequest("link invalid or expired / 链接无效或已过期")
	}
	// Workspace password floor (security.min_password, NIST default 15).
	// 工作区密码下限（security.min_password，NIST 默认 15）。
	if min := int(d.Settings.Int(ctx, settings.KeySecurityMinPass, settings.DefMinPassword)); utf8.RuneCountInString(in.Body.Password) < min {
		return nil, huma.Error400BadRequest(fmt.Sprintf("password too short: at least %d chars / 密码至少 %d 位", min, min))
	}
	phc, err := d.hashPassword(ctx, in.Body.Password)
	if err != nil {
		return nil, err
	}
	if err := d.Store.SetPassword(ctx, userID, phc); err != nil {
		return nil, err
	}
	d.docRevalidateUser(ctx, userID) // reset link bumps cred version / 重置会作废旧会话
	d.Hub.KickUser(userID)
	if kind == auth.LinkKindActivation {
		if err := d.Store.ActivateUser(ctx, userID); err != nil {
			return nil, err
		}
	}
	u, err := d.Store.UserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	d.Audit.Log(ctx, &u.ID, audit.ActorUser, "auth.link.used:"+kind, u.Email, nil, clientIPCtx(ctx))

	// Auto-login after activation/reset — one less step.
	// 激活/重置后直接发会话，少一步登录。
	// NOTE: must use the NEW credential version — SetPassword just bumped it.
	// 必须用最新的凭证版本——SetPassword 刚把它提升了。
	_, _, credVersion, err := d.Store.PasswordAuth(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	token, sess, err := d.Store.CreateSession(ctx, u.ID, "activation", "web", credVersion)
	if err != nil {
		return nil, err
	}
	return &struct{ Body apitypes.LoginResponse }{
		Body: apitypes.LoginResponse{
			SessionToken: token,
			User:         publicUser(u),
			ExpiresAt:    sess.ExpiresAt,
		},
	}, nil
}

func (d *Deps) listSessions(ctx context.Context, in *struct {
	Headers
},
) (*struct{ Body apitypes.SessionListResponse }, error) {
	sess, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	list, err := d.Store.ListSessions(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	out := &struct{ Body apitypes.SessionListResponse }{
		Body: apitypes.SessionListResponse{Sessions: make([]*apitypes.SessionInfo, 0, len(list))},
	}
	for _, s0 := range list {
		out.Body.Sessions = append(out.Body.Sessions, &apitypes.SessionInfo{
			ID: s0.ID.String(), DeviceName: s0.DeviceName, DeviceKind: s0.DeviceKind,
			LastSeenAt: s0.LastSeenAt, CreatedAt: s0.CreatedAt, Current: s0.ID == sess.ID,
		})
	}
	return out, nil
}

func (d *Deps) revokeSession(ctx context.Context, in *struct {
	Headers
	ID string `path:"id" format:"uuid"`
},
) (*struct{}, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	sid, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error400BadRequest("bad session id")
	}
	if err := d.Store.RevokeSession(ctx, u.ID, sid); err != nil {
		if errors.Is(err, auth.ErrSessionInvalid) {
			return nil, huma.Error404NotFound("session not found")
		}
		return nil, err
	}
	d.docRevalidateUser(ctx, u.ID)
	d.Hub.KickSession(u.ID, sid)
	d.Audit.Log(ctx, &u.ID, audit.ActorUser, "auth.session.revoke", in.ID, nil, clientIPCtx(ctx))
	return &struct{}{}, nil
}

func (d *Deps) totpEnroll(ctx context.Context, in *struct {
	Headers
},
) (*struct{ Body apitypes.TOTPEnrollResponse }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	uri, err := d.Store.TOTPEnroll(ctx, u.ID, u.Email, d.Issuer, d.SecretKey)
	if err != nil {
		return nil, err
	}
	return &struct{ Body apitypes.TOTPEnrollResponse }{
		Body: apitypes.TOTPEnrollResponse{SecretURI: uri},
	}, nil
}

func (d *Deps) totpConfirm(ctx context.Context, in *struct {
	Headers
	Body apitypes.TOTPConfirmRequest
},
) (*struct{}, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	if err := d.Store.TOTPConfirm(ctx, u.ID, in.Body.Code, d.SecretKey); err != nil {
		if errors.Is(err, auth.ErrTOTPBadCode) {
			return nil, huma.Error400BadRequest("invalid code / 验证码错误")
		}
		if errors.Is(err, auth.ErrTOTPNotEnrolled) {
			return nil, huma.Error400BadRequest("enroll first / 请先绑定")
		}
		return nil, err
	}
	d.Audit.Log(ctx, &u.ID, audit.ActorUser, "auth.totp.enable", u.Email, nil, clientIPCtx(ctx))
	return &struct{}{}, nil
}

func (d *Deps) totpDisable(ctx context.Context, in *struct {
	Headers
	Body apitypes.TOTPDisableRequest
},
) (*struct{}, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	hash, _, _, err := d.Store.PasswordAuth(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	ok, err := d.verifyPassword(ctx, in.Body.Password, hash)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, huma.Error403Forbidden("wrong password / 密码错误")
	}
	if err := d.Store.TOTPDisable(ctx, u.ID); err != nil {
		return nil, err
	}
	d.Audit.Log(ctx, &u.ID, audit.ActorUser, "auth.totp.disable", u.Email, nil, clientIPCtx(ctx))
	return &struct{}{}, nil
}
