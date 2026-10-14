package dexosv1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testEpoch = "k1-1"

// ── 假 WS 服务端 ──

// wsReq 客户端发来的一帧。
type wsReq struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
	ID     int64          `json:"id"`
}

type fakeWS struct{ conns chan *fakeConn }

// fakeConn 服务端这一侧的一条连接:客户端的帧进 reqs,连接断了 reqs 关闭。
type fakeConn struct {
	ws   *websocket.Conn
	reqs chan wsReq
	wmu  sync.Mutex
}

func newFakeWS(t *testing.T, f *fakeV1) *fakeWS {
	fw := &fakeWS{conns: make(chan *fakeConn, 16)}
	up := websocket.Upgrader{}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes["/v1/ws"] = func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("WS 升级失败: %v", err)
			return
		}
		c := &fakeConn{ws: ws, reqs: make(chan wsReq, 1024)}
		fw.conns <- c
		defer close(c.reqs)
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var q wsReq
			if err := json.Unmarshal(msg, &q); err != nil {
				t.Errorf("客户端帧不是 JSON: %s", msg)
				return
			}
			c.reqs <- q
		}
	}
	return fw
}

func (fw *fakeWS) accept(t *testing.T) *fakeConn {
	t.Helper()
	select {
	case c := <-fw.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("等不到 WS 连接")
		return nil
	}
}

// next 客户端的下一帧(跳过心跳)。
func (c *fakeConn) next(t *testing.T) wsReq {
	t.Helper()
	for {
		select {
		case q, ok := <-c.reqs:
			if !ok {
				t.Fatal("WS 连接已断开")
			}
			if q.Method == "ping" {
				continue
			}
			return q
		case <-time.After(5 * time.Second):
			t.Fatal("等不到客户端的帧")
		}
	}
}

// silent 这段时间里客户端不该再发(心跳除外)。
func (c *fakeConn) silent(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case q, ok := <-c.reqs:
			if ok && q.Method != "ping" {
				t.Fatalf("客户端不该再发帧: %+v", q)
			}
		case <-deadline:
			return
		}
	}
}

// subscribes 读 n 个 subscribe 请求,按频道名索引。
func (c *fakeConn) subscribes(t *testing.T, n int) map[string]wsReq {
	t.Helper()
	out := map[string]wsReq{}
	for len(out) < n {
		q := c.next(t)
		if q.Method != "subscribe" {
			t.Fatalf("要 subscribe,来的是 %+v", q)
		}
		out[q.Params["channel"].(string)] = q
	}
	return out
}

func (c *fakeConn) send(t *testing.T, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.ws.WriteMessage(websocket.TextMessage, raw); err != nil {
		t.Fatal(err)
	}
}

func (c *fakeConn) ack(t *testing.T, q wsReq, epoch string) {
	t.Helper()
	c.send(t, map[string]any{"channel": "subscribed", "data": q.Params, "epoch": epoch, "id": q.ID})
}

// push 一帧数据。epoch 空 = 公共频道(不带纪元)。
func (c *fakeConn) push(t *testing.T, channel string, data any, seq uint64, epoch string, snapshot bool) {
	t.Helper()
	f := map[string]any{"channel": channel, "data": data, "seq": seq, "isSnapshot": snapshot}
	if epoch != "" {
		f["epoch"] = epoch
	}
	c.send(t, f)
}

// gone 等客户端断开这条连接。
func (c *fakeConn) gone(t *testing.T) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-c.reqs:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("客户端没有断开连接")
		}
	}
}

// jsonObj 把 JSON 解成对象并改几个字段(数字保持原样)。
func jsonObj(t *testing.T, raw []byte, set map[string]any) map[string]any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		t.Fatal(err)
	}
	for k, v := range set {
		m[k] = v
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ── 被测客户端 ──

type wsHarness struct {
	t   *testing.T
	f   *fakeV1
	c   *Client
	cat *Catalog
	fw  *fakeWS
	st  *Stream
	ref AccountRef

	mu       sync.Mutex
	states   []StateEvent
	seen     map[*Subscription]int
	connErrs []error
}

func newWSHarness(t *testing.T, tune func(*StreamOptions)) *wsHarness {
	f, c := newFake(t)
	h := &wsHarness{t: t, f: f, c: c, cat: catalog(t, f, c), seen: map[*Subscription]int{}}
	h.fw = newFakeWS(t, f)
	h.ref = AccountRef{Address: mustAddr(t, testAddr)}
	opt := StreamOptions{
		PingInterval: time.Hour,
		ReadTimeout:  time.Hour,
		ReconnectMin: 10 * time.Millisecond,
		ReconnectMax: 40 * time.Millisecond,
		SettleDelay:  time.Hour,
		CatchUpWait:  3 * time.Second,
		OnState: func(e StateEvent) {
			h.mu.Lock()
			h.states = append(h.states, e)
			h.mu.Unlock()
		},
		OnConnError: func(err error) {
			h.mu.Lock()
			h.connErrs = append(h.connErrs, err)
			h.mu.Unlock()
		},
	}
	if tune != nil {
		tune(&opt)
	}
	st, err := NewStream(c, h.cat, opt)
	if err != nil {
		t.Fatal(err)
	}
	h.st = st
	return h
}

func (h *wsHarness) run() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.st.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				h.t.Errorf("Run 返回 %v", err)
			}
		case <-time.After(5 * time.Second):
			h.t.Error("Run 没有退出")
		}
	})
}

// waitState 等这个订阅在上次等到的事件之后、状态为 want 的事件;途中别的状态跳过。
func (h *wsHarness) waitState(sub *Subscription, want State) StateEvent {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		for i := h.seen[sub]; i < len(h.states); i++ {
			if e := h.states[i]; e.Sub == sub && e.State == want {
				h.seen[sub] = i + 1
				h.mu.Unlock()
				return e
			}
		}
		h.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("%s 订阅等不到 %s", sub.Channel(), want)
	return StateEvent{}
}

