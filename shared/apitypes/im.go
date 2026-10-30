package apitypes

// IM domain API types — the client contract for conversations + op stream.
// IM 域 API 类型：会话与 op 流的客户端契约。

import "time"

// Conversation kinds / 会话类型
const (
	ConvKindDM        = "dm"
	ConvKindGroup     = "group"
	ConvKindAssistant = "assistant"
)

// Op kinds / op 类型
const (
	OpMessage      = "message"
	OpEdit         = "edit"
	OpRecall       = "recall"
	OpMemberAdd    = "member_add"
	OpMemberRemove = "member_remove"
	OpRoleChange   = "role_change"
	OpRename       = "rename"
	OpTransfer     = "transfer"
)

// ConversationInfo is one directory entry.
type ConversationInfo struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Title       string    `json:"title,omitempty"`
	IsPublic    bool      `json:"is_public,omitempty"`
	LastOpSeq   int64     `json:"last_op_seq"`
	LastReadSeq int64     `json:"last_read_seq"`
	Unread      int64     `json:"unread"`
	Preview     string    `json:"preview,omitempty"`
	PreviewAt   time.Time `json:"preview_at,omitempty"`
	PeerUserID  string    `json:"peer_user_id,omitempty"` // dm only
	PeerName    string    `json:"peer_name,omitempty"`
	PeerReadSeq int64     `json:"peer_read_seq,omitempty"` // dm: peer watermark → 已读回执
	MemberCount int       `json:"member_count,omitempty"`
}

// DirectoryResponse is the client's whole conv list + watermarks.
type DirectoryResponse struct {
	Conversations []*ConversationInfo `json:"conversations"`
}

// OpEvent is one entry of the conversation op stream.
// Body/Payload are the EFFECTIVE view: edits applied, recalls redacted.
// Body/Payload 是有效视图：编辑已应用、撤回已脱敏。
type OpEvent struct {
	OpSeq     int64          `json:"op_seq"`
	OpID      string         `json:"op_id"`
	Kind      string         `json:"kind"`
	ActorID   string         `json:"actor_id,omitempty"`
	ActorName string         `json:"actor_name,omitempty"`
	ClientMsg string         `json:"client_msg_id,omitempty"`
	TargetSeq int64          `json:"target_seq,omitempty"`
	Body      string         `json:"body,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
	Edited    bool           `json:"edited,omitempty"`
	Recalled  bool           `json:"recalled,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// OpsResponse is one page of the stream.
type OpsResponse struct {
	Ops       []*OpEvent `json:"ops"`
	HasMore   bool       `json:"has_more"`
	LatestSeq int64      `json:"latest_seq"` // conv last_op_seq / 会话最新序号
}

// ---------- requests ----------

type CreateDMRequest struct {
	UserID string `json:"user_id" required:"true" format:"uuid"`
}

type CreateGroupRequest struct {
	Title     string   `json:"title" required:"true" minLength:"1" maxLength:"80"`
	IsPublic  bool     `json:"is_public"`
	MemberIDs []string `json:"member_ids" maxItems:"200"`
}

type SendMessageRequest struct {
	ClientMsgID string   `json:"client_msg_id" required:"true" format:"uuid"`
	Markdown    string   `json:"markdown" required:"true" minLength:"0" maxLength:"5000"` // may be "" when object_ids present
	QuoteOpSeq  int64    `json:"quote_op_seq,omitempty"`
	Mentions    []string `json:"mentions,omitempty" maxItems:"50"`
	MentionAll  bool     `json:"mention_all,omitempty"`              // @所有人（受 im.at_all 策略限制）
	ObjectIDs   []string `json:"object_ids,omitempty" maxItems:"20"` // attachments / 附件
}

type RenameConvRequest struct {
	Title string `json:"title" required:"true" minLength:"1" maxLength:"80"`
}

type TransferRequest struct {
	UserID string `json:"user_id" required:"true" format:"uuid"`
}

type SetRoleRequest struct {
	Role string `json:"role" required:"true" enum:"admin,member"`
}

// PublicGroupInfo is one public-group search hit.
type PublicGroupInfo struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	MemberCount int    `json:"member_count"`
	Joined      bool   `json:"joined"`
}

// MessageSearchHit is one message-search result.
type MessageSearchHit struct {
	ConvID    string `json:"conv_id"`
	OpSeq     int64  `json:"op_seq"`
	Body      string `json:"body"`
	ActorName string `json:"actor_name"`
	ConvTitle string `json:"conv_title"`
	CreatedAt string `json:"created_at"`
}

// UnreadSummary is the aggregate unread badge.
type UnreadSummary struct {
	Total int64 `json:"total"`
}

type EditMessageRequest struct {
	Markdown string `json:"markdown" required:"true" minLength:"1" maxLength:"5000"`
}

type MarkReadRequest struct {
	OpSeq int64 `json:"op_seq" required:"true" minimum:"0"`
}

type AddMembersRequest struct {
	UserIDs []string `json:"user_ids" required:"true" minItems:"1" maxItems:"200"`
}
