package dexos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// 执行历史读接口(/fills /ledger /events)的共同契约(dex-os #1/#2/#5,
// crates/gateway/src/api.rs 的 fills / account_ledger / events_backfill):
//
//   - 每一页都固定在一个完整上界 `upper` 之内;第一页给出 `epoch` 与 `upper`,
//     **续页必须原样带回这两个值**,缺一即 400 —— 否则上界会重读当前完整前缀,
//     并发新增混进本轮,`hasMore == false` 就不再表示「到达第一页的上界」;
//   - 纪元不符是 409 `history_epoch_mismatch`:账本换了纪元,旧游标整体作废;
//   - 查询区间与本副本的缺口相交是 503 `history_unavailable` 并列出缺口 ——
//     证明不了有没有,就不给答案,绝不回空页。

// Gap 本副本执行历史里证明不了的一段日志 [First, Last]。
type Gap struct {
	First uint64 `json:"first"`
	Last  uint64 `json:"last"`
}

// HistoryUnavailableError 查询区间与执行历史的缺口相交(503 history_unavailable)。
//
// 这不是「没有数据」:缺口里有没有本账户的记录,本副本证明不了。调用方要么等本副本
// 从对端导入缺口,要么换一个没有缺口的副本;不能把它当空结果继续。
type HistoryUnavailableError struct {
	Gaps []Gap
	API  *APIError
}

func (e *HistoryUnavailableError) Error() string {
	return fmt.Sprintf("dexos: 执行历史有缺口,查询区间证明不了完整 %v: %v", e.Gaps, e.API)
}

func (e *HistoryUnavailableError) Unwrap() error { return e.API }

// ErrHistoryEpochMismatch 请求的纪元与节点当前纪元不符(409):账本已换纪元,旧游标作废。
var ErrHistoryEpochMismatch = errors.New("dexos: 执行历史纪元与节点不符(账本已换纪元,旧游标作废)")

// classifyAPIError 把网关错误体里有语义的两类挑出来,其余保持 *APIError。
func classifyAPIError(ae *APIError) error {
	var body struct {
		Error  string `json:"error"`
		Status string `json:"status"`
		Gaps   []Gap  `json:"gaps"`
	}
	if json.Unmarshal([]byte(ae.Body), &body) != nil {
		return ae
	}
	switch {
	case ae.Status == http.StatusServiceUnavailable && body.Status == "history_unavailable":
		return &HistoryUnavailableError{Gaps: body.Gaps, API: ae}
	case ae.Status == http.StatusConflict && body.Error == "history_epoch_mismatch":
		return fmt.Errorf("%w: %w", ErrHistoryEpochMismatch, ae)
	}
	return ae
}

// historyPage 一页的公共部分;条目按接口各自的字段名另取。
type historyPage struct {
	Epoch       string  `json:"epoch"`
	Upper       string  `json:"upper"`
	Next        *string `json:"next"`
	HasMore     bool    `json:"hasMore"`
	CompleteSeq *uint64 `json:"completeSeq"`
}

// historyCursor 本轮翻页的起点与边界。
type historyCursor struct {
	after, epoch    string
	pageSize, limit int
	epochOut, upper string
	completeSeq     uint64
	truncated       bool
}

