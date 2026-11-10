package docs

// Minimal Yjs "v1" update reader + state-vector codec. The relay needs the
// per-client contiguous clock ranges an update carries so it can keep a
// server-side state vector per doc: clients then get a real SyncStep1 vector
// and answer with only the diff, instead of every reconnect pushing the
// whole document back into the log. Struct payloads are skipped, not
// decoded — zero allocation beyond the ranges map and the encoded vector.
// A malformed update returns errBadUpdate; callers degrade to "sv unknown"
// (the relay falls back to the empty-vector pull), never to wrong data.
//
// 最小 Yjs v1 update 读取器 + 状态向量编解码。中继需要 update 携带的"每
// 客户端连续时钟区间"来维护服务端状态向量：客户端 SyncStep1 拿到真实向量
// 后只回差量，重连不再把整篇文档灌回日志。struct 载荷只跳过不解码——除区
// 间表与输出向量外零分配。畸形 update 返回 errBadUpdate；调用方降级为
// "sv 未知"（回退空向量拉取），绝不产生错误数据。

import (
	"errors"
	"sort"
	"unicode/utf8"
)

var errBadUpdate = errors.New("docs: malformed yjs update")

// ClockRange is one client's contiguous struct run [Start, End).
// ClockRange：某客户端在 update 中连续覆盖的时钟区间 [Start, End)。
type ClockRange struct {
	Start, End uint64
}

// EncodeSV renders a client→clock map as a Yjs state vector.
// 把 client→clock 表编码为 Yjs 状态向量。
func EncodeSV(sv map[uint64]uint64) []byte {
	clients := make([]uint64, 0, len(sv))
	for c := range sv {
		clients = append(clients, c)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] < clients[j] })
	out := appendVarUint(nil, uint64(len(clients)))
	for _, c := range clients {
		out = appendVarUint(out, c)
		out = appendVarUint(out, sv[c])
	}
	return out
}

// DecodeSV parses a state vector blob back into a map.
// 解析状态向量为 map。
func DecodeSV(b []byte) (map[uint64]uint64, error) {
	r := &updReader{b: b}
	n, err := r.varUint()
	if err != nil {
		return nil, errBadUpdate
	}
	out := make(map[uint64]uint64, n)
	for ; n > 0; n-- {
		c, err1 := r.varUint()
		ck, err2 := r.varUint()
		if err1 != nil || err2 != nil {
			return nil, errBadUpdate
		}
		out[c] = ck
	}
	return out, nil
}

// MergeSV folds parsed update ranges into sv (in place). A range may only
// EXTEND a client's vector when it overlaps or continues what the server
// already has — a gap (e.g. an edit the cap rejected) must keep sv honest,
// or the next pull would tell that client we hold clocks we never stored.
// 把 update 区间并入 sv（原地）。区间只能在与服务端已有时钟重叠/相接时
// 扩展——中间有洞（如被上限拒掉的编辑）必须如实保留缺口，否则下次拉取
// 会告诉客户端我们存了实际没有的时钟。
func MergeSV(sv map[uint64]uint64, ranges map[uint64]ClockRange) {
	for c, rg := range ranges {
		if rg.Start <= sv[c] && rg.End > sv[c] {
			sv[c] = rg.End
		}
	}
}

