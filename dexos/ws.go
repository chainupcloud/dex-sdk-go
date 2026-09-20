package dexos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
)

// WebSocket 事件流。
//
// 协议刻意极简:连上 /ws 就开始收,**没有订阅报文**。服务端推的是内核事件的
// 全量流,过滤在客户端做。
//
// 过滤参数只影响当前连接；重连只接收未来事件，不能证明历史完整。
//
// 消息形如:
//
//	{"seq": 12345, "kind": "Fill", "data": {...}}
//
// **seq 是 Raft 日志序号,不是逐事件计数器。**同一条命令产出的多个事件共享同一个
// seq(实测一条 BatchCancel 的 22 条 OrderCanceled 全是同一个 seq),而没有产生
// 事件的日志条目会让它跳号。所以它只保证**非递减**,既不连续也不唯一 ——
// 拿 seq 做丢帧检测会疯狂误报,而且**根本区分不出**「跳号是因为无事件的日志」
// 还是「因为丢了帧」。
//
// 丢帧由服务端**显式下发**:
//
//	{"kind": "Lagged", "dropped": 128}
//
// 收到它意味着广播通道积压、中间那些事件永远拿不到了 —— 本地的持仓/盘口镜像
// 已不可信，必须停止并补齐历史。快照不能补回逐笔成交。丢一条自己的 Fill,
// 之后就一直拿着错的仓位报价,而业务层完全看不出来。