// trail 这个订阅报出过的全部状态。
func (h *wsHarness) trail(sub *Subscription) []State {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []State
	for _, e := range h.states {
		if e.Sub == sub {
			out = append(out, e.State)
		}
	}
	return out
}

func (h *wsHarness) connErrors() []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]error(nil), h.connErrs...)
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("等不到回调")
		var zero T
		return zero
	}
}

func quiet[T any](t *testing.T, ch <-chan T, d time.Duration) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("不该有回调: %+v", v)
	case <-time.After(d):
	}
}

// ── 假执行历史(REST 补拉)──

type fakeHistory struct {
	mu       sync.Mutex
	epoch    string
	asOf     uint64 // 账户视图位号
	complete uint64 // 执行历史已完整到的日志位号;上界超过它答 400 not yet complete
	rows     map[string][]map[string]any
	queries  map[string][]url.Values
}

// history 装上 /v1/account 与成交 / 流水两个补拉接口。
func (h *wsHarness) history(asOf, complete uint64) *fakeHistory {
	fh := &fakeHistory{epoch: testEpoch, asOf: asOf, complete: complete,
		rows: map[string][]map[string]any{}, queries: map[string][]url.Values{}}
	account := jsonObj(h.t, fixture(h.t, "account.json"), nil)
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	h.f.routes["/v1/account"] = func(w http.ResponseWriter, _ *http.Request) {
		fh.mu.Lock()
		body := private(account)
		body["epoch"], body["asOfSeq"] = fh.epoch, fh.asOf
		fh.mu.Unlock()
		writeJSON(w, 200, body)
	}
	h.f.routes["/v1/account/fills"] = fh.handler(h.t, "/v1/account/fills", "fills")
	h.f.routes["/v1/account/ledger"] = fh.handler(h.t, "/v1/account/ledger", "entries")
	return fh
}

func (fh *fakeHistory) set(fn func(*fakeHistory)) {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	fn(fh)
}

func (fh *fakeHistory) add(path string, rows ...map[string]any) {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	all := append(fh.rows[path], rows...)
	sort.Slice(all, func(i, j int) bool {
		a, _ := parseCursor(all[i]["id"].(string))
		b, _ := parseCursor(all[j]["id"].(string))
		return a.less(b)
	})
	fh.rows[path] = all
}

func (fh *fakeHistory) asked(path string) []url.Values {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	return append([]url.Values(nil), fh.queries[path]...)
}

func (fh *fakeHistory) handler(t *testing.T, path, field string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		fh.mu.Lock()
		defer fh.mu.Unlock()
		fh.queries[path] = append(fh.queries[path], q)
		if q.Get("epoch") != fh.epoch {
			writeJSON(w, 409, map[string]any{"error": map[string]any{"code": "GatewayConflict",
				"message": "history_epoch_mismatch", "num": 10409, "details": map[string]any{"epoch": fh.epoch}}})
			return
		}
		after, err1 := parseCursor(q.Get("after"))
		upper, err2 := parseCursor(q.Get("upper"))
		if err1 != nil || err2 != nil {
			t.Errorf("补拉要带 after 与 upper: %v", q)
			writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "GatewayBadRequest", "message": "bad cursor", "num": 10400}})
			return
		}
		if upper.seq > fh.complete {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "GatewayBadRequest",
				"message": fmt.Sprintf("upper %d not yet complete (complete through %d)", upper.seq, fh.complete), "num": 10400}})
			return
		}
		out := []map[string]any{}
		for _, row := range fh.rows[path] {
			p, _ := parseCursor(row["id"].(string))
			if after.less(p) && !upper.less(p) {
				out = append(out, row)
			}
		}
		writeJSON(w, 200, map[string]any{"address": testAddr, "subaccount": 0, field: out, "epoch": fh.epoch,
			"upper": q.Get("upper"), "hasMore": false, "next": nil, "finality": "raft_committed"})
	}
}

func fillRow(id string) map[string]any {
	p, _ := parseCursor(id)
	return map[string]any{"id": id, "seq": p.seq, "orderId": "7", "symbol": "ETH-USD", "side": "buy", "role": "maker",
		"price": "2492.6", "size": "0.1", "fee": "0", "feeToken": "USDC", "ts": 1791600487016}
}

func ledgerRow(id string) map[string]any {
	p, _ := parseCursor(id)
	return map[string]any{"id": id, "seq": p.seq, "sub": p.sub, "idx": p.idx, "kind": "trade", "token": "USDC",
		"amount": "-1.5", "symbol": "ETH-USD", "order": 7}
}

// private 私有帧的 data:REST 的行加上地址与子账户序号。
func private(row map[string]any) map[string]any {
	out := map[string]any{"address": testAddr, "subaccount": 0}
	for k, v := range row {
		out[k] = v
	}
	return out
}

func ids[T any](t *testing.T, ch <-chan T, n int, id func(T) string) []string {
	t.Helper()
	out := make([]string, 0, n)
	for range n {
		out = append(out, id(recv(t, ch)))
	}
	return out
}

func fillID(f Fill) string { return f.ID }

// ── 用例 ──

