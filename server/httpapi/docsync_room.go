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
	"sync/atomic"
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

// docFullRenewEvery keeps the relay-owned {"docFull":true} awareness
// state alive — remote awareness states expire client-side after ~30s,
// so a one-shot notice would silently un-banner a still-full doc.
// 伪 awareness 续约周期——远端 awareness 状态约 30s 后在客户端过期，
// 单次通知会让仍写满的文档悄悄失去横幅。
const docFullRenewEvery = 15 * time.Second

// docMaxConnsPerUser bounds one user's sockets per doc — a member could
// otherwise open unbounded conns and multiply the big-frame memory
// pressure. Generous for multi-tab use. / 单用户单文档连接上限——否则
// 成员可无限开 socket 放大帧内存压力。对多标签页留了余量。
const docMaxConnsPerUser = 8

// docInFlightCap bounds the total bytes of frames currently being read
// or handled across ALL doc conns. A conn whose frame pushes the total
// over the budget is killed — bounding concurrent big-frame spikes while
// letting normal-size sync proceed. / 全局在途帧字节预算——超限连接被
// 断开，把并发大帧尖峰压进预算内。
const docInFlightCap = 512 << 20 // 512 MiB

// docOutQueueCap bounds each conn's durable outbound queue (frame refs,
// not copies) and docOutBytesCap the queued bytes a slow consumer may
// pin — past it the conn is dead weight and gets dropped.
// 每连接持久出站队列上限（帧引用非拷贝）与慢消费者可积压的字节上限——
// 超过即视为僵死连接断开。
const (
	docOutQueueCap = 64
	docOutBytesCap = 64 << 20 // 64 MiB
)

var docInFlight atomic.Int64 // bytes being processed by doc read loops

type docRoom struct {
	mu         sync.Mutex
	conns      map[*docConn]struct{}
	dead       bool   // removed from docRooms; joiners must fetch a fresh room
	full       bool   // log at the workspace cap — updates rejected until compacted / 日志到工作区上限
	fullClock  uint64 // awareness clock of the pseudo "doc full" state / 伪状态的 awareness 时钟
	fullPulled bool   // owner already pulled this full-episode — resists the full→compact→full loop / 本轮已满拉过属主——防写满↔压实死循环
	sv         []byte // maintained state vector of the persisted log / 已落库日志的状态向量
	svInit     bool   // sv was loaded from the DB at least once / sv 已从库里加载过
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
	outQ         chan []byte        // durable outbound frames; full → conn dies / 持久出站队列
	outBytes     atomic.Int64       // bytes pinned by outQ (kill watermark) / outQ 积压字节水位
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
		outQ: make(chan []byte, docOutQueueCap),
	}
	d.docServe(ctx, c)
}

// write serializes WS sends per conn. 每连接串行写。
func (c *docConn) write(ctx context.Context, frame []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.Write(ctx, websocket.MessageBinary, frame)
}

// enqueue queues a durable frame for this conn's writer goroutine.
// Nonblocking: a conn whose queue (or pinned bytes) is full can't keep
// up — it gets killed so one dead peer can't stall or pin the room.
// false means the conn is being dropped. / 持久帧进连接出站队列，非
// 阻塞：队列（或积压字节）满说明对端跟不上——断开它，免得一个僵死
// 对端拖住/占住整个房间。
func (c *docConn) enqueue(f []byte) bool {
	if c.outBytes.Load() > docOutBytesCap {
		return false
	}
	select {
	case c.outQ <- f:
		c.outBytes.Add(int64(len(f)))
		return true
	default:
		return false
	}
}

// sendAwareness latest-wins: drop the queued stale frame and keep the
// newest — presence is ephemeral; an old cursor position is worse than
// none. / 真·留最新：顶掉队列里的旧帧放新帧——在场态是暂态，旧光标
// 比没有更糟。
func (c *docConn) sendAwareness(f []byte) {
	select {
	case c.awSend <- f:
		return
	default:
	}
	select {
	case <-c.awSend:
	default:
	}
	select {
	case c.awSend <- f:
	default:
	}
}

