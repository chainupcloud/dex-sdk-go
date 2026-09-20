package dexos

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// 成交账本与**断线补拉**。
//
// 与行情成交(`/trades`)的分工要说清楚,因为用错了不会报错、只会少数据:
//
//   - `/trades` 是**行情**:市场级最新若干笔,降序,上限 200。给界面看的。
//   - `/fills`(本文件)是**账本**:账户级全量,按成交身份**升序**,游标可续。
//     对账、算盈亏、断线补拉走这条。
//
// # 为什么非要有稳定的成交身份
//
// `seq` 是 Raft 日志下标,**一条日志承载多个事件** —— 同一条命令扫十个 maker
// 就是十笔成交,它们共用一个 seq;没产生事件的日志还会让它跳号。
// 拿 seq 当成交 ID,同价同量的两笔成交就分不开,而做市商一轮报价里
// 同价同量比比皆是:分不开 = 逐笔对冲与逐笔盈亏都做不了。
//
// 所以身份是三段的 `"<seq>-<sub>-<idx>"`:日志下标、条目内请求下标、批内事件下标。
// 三段都来自确定性重放,每个副本算出同一个值。
//
// # 断线之后
//
// 记住事件流上收到的最后一个 [Fill.ID],重连后拿它当 [FillQuery.After] 调 [Session.Fills],
// 拉到服务端说没有更多为止。重叠部分按 ID 去重 —— 与补拉重叠是**正常**的,不是 bug。
//
// 「停止」不等于「补回来」:发现 Lagged 之后停下来只是不再把错的数据往下传,
// 断线期间那几笔成交仍然得有人去取。这就是这个接口存在的理由。

// Fill 一笔成交,**按本账户视角**给。
//
// 视角不是修辞:同一笔成交对买卖两边是两个不同的费、两张不同的单、
// 两个相反的方向。服务端已经翻好,不要再按 taker/maker 自己翻一遍。
type Fill struct {
	// ID 成交身份 "<seq>-<sub>-<idx>"。与事件流里同一笔的 id 逐字符相同,
	// 可直接用于去重与续拉。
	ID  string `json:"id"`
	Seq uint64 `json:"seq"`
	Sub uint16 `json:"sub"`
	Idx uint32 `json:"idx"`

	Market uint16 `json:"market"`
	Price  uint32 `json:"price"`
	Lots   uint64 `json:"lots"`
	// Side 本账户这一边的方向:"buy" | "sell"。
	Side string `json:"side"`
	// Role 本账户这一边是 "taker" 还是 "maker"。
	Role string `json:"role"`

	// Order 本账户那张单的序号;CounterOrder 是对手方的。
	Order        uint64 `json:"order"`
	CounterOrder uint64 `json:"counterOrder"`

	// Fee 本账户这一边的费(定点整数字符串,maker 为负 = 返佣)。
	//
	// **指针,因为它可能是 null**,而 null 与 "0" 是两件完全不同的事:
	// null = 服务端不知道(2026-09-19 之前落库的旧成交),"0" = 确实没收费。
	// 把 null 当 0 处理会让盈亏算错而毫无征兆。
	//
	// 口径:taker 费按**整单**收一次(trunc(Σ 腿名义额 × 费率)),再按名义额用
	// 累积法摊回每一笔 —— 同一张单各笔 Fee 之和恒等于这张单实收的费
	// (dex-os INV-FEE-ATTRIB),可以直接用它算逐笔与整单盈亏。
	Fee *string `json:"fee"`

	TS int64 `json:"ts"`
}

// FillQuery 成交查询条件。
type FillQuery struct {
	// After 游标:只要**严格大于**这个身份的成交。空 = 从头。
	After string
	// Market 只看某个市场。nil = 全部。
	Market *uint16
	// PageSize 每次请求取多少条(1..=1000,0 = 用服务端默认)。
	// 它只影响往返次数,不影响结果。
	PageSize int
	// Limit 最多返回多少条,0 = 不限(一直翻到服务端说没有更多)。
	//
	// 补拉要的就是「翻到底」,所以默认不限。设上限只在你确实只想看一眼时用 ——
	// 用它做补拉等于给自己留一个**看起来成功的漏拉**。
	Limit int
}

// fillsPage 一页的线格式。
type fillsPage struct {
	Fills   []Fill `json:"fills"`
	Next    string `json:"next"`
	HasMore bool   `json:"hasMore"`
}

// Fills 拉取账户成交,**自动翻页直到服务端说没有更多**。
//
//	// 断线补拉:从上次见到的最后一笔往后
//	fills, err := s.Fills(ctx, dexos.FillQuery{After: lastSeenID})
//
// 翻页封在这里而不是交给调用方,是因为那个 while 循环有两个容易写错的地方,
// 而两个都**不会报错**:把「本页返回不足」当成结束(服务端可能恰好取满),
// 以及忘了判游标不前进(服务端异常时无限空转)。
func (c *Client) Fills(ctx context.Context, account uint32, q FillQuery) ([]Fill, error) {
	var out []Fill
	cursor := q.After
	for {
		v := url.Values{}
		v.Set("account", strconv.FormatUint(uint64(account), 10))
		if cursor != "" {
			v.Set("after", cursor)
		}
		if q.Market != nil {
			v.Set("market", strconv.FormatUint(uint64(*q.Market), 10))
		}
		if q.PageSize > 0 {
			v.Set("limit", strconv.Itoa(q.PageSize))
		}

		var page fillsPage
		if err := c.do(ctx, http.MethodGet, "/fills?"+v.Encode(), nil, &page); err != nil {
			return out, err
		}
		out = append(out, page.Fills...)

		if q.Limit > 0 && len(out) >= q.Limit {
			return out[:q.Limit], nil
		}
		if !page.HasMore {
			return out, nil
		}
		// 游标必须前进。不前进而 hasMore 仍为 true = 服务端异常,
		// 朴素的循环会在这里永远转下去 —— 而补拉是启动路径上的一步,
		// 卡住的表现是"进程起不来,也不说为什么"。宁可报错。
		if page.Next == "" || page.Next == cursor {
			return out, fmt.Errorf(
				"dexos: 成交补拉的游标不前进(停在 %q,服务端仍说有更多)—— 已取 %d 笔,不再继续",
				cursor, len(out))
		}
		cursor = page.Next
	}
}

// Fills 本会话账户的成交(见 [Client.Fills])。
func (s *Session) Fills(ctx context.Context, q FillQuery) ([]Fill, error) {
	return s.Client.Fills(ctx, s.Account, q)
}