func TestNewStreamURLFromBaseURL(t *testing.T) {
	cat := &Catalog{}
	for base, want := range map[string]string{
		"http://127.0.0.1:17807":    "ws://127.0.0.1:17807/v1/ws",
		"https://dex.example/api/":  "wss://dex.example/api/v1/ws",
		"https://dex.example:8443":  "wss://dex.example:8443/v1/ws",
		"http://localhost:8080/sub": "ws://localhost:8080/sub/v1/ws",
	} {
		st, err := NewStream(NewClient(base), cat, StreamOptions{})
		if err != nil || st.url != want {
			t.Errorf("%s → %q, %v;要 %s", base, st.url, err, want)
		}
	}
	if _, err := NewStream(NewClient("ftp://x"), cat, StreamOptions{}); err == nil {
		t.Error("推不出 WS 地址时要报错")
	}
}

// 快照类频道:订阅参数、确认后等快照、快照到了才 Live、之后的整帧照常回调。
func TestStreamSnapshotChannels(t *testing.T) {
	h := newWSHarness(t, nil)
	books := make(chan *Book, 8)
	bbos := make(chan BBO, 8)
	prices := make(chan *Prices, 8)
	accounts := make(chan *Account, 8)
	positions := make(chan []Position, 8)
	bs, err := h.st.SubscribeBook("eth-usd", 5, func(b *Book) { books <- b })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.SubscribeBook("ETH-USD", 0, func(*Book) {}); err == nil {
		t.Fatal("同一市场的第二个盘口订阅要拒绝(帧里分不出是哪个)")
	}
	if _, err := h.st.SubscribeBook("BTC-USD", 101, func(*Book) {}); err == nil {
		t.Fatal("档数超过 100 要拒绝")
	}
	if _, err := h.st.SubscribeBBO("NOPE-USD", func(BBO) {}); err == nil {
		t.Fatal("不认识的市场要拒绝")
	}
	bb, _ := h.st.SubscribeBBO("ETH-USD", func(b BBO) { bbos <- b })
	ps, _ := h.st.SubscribePrices(func(p *Prices) { prices <- p })
	as, _ := h.st.SubscribeAccount(h.ref, func(a *Account) { accounts <- a })
	pos, _ := h.st.SubscribePositions(h.ref, func(p []Position) { positions <- p })
	h.run()

	conn := h.fw.accept(t)
	reqs := conn.subscribes(t, 5)
	if want := map[string]any{"channel": "book", "symbol": "ETH-USD", "depth": float64(5)}; !reflect.DeepEqual(reqs["book"].Params, want) {
		t.Fatalf("book 订阅参数 %v", reqs["book"].Params)
	}
	if want := map[string]any{"channel": "account", "address": testAddr, "subaccount": float64(0)}; !reflect.DeepEqual(reqs["account"].Params, want) {
		t.Fatalf("account 订阅参数 %v", reqs["account"].Params)
	}
	for _, q := range reqs {
		conn.ack(t, q, testEpoch)
	}
	// 确认之后、快照之前不算就绪
	conn.silent(t, 30*time.Millisecond)
	if bs.State() != StateSyncing || as.State() != StateSyncing {
		t.Fatalf("快照之前就就绪了: book %s account %s", bs.State(), as.State())
	}

	conn.push(t, "book", jsonObj(t, fixture(t, "book.json"), nil), 231461888, "", true)
	conn.push(t, "bbo", map[string]any{"symbol": "ETH-USD", "bid": []string{"2492.6", "3"}, "ask": nil, "ts": 1}, 231461888, "", true)
	conn.push(t, "prices", jsonObj(t, fixture(t, "prices.json"), nil), 231461888, "", true)
	conn.push(t, "account", jsonObj(t, fixture(t, "account.json"), nil), 231461888, testEpoch, true)
	conn.push(t, "positions", jsonObj(t, fixture(t, "positions.json"), nil), 231461888, testEpoch, true)
	for _, s := range []*Subscription{bs, bb, ps, as, pos} {
		h.waitState(s, StateLive)
	}
	if b := recv(t, books); b.Symbol != "ETH-USD" || b.Bids[0] != (Level{Price: 24926, Lots: 300}) || b.Asks[1] != (Level{Price: 24936, Lots: 2}) {
		t.Fatalf("盘口换算不对: %+v", b)
	}
	if b := recv(t, bbos); b.Bid == nil || *b.Bid != (Level{Price: 24926, Lots: 300}) || b.Ask != nil {
		t.Fatalf("bbo 换算不对: %+v", b)
	}
	if p := recv(t, prices); len(p.Prices) != 2 || p.Prices[1].Last != 24930 {
		t.Fatalf("prices 换算不对: %+v", p)
	}
	if a := recv(t, accounts); a.Epoch != testEpoch || a.AsOfSeq != 231461888 {
		t.Fatalf("account 不对: %+v", a)
	}
	if p := recv(t, positions); len(p) != 1 || p[0].Symbol != "BTC-USD" || p[0].Lots != 184 {
		t.Fatalf("positions 不对: %+v", p)
	}

	// 块边界的整帧(非快照标记)照常回调
	conn.push(t, "book", map[string]any{"symbol": "ETH-USD", "bids": [][]string{{"2492.7", "1"}}, "asks": [][]string{}, "seq": 231461890, "ts": 2},
		231461890, "", false)
	if b := recv(t, books); b.Bids[0] != (Level{Price: 24927, Lots: 100}) || len(b.Asks) != 0 || b.Seq != 231461890 {
		t.Fatalf("增量整帧不对: %+v", b)
	}
	// 价格不在刻度上:只这个订阅置 Syncing 报出原因,稍后退订重订;别的订阅不受影响
	conn.push(t, "book", map[string]any{"symbol": "ETH-USD", "bids": [][]string{{"2492.75", "1"}}, "asks": [][]string{}, "seq": 231461891, "ts": 3},
		231461891, "", false)
	if e := h.waitState(bs, StateSyncing); e.Err == nil || errors.Is(e.Err, ErrConnLost) {
		t.Fatalf("坏帧原因 %v", e.Err)
	}
	if q := conn.next(t); q.Method != "unsubscribe" || q.Params["channel"] != "book" {
		t.Fatalf("要先退订 %+v", q)
	}
	if q := conn.next(t); q.Method != "subscribe" || q.Params["channel"] != "book" {
		t.Fatalf("要重订 %+v", q)
	}
	if as.State() != StateLive || len(h.connErrors()) != 0 {
		t.Fatalf("别的订阅不受影响: account %s, 连接错误 %v", as.State(), h.connErrors())
	}
}