// ParseUpdateRanges walks a Yjs v1 update and returns each client's
// contiguous clock coverage. 解析 Yjs v1 update，返回各客户端连续覆盖的
// 时钟区间。
func ParseUpdateRanges(data []byte) (map[uint64]ClockRange, error) {
	r := &updReader{b: data}
	nClients, err := r.varUint()
	if err != nil || nClients > uint64(len(data)) {
		return nil, errBadUpdate
	}
	ranges := make(map[uint64]ClockRange, nClients)
	for i := uint64(0); i < nClients; i++ {
		nStructs, err := r.varUint()
		if err != nil {
			return nil, errBadUpdate
		}
		client, err := r.varUint()
		if err != nil {
			return nil, errBadUpdate
		}
		clock, err := r.varUint()
		if err != nil {
			return nil, errBadUpdate
		}
		start := clock
		for j := uint64(0); j < nStructs; j++ {
			info, err := r.byte()
			if err != nil {
				return nil, errBadUpdate
			}
			var l uint64
			switch info & 0x1f { // contentRef: 0=GC, 10=Skip, else Item
			case 0, 10: // GC / Skip: only a length
				l, err = r.varUint()
				if err != nil {
					return nil, errBadUpdate
				}
			default:
				l, err = r.skipItem(info)
				if err != nil {
					return nil, errBadUpdate
				}
			}
			clock += l
		}
		// One block per client normally; merge liberally if the encoder
		// split a client's run into several blocks. / 正常每客户端一块；
		// 若编码器把同一客户端拆成多块则宽容合并。
		rg, ok := ranges[client]
		if !ok || start < rg.Start {
			rg.Start = start
		}
		if clock > rg.End {
			rg.End = clock
		}
		ranges[client] = rg
	}
	// Trailing delete set: consume to prove the blob isn't truncated.
	// 末尾删除集：读完以证明 blob 未被截断。
	nDS, err := r.varUint()
	if err != nil {
		return nil, errBadUpdate
	}
	for ; nDS > 0; nDS-- {
		if _, err = r.varUint(); err != nil { // client
			return nil, errBadUpdate
		}
		nDel, err := r.varUint()
		if err != nil {
			return nil, errBadUpdate
		}
		for ; nDel > 0; nDel-- {
			if _, err = r.varUint(); err != nil { // clock
				return nil, errBadUpdate
			}
			if _, err = r.varUint(); err != nil { // len
				return nil, errBadUpdate
			}
		}
	}
	return ranges, nil
}

// ---- reader ----

type updReader struct {
	b   []byte
	pos int
}

func appendVarUint(dst []byte, n uint64) []byte {
	for n >= 0x80 {
		dst = append(dst, byte(n)|0x80)
		n >>= 7
	}
	return append(dst, byte(n))
}

func (r *updReader) byte() (uint8, error) {
	if r.pos >= len(r.b) {
		return 0, errBadUpdate
	}
	b := r.b[r.pos]
	r.pos++
	return b, nil
}

func (r *updReader) varUint() (uint64, error) {
	var n uint64
	for i := uint(0); ; i += 7 {
		if r.pos >= len(r.b) {
			return 0, errBadUpdate
		}
		b := r.b[r.pos]
		r.pos++
		n |= uint64(b&0x7f) << i
		if b < 0x80 {
			return n, nil
		}
		if i > 63 {
			return 0, errBadUpdate
		}
	}
}

// skip advances past n payload bytes without copying. 跳过 n 字节。
func (r *updReader) skip(n uint64) error {
	if n > uint64(len(r.b)-r.pos) { //nolint:gosec // pos is always within len(b)
		return errBadUpdate
	}
	r.pos += int(n) //nolint:gosec // n is bounded by the check above
	return nil
}

// varSkip skips a varUint-length-prefixed blob (varString/varBytes).
// 跳过一段变长前缀的块（varString/varBytes）。
func (r *updReader) varSkip() error {
	n, err := r.varUint()
	if err != nil {
		return err
	}
	return r.skip(n)
}

// skipID consumes a two-varUint ID (client, clock). 消耗一个 ID。
func (r *updReader) skipID() error {
	if _, err := r.varUint(); err != nil {
		return err
	}
	_, err := r.varUint()
	return err
}

// utf16Len counts JS string length (UTF-16 code units) — ContentString's
// struct length is measured in code units, not bytes/runes.
// 计算 JS 字符串长度（UTF-16 码元数）——ContentString 的 struct 长度按
// 码元计，而非字节/rune。
func utf16Len(b []byte) (uint64, error) {
	var n uint64
	for len(b) > 0 {
		rn, size := utf8.DecodeRune(b)
		if rn == utf8.RuneError && size <= 1 {
			return 0, errBadUpdate
		}
		if rn > 0xFFFF {
			n += 2
		} else {
			n++
		}
		b = b[size:]
	}
	return n, nil
}

