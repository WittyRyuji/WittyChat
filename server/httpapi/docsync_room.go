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
	mu        sync.Mutex
	conns     map[*docConn]struct{}
	dead      bool   // removed from docRooms; joiners must fetch a fresh room
	full      bool   // log at the workspace cap — members see the banner / 日志已到工作区上限
	fullClock uint64 // awareness clock of the pseudo "doc full" state / 伪状态的 awareness 时钟
}

type docConn struct {
	ws           *websocket.Conn
	wmu          sync.Mutex
	docID        uuid.UUID
	uid          uuid.UUID
	owner        uuid.UUID          // doc owner — only their conn may compact / 仅属主连接可压缩
	token        string             // session token, re-validated periodically
	cancel       context.CancelFunc // kicks the conn (ends its read loop)
	replayedTo   int64              // seq watermark this conn has been served / 已下发水位
	awIDs        map[uint64]uint64  // awareness clientIDs seen on this conn
	awLast       []byte             // last awareness payload, replayed to joiners
	readLimit    int64              // per-conn frame cap from docs.log_max_mb / 按文档上限的帧上限
	awSend       chan []byte        // latest-wins awareness queue (1 deep) / awareness 队列（留最新一帧）
	awDropLogged bool               // log oversized-awareness drop once per conn / 超限 awareness 每连接只记一次
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
	// The cap follows docs.log_max_mb (a full-state frame approaches the
	// doc size) plus headroom, bounded by an absolute ceiling.
	// 库默认 32 KiB：文档态一旦超过，全量同步回复会被掐死在握手期。
	// 上限跟随 docs.log_max_mb（全量态帧逼近文档体积）外加余量，且有绝对顶。
	limit := d.docReadLimit(r.Context())
	ws.SetReadLimit(limit)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	owner, _ := d.Docs.OwnerOf(r.Context(), docID)
	c := &docConn{
		ws: ws, docID: docID, uid: sess.UserID, owner: owner, token: token, cancel: cancel,
		awIDs: map[uint64]uint64{}, readLimit: limit, awSend: make(chan []byte, 1),
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
			d.docBroadcast(c.docID, c, awarenessRemove(id, clock+1), true)
		}
		_ = c.ws.Close(websocket.StatusPolicyViolation, "closed")
	}()

	// Awareness fan-out runs off the read loop: latest-wins queue so a
	// dead peer can't stall cursor updates for the whole room (sync
	// frames stay on the serial path — they're durable).
	// awareness 扇出走独立队列：留最新一帧，僵死对端不会拖住全房间的
	// 光标更新（sync 帧必须可靠，仍走串行路径）。
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-c.awSend:
				wctx, wcancel := context.WithTimeout(ctx, 3*time.Second)
				_ = c.write(wctx, f)
				wcancel()
			}
		}
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
	// instead of waiting for their next cursor move. Also surface a
	// "doc full" state the room already knows about (or the DB reports —
	// e.g. a fresh room after a restart). / 回放对端最新 awareness 让
	// 在线状态即时可见；房间已知（或库里已是）"写满"状态也一并下发——
	// 例如重启后刚建的新房间。
	room.mu.Lock()
	var awFrames [][]byte
	for peer := range room.conns {
		if peer != c && len(peer.awLast) > 0 {
			awFrames = append(awFrames, awarenessFrame(peer.awLast))
		}
	}
	full, fullClock := room.full, room.fullClock
	room.mu.Unlock()
	for _, f := range awFrames {
		if err := c.write(ctx, f); err != nil {
			return
		}
	}
	if !full && d.docIsFull(ctx, c.docID) {
		room.mu.Lock()
		if !room.full {
			room.full = true
			room.fullClock++
		}
		full, fullClock = room.full, room.fullClock
		room.mu.Unlock()
	}
	if full {
		if err := c.write(ctx, docFullAwareness(fullClock)); err != nil {
			return
		}
	}

	for {
		mt, raw, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		if mt != websocket.MessageBinary || len(raw) == 0 || int64(len(raw)) > c.readLimit {
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
					// but the owner can rewrite the whole history. force
					// when the room is marked full — log_bytes can't reach
					// the cap through appends (the tipping write rolls
					// back), so a full doc below the byte threshold would
					// otherwise be stuck. / 属主回放后的全量态→压缩（大
					// 日志或已超字节上限，旧行归档）。房间已标记写满时强制
					// 执行——log_bytes 到不了字节上限（越限写会回滚），
					// 否则低于阈值的"满"文档会永久卡死。
					room.mu.Lock()
					force := room.full
					room.mu.Unlock()
					var done bool
					done, perr = d.Docs.CompactIfLarge(ctx, c.docID, data, c.replayedTo, d.docMaxLogBytes(ctx), force)
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
						// Over the workspace cap: do NOT relay what was
						// never persisted — peers would trust an edit the
						// store dropped. Mark the room full and notify
						// everyone; the owner's next snapshot compacts
						// the log back under the cap.
						// 超过工作区上限：未落库的更新绝不转发——否则对端
						// 会信任一条被丢弃的编辑。标记房间写满并通知所有人；
						// 属主下次快照会把日志压回上限内。
						d.docSetFull(c.docID, room)
						continue
					}
					return // doc deleted or DB down: stop relaying / 文档已删或库不可用
				}
				d.docClearFull(c.docID, room)
				d.docBroadcast(c.docID, c, raw, false)
			}
		case msgAwareness:
			data, err := getVarBytes(r)
			if err != nil {
				continue
			}
			if len(data) > awMaxPayload {
				// Awareness is ephemeral and normally a few hundred
				// bytes — a giant frame is just memory pressure. Drop it
				// (log once per conn) instead of relaying/retaining it.
				// awareness 是暂态的，正常只有几百字节——超大帧纯属内存
				// 压力。丢弃（每连接只记一次）而非转发/留存。
				if !c.awDropLogged {
					log.Printf("docsync %s: oversized awareness dropped (%d B)", c.docID, len(data))
					c.awDropLogged = true
				}
				continue
			}
			for id, clock := range awarenessClients(data) {
				c.awIDs[id] = clock
			}
			room.mu.Lock()
			c.awLast = data
			room.mu.Unlock()
			d.docBroadcast(c.docID, c, raw, true)
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