func orderFrame(fields map[string]any) map[string]any {
	out := map[string]any{"address": testAddr, "subaccount": 0, "symbol": "ETH-USD", "clientOrderId": nil, "ts": 1}
	for k, v := range fields {
		out[k] = v
	}
	return out
}

// orders 状态机:快照整体替换;status 为 open 即在挂、否则移除;null 不改成员;对不上就重订换快照。
func TestStreamOrdersStateMachine(t *testing.T) {
	h := newWSHarness(t, nil)
	ups := make(chan OrdersUpdate, 16)
	sub, _ := h.st.SubscribeOrders(h.ref, func(u OrdersUpdate) { ups <- u })
	h.run()
	conn := h.fw.accept(t)
	q := conn.subscribes(t, 1)["orders"]
	conn.ack(t, q, testEpoch)

	// 快照之前的增量不应用(快照会包含它)
	conn.push(t, "orders", orderFrame(map[string]any{"orderId": "5", "event": "canceled", "status": "canceled", "remaining": "0"}), 10, testEpoch, false)
	conn.push(t, "orders", jsonObj(t, fixture(t, "orders.json"), nil), 11, testEpoch, true)
	u := recv(t, ups)
	if !u.Snapshot || u.Event != nil || len(u.Open) != 2 || u.Open[0].OrderID != 2240412712968 || *u.Open[0].ClientOrderID != "mm-1" ||
		u.Open[0].RemainingLots != 40 || u.Seq != 11 {
		t.Fatalf("快照不对: %+v", u)
	}
	h.waitState(sub, StateLive)

	open := func(u OrdersUpdate) map[uint64]Order {
		m := map[uint64]Order{}
		for _, o := range u.Open {
			m[o.OrderID] = o
		}
		return m
	}
	// 挂上的单进集合
	conn.push(t, "orders", orderFrame(map[string]any{"orderId": "2240412712970", "clientOrderId": "mm-2", "event": "accepted", "status": "open",
		"side": "sell", "price": "2495", "size": "1", "filled": "0.25", "remaining": "0.75"}), 12, testEpoch, false)
	u = recv(t, ups)
	if o, ok := open(u)[2240412712970]; !ok || *o.ClientOrderID != "mm-2" || o.Price != 24950 || o.Lots != 100 || o.FilledLots != 25 ||
		o.RemainingLots != 75 || o.Side != "sell" || u.Event.Kind != "accepted" {
		t.Fatalf("accepted 不对: %+v", u)
	}
	// maker 成交:更新剩余
	conn.push(t, "orders", orderFrame(map[string]any{"orderId": "2240412712968", "clientOrderId": "mm-1", "event": "fill", "fillId": "13-0-1",
		"lastFilled": "0.1", "remaining": "0.3", "status": "open"}), 13, testEpoch, false)
	u = recv(t, ups)
	if o := open(u)[2240412712968]; o.RemainingLots != 30 || o.FilledLots != 20 || u.Event.FillID != "13-0-1" || *u.Event.LastFilledLots != 10 {
		t.Fatalf("maker 成交不对: %+v / %+v", o, u.Event)
	}
	// 吃单成交帧先于它的下单结论(status null):集合不变
	conn.push(t, "orders", orderFrame(map[string]any{"orderId": "2240412712971", "event": "fill", "fillId": "14-0-0",
		"lastFilled": "0.1", "remaining": nil, "status": nil}), 14, testEpoch, false)
	if u = recv(t, ups); len(u.Open) != 3 || u.Event.Status != nil || u.Event.RemainingLots != nil {
		t.Fatalf("status null 不该改集合: %+v", u)
	}
	// 没挂上的下单结论(吃满):不进集合
	conn.push(t, "orders", orderFrame(map[string]any{"orderId": "2240412712971", "event": "accepted", "status": "filled",
		"side": "buy", "price": "2493.4", "size": "0.1", "filled": "0.1", "remaining": "0"}), 14, testEpoch, false)
	if u = recv(t, ups); len(u.Open) != 3 {
		t.Fatalf("吃满的单不该进集合: %+v", u)
	}
	// 撤单:移除
	conn.push(t, "orders", orderFrame(map[string]any{"orderId": "2240412712969", "symbol": "BTC-USD", "event": "canceled", "status": "canceled",
		"remaining": "0.002"}), 15, testEpoch, false)
	if u = recv(t, ups); len(u.Open) != 2 || open(u)[2240412712969].OrderID != 0 {
		t.Fatalf("撤单没移除: %+v", u)
	}
	// 对不上(说在挂、本地没有):不猜,退订重订换快照
	conn.push(t, "orders", orderFrame(map[string]any{"orderId": "999", "event": "fill", "fillId": "16-0-0", "lastFilled": "0.1",
		"remaining": "0.1", "status": "open"}), 16, testEpoch, false)
	if e := h.waitState(sub, StateSyncing); e.Err == nil {
		t.Fatal("进 Syncing 要带原因")
	}
	if q := conn.next(t); q.Method != "unsubscribe" || q.Params["channel"] != "orders" {
		t.Fatalf("要先退订: %+v", q)
	}
	q = conn.next(t)
	if q.Method != "subscribe" || q.Params["channel"] != "orders" {
		t.Fatalf("要重订: %+v", q)
	}
	conn.ack(t, q, testEpoch)
	// 新快照之前的增量不应用
	conn.push(t, "orders", orderFrame(map[string]any{"orderId": "2240412712968", "event": "canceled", "status": "canceled", "remaining": "0.3"}),
		17, testEpoch, false)
	conn.push(t, "orders", map[string]any{"address": testAddr, "subaccount": 0, "orders": []any{}}, 18, testEpoch, true)
	if u = recv(t, ups); !u.Snapshot || len(u.Open) != 0 {
		t.Fatalf("重订之后第一帧要是新快照: %+v", u)
	}
	h.waitState(sub, StateLive)
	quiet(t, ups, 30*time.Millisecond)
}

