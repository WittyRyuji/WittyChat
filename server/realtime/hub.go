// Package realtime is the in-memory push layer. It carries NOTIFICATIONS,
// never data: a "conv_update" frame only tells the client "go fetch your
// watermark delta" — conv_ops remains the source of truth. Typing and
// presence are transient and never touch the database.
//
// realtime 包是内存推送层。它只传提醒不传数据：
// conv_update 帧仅告知客户端"来拉你的水印差额"，conv_ops 仍是事实源。
// 输入中/在线状态是暂态，永不落库。
package realtime

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
)

// Frame is anything pushed to a client.
type Frame struct {
	Type    string `json:"type"`              // conv_update | presence | typing
	ConvID  string `json:"conv_id,omitempty"` // conv_update, typing
	LastSeq int64  `json:"last_op_seq,omitempty"`
	UserID  string `json:"user_id,omitempty"` // presence, typing
	Online  bool   `json:"online,omitempty"`  // presence
	Name    string `json:"name,omitempty"`
	Avatar  string `json:"avatar_object_id,omitempty"` // user_update
}

// InFrame is anything a client sends over WS.
type InFrame struct {
	Type   string `json:"type"` // typing | ping
	ConvID string `json:"conv_id,omitempty"`
}

type conn struct {
	userID uuid.UUID
	sessID uuid.UUID
	ws     *websocket.Conn
	send   chan Frame
	cancel context.CancelFunc // unblocks the read loop on kick / 踢出时解除读循环阻塞
}

// kill tears the conn down without the close handshake: Close() waits up
// to ~5s for the peer's close reply, which would stall the whole hub if
// called under its lock. / 不走关闭握手直接拆连接：Close() 为等对端回应
// 最长阻塞约 5 秒，持锁调用会卡住整个推送中枢。
func (c *conn) kill() {
	c.cancel()
	_ = c.ws.CloseNow()
}

// Hub tracks online connections per user (multi-device) and fans out frames.
type Hub struct {
	mu          sync.RWMutex
	conns       map[uuid.UUID]map[*conn]struct{}
	isMember    func(ctx context.Context, convID, userID uuid.UUID) bool
	typingSink  func(convID, from uuid.UUID, name string)
	onlineHooks []func(userID uuid.UUID, online bool)
}

func NewHub(isMember func(ctx context.Context, convID, userID uuid.UUID) bool) *Hub {
	return &Hub{conns: map[uuid.UUID]map[*conn]struct{}{}, isMember: isMember}
}

// OnPresence registers a hook fired on first-connect / last-disconnect.
func (h *Hub) OnPresence(fn func(userID uuid.UUID, online bool)) {
	h.onlineHooks = append(h.onlineHooks, fn)
}

// IsOnline reports whether the user has at least one connection.
func (h *Hub) IsOnline(userID uuid.UUID) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns[userID]) > 0
}

// OnlineUsers returns a snapshot of online user ids (presence init).
func (h *Hub) OnlineUsers() []uuid.UUID {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]uuid.UUID, 0, len(h.conns))
	for id, cs := range h.conns {
		if len(cs) > 0 {
			out = append(out, id)
		}
	}
	return out
}

// NotifyConv tells members "conv has new ops up to seq" — clients then
// fetch their own delta via HTTP. Never sends op data itself.
func (h *Hub) NotifyConv(memberIDs []uuid.UUID, convID uuid.UUID, seq int64, exclude *uuid.UUID) {
	f := Frame{Type: "conv_update", ConvID: convID.String(), LastSeq: seq}
	for _, uid := range memberIDs {
		if exclude != nil && uid == *exclude {
			continue
		}
		h.sendTo(uid, f)
	}
}

// Read tells members "userID advanced read watermark to seq" — lightweight
// frame so clients update read-receipt circles without refetching.
func (h *Hub) Read(memberIDs []uuid.UUID, convID, reader uuid.UUID, seq int64, exclude bool) {
	f := Frame{Type: "read", ConvID: convID.String(), UserID: reader.String(), LastSeq: seq}
	for _, uid := range memberIDs {
		if exclude && uid == reader {
			continue
		}
		h.sendTo(uid, f)
	}
}

// Typing fans a transient typing hint to other online members (no persist).
func (h *Hub) Typing(memberIDs []uuid.UUID, convID, from uuid.UUID, name string) {
	f := Frame{Type: "typing", ConvID: convID.String(), UserID: from.String(), Name: name}
	for _, uid := range memberIDs {
		if uid != from {
			h.sendTo(uid, f)
		}
	}
}

func (h *Hub) sendTo(userID uuid.UUID, f Frame) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.conns[userID] {
		select {
		case c.send <- f:
		default:
			// Slow consumer: drop the notification. Client catches up
			// via watermark compare on next reconnect anyway.
		}
	}
}

