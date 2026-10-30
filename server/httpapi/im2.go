package httpapi

// IM phase-2 endpoints: group management, public discovery, message
// search, unread aggregate, effective settings read, notification
// placeholder.
//
// IM 二期接口：群管理、公开群发现、消息搜索、未读聚合、生效设置、通知占位。

import (
	"context"
	"net/http"
	"wittychat/core/settings"
	"wittychat/shared/apitypes"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

func registerIM2(api huma.API, d *Deps) {
	sec := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "im-rename", Method: http.MethodPost,
		Path: "/api/v1/conversations/{id}/rename", Summary: "Rename group (owner/admin) / 群改名",
		Tags: []string{"im"}, Security: sec,
	}, d.renameConv)

	huma.Register(api, huma.Operation{
		OperationID: "im-transfer", Method: http.MethodPost,
		Path: "/api/v1/conversations/{id}/transfer", Summary: "Transfer ownership (owner) / 转让群主",
		Tags: []string{"im"}, Security: sec,
	}, d.transferConv)

	huma.Register(api, huma.Operation{
		OperationID: "im-set-role", Method: http.MethodPost,
		Path: "/api/v1/conversations/{id}/members/{user_id}/role", Summary: "Set member role (owner) / 设管理员",
		Tags: []string{"im"}, Security: sec,
	}, d.setMemberRole)

	huma.Register(api, huma.Operation{
		OperationID: "im-public-groups", Method: http.MethodGet,
		Path: "/api/v1/groups/public", Summary: "Search public groups / 搜索公开群",
		Tags: []string{"im"}, Security: sec,
	}, d.searchPublicGroups)

	huma.Register(api, huma.Operation{
		OperationID: "im-join-public", Method: http.MethodPost,
		Path: "/api/v1/groups/{id}/join", Summary: "Join a public group / 加入公开群",
		Tags: []string{"im"}, Security: sec,
	}, d.joinPublicGroup)

	huma.Register(api, huma.Operation{
		OperationID: "im-search", Method: http.MethodGet,
		Path: "/api/v1/search/messages", Summary: "Search my messages (substring) / 消息搜索",
		Tags: []string{"im"}, Security: sec,
	}, d.searchMessages)

	huma.Register(api, huma.Operation{
		OperationID: "im-unread", Method: http.MethodGet,
		Path: "/api/v1/unread", Summary: "Aggregate unread count / 未读聚合",
		Tags: []string{"im"}, Security: sec,
	}, d.unreadSummary)

	huma.Register(api, huma.Operation{
		OperationID: "settings-effective", Method: http.MethodGet,
		Path: "/api/v1/settings", Summary: "Effective workspace settings / 生效设置",
		Tags: []string{"settings"}, Security: sec,
	}, d.effectiveSettings)

	huma.Register(api, huma.Operation{
		OperationID: "notifications-list", Method: http.MethodGet,
		Path: "/api/v1/notifications", Summary: "Notification center (placeholder) / 通知中心占位",
		Tags: []string{"notifications"}, Security: sec,
	}, d.notifications)

	huma.Register(api, huma.Operation{
		OperationID: "users-list", Method: http.MethodGet,
		Path: "/api/v1/users", Summary: "Workspace contacts / 通讯录",
		Tags: []string{"users"}, Security: sec,
	}, d.listUsers)
}

// listUsers — contacts directory for member pickers & DM creation.
// 通讯录：建群拉人、开私聊选人用。
func (d *Deps) listUsers(ctx context.Context, in *struct{ Headers }) (*struct {
	Body any
}, error,
) {
	if _, _, err := d.requireUser(ctx, in.Authorization); err != nil {
		return nil, err
	}
	users, err := d.Store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := []*apitypes.UserPublic{}
	for _, u := range users {
		if u.Status != "active" {
			continue // 未激活/停用/离职不进通讯录
		}
		out = append(out, publicUser(u))
	}
	return &struct{ Body any }{Body: map[string]any{"users": out}}, nil
}

func (d *Deps) renameConv(ctx context.Context, in *struct {
	Headers
	ID   string `path:"id" format:"uuid"`
	Body apitypes.RenameConvRequest
},
) (*struct{ Body apitypes.OpEvent }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	convID, _ := uuid.Parse(in.ID)
	seq, err := d.IM.RenameConv(ctx, u.ID, convID, in.Body.Title)
	if err != nil {
		return nil, imErr(err)
	}
	d.notifyMembers(ctx, convID, seq, u.ID)
	return &struct{ Body apitypes.OpEvent }{
		Body: apitypes.OpEvent{OpSeq: seq, Kind: apitypes.OpRename},
	}, nil
}

func (d *Deps) transferConv(ctx context.Context, in *struct {
	Headers
	ID   string `path:"id" format:"uuid"`
	Body apitypes.TransferRequest
},
) (*struct{ Body apitypes.OpEvent }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	convID, _ := uuid.Parse(in.ID)
	target, err := uuid.Parse(in.Body.UserID)
	if err != nil {
		return nil, huma.Error400BadRequest("bad user_id")
	}
	seq, err := d.IM.TransferOwner(ctx, u.ID, convID, target)
	if err != nil {
		return nil, imErr(err)
	}
	d.notifyMembers(ctx, convID, seq, u.ID)
	return &struct{ Body apitypes.OpEvent }{
		Body: apitypes.OpEvent{OpSeq: seq, Kind: apitypes.OpTransfer},
	}, nil
}

