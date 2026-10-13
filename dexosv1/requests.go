package dexosv1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// RequestReceipt 一次请求回执的结论(`GET /v1/requests/{requestId}`)。六种结论都是**答案**,
// 不是错误,含义与 [dexos.ReceiptStatus] 相同:executed / rejected / pending(200),
// not_found(404)/ conflict(409)/ history_unavailable(503)。
//
// v1 把后三种装在错误体里(`error.details` 是回执字段),本包把它们还原成回执。
// Raw 是回执字段的原文,逐项结局等本阶段没有建模的内容从这里取。
type RequestReceipt struct {
	Status             dexos.ReceiptStatus `json:"status"`
	RequestID          string              `json:"requestId"`
	Epoch              string              `json:"epoch"`
	Seq                *uint64             `json:"seq"`
	Sub                *uint16             `json:"sub"`
	AsOfSeq            *uint64             `json:"asOfSeq"`
	ConflictingRequest *string             `json:"conflictingRequest"`
	Gaps               []dexos.Gap         `json:"gaps"`
	Raw                json.RawMessage     `json:"-"`
}

// RequestQuery 可选的 nonce 冲突检查,规则同 [dexos.ReceiptQuery]:Scope 与 Nonce 成对给;
// CommandHash 只配 agent scope,历史有缺口时服务端凭它用代理 nonce 证明请求没执行。
type RequestQuery struct {
	Scope       string
	Nonce       *uint64
	CommandHash string
}

// Request 按请求身份(0x + 64 位十六进制)查回执。存储故障、请求错误、结论与状态码对不上都是 error。
func (c *Client) Request(ctx context.Context, requestID string, q RequestQuery) (*RequestReceipt, error) {
	if !isHash32(requestID) {
		return nil, fmt.Errorf("dexosv1: 请求身份应为 0x + 64 位十六进制,得到 %q", requestID)
	}
	if (q.Scope == "") != (q.Nonce == nil) {
		return nil, errors.New("dexosv1: 回执查询的 scope 与 nonce 必须成对")
	}
	if q.CommandHash != "" && (!strings.HasPrefix(q.Scope, "agent:") || !isHash32(q.CommandHash)) {
		return nil, errors.New("dexosv1: commandHash 只能配 agent scope 与 nonce,且为 0x + 64 位十六进制")
	}
	v := url.Values{}
	if q.Scope != "" {
		v.Set("scope", q.Scope)
		v.Set("nonce", strconv.FormatUint(*q.Nonce, 10))
		if q.CommandHash != "" {
			v.Set("commandHash", q.CommandHash)
		}
	}
	status, header, raw, err := c.roundTrip(ctx, "/v1/requests/"+requestID, v, accountRead)
	if err != nil {
		return nil, err
	}
	fields := json.RawMessage(raw)
	if status < 200 || status >= 300 {
		perr := parseError(status, header, raw)
		var e *Error
		if !errors.As(perr, &e) || e.Details == nil {
			return nil, perr
		}
		if fields, err = json.Marshal(e.Details); err != nil {
			return nil, perr
		}
	}
	var r RequestReceipt
	if err := json.Unmarshal(fields, &r); err != nil {
		return nil, fmt.Errorf("dexosv1: 回执解析失败: %w", err)
	}
	r.Raw = fields
	allowed := map[int]dexos.ReceiptStatus{
		http.StatusNotFound:           dexos.ReceiptNotFound,
		http.StatusConflict:           dexos.ReceiptConflict,
		http.StatusServiceUnavailable: dexos.ReceiptHistoryUnavailable,
	}
	switch want, isErr := allowed[status]; {
	case status == http.StatusOK:
		if r.Status != dexos.ReceiptExecuted && r.Status != dexos.ReceiptRejected && r.Status != dexos.ReceiptPending {
			return nil, fmt.Errorf("dexosv1: 回执结论 %q 不在契约内", r.Status)
		}
		if r.Seq == nil || r.Sub == nil {
			return nil, fmt.Errorf("dexosv1: %s 回执缺 seq/sub", r.Status)
		}
	case isErr && r.Status == want:
		if r.Status == dexos.ReceiptNotFound && r.AsOfSeq == nil {
			return nil, errors.New("dexosv1: not_found 回执缺 asOfSeq")
		}
	default:
		return nil, parseError(status, header, raw)
	}
	if r.Epoch == "" || !strings.EqualFold(r.RequestID, requestID) {
		return nil, fmt.Errorf("dexosv1: 回执缺纪元或请求身份不符(问 %s,答 %q)", requestID, r.RequestID)
	}
	return &r, nil
}

func isHash32(s string) bool {
	h, ok := strings.CutPrefix(s, "0x")
	if !ok || len(h) != 64 {
		return false
	}
	for _, c := range strings.ToLower(h) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