// BroadcastUser tells every connected client that a user's display name or
// avatar changed, so names and avatars re-resolve by user id everywhere.
// 广播用户昵称/头像变更：所有客户端按 uid 重新解析，历史消息同步更新。
func (h *Hub) BroadcastUser(userID uuid.UUID, name, avatarObjectID string) {
	f := Frame{Type: "user_update", UserID: userID.String(), Name: name, Avatar: avatarObjectID}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, cs := range h.conns {
		for c := range cs {
			select {
			case c.send <- f:
			default:
			}
		}
	}
}

// broadcastPresence sends presence to ALL connected users (single-tenant P0).
func (h *Hub) broadcastPresence(userID uuid.UUID, name string, online bool) {
	f := Frame{Type: "presence", UserID: userID.String(), Name: name, Online: online}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, cs := range h.conns {
		for c := range cs {
			select {
			case c.send <- f:
			default:
			}
		}
	}
}

// KickUser drops every live conn of a user — password change, account
// deactivation, offboarding. Conns are collected under the lock but
// closed after it is released: a dead peer's close handshake would
// otherwise stall every broadcast for seconds.
// 断开某用户的全部连接——改密、停用、离职。锁内只取连接快照，锁外关闭：
// 僵死对端的关闭握手会把每次广播拖住数秒。
func (h *Hub) KickUser(userID uuid.UUID) {
	h.mu.RLock()
	var cs []*conn
	for c := range h.conns[userID] {
		cs = append(cs, c)
	}
	h.mu.RUnlock()
	for _, c := range cs {
		c.kill()
	}
}

// KickSession drops only conns bound to one session — a single-device
// logout must not knock the user's other devices offline.
// 仅断开绑定某会话的连接——单设备登出不能把用户其它设备踢下线。
func (h *Hub) KickSession(userID, sessID uuid.UUID) {
	h.mu.RLock()
	var cs []*conn
	for c := range h.conns[userID] {
		if c.sessID == sessID {
			cs = append(cs, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range cs {
		c.kill()
	}
}

// Serve runs one websocket connection until close.
// authenticate is done by the caller; we only need the user id.
// revalidate is polled every ~20s — returning false drops the conn, so a
// deleted session or deactivated user can't keep streaming.
// revalidate 每约 20 秒复核一次；返回 false 即断开——已删会话或停用
// 用户不能继续收推送。
func (h *Hub) Serve(ctx context.Context, w *websocket.Conn, userID uuid.UUID, displayName string, sessID uuid.UUID, revalidate func(context.Context) bool) {
	ctx, cancel := context.WithCancel(ctx)
	c := &conn{userID: userID, sessID: sessID, ws: w, send: make(chan Frame, 64), cancel: cancel}

	h.mu.Lock()
	if h.conns[userID] == nil {
		h.conns[userID] = map[*conn]struct{}{}
	}
	first := len(h.conns[userID]) == 0
	h.conns[userID][c] = struct{}{}
	h.mu.Unlock()

	if first {
		h.broadcastPresence(userID, displayName, true)
		for _, fn := range h.onlineHooks {
			fn(userID, true)
		}
	}
	// Presence snapshot for the newcomer.
	_ = wsjson.Write(ctx, w, Frame{Type: "presence_init"})

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		for f := range c.send {
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = wsjson.Write(wctx, w, f)
			cancel()
		}
	}()

	stopReval := make(chan struct{})
	if revalidate != nil {
		go func() {
			t := time.NewTicker(20 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stopReval:
					return
				case <-ctx.Done():
					return
				case <-t.C:
					rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
					ok := revalidate(rctx)
					cancel()
					if !ok {
						c.kill()
						return
					}
				}
			}
		}()
	}

	// Single read loop owns the conn until it dies; inbound frames are
	// transient only (typing/ping). CloseRead would race our reads.
	defer func() {
		h.mu.Lock()
		delete(h.conns[userID], c)
		last := len(h.conns[userID]) == 0
		if last {
			delete(h.conns, userID)
		}
		h.mu.Unlock()

		close(stopReval)
		close(c.send)
		<-writeDone
		_ = w.Close(websocket.StatusNormalClosure, "bye")

		if last {
			h.broadcastPresence(userID, displayName, false)
			for _, fn := range h.onlineHooks {
				fn(userID, false)
			}
		}
	}()

	for {
		var in InFrame
		if err := wsjson.Read(ctx, w, &in); err != nil {
			return
		}
		switch in.Type {
		case "ping":
			select {
			case c.send <- Frame{Type: "pong"}:
			default:
			}
		case "typing":
			convID, err := uuid.Parse(in.ConvID)
			if err != nil || h.isMember == nil || !h.isMember(ctx, convID, userID) {
				continue
			}
			if h.typingSink != nil {
				h.typingSink(convID, userID, displayName)
			}
		}
	}
}

// SetTypingSink lets the HTTP layer wire "typing" → member fan-out.
func (h *Hub) SetTypingSink(fn func(convID, from uuid.UUID, name string)) {
	h.typingSink = fn
}
