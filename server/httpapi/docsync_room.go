package httpapi

// Doc relay connection lifecycle: rooms, serve loop, revalidation, kicks.
// 文档中继连接生命周期：房间、收发循环、权限复核、踢出。

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"
	"wittychat/core/docs"
	"wittychat/core/settings"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// docRevalidateEvery is the keepalive + access re-check period. It must
// stay under the y-websocket client's 30s silence watchdog.
// 保活兼权限复核周期，须小于 y-websocket 客户端 30s 静默看门狗。
const docRevalidateEvery = 20 * time.Second

type docRoom struct {
	mu    sync.Mutex
	conns map[*docConn]struct{}
	dead  bool // removed from docRooms; joiners must fetch a fresh room
}

type docConn struct {
	ws         *websocket.Conn
	wmu        sync.Mutex
	docID      uuid.UUID
	uid        uuid.UUID
	owner      uuid.UUID          // doc owner — only their conn may compact / 仅属主连接可压缩
	token      string             // session token, re-validated periodically
	cancel     context.CancelFunc // kicks the conn (ends its read loop)
	replayedTo int64              // seq watermark this conn has been served / 已下发水位
	awIDs      map[uint64]uint64  // awareness clientIDs seen on this conn
	awLast     []byte             // last awareness payload, replayed to joiners
}

var docRooms sync.Map // uuid.UUID → *docRoom

func docBrokerCount(docID uuid.UUID) int {
	if v, ok := docRooms.Load(docID); ok {
		r := v.(*docRoom)
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.conns)
	}
	return 0
}

// docJoin adds c to its doc's room. A room that emptied out is marked
// dead and dropped from the map under its own lock, so a racing joiner
// retries with a fresh room instead of joining an orphan.
// 加入房间。空房间在自身锁内标记 dead 并移出 map，竞争中的加入者重试
// 新房间，不会进入孤儿房间。
func docJoin(c *docConn) *docRoom {
	for {
		v, _ := docRooms.LoadOrStore(c.docID, &docRoom{conns: map[*docConn]struct{}{}})
		room := v.(*docRoom)
		room.mu.Lock()
		if room.dead {
			room.mu.Unlock()
			continue
		}
		room.conns[c] = struct{}{}
		room.mu.Unlock()
		return room
	}
}

func docLeave(room *docRoom, c *docConn) {
	room.mu.Lock()
	defer room.mu.Unlock()
	delete(room.conns, c)
	if len(room.conns) == 0 {
		room.dead = true
		docRooms.CompareAndDelete(c.docID, room)
	}
}

func docRoomConns(docID uuid.UUID) []*docConn {
	v, ok := docRooms.Load(docID)
	if !ok {
		return nil
	}
	room := v.(*docRoom)
	room.mu.Lock()
	defer room.mu.Unlock()
	out := make([]*docConn, 0, len(room.conns))
	for c := range room.conns {
		out = append(out, c)
	}
	return out
}

// docStillAllowed: session still valid for the same user AND doc ACL holds.
// 会话仍有效（同一用户）且文档权限仍成立。
func (d *Deps) docStillAllowed(ctx context.Context, c *docConn) bool {
	sess, err := d.Store.ValidateSession(ctx, c.token)
	return err == nil && sess.UserID == c.uid && d.docAccess(ctx, c.docID, c.uid)
}

// docRevalidateUser immediately re-checks every live doc conn of uid and
// kicks the ones that lost access (member removed, suspended, password
// changed, session revoked). uuid.Nil re-checks everyone.
// 立即复核 uid 的全部文档连接并踢掉失权者（被踢、停用、改密、吊销）。
func (d *Deps) docRevalidateUser(ctx context.Context, uid uuid.UUID) {
	docRooms.Range(func(k, _ any) bool {
		for _, c := range docRoomConns(k.(uuid.UUID)) {
			if (uid == uuid.Nil || c.uid == uid) && !d.docStillAllowed(ctx, c) {
				c.cancel()
			}
		}
		return true
	})
}

