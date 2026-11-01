package httpapi

// Yjs realtime sync relay — a self-contained y-websocket-protocol
// implementation so the product stays a single Go binary (no Node
// sidecar). The relay treats Yjs update blobs as opaque, idempotent
// frames: persist + broadcast + replay. It never parses CRDT internals.
//
// Yjs 实时协同中继：内嵌 y-websocket 协议实现，保持单 Go 二进制（免
// Node 侧车）。update 帧按不透明幂等负载处理：落库 + 广播 + 回放，
// 不解析 CRDT 内部结构。
//
// Protocol (y-protocols): every WS binary message =
//   varint msgType | payload
//   msgType 0 = sync:    varint subType | varBytes data
//                        subType 0=SyncStep1 1=SyncStep2 2=Update
//   msgType 1 = awareness: varBytes awarenessUpdate (relay verbatim)
//
// Handshake: client sends SyncStep1(stateVector) → we reply SyncStep2
// per stored update (extra updates apply idempotently client-side).
// We also push SyncStep1(empty sv) so the client flushes its offline
// state back to us — that full-state dump doubles as log compaction.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

const (
	msgSync           = 0
	msgAwareness      = 1
	msgQueryAwareness = 3

	syncStep1 = 0
	syncStep2 = 1
	syncUpd   = 2

	docMaxMsg = 4 << 20 // 4 MiB frame cap
)

// emptyYjsUpdate is a valid zero-content Yjs update (0 structs, 0
// delete-set items) — sent as SyncStep2 when the log is empty so the
// client's decoder doesn't choke on a zero-length payload.
// 合法的空 Yjs update（0 结构 + 0 删除集），日志为空时作 SyncStep2
// 回帧——空 payload 会让客户端解码器报 Unexpected end of array。
var emptyYjsUpdate = []byte{0x00, 0x00}

// emptyStateVector is a valid empty Yjs state vector (0 entries) — the
// SyncStep1 payload asking the client to send everything it has.
// 合法空状态向量（0 条目），作 SyncStep1 载荷拉取客户端全量状态。
var emptyStateVector = []byte{0x00}

// ---- lib0 varint helpers (LEB128) ----

func putVarUint(w *bytes.Buffer, n uint64) {
	for n >= 0x80 {
		w.WriteByte(byte(n) | 0x80)
		n >>= 7
	}
	w.WriteByte(byte(n))
}

func putVarBytes(w *bytes.Buffer, b []byte) {
	putVarUint(w, uint64(len(b)))
	w.Write(b)
}

func getVarUint(r *bytes.Reader) (uint64, error) {
	var n uint64
	for i := uint(0); ; i += 7 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		n |= uint64(b&0x7f) << i
		if b < 0x80 {
			return n, nil
		}
		if i > 63 {
			return 0, errors.New("varint overflow")
		}
	}
}

func getVarBytes(r *bytes.Reader) ([]byte, error) {
	n, err := getVarUint(r)
	if err != nil {
		return nil, err
	}
	if n > docMaxMsg || uint64(r.Len()) < n {
		return nil, errors.New("varbytes out of range")
	}
	b := make([]byte, n)
	_, err = io.ReadFull(r, b)
	return b, err
}

// frame builders / 帧构造
func syncFrame(sub int, payload []byte) []byte {
	var w bytes.Buffer
	putVarUint(&w, msgSync)
	putVarUint(&w, uint64(sub))
	putVarBytes(&w, payload)
	return w.Bytes()
}

func awarenessFrame(payload []byte) []byte {
	var w bytes.Buffer
	putVarUint(&w, msgAwareness)
	putVarBytes(&w, payload)
	return w.Bytes()
}

// awarenessRemove builds the "client gone" awareness update:
// count=1, {clientID, clock, "null"}.
func awarenessRemove(clientID, clock uint64) []byte {
	var p bytes.Buffer
	putVarUint(&p, 1)
	putVarUint(&p, clientID)
	putVarUint(&p, clock)
	putVarBytes(&p, []byte("null"))
	return awarenessFrame(p.Bytes())
}

// awarenessClients extracts (clientID, clock) pairs — needed to emit
// removal notices when a conn drops. 取客户端 id 供断线广播移除。
func awarenessClients(b []byte) map[uint64]uint64 {
	out := map[uint64]uint64{}
	r := bytes.NewReader(b)
	n, err := getVarUint(r)
	if err != nil {
		return out
	}
	for ; n > 0; n-- {
		id, err1 := getVarUint(r)
		clock, err2 := getVarUint(r)
		if _, err3 := getVarBytes(r); err1 != nil || err2 != nil || err3 != nil {
			break
		}
		out[id] = clock
	}
	return out
}