// pageThrough 按上述契约翻完一个历史接口,返回全部条目(失败时返回已取到的部分与错误)。
//
// 从已存游标续拉时,游标属于哪个纪元只有调用方知道,所以 after 必须与 epoch 一起给;
// 当前上界先以该纪元探一页(limit=1、不带 after)取得 —— 纪元已变就在这一步 409。
func pageThrough[T any](ctx context.Context, c *Client, path string, params url.Values, field string, cur *historyCursor) ([]T, error) {
	if cur.after != "" && cur.epoch == "" {
		return nil, errors.New("dexos: 从游标续拉必须同时给出游标所属的纪元")
	}
	get := func(extra url.Values) (historyPage, []T, error) {
		v := url.Values{}
		for k, vs := range params {
			v[k] = vs
		}
		for k, vs := range extra {
			v[k] = vs
		}
		var raw map[string]json.RawMessage
		if err := c.do(ctx, http.MethodGet, path+"?"+v.Encode(), nil, &raw); err != nil {
			return historyPage{}, nil, err
		}
		var page historyPage
		whole, _ := json.Marshal(raw)
		if err := json.Unmarshal(whole, &page); err != nil {
			return historyPage{}, nil, fmt.Errorf("dexos: %s 响应解析失败: %w", path, err)
		}
		items, ok := raw[field]
		if !ok || page.Epoch == "" || page.Upper == "" {
			return historyPage{}, nil, fmt.Errorf("dexos: %s 响应缺 epoch/upper/%s", path, field)
		}
		var out []T
		if err := json.Unmarshal(items, &out); err != nil {
			return historyPage{}, nil, fmt.Errorf("dexos: %s 条目解析失败: %w", path, err)
		}
		return page, out, nil
	}
	size := func() url.Values {
		v := url.Values{}
		if cur.pageSize > 0 {
			v.Set("limit", strconv.Itoa(cur.pageSize))
		}
		if cur.epoch != "" {
			v.Set("epoch", cur.epoch)
		}
		return v
	}
	after := cur.after
	if after != "" {
		probe := url.Values{"limit": {"1"}, "epoch": {cur.epoch}}
		page, _, err := get(probe)
		if err != nil {
			return nil, err
		}
		cur.epochOut, cur.upper = page.Epoch, page.Upper
		if page.CompleteSeq != nil {
			cur.completeSeq = *page.CompleteSeq
		}
	}
	var all []T
	for {
		q := size()
		if after != "" {
			q.Set("after", after)
			q.Set("epoch", cur.epochOut)
			q.Set("upper", cur.upper)
		}
		page, items, err := get(q)
		if err != nil {
			return all, err
		}
		if cur.upper == "" {
			cur.epochOut, cur.upper = page.Epoch, page.Upper
			if page.CompleteSeq != nil {
				cur.completeSeq = *page.CompleteSeq
			}
		} else if page.Epoch != cur.epochOut || page.Upper != cur.upper {
			// 两段不同的历史不能拼成一份
			return all, fmt.Errorf("dexos: %s 页间纪元/上界漂移(%s %s → %s %s)", path, cur.epochOut, cur.upper, page.Epoch, page.Upper)
		}
		all = append(all, items...)
		if cur.limit > 0 && len(all) >= cur.limit && (len(all) > cur.limit || page.HasMore) {
			cur.truncated = true
			return all[:cur.limit], nil
		}
		if !page.HasMore {
			return all, nil
		}
		// 游标必须前进。不前进而 hasMore 仍为 true = 服务端异常,朴素的循环会永远转下去。
		if page.Next == nil || *page.Next == "" || *page.Next == after {
			return all, fmt.Errorf("dexos: %s 游标不前进(停在 %q,服务端仍说有更多)—— 已取 %d 条,不再继续", path, after, len(all))
		}
		after = *page.Next
	}
}

// LedgerEntry 一条资金流水:本账户某 token 余额的一次有符号变化(Σ流水 = 余额变化)。
//
// Kind 是经济分类(外部契约,只增不改;28 种,见 dex-os crates/shards/src/journal.rs);
// Amount 是 token 最小单位的十进制字符串(i128)。Sub = 65535 是纪元折叠行
// (资金费 / 社会化分摊 / 结算价平仓就地结算,不属于任何命令)。可空字段 nil = 不适用。
type LedgerEntry struct {
	ID           string  `json:"id"`
	Seq          uint64  `json:"seq"`
	Sub          uint16  `json:"sub"`
	Idx          uint32  `json:"idx"`
	Kind         string  `json:"kind"`
	Token        uint32  `json:"token"`
	Amount       string  `json:"amount"`
	Market       *uint16 `json:"market"`
	Counterparty *uint32 `json:"counterparty"`
	Order        *uint64 `json:"order"`
	CounterOrder *uint64 `json:"counterOrder"`
	Reference    *string `json:"reference"`
	TS           *int64  `json:"ts"`
	// OutboxStatus / AckedAt 只出现在出金(send_asset)行:pending / acked,按同一上界给出。
	OutboxStatus *string `json:"outboxStatus,omitempty"`
	AckedAt      *string `json:"ackedAt,omitempty"`
}

// LedgerQuery 流水查询条件。After 非空时必须同时给 Epoch(见 FillQuery)。
type LedgerQuery struct {
	After    string
	Epoch    string
	Token    *uint32
	Kind     string
	PageSize int
	Limit    int
}

// LedgerHistory 一次翻页的结果:Upper 是本轮的上界(与 /equity 的 upper 同格式)。
// Truncated = 被 Limit 截断,只覆盖到最后一条,**没有**补齐到 Upper。
type LedgerHistory struct {
	Epoch     string
	Upper     string
	Truncated bool
	Entries   []LedgerEntry
}

// Ledger 账户资金流水,**自动翻页直到上界**。
func (c *Client) Ledger(ctx context.Context, account uint32, q LedgerQuery) (*LedgerHistory, error) {
	params := url.Values{"account": {strconv.FormatUint(uint64(account), 10)}}
	if q.Token != nil {
		params.Set("token", strconv.FormatUint(uint64(*q.Token), 10))
	}
	if q.Kind != "" {
		params.Set("kind", q.Kind)
	}
	cur := &historyCursor{after: q.After, epoch: q.Epoch, pageSize: q.PageSize, limit: q.Limit}
	entries, err := pageThrough[LedgerEntry](ctx, c, "/ledger", params, "entries", cur)
	return &LedgerHistory{Epoch: cur.epochOut, Upper: cur.upper, Truncated: cur.truncated, Entries: entries}, err
}

