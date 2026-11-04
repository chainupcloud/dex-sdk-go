package dexosv1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"
)

// 按市场读(盘口、K 线、单个市场、挂单 / 成交过滤、按市场的实收费率)与 WS 市场频道:线上按 symbol + kind 定位,
// 同名的合约与现货各走各的;应答里的 kind 与问的市场不符即报错。同名场景见 same_symbol_test.go。

// sameNameFake 假服务换成同名目录。
func sameNameFake(t *testing.T) (*fakeV1, *Client, *Catalog) {
	t.Helper()
	f, c := newFake(t)
	body := sameNameMarkets(t, nil)
	f.routes["/v1/markets"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }
	cat, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return f, c, cat
}

// answerFor 按查询串里的 kind 应答;没带 kind 就按服务端的同名规则回 400 ambiguous_symbol。
func answerFor(t *testing.T, seen *[]url.Values, byKind map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		*seen = append(*seen, q)
		body, ok := byKind[q.Get("kind")]
		if !ok {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"code":"GatewayBadRequest","num":10400,"message":"symbol BTC-USD names several markets; pass kind (perp | spot)","details":{"symbol":"BTC-USD","rule":"ambiguous_symbol","kinds":["perp","spot"]}}}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}
}

func TestSameSymbolMarketReadsSendKind(t *testing.T) {
	ctx := context.Background()
	f, c, cat := sameNameFake(t)
	spot, _ := cat.Lookup("BTC-USD", KindSpot)
	perp, _ := cat.Lookup("BTC-USD", KindPerp)
	var seen []url.Values

	f.routes["/v1/markets/BTC-USD/book"] = answerFor(t, &seen, map[string]string{
		"spot": `{"symbol":"BTC-USD","kind":"spot","depth":1,"bids":[["40000","0.00001"]],"asks":[],"seq":5,"ts":1}`,
		"perp": `{"symbol":"BTC-USD","kind":"spot","depth":1,"bids":[["40000","0.00001"]],"asks":[],"seq":5,"ts":1}`,
	})
	b, err := c.Book(ctx, spot, 1)
	if err != nil {
		t.Fatal(err)
	}
	if b.Kind != KindSpot || b.Bids[0] != (Level{Price: 40000, Lots: 10}) || seen[0].Get("kind") != "spot" || seen[0].Get("depth") != "1" {
		t.Fatalf("现货盘口: %+v %v", b, seen[0])
	}
	if _, err := c.Book(ctx, perp, 1); err == nil {
		t.Fatal("问合约的盘口,答的是现货,应报错")
	}

	f.routes["/v1/markets/BTC-USD/candles"] = answerFor(t, &seen, map[string]string{
		"perp": `{"symbol":"BTC-USD","kind":"perp","interval":"1m","candles":[{"t":0,"T":60000,"i":"1m","o":"65000","h":"65000","l":"65000","c":"65000","v":"0.5"}]}`,
	})
	k, err := c.Candles(ctx, perp, CandleQuery{Interval: Interval1m})
	if err != nil || len(k) != 1 || k[0].Volume != 500 || seen[len(seen)-1].Get("kind") != "perp" {
		t.Fatalf("合约 K 线: %+v %v", k, err)
	}

	spotJSON := string(sameNameMarkets(t, nil))
	f.routes["/v1/markets/BTC-USD"] = answerFor(t, &seen, map[string]string{"spot": marketRow(t, spotJSON, spotBTCUSD), "perp": marketRow(t, spotJSON, spotBTCUSD)})
	m, err := c.Market(ctx, "BTC-USD", KindSpot)
	if err != nil || m.SigningMarketID != spotBTCUSD || m.Kind != KindSpot {
		t.Fatalf("单个市场按名称 + kind: %+v %v", m, err)
	}
	if _, err := c.Market(ctx, "BTC-USD", ""); !errors.Is(err, ErrAmbiguousSymbol) {
		t.Fatalf("同名不带 kind,服务端回 ambiguous_symbol: %v", err)
	}
	if _, err := c.Market(ctx, "BTC-USD", KindPerp); err == nil {
		t.Fatal("问合约,答的是现货,应报错")
	}
	n := len(seen)
	if _, err := c.Market(ctx, "BTC-USD", "future"); !errors.Is(err, ErrBadKind) || len(seen) != n {
		t.Fatalf("非法 kind 本地就拦下、不发请求: %v", err)
	}
}

// marketRow 目录响应里市场号为 id 的那一行(单个市场接口的响应体)。
func marketRow(t *testing.T, markets string, id uint16) string {
	t.Helper()
	var doc struct {
		Markets []map[string]any `json:"markets"`
	}
	if err := json.Unmarshal([]byte(markets), &doc); err != nil {
		t.Fatal(err)
	}
	for _, row := range doc.Markets {
		if row["signingMarketId"] == float64(id) {
			raw, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			return string(raw)
		}
	}
	t.Fatalf("目录里没有市场 %d", id)
	return ""
}

