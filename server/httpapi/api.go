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
	"strings"
	"wittychat/core/audit"
	"wittychat/core/auth"
	"wittychat/core/im"
	"wittychat/server/realtime"
	"wittychat/shared/apitypes"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Deps carries everything handlers need.
type Deps struct {
	Pool      *pgxpool.Pool
	Store     *auth.Store
	Audit     *audit.Emitter
	Limiter   *auth.Limiter
	SecretKey []byte
	Issuer    string // TOTP issuer / 工作区名
	PublicURL string // e.g. https://im.example.com — used to build activation URLs
	IM        *im.Store
	Hub       *realtime.Hub
}

// New builds the root handler: API routes + HTML pages + healthz.
func New(d *Deps) http.Handler {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("WittyChat API", "0.1.0"))

	registerAuth(api, d)
	registerAdmin(api, d)
	registerIM(api, d)
	registerPages(mux, d)
	registerWS(mux, d)

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
	return securityHeaders(mux)
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

func clientIP(xff string) net.IP {
	if xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		return net.ParseIP(strings.TrimSpace(xff))
	}
	return nil
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
	return &apitypes.UserPublic{
		ID:          u.ID.String(),
		Email:       u.Email,
		DisplayName: u.DisplayName,
		Status:      u.Status,
		IsAdmin:     u.IsAdmin,
		TOTPEnabled: u.TOTPEnabled,
		CreatedAt:   u.CreatedAt,
	}
}
