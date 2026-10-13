package dexosv1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

// 执行历史读接口(`/v1/account/fills`、`/v1/account/ledger`)的翻页契约,与旧接口相同:
//
//   - 每一轮固定在一个上界 `upper` 之内;首页给出 `epoch` 与 `upper`,**续页必须原样带回这两个值**
//     (服务端缺一即 400),否则上界会重读当前完整前缀,`hasMore == false` 就不再表示「到达首页的上界」;
//   - 纪元不符是 409(errors.Is(err, dexos.ErrHistoryEpochMismatch)):账本换了纪元,旧游标整体作废;
//   - 查询区间与缺口相交是 503([*HistoryUnavailableError]):证明不了有没有,就不给答案。
//
// 翻页封在本包里,因为那个循环有几处容易写错、而且都不会报错:续页没带回 epoch/upper、
// 把「本页不足」当成结束、游标不前进时无限空转。

// cursor 一轮翻页的输入与输出。
type cursor struct {
	after, epoch, upper string
	pageSize, limit     int
	truncated           bool
}

// historyPage 一页的公共部分;条目字段名各接口不同,另取。
type historyPage struct {
	Epoch   string  `json:"epoch"`
	Upper   string  `json:"upper"`
	Next    *string `json:"next"`
	HasMore bool    `json:"hasMore"`
}

