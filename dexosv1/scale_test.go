package dexosv1

import (
	"math/big"
	"testing"
)

func TestScaleExactBothWays(t *testing.T) {
	s := Scale{PriceDecimals: 1, SizeDecimals: 3}
	for _, c := range []struct {
		price string
		ticks uint32
	}{{"2493.4", 24934}, {"2493", 24930}, {"2493.40", 24934}, {"0", 0}, {"0.1", 1}} {
		got, err := s.Ticks(c.price)
		if err != nil || got != c.ticks {
			t.Fatalf("Ticks(%q) = %d, %v; 要 %d", c.price, got, err, c.ticks)
		}
	}
	for _, c := range []struct {
		size string
		lots uint64
	}{{"0.184", 184}, {"1", 1000}, {"0.0010", 1}, {"12.5", 12500}} {
		got, err := s.Lots(c.size)
		if err != nil || got != c.lots {
			t.Fatalf("Lots(%q) = %d, %v; 要 %d", c.size, got, err, c.lots)
		}
	}
	// 反方向去掉末尾的 0,与服务端同一写法
	if got := s.Price(24930); got != "2493" {
		t.Fatalf("Price(24930) = %q", got)
	}
	if got := s.Size(1); got != "0.001" {
		t.Fatalf("Size(1) = %q", got)
	}
	if got := s.Size(0); got != "0" {
		t.Fatalf("Size(0) = %q", got)
	}
}

// 没对齐、负数、溢出、不是十进制:一律报错,不舍入。
func TestScaleRejectsInsteadOfRounding(t *testing.T) {
	s := Scale{PriceDecimals: 1, SizeDecimals: 3}
	for _, bad := range []string{"2493.45", "-1", "4294967296", "", "1.", ".5", "1e3", " 1", "0x10", "1.2.3"} {
		if v, err := s.Ticks(bad); err == nil {
			t.Fatalf("Ticks(%q) = %d,应当报错", bad, v)
		}
	}
	for _, bad := range []string{"0.0001", "-0.001", "18446744073709551.616"} {
		if v, err := s.Lots(bad); err == nil {
			t.Fatalf("Lots(%q) = %d,应当报错", bad, v)
		}
	}
}

func TestParseFixedSignedAmounts(t *testing.T) {
	for _, c := range []struct {
		in   string
		dec  uint8
		want int64
	}{{"-1282.814453", 6, -1282814453}, {"5000", 6, 5000000000}, {"-0.0249", 6, -24900}, {"0", 6, 0}} {
		v, err := ParseFixed(c.in, c.dec)
		if err != nil || v.Cmp(big.NewInt(c.want)) != 0 {
			t.Fatalf("ParseFixed(%q, %d) = %v, %v; 要 %d", c.in, c.dec, v, err, c.want)
		}
		if back := FormatFixed(v, c.dec); back != c.in {
			t.Fatalf("FormatFixed 回不到原文:%q → %q", c.in, back)
		}
	}
}
