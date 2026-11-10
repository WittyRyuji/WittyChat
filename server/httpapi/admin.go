package httpapi

// Workspace-admin endpoints: provisioning, links, suspend/offboard/reactivate.
// 工作区管理员接口：开通账号、签发链接、停用/离职交接/恢复。

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
	"wittychat/core/audit"
	"wittychat/core/auth"
	"wittychat/core/settings"
	"wittychat/shared/apitypes"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

func registerAdmin(api huma.API, d *Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "admin-create-user", Method: http.MethodPost,
		Path: "/api/v1/admin/users", Summary: "Create user + activation link / 创建用户并签发激活链接",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.adminCreateUser)

	huma.Register(api, huma.Operation{
		OperationID: "admin-list-users", Method: http.MethodGet,
		Path: "/api/v1/admin/users", Summary: "List users / 用户列表",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.adminListUsers)

	huma.Register(api, huma.Operation{
		OperationID: "admin-new-link", Method: http.MethodPost,
		Path: "/api/v1/admin/users/{id}/links", Summary: "Issue activation/reset link / 签发激活或重置链接",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.adminNewLink)

	huma.Register(api, huma.Operation{
		OperationID: "admin-deactivate", Method: http.MethodPost,
		Path: "/api/v1/admin/users/{id}/deactivate", Summary: "Suspend user (sessions die) / 停用（会话全失效）",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.adminSetStatus(auth.StatusSuspended, "admin.user.suspend"))

	huma.Register(api, huma.Operation{
		OperationID: "admin-reactivate", Method: http.MethodPost,
		Path: "/api/v1/admin/users/{id}/reactivate", Summary: "Reactivate a suspended user / 恢复已停用用户",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.adminSetStatus(auth.StatusActive, "admin.user.reactivate"))

	huma.Register(api, huma.Operation{
		OperationID: "admin-offboard", Method: http.MethodPost,
		Path: "/api/v1/admin/users/{id}/offboard", Summary: "Offboard → data to Team Owner / 离职交接给 Admin",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.adminOffboard)

	huma.Register(api, huma.Operation{
		OperationID: "admin-audit-list", Method: http.MethodGet,
		Path: "/api/v1/admin/audit", Summary: "Audit log viewer (viewing is audited) / 审计查看（查看本身被审计）",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.adminAuditList)

	huma.Register(api, huma.Operation{
		OperationID: "admin-settings-get", Method: http.MethodGet,
		Path: "/api/v1/admin/settings", Summary: "All workspace settings / 全部工作区设置",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.adminSettingsGet)

	huma.Register(api, huma.Operation{
		OperationID: "admin-settings-set", Method: http.MethodPut,
		Path: "/api/v1/admin/settings", Summary: "Update workspace settings / 更新工作区设置",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.adminSettingsSet)
}

func (d *Deps) linkURL(token string) string {
	return d.PublicURL + "/activate?token=" + url.QueryEscape(token)
}

func (d *Deps) adminCreateUser(ctx context.Context, in *struct {
	Headers
	Body apitypes.CreateUserRequest
},
) (*struct{ Body apitypes.CreateUserResponse }, error) {
	_, admin, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	u, err := d.Store.CreateUser(ctx, in.Body.Email, in.Body.DisplayName, in.Body.IsAdmin)
	if err != nil {
		if errors.Is(err, auth.ErrEmailTaken) {
			return nil, huma.Error409Conflict("email already registered / 邮箱已注册")
		}
		return nil, err
	}
	// Auto-provision a primary mail address from the login email.
	// 自动配置主邮箱地址（登录邮箱即邮箱地址）。
	if d.Mail != nil {
		_ = d.Mail.AddAddress(ctx, u.ID, u.Email, "primary")
		_ = d.Mail.EnsureFolders(ctx, u.ID)
	}
	token, expiresAt, err := d.Store.CreateActivationLink(ctx, u.ID, auth.LinkKindActivation, admin.ID)
	if err != nil {
		return nil, err
	}
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "admin.user.create", u.Email, nil, clientIPCtx(ctx))
	return &struct{ Body apitypes.CreateUserResponse }{
		Body: apitypes.CreateUserResponse{
			User:          publicUser(u),
			ActivationURL: d.linkURL(token),
			ExpiresAt:     expiresAt,
		},
	}, nil
}

func (d *Deps) adminListUsers(ctx context.Context, in *struct {
	Headers
},
) (*struct{ Body apitypes.UserListResponse }, error) {
	if _, _, err := d.requireAdmin(ctx, in.Authorization); err != nil {
		return nil, err
	}
	users, err := d.Store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := &struct{ Body apitypes.UserListResponse }{
		Body: apitypes.UserListResponse{Users: make([]*apitypes.UserPublic, 0, len(users))},
	}
	for _, u := range users {
		out.Body.Users = append(out.Body.Users, publicUser(u))
	}
	return out, nil
}

func (d *Deps) adminNewLink(ctx context.Context, in *struct {
	Headers
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Kind string `json:"kind" enum:"activation,reset"`
	}
},
) (*struct{ Body apitypes.CreateUserResponse }, error) {
	_, admin, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	uid, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error400BadRequest("bad user id")
	}
	u, err := d.Store.UserByID(ctx, uid)
	if err != nil {
		return nil, huma.Error404NotFound("user not found")
	}
	kind := in.Body.Kind
	if kind == "" {
		kind = auth.LinkKindActivation
	}
	token, expiresAt, err := d.Store.CreateActivationLink(ctx, uid, kind, admin.ID)
	if err != nil {
		return nil, err
	}
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "admin.user.link:"+kind, u.Email, nil, clientIPCtx(ctx))
	return &struct{ Body apitypes.CreateUserResponse }{
		Body: apitypes.CreateUserResponse{
			User:          publicUser(u),
			ActivationURL: d.linkURL(token),
			ExpiresAt:     expiresAt,
		},
	}, nil
}

// adminSetStatus builds a handler for suspend/reactivate.
func (d *Deps) adminSetStatus(status, action string) func(ctx context.Context, in *struct {
	Headers
	ID string `path:"id" format:"uuid"`
}) (*struct{}, error) {
	return func(ctx context.Context, in *struct {
		Headers
		ID string `path:"id" format:"uuid"`
	},
	) (*struct{}, error) {
		_, admin, err := d.requireAdmin(ctx, in.Authorization)
		if err != nil {
			return nil, err
		}
		uid, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, huma.Error400BadRequest("bad user id")
		}
		target, err := d.Store.UserByID(ctx, uid)
		if err != nil {
			return nil, huma.Error404NotFound("user not found")
		}
		if target.ID == admin.ID && status != auth.StatusActive {
			return nil, huma.Error400BadRequest("cannot suspend yourself / 不能停用自己")
		}
		if err := d.Store.SetStatus(ctx, uid, status, nil); err != nil {
			return nil, err
		}
		d.docRevalidateUser(ctx, uid)
		d.Hub.KickUser(uid)
		d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, action, target.Email, nil, clientIPCtx(ctx))
		return &struct{}{}, nil
	}
}

func (d *Deps) adminOffboard(ctx context.Context, in *struct {
	Headers
	ID   string `path:"id" format:"uuid"`
	Body apitypes.OffboardRequest
},
) (*struct{}, error) {
	_, admin, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	uid, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error400BadRequest("bad user id")
	}
	to, err := uuid.Parse(in.Body.TransferTo)
	if err != nil {
		return nil, huma.Error400BadRequest("bad transfer_to id")
	}
	toUser, err := d.Store.UserByID(ctx, to)
	if err != nil || !toUser.IsAdmin {
		return nil, huma.Error400BadRequest("transfer target must be an admin / 交接对象必须是管理员")
	}
	target, err := d.Store.UserByID(ctx, uid)
	if err != nil {
		return nil, huma.Error404NotFound("user not found")
	}
	if err := d.Store.SetStatus(ctx, uid, auth.StatusOffboarded, &to); err != nil {
		return nil, err
	}
	d.docRevalidateUser(ctx, uid)
	d.Hub.KickUser(uid)
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "admin.user.offboard", target.Email,
		map[string]string{"transfer_to": toUser.Email}, clientIPCtx(ctx))
	return &struct{}{}, nil
}