// writerLoop drains both outbound queues — durable frames first (they
// matter), awareness opportunistically. One goroutine per conn so a slow
// peer never serializes the room. / 出站写循环——持久帧优先、awareness
// 顺带写。每连接一个 goroutine，慢对端不再串行拖住全房间。
func (c *docConn) writerLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-c.outQ:
			c.outBytes.Add(-int64(len(f)))
			wctx, wc := context.WithTimeout(ctx, 10*time.Second)
			_ = c.write(wctx, f)
			wc()
		default:
			select {
			case <-ctx.Done():
				return
			case f := <-c.outQ:
				c.outBytes.Add(-int64(len(f)))
				wctx, wc := context.WithTimeout(ctx, 10*time.Second)
				_ = c.write(wctx, f)
				wc()
			case f := <-c.awSend:
				wctx, wc := context.WithTimeout(ctx, 3*time.Second)
				_ = c.write(wctx, f)
				wc()
			}
		}
	}
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

	// One user can't flood the doc with sockets — over the cap the new
	// conn is refused before it costs replay bandwidth.
	// 单用户连接数封顶——超限的新连接在耗费回放带宽前就被拒。
	room.mu.Lock()
	own := 0
	for p := range room.conns {
		if p != c && p.uid == c.uid {
			own++
		}
	}
	room.mu.Unlock()
	if own >= docMaxConnsPerUser {
		return
	}

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

	// The room's maintained state vector tells clients exactly what the
	// server holds: members answer Step1(sv) with only their diff, so a
	// reconnect no longer re-appends the whole doc. The owner gets the
	// empty vector ONLY when a compaction snapshot is actually wanted
	// (log ≥ compactMinRows, room full, or a near-cap log after
	// restart) — every other join costs just a diff. Load it once per
	// room (lazy), refreshed on every append.
	// 房间维护的状态向量告诉客户端服务端持有的精确范围：成员对
	// Step1(sv) 只回差量，重连不再把整篇文档追加回来。属主只在真正需要
	// 压缩快照时（行数达标、房间写满、或重启后日志逼近上限）才收空
	// 向量——其余连接只花一个差量。每房间惰性加载一次，随追加刷新。
	room.mu.Lock()
	needSV := !room.svInit
	full := room.full
	room.mu.Unlock()
	var sv []byte
	var logBytes, nRows int64
	if needSV {
		var lerr error
		var nsv []byte
		nsv, logBytes, nRows, lerr = d.Docs.LoadDocHead(ctx, c.docID)
		if lerr == nil {
			room.mu.Lock()
			if !room.svInit {
				room.sv, room.svInit = nsv, true
			}
			sv = room.sv
			room.mu.Unlock()
		}
	} else {
		room.mu.Lock()
		sv = room.sv
		room.mu.Unlock()
	}
	maxB := d.docMaxLogBytes(ctx)
	nearCap := maxB > 0 && logBytes > 0 && logBytes+maxB/8 >= maxB
	pull := sv
	if len(pull) == 0 {
		pull = emptyStateVector
	}
	if c.uid == c.owner && (full || nearCap || nRows >= docs.CompactMinRows || len(sv) == 0) {
		// Pull the full state: it becomes the compaction snapshot
		// (coverage-checked at apply time — a diff is never compacted).
		// 拉全量态——落库前做覆盖校验，差量绝不被当成快照压实。
		pull = emptyStateVector
	}
	if err := c.write(ctx, syncFrame(syncStep1, pull)); err != nil {
		return
	}

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

	// Outbound writer starts only now — anything queued during replay
	// flushes after the handshake, preserving replay order.
	// 出站写协程此刻才启动——回放期间排队的帧在握手后按序发出。
	go c.writerLoop(ctx)

	// Replay peers' latest awareness so presence shows up instantly.
	// Surface "doc full" when the room knows it (or the DB hints at a
	// stuck log after a restart — informational only; the honest signal
	// is a runtime ErrLogFull). / 回放对端最新 awareness 让在线状态
	// 即时可见；房间已知（或库中日志逼近上限——仅提示，真实信号是
	// 运行时 ErrLogFull）"写满"也一并下发。
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
	if !full && nearCap {
		_ = c.write(ctx, docFullAwareness(fullClock+1))
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
		if docInFlight.Add(int64(len(raw))) > docInFlightCap {
			log.Printf("docsync %s: in-flight frame budget exceeded — dropping conn", c.docID)
			return
		}
		d.docHandleFrame(ctx, room, c, mt, raw)
		docInFlight.Add(-int64(len(raw)))
	}
}