// fills 补拉:订阅后先补到账户视图;订阅前后没推的那段在第一帧到达时补齐(历史没写到就等);之后的帧直接交付;不缺不重。
func TestStreamFillsBackfillGapAndDedup(t *testing.T) {
	h := newWSHarness(t, nil)
	fh := h.history(102, 102)
	fh.add("/v1/account/fills", fillRow("100-0-0"), fillRow("101-0-0"), fillRow("102-0-1"), fillRow("103-0-0"),
		fillRow("104-0-0"), fillRow("104-0-2"))
	fills := make(chan Fill, 32)
	if _, err := h.st.SubscribeFills(h.ref, HistoryCursor{}, func(Fill) {}); err == nil {
		t.Fatal("没给起点要拒绝")
	}
	sub, err := h.st.SubscribeFills(h.ref, HistoryCursor{After: "100-0-0", Epoch: testEpoch}, func(f Fill) { fills <- f })
	if err != nil {
		t.Fatal(err)
	}
	h.run()
	conn := h.fw.accept(t)
	q := conn.subscribes(t, 1)["fills"]
	if q.Params["address"] != testAddr || q.Params["subaccount"] != float64(0) {
		t.Fatalf("fills 订阅参数 %v", q.Params)
	}
	conn.ack(t, q, testEpoch)

	// 补到视图(102)
	if got := ids(t, fills, 2, fillID); !reflect.DeepEqual(got, []string{"101-0-0", "102-0-1"}) {
		t.Fatalf("补到视图: %v", got)
	}
	h.waitState(sub, StateLive)

	// 103 发生在订阅前后、没有推;第一帧是 104-0-0,执行历史还没写到 104:先等,不交付
	conn.push(t, "fills", private(fillRow("104-0-0")), 104, testEpoch, false)
	quiet(t, fills, 150*time.Millisecond)
	fh.set(func(fh *fakeHistory) { fh.complete = 104 })
	if got := ids(t, fills, 2, fillID); !reflect.DeepEqual(got, []string{"103-0-0", "104-0-0"}) {
		t.Fatalf("补齐缺口: %v", got)
	}
	// 接上之后:重复帧丢掉,新帧直接交付
	conn.push(t, "fills", private(fillRow("104-0-0")), 104, testEpoch, false)
	conn.push(t, "fills", private(fillRow("104-0-2")), 104, testEpoch, false)
	conn.push(t, "fills", private(fillRow("105-0-0")), 105, testEpoch, false)
	if got := ids(t, fills, 2, fillID); !reflect.DeepEqual(got, []string{"104-0-2", "105-0-0"}) {
		t.Fatalf("接上之后: %v", got)
	}
	quiet(t, fills, 30*time.Millisecond)
	if c := sub.Cursor(); c != (HistoryCursor{After: "105-0-0", Epoch: testEpoch}) {
		t.Fatalf("游标 %+v", c)
	}
	// 等历史的那段时间只是延迟,不算失效
	if got := h.trail(sub); !reflect.DeepEqual(got, []State{StateLive}) {
		t.Fatalf("状态轨迹 %v", got)
	}
	asked := fh.asked("/v1/account/fills")
	if first := asked[0]; first.Get("after") != "100-0-0" || first.Get("upper") != "102-65535-4294967295" || first.Get("epoch") != testEpoch {
		t.Fatalf("第一次补拉 %v", first)
	}
	if last := asked[len(asked)-1]; last.Get("after") != "102-0-1" || last.Get("upper") != "104-0-0" {
		t.Fatalf("补缺口 %v", last)
	}
}

// 流水订阅建立后迟迟没有帧:SettleDelay 后再按视图补一次,不必等下一帧。
func TestStreamFillsSettlePass(t *testing.T) {
	h := newWSHarness(t, func(o *StreamOptions) { o.SettleDelay = 300 * time.Millisecond })
	fh := h.history(500, 501)
	fh.add("/v1/account/fills", fillRow("501-0-0"))
	fills := make(chan Fill, 8)
	sub, _ := h.st.SubscribeFills(h.ref, HistoryCursor{After: "500-65535-4294967295", Epoch: testEpoch}, func(f Fill) { fills <- f })
	h.run()
	conn := h.fw.accept(t)
	conn.ack(t, conn.subscribes(t, 1)["fills"], testEpoch)
	h.waitState(sub, StateLive)
	// 501 在订阅那一刻前后发生,当时还没进视图
	fh.set(func(fh *fakeHistory) { fh.asOf = 501 })
	if f := recv(t, fills); f.ID != "501-0-0" || f.Price != 24926 || f.Lots != 10 || *f.OrderID != 7 {
		t.Fatalf("settle 补拉 %+v", f)
	}
}

