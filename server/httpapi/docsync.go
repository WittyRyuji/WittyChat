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
// state back to us — that full-state reply (the conn's first SyncStep2)
// replaces every update up to the replay watermark (Docs.Compact).
//
// Access is not a handshake-only check: every conn re-validates its
// session + doc ACL on the keepalive tick, and member removal / doc
// deletion / session revocation kick affected conns immediately.
// 权限不只在握手时检查：每个连接在保活周期复核会话与文档权限；踢人、
// 删文档、吊销会话会立即断开受影响连接。

import (
	"bytes"
	"errors"
	"io"
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
	putVarUint(w, uint64(len(b))) //nolint:gosec // len() is never negative
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
	if n > docMaxMsg || uint64(r.Len()) < n { //nolint:gosec // Len() is never negative
		return nil, errors.New("varbytes out of range")
	}
	b := make([]byte, n)
	_, err = io.ReadFull(r, b)
	return b, err
}

// frame builders / 帧构造
func syncFrame(sub uint64, payload []byte) []byte {
	var w bytes.Buffer
	putVarUint(&w, msgSync)
	putVarUint(&w, sub)
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
