package httpapi

import (
	"net"
	"testing"
)

// IPv6 rotation inside one /64 must land on the same throttle key — that is
// the whole point of bucketing (providers hand out prefixes, not addresses).
// 同一 /64 内换地址必须落到同一个限速键——运营商按前缀分配，不归并就能
// 零成本轮换。
func TestLimitIPKeyIPv6Bucket(t *testing.T) {
	a := limitIPKey(net.ParseIP("2001:db8:abcd:1234::1"))
	b := limitIPKey(net.ParseIP("2001:db8:abcd:1234:ffff:ee00:11:99"))
	if a != b {
		t.Fatalf("same /64 produced different keys: %q vs %q", a, b)
	}
	c := limitIPKey(net.ParseIP("2001:db8:abcd:1235::1"))
	if c == a {
		t.Fatalf("different /64 folded into one key: %q", c)
	}
}

func TestLimitIPKeyV4Unchanged(t *testing.T) {
	if k := limitIPKey(net.ParseIP("192.0.2.7")); k != "192.0.2.7" {
		t.Fatalf("v4 key mangled: %q", k)
	}
	if limitIPKey(net.ParseIP("192.0.2.7")) == limitIPKey(net.ParseIP("192.0.2.8")) {
		t.Fatal("distinct v4 addresses folded")
	}
	if k := limitIPKey(nil); k == "" {
		t.Fatal("nil IP must still produce a key")
	}
}