// docHandleFrame processes one websocket message. Split from the read
// loop so the in-flight accounting stays single-line.
// 处理一条 ws 消息（从读循环拆出，便于在途字节记账）。
func (d *Deps) docHandleFrame(ctx context.Context, room *docRoom, c *docConn, mt websocket.MessageType, raw []byte) {
	if mt != websocket.MessageBinary || len(raw) == 0 || int64(len(raw)) > c.readLimit {
		return
	}
	r := bytes.NewReader(raw)
	mtype, err := getVarUint(r)
	if err != nil {
		return
	}
	switch mtype {
	case msgSync:
		sub, err := getVarUint(r)
		if err != nil {
			return
		}
		data, err := getVarBytes(r)
		if err != nil {
			return
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
				c.cancel()
				return
			}
			for _, u := range log2 {
				if !c.enqueue(syncFrame(syncStep2, u)) {
					c.cancel()
					return
				}
			}
			if len(log2) == 0 {
				if !c.enqueue(syncFrame(syncStep2, emptyYjsUpdate)) {
					c.cancel()
					return
				}
			}
			if maxSeq > c.replayedTo {
				c.replayedTo = maxSeq
			}
		case syncUpd, syncStep2:
			if len(data) == 0 || bytes.Equal(data, emptyYjsUpdate) {
				return
			}
			// While the room is full every update is refused — a
			// persisted-but-unbroadcastable write would wedge the
			// sender's later edits as pending structs (divergence).
			// The only writable path left is the owner's compaction
			// snapshot. / 房间写满期间所有更新一律拒收——落库却不能
			// 广播的写会把发送者后续编辑卡成悬挂结构（分叉）。唯一可写
			// 路径只剩属主的压缩快照。
			room.mu.Lock()
			roomFull := room.full
			svNow := room.sv
			room.mu.Unlock()
			// A SyncStep2 covering the maintained vector is a full-state
			// push — only those may compact. A diff (answering
			// Step1(sv)) flows through the ordinary append path; an
			// owner spoofing Step2 can't rewrite history as a diff.
			// 覆盖维护向量的 SyncStep2 才是全量态——只有它能压实；差量
			// （应答 Step1(sv)）走普通追加；属主伪造 Step2 也无法把
			// 差量写成历史。
			ownerSnap := sub == syncStep2 && c.uid == c.owner &&
				c.replayedTo > 0 &&
				(len(svNow) == 0 || docs.UpdateCoversSV(data, svNow))
			if roomFull && !ownerSnap {
				return
			}
			var sv []byte
			var perr error
			compacted := false
			if ownerSnap {
				// Owner's full state → compact (large logs only,
				// replaced rows archived). force when the room is marked
				// full; if the plain append tips the cap the snapshot
				// itself is forced through compaction — one push
				// unstucks the doc. / 属主全量态→压缩（仅大日志/写满时
				// 强制；若普通追加越限则当场强制压缩——一次推送即解卡）。
				var done bool
				done, sv, perr = d.Docs.CompactIfLarge(ctx, c.docID, data, c.replayedTo, d.docMaxLogBytes(ctx), roomFull)
				if perr == nil && !done {
					sv, perr = d.Docs.AppendUpdate(ctx, c.docID, data, d.docMaxLogBytes(ctx))
					if errors.Is(perr, docs.ErrLogFull) {
						done, sv, perr = d.Docs.CompactIfLarge(ctx, c.docID, data, c.replayedTo, d.docMaxLogBytes(ctx), true)
					}
				}
				compacted = done
			} else {
				// A Step2 replying to our pull a beat late echoes ranges
				// the log just persisted (e.g. the join-pull raced the
				// client's first edit). Pure duplication → skip, don't
				// bloat the log. / Step2 应答迟到一拍会把刚落库的区间
				// 又推回来——纯重复，跳过不写。
				if len(svNow) > 0 && docs.UpdateFullyInSV(data, svNow) {
					return
				}
				sv, perr = d.Docs.AppendUpdate(ctx, c.docID, data, d.docMaxLogBytes(ctx))
			}
			if perr != nil {
				if errors.Is(perr, docs.ErrLogFull) {
					// Over the workspace cap: do NOT relay what was
					// never persisted — peers would trust an edit the
					// store dropped. Mark the room full (sticky) and
					// notify; the owner's snapshot unwedges it.
					// 超过工作区上限：未落库的更新绝不转发——对端会信任
					// 一条被丢弃的编辑。标记房间写满（粘滞）并通知；
					// 属主快照会解开。
					d.docSetFull(c.docID, room)
					return
				}
				c.cancel() // doc deleted or DB down / 文档已删或库不可用
				return
			}
			room.mu.Lock()
			room.sv = sv
			if !compacted {
				// A real append landed → the doc can grow again; the next
				// overflow earns a fresh owner pull. / 普通追加成功说明
				// 文档又能涨了，下次写满重新允许拉属主。
				room.fullPulled = false
			}
			room.mu.Unlock()
			if compacted {
				d.docClearFull(c.docID, room)
				return // snapshot consumed — not an update to relay / 快照已消费，不再转发
			}
			d.docBroadcast(c.docID, c, raw, false)
		}
	case msgAwareness:
		data, err := getVarBytes(r)
		if err != nil {
			return
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
			return
		}
		// The relay owns clientID 0 ({docFull} state) — strip forged or
		// echoed entries so a member can't fake the banner or get it
		// removed when their conn dies. / 保留 clientID 0 归中继所有——
		// 剥掉伪造/回显条目：成员不能伪造横幅，断线也不会误删它。
		data = awarenessStripReserved(data)
		if data == nil {
			return
		}
		for id, clock := range awarenessClients(data) {
			c.awIDs[id] = clock
		}
		room.mu.Lock()
		c.awLast = data
		room.mu.Unlock()
		// Relay the STRIPPED frame (raw carries the forged entry).
		var w bytes.Buffer
		putVarUint(&w, msgAwareness)
		putVarBytes(&w, data)
		d.docBroadcast(c.docID, c, w.Bytes(), true)
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

// docSetFull marks the room full and notifies every conn — a pseudo
// awareness state (clientID 0, {"docFull":true}) the stock y-websocket
// client accepts without any protocol extension. Sticky until owner
// compaction; a 15s renewal keeps it past the client's 30s remote-state
// expiry. / 标记房间写满并通知所有连接——伪 awareness 状态（clientID 0），
// 原版客户端原生可解。粘滞到属主压缩；15s 续约防客户端 30s 远端过期。
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
	go d.docFullRenew(docID, room)
	// Pull the owner's full state NOW if one is connected — the empty
	// vector makes its client answer with a snapshot, which compacts and
	// clears the flag without waiting for a reconnect. Once per full
	// episode: if a post-compact diff overflows again, re-pulling would
	// snapshot an unchanged owner doc forever (archive churn). The flag
	// resets only when a normal append lands — i.e. the doc truly made
	// progress. / 若属主在线立刻拉全量，当场压满清标，不必等重连。
	// 每个写满周期只拉一次：压实后的差量再超限说明属主快照不会再变，
	// 反复拉取只会空转归档。标志仅在普通追加成功后复位——即文档真正
	// 取得进展时。
	room.mu.Lock()
	pulled := room.fullPulled
	room.fullPulled = true
	room.mu.Unlock()
	if pulled {
		return
	}
	for _, c := range docRoomConns(docID) {
		if c.uid == c.owner && !c.enqueue(syncFrame(syncStep1, emptyStateVector)) {
			c.cancel()
		}
	}
}

