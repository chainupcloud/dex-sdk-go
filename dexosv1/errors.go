package dexosv1

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// Error v1 的非 2xx 响应。v1 只有一种错误体:`{error:{code, num, message, details?}}`。
//
// Code 是稳定字符串(网关错误 `Gateway*`,执行层拒因是 CoreError 变体名),Num 是稳定数字码
// (1xxxx 网关 / 2xxxx 执行层)。判定用它们,不要匹配 Message。错误体不是这个形状时
// (反代直接回的 502 之类)Code 为空,只有 Status 与 Body。
type Error struct {
	Status  int
	Code    string
	Num     int
	Message string
	Details map[string]json.RawMessage
	Body    string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("dexosv1: HTTP %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("dexosv1: HTTP %d %s(%d): %s", e.Status, e.Code, e.Num, e.Message)
}

// RateLimitedError 429 `GatewayRateLimited`(v1.1 §4.1)。网关的 429 一定带 `details.bucket`、`retry-after`
// 和扣光那个桶的读数头;缺任何一项就不是网关限流的形状,不解析成本类型,而是包着 [*Error] 的普通错误。
//
// 本包不重试也不等待:同一个进程里往往有多个币对共用额度,退让多久、暂停哪个币对只有调用方知道。
type RateLimitedError struct {
	Err *Error
	// Bucket 扣光的是哪个桶:"ip" | "account"。
	// 账户桶满说明这个账户的写太密,IP 桶满说明同一台机器上的总量太大 —— 两者的退让对象不同。
	Bucket string
	// RetryAfter 服务端 `retry-after` 给的等待时长。
	RetryAfter time.Duration
	// Limit / Remaining / Reset 是扣光那个桶的读数:IP 桶取 `ratelimit`(IETF 限流头),
	// 账户桶取 `x-ratelimit-account`(账户桶 429 上的 `ratelimit` 是 IP 桶的读数,不是被扣光的那个)。
	Limit     int
	Remaining int
	Reset     time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("dexosv1: %s 桶被限流(retry-after %s, limit=%d remaining=%d): %v",
		e.Bucket, e.RetryAfter, e.Limit, e.Remaining, e.Err)
}

func (e *RateLimitedError) Unwrap() error { return e.Err }

// HistoryUnavailableError 查询区间与执行历史的缺口相交(503,`details.status = history_unavailable`)。
//
// 这不是「没有数据」:缺口里有没有本账户的记录,本副本证明不了。语义与 [dexos.HistoryUnavailableError] 相同。
type HistoryUnavailableError struct {
	Gaps []dexos.Gap
	Err  *Error
}

func (e *HistoryUnavailableError) Error() string {
	return fmt.Sprintf("dexosv1: 执行历史有缺口,查询区间证明不了完整 %v: %v", e.Gaps, e.Err)
}

func (e *HistoryUnavailableError) Unwrap() error { return e.Err }

// 按名称定位市场的三种拒绝(dex-os public-api-v1 §5「符号解析」,dex-os#318):一个名称至多对应一个合约市场和一个
// 现货市场,按名称定位时另带 kind(perp | spot)。用 errors.Is 判;名称、可选的类型用 errors.As 取 [*SymbolError]。
// 服务端回的与本包按目录判出的([Catalog.Lookup])是同一套。
var (
	// ErrAmbiguousSymbol 名称同时对应合约与现货市场,又没给 kind(400 `ambiguous_symbol`)。服务端不替你挑,本包也不挑:
	// 从 [SymbolError.Kinds] 里选一个,用 [Catalog.Lookup] 或市场号([Catalog.MarketByID])重新定位。
	ErrAmbiguousSymbol = errors.New("dexosv1: 名称同时对应合约与现货市场,须指明 kind")
	// ErrBadKind kind 不是 perp / spot(400 `bad_kind`)。
	ErrBadKind = errors.New("dexosv1: kind 只能是 perp 或 spot")
	// ErrDuplicateSymbol 同一类型里有两个在用市场同名(409 `duplicate_symbol`):部署方的配置错误,按名称定位不了它们,
	// 按市场号照常可用。
	ErrDuplicateSymbol = errors.New("dexosv1: 同一类型里有两个在用市场同名")
)

// 服务端 `details.rule` 的取值。
const (
	ruleAmbiguousSymbol = "ambiguous_symbol"
	ruleBadKind         = "bad_kind"
	ruleDuplicateSymbol = "duplicate_symbol"
)

var symbolRules = map[string]error{
	ruleAmbiguousSymbol: ErrAmbiguousSymbol,
	ruleBadKind:         ErrBadKind,
	ruleDuplicateSymbol: ErrDuplicateSymbol,
}

// SymbolError 按名称定位市场被拒。errors.Is 对得上 [ErrAmbiguousSymbol] / [ErrBadKind] / [ErrDuplicateSymbol] 之一;
// 服务端回的(Err 非 nil)经 errors.As 仍能取到 [*Error](状态码 400 / 409:定序之前就拒了,会话不锁、nonce 不动)。
type SymbolError struct {
	// Rule "ambiguous_symbol" | "bad_kind" | "duplicate_symbol"(同服务端的 details.rule)。
	Rule   string
	Symbol string
	// Kind 请求里给的 kind("" = 没给;服务端回的错误里没有这一项,也为 "")。
	Kind string
	// Kinds ambiguous_symbol:这个名称对应的类型,从中选一个作 kind;bad_kind:合法的取值;
	// duplicate_symbol:重名的那个类型。
	Kinds []string
	// Err 服务端的错误体;本包按目录判出时为 nil。
	Err *Error
}

func (e *SymbolError) Error() string {
	var msg string
	switch e.Rule {
	case ruleAmbiguousSymbol:
		msg = fmt.Sprintf("dexosv1: 名称 %q 同时对应 %s 市场,须指明 kind", e.Symbol, strings.Join(e.Kinds, " / "))
	case ruleBadKind:
		msg = fmt.Sprintf("dexosv1: kind %q 不是 %s", e.Kind, strings.Join(e.Kinds, " / "))
	case ruleDuplicateSymbol:
		msg = fmt.Sprintf("dexosv1: 名称 %q 有不止一个在用的 %s 市场(部署方配置错误),请按市场号定位", e.Symbol, strings.Join(e.Kinds, " / "))
	default:
		msg = fmt.Sprintf("dexosv1: 按名称 %q 定位市场被拒(%s)", e.Symbol, e.Rule)
	}
	if e.Err != nil {
		return msg + ": " + e.Err.Error()
	}
	return msg
}

func (e *SymbolError) Is(target error) bool { return target != nil && symbolRules[e.Rule] == target }

func (e *SymbolError) Unwrap() error {
	if e.Err == nil {
		return nil
	}
	return e.Err
}

// symbolError 服务端按名称定位市场的拒绝(400 GatewayBadRequest / 409 GatewayConflict 带 details.rule);不是就返回 nil。
func symbolError(e *Error) *SymbolError {
	rule := detailString(e.Details, "rule")
	if _, ok := symbolRules[rule]; !ok || (e.Status != http.StatusBadRequest && e.Status != http.StatusConflict) {
		return nil
	}
	se := &SymbolError{Rule: rule, Symbol: detailString(e.Details, "symbol"), Err: e}
	if raw, ok := e.Details["kinds"]; ok {
		_ = json.Unmarshal(raw, &se.Kinds)
	}
	return se
}

// parseError 把非 2xx 响应解析成类型化错误。执行历史的两类契约错误复用 dexos 包的哨兵值,
// 调用方用 errors.Is(err, dexos.ErrHistoryEpochMismatch) 判,两套接口一个写法。
func parseError(status int, header http.Header, raw []byte) error {
	e := &Error{Status: status, Body: string(raw)}
	var body struct {
		Error *struct {
			Code    string                     `json:"code"`
			Num     int                        `json:"num"`
			Message string                     `json:"message"`
			Details map[string]json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error != nil {
		e.Code, e.Num, e.Message, e.Details = body.Error.Code, body.Error.Num, body.Error.Message, body.Error.Details
	}
	switch {
	case status == http.StatusTooManyRequests:
		return rateLimited(e, header)
	case status == http.StatusConflict && e.Message == "history_epoch_mismatch":
		return fmt.Errorf("%w: %w", dexos.ErrHistoryEpochMismatch, e)
	case status == http.StatusBadRequest && e.Code == "GatewayHistoryIncomplete":
		// 补拉上界越过了本副本执行历史的完整前缀(v1.1 §4.9-5):稍后重试
		return fmt.Errorf("%w: %w", dexos.ErrHistoryUpperIncomplete, e)
	case status == http.StatusServiceUnavailable && detailString(e.Details, "status") == "history_unavailable":
		var gaps []dexos.Gap
		if raw, ok := e.Details["gaps"]; ok {
			if err := json.Unmarshal(raw, &gaps); err != nil {
				return fmt.Errorf("dexosv1: history_unavailable 的 gaps 解析失败: %w: %w", err, e)
			}
		}
		return &HistoryUnavailableError{Gaps: gaps, Err: e}
	}
	if se := symbolError(e); se != nil {
		return se
	}
	return e
}

// rateLimited 按 v1.1 §4.1 解析 429;形状不全就返回包着 e 的错误(状态码仍可经 errors.As 取到)。
func rateLimited(e *Error, h http.Header) error {
	r := &RateLimitedError{Err: e, Bucket: detailString(e.Details, "bucket")}
	header := map[string]string{"ip": "ratelimit", "account": "x-ratelimit-account"}[r.Bucket]
	if e.Code != "GatewayRateLimited" || header == "" {
		return fmt.Errorf("dexosv1: 429 不是网关限流的形状(code %q,details.bucket %q): %w", e.Code, r.Bucket, e)
	}
	s, err := strconv.Atoi(strings.TrimSpace(h.Get("retry-after")))
	if err != nil || s < 0 {
		return fmt.Errorf("dexosv1: 429 的 retry-after %q 不对: %w", h.Get("retry-after"), e)
	}
	r.RetryAfter = time.Duration(s) * time.Second
	kv := map[string]int{}
	for _, part := range strings.Split(h.Get(header), ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if n, err := strconv.Atoi(val); ok && err == nil {
			kv[k] = n
		}
	}
	limit, okL := kv["limit"]
	remaining, okR := kv["remaining"]
	reset, okS := kv["reset"]
	if !okL || !okR || !okS {
		return fmt.Errorf("dexosv1: %s 桶的 429 缺 %s 头(得到 %q): %w", r.Bucket, header, h.Get(header), e)
	}
	r.Limit, r.Remaining, r.Reset = limit, remaining, time.Duration(reset)*time.Second
	return r
}

func detailString(d map[string]json.RawMessage, key string) string {
	var s string
	if raw, ok := d[key]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}
