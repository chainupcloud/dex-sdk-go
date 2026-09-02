package dexos

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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
// seq 是内核事件序号,**单调递增且无洞**。断线重连后若新的首条 seq 不等于
// 上次 +1,说明中间有事件没收到 —— SDK 把这个差值报出来(见 Stream.Gap),
// 由调用方决定是补拉快照还是接受。

// Event 一条事件。
type Event struct {
	Seq  uint64          `json:"seq"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// Stream 事件流订阅。
type Stream struct {
	Events <-chan Event
	// Gap 在检测到 seq 不连续时收到一条 (期望, 实际)。
	// 收到它意味着**丢了事件**,持仓/盘口的本地镜像已不可信,应当重新拉快照。
	Gap <-chan [2]uint64
	// Err 在流终止时收到原因;之后 Events 关闭。
	Err <-chan error
}

// Subscribe 连接事件流。ctx 取消即断开。
//
// 自动重连:断线后按 backoff 重试,直到 ctx 取消。重连成功后若 seq 出现跳跃,
// 会往 Gap 里投一条 —— 不静默吞掉,因为「少收了几条成交」对做市是致命的,
// 而它在业务层完全看不出来。
func (c *Client) Subscribe(ctx context.Context) (*Stream, error) {
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

	events := make(chan Event, 1024)
	gaps := make(chan [2]uint64, 16)
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
				if haveLast && ev.Seq != lastSeq+1 {
					select {
					case gaps <- [2]uint64{lastSeq + 1, ev.Seq}:
					default: // Gap 通道满了也不阻塞事件投递
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

	return &Stream{Events: events, Gap: gaps, Err: errc}, nil
}
