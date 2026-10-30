package httpapi

// IM endpoints: conversations, op stream, read watermarks.
// Writes go through conv_ops; reads serve watermark deltas.
// WS carries only "go fetch" notifications.
//
// IM 接口：会话、op 流、已读水印。
// 写走 conv_ops，读按水印给差额；WS 只发"来拉"提醒。

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"wittychat/core/audit"
	"wittychat/core/im"
	"wittychat/shared/apitypes"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

func registerIM(api huma.API, d *Deps) {
	sec := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "im-directory", Method: http.MethodGet,
		Path: "/api/v1/conversations", Summary: "Conversation directory + watermarks / 会话目录与水印",
		Tags: []string{"im"}, Security: sec,
	}, d.directory)

	huma.Register(api, huma.Operation{
		OperationID: "im-create-dm", Method: http.MethodPost,
		Path: "/api/v1/conversations/dm", Summary: "Open a private chat / 打开私聊",
		Tags: []string{"im"}, Security: sec,
	}, d.createDM)

	huma.Register(api, huma.Operation{
		OperationID: "im-create-group", Method: http.MethodPost,
		Path: "/api/v1/conversations/group", Summary: "Create a group / 建群",
		Tags: []string{"im"}, Security: sec,
	}, d.createGroup)

	huma.Register(api, huma.Operation{
		OperationID: "im-ops-after", Method: http.MethodGet,
		Path: "/api/v1/conversations/{id}/ops", Summary: "Fetch op delta (after|before watermark) / 拉取 op 差额",
		Tags: []string{"im"}, Security: sec,
	}, d.fetchOps)

	huma.Register(api, huma.Operation{
		OperationID: "im-send", Method: http.MethodPost,
		Path: "/api/v1/conversations/{id}/messages", Summary: "Send a message (idempotent) / 发消息（幂等）",
		Tags: []string{"im"}, Security: sec,
	}, d.sendMessage)

	huma.Register(api, huma.Operation{
		OperationID: "im-edit", Method: http.MethodPost,
		Path: "/api/v1/conversations/{id}/messages/{seq}/edit", Summary: "Edit own message / 编辑自己的消息",
		Tags: []string{"im"}, Security: sec,
	}, d.editMessage)

	huma.Register(api, huma.Operation{
		OperationID: "im-recall", Method: http.MethodPost,
		Path: "/api/v1/conversations/{id}/messages/{seq}/recall", Summary: "Recall own message (10min) / 撤回（10分钟内）",
		Tags: []string{"im"}, Security: sec,
	}, d.recallMessage)

	huma.Register(api, huma.Operation{
		OperationID: "im-read", Method: http.MethodPost,
		Path: "/api/v1/conversations/{id}/read", Summary: "Advance read watermark / 推进已读水印",
		Tags: []string{"im"}, Security: sec,
	}, d.markRead)

	huma.Register(api, huma.Operation{
		OperationID: "im-members", Method: http.MethodGet,
		Path: "/api/v1/conversations/{id}/members", Summary: "List members / 成员列表",
		Tags: []string{"im"}, Security: sec,
	}, d.listMembers)

	huma.Register(api, huma.Operation{
		OperationID: "im-add-members", Method: http.MethodPost,
		Path: "/api/v1/conversations/{id}/members", Summary: "Add members (owner/admin) / 拉人",
		Tags: []string{"im"}, Security: sec,
	}, d.addMembers)

	huma.Register(api, huma.Operation{
		OperationID: "im-remove-member", Method: http.MethodDelete,
		Path: "/api/v1/conversations/{id}/members/{user_id}", Summary: "Remove member or leave / 踢人或退群",
		Tags: []string{"im"}, Security: sec,
	}, d.removeMember)
}

// ---------- helpers ----------

func opEvent(o *im.Op) *apitypes.OpEvent {
	e := &apitypes.OpEvent{
		OpSeq:     o.Seq,
		OpID:      o.ID.String(),
		Kind:      o.Kind,
		TargetSeq: o.TargetSeq,
		Body:      o.Body,
		Payload:   o.Payload,
		Edited:    o.Edited,
		Recalled:  o.Recalled,
		CreatedAt: o.CreatedAt,
		ActorName: o.ActorName,
	}
	if o.ActorID != nil {
		e.ActorID = o.ActorID.String()
	}
	if o.ClientMsg != nil {
		e.ClientMsg = o.ClientMsg.String()
	}
	return e
}