// ---- room broker ----

type docRoom struct {
	mu    sync.Mutex
	conns map[*docConn]struct{}
}

type docConn struct {
	ws     *websocket.Conn
	wmu    sync.Mutex
	docID  uuid.UUID
	uid    uuid.UUID
	awIDs  map[uint64]uint64 // awareness clientIDs seen on this conn
	awLast []byte            // last awareness payload, replayed to joiners
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

func (d *Deps) docRoom(docID uuid.UUID) *docRoom {
	v, _ := docRooms.LoadOrStore(docID, &docRoom{conns: map[*docConn]struct{}{}})
	return v.(*docRoom)
}

// docWS — GET /ws/doc/{id}?token= : auth → ACL → join room → relay.
func (d *Deps) docWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
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
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c := &docConn{ws: ws, docID: docID, uid: sess.UserID, awIDs: map[uint64]uint64{}}
	d.docServe(r.Context(), c)
}

// write serializes WS sends per conn. 每连接串行写。
func (c *docConn) write(ctx context.Context, frame []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.Write(ctx, websocket.MessageBinary, frame)
}

func (d *Deps) docServe(ctx context.Context, c *docConn) {
	room := d.docRoom(c.docID)
	room.mu.Lock()
	room.conns[c] = struct{}{}
	room.mu.Unlock()
	defer func() {
		room.mu.Lock()
		delete(room.conns, c)
		room.mu.Unlock()
		for id, clock := range c.awIDs {
			d.docBroadcast(c.docID, c, awarenessRemove(id, clock+1))
		}
		_ = c.ws.Close(websocket.StatusNormalClosure, "bye")
	}()

	// Replay stored updates: one SyncStep2 frame per update (empty
	// SyncStep2 when the log is empty so the client finishes its
	// handshake either way).
	updates, err := d.Docs.LoadUpdates(ctx, c.docID)
	if err != nil {
		return
	}
	if len(updates) == 0 {
		_ = c.write(ctx, syncFrame(syncStep2, emptyYjsUpdate))
	} else {
		for _, u := range updates {
			if err := c.write(ctx, syncFrame(syncStep2, u)); err != nil {
				return
			}
		}
	}
	// Ask the client for its state — empty state vector pulls its
	// full doc; the reply is a single compact update we persist.
	_ = c.write(ctx, syncFrame(syncStep1, emptyStateVector))

	// Keepalive: the y-websocket client locally closes any conn silent
	// for 30s ("no message received" watchdog) → reconnect storm →
	// duplicate full-state dumps. A periodic QueryAwareness frame
	// resets that timer and makes peers re-announce presence.
	// 保活：y-websocket 客户端 30s 无消息会主动断开重连，风暴式全量
	// 回包。周期性 QueryAwareness 帧既重置看门狗又刷新在场状态。
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-t.C:
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
				// Client asks for what it misses: replay the current
				// log — reload so updates landed since join are included.
				log2, err := d.Docs.LoadUpdates(ctx, c.docID)
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
			case syncUpd, syncStep2:
				// New update: persist then fan out to peers.
				if len(data) > 0 {
					_ = d.Docs.AppendUpdate(ctx, c.docID, data)
					updates = append(updates, data)
					d.docBroadcast(c.docID, c, raw)
				}
			}
		case msgAwareness:
			data, err := getVarBytes(r)
			if err != nil {
				continue
			}
			for id, clock := range awarenessClients(data) {
				c.awIDs[id] = clock
			}
			d.docRoom(c.docID).mu.Lock()
			c.awLast = data
			d.docRoom(c.docID).mu.Unlock()
			d.docBroadcast(c.docID, c, raw)
		}
	}
}

// docBroadcast sends a frame to every conn in the room except src.
func (d *Deps) docBroadcast(docID uuid.UUID, src *docConn, frame []byte) {
	room := d.docRoom(docID)
	room.mu.Lock()
	var targets []*docConn
	for c := range room.conns {
		if c != src {
			targets = append(targets, c)
		}
	}
	room.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, c := range targets {
		_ = c.write(ctx, frame)
	}
}