// skipItem consumes one Item body after its info byte and returns the
// struct's clock length. Layout (encoding.js readClientsStructRefs):
//
//	BIT8 → leftID | BIT7 → rightID
//	neither set → parentInfo varUint (1: varString key, 0: id) + BIT6 →
//	parentSub varString; then item content by ref.
//
// 消耗一个 Item 体（info 字节已读）并返回其时钟长度。
func (r *updReader) skipItem(info uint8) (uint64, error) {
	if info&0x80 != 0 { // origin (left)
		if err := r.skipID(); err != nil {
			return 0, err
		}
	}
	if info&0x40 != 0 { // rightOrigin (right)
		if err := r.skipID(); err != nil {
			return 0, err
		}
	}
	if info&0xc0 == 0 { // cantCopyParentInfo → parent (+ parentSub)
		parentIsKey, err := r.varUint()
		if err != nil {
			return 0, err
		}
		if parentIsKey == 1 {
			if err := r.varSkip(); err != nil { // parentYKey
				return 0, err
			}
		} else if err := r.skipID(); err != nil { // parent item id
			return 0, err
		}
		if info&0x20 != 0 { // parentSub
			if err := r.varSkip(); err != nil {
				return 0, err
			}
		}
	}
	return r.skipContent(info & 0x1f)
}

// skipContent consumes an Item content by ref id and returns its length.
// 按 ref 消耗 Item 内容并返回其长度。
func (r *updReader) skipContent(ref uint8) (uint64, error) {
	switch ref {
	case 1: // ContentDeleted → len
		return r.varUint()
	case 2: // ContentJSON → n × varString, len = n
		n, err := r.varUint()
		if err != nil {
			return 0, err
		}
		for i := uint64(0); i < n; i++ {
			if err := r.varSkip(); err != nil {
				return 0, err
			}
		}
		return n, nil
	case 3: // ContentBinary → varBytes, len 1
		return 1, r.varSkip()
	case 4: // ContentString → varString, len = UTF-16 units
		n, err := r.varUint()
		if err != nil {
			return 0, err
		}
		if n > uint64(len(r.b)-r.pos) { //nolint:gosec // pos is always within len(b)
			return 0, errBadUpdate
		}
		l, err := utf16Len(r.b[r.pos : r.pos+int(n)]) //nolint:gosec // n is bounded by the check above
		if err != nil {
			return 0, err
		}
		r.pos += int(n) //nolint:gosec // n is bounded by the check above
		return l, nil
	case 5: // ContentEmbed → JSON varString, len 1
		return 1, r.varSkip()
	case 6: // ContentFormat → key varString + JSON varString, len 1
		if err := r.varSkip(); err != nil {
			return 0, err
		}
		return 1, r.varSkip()
	case 7: // ContentType → typeRef; YXmlElement(3)/YXmlHook(5) carry a key
		t, err := r.varUint()
		if err != nil {
			return 0, err
		}
		if t == 3 || t == 5 {
			if err := r.varSkip(); err != nil {
				return 0, err
			}
		}
		if t > 6 {
			return 0, errBadUpdate
		}
		return 1, nil
	case 8: // ContentAny → n × any
		n, err := r.varUint()
		if err != nil {
			return 0, err
		}
		for i := uint64(0); i < n; i++ {
			if err := r.skipAny(); err != nil {
				return 0, err
			}
		}
		return n, nil
	case 9: // ContentDoc → varString + any, len 1
		if err := r.varSkip(); err != nil {
			return 0, err
		}
		return 1, r.skipAny()
	default:
		return 0, errBadUpdate
	}
}

// skipAny consumes one lib0 any-encoded value (type tag 116..127).
// 消耗一个 lib0 any 值（tag 116..127）。
func (r *updReader) skipAny() error {
	t, err := r.byte()
	if err != nil {
		return err
	}
	switch t {
	case 127, 126, 121, 120: // undefined, null, false, true
		return nil
	case 125: // varInt — zigzag varUint on the wire
		_, err := r.varUint()
		return err
	case 124: // float32
		return r.skip(4)
	case 123, 122: // float64 / bigint64
		return r.skip(8)
	case 119: // string
		return r.varSkip()
	case 118: // object: n × (key varString + any)
		n, err := r.varUint()
		if err != nil {
			return err
		}
		for ; n > 0; n-- {
			if err := r.varSkip(); err != nil {
				return err
			}
			if err := r.skipAny(); err != nil {
				return err
			}
		}
		return nil
	case 117: // array: n × any
		n, err := r.varUint()
		if err != nil {
			return err
		}
		for ; n > 0; n-- {
			if err := r.skipAny(); err != nil {
				return err
			}
		}
		return nil
	case 116: // Uint8Array
		return r.varSkip()
	default:
		return errBadUpdate
	}
}
