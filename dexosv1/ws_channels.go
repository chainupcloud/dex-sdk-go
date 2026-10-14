package dexosv1

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ── 公共频道 ──

// SubscribePrices 全市场实时价与 24h 统计(`prices`,与 `GET /v1/prices` 同一份体),块边界有变化才推。
func (st *Stream) SubscribePrices(fn func(*Prices)) (*Subscription, error) {
	sub := &Subscription{channel: "prices", route: "prices", snapshot: true,
		params: map[string]any{"channel": "prices"}}
	sub.apply = func(f *wsFrame) error {
		var w wirePrices
		if err := json.Unmarshal(f.Data, &w); err != nil {
			return fmt.Errorf("dexosv1: prices 帧解析失败: %w", err)
		}
		p, err := w.prices(st.cat)
		if err != nil {
			return err
		}
		fn(p)
		return nil
	}
	return st.add(sub)
}

// SubscribeBook 盘口(`book`):每帧都是前 depth 档的整份盘口(块边界合并,变了才推)。depth 0 = 服务端缺省 20,上限 100。
// 同一个市场只能有一个盘口订阅(帧按符号路由)。
func (st *Stream) SubscribeBook(symbol string, depth int, fn func(*Book)) (*Subscription, error) {
	m, err := st.cat.Market(symbol)
	if err != nil {
		return nil, err
	}
	if depth < 0 || depth > 100 {
		return nil, fmt.Errorf("dexosv1: 盘口档数 %d 不在 1..=100(0 = 缺省)", depth)
	}
	params := map[string]any{"channel": "book", "symbol": m.Symbol}
	if depth > 0 {
		params["depth"] = depth
	}
	sub := &Subscription{channel: "book", route: "book:" + strings.ToUpper(m.Symbol), snapshot: true, params: params}
	sub.apply = func(f *wsFrame) error {
		var w wireBook
		if err := json.Unmarshal(f.Data, &w); err != nil {
			return fmt.Errorf("dexosv1: book 帧解析失败: %w", err)
		}
		b, err := w.book(m)
		if err != nil {
			return err
		}
		fn(b)
		return nil
	}
	return st.add(sub)
}

// BBO 最优一档。Bid / Ask nil = 那一侧没有挂单。
type BBO struct {
	Symbol string
	Bid    *Level
	Ask    *Level
	TS     int64
}

// SubscribeBBO 最优一档(`bbo`),最优价或量变了才推。
func (st *Stream) SubscribeBBO(symbol string, fn func(BBO)) (*Subscription, error) {
	m, err := st.cat.Market(symbol)
	if err != nil {
		return nil, err
	}
	sub := &Subscription{channel: "bbo", route: "bbo:" + strings.ToUpper(m.Symbol), snapshot: true,
		params: map[string]any{"channel": "bbo", "symbol": m.Symbol}}
	sub.apply = func(f *wsFrame) error {
		var w struct {
			Symbol string     `json:"symbol"`
			Bid    *[2]string `json:"bid"`
			Ask    *[2]string `json:"ask"`
			TS     int64      `json:"ts"`
		}
		if err := json.Unmarshal(f.Data, &w); err != nil {
			return fmt.Errorf("dexosv1: bbo 帧解析失败: %w", err)
		}
		out := BBO{Symbol: w.Symbol, TS: w.TS}
		for _, side := range []struct {
			in  *[2]string
			out **Level
		}{{w.Bid, &out.Bid}, {w.Ask, &out.Ask}} {
			if side.in == nil {
				continue
			}
			l, err := levels(m.Scale, [][2]string{*side.in})
			if err != nil {
				return fmt.Errorf("dexosv1: %s bbo: %w", m.Symbol, err)
			}
			*side.out = &l[0]
		}
		fn(out)
		return nil
	}
	return st.add(sub)
}

// Trade 一笔公共成交。Side 是吃单方向;ID 与 `GET /v1/markets/{symbol}/trades` 的 id 相同。
type Trade struct {
	ID     string
	Symbol string
	Side   string
	Price  uint32
	Lots   uint64
	TS     int64
}

// SubscribeTrades 公共成交(`trades`),逐笔推。断线 / 丢帧时缺的帧不补,以 [*TradesGapError] 报出。
func (st *Stream) SubscribeTrades(symbol string, fn func(Trade)) (*Subscription, error) {
	m, err := st.cat.Market(symbol)
	if err != nil {
		return nil, err
	}
	sub := &Subscription{channel: "trades", route: "trades:" + strings.ToUpper(m.Symbol),
		params: map[string]any{"channel": "trades", "symbol": m.Symbol}}
	sub.apply = func(f *wsFrame) error {
		var w struct {
			ID     string `json:"id"`
			Symbol string `json:"symbol"`
			Side   string `json:"side"`
			Price  string `json:"price"`
			Size   string `json:"size"`
			TS     int64  `json:"ts"`
		}
		if err := json.Unmarshal(f.Data, &w); err != nil {
			return fmt.Errorf("dexosv1: trades 帧解析失败: %w", err)
		}
		t := Trade{ID: w.ID, Symbol: w.Symbol, Side: w.Side, TS: w.TS}
		if t.Price, err = m.Scale.Ticks(w.Price); err != nil {
			return fmt.Errorf("dexosv1: 成交 %s price: %w", w.ID, err)
		}
		if t.Lots, err = m.Scale.Lots(w.Size); err != nil {
			return fmt.Errorf("dexosv1: 成交 %s size: %w", w.ID, err)
		}
		fn(t)
		return nil
	}
	return st.add(sub)
}