// Event 一条事件,带**完整身份** (Seq, Sub, Idx)。
//
// 三段缺一不可,而 Seq 单独没有用:Seq 是 Raft 日志下标(一条日志承载多个事件,
// 无事件的日志还会让它跳号);Sub 是本请求在该条目内的下标(条目级聚批);Idx 是
// 本事件在该 (Seq, Sub) 里的下标。拿 Seq 当事件身份,同价同量的两笔成交就分不开。
// 用 [Event.ID] 取拼好的那一个 —— 对 Fill 来说它就是成交 ID,与 REST `/fills`
// 里同一笔的 id 逐字符相同,断线后直接拿它当 [FillQuery.After] 补拉。
type Event struct {
	Seq  uint64          `json:"seq"`
	Sub  uint16          `json:"sub"`
	Idx  uint32          `json:"idx"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// ID 事件身份 "<seq>-<sub>-<idx>"。
func (e Event) ID() string {
	return fmt.Sprintf("%d-%d-%d", e.Seq, e.Sub, e.Idx)
}

// Stream 事件流订阅。
type Stream struct {
	Events <-chan Event
	// Lost 在服务端报告丢帧时收到丢弃条数。
	//
	// seq 推不出来(见上)。收到后流终止，Err 同时报原因；快照不足以恢复成交账。
	Lost <-chan uint64
	// Err 在流终止时收到原因;之后 Events 关闭。
	Err <-chan error
}

// SubscribeOption 连接期过滤条件。
//
// 过滤只作用于当前连接，不代表断线期间事件会回放。
// 代价是改条件要重连 —— 对做市不是问题,关注面在进程生命周期内不变。
type SubscribeOption func(*subOpts)

type subOpts struct {
	markets  []uint16
	accounts []uint32
}

// WithMarkets 只收这些市场的事件。
func WithMarkets(m ...uint16) SubscribeOption {
	return func(o *subOpts) { o.markets = append(o.markets, m...) }
}

// WithAccounts 只收这些账户的事件。Fill 的两条腿(maker/taker)任一命中即算。
func WithAccounts(a ...uint32) SubscribeOption {
	return func(o *subOpts) { o.accounts = append(o.accounts, a...) }
}

// Subscribe 连接事件流。ctx 取消即断开。
//
// 不带 option 时收**全量**。带了则是**并集**语义:市场或账户命中任一即投递 ——
// 做市的典型诉求正是「市场 0 的全部行情,加上我自己账户的全部动静」,
// 那是 OR 不是 AND。无作用域的系统事件(BlockBegun、治理类变更)始终投递。
//
//	st, _ := c.Subscribe(ctx, dexos.WithMarkets(0), dexos.WithAccounts(master))
//
// 断线、坏帧或 Lagged 均终止流并报 ErrStreamGap(seq 回退**不算**:它不单调,见循环内注释);
// 不自动重连 —— 重连会掩盖
// 成交缺口。缺口有正路可补:记住流上最后一个 [Event.ID],用 [Session.Fills]
// (FillQuery{After: 那个 id})把断开期间的成交从服务端账本 `/fills` 拉齐、按 id 去重,
// 再重新 Subscribe。持仓 / 盘口快照(`/risk` `/book`)只能校准状态,补不回逐笔成交。
func (c *Client) Subscribe(ctx context.Context, opts ...SubscribeOption) (*Stream, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("BaseURL 不合法:%w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return nil, fmt.Errorf("BaseURL 应为 http/https,得到 %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/ws"

	var o subOpts
	for _, f := range opts {
		f(&o)
	}
	q := url.Values{}
	if len(o.markets) > 0 {
		parts := make([]string, len(o.markets))
		for i, m := range o.markets {
			parts[i] = strconv.FormatUint(uint64(m), 10)
		}
		q.Set("markets", strings.Join(parts, ","))
	}
	if len(o.accounts) > 0 {
		parts := make([]string, len(o.accounts))
		for i, a := range o.accounts {
			parts[i] = strconv.FormatUint(uint64(a), 10)
		}
		q.Set("accounts", strings.Join(parts, ","))
	}
	u.RawQuery = q.Encode()

	conn, response, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return nil, fmt.Errorf("dexos: WS 连接失败: %w", err)
	}
	conn.SetReadLimit(8 << 20)
	events := make(chan Event, 1024)
	lost := make(chan uint64, 1)
	errc := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(lost)
		defer close(errc)
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				if ctx.Err() != nil {
					errc <- ctx.Err()
				} else {
					errc <- fmt.Errorf("%w: %v", ErrStreamGap, err)
				}
				return
			}
			var wire struct {
				Seq     *uint64         `json:"seq"`
				Sub     uint16          `json:"sub"`
				Idx     uint32          `json:"idx"`
				Kind    string          `json:"kind"`
				Data    json.RawMessage `json:"data"`
				Dropped *uint64         `json:"dropped"`
			}
			if json.Unmarshal(raw, &wire) != nil || wire.Kind == "" {
				errc <- fmt.Errorf("%w: WS 帧无法解析", ErrStreamGap)
				return
			}
			if wire.Kind == "Lagged" {
				if wire.Dropped == nil || *wire.Dropped == 0 {
					errc <- fmt.Errorf("%w: Lagged 缺有效 dropped", ErrStreamGap)
					return
				}
				lost <- *wire.Dropped
				errc <- fmt.Errorf("%w: 服务端丢弃 %d 个事件", ErrStreamGap, *wire.Dropped)
				return
			}
			var data map[string]json.RawMessage
			if wire.Seq == nil || json.Unmarshal(wire.Data, &data) != nil || data == nil {
				errc <- fmt.Errorf("%w: WS 帧缺 seq/data", ErrStreamGap)
				return
			}
			// seq 不单调:keeper 的系统命令事件(OracleUpdated / FundingSampled / BlockBegun)
			// 与订单事件走不同的派生路径,真节点上每几分钟就有一次小幅回退(实测 3581 帧 4 次)。
			// 网关从未承诺 seq 顺序,身份是 (seq, sub, idx) 三段;把回退当成流损坏终止,
			// 等于让每个 WS 消费者几分钟断一次。
			ev := Event{Seq: *wire.Seq, Sub: wire.Sub, Idx: wire.Idx, Kind: wire.Kind, Data: wire.Data}
			select {
			case events <- ev:
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			}
		}
	}()
	return &Stream{Events: events, Lost: lost, Err: errc}, nil
}

// ErrStreamGap 表示流完整性已失去。快照补不回逐笔成交;成交账本 `/fills`
// ([Session.Fills],按成交身份游标续拉)可以 —— 补齐并去重后再重新 Subscribe。
var ErrStreamGap = errors.New("dexos: 事件流完整性未知,请用 Fills(After: 最后一个事件 id) 补拉后重建连接")
