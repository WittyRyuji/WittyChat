package objects

// Image metadata helpers: header capture, dimension sniffing with EXIF
// orientation correction, and pixel-level orientation application.
// Pure byte/image logic — no DB, no store.
//
// 图片元数据助手：头部捕获、带 EXIF 方向修正的尺寸嗅探、像素级方向
// 应用。纯字节/图像逻辑——不依赖 DB 与 store。

import (
	"encoding/binary"
	"image"
	_ "image/gif"  // DecodeConfig registration / 注册解码器
	_ "image/jpeg" // DecodeConfig registration
	_ "image/png"  // DecodeConfig registration
	"io"

	_ "golang.org/x/image/bmp"  // DecodeConfig registration
	_ "golang.org/x/image/webp" // DecodeConfig registration
)

// sniffHead is how many leading bytes HeadCapture keeps for EXIF parsing —
// covers APP1 segments on any sane JPEG. / 为 EXIF 解析保留的头部字节数，
// 足以覆盖正常 JPEG 的 APP1 段。
const sniffHead = 1 << 20

// HeadCapture is an io.Writer that retains only the first `max` bytes
// and silently drops the rest. It ALWAYS reports len(p) written so an
// io.TeeReader feeding it never aborts the primary read — the capture is
// a side channel, never a gate. / 只保留前 max 字节、其余静默丢弃的
// io.Writer。永远返回 len(p)——TeeReader 的主读取不会因它中止，捕获
// 只是旁路而非关卡。
type HeadCapture struct {
	buf []byte
	max int
}

// NewHeadCapture keeps at most `max` leading bytes.
func NewHeadCapture(max int) *HeadCapture {
	return &HeadCapture{max: max}
}

// Write appends p up to the cap, then discards. Never errors.
func (c *HeadCapture) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.max - len(c.buf); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		c.buf = append(c.buf, p...)
	}
	return n, nil
}

// Bytes returns the captured prefix (possibly shorter than the stream).
func (c *HeadCapture) Bytes() []byte { return c.buf }

// SniffDims decodes image dimensions from r, applying EXIF orientation
// for JPEGs (a stored-landscape phone photo is reported portrait, as the
// renderer will show it). Returns (nil,nil) for non-images and anything
// the decoders can't parse. / 从 r 解码图片尺寸，JPEG 按 EXIF 方向修正
// （竖拍但横存的手机照片按渲染效果报告竖幅）。非图片或解码失败返回
// (nil,nil)。
func SniffDims(r io.Reader) (w, h *int) {
	head := NewHeadCapture(sniffHead)
	cfg, format, err := image.DecodeConfig(io.TeeReader(r, head))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, nil
	}
	w, h = &cfg.Width, &cfg.Height
	if format == "jpeg" {
		// Orientations 5-8 transpose the image for display — the stored
		// W×H reads swapped. / 方向 5-8 显示时转置——存储宽高互换。
		if o := JPEGOrientation(head.Bytes()); o >= 5 && o <= 8 {
			w, h = h, w
		}
	}
	return w, h
}

// JPEGOrientation extracts EXIF orientation (1-8) from JPEG bytes.
// Returns 1 (normal) when absent or malformed — every read is bounds-
// checked, so arbitrary/truncated input can't panic. / 提取 JPEG 的
// EXIF 方向（1-8）。缺失或格式错误返回 1（正常）——所有读取都有边界
// 检查，任意/截断输入不会 panic。
func JPEGOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1 // not even a SOI / 连 SOI 都不是
	}
	pos := 2
	for pos < len(b) {
		// 0xFF fill bytes may precede the real marker. / 真标记前可能有
		// 0xFF 填充字节。
		for pos < len(b) && b[pos] == 0xFF {
			pos++
		}
		if pos >= len(b) {
			return 1
		}
		marker := b[pos]
		pos++
		if marker == 0xDA || marker == 0xD9 {
			return 1 // SOS/EOI — metadata ends / 扫描数据开始，元数据结束
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			continue // standalone markers carry no length / 独立标记无长度域
		}
		if pos+2 > len(b) {
			return 1
		}
		segLen := int(b[pos])<<8 | int(b[pos+1])
		// The u16 length counts itself; <2 or a truncated segment means
		// garbage — bail rather than guess. / 长度域含自身；<2 或段被
		// 截断即视为坏数据，直接放弃而非猜测。
		if segLen < 2 || pos+segLen > len(b) {
			return 1
		}
		data := b[pos+2 : pos+segLen]
		if marker == 0xE1 && len(data) >= 6 && string(data[:6]) == "Exif\x00\x00" {
			if o := tiffOrientation(data[6:]); o != 0 {
				return o
			}
		}
		pos += segLen
	}
	return 1
}

// tiffOrientation parses the TIFF inside an Exif APP1 payload and reads
// tag 0x0112 (Orientation, SHORT) from IFD0. 0 = not found/bad (caller
// keeps looking), 1-8 = the stored orientation. / 解析 Exif APP1 内的
// TIFF，读 IFD0 的 0x0112（Orientation，SHORT）。0=未找到/异常（调用方
// 继续找），1-8=存储方向。
func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch {
	case t[0] == 'I' && t[1] == 'I':
		bo = binary.LittleEndian
	case t[0] == 'M' && t[1] == 'M':
		bo = binary.BigEndian
	default:
		return 0
	}
	if bo.Uint16(t[2:4]) != 42 {
		return 0
	}
	ifd := int(bo.Uint32(t[4:8])) // offset from TIFF start / 相对 TIFF 起点
	if ifd < 0 || ifd+2 > len(t) {
		return 0
	}
	count := int(bo.Uint16(t[ifd : ifd+2]))
	for i := 0; i < count; i++ {
		e := ifd + 2 + i*12
		if e+12 > len(t) {
			return 0
		}
		if bo.Uint16(t[e:e+2]) == 0x0112 && bo.Uint16(t[e+2:e+4]) == 3 {
			o := int(bo.Uint16(t[e+8 : e+10])) // SHORT value inline / 短值内联
			if o >= 1 && o <= 8 {
				return o
			}
			return 0
		}
	}
	return 0
}

// ApplyOrientation returns the image as an EXIF orientation 2-8 viewer
// would render it. o outside 2-8 (or an in-place no-op) returns src
// unchanged; otherwise the result is a fresh *image.RGBA. / 按 EXIF
// 方向 2-8 的显示效果返回图像。o 不在 2-8 时原样返回 src；否则返回
// 新的 *image.RGBA。
func ApplyOrientation(src image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w // transposing orientations / 转置方向交换宽高
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	// src coordinates below are relative to the origin-normalized image.
	// 下列 src 坐标基于归一到原点的图像。
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch o {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			dst.Set(x, y, src.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return dst
}