// ledger:补拉与 WS 帧拼接;WS 帧的 seq / sub / idx 从 id 解出。
func TestStreamLedger(t *testing.T) {
	h := newWSHarness(t, nil)
	fh := h.history(601, 602)
	fh.add("/v1/account/ledger", ledgerRow("600-0-0"), ledgerRow("601-0-1"), ledgerRow("602-0-3"))
	entries := make(chan LedgerEntry, 8)
	sub, _ := h.st.SubscribeLedger(h.ref, HistoryCursor{After: "600-0-0", Epoch: testEpoch}, func(e LedgerEntry) { entries <- e })
	h.run()
	conn := h.fw.accept(t)
	conn.ack(t, conn.subscribes(t, 1)["ledger"], testEpoch)
	if e := recv(t, entries); e.ID != "601-0-1" {
		t.Fatalf("补拉 %+v", e)
	}
	h.waitState(sub, StateLive)
	frame := func(id string) map[string]any {
		return map[string]any{"id": id, "address": testAddr, "subaccount": 0, "kind": "trade", "token": "USDC", "amount": "-1.5",
			"symbol": "ETH-USD", "orderId": "7", "ts": 1}
	}
	conn.push(t, "ledger", frame("602-0-3"), 602, testEpoch, false)
	conn.push(t, "ledger", frame("603-1-2"), 603, testEpoch, false)
	if e := recv(t, entries); e.ID != "602-0-3" {
		t.Fatalf("缺口补拉 %+v", e)
	}
	e := recv(t, entries)
	if e.ID != "603-1-2" || e.Seq != 603 || e.Sub != 1 || e.Idx != 2 || e.Kind != "trade" || e.Amount != "-1.5" || *e.Symbol != "ETH-USD" {
		t.Fatalf("WS 流水 %+v", e)
	}
	quiet(t, entries, 30*time.Millisecond)
}

// lagged:流水按上界补拉(期间到的帧先缓存),快照类等重发的快照,公共成交报缺口。
func TestStreamLagged(t *testing.T) {
	h := newWSHarness(t, nil)
	fh := h.history(200, 201)
	fh.add("/v1/account/fills", fillRow("201-0-0"), fillRow("201-0-1"), fillRow("202-0-0"))
	fills := make(chan Fill, 16)
	books := make(chan *Book, 8)
	trades := make(chan Trade, 8)
	fs, _ := h.st.SubscribeFills(h.ref, HistoryCursor{After: "200-65535-4294967295", Epoch: testEpoch}, func(f Fill) { fills <- f })
	bs, _ := h.st.SubscribeBook("ETH-USD", 0, func(b *Book) { books <- b })
	ts, _ := h.st.SubscribeTrades("ETH-USD", func(t Trade) { trades <- t })
	h.run()
	conn := h.fw.accept(t)
	reqs := conn.subscribes(t, 3)
	if _, ok := reqs["book"].Params["depth"]; ok {
		t.Fatalf("depth 0 不该发 depth: %v", reqs["book"].Params)
	}
	for _, q := range reqs {
		conn.ack(t, q, testEpoch)
	}
	conn.push(t, "book", jsonObj(t, fixture(t, "book.json"), nil), 200, "", true)
	for _, s := range []*Subscription{fs, bs, ts} {
		h.waitState(s, StateLive)
	}
	recv(t, books)
	conn.push(t, "trades", map[string]any{"symbol": "ETH-USD", "id": "201-0-0", "price": "2492.6", "size": "0.1", "side": "buy", "ts": 1}, 201, "", false)
	if tr := recv(t, trades); tr.ID != "201-0-0" || tr.Price != 24926 || tr.Lots != 10 || tr.Side != "buy" {
		t.Fatalf("trades %+v", tr)
	}
	conn.push(t, "fills", private(fillRow("201-0-0")), 201, testEpoch, false)
	if f := recv(t, fills); f.ID != "201-0-0" {
		t.Fatalf("第一帧 %+v", f)
	}

	// 丢了 201-0-1、202-0-0;补拉上界 202 还没写进执行历史
	conn.send(t, map[string]any{"channel": "lagged", "data": map[string]any{"dropped": 5, "epoch": testEpoch, "upper": "202-65535-4294967295"}})
	var lag *LaggedError
	if e := h.waitState(fs, StateSyncing); !errors.As(e.Err, &lag) || lag.Upper != "202-65535-4294967295" || lag.Dropped != 5 {
		t.Fatalf("fills lagged 原因 %v", e.Err)
	}
	if e := h.waitState(bs, StateSyncing); !errors.As(e.Err, &lag) {
		t.Fatalf("book lagged 原因 %v", e.Err)
	}
	var gap *TradesGapError
	if e := h.waitState(ts, StateSyncing); !errors.As(e.Err, &gap) {
		t.Fatalf("trades 缺口 %v", e.Err)
	}
	h.waitState(ts, StateLive)
	// 服务端重发快照;上界之后的流水帧先缓存
	conn.push(t, "book", jsonObj(t, fixture(t, "book.json"), nil), 203, "", true)
	h.waitState(bs, StateLive)
	conn.push(t, "fills", private(fillRow("203-0-0")), 203, testEpoch, false)
	quiet(t, fills, 100*time.Millisecond)
	fh.set(func(fh *fakeHistory) { fh.complete = 202 })
	if got := ids(t, fills, 3, fillID); !reflect.DeepEqual(got, []string{"201-0-1", "202-0-0", "203-0-0"}) {
		t.Fatalf("lagged 拼接: %v", got)
	}
	h.waitState(fs, StateLive)
	conn.push(t, "fills", private(fillRow("204-0-0")), 204, testEpoch, false)
	if f := recv(t, fills); f.ID != "204-0-0" {
		t.Fatalf("lagged 之后直接交付 %+v", f)
	}
}