func (d *Deps) setMemberRole(ctx context.Context, in *struct {
	Headers
	ID     string `path:"id" format:"uuid"`
	UserID string `path:"user_id" format:"uuid"`
	Body   apitypes.SetRoleRequest
},
) (*struct{ Body apitypes.OpEvent }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	convID, _ := uuid.Parse(in.ID)
	target, err := uuid.Parse(in.UserID)
	if err != nil {
		return nil, huma.Error400BadRequest("bad user_id")
	}
	seq, err := d.IM.SetMemberRole(ctx, u.ID, convID, target, in.Body.Role)
	if err != nil {
		return nil, imErr(err)
	}
	d.notifyMembers(ctx, convID, seq, u.ID)
	return &struct{ Body apitypes.OpEvent }{
		Body: apitypes.OpEvent{OpSeq: seq, Kind: apitypes.OpRoleChange},
	}, nil
}

func (d *Deps) searchPublicGroups(ctx context.Context, in *struct {
	Headers
	Q string `query:"q"`
},
) (*struct{ Body any }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	gs, err := d.IM.SearchPublicGroups(ctx, u.ID, in.Q, 50)
	if err != nil {
		return nil, err
	}
	out := []apitypes.PublicGroupInfo{}
	for _, g := range gs {
		out = append(out, apitypes.PublicGroupInfo{
			ID: g.ID.String(), Title: g.Title, MemberCount: g.MemberCount, Joined: g.Joined,
		})
	}
	return &struct{ Body any }{Body: map[string]any{"groups": out}}, nil
}

func (d *Deps) joinPublicGroup(ctx context.Context, in *struct {
	Headers
	ID string `path:"id" format:"uuid"`
},
) (*struct{}, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	convID, _ := uuid.Parse(in.ID)
	seq, err := d.IM.JoinPublicGroup(ctx, convID, u.ID)
	if err != nil {
		return nil, imErr(err)
	}
	if seq >= 0 {
		d.notifyMembers(ctx, convID, seq, u.ID)
	}
	return &struct{}{}, nil
}

func (d *Deps) searchMessages(ctx context.Context, in *struct {
	Headers
	Q     string `query:"q" required:"true" minLength:"1" maxLength:"100"`
	Limit int    `query:"limit"`
},
) (*struct{ Body any }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	hits, err := d.IM.SearchMessages(ctx, u.ID, in.Q, in.Limit)
	if err != nil {
		return nil, err
	}
	out := []apitypes.MessageSearchHit{}
	for _, h := range hits {
		out = append(out, apitypes.MessageSearchHit{
			ConvID: h.ConvID.String(), OpSeq: h.OpSeq, Body: h.Body,
			ActorName: h.ActorName, ConvTitle: h.ConvTitle, CreatedAt: h.CreatedAt,
		})
	}
	return &struct{ Body any }{Body: map[string]any{"hits": out}}, nil
}

func (d *Deps) unreadSummary(ctx context.Context, in *struct{ Headers }) (*struct {
	Body apitypes.UnreadSummary
}, error,
) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	entries, err := d.IM.Directory(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, e := range entries {
		total += e.Unread
	}
	return &struct{ Body apitypes.UnreadSummary }{
		Body: apitypes.UnreadSummary{Total: total},
	}, nil
}

// effectiveSettings exposes the subset of workspace settings clients need.
func (d *Deps) effectiveSettings(ctx context.Context, in *struct{ Headers }) (*struct {
	Body any
}, error,
) {
	if _, _, err := d.requireUser(ctx, in.Authorization); err != nil {
		return nil, err
	}
	limit := d.Settings.Int(ctx, settings.KeyIMMessageMaxChars, settings.DefMessageMaxChars)
	if limit > settings.MaxMessageCharsCap {
		limit = settings.MaxMessageCharsCap
	}
	return &struct{ Body any }{Body: map[string]any{
		"workspace": map[string]any{
			"name": d.Settings.Str(ctx, "workspace.name", "WittyChat"),
		},
		"im": map[string]any{
			"message_max_chars":  limit,
			"recall_timeout_sec": d.Settings.Int(ctx, settings.KeyIMRecallTimeout, settings.DefRecallTimeout),
			"read_receipts":      d.Settings.Bool(ctx, settings.KeyIMReadReceipts, true),
			"at_all":             d.Settings.Str(ctx, settings.KeyIMAtAll, "all"),
		},
	}}, nil
}

// notifications is the Phase-2 placeholder: an empty list + shape the
// client renders. Real events land in P1.
// 通知中心占位：返回形状和空列表，真实事件 P1 落。
func (d *Deps) notifications(ctx context.Context, in *struct{ Headers }) (*struct {
	Body any
}, error,
) {
	if _, _, err := d.requireUser(ctx, in.Authorization); err != nil {
		return nil, err
	}
	return &struct{ Body any }{Body: map[string]any{
		"notifications": []any{},
	}}, nil
}
