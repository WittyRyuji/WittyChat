package docs

// Parser conformance: fixture updates + expected per-client clock ranges
// were produced by the authoritative JS decoder (Y.parseUpdateMeta) so a
// drift in our hand-rolled reader shows up as a failed test, not a
// desynced doc. / 解析一致性：fixture 的 update 与期望区间由权威 JS 解码器
// （Y.parseUpdateMeta）产出，自研读取器一旦漂移就表现为测试失败而非文档
// 分叉。
import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

type svFixture struct {
	Label  string                               `json:"label"`
	Update string                               `json:"update"`
	Ranges map[string]struct{ From, To uint64 } `json:"ranges"`
}

func TestParseUpdateRanges_fixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/svfixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx []svFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	for i, f := range fx {
		blob, err := hex.DecodeString(f.Update)
		if err != nil {
			t.Fatalf("#%d %s: bad hex: %v", i, f.Label, err)
		}
		got, err := ParseUpdateRanges(blob)
		if err != nil {
			t.Fatalf("#%d %s: parse: %v", i, f.Label, err)
		}
		if len(got) != len(f.Ranges) {
			t.Fatalf("#%d %s: clients=%d want %d (%v)", i, f.Label, len(got), len(f.Ranges), got)
		}
		for cs, want := range f.Ranges {
			var c uint64
			if _, err := fmtSscan(cs, &c); err != nil {
				t.Fatalf("#%d %s: client key %q", i, f.Label, cs)
			}
			g, ok := got[c]
			if !ok || g.Start != want.From || g.End != want.To {
				t.Fatalf("#%d %s: client %s got %+v want [%d,%d)", i, f.Label, cs, g, want.From, want.To)
			}
		}
	}
}

// fmtSscan parses a decimal client id (json keys are strings).
func fmtSscan(s string, out *uint64) (int, error) {
	var n uint64
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, errBadUpdate
		}
		n = n*10 + uint64(ch-'0')
	}
	*out = n
	return 1, nil
}

func TestSVRoundTrip(t *testing.T) {
	in := map[uint64]uint64{42: 5, 7: 100, 1 << 52: 3}
	b := EncodeSV(in)
	out, err := DecodeSV(b)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range in {
		if out[k] != v {
			t.Fatalf("client %d: got %d want %d", k, out[k], v)
		}
	}
	if _, err := DecodeSV([]byte{0x01, 0xff}); err == nil {
		t.Fatal("truncated sv must fail")
	}
}

func TestMergeSVHonorsGaps(t *testing.T) {
	sv := map[uint64]uint64{1: 10}
	// Contiguous + overlapping extends. / 相接或重叠才扩展。
	MergeSV(sv, map[uint64]ClockRange{1: {Start: 8, End: 15}})
	if sv[1] != 15 {
		t.Fatalf("got %d", sv[1])
	}
	// A gap (rejected edit in between) must NOT advance the vector — the
	// client holding [10,20) must be asked again. / 中间有洞不得推进——
	// 否则下次同步会声称存有其实没有的时钟。
	MergeSV(sv, map[uint64]ClockRange{1: {Start: 18, End: 25}})
	if sv[1] != 15 {
		t.Fatalf("gap wrongly advanced sv to %d", sv[1])
	}
	// Backfill closing the gap then extends. / 补洞后再扩展。
	MergeSV(sv, map[uint64]ClockRange{1: {Start: 14, End: 25}})
	if sv[1] != 25 {
		t.Fatalf("backfill: got %d", sv[1])
	}
	// Unknown client starting at 0 merges; mid-clock run does not.
	MergeSV(sv, map[uint64]ClockRange{2: {Start: 4, End: 9}})
	if sv[2] != 0 {
		t.Fatalf("mid-clock run for fresh client must be skipped")
	}
	MergeSV(sv, map[uint64]ClockRange{2: {Start: 0, End: 9}})
	if sv[2] != 9 {
		t.Fatalf("fresh client: got %d", sv[2])
	}
}

func TestParseUpdateRanges_malformed(t *testing.T) {
	for _, b := range [][]byte{
		nil,
		{0x05},                         // claims 5 clients, no body
		{0x01, 0x01, 0x01},             // truncated block
		{0x01, 0x01, 0x01, 0x00, 0xff}, // item info with no body
	} {
		if _, err := ParseUpdateRanges(b); err == nil {
			t.Fatalf("% x: want error", b)
		}
	}
}
