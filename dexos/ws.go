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
// 断线、坏帧、Lagged 或序号倒退均终止流并报错。当前服务端没有历史回放，
// 自动重连会掩盖成交缺口；调用方完成历史核对后才能重新 Subscribe。
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
		var lastSeq uint64
		var haveLast bool
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
			if haveLast && *wire.Seq < lastSeq {
				errc <- fmt.Errorf("%w: seq 倒退", ErrStreamGap)
				return
			}
			lastSeq, haveLast = *wire.Seq, true
			ev := Event{Seq: *wire.Seq, Kind: wire.Kind, Data: wire.Data}
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

// ErrStreamGap 表示流完整性已失去；快照不能补回缺失的逐笔成交。
var ErrStreamGap = errors.New("dexos: 事件流完整性未知，需要可靠历史补拉")