func imErr(err error) error {
	switch {
	case errors.Is(err, im.ErrNotFound):
		return huma.Error404NotFound("conversation not found / 会话不存在")
	case errors.Is(err, im.ErrNotMember):
		return huma.Error403Forbidden("not a member / 不是会话成员")
	case errors.Is(err, im.ErrNoPermission):
		return huma.Error403Forbidden("no permission / 无权限")
	case errors.Is(err, im.ErrRecallExpired):
		return huma.Error400BadRequest("recall window expired / 已过撤回时限")
	case errors.Is(err, im.ErrMessageTooLong):
		return huma.Error400BadRequest("message too long / 消息过长")
	case errors.Is(err, im.ErrSelfDM):
		return huma.Error400BadRequest("cannot dm yourself / 不能和自己私聊")
	default:
		return err
	}
}

// enrichAttachments replaces bare object-id strings in payload.attachments
// with {id,name,size,mime} maps resolved via attachment_refs.
// 用 attachment_refs 把 payload.attachments 的裸 id 换成 {id,name,size,mime}。
func (d *Deps) enrichAttachments(ctx context.Context, convID uuid.UUID, events []*apitypes.OpEvent) {
	if d.Objects == nil {
		return
	}
	for _, e := range events {
		if e.Kind != apitypes.OpMessage {
			continue
		}
		if _, has := e.Payload["attachments"]; !has {
			continue
		}
		objs, err := d.Objects.AttachmentsOf(ctx, convID, e.OpSeq)
		if err != nil || len(objs) == 0 {
			continue
		}
		list := make([]map[string]any, 0, len(objs))
		for _, o := range objs {
			list = append(list, map[string]any{
				"id": o.ID.String(), "name": o.Name, "size": o.Size, "mime": o.Mime,
			})
		}
		e.Payload["attachments"] = list
	}
}

// notifyMembers pushes a conv_update to all online members except the actor.
func (d *Deps) notifyMembers(ctx context.Context, convID uuid.UUID, seq int64, actor uuid.UUID) {
	if d.Hub == nil {
		return
	}
	ids, err := d.IM.MemberIDs(ctx, convID)
	if err != nil {
		return
	}
	d.Hub.NotifyConv(ids, convID, seq, &actor)
}

// ---------- handlers ----------

func (d *Deps) directory(ctx context.Context, in *struct{ Headers }) (*struct {
	Body apitypes.DirectoryResponse
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
	out := &struct{ Body apitypes.DirectoryResponse }{
		Body: apitypes.DirectoryResponse{Conversations: make([]*apitypes.ConversationInfo, 0, len(entries))},
	}
	for _, e := range entries {
		ci := &apitypes.ConversationInfo{
			ID: e.ConvID.String(), Kind: e.Kind, Title: e.Title, IsPublic: e.IsPublic,
			LastOpSeq: e.LastOpSeq, LastReadSeq: e.LastReadSeq, Unread: e.Unread,
			Preview: e.Preview, PreviewAt: e.PreviewAt, MemberCount: e.MemberCount,
		}
		if e.PeerUserID != nil {
			ci.PeerUserID = e.PeerUserID.String()
			ci.PeerName = e.PeerName
			ci.PeerReadSeq = e.PeerReadSeq
		}
		out.Body.Conversations = append(out.Body.Conversations, ci)
	}
	return out, nil
}

func (d *Deps) createDM(ctx context.Context, in *struct {
	Headers
	Body apitypes.CreateDMRequest
},
) (*struct{ Body apitypes.ConversationInfo }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	peer, err := uuid.Parse(in.Body.UserID)
	if err != nil {
		return nil, huma.Error400BadRequest("bad user id")
	}
	c, err := d.IM.CreateDM(ctx, u.ID, peer)
	if err != nil {
		return nil, imErr(err)
	}
	return &struct{ Body apitypes.ConversationInfo }{
		Body: apitypes.ConversationInfo{
			ID: c.ID.String(), Kind: c.Kind, LastOpSeq: c.LastOpSeq, PeerUserID: in.Body.UserID,
		},
	}, nil
}

func (d *Deps) createGroup(ctx context.Context, in *struct {
	Headers
	Body apitypes.CreateGroupRequest
},
) (*struct{ Body apitypes.ConversationInfo }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(in.Body.MemberIDs))
	for _, s := range in.Body.MemberIDs {
		if id, err := uuid.Parse(s); err == nil {
			ids = append(ids, id)
		}
	}
	c, err := d.IM.CreateGroup(ctx, u.ID, in.Body.Title, in.Body.IsPublic, ids)
	if err != nil {
		return nil, imErr(err)
	}
	d.Audit.Log(ctx, &u.ID, audit.ActorUser, "im.group.create", in.Body.Title, nil, clientIP(in.XForwardedFor))
	return &struct{ Body apitypes.ConversationInfo }{
		Body: apitypes.ConversationInfo{
			ID: c.ID.String(), Kind: c.Kind, Title: c.Title, IsPublic: c.IsPublic,
			LastOpSeq: c.LastOpSeq,
		},
	}, nil
}

