package dexos

import (
	"context"
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
// 记住事件流上收到的最后一个 [Fill.ID] **和它所属的纪元**(上一次 [FillHistory.Epoch]),
// 重连后拿它们当 [FillQuery.After] / [FillQuery.Epoch] 调 [Session.Fills],拉到上界为止。
// 重叠部分按 ID 去重 —— 与补拉重叠是**正常**的,不是 bug。纪元变了(账本换纪元)是
// [ErrHistoryEpochMismatch]:旧游标整体作废,不能当成「没有新成交」。
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

	// TS 成交所在区块的时间(毫秒)。nil = 服务端不知道(之前没有过区块),不是 1970 年。
	TS *int64 `json:"ts"`
}

// FillQuery 成交查询条件。
type FillQuery struct {
	// After 游标:只要**严格大于**这个身份的成交。空 = 从头。
	After string
	// Epoch 游标所属的纪元(上一次结果的 [FillHistory.Epoch])。After 非空时必填 ——
	// 游标只在它自己的纪元里有意义。
	Epoch string
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

// FillHistory 一次补拉的结果。Upper 是本轮的上界:「没有更多」只表示到 Upper 为止。
// Truncated = 被 Limit 截断,只覆盖到最后一条,**没有**补齐到 Upper。
type FillHistory struct {
	Epoch     string
	Upper     string
	Truncated bool
	Fills     []Fill
}

// Fills 拉取账户成交,**自动翻页直到上界**。
//
//	// 断线补拉:从上次见到的最后一笔往后
//	h, err := s.Fills(ctx, dexos.FillQuery{After: lastSeenID, Epoch: lastEpoch})
//
// 翻页封在这里而不是交给调用方,是因为那个循环有几处容易写错、而且都**不会报错**:
// 续页没带回第一页的 epoch/upper(服务端 400)、把「本页返回不足」当成结束、
// 以及忘了判游标不前进(服务端异常时无限空转)。失败时返回已取到的部分与错误。
func (c *Client) Fills(ctx context.Context, account uint32, q FillQuery) (*FillHistory, error) {
	params := url.Values{"account": {strconv.FormatUint(uint64(account), 10)}}
	if q.Market != nil {
		params.Set("market", strconv.FormatUint(uint64(*q.Market), 10))
	}
	cur := &historyCursor{after: q.After, epoch: q.Epoch, pageSize: q.PageSize, limit: q.Limit}
	fills, err := pageThrough[Fill](ctx, c, "/fills", params, "fills", cur)
	return &FillHistory{Epoch: cur.epochOut, Upper: cur.upper, Truncated: cur.truncated, Fills: fills}, err
}

// Fills 本会话账户的成交(见 [Client.Fills])。
func (s *Session) Fills(ctx context.Context, q FillQuery) (*FillHistory, error) {
	return s.Client.Fills(ctx, s.Account, q)
}
