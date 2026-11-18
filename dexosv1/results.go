package dexosv1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// LogPosition 一笔写在日志里的位置,即写回执的 Seq / Sub([Receipt],`ack=sequenced` 也有)。
type LogPosition struct {
	Seq uint64
	Sub uint16
}

// ResultStatus 按日志位置取结果时一项的结论。
type ResultStatus string

const (
	// ResultExecuted 执行了,逐项结果用 [Result.Batch] 解析。
	ResultExecuted ResultStatus = "executed"
	// ResultRejected 整笔被拒,[Result.Batch] 返回与同步写 422 相同的 *Error。
	ResultRejected ResultStatus = "rejected"
	// ResultPending 答话的副本还没应用到这个位置,再取。
	ResultPending ResultStatus = "pending"
	// ResultNotFound 这个位置上没有命令。
	ResultNotFound ResultStatus = "not_found"
	// ResultEvicted 已出 edge 的内存结果窗口,改按请求身份查([Client.Request])。
	ResultEvicted ResultStatus = "evicted"
)

// MaxResultPositions / MaxResultWait 一次 [Client.Results] 最多问的位置数与最长等待。
const (
	MaxResultPositions = 64
	MaxResultWait      = 5 * time.Second
)

// Result `GET /v1/results` 的一项。
type Result struct {
	LogPosition
	Status    ResultStatus
	RequestID string
	// Duplicate 这个位置是一次已经执行过的请求的重发,结果是原请求的,原请求的位置在 Original。
	Duplicate bool
	Original  *LogPosition
	// Error 整笔被拒时的拒因(Status 422,同同步写)。
	Error *Error
	// Message evicted 时网关给的去处。
	Message string
	raw     json.RawMessage
}

type wireResult struct {
	Seq       *uint64      `json:"seq"`
	Sub       *uint16      `json:"sub"`
	Status    ResultStatus `json:"status"`
	RequestID string       `json:"requestId"`
	Duplicate bool         `json:"duplicate"`
	Original  *struct {
		Seq uint64 `json:"seq"`
		Sub uint16 `json:"sub"`
	} `json:"original"`
	Error *struct {
		Code    string `json:"code"`
		Num     int    `json:"num"`
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

// Results 按日志位置等执行结果(`GET /v1/results`):一次 1~64 个位置,最长等 5 秒;等到最早的那个位置在答话的
// 副本上应用了,就把所有位置按问的顺序一起答。只读 edge 内存里的结果窗口,不读执行历史库;窗口外的位置答
// [ResultEvicted]。不带 read-your-writes 头:副本落后时答 pending,不答 412。
func (c *Client) Results(ctx context.Context, at []LogPosition, wait time.Duration) ([]Result, error) {
	if len(at) == 0 || len(at) > MaxResultPositions {
		return nil, fmt.Errorf("dexosv1: 一次按位置取结果要问 1~%d 个位置,得到 %d 个", MaxResultPositions, len(at))
	}
	if wait < 0 || wait > MaxResultWait {
		return nil, fmt.Errorf("dexosv1: 按位置取结果最多等 %s,得到 %s", MaxResultWait, wait)
	}
	parts := make([]string, len(at))
	for i, p := range at {
		parts[i] = strconv.FormatUint(p.Seq, 10) + ":" + strconv.FormatUint(uint64(p.Sub), 10)
	}
	q := url.Values{"at": {strings.Join(parts, ",")}, "waitMs": {strconv.FormatInt(wait.Milliseconds(), 10)}}
	status, header, raw, err := c.roundTrip(ctx, "/v1/results", q, marketRead)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, parseError(status, header, raw)
	}
	var body struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("dexosv1: 按位置取结果的回答解析失败: %w", err)
	}
	if len(body.Results) != len(at) {
		return nil, fmt.Errorf("dexosv1: 问了 %d 个位置,答了 %d 项", len(at), len(body.Results))
	}
	out := make([]Result, len(at))
	for i, entry := range body.Results {
		r, err := result(entry)
		if err != nil {
			return nil, fmt.Errorf("dexosv1: 位置 %d:%d: %w", at[i].Seq, at[i].Sub, err)
		}
		if r.LogPosition != at[i] {
			return nil, fmt.Errorf("dexosv1: 第 %d 项答的是 %d:%d,问的是 %d:%d", i, r.Seq, r.Sub, at[i].Seq, at[i].Sub)
		}
		out[i] = r
	}
	return out, nil
}

func result(raw json.RawMessage) (Result, error) {
	var w wireResult
	if err := json.Unmarshal(raw, &w); err != nil {
		return Result{}, err
	}
	if w.Seq == nil || w.Sub == nil {
		return Result{}, fmt.Errorf("缺 seq/sub: %s", raw)
	}
	r := Result{LogPosition: LogPosition{*w.Seq, *w.Sub}, Status: w.Status, RequestID: w.RequestID, Duplicate: w.Duplicate,
		Message: w.Message, raw: raw}
	if w.Original != nil {
		r.Original = &LogPosition{w.Original.Seq, w.Original.Sub}
	}
	switch w.Status {
	case ResultExecuted:
	case ResultRejected:
		if w.Error == nil || w.Error.Code == "" {
			return Result{}, fmt.Errorf("整笔被拒但没有拒因: %s", raw)
		}
		r.Error = &Error{Status: http.StatusUnprocessableEntity, Code: w.Error.Code, Num: w.Error.Num, Message: w.Error.Message, Body: string(raw)}
	case ResultPending, ResultNotFound, ResultEvicted:
	default:
		return Result{}, fmt.Errorf("结论 %q 不在契约内", w.Status)
	}
	if w.Duplicate != (w.Original != nil) {
		return Result{}, fmt.Errorf("duplicate 与 original 要同时给: %s", raw)
	}
	return r, nil
}

// Batch 按签好的那次批量写(下单、撤单、换单)解析一项结果,判据与同步写([Client.SendReplace] 等)相同:
// executed 返回逐项结局(重发时回执的位置与 Duplicate 同同步写,是原请求的);rejected 返回同步写 422 那个 *Error,
// 按 [ClassifyAction] 判;其余结论没有结果可解析,报错。
func (r Result) Batch(a *SignedAction) (*BatchResult, error) {
	switch r.Status {
	case ResultExecuted:
	case ResultRejected:
		return nil, r.Error
	default:
		return nil, fmt.Errorf("dexosv1: 位置 %d:%d 还没有可解析的结果(%s)", r.Seq, r.Sub, r.Status)
	}
	if !strings.EqualFold(r.RequestID, a.Request.RequestID) {
		return nil, fmt.Errorf("dexosv1: 位置 %d:%d 的请求身份 %q 不是这次签的 %s", r.Seq, r.Sub, r.RequestID, a.Request.RequestID)
	}
	receipt := Receipt{Seq: r.Seq, Sub: r.Sub, Duplicate: r.Duplicate, Request: a.Request}
	if r.Original != nil {
		receipt.Seq, receipt.Sub = r.Original.Seq, r.Original.Sub
	}
	return batchResult(a.Market, receipt, r.raw, a.Places, a.Cancels, a.Modifies)
}