// 断线:全部回到 Syncing,按退避重连、重订;流水从已交付的位置接着补。
func TestStreamReconnectResumes(t *testing.T) {
	h := newWSHarness(t, nil)
	fh := h.history(300, 302)
	fh.add("/v1/account/fills", fillRow("301-0-0"), fillRow("302-0-0"))
	fills := make(chan Fill, 8)
	fs, _ := h.st.SubscribeFills(h.ref, HistoryCursor{After: "300-65535-4294967295", Epoch: testEpoch}, func(f Fill) { fills <- f })
	bs, _ := h.st.SubscribeBook("ETH-USD", 0, func(*Book) {})
	ts, _ := h.st.SubscribeTrades("ETH-USD", func(Trade) {})
	h.run()
	conn := h.fw.accept(t)
	for _, q := range conn.subscribes(t, 3) {
		conn.ack(t, q, testEpoch)
	}
	conn.push(t, "book", jsonObj(t, fixture(t, "book.json"), nil), 300, "", true)
	conn.push(t, "fills", private(fillRow("301-0-0")), 301, testEpoch, false)
	if f := recv(t, fills); f.ID != "301-0-0" {
		t.Fatalf("%+v", f)
	}
	h.waitState(bs, StateLive)

	_ = conn.ws.Close()
	for _, s := range []*Subscription{fs, bs} {
		if e := h.waitState(s, StateSyncing); !errors.Is(e.Err, ErrConnLost) {
			t.Fatalf("%s 断线原因 %v", s.Channel(), e.Err)
		}
	}
	var gap *TradesGapError
	if e := h.waitState(ts, StateSyncing); !errors.As(e.Err, &gap) || !errors.Is(e.Err, ErrConnLost) {
		t.Fatalf("trades 断线要报缺口 %v", e.Err)
	}
	// 断线期间 302 发生
	fh.set(func(fh *fakeHistory) { fh.asOf = 302 })
	conn = h.fw.accept(t)
	reqs := conn.subscribes(t, 3)
	for _, q := range reqs {
		conn.ack(t, q, testEpoch)
	}
	conn.push(t, "book", jsonObj(t, fixture(t, "book.json"), nil), 302, "", true)
	if f := recv(t, fills); f.ID != "302-0-0" {
		t.Fatalf("重连后补拉 %+v", f)
	}
	for _, s := range []*Subscription{fs, bs, ts} {
		h.waitState(s, StateLive)
	}
	asked := fh.asked("/v1/account/fills")
	if last := asked[len(asked)-1]; last.Get("after") != "301-0-0" {
		t.Fatalf("要从已交付的位置续拉: %v", last)
	}
	if len(h.connErrors()) == 0 {
		t.Fatal("断线要经 OnConnError 报出")
	}
}

// 重连后纪元变了:私有订阅进终态并在服务端退订,公共订阅照常;调用方按新纪元重建。
func TestStreamEpochChangeOnReconnect(t *testing.T) {
	h := newWSHarness(t, nil)
	fh := h.history(400, 400)
	fs, _ := h.st.SubscribeFills(h.ref, HistoryCursor{After: "400-65535-4294967295", Epoch: testEpoch}, func(Fill) {})
	as, _ := h.st.SubscribeAccount(h.ref, func(*Account) {})
	bs, _ := h.st.SubscribeBook("ETH-USD", 0, func(*Book) {})
	h.run()
	conn := h.fw.accept(t)
	for _, q := range conn.subscribes(t, 3) {
		conn.ack(t, q, testEpoch)
	}
	conn.push(t, "account", jsonObj(t, fixture(t, "account.json"), nil), 400, testEpoch, true)
	for _, s := range []*Subscription{fs, as} {
		h.waitState(s, StateLive)
	}

	_ = conn.ws.Close()
	conn = h.fw.accept(t)
	reqs := conn.subscribes(t, 3)
	fh.set(func(fh *fakeHistory) { fh.epoch = "k2-1" })
	for _, q := range reqs {
		conn.ack(t, q, "k2-1")
	}
	for _, s := range []*Subscription{fs, as} {
		if e := h.waitState(s, StateFailed); !errors.Is(e.Err, ErrEpochChanged) {
			t.Fatalf("%s 纪元变化原因 %v", s.Channel(), e.Err)
		}
	}
	unsub := map[string]bool{}
	for range 2 {
		q := conn.next(t)
		if q.Method != "unsubscribe" {
			t.Fatalf("私有订阅要在服务端退订: %+v", q)
		}
		unsub[q.Params["channel"].(string)] = true
	}
	if !unsub["fills"] || !unsub["account"] {
		t.Fatalf("退订了 %v", unsub)
	}
	conn.push(t, "book", jsonObj(t, fixture(t, "book.json"), nil), 401, "", true)
	h.waitState(bs, StateLive)

	// 终态订阅不再重订
	_ = conn.ws.Close()
	conn = h.fw.accept(t)
	if q := conn.next(t); q.Params["channel"] != "book" {
		t.Fatalf("只该重订 book: %+v", q)
	}
	conn.silent(t, 50*time.Millisecond)
	// 调用方退订旧的、用新纪元的游标重新订阅
	fs.Unsubscribe()
	if _, err := h.st.SubscribeFills(h.ref, HistoryCursor{After: "400-65535-4294967295", Epoch: "k2-1"}, func(Fill) {}); err != nil {
		t.Fatal(err)
	}
	if q := conn.next(t); q.Method != "subscribe" || q.Params["channel"] != "fills" {
		t.Fatalf("新订阅 %+v", q)
	}
}

