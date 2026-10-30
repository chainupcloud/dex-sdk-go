package dexosv1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

const (
	testAddr  = "0x1111111111111111111111111111111111111111"
	testAgent = "0x2222222222222222222222222222222222222222"
	testReqID = "0xabababababababababababababababababababababababababababababababab"
)

// fakeV1 假 dex-os:按「路径 → 处理函数」应答,记下每个请求。
type fakeV1 struct {
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	reqs   []*http.Request
}

func newFake(t *testing.T) (*fakeV1, *Client) {
	t.Helper()
	f := &fakeV1{routes: map[string]http.HandlerFunc{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Clone(context.Background()))
		h, ok := f.routes[r.URL.Path]
		f.mu.Unlock()
		if !ok {
			t.Errorf("意外的请求 %s", r.URL)
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, NewClient(srv.URL)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// serve 固定回一个 fixture。
func serve(t *testing.T, status int, name string, headers ...string) http.HandlerFunc {
	body := fixture(t, name)
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

func (f *fakeV1) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.reqs...)
}

func mustAddr(t *testing.T, s string) dexos.Address {
	t.Helper()
	a, err := dexos.ParseAddress(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func catalog(t *testing.T, f *fakeV1, c *Client) *Catalog {
	t.Helper()
	f.routes["/v1/markets"] = serve(t, 200, "markets.json")
	cat, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func TestCatalogAndBookScaleToTicksAndLots(t *testing.T) {
	f, c := newFake(t)
	cat := catalog(t, f, c)
	btc, err := cat.Market("btc-usd")
	if err != nil {
		t.Fatal(err)
	}
	if btc.Scale != (Scale{PriceDecimals: 0, SizeDecimals: 3}) || btc.LastPrice != 83952 || *btc.MarkPrice != 82680 ||
		btc.BestBid != nil || *btc.OpenInterest != 184 || *btc.MaxLeverage != 20 || btc.SigningMarketID != 0 ||
		btc.QuotePerTickLot != 1000 || btc.StrictIsolated || btc.BaseToken != nil || btc.QuoteToken != nil || btc.BasePerLot != nil {
		t.Fatalf("BTC-USD 换算不对: %+v", btc)
	}
	eth, _ := cat.Market("ETH-USD")
	if *eth.BestBid != 24926 || *eth.BestAsk != 24934 || *eth.MarkPrice != 24931 || *eth.OpenInterest != 1250 || eth.MinNotional != "10" ||
		eth.MarginMode != "isolated" || !eth.StrictIsolated {
		t.Fatalf("ETH-USD 换算不对: %+v", eth)
	}
	// 现货:base / quote 的 token 号与每手的 base 原子单位;没有杠杆、标记价、持仓量
	spot, _ := cat.Market("BTC-USDC")
	if spot.Kind != "spot" || *spot.BaseToken != 1 || *spot.QuoteToken != 0 || *spot.BasePerLot != 100000 || spot.QuotePerTickLot != 1000 ||
		spot.MaxLeverage != nil || spot.MarkPrice != nil || spot.OpenInterest != nil || spot.FundingRate != nil || *spot.BestBid != 83890 {
		t.Fatalf("BTC-USDC 换算不对: %+v", spot)
	}
	if _, err := cat.Market("SOL-USD"); err == nil {
		t.Fatal("目录外的符号应当报错")
	}
	// 盘口聚合档位原样带过来(tick 数);null = 该市场未配置
	if !slices.Equal(btc.AggTicks, []uint64{1, 10, 100, 1000}) || eth.AggTicks != nil ||
		!slices.Equal(spot.AggTicks, []uint64{10, 100}) {
		t.Fatalf("聚合档位不对: %v %v %v", btc.AggTicks, eth.AggTicks, spot.AggTicks)
	}

	f.routes["/v1/markets/ETH-USD/book"] = serve(t, 200, "book.json")
	b, err := c.Book(context.Background(), eth, 2)
	if err != nil {
		t.Fatal(err)
	}
	if b.Seq != 231461888 || b.Depth != 2 || len(b.Bids) != 2 || b.Bids[0] != (Level{24926, 300}) || b.Asks[1] != (Level{24936, 2}) {
		t.Fatalf("盘口换算不对: %+v", b)
	}
	if got := f.requests()[1].URL.Query().Get("depth"); got != "2" {
		t.Fatalf("depth 没带上: %q", got)
	}
}

// 价格没对齐 tick、整数字段不是整数:响应不能被悄悄舍入或截断,整份目录报错。
func TestCatalogRejectsMisalignedPrice(t *testing.T) {
	for _, c := range []struct{ from, to, want string }{
		{`"lastPrice":"2493"`, `"lastPrice":"2493.05"`, "lastPrice"},
		{`"quotePerTickLot":"1000"`, `"quotePerTickLot":"1000.5"`, "quotePerTickLot"},
		{`"basePerLot":"100000"`, `"basePerLot":"-1"`, "basePerLot"},
	} {
		f, cl := newFake(t)
		body := strings.Replace(string(fixture(t, "markets.json")), c.from, c.to, 1)
		if body == string(fixture(t, "markets.json")) {
			t.Fatalf("fixture 里没有 %s", c.from)
		}
		f.routes["/v1/markets"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
		if _, err := cl.Catalog(context.Background()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s 应当报错,得到 %v", c.to, err)
		}
	}
}

// dex-os 下市后名称释放、可再上同名新市场（dex-os docs/deploy.md §6i，v_market_meta v2）：目录里同一符号可以有一个 Delisted
// 的旧市场和一个在用的新市场。按符号只认在用的那个，旧市场仍在 Markets() 里；两个在用的市场同名才是目录错误。
func TestCatalogResolvesRelistedSymbolToLiveMarket(t *testing.T) {
	relisted := func(status string) string {
		var doc struct {
			Markets []map[string]any `json:"markets"`
		}
		if err := json.Unmarshal(fixture(t, "markets.json"), &doc); err != nil {
			t.Fatal(err)
		}
		var old map[string]any
		for _, m := range doc.Markets {
			if m["symbol"] == "BTC-USD" {
				old = m
			}
		}
		if old == nil {
			t.Fatal("fixture 里没有 BTC-USD")
		}
		live := map[string]any{}
		for k, v := range old {
			live[k] = v
		}
		old["status"] = status
		live["signingMarketId"] = 8
		doc.Markets = append(doc.Markets, live)
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	f, c := newFake(t)
	body := relisted("Delisted")
	f.routes["/v1/markets"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
	cat, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatalf("下市后同名再上的目录应能读: %v", err)
	}
	btc, err := cat.Market("btc-usd")
	if err != nil || btc.SigningMarketID != 8 || btc.Status == "Delisted" {
		t.Fatalf("按符号应取在用的新市场 8: %+v %v", btc, err)
	}
	delisted := 0
	for _, m := range cat.Markets() {
		if m.Symbol == "BTC-USD" && m.Status == "Delisted" {
			delisted++
		}
	}
	if delisted != 1 {
		t.Fatalf("下市的旧市场仍应在 Markets() 里: %d", delisted)
	}

	body = relisted("Active")
	if _, err := c.Catalog(context.Background()); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("两个在用的市场同名应报目录错误: %v", err)
	}
}

// 整表行（/v1/prices、/v1/tickers，WS prices 帧同形）列出全部市场、只带符号：同名的已下市旧市场与在用新市场各占一行，按目录里
// 同名市场的先后逐个对上，行上带出市场号；只剩已下市的名称也照常对上，不报目录外符号（dex-os public-api-v1-1 §4.4）。
// 10-29 dex-01：demo:BTC-USD 的 1 号下市、8 号上线，/v1/prices 里两行 demo:BTC-USD，平台盘 BTC-USD 只剩下市的 0 号。
func TestListRowsFollowSameNameMarketsInOrder(t *testing.T) {
	var markets struct {
		Markets []map[string]any `json:"markets"`
	}
	if err := json.Unmarshal(fixture(t, "markets.json"), &markets); err != nil {
		t.Fatal(err)
	}
	var live map[string]any
	for _, m := range markets.Markets {
		if m["symbol"] == "BTC-USD" {
			live = map[string]any{}
			for k, v := range m {
				live[k] = v
			}
			m["status"] = "Delisted"
		}
	}
	live["signingMarketId"] = 8
	markets.Markets = append(markets.Markets, live)
	relisted := func(name, key string, last map[string]any) string {
		var doc map[string]any
		if err := json.Unmarshal(fixture(t, name), &doc); err != nil {
			t.Fatal(err)
		}
		rows := doc[key].([]any)
		var btc map[string]any
		for _, r := range rows {
			if r.(map[string]any)["symbol"] == "BTC-USD" {
				btc = map[string]any{}
				for k, v := range r.(map[string]any) {
					btc[k] = v
				}
			}
		}
		for k, v := range last {
			btc[k] = v
		}
		doc[key] = append(rows, btc)
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	body, err := json.Marshal(markets)
	if err != nil {
		t.Fatal(err)
	}
	f, c := newFake(t)
	f.routes["/v1/markets"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }
	ctx := context.Background()
	cat, err := c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}

	prices := relisted("prices.json", "prices", map[string]any{"mark": "83000"})
	f.routes["/v1/prices"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(prices)) }
	ps, err := c.Prices(ctx, cat)
	if err != nil {
		t.Fatalf("同名两行的价格表应能读: %v", err)
	}
	var ids []uint16
	for _, p := range ps.Prices {
		ids = append(ids, p.SigningMarketID)
	}
	if !slices.Equal(ids, []uint16{0, 1, 8}) || *ps.Prices[0].Mark != 82680 || *ps.Prices[2].Mark != 83000 {
		t.Fatalf("价格行应按同名市场的先后对到 0 与 8: %v %+v", ids, ps.Prices)
	}

	tickers := relisted("tickers.json", "tickers", map[string]any{"trades": 7})
	f.routes["/v1/tickers"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tickers)) }
	ts, err := c.Tickers(ctx, cat)
	if err != nil {
		t.Fatalf("同名两行的成交统计应能读: %v", err)
	}
	if ts.Tickers[0].SigningMarketID != 0 || ts.Tickers[0].Trades != 42 || ts.Tickers[3].SigningMarketID != 8 || ts.Tickers[3].Trades != 7 {
		t.Fatalf("成交统计行应按同名市场的先后对到 0 与 8: %+v", ts.Tickers)
	}

	// 价格表比目录多出一行同名（目录取得早，之后又上了同名市场）：报目录外符号，调用方重新取目录。
	var doc map[string]any
	if err := json.Unmarshal([]byte(prices), &doc); err != nil {
		t.Fatal(err)
	}
	rows := doc["prices"].([]any)
	doc["prices"] = append(rows, rows[len(rows)-1])
	extraBody, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	extra := string(extraBody)
	f.routes["/v1/prices"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(extra)) }
	if _, err := c.Prices(ctx, cat); err == nil || !strings.Contains(err.Error(), "重新取目录") {
		t.Fatalf("同名行多于目录里的同名市场应报错: %v", err)
	}
}

func TestAccountReads(t *testing.T) {
	f, c := newFake(t)
	cat := catalog(t, f, c)
	ref := AccountRef{Address: mustAddr(t, testAddr)}
	ctx := context.Background()

	f.routes["/v1/account"] = serve(t, 200, "account.json")
	a, err := c.Account(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if a.Equity != "13928.966644" || a.Balances[0].Amount != "-1282.814453" || a.FeeTier != nil || *a.TakerFee != "0.0005" ||
		a.Leverage[0] != (MarketLeverage{"ETH-USD", 10}) || a.Nonce != 2 {
		t.Fatalf("账户汇总不对: %+v", a)
	}
	if a.Epoch != "k1-1" || a.AsOfSeq != 231461888 || a.OpenOrderMargin != "12.5" || len(a.Groups) != 1 ||
		a.Groups[0] != (RiskGroup{"USDC", "-1282.814453", "13928.966644", "760.656", "456.3936", "12.5", "13168.310644"}) {
		t.Fatalf("账户风险头不对: %+v", a)
	}
	if a.Fees.Builder == nil || *a.Fees.Builder != 1 || len(a.Fees.Markets) != 2 ||
		a.Fees.Markets[0] != (AccountMarketFees{"BTC-USD", MakerFeeRates{"0", "0", "0"}, TakerFeeRates{"0.0005", "0.0005", "0", "0", "0"}, FeeSource{"tier", "account"}}) ||
		a.Fees.Markets[1].Maker != (MakerFeeRates{"0.00035", "0.00015", "0.0002"}) || a.Fees.Markets[1].Source != (FeeSource{"market", "default"}) {
		t.Fatalf("账户实收费率不对: %+v", a.Fees)
	}

	f.routes["/v1/account/positions"] = serve(t, 200, "positions.json")
	ps, err := c.Positions(ctx, cat, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Lots != 184 || ps[0].MarkPrice != 82680 || ps[0].EntryPrice != "83408.37" || ps[0].Side != "long" {
		t.Fatalf("持仓不对: %+v", ps)
	}

	f.routes["/v1/account/orders"] = serve(t, 200, "orders.json")
	orders, err := c.Orders(ctx, cat, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 3 || orders[0].OrderID != 2240412712968 || *orders[0].ClientOrderID != "mm-1" || orders[0].Price != 24905 ||
		orders[0].Lots != 50 || orders[0].FilledLots != 10 || orders[0].RemainingLots != 40 || orders[1].ClientOrderID != nil || orders[1].Price != 84000 {
		t.Fatalf("挂单不对: %+v", orders)
	}
	// 在簿挂单没有 tif / trigger;reduceOnly 台账里没有记录时为 null
	if orders[0].TIF != nil || orders[0].Trigger != nil || orders[0].ReduceOnly == nil || *orders[0].ReduceOnly || orders[1].ReduceOnly != nil {
		t.Fatalf("在簿挂单的 tif / reduceOnly / trigger 不对: %+v", orders[:2])
	}
	// 待触发的条件单(仓位止损):价量方向是子单的,触发价换成 tick
	if o := orders[2]; o.Trigger == nil || *o.Trigger != (OrderTrigger{Price: 80500, Above: false, PositionTpsl: true}) ||
		o.TIF == nil || *o.TIF != "gtc" || o.ReduceOnly == nil || !*o.ReduceOnly || o.Price != 80000 || o.Lots != 184 || o.Status != "open" {
		t.Fatalf("条件单不对: %+v", o)
	}

	f.routes["/v1/account/agents"] = serve(t, 200, "agents.json")
	ags, err := c.Agents(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(ags) != 2 || ags[0].Address != testAgent || ags[0].ValidUntil != nil || !ags[1].Expired {
		t.Fatalf("API 钱包列表不对: %+v", ags)
	}

	// 寻址参数:地址小写 + 子账户
	q := f.requests()[1].URL.Query()
	if q.Get("address") != testAddr || q.Get("subaccount") != "0" {
		t.Fatalf("账户寻址参数不对: %v", q)
	}
}

// 按市场读实收费率:带上 symbol,应答只能有这一个市场。
func TestAccountFeesIn(t *testing.T) {
	f, c := newFake(t)
	ref := AccountRef{Address: mustAddr(t, testAddr)}
	ctx := context.Background()
	f.routes["/v1/account"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fixture(t, map[string]string{"": "account_vault.json", "BTC-USD": "account_vault.json", "ETH-USD": "account.json"}[r.URL.Query().Get("symbol")]))
	}
	got, err := c.AccountFeesIn(ctx, ref, "BTC-USD")
	if err != nil {
		t.Fatal(err)
	}
	if got.Maker != (MakerFeeRates{"0", "0", "0"}) || got.Source != (FeeSource{"tier", "none"}) {
		t.Fatalf("BTC-USD 的实收费率不对: %+v", got)
	}
	if q := f.requests()[0].URL.Query(); q.Get("symbol") != "BTC-USD" || q.Get("address") != testAddr {
		t.Fatalf("按市场读费率的参数不对: %v", q)
	}
	if _, err := c.AccountFeesIn(ctx, ref, "ETH-USD"); err == nil {
		t.Fatal("应答不止一个市场应报错")
	}
}

// 缺 epoch / asOfSeq 的账户汇总不能用:判断不了纪元变没变、视图新不新。
func TestAccountRequiresEpochAndAsOfSeq(t *testing.T) {
	for _, field := range []string{`"epoch":"k1-1",`, `"asOfSeq":231461888,`} {
		f, c := newFake(t)
		body := strings.Replace(string(fixture(t, "account.json")), field, "", 1)
		if body == string(fixture(t, "account.json")) {
			t.Fatalf("fixture 里没有 %s", field)
		}
		f.routes["/v1/account"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
		if _, err := c.Account(context.Background(), AccountRef{Address: mustAddr(t, testAddr)}); err == nil {
			t.Fatalf("缺 %s 应当报错", field)
		}
	}
}

func TestMarketDataReads(t *testing.T) {
	f, c := newFake(t)
	cat := catalog(t, f, c)
	ctx := context.Background()
	btc, _ := cat.Market("BTC-USD")
	if btc.MarginMode != "cross" {
		t.Fatalf("marginMode: %q", btc.MarginMode)
	}

	f.routes["/v1/prices"] = serve(t, 200, "prices.json")
	ps, err := c.Prices(ctx, cat)
	if err != nil {
		t.Fatal(err)
	}
	b, e := ps.Prices[0], ps.Prices[1]
	if ps.Window == nil || ps.Window.From != 1791801480000 || b.Last != 83952 || *b.Mark != 82680 || *b.PrevDayPrice != 81000 ||
		*b.High24h != 84500 || *b.Low24h != 80500 || *b.Volume24h != 12345 || *b.Change24h != "0.036444" || *b.OpenInterest != 184 {
		t.Fatalf("BTC 24h 统计不对: %+v", b)
	}
	if *e.BestBid != 24926 || *e.Mark != 24931 || e.Oracle != nil || e.PrevDayPrice != nil || *e.Volume24h != 0 {
		t.Fatalf("ETH 价格不对: %+v", e)
	}
	// 资金费时间表:结算过的有指数与上次 / 下次时刻;还没结算过的时刻为 null
	if *b.FundingIndex != "-1250" || *b.LastFundingMs != 1791884400012 || *b.NextFundingMs != 1791888000012 ||
		*e.FundingIndex != "0" || e.LastFundingMs != nil || e.NextFundingMs != nil {
		t.Fatalf("资金费时间表不对: BTC %+v / ETH %+v", b, e)
	}

	f.routes["/v1/markets/BTC-USD/candles"] = serve(t, 200, "candles.json")
	ks, err := c.Candles(ctx, btc, CandleQuery{Interval: Interval1M, StartTime: 1788220800000, Limit: 12})
	if err != nil {
		t.Fatal(err)
	}
	if len(ks) != 1 || ks[0] != (Candle{Open: 1788220800000, Close: 1790812800000, Interval: "1M", O: 80000, H: 85000, L: 79000, C: 83952, Volume: 123456}) {
		t.Fatalf("K 线不对: %+v", ks)
	}
	q := f.requests()[len(f.requests())-1].URL.Query()
	if q.Get("interval") != "1M" || q.Get("startTime") != "1788220800000" || q.Get("limit") != "12" || q.Get("endTime") != "" {
		t.Fatalf("K 线参数不对: %v", q)
	}
	// 问 1M 答了别的周期:报错
	if _, err := c.Candles(ctx, btc, CandleQuery{Interval: Interval12h}); err == nil {
		t.Fatal("周期对不上应当报错")
	}
	if q := f.requests()[len(f.requests())-1].URL.Query(); q.Get("interval") != "12h" {
		t.Fatalf("12h 应当发出去: %v", q)
	}
	// 不在 v1 周期集合里:本地报错,不发请求
	before := len(f.requests())
	for _, bad := range []string{"", "1D", "3h", "1W"} {
		if _, err := c.Candles(ctx, btc, CandleQuery{Interval: bad}); err == nil {
			t.Fatalf("周期 %q 应当本地报错", bad)
		}
	}
	if len(f.requests()) != before {
		t.Fatal("未知周期不该发请求")
	}

	// 24 小时成交统计:价格换成 tick、量换成 lot;窗口里没有成交的市场价格为 nil
	f.routes["/v1/tickers"] = serve(t, 200, "tickers.json")
	tk, err := c.Tickers(ctx, cat)
	if err != nil {
		t.Fatal(err)
	}
	if tk.WindowStart != 1791801480000 || tk.WindowEnd != 1791887880000 || len(tk.Tickers) != 3 {
		t.Fatalf("tickers 不对: %+v", tk)
	}
	b0, e0 := tk.Tickers[0], tk.Tickers[1]
	if b0.Symbol != "BTC-USD" || *b0.Open != 81000 || *b0.High != 84500 || *b0.Low != 80500 || *b0.Last != 83952 || b0.Volume != 12345 ||
		b0.QuoteVolume != "1012345.5" || b0.Trades != 42 {
		t.Fatalf("BTC ticker 不对: %+v", b0)
	}
	if e0.Open != nil || e0.Last != nil || e0.Volume != 0 || e0.Trades != 0 {
		t.Fatalf("没有成交的 ticker 不对: %+v", e0)
	}
	if r := f.requests()[len(f.requests())-1]; r.URL.RawQuery != "" || r.Header.Get(minSeqHeader) != "" {
		t.Fatalf("tickers 不带参数、不带 min-seq: %v %v", r.URL, r.Header)
	}
}

func TestPortfolio(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	ref := AccountRef{Address: mustAddr(t, testAddr)}
	c.ObserveSeq(900)
	f.routes["/v1/account/portfolio"] = serve(t, 200, "portfolio.json")
	p, err := c.Portfolio(ctx, ref, Period7d)
	if err != nil {
		t.Fatal(err)
	}
	if p.Epoch != "k1-1" || p.BucketMs != 3600000 || *p.SeriesStartMs != 1791200000000 || len(p.Total.History) != 2 ||
		p.Total.History[1].PnL != "128.466644" || *p.Total.ReturnRate != "0.009309" || p.Perp.PnL != nil || len(p.Perp.History) != 0 ||
		p.UnvaluedTokens[0] != "XYZ" || p.Gaps[0] != (PortfolioGap{1791500000000, 1791600000000}) {
		t.Fatalf("portfolio 不对: %+v", p)
	}
	r := f.requests()[0]
	if r.URL.Query().Get("period") != "7d" || r.Header.Get(minSeqHeader) != "" {
		t.Fatalf("portfolio 参数不对(不带 min-seq): %v %v", r.URL.Query(), r.Header)
	}
	if _, err := c.Portfolio(ctx, ref, Period30d); err == nil {
		t.Fatal("问 30d 答 7d 应当报错")
	}
	before := len(f.requests())
	if _, err := c.Portfolio(ctx, ref, "1y"); err == nil || len(f.requests()) != before {
		t.Fatalf("未知区间应本地报错、不发请求: %v", err)
	}
}

func TestAgentGrantsAndMyVaults(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	f.routes["/v1/agents/"+testAgent] = serve(t, 200, "agent_grants.json")
	gs, err := c.AgentGrants(ctx, mustAddr(t, testAgent))
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 2 || gs[0].Address != testAddr || *gs[0].Subaccount != 0 || gs[0].Vault != nil || gs[0].ValidUntil != nil ||
		gs[1].Subaccount != nil || *gs[1].Vault != 3 || *gs[1].ValidUntil != 1791999999000 {
		t.Fatalf("授权反查不对: %+v", gs)
	}
	// 答的不是问的那把钥匙:报错
	f.routes["/v1/agents/"+testAddr] = serve(t, 200, "agent_grants.json")
	if _, err := c.AgentGrants(ctx, mustAddr(t, testAddr)); err == nil {
		t.Fatal("反查结果的 agent 对不上应当报错")
	}

	f.routes["/v1/account/vaults"] = serve(t, 200, "my_vaults.json")
	vs, err := c.MyVaults(ctx, mustAddr(t, testAddr))
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 || vs[0].Vault != 3 || vs[0].Class != "lp" || vs[0].Equity != "1012.345678" || vs[0].AllTimePnl != "12.345678" || !vs[0].Withdrawable.OK {
		t.Fatalf("金库头寸不对: %+v", vs)
	}
	if q := f.requests()[len(f.requests())-1].URL.Query(); q.Get("address") != testAddr || q.Has("subaccount") {
		t.Fatalf("金库头寸只按主钱包地址查: %v", q)
	}
}

func TestNonceScopes(t *testing.T) {
	f, c := newFake(t)
	ref := AccountRef{Address: mustAddr(t, testAddr)}
	agent := mustAddr(t, testAgent)
	ctx := context.Background()

	f.routes["/v1/account/nonce"] = serve(t, 200, "nonce.json")
	n, err := c.Nonce(ctx, ref, nil)
	if err != nil || n.Scope != "account" || n.Nonce != 2 || n.SigningAccountID != 7 {
		t.Fatalf("主钱包 nonce 不对: %+v, %v", n, err)
	}
	// 问 API 钱包的 nonce,服务端却答主钱包作用域:两个计数器混用就是 NonceMismatch,当场报错
	if _, err := c.Nonce(ctx, ref, &agent); err == nil {
		t.Fatal("作用域对不上应当报错")
	}
	f.routes["/v1/account/nonce"] = serve(t, 200, "nonce_agent.json")
	n, err = c.Nonce(ctx, ref, &agent)
	if err != nil || n.Scope != "agent" || n.Nonce != 41 {
		t.Fatalf("API 钱包 nonce 不对: %+v, %v", n, err)
	}
	if got := f.requests()[2].URL.Query().Get("signer"); got != testAgent {
		t.Fatalf("signer 没带上: %q", got)
	}
}

// 行情读不带 x-dexos-min-seq;账户读带上见过的最大位号。
func TestMinSeqOnlyOnAccountReads(t *testing.T) {
	f, c := newFake(t)
	f.routes["/v1/account"] = serve(t, 200, "account.json", "x-dexos-seq", "900")
	f.routes["/v1/markets"] = serve(t, 200, "markets.json")
	ctx := context.Background()
	ref := AccountRef{Address: mustAddr(t, testAddr)}
	if _, err := c.Account(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if c.Seq() != 900 {
		t.Fatalf("位号没采信: %d", c.Seq())
	}
	if _, err := c.Catalog(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Account(ctx, ref); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if reqs[0].Header.Get(minSeqHeader) != "" || reqs[1].Header.Get(minSeqHeader) != "" || reqs[2].Header.Get(minSeqHeader) != "900" {
		t.Fatalf("min-seq 头不对: %q %q %q", reqs[0].Header.Get(minSeqHeader), reqs[1].Header.Get(minSeqHeader), reqs[2].Header.Get(minSeqHeader))
	}
}

func TestFillsPageThroughCarriesEpochAndUpper(t *testing.T) {
	f, c := newFake(t)
	cat := catalog(t, f, c)
	page := 0
	f.routes["/v1/account/fills"] = func(w http.ResponseWriter, r *http.Request) {
		page++
		serve(t, 200, map[int]string{1: "fills_p1.json", 2: "fills_p2.json"}[page])(w, r)
	}
	h, err := c.Fills(context.Background(), cat, AccountRef{Address: mustAddr(t, testAddr)}, "", HistoryQuery{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if h.Epoch != "k1-1" || h.Upper != "231461888-65535-4294967295" || h.Truncated || len(h.Fills) != 2 {
		t.Fatalf("翻页结果不对: %+v", h)
	}
	f0, f1 := h.Fills[0], h.Fills[1]
	if f0.ID != "34186497-0-1" || *f0.OrderID != 2240412712968 || f0.Price != 82359 || f0.Lots != 2 || *f0.TS != 1791600487016 {
		t.Fatalf("第一笔不对: %+v", f0)
	}
	if f1.OrderID != nil || f1.Price != 24926 || f1.Lots != 10 || f1.Fee != "-0.0249" || f1.TS != nil {
		t.Fatalf("第二笔不对: %+v", f1)
	}
	reqs := f.requests()[1:]
	q1, q2 := reqs[0].URL.Query(), reqs[1].URL.Query()
	if q1.Get("after") != "" || q1.Get("epoch") != "" || q1.Get("upper") != "" || q1.Get("limit") != "1" {
		t.Fatalf("首页参数不对: %v", q1)
	}
	if q2.Get("after") != "34186497-0-1" || q2.Get("epoch") != "k1-1" || q2.Get("upper") != "231461888-65535-4294967295" {
		t.Fatalf("续页必须带回首页的 epoch 与 upper: %v", q2)
	}
}

func TestHistoryResumeRules(t *testing.T) {
	f, c := newFake(t)
	cat := catalog(t, f, c)
	ref := AccountRef{Address: mustAddr(t, testAddr)}
	ctx := context.Background()

	// 游标没有纪元:不发请求,直接报错
	if _, err := c.Fills(ctx, cat, ref, "", HistoryQuery{After: "1-0-0"}); err == nil {
		t.Fatal("从游标续拉却不给纪元应当报错")
	}

	// 给了游标与纪元、没给上界:先不带游标探一页取上界,再从游标续拉
	f.routes["/v1/account/fills"] = serve(t, 200, "fills_p2.json")
	before := len(f.requests())
	if _, err := c.Fills(ctx, cat, ref, "", HistoryQuery{After: "34186497-0-1", Epoch: "k1-1"}); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()[before:]
	if len(reqs) != 2 {
		t.Fatalf("应当探一页再续拉,实际 %d 个请求", len(reqs))
	}
	probe, resume := reqs[0].URL.Query(), reqs[1].URL.Query()
	if probe.Get("after") != "" || probe.Get("limit") != "1" || probe.Get("epoch") != "k1-1" {
		t.Fatalf("探测页参数不对: %v", probe)
	}
	if resume.Get("after") != "34186497-0-1" || resume.Get("upper") != "231461888-65535-4294967295" {
		t.Fatalf("续拉参数不对: %v", resume)
	}

	// 服务端说还有更多、游标却不前进:报错,不空转
	f.routes["/v1/account/fills"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"epoch":"k1-1","upper":"9-0-0","fills":[],"hasMore":true,"next":"` + r.URL.Query().Get("after") + `"}`))
	}
	if _, err := c.Fills(ctx, cat, ref, "", HistoryQuery{After: "5-0-0", Epoch: "k1-1", Upper: "9-0-0"}); err == nil ||
		!strings.Contains(err.Error(), "游标没有前进") {
		t.Fatalf("游标不前进应当报错,得到 %v", err)
	}
}

func TestHistoryLimitTruncates(t *testing.T) {
	f, c := newFake(t)
	cat := catalog(t, f, c)
	f.routes["/v1/account/fills"] = serve(t, 200, "fills_p1.json")
	h, err := c.Fills(context.Background(), cat, AccountRef{Address: mustAddr(t, testAddr)}, "", HistoryQuery{Limit: 1})
	if err != nil || len(h.Fills) != 1 || !h.Truncated {
		t.Fatalf("Limit 截断应当标 Truncated: %+v, %v", h, err)
	}
}

func TestLedgerFilters(t *testing.T) {
	f, c := newFake(t)
	f.routes["/v1/account/ledger"] = serve(t, 200, "ledger.json")
	h, err := c.Ledger(context.Background(), AccountRef{Address: mustAddr(t, testAddr), Subaccount: 2},
		LedgerQuery{Kind: "deposit", Token: "USDC"})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Entries) != 2 || h.Entries[0].Kind != "deposit" || h.Entries[0].Symbol != nil || *h.Entries[1].Symbol != "BTC-USD" || h.Entries[1].Amount != "-164.718" {
		t.Fatalf("流水不对: %+v", h)
	}
	q := f.requests()[0].URL.Query()
	if q.Get("kind") != "deposit" || q.Get("token") != "USDC" || q.Get("subaccount") != "2" || q.Has("external") {
		t.Fatalf("过滤参数不对: %v", q)
	}
	if _, err := c.Ledger(context.Background(), AccountRef{Address: mustAddr(t, testAddr)}, LedgerQuery{External: true}); err != nil {
		t.Fatal(err)
	}
	if q = f.requests()[1].URL.Query(); q.Get("external") != "true" || q.Has("kind") {
		t.Fatalf("external 过滤参数不对: %v", q)
	}
	// external 与 kind 同给:服务端 400,本地先拦住
	if _, err := c.Ledger(context.Background(), AccountRef{Address: mustAddr(t, testAddr)}, LedgerQuery{Kind: "deposit", External: true}); err == nil || len(f.requests()) != 2 {
		t.Fatalf("external 与 kind 同给应本地报错: %v", err)
	}
}

// 持仓 size 带符号(多为正、空为负),换成带符号的 lot;side 与符号对不上就报错。
func TestPositionsSignedSize(t *testing.T) {
	f, c := newFake(t)
	cat := catalog(t, f, c)
	ref := AccountRef{Address: mustAddr(t, testAddr)}
	body := func(rows string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"address":"` + testAddr + `","subaccount":0,"positions":[` + rows + `]}`))
		}
	}
	mark := map[string]string{"BTC-USD": "82680", "ETH-USD": "2501.9"}
	row := func(symbol, side, size string) string {
		return `{"symbol":"` + symbol + `","side":"` + side + `","size":"` + size + `","entryPrice":"2509.3","markPrice":"` + mark[symbol] + `",` +
			`"notional":"1250.95","unrealizedPnl":"3.7","fundingIndex":"0"}`
	}
	f.routes["/v1/account/positions"] = body(row("BTC-USD", "long", "0.184") + "," + row("ETH-USD", "short", "-0.5"))
	ps, err := c.Positions(context.Background(), cat, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].Lots != 184 || ps[0].Side != "long" || ps[1].Lots != -50 || ps[1].Side != "short" {
		t.Fatalf("带符号持仓不对: %+v", ps)
	}
	for _, bad := range []string{row("ETH-USD", "long", "-0.5"), row("ETH-USD", "short", "0.5"), row("ETH-USD", "flat", "0.5")} {
		f.routes["/v1/account/positions"] = body(bad)
		if _, err := c.Positions(context.Background(), cat, ref); err == nil {
			t.Fatalf("side 与 size 符号不一致要报错: %s", bad)
		}
	}
}

// 响应里出现目录外的符号:不猜精度,报错。
func TestUnknownSymbolInAccountResponse(t *testing.T) {
	f, c := newFake(t)
	cat := catalog(t, f, c)
	body := strings.ReplaceAll(string(fixture(t, "positions.json")), "BTC-USD", "market-9")
	f.routes["/v1/account/positions"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
	if _, err := c.Positions(context.Background(), cat, AccountRef{Address: mustAddr(t, testAddr)}); err == nil {
		t.Fatal("目录外的符号应当报错")
	}
}

func TestTypedErrors(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	ref := AccountRef{Address: mustAddr(t, testAddr)}

	f.routes["/v1/markets/NOPE-USD"] = serve(t, 404, "err404.json")
	_, err := c.Market(ctx, "NOPE-USD")
	var e *Error
	if !errors.As(err, &e) || e.Status != 404 || e.Code != "GatewayNotFound" || e.Num != 10404 || string(e.Details["symbol"]) != `"NOPE-USD"` {
		t.Fatalf("404 应解析成 *Error: %#v", err)
	}

	f.routes["/v1/account"] = serve(t, 429, "err429.json", "retry-after", "7", "ratelimit", "limit=1200, remaining=0, reset=42")
	_, err = c.Account(ctx, ref)
	var rl *RateLimitedError
	if !errors.As(err, &rl) || rl.Bucket != "ip" || rl.RetryAfter != 7*time.Second || rl.Limit != 1200 || rl.Remaining != 0 || rl.Reset != 42*time.Second {
		t.Fatalf("429 应解析成 *RateLimitedError: %#v", err)
	}
	if !errors.As(err, &e) || e.Code != "GatewayRateLimited" {
		t.Fatalf("*RateLimitedError 应能展开成 *Error: %#v", err)
	}
	// 不是网关限流的形状(没 bucket、没 retry-after、没扣光那个桶的读数头):不当成限流,但仍能取到 429
	ipBody := string(fixture(t, "err429.json"))
	noBucket := `{"error":{"code":"GatewayRateLimited","num":10429,"message":"rate limit exceeded"}}`
	for name, tc := range map[string]struct {
		body string
		h    []string
	}{
		"缺 bucket":      {noBucket, []string{"retry-after", "7", "ratelimit", "limit=1200, remaining=0, reset=42"}},
		"缺 retry-after": {ipBody, []string{"ratelimit", "limit=1200, remaining=0, reset=42"}},
		"缺 ratelimit":   {ipBody, []string{"retry-after", "7"}},
		"读数头取错桶":        {ipBody, []string{"retry-after", "7", "x-ratelimit-account", "limit=1200, remaining=0, reset=42"}},
	} {
		f.routes["/v1/account"] = func(w http.ResponseWriter, _ *http.Request) {
			for i := 0; i+1 < len(tc.h); i += 2 {
				w.Header().Set(tc.h[i], tc.h[i+1])
			}
			w.WriteHeader(429)
			_, _ = w.Write([]byte(tc.body))
		}
		_, err = c.Account(ctx, ref)
		if errors.As(err, &rl) || !errors.As(err, &e) || e.Status != 429 {
			t.Fatalf("%s:不该解析成 *RateLimitedError,但要能取到 *Error: %#v", name, err)
		}
	}

	f.routes["/v1/account/ledger"] = serve(t, 409, "err409_epoch.json")
	if _, err = c.Ledger(ctx, ref, LedgerQuery{}); !errors.Is(err, dexos.ErrHistoryEpochMismatch) {
		t.Fatalf("纪元不符应当 errors.Is ErrHistoryEpochMismatch: %v", err)
	}

	// 补拉上界越过完整前缀:按稳定错误码判,不看 message
	f.routes["/v1/account/ledger"] = serve(t, 400, "err400_incomplete.json")
	if _, err = c.Ledger(ctx, ref, LedgerQuery{}); !errors.Is(err, dexos.ErrHistoryUpperIncomplete) {
		t.Fatalf("GatewayHistoryIncomplete 应当 errors.Is ErrHistoryUpperIncomplete: %v", err)
	}
	f.routes["/v1/account/ledger"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"code":"GatewayBadRequest","num":10400,"message":"upper is not yet complete"}}`))
	}
	if _, err = c.Ledger(ctx, ref, LedgerQuery{}); errors.Is(err, dexos.ErrHistoryUpperIncomplete) {
		t.Fatalf("别的 400 不能因为 message 像就当成上界未完整: %v", err)
	}

	f.routes["/v1/account/ledger"] = serve(t, 503, "err503_gaps.json")
	_, err = c.Ledger(ctx, ref, LedgerQuery{})
	var hu *HistoryUnavailableError
	if !errors.As(err, &hu) || len(hu.Gaps) != 1 || hu.Gaps[0] != (dexos.Gap{First: 100, Last: 200}) {
		t.Fatalf("缺口应解析成 *HistoryUnavailableError: %#v", err)
	}

	// 反代直接回的非 JSON 错误:只有状态码与原文
	f.routes["/v1/exchange"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	}
	_, err = c.Exchange(ctx)
	if !errors.As(err, &e) || e.Status != 502 || e.Code != "" || !strings.Contains(e.Body, "bad gateway") {
		t.Fatalf("非 JSON 错误体应保留原文: %#v", err)
	}
}

func TestRequestReceipt(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()

	f.routes["/v1/requests/"+testReqID] = serve(t, 200, "request_executed.json")
	r, err := c.Request(ctx, testReqID, RequestQuery{})
	if err != nil || r.Status != dexos.ReceiptExecuted || *r.Seq != 34186497 || *r.Sub != 0 || r.Epoch != "k1-1" || len(r.Raw) == 0 {
		t.Fatalf("executed 回执不对: %+v, %v", r, err)
	}

	// not_found 装在 404 错误体里,它是答案不是错误
	f.routes["/v1/requests/"+testReqID] = serve(t, 404, "request_not_found.json")
	nonce := uint64(5)
	r, err = c.Request(ctx, testReqID, RequestQuery{Scope: "agent:" + testAgent, Nonce: &nonce})
	if err != nil || r.Status != dexos.ReceiptNotFound || *r.AsOfSeq != 231461888 {
		t.Fatalf("not_found 回执不对: %+v, %v", r, err)
	}
	q := f.requests()[1].URL.Query()
	if q.Get("scope") != "agent:"+testAgent || q.Get("nonce") != "5" {
		t.Fatalf("nonce 冲突检查参数不对: %v", q)
	}

	// 404 但不是回执(坏路径之类):照常是错误
	f.routes["/v1/requests/"+testReqID] = serve(t, 404, "err404.json")
	if _, err = c.Request(ctx, testReqID, RequestQuery{}); err == nil {
		t.Fatal("不是回执的 404 应当是错误")
	}

	// 本地就能判的错:不发请求
	before := len(f.requests())
	for _, bad := range []struct {
		id string
		q  RequestQuery
	}{
		{"0x12", RequestQuery{}},
		{testReqID, RequestQuery{Scope: "agent:" + testAgent}},
		{testReqID, RequestQuery{Scope: "account:7", Nonce: &nonce, CommandHash: testReqID}},
	} {
		if _, err := c.Request(ctx, bad.id, bad.q); err == nil {
			t.Fatalf("应当本地报错: %+v", bad)
		}
	}
	if len(f.requests()) != before {
		t.Fatal("本地能判的错不该发请求")
	}
}
