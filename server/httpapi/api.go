// Package httpapi wires the HTTP API. huma generates OpenAPI from the
// registered operations, so the public contract is always in sync with code.
//
// httpapi 包装配 HTTP API。huma 从注册的接口生成 OpenAPI，
// 对外契约与代码天然同步。
package httpapi

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"wittychat/core/agents"
	"wittychat/core/audit"
	"wittychat/core/auth"
	"wittychat/core/docs"
	"wittychat/core/hr"
	"wittychat/core/im"
	"wittychat/core/mail"
	"wittychat/core/objects"
	"wittychat/core/settings"
	"wittychat/server/realtime"
	"wittychat/shared/apitypes"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Deps carries everything handlers need.
type Deps struct {
	Pool        *pgxpool.Pool
	Store       *auth.Store
	Audit       *audit.Emitter
	Limiter     *auth.Limiter // per email+IP throttle / 按账号+IP
	AcctLimiter *auth.Limiter // per-account throttle / 按账号（IP 轮换防不住）
	SecretKey   []byte
	Issuer      string // TOTP issuer / 工作区名
	PublicURL   string // e.g. https://im.example.com — used to build activation URLs
	TrustProxy  bool   // WITTYCHAT_TRUST_PROXY: honor the rightmost XFF hop / 信任反代追加的 XFF
	IM          *im.Store
	Hub         *realtime.Hub
	Objects     *objects.Store
	Settings    *settings.Store
	Signer      *objects.Signer // short-lived download URLs / 签名下载URL
	Mail        *mail.Store
	Agents      *agents.Store
	HR          *hr.Store
	Docs        *docs.Store
	Office      *OfficeConfig // nil → OnlyOffice routes off / 未配置则不挂编辑路由
}

// New builds the root handler: API routes + HTML pages + healthz.
func New(d *Deps) http.Handler {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("WittyChat API", "0.1.0"))

	registerAuth(api, d)
	registerAdmin(api, d)
	registerIM(api, d)
	registerIM2(api, d)
	registerPrefs(api, d)
	registerHR(api, d)
	registerProfile(api, mux, d)
	registerPages(mux, d)
	registerWS(mux, d)
	registerObjects(mux, d)
	registerOffice(mux, d)
	registerDocs(mux, d)
	registerMail(mux, d)
	registerAgents(mux, d)
	mux.HandleFunc("GET /api/v1/admin/audit/export", d.adminAuditExport)

	// Typing fan-out: hub asks im for the member list lazily.
	// 输入中扇出：hub 按需向 im 要成员列表。
	if d.Hub != nil && d.IM != nil {
		d.Hub.SetTypingSink(func(convID, from uuid.UUID, name string) {
			ids, err := d.IM.MemberIDs(context.Background(), convID)
			if err == nil {
				d.Hub.Typing(ids, convID, from, name)
			}
		})
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return cors(securityHeaders(realIPMiddleware(d.TrustProxy, mux)))
}

// cors allows cross-origin API access for trusted client shells only —
// the Wails desktop app serves assets from http(s)://wails.localhost /
// wails://wails.localhost, so its fetch to the backend is cross-origin.
// Extra origins via WITTYCHAT_CORS_ORIGINS (comma-separated). Bearer-token
// auth means cookies are never involved, so ACAO reflection is safe.
// CORS：仅放行受信客户端壳（桌面 App 的资源源是 wails.localhost，
// 跨域 fetch 需要 ACAO）；其余来源不带 CORS 头，浏览器继续拦截。
func cors(next http.Handler) http.Handler {
	allowed := map[string]bool{
		"http://wails.localhost":     true,
		"https://wails.localhost":    true,
		"wails://wails.localhost":    true,
		"tauri://localhost":          true,
		"http://tauri.localhost":     true,
		"http://localhost:9245":      true,
		"https://localhost:9245":     true,
		"capacitor://localhost":      true,
		"http://capacitor.localhost": true,
	}
	for _, o := range strings.Split(os.Getenv("WITTYCHAT_CORS_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || !allowed[origin] {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- helpers / 辅助 ----------

func bearerToken(h string) (string, bool) {
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return "", false
	}
	t := strings.TrimSpace(h[len(p):])
	return t, t != ""
}

// The real client IP is resolved ONCE per request in realIPMiddleware and
// carried in the context. Trust rules: without a trusted proxy the TCP peer
// is authoritative; behind our own proxy (WITTYCHAT_TRUST_PROXY) only the
// RIGHTMOST X-Forwarded-For hop counts — proxies append, so the last entry
// is the one our trusted proxy saw and the client cannot forge it. Using
// the leftmost entry let attackers rotate IPs past the login lockout.
// 真实客户端 IP 每请求只解析一次（realIPMiddleware）入 context。信任规则：
// 无受信代理时以 TCP 对端为准；在自有代理后只认 XFF 最右一跳——代理是追加
// 语义，最右项是可信代理亲眼所见，客户端伪造不了。此前取最左项，攻击者轮换
// IP 即可绕过登录锁定。
type ctxKeyIP struct{}

func realIPMiddleware(trustProxy bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := remoteIP(r)
		if trustProxy {
			if x := rightmostXFF(r.Header.Get("X-Forwarded-For")); x != nil {
				ip = x
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyIP{}, ip)))
	})
}

func remoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

func rightmostXFF(xff string) net.IP {
	if xff == "" {
		return nil
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		if ip := net.ParseIP(strings.TrimSpace(parts[i])); ip != nil {
			return ip
		}
	}
	return nil
}

// clientIPCtx returns the resolved client IP (nil outside the middleware).
func clientIPCtx(ctx context.Context) net.IP {
	ip, _ := ctx.Value(ctxKeyIP{}).(net.IP)
	return ip
}

// requireUser validates the bearer token → session + active user.
func (d *Deps) requireUser(ctx context.Context, header string) (*auth.Session, *auth.User, error) {
	tok, ok := bearerToken(header)
	if !ok {
		return nil, nil, huma.Error401Unauthorized("missing bearer token")
	}
	sess, err := d.Store.ValidateSession(ctx, tok)
	if err != nil {
		return nil, nil, huma.Error401Unauthorized("invalid or expired session")
	}
	u, err := d.Store.UserByID(ctx, sess.UserID)
	if err != nil {
		return nil, nil, huma.Error401Unauthorized("invalid session")
	}
	return sess, u, nil
}

// requireAdmin additionally demands workspace-admin role.
func (d *Deps) requireAdmin(ctx context.Context, header string) (*auth.Session, *auth.User, error) {
	sess, u, err := d.requireUser(ctx, header)
	if err != nil {
		return nil, nil, err
	}
	if !u.IsAdmin {
		return nil, nil, huma.Error403Forbidden("admin role required")
	}
	return sess, u, nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func publicUser(u *auth.User) *apitypes.UserPublic {
	p := &apitypes.UserPublic{
		ID:          u.ID.String(),
		Email:       u.Email,
		DisplayName: u.DisplayName,
		Status:      u.Status,
		IsAdmin:     u.IsAdmin,
		TOTPEnabled: u.TOTPEnabled,
		CreatedAt:   u.CreatedAt,
	}
	if u.AvatarObjectID != nil {
		p.AvatarObjectID = u.AvatarObjectID.String()
	}
	return p
}
