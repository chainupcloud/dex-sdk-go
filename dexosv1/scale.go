package dexosv1

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

// Scale 一个市场的十进制 ↔ 整数换算:ticks = price × 10^PriceDecimals,lots = size × 10^SizeDecimals。
//
// 两个方向都是精确的:十进制字符串的小数位多于市场精度且多出来的不全是 0,就是没对齐
// (服务端同样拒,`400 OrderNotAligned`),这里直接报错,不舍入。
type Scale struct {
	PriceDecimals uint8
	SizeDecimals  uint8
}

// Ticks 十进制价格 → tick。负数、没对齐、超出 uint32 都是错误。
func (s Scale) Ticks(price string) (uint32, error) {
	v, err := ParseFixed(price, s.PriceDecimals)
	if err != nil {
		return 0, fmt.Errorf("价格 %q: %w", price, err)
	}
	if v.Sign() < 0 || !v.IsUint64() || v.Uint64() > math.MaxUint32 {
		return 0, fmt.Errorf("价格 %q 换成 tick 后超出 uint32 或为负", price)
	}
	return uint32(v.Uint64()), nil
}

// Lots 十进制数量 → lot。负数、没对齐、超出 uint64 都是错误。
func (s Scale) Lots(size string) (uint64, error) {
	v, err := ParseFixed(size, s.SizeDecimals)
	if err != nil {
		return 0, fmt.Errorf("数量 %q: %w", size, err)
	}
	if v.Sign() < 0 || !v.IsUint64() {
		return 0, fmt.Errorf("数量 %q 换成 lot 后超出 uint64 或为负", size)
	}
	return v.Uint64(), nil
}

// Price tick → 十进制价格(去掉末尾多余的 0,与服务端同一写法)。
func (s Scale) Price(ticks uint32) string {
	return FormatFixed(new(big.Int).SetUint64(uint64(ticks)), s.PriceDecimals)
}

// Size lot → 十进制数量。
func (s Scale) Size(lots uint64) string {
	return FormatFixed(new(big.Int).SetUint64(lots), s.SizeDecimals)
}

var errNotDecimal = errors.New("不是十进制字符串(^-?[0-9]+(\\.[0-9]+)?$)")

// ParseFixed 把十进制字符串精确换成以 10^-decimals 为单位的整数。
//
// 小数位多于 decimals 时,多出来的位必须全是 0(`"0.0010"` 按 3 位是 1),否则报「没对齐」——
// 不四舍五入。用于价量之外的金额:余额、费用、盈亏按 token 小数位换成原子单位。
func ParseFixed(s string, decimals uint8) (*big.Int, error) {
	body, neg := strings.CutPrefix(s, "-")
	intPart, frac, hasDot := strings.Cut(body, ".")
	if intPart == "" || (hasDot && frac == "") || !allDigits(intPart) || !allDigits(frac) {
		return nil, errNotDecimal
	}
	d := int(decimals)
	if len(frac) > d {
		if strings.Trim(frac[d:], "0") != "" {
			return nil, fmt.Errorf("小数位多于 %d 位且没对齐(不整除)", decimals)
		}
		frac = frac[:d]
	}
	frac += strings.Repeat("0", d-len(frac))
	v, ok := new(big.Int).SetString(intPart+frac, 10)
	if !ok {
		return nil, errNotDecimal
	}
	if neg {
		v.Neg(v)
	}
	return v, nil
}

// FormatFixed 以 10^-decimals 为单位的整数 → 十进制字符串,去掉小数末尾的 0。
func FormatFixed(v *big.Int, decimals uint8) string {
	digits := new(big.Int).Abs(v).String()
	sign := ""
	if v.Sign() < 0 {
		sign = "-"
	}
	d := int(decimals)
	if d == 0 {
		return sign + digits
	}
	if len(digits) <= d {
		digits = strings.Repeat("0", d-len(digits)+1) + digits
	}
	intPart, frac := digits[:len(digits)-d], strings.TrimRight(digits[len(digits)-d:], "0")
	if frac == "" {
		return sign + intPart
	}
	return sign + intPart + "." + frac
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