// docRevalidateDoc re-checks every live conn of one doc and kicks the
// ones that lost access (e.g. unshare). 复核一个文档的全部连接并踢掉
// 失权者（如撤销分享）。
func (d *Deps) docRevalidateDoc(ctx context.Context, docID uuid.UUID) {
	for _, c := range docRoomConns(docID) {
		if !d.docStillAllowed(ctx, c) {
			c.cancel()
		}
	}
}

// docCloseRoom kicks everyone off a doc (deleted). 删除文档时清空房间。
func docCloseRoom(docID uuid.UUID) {
	for _, c := range docRoomConns(docID) {
		c.cancel()
	}
}

// docWS — GET /ws/doc/{id} : auth (wt1. subprotocol or ?token=) → ACL →
// join room → relay.
func (d *Deps) docWS(w http.ResponseWriter, r *http.Request) {
	token := wsToken(r)
	sess, err := d.Store.ValidateSession(r.Context(), token)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	docID, err := uuid.Parse(r.PathValue("id"))
	if err != nil || !d.docAccess(r.Context(), docID, sess.UserID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		// Clients that require a negotiated protocol get "wittychat"
		// back — never wt1.<token>. / 需要协商子协议的客户端回选固定值。
		Subprotocols: []string{"wittychat"},
	})
	if err != nil {
		return
	}
	// The library default is 32 KiB — a full-state sync reply for any doc
	// past that size would be killed mid-handshake (every client looped).
	// 库默认 32 KiB：文档态一旦超过，全量同步回复会被掐死在握手期。
	ws.SetReadLimit(docMaxMsg)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	owner, _ := d.Docs.OwnerOf(r.Context(), docID)
	c := &docConn{
		ws: ws, docID: docID, uid: sess.UserID, owner: owner, token: token, cancel: cancel,
		awIDs: map[uint64]uint64{},
	}
	d.docServe(ctx, c)
}

// write serializes WS sends per conn. 每连接串行写。
func (c *docConn) write(ctx context.Context, frame []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.Write(ctx, websocket.MessageBinary, frame)
}

