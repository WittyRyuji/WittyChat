package objects

// Pure byte-level tests for the image metadata helpers — no DB.
// 图片元数据助手的纯字节级测试——不依赖数据库。

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"golang.org/x/image/bmp"
)

// pngBytes encodes a W×H blank RGBA as PNG.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// jpegWithEXIF encodes a W×H blank RGBA as JPEG and splices an EXIF APP1
// segment carrying `orient` right after the SOI. mm=false writes an
// Intel (II) TIFF, mm=true a Motorola (MM) one.
// jpegWithEXIF 生成 W×H JPEG 并在 SOI 后插入携带方向值的 EXIF APP1；
// mm=false 写 II 小端 TIFF，mm=true 写 MM 大端。
func jpegWithEXIF(t *testing.T, w, h int, orient uint16, mm bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()

	var tiff bytes.Buffer
	var bo binary.ByteOrder = binary.LittleEndian
	if mm {
		tiff.WriteString("MM")
		bo = binary.BigEndian
	} else {
		tiff.WriteString("II")
	}
	for _, v := range []any{
		uint16(42),     // TIFF magic
		uint32(8),      // IFD0 offset from TIFF start
		uint16(1),      // IFD0 entry count
		uint16(0x0112), // tag: Orientation
		uint16(3),      // type: SHORT
		uint32(1),      // count
		orient,
		uint16(0), // pad value[] to 4 bytes
		uint32(0), // next IFD = none
	} {
		if err := binary.Write(&tiff, bo, v); err != nil {
			t.Fatal(err)
		}
	}
	app1 := []byte{0xFF, 0xE1, 0, 0}
	//nolint:gosec // segment length is ~30 bytes here by construction / 测试构造的段长仅数十字节
	binary.BigEndian.PutUint16(app1[2:4], uint16(2+6+tiff.Len()))
	app1 = append(app1, []byte("Exif\x00\x00")...)
	app1 = append(app1, tiff.Bytes()...)

	out := []byte{0xFF, 0xD8}
	out = append(out, app1...)
	out = append(out, raw[2:]...)
	return out
}

func TestJPEGOrientation(t *testing.T) {
	cases := []struct {
		name string
		jpeg []byte
		want int
	}{
		{"II o6", jpegWithEXIF(t, 40, 30, 6, false), 6},
		{"II o8", jpegWithEXIF(t, 40, 30, 8, false), 8},
		{"MM o6", jpegWithEXIF(t, 40, 30, 6, true), 6},
	}
	for _, c := range cases {
		if got := JPEGOrientation(c.jpeg); got != c.want {
			t.Errorf("%s: orientation %d want %d", c.name, got, c.want)
		}
	}

	// No APP1 at all → normal orientation.
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	if got := JPEGOrientation(buf.Bytes()); got != 1 {
		t.Fatalf("plain jpeg: orientation %d want 1", got)
	}
	// Garbage that isn't a JPEG → 1, not a panic.
	if got := JPEGOrientation([]byte("definitely not a jpeg")); got != 1 {
		t.Fatalf("garbage: orientation %d want 1", got)
	}
}

// Every truncation prefix of a valid EXIF JPEG must be safe to parse and
// yield either the true orientation or the 1 fallback — never a panic,
// never a wrong value. / 有效 EXIF JPEG 的每个截断前缀都必须可安全
// 解析，结果要么是真实方向要么回退 1——不 panic、不误报。
func TestJPEGOrientationTruncated(t *testing.T) {
	b := jpegWithEXIF(t, 40, 30, 6, false)
	for n := 0; n <= len(b); n++ {
		got := JPEGOrientation(b[:n])
		if got != 1 && got != 6 {
			t.Fatalf("prefix %d: orientation %d (want 1 or 6)", n, got)
		}
	}
}

func TestSniffDims(t *testing.T) {
	w, h := SniffDims(bytes.NewReader(pngBytes(t, 517, 389)))
	if w == nil || h == nil || *w != 517 || *h != 389 {
		t.Fatalf("png 517x389: got %v,%v", w, h)
	}

	// Stored 40×30 with orientation 6 → displayed swapped.
	w, h = SniffDims(bytes.NewReader(jpegWithEXIF(t, 40, 30, 6, false)))
	if w == nil || h == nil || *w != 30 || *h != 40 {
		t.Fatalf("jpeg 40x30 o6: got %v,%v want 30,40", w, h)
	}
	w, h = SniffDims(bytes.NewReader(jpegWithEXIF(t, 40, 30, 1, false)))
	if w == nil || h == nil || *w != 40 || *h != 30 {
		t.Fatalf("jpeg 40x30 o1: got %v,%v want 40,30", w, h)
	}

	// BMP is decodable via x/image/bmp.
	var bb bytes.Buffer
	if err := bmp.Encode(&bb, image.NewRGBA(image.Rect(0, 0, 20, 10))); err != nil {
		t.Fatal(err)
	}
	w, h = SniffDims(bytes.NewReader(bb.Bytes()))
	if w == nil || h == nil || *w != 20 || *h != 10 {
		t.Fatalf("bmp 20x10: got %v,%v want 20,10", w, h)
	}

	// PNG signature followed by garbage → undecodable → nil.
	bad := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	bad = append(bad, bytes.Repeat([]byte{0x99}, 64)...)
	if w, h := SniffDims(bytes.NewReader(bad)); w != nil || h != nil {
		t.Fatalf("corrupt png: got %v,%v want nil,nil", w, h)
	}
	if w, h := SniffDims(bytes.NewReader([]byte("plain text body"))); w != nil || h != nil {
		t.Fatalf("text: got %v,%v want nil,nil", w, h)
	}
}

// ApplyOrientation on a 3×2 source with a unique color per pixel —
// every dst pixel must read from the spec'd src coordinate.
// ApplyOrientation：3×2 源图每像素颜色唯一，逐一核对 dst 坐标映射。
func TestApplyOrientation(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	cols := [2][3]color.RGBA{
		{{R: 0x11, A: 0xFF}, {R: 0x22, A: 0xFF}, {R: 0x33, A: 0xFF}},
		{{R: 0x44, A: 0xFF}, {R: 0x55, A: 0xFF}, {R: 0x66, A: 0xFF}},
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			src.SetRGBA(x, y, cols[y][x])
		}
	}
	px := func(im image.Image, x, y int) color.RGBA {
		return color.RGBAModel.Convert(im.At(x, y)).(color.RGBA)
	}

	for o := 1; o <= 8; o++ {
		dst := ApplyOrientation(src, o)
		dw, dh := 3, 2
		if o >= 5 {
			dw, dh = 2, 3
		}
		if b := dst.Bounds(); b.Dx() != dw || b.Dy() != dh {
			t.Fatalf("o%d bounds %dx%d want %dx%d", o, b.Dx(), b.Dy(), dw, dh)
		}
	}

	corners := []struct {
		o      int
		dx, dy int
		sx, sy int
	}{
		{2, 0, 0, 2, 0},
		{3, 0, 0, 2, 1},
		{4, 0, 0, 0, 1},
		{5, 1, 0, 0, 1},
		{6, 0, 0, 0, 1},
		{6, 1, 0, 0, 0},
		{7, 0, 0, 2, 1},
		{8, 0, 0, 2, 0},
	}
	for _, c := range corners {
		dst := ApplyOrientation(src, c.o)
		if got, want := px(dst, c.dx, c.dy), cols[c.sy][c.sx]; got != want {
			t.Errorf("o%d dst(%d,%d)=%v want src(%d,%d)=%v",
				c.o, c.dx, c.dy, got, c.sx, c.sy, want)
		}
	}
}