func pageThrough[W any](ctx context.Context, c *Client, path string, params url.Values, field string, cur *cursor) ([]W, error) {
	if cur.after != "" && cur.epoch == "" {
		return nil, errors.New("dexosv1: 从游标续拉必须同时给出游标所属的纪元")
	}
	get := func(after string, limit int) (historyPage, []W, error) {
		q := cloneValues(params)
		if limit > 0 {
			q.Set("limit", strconv.Itoa(limit))
		}
		if cur.epoch != "" {
			q.Set("epoch", cur.epoch)
		}
		if cur.upper != "" {
			q.Set("upper", cur.upper)
		}
		if after != "" {
			q.Set("after", after)
		}
		var raw map[string]json.RawMessage
		if err := c.get(ctx, path, q, accountRead, &raw); err != nil {
			return historyPage{}, nil, err
		}
		var page historyPage
		whole, _ := json.Marshal(raw)
		if err := json.Unmarshal(whole, &page); err != nil {
			return historyPage{}, nil, fmt.Errorf("dexosv1: %s 响应解析失败: %w", path, err)
		}
		items, ok := raw[field]
		if !ok || page.Epoch == "" || page.Upper == "" {
			return historyPage{}, nil, fmt.Errorf("dexosv1: %s 响应缺 epoch/upper/%s", path, field)
		}
		if cur.epoch != "" && page.Epoch != cur.epoch {
			return historyPage{}, nil, fmt.Errorf("dexosv1: %s 问纪元 %s,答 %s", path, cur.epoch, page.Epoch)
		}
		var out []W
		if err := json.Unmarshal(items, &out); err != nil {
			return historyPage{}, nil, fmt.Errorf("dexosv1: %s 条目解析失败: %w", path, err)
		}
		return page, out, nil
	}
	// 从游标续拉却没给上界:先不带游标探一页取当前上界(纪元已变就在这一步 409)。
	// 这一页从账户开户位置查缺口,开户被裁出保留窗口后永远 503 —— 能从实时视图拿到位置的
	// 调用方应当自己给上界。
	if cur.after != "" && cur.upper == "" {
		page, _, err := get("", 1)
		if err != nil {
			return nil, err
		}
		cur.upper = page.Upper
	}
	var out []W
	after := cur.after
	for {
		page, items, err := get(after, cur.pageSize)
		if err != nil {
			return out, err
		}
		cur.epoch, cur.upper = page.Epoch, page.Upper
		if cur.limit > 0 && len(out)+len(items) >= cur.limit {
			cur.truncated = len(out)+len(items) > cur.limit || page.HasMore
			return append(out, items[:cur.limit-len(out)]...), nil
		}
		out = append(out, items...)
		if !page.HasMore {
			return out, nil
		}
		if page.Next == nil || *page.Next == "" || *page.Next == after {
			return out, fmt.Errorf("dexosv1: %s 说还有更多,但游标没有前进(after=%q)", path, after)
		}
		after = *page.Next
	}
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// HistoryQuery 执行历史查询的游标与边界(成交与流水共用)。
type HistoryQuery struct {
	// After 游标 `<seq>-<sub>-<idx>`:只要严格大于它的条目。空 = 从头。
	After string
	// Epoch 游标所属的纪元(上一次结果的 Epoch)。After 非空时必填。
	Epoch string
	// Upper 本轮上界。空 = 由服务端取当前完整前缀(从游标续拉时 SDK 先探一页取得,见 pageThrough)。
	Upper string
	// PageSize 每次请求取多少条(1..=1000,0 = 服务端缺省)。只影响往返次数。
	PageSize int
	// Limit 最多返回多少条,0 = 翻到服务端说没有更多。补拉要翻到底,别用它做补拉。
	Limit int
}

// Fill 一笔成交,按本账户视角。ID 与 WS 帧里同一笔的 id 逐字符相同,可直接去重与续拉。
type Fill struct {
	ID      string
	Seq     uint64
	OrderID *uint64
	Symbol  string
	Side    string // 本账户这一边:"buy" | "sell"
	Role    string // "taker" | "maker"
	Price   uint32
	Lots    uint64
	// Fee 本账户这一边的费(十进制,FeeToken 单位;maker 为负 = 返佣)。
	Fee      string
	FeeToken string
	// TS 成交所在区块的时间(毫秒);nil = 服务端不知道,不是 1970 年。
	TS *int64
}

type wireFill struct {
	ID       string  `json:"id"`
	Seq      uint64  `json:"seq"`
	OrderID  *string `json:"orderId"`
	Symbol   string  `json:"symbol"`
	Side     string  `json:"side"`
	Role     string  `json:"role"`
	Price    string  `json:"price"`
	Size     string  `json:"size"`
	Fee      string  `json:"fee"`
	FeeToken string  `json:"feeToken"`
	TS       *int64  `json:"ts"`
}

// FillHistory 一轮补拉的结果。「没有更多」只表示到 Upper 为止;Truncated = 被 Limit 截断,没补齐到 Upper。
type FillHistory struct {
	Epoch     string
	Upper     string
	Truncated bool
	Fills     []Fill
}

// Fills 账户成交(`/v1/account/fills`),自动翻页到上界。symbol 为空 = 全市场。
// 失败时返回已取到的部分与错误。
func (c *Client) Fills(ctx context.Context, cat *Catalog, ref AccountRef, symbol string, q HistoryQuery) (*FillHistory, error) {
	params := ref.values()
	if symbol != "" {
		params.Set("symbol", symbol)
	}
	cur := &cursor{after: q.After, epoch: q.Epoch, upper: q.Upper, pageSize: q.PageSize, limit: q.Limit}
	wires, err := pageThrough[wireFill](ctx, c, "/v1/account/fills", params, "fills", cur)
	h := &FillHistory{Epoch: cur.epoch, Upper: cur.upper, Truncated: cur.truncated}
	for _, w := range wires {
		f, ferr := w.fill(cat)
		if ferr != nil {
			return h, ferr
		}
		h.Fills = append(h.Fills, f)
	}
	return h, err
}

func (w wireFill) fill(cat *Catalog) (Fill, error) {
	m, err := cat.Market(w.Symbol)
	if err != nil {
		return Fill{}, err
	}
	f := Fill{ID: w.ID, Seq: w.Seq, Symbol: w.Symbol, Side: w.Side, Role: w.Role, Fee: w.Fee, FeeToken: w.FeeToken, TS: w.TS}
	if w.OrderID != nil {
		oid, err := strconv.ParseUint(*w.OrderID, 10, 64)
		if err != nil {
			return Fill{}, fmt.Errorf("dexosv1: 成交 %s orderId %q: %w", w.ID, *w.OrderID, err)
		}
		f.OrderID = &oid
	}
	if f.Price, err = m.Scale.Ticks(w.Price); err != nil {
		return Fill{}, fmt.Errorf("dexosv1: 成交 %s price: %w", w.ID, err)
	}
	if f.Lots, err = m.Scale.Lots(w.Size); err != nil {
		return Fill{}, fmt.Errorf("dexosv1: 成交 %s size: %w", w.ID, err)
	}
	return f, nil
}

// LedgerEntry 一条资金流水。Amount 带符号,Token 单位的十进制;Symbol nil = 与市场无关(入金等)。
type LedgerEntry struct {
	ID     string  `json:"id"`
	Seq    uint64  `json:"seq"`
	Sub    uint16  `json:"sub"`
	Idx    uint32  `json:"idx"`
	Kind   string  `json:"kind"`
	Token  string  `json:"token"`
	Amount string  `json:"amount"`
	Symbol *string `json:"symbol"`
}

// LedgerQuery 流水查询:Kind / Token 是可选过滤,其余同 [HistoryQuery]。
type LedgerQuery struct {
	HistoryQuery
	Kind  string
	Token string
}

// LedgerHistory 一轮补拉的结果,语义同 [FillHistory]。
type LedgerHistory struct {
	Epoch     string
	Upper     string
	Truncated bool
	Entries   []LedgerEntry
}

// Ledger 账户资金流水(`/v1/account/ledger`),自动翻页到上界。失败时返回已取到的部分与错误。
func (c *Client) Ledger(ctx context.Context, ref AccountRef, q LedgerQuery) (*LedgerHistory, error) {
	params := ref.values()
	if q.Kind != "" {
		params.Set("kind", q.Kind)
	}
	if q.Token != "" {
		params.Set("token", q.Token)
	}
	cur := &cursor{after: q.After, epoch: q.Epoch, upper: q.Upper, pageSize: q.PageSize, limit: q.Limit}
	entries, err := pageThrough[LedgerEntry](ctx, c, "/v1/account/ledger", params, "entries", cur)
	return &LedgerHistory{Epoch: cur.epoch, Upper: cur.upper, Truncated: cur.truncated, Entries: entries}, err
}
