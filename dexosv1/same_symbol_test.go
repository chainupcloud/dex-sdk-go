package dexosv1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 合约与现货同名(dex-os#318 / #321,issue #38):一个名称至多对应一个合约市场和一个现货市场,按名称定位时另带 kind。
// 这里的同名场景照 dex-os api_e2e test_v1_perp_and_spot_may_share_a_symbol:BTC-USD 合约(市场号 0,精度 0 / 3)与
// BTC-USD 现货(市场号 3,精度 0 / 6)。两边精度不同,价量按错的市场换算就会对不上刻度,测得出来。

const spotBTCUSD = 3

// sameNameMarkets markets.json 再加一个与 BTC-USD 合约同名的现货市场;edit 可再改各行(按市场号取)。
func sameNameMarkets(t *testing.T, edit func(byID map[uint16]map[string]any)) []byte {
	t.Helper()
	var doc struct {
		Markets []map[string]any `json:"markets"`
	}
	if err := json.Unmarshal(fixture(t, "markets.json"), &doc); err != nil {
		t.Fatal(err)
	}
	spot := map[string]any{}
	for k, v := range doc.Markets[2] { // BTC-USDC 现货
		spot[k] = v
	}
	for k, v := range map[string]any{"symbol": "BTC-USD", "signingMarketId": spotBTCUSD, "sizeDecimals": 6, "lotSize": "0.000001",
		"quotePerTickLot": "1", "basePerLot": "100", "lastPrice": "40000", "bestBid": "39990", "bestAsk": nil, "aggTicks": nil, "minNotional": "0"} {
		spot[k] = v
	}
	doc.Markets = append(doc.Markets, spot)
	if edit != nil {
		byID := map[uint16]map[string]any{}
		for _, m := range doc.Markets {
			id, _ := m["signingMarketId"].(float64)
			if n, ok := m["signingMarketId"].(int); ok {
				id = float64(n)
			}
			byID[uint16(id)] = m
		}
		edit(byID)
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func sameNameCatalog(t *testing.T) *Catalog {
	t.Helper()
	var doc struct {
		Markets []wireMarket `json:"markets"`
	}
	if err := json.Unmarshal(sameNameMarkets(t, nil), &doc); err != nil {
		t.Fatal(err)
	}
	cat, err := newCatalog(doc.Markets)
	if err != nil {
		t.Fatalf("合约与现货同名的目录应能读: %v", err)
	}
	return cat
}

// 目录不以名称为唯一键:只给名称 → ErrAmbiguousSymbol(列出可选的 kind,不替调用方挑);名称 + kind、市场号都能定位;
// 名称只对应一个市场的照旧只给名称。
func TestSameSymbolCatalogLocatesByKindOrID(t *testing.T) {
	cat := sameNameCatalog(t)

	_, err := cat.Market("btc-usd")
	var se *SymbolError
	var apiErr *Error
	if !errors.Is(err, ErrAmbiguousSymbol) || !errors.As(err, &se) || se.Rule != "ambiguous_symbol" || se.Symbol != "btc-usd" ||
		!slices.Equal(se.Kinds, []string{KindPerp, KindSpot}) || se.Err != nil || errors.As(err, &apiErr) {
		t.Fatalf("同名只给名称应报 ErrAmbiguousSymbol 并列出 kind: %#v", err)
	}
	perp, err := cat.Lookup("BTC-USD", KindPerp)
	if err != nil || perp.SigningMarketID != 0 || perp.Kind != KindPerp || perp.Scale != (Scale{0, 3}) {
		t.Fatalf("按名称 + perp: %+v %v", perp, err)
	}
	spot, err := cat.Lookup("btc-usd", KindSpot)
	if err != nil || spot.SigningMarketID != spotBTCUSD || spot.Kind != KindSpot || spot.Scale != (Scale{0, 6}) || *spot.BestBid != 39990 {
		t.Fatalf("按名称 + spot: %+v %v", spot, err)
	}
	if byID, err := cat.MarketByID(spotBTCUSD); err != nil || byID != spot {
		t.Fatalf("按市场号: %+v %v", byID, err)
	}
	if _, err := cat.MarketByID(99); err == nil {
		t.Fatal("目录外的市场号应报错")
	}
	if _, err := cat.Lookup("BTC-USD", "Spot"); !errors.Is(err, ErrBadKind) || !errors.As(err, &se) || se.Kind != "Spot" {
		t.Fatalf("kind 只认 perp / spot(区分大小写,同服务端): %v", err)
	}
	if _, err := cat.Lookup("ETH-USD", KindSpot); err == nil || errors.As(err, &se) {
		t.Fatalf("名称下没有这种类型是「没有这个市场」,不是 SymbolError: %v", err)
	}
	if eth, err := cat.Market("ETH-USD"); err != nil || eth.SigningMarketID != 1 {
		t.Fatalf("名称只对应一个市场时只给名称照旧: %+v %v", eth, err)
	}
	if eth, err := cat.Lookup("ETH-USD", KindPerp); err != nil || eth.SigningMarketID != 1 {
		t.Fatalf("名称唯一时带上 kind 也一样: %+v %v", eth, err)
	}

	// 合约下市了,名称只剩现货在用:只给名称就是现货
	body := sameNameMarkets(t, func(byID map[uint16]map[string]any) { byID[0]["status"] = "Delisted" })
	var doc struct {
		Markets []wireMarket `json:"markets"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	cat2, err := newCatalog(doc.Markets)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := cat2.Market("BTC-USD"); err != nil || m.SigningMarketID != spotBTCUSD {
		t.Fatalf("合约下市后名称只对应现货: %+v %v", m, err)
	}
	if _, err := cat2.Lookup("BTC-USD", KindPerp); err == nil {
		t.Fatal("下市的合约不再按名称定位")
	}

	// 市场号重复才是目录错误
	doc.Markets[3].SigningMarketID = 0
	if _, err := newCatalog(doc.Markets); err == nil || !strings.Contains(err.Error(), "市场号") {
		t.Fatalf("市场号重复应报目录错误: %v", err)
	}
}

// 服务端按名称定位的三种拒绝(dex-os v1.rs SymErr::response)解析成 *SymbolError:errors.Is 对得上哨兵值,
// errors.As 仍能取到 *Error(400 / 409,定序之前)。写入遇到它不锁会话、不动 nonce。
func TestSymbolErrorsFromGateway(t *testing.T) {
	bodies := map[string]string{
		"ambiguous": `{"error":{"code":"GatewayBadRequest","num":10400,"message":"symbol BTC-USD names several markets; pass kind (perp | spot)","details":{"symbol":"BTC-USD","rule":"ambiguous_symbol","kinds":["perp","spot"]}}}`,
		"badKind":   `{"error":{"code":"GatewayBadRequest","num":10400,"message":"kind must be perp | spot, got future","details":{"rule":"bad_kind","kinds":["perp","spot"]}}}`,
		"duplicate": `{"error":{"code":"GatewayConflict","num":10409,"message":"symbol BTC-USD names more than one spot market; market names need fixing","details":{"symbol":"BTC-USD","rule":"duplicate_symbol"}}}`,
		"plain400":  `{"error":{"code":"GatewayBadRequest","num":10400,"message":"depth must be 1..=200"}}`,
	}
	spec := loadSpec(t)
	for name, b := range bodies {
		if errs := validate(spec, componentSchema(t, spec, "Error"), decodeJSON(t, []byte(b)), name); len(errs) > 0 {
			t.Fatalf("%s 不符合规格的 Error: %v", name, errs)
		}
	}
	cases := []struct {
		name   string
		status int
		want   error
		kinds  []string
	}{
		{"ambiguous", 400, ErrAmbiguousSymbol, []string{"perp", "spot"}},
		{"badKind", 400, ErrBadKind, []string{"perp", "spot"}},
		{"duplicate", 409, ErrDuplicateSymbol, nil},
	}
	for _, c := range cases {
		f, cl := newFake(t)
		cat := catalog(t, f, cl)
		btc := mustMarket(t, cat, "BTC-USD")
		f.routes["/v1/markets/BTC-USD/book"] = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(bodies[c.name]))
		}
		_, err := cl.Book(context.Background(), btc, 0)
		var se *SymbolError
		var e *Error
		if !errors.Is(err, c.want) || !errors.As(err, &se) || !errors.As(err, &e) || e.Status != c.status || se.Err != e ||
			!slices.Equal(se.Kinds, c.kinds) {
			t.Fatalf("%s: %#v", c.name, err)
		}
		for _, other := range []error{ErrAmbiguousSymbol, ErrBadKind, ErrDuplicateSymbol} {
			if other != c.want && errors.Is(err, other) {
				t.Fatalf("%s 不该同时是 %v", c.name, other)
			}
		}
	}
	f, cl := newFake(t)
	cat := catalog(t, f, cl)
	f.routes["/v1/markets/BTC-USD/book"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(bodies["plain400"]))
	}
	_, err := cl.Book(context.Background(), mustMarket(t, cat, "BTC-USD"), 0)
	var se *SymbolError
	var e *Error
	if errors.As(err, &se) || !errors.As(err, &e) || e.Status != 400 {
		t.Fatalf("没有 rule 的 400 仍是普通 *Error: %#v", err)
	}

	// 写入:按名称定位被拒在定序之前,会话照常可用、nonce 不动
	wf, s, wcat := newTestSession(t)
	wf.answerRaw(t, "/v1/orders", 400, []byte(bodies["ambiguous"]))
	btc := mustMarket(t, wcat, "BTC-USD")
	_, err = s.Place(context.Background(), OrderRequest{Market: btc, Side: dexos.Buy, Price: 1, Lots: 1})
	if !errors.Is(err, ErrAmbiguousSymbol) || errors.Is(err, ErrSessionBlocked) || s.Nonce() != 41 || s.Pending() != nil {
		t.Fatalf("ambiguous_symbol 应是定序前的终局拒绝(不锁、nonce 不动): %v nonce=%d", err, s.Nonce())
	}
	if ClassifyAction(err) != ActionNotExecuted {
		t.Fatalf("ClassifyAction 应判没执行: %v", ClassifyAction(err))
	}
}

// 整表行(prices / tickers)与账户行(持仓、挂单、成交)按名称 + kind 对到目录市场,带出类型与市场号,价量按那个市场换算。
// 行的先后与目录不同也对得上;同名时行上没有 kind(dex-os#318 之前的节点不会出现这种目录)不猜,报 ErrAmbiguousSymbol。
func TestSameSymbolRowsMapByKind(t *testing.T) {
	cat := sameNameCatalog(t)
	var prices wirePrices
	if err := json.Unmarshal([]byte(`{"prices":[
		{"symbol":"BTC-USD","kind":"spot","last":"40000","mark":null,"oracle":null,"bestBid":"39990","bestAsk":null,"volume24h":"0.00001"},
		{"symbol":"ETH-USD","kind":"perp","last":"2493"},
		{"symbol":"BTC-USD","kind":"perp","last":"83952","volume24h":"12.345"},
		{"symbol":"BTC-USDC","kind":"spot","last":"83900"}],"ts":1}`), &prices); err != nil {
		t.Fatal(err)
	}
	ps, err := prices.prices(cat)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps.Prices) != 4 || len(ps.Uncataloged) != 0 {
		t.Fatalf("四行都在目录里: %+v", ps)
	}
	if p := ps.Prices[0]; p.SigningMarketID != spotBTCUSD || p.Kind != KindSpot || p.Last != 40000 || *p.Volume24h != 10 {
		t.Fatalf("现货行应对到市场 3、按 6 位数量精度换算: %+v", p)
	}
	if p := ps.Prices[2]; p.SigningMarketID != 0 || p.Kind != KindPerp || *p.Volume24h != 12345 {
		t.Fatalf("合约行应对到市场 0: %+v", p)
	}

	positions, err := positionsFrom(cat, []wirePosition{
		{Symbol: "BTC-USD", Kind: "perp", Side: "long", Size: "0.184", MarkPrice: "82680"},
		{Symbol: "BTC-USD", Kind: "spot", Side: "long", Size: "0.000002", MarkPrice: "40000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if positions[0].SigningMarketID != 0 || positions[0].Lots != 184 || positions[1].SigningMarketID != spotBTCUSD || positions[1].Kind != KindSpot || positions[1].Lots != 2 {
		t.Fatalf("持仓行: %+v", positions)
	}
	o, err := wireOrder{OrderID: "7", Symbol: "BTC-USD", Kind: "spot", Side: "buy", Price: "40000", Size: "0.00001", Filled: "0", Remaining: "0.00001", Status: "open"}.order(cat)
	if err != nil || o.SigningMarketID != spotBTCUSD || o.Kind != KindSpot || o.Lots != 10 {
		t.Fatalf("挂单行: %+v %v", o, err)
	}
	fl, err := wireFill{ID: "9-0-0", Symbol: "BTC-USD", Kind: "perp", Side: "buy", Role: "maker", Price: "65000", Size: "0.5"}.fill(cat)
	if err != nil || fl.SigningMarketID != 0 || fl.Kind != KindPerp || fl.Lots != 500 {
		t.Fatalf("成交行: %+v %v", fl, err)
	}
	if _, err := (wireFill{ID: "9-0-1", Symbol: "BTC-USD", Side: "buy", Role: "maker", Price: "65000", Size: "0.5"}).fill(cat); !errors.Is(err, ErrAmbiguousSymbol) {
		t.Fatalf("同名而行上没有 kind 不猜: %v", err)
	}
	// 名称唯一时行上没有 kind(dex-os#318 之前的节点)照旧对上
	if fl, err := (wireFill{ID: "9-0-2", Symbol: "ETH-USD", Side: "buy", Role: "maker", Price: "2493", Size: "0.5"}).fill(cat); err != nil || fl.SigningMarketID != 1 || fl.Kind != KindPerp {
		t.Fatalf("名称唯一、行上没有 kind: %+v %v", fl, err)
	}

	f, c := newFake(t)
	f.routes["/v1/tickers"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tickers":[{"symbol":"BTC-USD","kind":"spot","open":null,"high":null,"low":null,"last":"40000","volume":"0.00003","quoteVolume":"1.2","trades":3},
			{"symbol":"BTC-USD","kind":"perp","open":null,"high":null,"low":null,"last":null,"volume":"0","quoteVolume":"0","trades":0}],"windowStart":1,"windowEnd":2}`))
	}
	ts, err := c.Tickers(context.Background(), cat)
	if err != nil {
		t.Fatal(err)
	}
	if ts.Tickers[0].SigningMarketID != spotBTCUSD || ts.Tickers[0].Kind != KindSpot || ts.Tickers[0].Volume != 30 || ts.Tickers[1].SigningMarketID != 0 {
		t.Fatalf("成交统计行: %+v", ts.Tickers)
	}
}

// 返回体里带 symbol 的市场行都带 kind,流水改带 signingMarketId(dex-os public-api-v1 §5);builder 目录行另有 signingMarketId。
func TestSameSymbolMarketRowsDecodeKind(t *testing.T) {
	var b Builder
	if err := json.Unmarshal([]byte(`{"builderId":1,"status":"active","asOfSeq":1,
		"catalog":{"version":2,"markets":[
			{"symbol":"demo:ETH-USDC","kind":"perp","signingMarketId":5,"display":"ETH-USDC","status":"listed","sort":0,"tags":[]},
			{"symbol":"demo:ETH-USDC","kind":"spot","signingMarketId":7,"display":"ETH-USDC","status":"listed","sort":1,"tags":[]}]},
		"fees":{"default":null,"markets":[{"symbol":"demo:ETH-USDC","kind":"spot","takerRate":"0.0003","makerRate":"0"}]}}`), &b); err != nil {
		t.Fatal(err)
	}
	rows := b.Catalog.Markets
	if rows[0].Kind != KindPerp || *rows[0].SigningMarketID != 5 || rows[1].Kind != KindSpot || *rows[1].SigningMarketID != 7 ||
		b.Fees.Markets[0].Kind != KindSpot {
		t.Fatalf("builder 目录 / 费率行: %+v %+v", rows, b.Fees.Markets)
	}
	var e LedgerEntry
	if err := json.Unmarshal([]byte(`{"id":"1-0-0","seq":1,"sub":0,"idx":0,"kind":"trade","token":"USDC","amount":"-1","symbol":"demo:ETH-USDC","signingMarketId":7}`), &e); err != nil {
		t.Fatal(err)
	}
	if e.Kind != "trade" || e.SigningMarketID == nil || *e.SigningMarketID != 7 {
		t.Fatalf("流水: %+v", e)
	}
	a, err := decodeAccount([]byte(`{"epoch":"k1-1","asOfSeq":1,"leverage":[{"symbol":"BTC-USD","kind":"perp","leverage":10,"type":"cross"}],
		"fees":{"builder":null,"markets":[{"symbol":"BTC-USD","kind":"spot","maker":{"rate":"0","protocol":"0","builder":"0"},
		"taker":{"rate":"0.001","protocol":"0.001","dex":"0","deployer":"0","builder":"0"},"source":{"protocol":"tier","builder":"none"}}]}}`), "test")
	if err != nil {
		t.Fatal(err)
	}
	if a.Leverage[0].Kind != KindPerp || a.Fees.Markets[0].Kind != KindSpot {
		t.Fatalf("账户杠杆 / 费率行: %+v %+v", a.Leverage, a.Fees.Markets)
	}
}

// newSameNameSession 写接口的假网关换成同名目录:网关按请求体里的 symbol + kind 定位市场(gatewayMarket),
// 不带 kind 就定位不了、测试失败;再按定位到的市场号重建签名摘要核对请求身份。
func newSameNameSession(t *testing.T) (*writeFake, *Session, *Catalog) {
	t.Helper()
	f, c := newWriteFake(t)
	body := sameNameMarkets(t, nil)
	f.routes["/v1/markets"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }
	cat, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.cat = cat
	s, err := NewSession(context.Background(), c, signer(t, agentKey), AccountRef{Address: mustAddr(t, ownerAddr)})
	if err != nil {
		t.Fatal(err)
	}
	return f, s, cat
}

// 写请求在需要时带 kind:下单各项、撤单各项、换单外层与新单各项、杠杆、成对成交都带上市场的 kind;
// 批量回执项按 signingMarketId 核对是不是本市场。
func TestSameSymbolWritesCarryKind(t *testing.T) {
	ctx := context.Background()
	f, s, cat := newSameNameSession(t)
	spot, _ := cat.Lookup("BTC-USD", KindSpot)
	perp, _ := cat.Lookup("BTC-USD", KindPerp)

	f.answer(t, "/v1/orders", 200, "w_batch_place.json")
	res, err := s.Place(ctx,
		OrderRequest{Market: spot, Side: dexos.Buy, Price: 40000, Lots: 10, TIF: dexos.GTC},
		OrderRequest{Market: spot, Side: dexos.Sell, Price: 41000, Lots: 20, TIF: dexos.PostOnly, ClientOrderID: "q-1"})
	if err != nil {
		t.Fatal(err)
	}
	req := f.last(t)
	conformsRequest(t, "/v1/orders", req.body)
	for i, it := range req.body.([]any) {
		if m(it)["symbol"] != "BTC-USD" || m(it)["kind"] != "spot" || m(it)["size"] != []string{"0.00001", "0.00002"}[i] {
			t.Fatalf("第 %d 项应带 kind spot、按现货精度上线: %v", i, it)
		}
	}
	if g := gatewayOrder(t, cat, m(req.body.([]any)[0])); g.Market != spotBTCUSD {
		t.Fatalf("网关按 symbol + kind 应定位到现货 3: %+v", g)
	}
	if res.Items[0].SigningMarketID != spotBTCUSD {
		t.Fatalf("回执项的市场号: %+v", res.Items[0])
	}

	f.answer(t, "/v1/orders/cancel", 200, "w_batch_cancel.json")
	if _, err := s.Cancel(ctx, spot, 101, 999999); err != nil {
		t.Fatal(err)
	}
	conformsRequest(t, "/v1/orders/cancel", f.last(t).body)
	for _, it := range f.last(t).body.([]any) {
		if m(it)["kind"] != "spot" {
			t.Fatalf("撤单各项都带 kind(网关要求各项解析到同一市场): %v", it)
		}
	}

	f.answer(t, "/v1/orders/replace", 200, "w_replace.json")
	if _, err := s.Replace(ctx, perp, []uint64{101}, []OrderRequest{{Market: perp, Side: dexos.Buy, Price: 64900, Lots: 200, TIF: dexos.GTC}}); err != nil {
		t.Fatal(err)
	}
	body := m(f.last(t).body)
	conformsRequest(t, "/v1/orders/replace", body)
	if body["kind"] != "perp" || m(body["orders"].([]any)[0])["kind"] != "perp" {
		t.Fatalf("换单外层与新单都带 kind(网关按新单自己的 symbol + kind 定位): %v", body)
	}

	// 混了同名不同类的单:本地就拦下,不发请求、不动 nonce
	before, nonce := f.count(), s.Nonce()
	if _, err := s.Place(ctx, OrderRequest{Market: perp, Side: dexos.Buy, Price: 1, Lots: 1}, OrderRequest{Market: spot, Side: dexos.Buy, Price: 1, Lots: 1}); err == nil ||
		f.count() != before || s.Nonce() != nonce {
		t.Fatalf("同名的合约与现货不是同一个市场: %v", err)
	}

	// 回执项的市场号不是本市场(现货的单回执说是合约)→ 批量回执对不上
	f.answerRaw(t, "/v1/orders", 200, []byte(`{"status":"ok","items":[{"index":0,"kind":"place","status":"accepted","symbol":"BTC-USD","signingMarketId":0,"orderId":"1","filled":"0","resting":true}],"seq":9,"sub":0,"requestId":"`+testReqID+`"}`))
	if _, err := s.Place(ctx, OrderRequest{Market: spot, Side: dexos.Buy, Price: 40000, Lots: 10}); !errors.Is(err, ErrIncompleteBatch) {
		t.Fatalf("回执项落在同名的另一个市场上应报 ErrIncompleteBatch: %v", err)
	}
}

// 主钱包与 sigVersion 2 的写:单笔下单 / 撤单 / 杠杆、预签换单与撤单、成对成交都带 kind;回执的 kind 与请求的市场不符即报错。
func TestSameSymbolWalletAndActionWritesCarryKind(t *testing.T) {
	ctx := context.Background()
	f, s, cat := newSameNameSession(t)
	spot, _ := cat.Lookup("BTC-USD", KindSpot)
	perp, _ := cat.Lookup("BTC-USD", KindPerp)
	w, err := NewWallet(ctx, s.Client, signer(t, ownerKey), 0)
	if err != nil {
		t.Fatal(err)
	}

	order := []byte(`{"status":"resting","symbol":"BTC-USD","kind":"spot","clientOrderId":null,"orderId":"202","price":"40000","size":"0.00001","filled":"0","avgPrice":null,"seq":503,"sub":0,"requestId":"` + testReqID + `","fills":[]}`)
	f.answerRaw(t, "/v1/orders", 200, order)
	o, err := w.PlaceOrder(ctx, OrderRequest{Market: spot, Side: dexos.Buy, Price: 40000, Lots: 10, TIF: dexos.GTC}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if body := m(f.last(t).body); body["kind"] != "spot" || o.Kind != KindSpot || o.Lots != 10 {
		t.Fatalf("主钱包下单: %v / %+v", body, o)
	}
	f.answerRaw(t, "/v1/orders", 200, []byte(strings.Replace(string(order), `"kind":"spot"`, `"kind":"perp"`, 1)))
	if _, err := w.PlaceOrder(ctx, OrderRequest{Market: spot, Side: dexos.Buy, Price: 40000, Lots: 10, TIF: dexos.GTC}, time.Time{}); err == nil {
		t.Fatal("在现货下单,回执说是合约,应报错")
	}

	w2, err := NewWallet(ctx, s.Client, signer(t, ownerKey), 0)
	if err != nil {
		t.Fatal(err)
	}
	f.answerRaw(t, "/v1/orders/cancel", 200, []byte(`{"status":"canceled","symbol":"BTC-USD","kind":"spot","orderId":"202","canceled":true,"remaining":"0.00001","seq":504,"sub":0,"requestId":"`+testReqID+`"}`))
	cr, err := w2.Cancel(ctx, spot, 202)
	if err != nil {
		t.Fatal(err)
	}
	if body := m(f.last(t).body); body["kind"] != "spot" || *cr.RemainingLots != 10 {
		t.Fatalf("主钱包撤单: %v / %+v", body, cr)
	}
	f.answer(t, "/v1/account/leverage", 200, "w_leverage.json")
	if _, err := w2.SetLeverage(ctx, perp, 3); err != nil {
		t.Fatal(err)
	}
	if body := m(f.last(t).body); body["kind"] != "perp" {
		t.Fatalf("设杠杆带 kind: %v", body)
	}
	conformsRequest(t, "/v1/account/leverage", f.last(t).body)

	f.routes["/v1/account/agents"] = serve(t, 200, "w_agents_owner.json")
	aw, err := NewActionWriter(ctx, s.Client, signer(t, agentKey), AccountRef{Address: mustAddr(t, ownerAddr), Subaccount: 1}, NewNonceSource(0))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := aw.SignReplace(spot, []uint64{101}, nil, []OrderRequest{{Market: spot, Side: dexos.Buy, Price: 40000, Lots: 10, TIF: dexos.GTC}}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var rb map[string]any
	if err := json.Unmarshal(signed.Body, &rb); err != nil {
		t.Fatal(err)
	}
	if rb["kind"] != "spot" || m(rb["orders"].([]any)[0])["kind"] != "spot" {
		t.Fatalf("预签换单带 kind: %s", signed.Body)
	}
	f.answer(t, "/v1/orders/replace", 200, "w_replace.json")
	if _, err := s.Client.SendReplace(ctx, signed); err != nil {
		t.Fatalf("网关按 symbol + kind 重建的摘要应与签名一致: %v", err)
	}
	cancel, err := aw.SignCancel(spot, []uint64{101, 999999}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var cb []map[string]any
	if err := json.Unmarshal(cancel.Body, &cb); err != nil {
		t.Fatal(err)
	}
	if cb[0]["kind"] != "spot" || cb[1]["kind"] != "spot" {
		t.Fatalf("预签撤单各项带 kind: %s", cancel.Body)
	}

	p, err := NewPairedSession(ctx, s.Client, signer(t, agentKey), AccountRef{Address: mustAddr(t, ownerAddr), Subaccount: 1}, AccountRef{Address: mustAddr(t, ownerAddr), Subaccount: 2})
	if err != nil {
		t.Fatal(err)
	}
	paired := []byte(`{"status":"filled","symbol":"BTC-USD","kind":"perp","price":"65000","size":"0.01","makerOrderId":"301","takerOrderId":"302","fills":[],"avgPrice":null,"seq":510,"sub":0,"requestId":"` + testReqID + `"}`)
	f.answerRaw(t, "/v1/orders/paired", 200, paired)
	pr, err := p.Trade(ctx, PairedTrade{Market: perp, MakerSide: dexos.Buy, Price: 65000, Lots: 10})
	if err != nil {
		t.Fatal(err)
	}
	if body := m(f.last(t).body); body["kind"] != "perp" || pr.Kind != KindPerp || pr.Lots != 10 {
		t.Fatalf("成对成交带 kind: %v / %+v", body, pr)
	}
	conformsRequest(t, "/v1/orders/paired", f.last(t).body)
}