// ---------- audit viewer + workspace settings ----------

func (d *Deps) adminAuditList(ctx context.Context, in *struct {
	Headers
	Action string `query:"action"`
	Actor  string `query:"actor"`
	Limit  int    `query:"limit"`
},
) (*struct{ Body any }, error) {
	_, admin, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	// Viewing retained audit data is itself audited. 查看审计数据本身留痕。
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "admin.audit.view", "",
		map[string]string{"action": in.Action, "actor": in.Actor}, clientIPCtx(ctx))
	limit := in.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.Pool.Query(ctx,
		`SELECT id, actor_id, actor_kind, action, target, detail, created_at
		 FROM audit_events
		 WHERE ($1='' OR action ILIKE '%'||$1||'%')
		   AND ($2='' OR COALESCE(actor_id::text,'')=$2 OR actor_kind=$2)
		 ORDER BY id DESC LIMIT $3`, in.Action, in.Actor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type evt struct {
		ID        int64  `json:"id"`
		ActorID   string `json:"actor_id,omitempty"`
		ActorKind string `json:"actor_kind"`
		Action    string `json:"action"`
		Target    string `json:"target"`
		Detail    any    `json:"detail"`
		CreatedAt any    `json:"created_at"`
	}
	out := []evt{}
	for rows.Next() {
		var e evt
		var actorID *uuid.UUID
		var detail []byte
		var created any
		if err := rows.Scan(&e.ID, &actorID, &e.ActorKind, &e.Action, &e.Target, &detail, &created); err != nil {
			return nil, err
		}
		if actorID != nil {
			e.ActorID = actorID.String()
		}
		_ = json.Unmarshal(detail, &e.Detail)
		e.CreatedAt = created
		out = append(out, e)
	}
	return &struct{ Body any }{Body: map[string]any{"events": out}}, nil
}