// EventQuery 全局事件补拉条件(不分账户/市场)。After 非空时必须同时给 Epoch。
type EventQuery struct {
	After    string
	Epoch    string
	PageSize int
	Limit    int
}

// EventHistory 事件补拉结果。事件身份与 WS 推送的同一条逐字符相同([Event.ID])。
// Truncated = 被 Limit 截断,只覆盖到最后一条,**没有**补齐到 Upper / CompleteSeq。
type EventHistory struct {
	Epoch       string
	Upper       string
	CompleteSeq uint64
	Truncated   bool
	Events      []Event
}

// Events 从执行历史补拉事件(WS 丢帧 / 断线之后),自动翻页直到上界。
func (c *Client) Events(ctx context.Context, q EventQuery) (*EventHistory, error) {
	cur := &historyCursor{after: q.After, epoch: q.Epoch, pageSize: q.PageSize, limit: q.Limit}
	events, err := pageThrough[Event](ctx, c, "/events", url.Values{}, "events", cur)
	return &EventHistory{Epoch: cur.epochOut, Upper: cur.upper, CompleteSeq: cur.completeSeq, Truncated: cur.truncated, Events: events}, err
}

// Equity 同水位权益快照(dex-os #5,GET /equity)。
//
// 一代读模型视图给出余额、仓位与各风险组 NC;流水合计取同一段日志前缀 —— 两者覆盖
// 同一段历史,服务端核对不等即 500(不给自相矛盾的快照)。金额全是抵押 token 最小单位的
// 十进制字符串;Upper 与 /ledger 同格式,带着它翻 /ledger 拿到的正是合计进来的流水。
type Equity struct {
	Account   uint32           `json:"account"`
	Epoch     string           `json:"epoch"`
	ViewEpoch uint64           `json:"viewEpoch"`
	Upper     string           `json:"upper"`
	Groups    []EquityGroup    `json:"groups"`
	Balances  []EquityBalance  `json:"balances"`
	Positions []EquityPosition `json:"positions"`
}

// EquityGroup 一个风险组(按抵押 token)。Equity = NC;恒等式见 dex-os api.rs equity_snapshot。
type EquityGroup struct {
	Token          uint32 `json:"token"`
	Balance        string `json:"balance"`
	Equity         string `json:"equity"`
	NetInflow      string `json:"netInflow"`
	RealizedPnl    string `json:"realizedPnl"`
	UnrealizedPnl  string `json:"unrealizedPnl"`
	AccruedFunding string `json:"accruedFunding"`
}

// EquityBalance 一个 token 的余额与按分类的流水合计(ByKind 键 = LedgerEntry.Kind)。
type EquityBalance struct {
	Token     uint32            `json:"token"`
	Balance   string            `json:"balance"`
	Frozen    string            `json:"frozen"`
	NetInflow string            `json:"netInflow"`
	ByKind    map[string]string `json:"byKind"`
}

// EquityPosition 一个仓位的估值。
type EquityPosition struct {
	Market          uint16 `json:"market"`
	Token           uint32 `json:"token"`
	Lots            int64  `json:"lots"`
	Oracle          string `json:"oracle"`
	QuotePerTickLot string `json:"quotePerTickLot"`
	Value           string `json:"value"`
	CostBasis       string `json:"costBasis"`
	UnrealizedPnl   string `json:"unrealizedPnl"`
	AccruedFunding  string `json:"accruedFunding"`
}

// Equity 取同水位权益快照。epoch 非空时要求节点仍在该纪元(不符即 ErrHistoryEpochMismatch)。
// 账户不存在返回 ErrNotRegistered;服务端核对不等(500)、历史有缺口(503)一律是错误,不给快照。
func (c *Client) Equity(ctx context.Context, account uint32, epoch string) (*Equity, error) {
	v := url.Values{"account": {strconv.FormatUint(uint64(account), 10)}}
	if epoch != "" {
		v.Set("epoch", epoch)
	}
	var out Equity
	if err := c.do(ctx, http.MethodGet, "/equity?"+v.Encode(), nil, &out); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %w", ErrNotRegistered, err)
		}
		return nil, err
	}
	if out.Epoch == "" || out.Upper == "" || out.Groups == nil || out.Balances == nil || out.Positions == nil || out.Account != account {
		return nil, errors.New("dexos: 权益快照缺 epoch/upper/groups/balances/positions 或账户不符")
	}
	return &out, nil
}