func (d *Deps) fetchOps(ctx context.Context, in *struct {
	Headers
	ID     string `path:"id" format:"uuid"`
	After  string `query:"after" doc:"fetch ops with seq > after (incremental)"`
	Before string `query:"before" doc:"fetch ops with seq < before (backfill)"`
	Limit  int    `query:"limit" default:"200" minimum:"1" maximum:"500"`
},
) (*struct{ Body apitypes.OpsResponse }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	convID, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error400BadRequest("bad conversation id")
	}
	out := &struct{ Body apitypes.OpsResponse }{
		Body: apitypes.OpsResponse{Ops: []*apitypes.OpEvent{}},
	}
	if in.Before != "" {
		before, _ := strconv.ParseInt(in.Before, 10, 64)
		ops, hasMore, err := d.IM.FetchOpsBefore(ctx, u.ID, convID, before, in.Limit)
		if err != nil {
			return nil, imErr(err)
		}
		for _, o := range ops {
			out.Body.Ops = append(out.Body.Ops, opEvent(o))
		}
		out.Body.HasMore = hasMore
		d.enrichAttachments(ctx, convID, out.Body.Ops)
		return out, nil
	}
	var after int64
	if in.After != "" {
		after, _ = strconv.ParseInt(in.After, 10, 64)
	}
	ops, hasMore, latest, err := d.IM.FetchOpsAfter(ctx, u.ID, convID, after, in.Limit)
	if err != nil {
		return nil, imErr(err)
	}
	for _, o := range ops {
		out.Body.Ops = append(out.Body.Ops, opEvent(o))
	}
	out.Body.HasMore = hasMore
	out.Body.LatestSeq = latest
	d.enrichAttachments(ctx, convID, out.Body.Ops)
	return out, nil
}

func (d *Deps) sendMessage(ctx context.Context, in *struct {
	Headers
	ID   string `path:"id" format:"uuid"`
	Body apitypes.SendMessageRequest
}) (*struct {
	Body   apitypes.OpEvent
	Status int
}, error,
) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	convID, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error400BadRequest("bad conversation id")
	}
	// Workspace default 2000, admin may raise to 5000 (hard ceiling).
	// 工作区默认 2000，管理员可调至 5000（硬顶）。
	msgLimit := int64(2000) // read workspace_settings later / 后续从设置读
	if int64(len([]rune(in.Body.Markdown))) > msgLimit {
		return nil, huma.Error400BadRequest("message exceeds limit / 消息超长")
	}
	if in.Body.Markdown == "" && len(in.Body.ObjectIDs) == 0 {
		return nil, huma.Error400BadRequest("empty message / 空消息")
	}
	clientMsgID, err := uuid.Parse(in.Body.ClientMsgID)
	if err != nil {
		return nil, huma.Error400BadRequest("bad client_msg_id")
	}
	var mentions []uuid.UUID
	for _, s := range in.Body.Mentions {
		if id, err := uuid.Parse(s); err == nil {
			mentions = append(mentions, id)
		}
	}
	var objIDs []uuid.UUID
	for _, s := range in.Body.ObjectIDs {
		if id, err := uuid.Parse(s); err == nil {
			objIDs = append(objIDs, id)
		}
	}
	op, existed, err := d.IM.SendMessage(ctx, u.ID, convID, clientMsgID,
		in.Body.Markdown, in.Body.QuoteOpSeq, mentions, objIDs)
	if err != nil {
		return nil, imErr(err)
	}
	if !existed {
		if len(objIDs) > 0 {
			_ = d.Objects.LinkAttachments(ctx, convID, op.Seq, objIDs)
		}
		d.notifyMembers(ctx, convID, op.Seq, u.ID)
	}
	status := http.StatusOK
	if !existed {
		status = http.StatusCreated
	}
	op.ActorName = u.DisplayName // send response carries the name (fetch fills it via join)
	event := opEvent(op)
	d.enrichAttachments(ctx, convID, []*apitypes.OpEvent{event})
	return &struct {
		Body   apitypes.OpEvent
		Status int
	}{Body: *event, Status: status}, nil
}

