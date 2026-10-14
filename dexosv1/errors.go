package dexosv1

import (
	"encoding/json"
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

// RateLimitedError 429 `GatewayRateLimited`。
//
// 本包不重试也不等待:同一个进程里往往有多个币对共用额度,退让多久、暂停哪个币对只有调用方知道。
type RateLimitedError struct {
	Err *Error
	// Bucket 扣光的是哪个桶(v1.1 §4.1 的 `details.bucket`):"ip" | "account";"" = 服务端没给。
	// 账户桶满说明这个账户的写太密,IP 桶满说明同一台机器上的总量太大 —— 两者的退让对象不同。
	Bucket string
	// RetryAfter 服务端 `retry-after` 给的等待时长;0 = 响应没带这个头。
	RetryAfter time.Duration
	// Limit / Remaining / Reset 来自 `ratelimit: limit=…, remaining=…, reset=…`(IETF 限流头);
	// HasRateLimit=false 表示响应没带这个头,三个值无意义。
	HasRateLimit bool
	Limit        int
	Remaining    int
	Reset        time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("dexosv1: 被限流(retry-after %s, limit=%d remaining=%d): %v",
		e.RetryAfter, e.Limit, e.Remaining, e.Err)
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
	case status == http.StatusBadRequest && strings.Contains(e.Message, "not yet complete"):
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
	return e
}

func rateLimited(e *Error, h http.Header) *RateLimitedError {
	r := &RateLimitedError{Err: e, Bucket: detailString(e.Details, "bucket")}
	if s, err := strconv.Atoi(strings.TrimSpace(h.Get("retry-after"))); err == nil && s >= 0 {
		r.RetryAfter = time.Duration(s) * time.Second
	}
	if v := h.Get("ratelimit"); v != "" {
		kv := map[string]int{}
		for _, part := range strings.Split(v, ",") {
			k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
			if n, err := strconv.Atoi(val); ok && err == nil {
				kv[k] = n
			}
		}
		limit, okL := kv["limit"]
		remaining, okR := kv["remaining"]
		reset, okS := kv["reset"]
		if okL && okR && okS {
			r.HasRateLimit, r.Limit, r.Remaining, r.Reset = true, limit, remaining, time.Duration(reset)*time.Second
		}
	}
	return r
}

func detailString(d map[string]json.RawMessage, key string) string {
	var s string
	if raw, ok := d[key]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}
