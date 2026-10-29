package httpapi

// Workspace-admin endpoints: provisioning, links, suspend/offboard/reactivate.
// 工作区管理员接口：开通账号、签发链接、停用/离职交接/恢复。

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"wittychat/core/audit"
	"wittychat/core/auth"
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
	token, expiresAt, err := d.Store.CreateActivationLink(ctx, u.ID, auth.LinkKindActivation, admin.ID)
	if err != nil {
		return nil, err
	}
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "admin.user.create", u.Email, nil, clientIP(in.XForwardedFor))
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
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "admin.user.link:"+kind, u.Email, nil, clientIP(in.XForwardedFor))
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
		d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, action, target.Email, nil, clientIP(in.XForwardedFor))
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
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "admin.user.offboard", target.Email,
		map[string]string{"transfer_to": toUser.Email}, clientIP(in.XForwardedFor))
	return &struct{}{}, nil
}