// 起点游标的纪元与服务端不符:订阅即失败,不拿旧游标续读。
func TestStreamFillsStaleEpochFails(t *testing.T) {
	h := newWSHarness(t, nil)
	h.history(400, 400)
	fs, _ := h.st.SubscribeFills(h.ref, HistoryCursor{After: "400-0-0", Epoch: "k0-9"}, func(Fill) {})
	h.run()
	conn := h.fw.accept(t)
	conn.ack(t, conn.subscribes(t, 1)["fills"], testEpoch)
	if e := h.waitState(fs, StateFailed); !errors.Is(e.Err, ErrEpochChanged) {
		t.Fatalf("原因 %v", e.Err)
	}
	if q := conn.next(t); q.Method != "unsubscribe" {
		t.Fatalf("要在服务端退订: %+v", q)
	}
}

// 心跳:客户端定时 ping;有帧来就保持连接;读超时就断开重连。
func TestStreamHeartbeatAndReadTimeout(t *testing.T) {
	h := newWSHarness(t, func(o *StreamOptions) {
		o.PingInterval = 20 * time.Millisecond
		o.ReadTimeout = 200 * time.Millisecond
	})
	bs, _ := h.st.SubscribeBook("ETH-USD", 0, func(*Book) {})
	h.run()
	conn := h.fw.accept(t)
	pings := 0
	keep := time.After(500 * time.Millisecond)
loop:
	for {
		select {
		case q, ok := <-conn.reqs:
			if !ok {
				t.Fatal("一直有 pong 时不该断开")
			}
			switch q.Method {
			case "ping":
				pings++
				conn.send(t, map[string]any{"channel": "pong", "data": map[string]any{"ts": 1}})
			case "subscribe":
				conn.ack(t, q, testEpoch)
				conn.push(t, "book", jsonObj(t, fixture(t, "book.json"), nil), 1, "", true)
			}
		case <-keep:
			break loop
		}
	}
	if pings < 5 {
		t.Fatalf("500ms 里只有 %d 次心跳", pings)
	}
	select {
	case <-h.fw.conns:
		t.Fatal("一直有 pong 时不该重连")
	default:
	}
	h.waitState(bs, StateLive)
	// 服务端不再回帧:读超时,断开重连
	conn.gone(t)
	if e := h.waitState(bs, StateSyncing); !errors.Is(e.Err, ErrConnLost) {
		t.Fatalf("原因 %v", e.Err)
	}
	conn = h.fw.accept(t)
	conn.subscribes(t, 1)
}

// 订阅请求被拒是终态:报出错误帧,重连后也不重订。
func TestStreamSubscribeRejected(t *testing.T) {
	h := newWSHarness(t, nil)
	as, _ := h.st.SubscribeAccount(h.ref, func(*Account) {})
	h.run()
	conn := h.fw.accept(t)
	q := conn.subscribes(t, 1)["account"]
	conn.send(t, map[string]any{"channel": "error", "id": q.ID,
		"data": map[string]any{"code": "GatewayBadRequest", "message": "unknown address (no account yet, or no such subaccount)"}})
	var we *WSError
	if e := h.waitState(as, StateFailed); !errors.As(e.Err, &we) || we.Code != "GatewayBadRequest" {
		t.Fatalf("原因 %v", e.Err)
	}
	_ = conn.ws.Close()
	h.fw.accept(t).silent(t, 50*time.Millisecond)
}

// 某个订阅取快照失败:置 Syncing 报出原因,稍后退订重订换快照。
func TestStreamSnapshotErrorResubscribes(t *testing.T) {
	h := newWSHarness(t, nil)
	positions := make(chan []Position, 4)
	ps, _ := h.st.SubscribePositions(h.ref, func(p []Position) { positions <- p })
	h.run()
	conn := h.fw.accept(t)
	q := conn.subscribes(t, 1)["positions"]
	conn.ack(t, q, testEpoch)
	conn.send(t, map[string]any{"channel": "error", "data": map[string]any{"code": "GatewayUnavailable",
		"message": "positions snapshot failed: HTTP 503", "subscription": q.Params}})
	var we *WSError
	if e := h.waitState(ps, StateSyncing); !errors.As(e.Err, &we) || we.Code != "GatewayUnavailable" {
		t.Fatalf("原因 %v", e.Err)
	}
	if q := conn.next(t); q.Method != "unsubscribe" {
		t.Fatalf("要先退订 %+v", q)
	}
	q = conn.next(t)
	if q.Method != "subscribe" {
		t.Fatalf("要重订 %+v", q)
	}
	conn.ack(t, q, testEpoch)
	conn.push(t, "positions", jsonObj(t, fixture(t, "positions.json"), nil), 2, testEpoch, true)
	h.waitState(ps, StateLive)
	if p := recv(t, positions); len(p) != 1 {
		t.Fatalf("%+v", p)
	}
}

// 退订:服务端退订,在途的帧不再回调;同一组参数可以重新订阅。
func TestStreamUnsubscribe(t *testing.T) {
	h := newWSHarness(t, nil)
	books := make(chan *Book, 4)
	bs, _ := h.st.SubscribeBook("ETH-USD", 0, func(b *Book) { books <- b })
	h.run()
	conn := h.fw.accept(t)
	q := conn.subscribes(t, 1)["book"]
	conn.ack(t, q, testEpoch)
	conn.push(t, "book", jsonObj(t, fixture(t, "book.json"), nil), 1, "", true)
	recv(t, books)
	bs.Unsubscribe()
	if u := conn.next(t); u.Method != "unsubscribe" || !reflect.DeepEqual(u.Params, q.Params) {
		t.Fatalf("退订 %+v", u)
	}
	conn.push(t, "book", jsonObj(t, fixture(t, "book.json"), nil), 2, "", false)
	quiet(t, books, 50*time.Millisecond)
	if _, err := h.st.SubscribeBook("ETH-USD", 0, func(*Book) {}); err != nil {
		t.Fatal(err)
	}
	if q := conn.next(t); q.Method != "subscribe" {
		t.Fatalf("重新订阅 %+v", q)
	}
}