// ── 私有频道:快照类 ──

// SubscribeAccount 账户汇总(`account`,与 `GET /v1/account` 同一份体、同一次取视图),块边界有变化才推。
func (st *Stream) SubscribeAccount(ref AccountRef, fn func(*Account)) (*Subscription, error) {
	sub, err := accountSub("account", ref, true)
	if err != nil {
		return nil, err
	}
	sub.apply = func(f *wsFrame) error {
		a, err := decodeAccount(f.Data, "WS account 帧")
		if err != nil {
			return err
		}
		fn(a)
		return nil
	}
	return st.add(sub)
}

// SubscribePositions 持仓(`positions`,与 `GET /v1/account/positions` 同一份体),块边界有变化才推。每帧是全部持仓。
func (st *Stream) SubscribePositions(ref AccountRef, fn func([]Position)) (*Subscription, error) {
	sub, err := accountSub("positions", ref, true)
	if err != nil {
		return nil, err
	}
	sub.apply = func(f *wsFrame) error {
		var w struct {
			Positions []wirePosition `json:"positions"`
		}
		if err := json.Unmarshal(f.Data, &w); err != nil {
			return fmt.Errorf("dexosv1: positions 帧解析失败: %w", err)
		}
		ps, err := positionsFrom(st.cat, w.Positions)
		if err != nil {
			return err
		}
		fn(ps)
		return nil
	}
	return st.add(sub)
}

// OrderEvent 一个订单事件(`orders` 增量帧)。
type OrderEvent struct {
	// Kind "accepted" | "fill" | "canceled" | "expired"
	Kind          string
	OrderID       uint64
	ClientOrderID *string
	Symbol        string
	// Status 本条命令结束时这张单的状态:"open" | "filled" | "canceled" | "expired"(吃单成交帧与它的
	// accepted 帧相同;协议代下的单 OrderID 为 0、"filled")。
	Status string
	// RemainingLots 本条命令结束时挂在簿上的剩余手数(不挂为 0)。
	RemainingLots uint64
	// 以下只有 accepted 帧有
	Side       string
	Price      uint32
	Lots       uint64
	FilledLots uint64
	// 以下只有 fill 帧有
	FillID         string
	LastFilledLots *uint64
	TS             int64
}

// OrdersUpdate `orders` 订阅的一次更新:快照(Event == nil)或一个事件;Open 是应用之后的全部在挂单(按 OrderID 升序)。
type OrdersUpdate struct {
	Snapshot bool
	Event    *OrderEvent
	Open     []Order
	Seq      uint64
}

type wireOrderEvent struct {
	OrderID       string  `json:"orderId"`
	ClientOrderID *string `json:"clientOrderId"`
	Symbol        string  `json:"symbol"`
	Event         string  `json:"event"`
	Status        *string `json:"status"`
	Side          *string `json:"side"`
	Price         *string `json:"price"`
	Size          *string `json:"size"`
	Filled        *string `json:"filled"`
	Remaining     *string `json:"remaining"`
	LastFilled    *string `json:"lastFilled"`
	FillID        *string `json:"fillId"`
	TS            int64   `json:"ts"`
}

// SubscribeOrders 在挂单(`orders`)。订阅即推全市场挂单快照,之后每个下单 / 成交 / 撤单 / 过期一帧;
// 本包维护在挂单集合:快照整体替换,status 为 open 即在挂、否则移除。帧与本地集合对不上、帧缺 status /
// remaining 时不猜,退订重订换一帧新快照(期间 Syncing)。服务端自己算不出某帧时会直接重发快照。
// 待触发的条件单也在集合里(Order.Trigger 非 nil):它被下单、触发、拒绝、自动撤时服务端重发快照,被撤时推 canceled 帧。
func (st *Stream) SubscribeOrders(ref AccountRef, fn func(OrdersUpdate)) (*Subscription, error) {
	sub, err := accountSub("orders", ref, true)
	if err != nil {
		return nil, err
	}
	open := map[uint64]Order{}
	emit := func(snapshot bool, ev *OrderEvent, seq uint64) {
		list := make([]Order, 0, len(open))
		for _, o := range open {
			list = append(list, o)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].OrderID < list[j].OrderID })
		fn(OrdersUpdate{Snapshot: snapshot, Event: ev, Open: list, Seq: seq})
	}
	sub.apply = func(f *wsFrame) error {
		var seq uint64
		if f.Seq != nil {
			seq = *f.Seq
		}
		if f.IsSnapshot {
			var w struct {
				Orders []wireOrder `json:"orders"`
			}
			if err := json.Unmarshal(f.Data, &w); err != nil {
				return fmt.Errorf("dexosv1: orders 快照解析失败: %w", err)
			}
			next := make(map[uint64]Order, len(w.Orders))
			for _, wo := range w.Orders {
				o, err := wo.order(st.cat)
				if err != nil {
					return err
				}
				next[o.OrderID] = o
			}
			open = next
			emit(true, nil, seq)
			return nil
		}
		if sub.State() != StateLive {
			// 快照之前的增量:快照会包含它
			return nil
		}
		var w wireOrderEvent
		if err := json.Unmarshal(f.Data, &w); err != nil {
			return fmt.Errorf("dexosv1: orders 帧解析失败: %w", err)
		}
		ev, err := w.event(st.cat)
		if err != nil {
			return err
		}
		if err := applyOrderEvent(open, ev); err != nil {
			return &resyncError{err: err}
		}
		emit(false, ev, seq)
		return nil
	}
	return st.add(sub)
}