func TestSameSymbolAccountReadsFilterByKind(t *testing.T) {
	ctx := context.Background()
	f, c, cat := sameNameFake(t)
	spot, _ := cat.Lookup("BTC-USD", KindSpot)
	ref := AccountRef{Address: mustAddr(t, testAddr)}
	var seen []url.Values

	f.routes["/v1/account/orders"] = answerFor(t, &seen, map[string]string{
		"spot": `{"address":"` + testAddr + `","subaccount":0,"orders":[{"orderId":"7","clientOrderId":null,"symbol":"BTC-USD","kind":"spot","side":"buy","price":"40000","size":"0.00001","filled":"0","remaining":"0.00001","tif":null,"reduceOnly":false,"trigger":null,"status":"open"}]}`,
		"":     `{"address":"` + testAddr + `","subaccount":0,"orders":[{"orderId":"7","clientOrderId":null,"symbol":"BTC-USD","kind":"spot","side":"buy","price":"40000","size":"0.00001","filled":"0","remaining":"0.00001","tif":null,"reduceOnly":false,"trigger":null,"status":"open"},{"orderId":"8","clientOrderId":null,"symbol":"BTC-USD","kind":"perp","side":"sell","price":"65000","size":"0.5","filled":"0","remaining":"0.5","tif":null,"reduceOnly":false,"trigger":null,"status":"open"}]}`,
	})
	open, err := c.Orders(ctx, cat, ref, spot)
	if err != nil {
		t.Fatal(err)
	}
	if q := seen[0]; q.Get("symbol") != "BTC-USD" || q.Get("kind") != "spot" || len(open) != 1 || open[0].SigningMarketID != spotBTCUSD || open[0].Lots != 10 {
		t.Fatalf("按现货过滤挂单: %v %+v", q, open)
	}
	all, err := c.Orders(ctx, cat, ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if q := seen[1]; q.Has("symbol") || q.Has("kind") || len(all) != 2 || all[1].SigningMarketID != 0 || all[1].Kind != KindPerp || all[1].Lots != 500 {
		t.Fatalf("全市场挂单: %v %+v", q, all)
	}

	f.routes["/v1/account/fills"] = answerFor(t, &seen, map[string]string{
		"spot": `{"address":"` + testAddr + `","subaccount":0,"epoch":"k1-1","upper":"9-65535-4294967295","hasMore":false,"next":null,"fills":[{"id":"9-0-0","seq":9,"orderId":"7","symbol":"BTC-USD","kind":"spot","side":"buy","role":"maker","price":"40000","size":"0.00001","fee":"0","feeToken":"USDC","ts":1}]}`,
	})
	h, err := c.Fills(ctx, cat, ref, spot, HistoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if q := seen[len(seen)-1]; q.Get("symbol") != "BTC-USD" || q.Get("kind") != "spot" || len(h.Fills) != 1 || h.Fills[0].SigningMarketID != spotBTCUSD || h.Fills[0].Lots != 10 {
		t.Fatalf("按现货过滤成交: %v %+v", q, h.Fills)
	}

	fees := `{"epoch":"k1-1","asOfSeq":1,"fees":{"builder":null,"markets":[{"symbol":"BTC-USD","kind":"%s","maker":{"rate":"0","protocol":"0","builder":"0"},"taker":{"rate":"0.001","protocol":"0.001","dex":"0","deployer":"0","builder":"0"},"source":{"protocol":"tier","builder":"none"}}]}}`
	f.routes["/v1/account"] = answerFor(t, &seen, map[string]string{"spot": fmt.Sprintf(fees, "spot"), "perp": fmt.Sprintf(fees, "spot")})
	got, err := c.AccountFeesIn(ctx, ref, spot)
	if err != nil || got.Kind != KindSpot {
		t.Fatalf("现货的实收费率: %+v %v", got, err)
	}
	if q := seen[len(seen)-1]; q.Get("symbol") != "BTC-USD" || q.Get("kind") != "spot" {
		t.Fatalf("按市场读费率的参数: %v", q)
	}
	perp, _ := cat.Lookup("BTC-USD", KindPerp)
	if _, err := c.AccountFeesIn(ctx, ref, perp); err == nil {
		t.Fatal("问合约的费率,答的是现货,应报错")
	}
}

// WS:同名的合约与现货可以同时订盘口 / 成交(订阅参数带 kind,帧按名称 + kind 路由,各按自己的精度换算);
// dex-os#318 之前的节点帧不带 kind,按这条连接上订的那一种路由。orders 增量帧带出市场类型与市场号。
func TestStreamSameSymbolRoutesByKind(t *testing.T) {
	h := newWSHarness(t, nil)
	body := sameNameMarkets(t, nil)
	h.f.mu.Lock()
	h.f.routes["/v1/markets"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }
	h.f.mu.Unlock()
	cat, err := h.c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetCatalog(cat); err != nil {
		t.Fatal(err)
	}
	spot, _ := cat.Lookup("BTC-USD", KindSpot)
	perp, _ := cat.Lookup("BTC-USD", KindPerp)
	spotBooks, perpBooks := make(chan *Book, 4), make(chan *Book, 4)
	trades := make(chan Trade, 4)
	ups := make(chan OrdersUpdate, 4)
	sb, err := h.st.SubscribeBook(spot, 1, func(b *Book) { spotBooks <- b })
	if err != nil {
		t.Fatal(err)
	}
	pb, err := h.st.SubscribeBook(perp, 1, func(b *Book) { perpBooks <- b })
	if err != nil {
		t.Fatalf("同名的合约与现货可以各订一个盘口: %v", err)
	}
	if _, err := h.st.SubscribeBook(spot, 2, func(*Book) {}); err == nil {
		t.Fatal("同一个市场只能有一个盘口订阅")
	}
	ts, _ := h.st.SubscribeTrades(spot, func(tr Trade) { trades <- tr })
	osub, _ := h.st.SubscribeOrders(h.ref, func(u OrdersUpdate) { ups <- u })
	h.run()

	conn := h.fw.accept(t)
	reqs := map[string]wsReq{}
	for range 4 {
		q := conn.next(t)
		key, _ := q.Params["channel"].(string)
		if k, ok := q.Params["kind"].(string); ok {
			key += ":" + k
		}
		reqs[key] = q
		conn.ack(t, q, testEpoch, 1)
	}
	for key, want := range map[string]map[string]any{
		"book:spot":   {"channel": "book", "symbol": "BTC-USD", "kind": "spot", "depth": float64(1)},
		"book:perp":   {"channel": "book", "symbol": "BTC-USD", "kind": "perp", "depth": float64(1)},
		"trades:spot": {"channel": "trades", "symbol": "BTC-USD", "kind": "spot"},
	} {
		if !reflect.DeepEqual(reqs[key].Params, want) {
			t.Fatalf("%s 订阅参数 %v", key, reqs[key].Params)
		}
	}
	conn.push(t, "book", map[string]any{"symbol": "BTC-USD", "kind": "perp", "depth": 1, "bids": [][]string{{"65000", "0.5"}}, "asks": [][]string{}, "seq": 2, "ts": 1}, 2, "", true)
	conn.push(t, "book", map[string]any{"symbol": "BTC-USD", "kind": "spot", "depth": 1, "bids": [][]string{{"40000", "0.00001"}}, "asks": [][]string{}, "seq": 2, "ts": 1}, 2, "", true)
	conn.push(t, "orders", map[string]any{"address": testAddr, "subaccount": 0, "orders": []any{}}, 2, testEpoch, true)
	h.waitState(sb, StateLive)
	h.waitState(pb, StateLive)
	h.waitState(osub, StateLive)
	if b := recv(t, perpBooks); b.Kind != KindPerp || b.Bids[0] != (Level{Price: 65000, Lots: 500}) {
		t.Fatalf("合约盘口: %+v", b)
	}
	if b := recv(t, spotBooks); b.Kind != KindSpot || b.Bids[0] != (Level{Price: 40000, Lots: 10}) {
		t.Fatalf("现货盘口: %+v", b)
	}
	recv(t, ups)
	conn.push(t, "trades", map[string]any{"symbol": "BTC-USD", "kind": "spot", "id": "3-0-0", "price": "40000", "size": "0.00002", "side": "buy", "ts": 1}, 3, "", false)
	if tr := recv(t, trades); tr.Kind != KindSpot || tr.Lots != 20 || ts.State() != StateLive {
		t.Fatalf("现货成交: %+v", tr)
	}
	conn.push(t, "orders", orderFrame(map[string]any{"symbol": "BTC-USD", "kind": "spot", "orderId": "7", "event": "accepted", "status": "open",
		"side": "buy", "price": "40000", "size": "0.00001", "filled": "0", "remaining": "0.00001"}), 4, testEpoch, false)
	u := recv(t, ups)
	if u.Event == nil || u.Event.MarketKind != KindSpot || u.Event.SigningMarketID != spotBTCUSD || u.Event.Lots != 10 ||
		len(u.Open) != 1 || u.Open[0].Kind != KindSpot || u.Open[0].SigningMarketID != spotBTCUSD {
		t.Fatalf("orders 增量帧: %+v", u)
	}
	quiet(t, spotBooks, 20*time.Millisecond)
	quiet(t, perpBooks, 20*time.Millisecond)
}

// dex-os#318 之前的节点:帧不带 kind。名称只对应一个市场,这条连接上只订了它一种类型,照常路由。
func TestStreamRoutesFramesWithoutKind(t *testing.T) {
	h := newWSHarness(t, nil)
	books := make(chan *Book, 2)
	bs, _ := h.st.SubscribeBook(mustMarket(t, h.cat, "ETH-USD"), 0, func(b *Book) { books <- b })
	h.run()
	conn := h.fw.accept(t)
	q := conn.next(t)
	if q.Params["kind"] != "perp" {
		t.Fatalf("订阅参数带 kind: %v", q.Params)
	}
	conn.ack(t, q, testEpoch, 1)
	conn.push(t, "book", jsonObj(t, fixture(t, "book.json"), nil), 2, "", true)
	h.waitState(bs, StateLive)
	if b := recv(t, books); b.Kind != KindPerp || b.Bids[0] != (Level{Price: 24926, Lots: 300}) {
		t.Fatalf("不带 kind 的帧: %+v", b)
	}
}
