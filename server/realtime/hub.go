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
}

// InFrame is anything a client sends over WS.
type InFrame struct {
	Type   string `json:"type"` // typing | ping
	ConvID string `json:"conv_id,omitempty"`
}

type conn struct {
	userID uuid.UUID
	ws     *websocket.Conn
	send   chan Frame
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

// Serve runs one websocket connection until close.
// authenticate is done by the caller; we only need the user id.
func (h *Hub) Serve(ctx context.Context, w *websocket.Conn, userID uuid.UUID, displayName string) {
	c := &conn{userID: userID, ws: w, send: make(chan Frame, 64)}

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
