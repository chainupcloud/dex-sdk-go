package dexos

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocket 事件流。
//
// 协议刻意极简:连上 /ws 就开始收,**没有订阅报文**。服务端推的是内核事件的
// 全量流,过滤在客户端做。
//
// 这么设计的代价是带宽,收益是没有「订阅状态」这个东西 —— 重连即完整,
// 不需要在重连后重放订阅、也不会出现「以为订上了其实没有」的静默失败。
// 对做市和风控这类必须看到每一笔的场景,全量流反而是对的。
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
// 已不可信,必须重拉快照。对做市这是正确性问题不是性能问题:丢一条自己的 Fill,
// 之后就一直拿着错的仓位报价,而业务层完全看不出来。

// Event 一条事件。
type Event struct {
	Seq  uint64          `json:"seq"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// Stream 事件流订阅。
type Stream struct {
	Events <-chan Event
	// Lost 在服务端报告丢帧时收到丢弃条数。
	//
	// 这是**唯一**可靠的丢帧信号 —— seq 推不出来(见上)。收到即应重拉快照
	// (/risk/:id、/book/:m),本地镜像已不可信。
	Lost <-chan uint64
	// Err 在流终止时收到原因;之后 Events 关闭。
	Err <-chan error
}

// SubscribeOption 连接期过滤条件。
//
// 刻意做成**连接参数**而不是订阅协议:没有「订阅状态」这个东西,重连即完整,
// 不需要在重连后重放订阅、也不会出现「以为订上了其实没有」的静默失败。
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
// 自动重连:断线后按指数退避重试(250ms → 8s 封顶),直到 ctx 取消。
//
// **重连本身就意味着丢帧**:断开期间的事件不会补发。所以重连后同样应当重拉快照,
// 与收到 Lost 时的处置一样。
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

	events := make(chan Event, 1024)
	lost := make(chan uint64, 16)
	errc := make(chan error, 1)

	go func() {
		defer close(events)
		var lastSeq uint64
		var haveLast bool
		backoff := 250 * time.Millisecond

		for {
			if ctx.Err() != nil {
				errc <- ctx.Err()
				return
			}
			conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
			if err != nil {
				select {
				case <-ctx.Done():
					errc <- ctx.Err()
					return
				case <-time.After(backoff):
				}
				if backoff < 8*time.Second {
					backoff *= 2
				}
				continue
			}
			backoff = 250 * time.Millisecond

			// 连上就一直读到出错;出错就跳出去重连
			for {
				if ctx.Err() != nil {
					conn.Close()
					errc <- ctx.Err()
					return
				}
				_, raw, err := conn.ReadMessage()
				if err != nil {
					conn.Close()
					break
				}
				var ev Event
				if json.Unmarshal(raw, &ev) != nil {
					continue // 解不动的单条跳过,不因为一条坏消息断整条流
				}
				if ev.Kind == "Lagged" {
					var d struct {
						Dropped uint64 `json:"dropped"`
					}
					_ = json.Unmarshal(raw, &d)
					select {
					case lost <- d.Dropped:
					default: // 通道满也不阻塞事件投递:丢帧信号可以合并,事件不能堵
					}
					continue
				}
				// seq 只保证非递减。倒退是**服务端 bug**,不是丢帧 —— 单独报出来,
				// 因为它意味着事件顺序不可信,比丢帧更严重。
				if haveLast && ev.Seq < lastSeq {
					select {
					case lost <- 0: // 0 = 顺序异常,不是丢了 N 条
					default:
					}
				}
				lastSeq, haveLast = ev.Seq, true
				select {
				case events <- ev:
				case <-ctx.Done():
					conn.Close()
					errc <- ctx.Err()
					return
				}
			}
		}
	}()

	return &Stream{Events: events, Lost: lost, Err: errc}, nil
}