// docReadLimit — per-conn frame cap, derived from the doc size cap: a
// whole-state SyncStep2 approaches the doc size, so the limit tracks
// docs.log_max_mb with headroom, floored and absolutely capped.
// 每连接帧上限：全量态帧逼近文档体积，故跟随 docs.log_max_mb 加余量，
// 有下限也有绝对顶（防设置被调成天文数字）。
func (d *Deps) docReadLimit(ctx context.Context) int64 {
	maxB := d.docMaxLogBytes(ctx)
	if maxB <= 0 {
		return docMsgCeiling // unlimited logs still get an absolute frame cap
	}
	lim := maxB + 8<<20
	if lim < docReadFloor {
		return docReadFloor
	}
	if lim > docMsgCeiling {
		return docMsgCeiling
	}
	return lim
}

// docIsFull reports whether the doc's log is effectively at the workspace
// cap — checked once per join so a freshly-created room (e.g. after a
// restart) still shows the banner. "Effectively" = within 1/8 of the cap:
// log_bytes can never reach the cap through appends (the tipping write
// rolls back), so the honest signal is runtime ErrLogFull; this margin
// catches the stuck state, and any append that still succeeds clears the
// flag on its own. / 判断文档日志是否"实际"到达上限——每次加入查一次，
// 重启后新建房间也能显示横幅。"实际"=距上限 1/8 以内：log_bytes 永远
// 到不了上限（越限写会回滚），真实信号是运行时的 ErrLogFull；这个余量
// 能识别卡死态，而仍能成功的追加会自行清除标记。
func (d *Deps) docIsFull(ctx context.Context, docID uuid.UUID) bool {
	maxB := d.docMaxLogBytes(ctx)
	if maxB <= 0 {
		return false
	}
	var lb int64
	err := d.Pool.QueryRow(ctx,
		`SELECT COALESCE(log_bytes,0) FROM docs WHERE id=$1`, docID).Scan(&lb)
	return err == nil && lb+maxB/8 >= maxB
}

// docSetFull marks the room full and notifies every conn — a pseudo
// awareness state (clientID 0, {"docFull":true}) the stock y-websocket
// client accepts without any protocol extension. State transitions only,
// so a spammy writer can't flood the room. / 标记房间写满并通知所有
// 连接——伪 awareness 状态（clientID 0），原版 y-websocket 客户端原生
// 可解，不需协议扩展。只在状态翻转时广播，刷写的客户端泛不了洪。
func (d *Deps) docSetFull(docID uuid.UUID, room *docRoom) {
	room.mu.Lock()
	if room.full {
		room.mu.Unlock()
		return
	}
	room.full = true
	room.fullClock++
	clock := room.fullClock
	room.mu.Unlock()
	log.Printf("docsync %s: log at cap — updates rejected until owner compaction", docID)
	d.docBroadcast(docID, nil, docFullAwareness(clock), false)
}

// docClearFull lifts the full mark after an update persisted again
// (owner compacted the log). Broadcasts the removal once.
// 成功落库后解除写满标记（属主已把日志压回上限内），一次性广播清除。
func (d *Deps) docClearFull(docID uuid.UUID, room *docRoom) {
	room.mu.Lock()
	if !room.full {
		room.mu.Unlock()
		return
	}
	room.full = false
	room.fullClock++
	clock := room.fullClock
	room.mu.Unlock()
	d.docBroadcast(docID, nil, docFullRemove(clock), false)
}

// docBroadcast sends a frame to every conn in the room except src.
// Ephemeral frames (cursor/presence awareness) go through each conn's
// latest-wins queue; durable frames get a fresh per-conn write timeout
// so one dead peer can't eat everyone else's budget.
// 向房间内除 src 外的每个连接发帧。暂态帧（光标/在场）走各连接"留
// 最新"队列；持久帧逐连接独立超时——一个僵死对端不会吃掉别人的预算。
func (d *Deps) docBroadcast(docID uuid.UUID, src *docConn, frame []byte, ephemeral bool) {
	for _, c := range docRoomConns(docID) {
		if c == src {
			continue
		}
		if ephemeral {
			select {
			case c.awSend <- frame:
			default: // slow consumer keeps only the newest state / 慢消费者只留最新态
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = c.write(ctx, frame)
		cancel()
	}
}