func (d *Deps) editMessage(ctx context.Context, in *struct {
	Headers
	ID   string `path:"id" format:"uuid"`
	Seq  int64  `path:"seq"`
	Body apitypes.EditMessageRequest
},
) (*struct{ Body apitypes.OpEvent }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	convID, _ := uuid.Parse(in.ID)
	seq, err := d.IM.EditMessage(ctx, u.ID, convID, in.Seq, in.Body.Markdown)
	if err != nil {
		return nil, imErr(err)
	}
	d.notifyMembers(ctx, convID, seq, u.ID)
	return &struct{ Body apitypes.OpEvent }{
		Body: apitypes.OpEvent{OpSeq: seq, Kind: apitypes.OpEdit, TargetSeq: in.Seq},
	}, nil
}

func (d *Deps) recallMessage(ctx context.Context, in *struct {
	Headers
	ID  string `path:"id" format:"uuid"`
	Seq int64  `path:"seq"`
},
) (*struct{ Body apitypes.OpEvent }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	convID, _ := uuid.Parse(in.ID)
	seq, err := d.IM.RecallMessage(ctx, u.ID, convID, in.Seq)
	if err != nil {
		return nil, imErr(err)
	}
	d.notifyMembers(ctx, convID, seq, u.ID)
	return &struct{ Body apitypes.OpEvent }{
		Body: apitypes.OpEvent{OpSeq: seq, Kind: apitypes.OpRecall, TargetSeq: in.Seq},
	}, nil
}

func (d *Deps) markRead(ctx context.Context, in *struct {
	Headers
	ID   string `path:"id" format:"uuid"`
	Body apitypes.MarkReadRequest
},
) (*struct{}, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	convID, _ := uuid.Parse(in.ID)
	if err := d.IM.MarkRead(ctx, convID, u.ID, in.Body.OpSeq); err != nil {
		return nil, imErr(err)
	}
	if d.Hub != nil {
		if ids, err := d.IM.MemberIDs(ctx, convID); err == nil {
			d.Hub.Read(ids, convID, u.ID, in.Body.OpSeq, true)
		}
	}
	return &struct{}{}, nil
}

func (d *Deps) listMembers(ctx context.Context, in *struct {
	Headers
	ID string `path:"id" format:"uuid"`
},
) (*struct{ Body any }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	convID, _ := uuid.Parse(in.ID)
	members, err := d.IM.Members(ctx, convID, u.ID)
	if err != nil {
		return nil, imErr(err)
	}
	type m struct {
		UserID      string `json:"user_id"`
		Name        string `json:"name"`
		Role        string `json:"role"`
		JoinedAt    any    `json:"joined_at"`
		LastReadSeq int64  `json:"last_read_seq"`
	}
	out := []m{}
	for _, mm := range members {
		out = append(out, m{mm.UserID.String(), mm.Name, mm.Role, mm.JoinedAt, mm.LastReadSeq})
	}
	return &struct{ Body any }{Body: map[string]any{"members": out}}, nil
}

func (d *Deps) addMembers(ctx context.Context, in *struct {
	Headers
	ID   string `path:"id" format:"uuid"`
	Body apitypes.AddMembersRequest
},
) (*struct{}, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	convID, _ := uuid.Parse(in.ID)
	var ids []uuid.UUID
	for _, s := range in.Body.UserIDs {
		if id, err := uuid.Parse(s); err == nil {
			ids = append(ids, id)
		}
	}
	if _, err := d.IM.AddMembers(ctx, u.ID, convID, ids); err != nil {
		return nil, imErr(err)
	}
	d.Audit.Log(ctx, &u.ID, audit.ActorUser, "im.member.add", convID.String(),
		map[string]any{"count": len(ids)}, clientIP(in.XForwardedFor))
	return &struct{}{}, nil
}

func (d *Deps) removeMember(ctx context.Context, in *struct {
	Headers
	ID     string `path:"id" format:"uuid"`
	UserID string `path:"user_id" format:"uuid"`
},
) (*struct{}, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	convID, _ := uuid.Parse(in.ID)
	target, _ := uuid.Parse(in.UserID)
	if err := d.IM.RemoveMember(ctx, u.ID, convID, target); err != nil {
		return nil, imErr(err)
	}
	return &struct{}{}, nil
}