func (d *Deps) docServe(ctx context.Context, c *docConn) {
	room := docJoin(c)
	defer func() {
		docLeave(room, c)
		for id, clock := range c.awIDs {
			d.docBroadcast(c.docID, c, awarenessRemove(id, clock+1))
		}
		_ = c.ws.Close(websocket.StatusPolicyViolation, "closed")
	}()

	// Replay stored updates: one SyncStep2 frame per update (empty
	// SyncStep2 when the log is empty so the client finishes its
	// handshake either way). c.replayedTo is the compaction watermark.
	updates, replayedTo, err := d.Docs.LoadUpdates(ctx, c.docID, 0)
	if err != nil {
		return
	}
	c.replayedTo = replayedTo
	if len(updates) == 0 {
		_ = c.write(ctx, syncFrame(syncStep2, emptyYjsUpdate))
	}
	for _, u := range updates {
		if err := c.write(ctx, syncFrame(syncStep2, u)); err != nil {
			return
		}
	}
	// Ask the client for its state — empty state vector pulls its full
	// doc; the reply (this conn's first SyncStep2) compacts the log.
	_ = c.write(ctx, syncFrame(syncStep1, emptyStateVector))
	snapshotPending := true

	// Keepalive + access re-check (see docRevalidateEvery).
	// 保活 + 权限复核。
	go func() {
		t := time.NewTicker(docRevalidateEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !d.docStillAllowed(ctx, c) {
					c.cancel()
					return
				}
				if err := c.write(ctx, []byte{msgQueryAwareness}); err != nil {
					return
				}
			}
		}
	}()

	// Replay peers' latest awareness so presence shows up instantly
	// instead of waiting for their next cursor move.
	room.mu.Lock()
	var awFrames [][]byte
	for peer := range room.conns {
		if peer != c && len(peer.awLast) > 0 {
			awFrames = append(awFrames, awarenessFrame(peer.awLast))
		}
	}
	room.mu.Unlock()
	for _, f := range awFrames {
		if err := c.write(ctx, f); err != nil {
			return
		}
	}

	for {
		mt, raw, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		if mt != websocket.MessageBinary || len(raw) == 0 || len(raw) > docMaxMsg {
			continue
		}
		r := bytes.NewReader(raw)
		mtype, err := getVarUint(r)
		if err != nil {
			continue
		}
		switch mtype {
		case msgSync:
			sub, err := getVarUint(r)
			if err != nil {
				continue
			}
			data, err := getVarBytes(r)
			if err != nil {
				continue
			}
			switch sub {
			case syncStep1:
				// Client asks for what it misses. The join already sent
				// everything up to c.replayedTo (frames arrive in order),
				// so only the delta since then is due — answering with
				// the whole log again doubled every connect's traffic.
				// 客户端请求补差。连接建立时已按序下发到水位，此处只补增量——
				// 此前每次连接都把全量日志重复下发了一遍。
				log2, maxSeq, err := d.Docs.LoadUpdates(ctx, c.docID, c.replayedTo)
				if err != nil {
					return
				}
				for _, u := range log2 {
					if err := c.write(ctx, syncFrame(syncStep2, u)); err != nil {
						return
					}
				}
				if len(log2) == 0 {
					_ = c.write(ctx, syncFrame(syncStep2, emptyYjsUpdate))
				}
				if maxSeq > c.replayedTo {
					c.replayedTo = maxSeq
				}
			case syncUpd, syncStep2:
				if len(data) == 0 || bytes.Equal(data, emptyYjsUpdate) {
					if sub == syncStep2 {
						snapshotPending = false
					}
					continue
				}
				var perr error
				if sub == syncStep2 && snapshotPending && c.replayedTo > 0 && c.uid == c.owner {
					// Owner's full state after the replay → compact (large
					// logs only, replaced rows archived). Members' first
					// SyncStep2 is treated as an ordinary update so no one
					// but the owner can rewrite the whole history.
					// 属主回放后的全量态→压缩（仅大日志，旧行归档）。成员首帧
					// 按普通 update 处理——除属主外无人能改写整体历史。
					var done bool
					done, perr = d.Docs.CompactIfLarge(ctx, c.docID, data, c.replayedTo)
					if perr == nil && !done {
						perr = d.Docs.AppendUpdate(ctx, c.docID, data, d.docMaxLogBytes(ctx))
					}
				} else {
					perr = d.Docs.AppendUpdate(ctx, c.docID, data, d.docMaxLogBytes(ctx))
				}
				if sub == syncStep2 {
					snapshotPending = false
				}
				if perr != nil {
					if errors.Is(perr, docs.ErrLogFull) {
						// Log at the workspace cap: keep relaying so live
						// collaboration continues; the update just isn't
						// persisted until the owner's next compaction.
						// A late joiner only sees what's in the log.
						// 日志到顶：继续转发保活协作，该 update 只是不落库，
						// 待属主下次压缩恢复持久化；后进者只见日志已有内容。
						log.Printf("docsync %s: log full, update dropped (live-sync only)", c.docID)
					} else {
						return // doc deleted or DB down: stop relaying / 文档已删或库不可用
					}
				}
				d.docBroadcast(c.docID, c, raw)
			}
		case msgAwareness:
			data, err := getVarBytes(r)
			if err != nil {
				continue
			}
			for id, clock := range awarenessClients(data) {
				c.awIDs[id] = clock
			}
			room.mu.Lock()
			c.awLast = data
			room.mu.Unlock()
			d.docBroadcast(c.docID, c, raw)
		}
	}
}

// docMaxLogBytes — docs.log_max_mb workspace cap (0 = unlimited).
// 文档日志字节上限（工作区设置，0=不限）。
func (d *Deps) docMaxLogBytes(ctx context.Context) int64 {
	if d.Settings == nil {
		return 0
	}
	return d.settingInt(ctx, settings.KeyDocsLogMaxMB, settings.DefDocsLogMaxMB) << 20
}

// docBroadcast sends a frame to every conn in the room except src.
func (d *Deps) docBroadcast(docID uuid.UUID, src *docConn, frame []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, c := range docRoomConns(docID) {
		if c != src {
			_ = c.write(ctx, frame)
		}
	}
}