func (w wireOrderEvent) event(cat *Catalog) (*OrderEvent, error) {
	m, err := cat.Market(w.Symbol)
	if err != nil {
		return nil, err
	}
	// status / remaining 每帧都有(v1.1 §4.9-3);缺了就是服务端没算出来,不猜
	if w.Status == nil || w.Remaining == nil {
		return nil, fmt.Errorf("dexosv1: orders %s 帧 %s 缺 status / remaining", w.Event, w.OrderID)
	}
	ev := &OrderEvent{Kind: w.Event, ClientOrderID: w.ClientOrderID, Symbol: w.Symbol, Status: *w.Status, TS: w.TS}
	if ev.OrderID, err = strconv.ParseUint(w.OrderID, 10, 64); err != nil {
		return nil, fmt.Errorf("dexosv1: orders 帧 orderId %q: %w", w.OrderID, err)
	}
	lots := func(name string, in *string) (*uint64, error) {
		if in == nil {
			return nil, nil
		}
		v, err := m.Scale.Lots(*in)
		if err != nil {
			return nil, fmt.Errorf("dexosv1: orders 帧 %s %s: %w", w.OrderID, name, err)
		}
		return &v, nil
	}
	if ev.RemainingLots, err = m.Scale.Lots(*w.Remaining); err != nil {
		return nil, fmt.Errorf("dexosv1: orders 帧 %s remaining: %w", w.OrderID, err)
	}
	switch w.Event {
	case "accepted":
		if w.Side == nil || w.Price == nil || w.Size == nil || w.Filled == nil {
			return nil, fmt.Errorf("dexosv1: orders accepted 帧 %s 缺字段", w.OrderID)
		}
		ev.Side = *w.Side
		if ev.Price, err = m.Scale.Ticks(*w.Price); err != nil {
			return nil, fmt.Errorf("dexosv1: orders 帧 %s price: %w", w.OrderID, err)
		}
		for _, f := range []struct {
			name string
			in   *string
			out  *uint64
		}{{"size", w.Size, &ev.Lots}, {"filled", w.Filled, &ev.FilledLots}} {
			v, err := lots(f.name, f.in)
			if err != nil {
				return nil, err
			}
			*f.out = *v
		}
	case "fill":
		if w.FillID == nil {
			return nil, fmt.Errorf("dexosv1: orders fill 帧 %s 缺 fillId", w.OrderID)
		}
		ev.FillID = *w.FillID
		if ev.LastFilledLots, err = lots("lastFilled", w.LastFilled); err != nil {
			return nil, err
		}
	case "canceled", "expired":
	default:
		return nil, fmt.Errorf("dexosv1: orders 帧 %s 的 event %q 不认识", w.OrderID, w.Event)
	}
	return ev, nil
}

// applyOrderEvent 把一个事件应用到在挂单集合。对不上(说在挂却不在集合里)就报错,由调用方重订换快照。
func applyOrderEvent(open map[uint64]Order, ev *OrderEvent) error {
	if ev.Status != "open" {
		delete(open, ev.OrderID)
		return nil
	}
	if ev.Kind == "accepted" {
		open[ev.OrderID] = Order{OrderID: ev.OrderID, ClientOrderID: ev.ClientOrderID, Symbol: ev.Symbol, Side: ev.Side,
			Price: ev.Price, Lots: ev.Lots, FilledLots: ev.FilledLots, RemainingLots: ev.RemainingLots, Status: "open"}
		return nil
	}
	o, ok := open[ev.OrderID]
	if !ok {
		return fmt.Errorf("dexosv1: orders 帧说订单 %d 在挂,本地集合里没有它", ev.OrderID)
	}
	o.RemainingLots = ev.RemainingLots
	o.FilledLots = o.Lots - o.RemainingLots
	if ev.ClientOrderID != nil {
		o.ClientOrderID = ev.ClientOrderID
	}
	open[ev.OrderID] = o
	return nil
}