// docFullRenew re-broadcasts the pseudo state while the room stays full
// so the banner survives the client's awareness expiry.
// 写满期间周期性重广播伪状态，横幅不再随 awareness 过期消失。
func (d *Deps) docFullRenew(docID uuid.UUID, room *docRoom) {
	t := time.NewTicker(docFullRenewEvery)
	defer t.Stop()
	for range t.C {
		room.mu.Lock()
		if !room.full || room.dead {
			room.mu.Unlock()
			return
		}
		room.fullClock++
		clock := room.fullClock
		room.mu.Unlock()
		d.docBroadcast(docID, nil, docFullAwareness(clock), true)
	}
}

// docClearFull lifts the full mark after the owner's compaction made
// room again — removal broadcast, then a fresh Step1(sv) pull so members
// whose edits were rejected while full resend their diff. / 属主压缩腾出
// 空间后解除写满——广播移除，并向各连接下发 Step1(sv)，让写满期间被拒
// 的编辑以差量补回。
func (d *Deps) docClearFull(docID uuid.UUID, room *docRoom) {
	room.mu.Lock()
	if !room.full {
		room.mu.Unlock()
		return
	}
	room.full = false
	room.fullClock++
	clock := room.fullClock
	sv := room.sv
	if len(sv) == 0 {
		sv = emptyStateVector
	}
	room.mu.Unlock()
	d.docBroadcast(docID, nil, docFullRemove(clock), false)
	for _, c := range docRoomConns(docID) {
		if !c.enqueue(syncFrame(syncStep1, sv)) {
			c.cancel()
		}
	}
}

// docBroadcast sends a frame to every conn in the room except src.
// Ephemeral frames (cursor/presence awareness) go through each conn's
// latest-wins queue; durable frames queue per conn — a full queue means
// a dead peer, which is dropped instead of stalling everyone.
// 向房间内除 src 外的每个连接发帧。暂态帧走"留最新"队列；持久帧逐
// 连接入队——队列满=僵死对端，断开它而不是拖住所有人。
func (d *Deps) docBroadcast(docID uuid.UUID, src *docConn, frame []byte, ephemeral bool) {
	for _, c := range docRoomConns(docID) {
		if c == src {
			continue
		}
		if ephemeral {
			c.sendAwareness(frame)
			continue
		}
		if !c.enqueue(frame) {
			c.cancel()
		}
	}
}