func (d *Deps) adminSettingsGet(ctx context.Context, in *struct{ Headers }) (*struct {
	Body any
}, error,
) {
	if _, _, err := d.requireAdmin(ctx, in.Authorization); err != nil {
		return nil, err
	}
	all, err := d.Settings.All(ctx)
	if err != nil {
		return nil, err
	}
	return &struct{ Body any }{Body: map[string]any{"settings": all}}, nil
}

func (d *Deps) adminSettingsSet(ctx context.Context, in *struct {
	Headers
	Body struct {
		Settings map[string]any `json:"settings" required:"true"`
	}
},
) (*struct{}, error) {
	_, admin, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	for k, v := range in.Body.Settings {
		if err := d.Settings.Set(ctx, k, v, admin.ID); err != nil {
			return nil, err
		}
	}
	// Refresh hot settings the server caches at boot.
	// 刷新启动时缓存的热设置（撤回时限）。
	d.IM.RecallWindow = time.Duration(
		d.Settings.Int(ctx, settings.KeyIMRecallTimeout, settings.DefRecallTimeout)) * time.Second
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "admin.settings.set", "",
		in.Body.Settings, clientIPCtx(ctx))
	return &struct{}{}, nil
}

// adminAuditExport streams audit events as CSV (raw mux route — huma wraps
// responses and can't stream). Admin-only; viewing is audited.
//
// 审计 CSV 导出（裸 mux 路由，huma 没法流式）。仅管理员，导出自审计。
func (d *Deps) adminAuditExport(w http.ResponseWriter, r *http.Request) {
	_, admin, err := d.requireAdmin(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	d.Audit.Log(r.Context(), &admin.ID, audit.ActorAdmin, "admin.audit.export", "", nil,
		net.ParseIP(r.Header.Get("X-Forwarded-For")))
	rows, err := d.Pool.Query(r.Context(),
		`SELECT id, actor_id, actor_kind, action, target, detail, ip, created_at
		 FROM audit_events ORDER BY id`)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=audit.csv")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "actor_id", "actor_kind", "action", "target", "detail", "ip", "created_at"})
	for rows.Next() {
		var id int64
		var actorID *uuid.UUID
		var kind, action string
		var target, detail []byte
		var ip *string
		var ts time.Time
		if err := rows.Scan(&id, &actorID, &kind, &action, &target, &detail, &ip, &ts); err != nil {
			break
		}
		actor := ""
		if actorID != nil {
			actor = actorID.String()
		}
		ipStr := ""
		if ip != nil {
			ipStr = *ip
		}
		_ = cw.Write([]string{
			fmt.Sprintf("%d", id), actor, kind, action, string(target),
			string(detail), ipStr, ts.Format(time.RFC3339),
		})
	}
	cw.Flush()
}
