//go:build dexos_contract

package dexosv1

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 契约测试(WS):对一台真实 dex-os 订阅全部公共与私有频道,等每个订阅就绪(快照 / 补拉),再用两个
// **测试账户**做一笔真实成交 —— maker 挂最优买价的只挂单,taker 用 IOC 卖单吃掉它 —— 核对双方的
// orders / fills 帧、maker 的 positions / account 帧与公共 trades 帧都收到且字段对得上。帧结构靠本包的
// 类型化解码校验:解不开的帧会让订阅带着原因进 Syncing,这里一律判失败。默认不编译,要跑:
//
//	DEXOS_V1_URL=http://127.0.0.1:17807 DEXOS_V1_WRITE_KEY=0x… DEXOS_V1_TAKER_KEY=0x… \
//	  go test -tags dexos_contract -run ContractStream ./dexosv1/
//
// 两把私钥都必须是测试账户的主钱包(例如本地栈的 anvil 测试账户),账户里要有 USDC。成交开出的两笔
// 最小仓位在结束时用一笔反向成交(maker 挂卖、taker IOC 买)平掉,账户里原有的仓位不动。
// DEXOS_V1_WRITE_SYMBOL 可选,缺省用目录里第一个有标记价的永续;盘口价差只有一档时挂不出最优价,换个市场。
func TestContractStream(t *testing.T) {
	base, makerKey, takerKey := os.Getenv("DEXOS_V1_URL"), os.Getenv("DEXOS_V1_WRITE_KEY"), os.Getenv("DEXOS_V1_TAKER_KEY")
	if base == "" || makerKey == "" || takerKey == "" {
		t.Skip("没给 DEXOS_V1_URL / DEXOS_V1_WRITE_KEY / DEXOS_V1_TAKER_KEY")
	}
	c := NewClient(strings.TrimRight(base, "/"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cat, err := c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m *Market
	if sym := os.Getenv("DEXOS_V1_WRITE_SYMBOL"); sym != "" {
		if m, err = cat.Market(sym); err != nil {
			t.Fatal(err)
		}
	} else {
		for i := range cat.Markets() {
			if mk := &cat.Markets()[i]; mk.Kind == "perp" && mk.MarkPrice != nil && *mk.MarkPrice > 1 {
				m = mk
				break
			}
		}
	}
	if m == nil || m.MarkPrice == nil {
		t.Fatal("找不到有标记价的市场")
	}
	wallet := func(key string) *Wallet {
		s, err := dexos.NewSigner(key)
		if err != nil {
			t.Fatal(err)
		}
		w, err := NewWallet(ctx, c, s, 0)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	mw, tw := wallet(makerKey), wallet(takerKey)
	if mw.Account == tw.Account {
		t.Fatal("maker 与 taker 要是两个账户")
	}
	makerHead, err := c.HistoryHead(ctx, mw.Account)
	if err != nil {
		t.Fatal(err)
	}
	takerHead, err := c.HistoryHead(ctx, tw.Account)
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu     sync.Mutex
		failed []StateEvent
	)
	st, err := NewStream(c, cat, StreamOptions{
		OnState: func(e StateEvent) {
			t.Logf("订阅 %s %v → %s(%v)", e.Sub.Channel(), e.Sub.Params(), e.State, e.Err)
			// 服务端丢帧是负载问题,不是帧结构问题;其余带原因的状态变化(解不开的帧、订阅被拒、补拉失败)都算失败
			var lag *LaggedError
			var gap *TradesGapError
			if e.Err != nil && !errors.As(e.Err, &lag) && !errors.As(e.Err, &gap) {
				mu.Lock()
				failed = append(failed, e)
				mu.Unlock()
			}
		},
		OnConnError: func(err error) { t.Errorf("连接错误: %v", err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	// 回调在 Run 的 goroutine 里,不能阻塞:满了就丢(只核对「收到过」)
	books, bbos, trades := make(chan *Book, 256), make(chan BBO, 256), make(chan Trade, 256)
	prices, accounts, positions := make(chan *Prices, 256), make(chan *Account, 256), make(chan []Position, 256)
	makerOrders, takerOrders := make(chan OrdersUpdate, 256), make(chan OrdersUpdate, 256)
	makerFills, takerFills := make(chan Fill, 256), make(chan Fill, 256)
	ledger, takerLedger := make(chan LedgerEntry, 256), make(chan LedgerEntry, 256)
	var subs []*Subscription
	must := func(s *Subscription, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, s)
	}
	must(st.SubscribeBook(m.Symbol, 10, func(b *Book) { put(books, b) }))
	must(st.SubscribeBBO(m.Symbol, func(b BBO) { put(bbos, b) }))
	must(st.SubscribeTrades(m.Symbol, func(tr Trade) { put(trades, tr) }))
	must(st.SubscribePrices(func(p *Prices) { put(prices, p) }))
	must(st.SubscribeAccount(mw.Account, func(a *Account) { put(accounts, a) }))
	must(st.SubscribePositions(mw.Account, func(p []Position) { put(positions, p) }))
	must(st.SubscribeOrders(mw.Account, func(u OrdersUpdate) { put(makerOrders, u) }))
	must(st.SubscribeOrders(tw.Account, func(u OrdersUpdate) { put(takerOrders, u) }))
	mf, err := st.SubscribeFills(mw.Account, makerHead, func(f Fill) { put(makerFills, f) })
	must(mf, err)
	must(st.SubscribeFills(tw.Account, takerHead, func(f Fill) { put(takerFills, f) }))
	must(st.SubscribeLedger(mw.Account, makerHead, func(e LedgerEntry) { put(ledger, e) }))
	must(st.SubscribeLedger(tw.Account, takerHead, func(e LedgerEntry) { put(takerLedger, e) }))
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- st.Run(runCtx) }()
	defer func() {
		stop()
		<-done
	}()

	// 全部就绪:快照类收到快照,流水类补拉到视图
	deadline := time.Now().Add(30 * time.Second)
	for _, s := range subs {
		for s.State() != StateLive {
			if time.Now().After(deadline) {
				t.Fatalf("%s %v 30s 内没有就绪(%s)", s.Channel(), s.Params(), s.State())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if b := wait(t, books, "book", func(b *Book) bool { return true }); b.Symbol != m.Symbol || len(b.Bids) > 10 || len(b.Asks) > 10 {
		t.Errorf("盘口帧: %s %d/%d 档", b.Symbol, len(b.Bids), len(b.Asks))
	}
	wait(t, bbos, "bbo", func(BBO) bool { return true })
	if p := wait(t, prices, "prices", func(*Prices) bool { return true }); len(p.Prices) == 0 {
		t.Error("prices 帧是空的")
	}
	if a := wait(t, accounts, "account", func(*Account) bool { return true }); a.Epoch != makerHead.Epoch {
		t.Errorf("account 帧纪元 %s,REST %s", a.Epoch, makerHead.Epoch)
	}
	wait(t, positions, "positions", func([]Position) bool { return true })
	wait(t, makerOrders, "maker orders 快照", func(u OrdersUpdate) bool { return u.Snapshot })
	wait(t, takerOrders, "taker orders 快照", func(u OrdersUpdate) bool { return u.Snapshot })

	// maker 挂最优买价的只挂单
	book, err := c.Book(ctx, m, 1)
	if err != nil {
		t.Fatal(err)
	}
	var price uint32
	switch {
	case len(book.Bids) == 0 && len(book.Asks) == 0:
		price = *m.MarkPrice
	case len(book.Bids) == 0:
		price = book.Asks[0].Price - 1
	case len(book.Asks) == 0 || book.Asks[0].Price-book.Bids[0].Price > 1:
		price = book.Bids[0].Price + 1
	default:
		t.Fatalf("%s 盘口价差只有一档,挂不出最优买价;用 DEXOS_V1_WRITE_SYMBOL 换个市场", m.Symbol)
	}
	minN, err := ParseFixed(m.MinNotional, m.Scale.PriceDecimals+m.Scale.SizeDecimals)
	if err != nil {
		t.Fatal(err)
	}
	lots := minN.Uint64()/uint64(price) + 1
	cid := fmt.Sprintf("sdk-ws-%d", time.Now().UnixMilli())
	t.Logf("市场 %s:maker 挂买 %s × %s(alo,%s),taker IOC 卖", m.Symbol, m.Scale.Price(price), m.Scale.Size(lots), cid)
	placed, err := mw.PlaceOrder(ctx, OrderRequest{Market: m, Side: dexos.Buy, Price: price, Lots: lots, TIF: dexos.PostOnly,
		ClientOrderID: cid}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("maker 挂单: %v", err)
	}
	if placed.Status != "resting" {
		t.Fatalf("maker 挂单没挂上: %+v", placed)
	}
	oid := placed.OrderID
	// 在挂单集合里出现,带 clientOrderId
	wait(t, makerOrders, "maker 挂单进集合", func(u OrdersUpdate) bool {
		for _, o := range u.Open {
			if o.OrderID == oid {
				return o.ClientOrderID != nil && *o.ClientOrderID == cid && o.Price == price && o.Lots == lots
			}
		}
		return false
	})

	took, err := tw.PlaceOrder(ctx, OrderRequest{Market: m, Side: dexos.Sell, Price: price, Lots: lots, TIF: dexos.IOC},
		time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("taker 吃单: %v", err)
	}
	t.Logf("taker 回执: %s 成交 %s", took.Status, m.Scale.Size(took.FilledLots))
	if took.FilledLots > 0 {
		defer unwind(t, c, m, mw, tw, took.FilledLots)
	}

	// maker:orders 的 fill 帧,之后单子离开在挂集合;fills 帧字段对得上
	sawFill := false
	wait(t, makerOrders, "maker 挂单成交后离开集合", func(u OrdersUpdate) bool {
		if u.Event != nil && u.Event.OrderID == oid && u.Event.Kind == "fill" {
			sawFill = true
		}
		for _, o := range u.Open {
			if o.OrderID == oid {
				return false
			}
		}
		return sawFill
	})
	f := wait(t, makerFills, "maker 成交", func(f Fill) bool { return f.OrderID != nil && *f.OrderID == oid })
	if f.Role != "maker" || f.Side != "buy" || f.Price != price || f.Symbol != m.Symbol || f.Lots == 0 {
		t.Errorf("maker 成交帧: %+v", f)
	}
	if got := mf.Cursor(); got.Epoch != makerHead.Epoch {
		t.Errorf("游标纪元 %+v", got)
	}
	// taker:orders 帧里有这张单的事件,fills 帧是吃单方
	wait(t, takerOrders, "taker 订单事件", func(u OrdersUpdate) bool { return u.Event != nil && u.Event.OrderID == took.OrderID })
	tf := wait(t, takerFills, "taker 成交", func(f Fill) bool { return f.OrderID != nil && *f.OrderID == took.OrderID })
	if tf.Role != "taker" || tf.Side != "sell" {
		t.Errorf("taker 成交帧: %+v", tf)
	}
	// taker 付了费就有这条命令的 taker_fee 流水,经 WS ledger 帧(entries[])推来
	if fee, err := ParseFixed(tf.Fee, 18); err != nil {
		t.Errorf("taker 成交费 %q: %v", tf.Fee, err)
	} else if fee.Sign() != 0 {
		e := wait(t, takerLedger, "taker 手续费流水", func(e LedgerEntry) bool { return e.Seq == tf.Seq && e.Kind == "taker_fee" })
		if e.Symbol == nil || *e.Symbol != m.Symbol || e.Token != tf.FeeToken {
			t.Errorf("taker 手续费流水: %+v", e)
		}
	} else {
		t.Log("taker 费率为 0,没有手续费流水可核对")
	}
	// 公共成交与 maker 的仓位 / 账户在块边界推出
	wait(t, trades, "公共成交", func(tr Trade) bool { return tr.Price == price && tr.Symbol == m.Symbol })
	wait(t, positions, "maker 仓位", func(ps []Position) bool {
		for _, p := range ps {
			if p.Symbol == m.Symbol {
				return true
			}
		}
		return false
	})
	wait(t, accounts, "maker 账户", func(*Account) bool { return true })
	// 资金流水:maker 费为 0 时这笔成交可能没有流水,只核对收到的条目解得开
	for len(ledger) > 0 {
		if e := <-ledger; e.ID == "" || e.Kind == "" || e.Token == "" {
			t.Errorf("流水帧: %+v", e)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for _, e := range failed {
		t.Errorf("订阅 %s %v 报错: %s %v", e.Sub.Channel(), e.Sub.Params(), e.State, e.Err)
	}
}

// unwind 用一笔反向成交平掉本测试开出的仓位:maker 挂最优卖价的只挂单,taker 用 IOC 买单吃掉。
func unwind(t *testing.T, c *Client, m *Market, mw, tw *Wallet, lots uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	book, err := c.Book(ctx, m, 1)
	if err != nil {
		t.Errorf("平仓读盘口: %v", err)
		return
	}
	mk, err := c.Market(ctx, m.Symbol)
	if err != nil {
		t.Errorf("平仓读标记价: %v", err)
		return
	}
	var price uint32
	switch {
	case len(book.Bids) == 0 && len(book.Asks) == 0 && mk.MarkPrice != nil:
		price = *mk.MarkPrice
	case len(book.Asks) == 0 && len(book.Bids) > 0:
		price = book.Bids[0].Price + 1
	case len(book.Asks) > 0 && (len(book.Bids) == 0 || book.Asks[0].Price-book.Bids[0].Price > 1):
		price = book.Asks[0].Price - 1
	default:
		t.Errorf("平仓:%s 盘口价差只有一档,挂不出最优卖价;maker 多、taker 空各剩 %s 要手工平", m.Symbol, m.Scale.Size(lots))
		return
	}
	sell, err := mw.PlaceOrder(ctx, OrderRequest{Market: m, Side: dexos.Sell, Price: price, Lots: lots, TIF: dexos.PostOnly},
		time.Now().Add(time.Minute))
	if err != nil || sell.Status != "resting" {
		t.Errorf("平仓 maker 挂卖: %+v %v", sell, err)
		return
	}
	buy, err := tw.PlaceOrder(ctx, OrderRequest{Market: m, Side: dexos.Buy, Price: price, Lots: lots, TIF: dexos.IOC},
		time.Now().Add(time.Minute))
	if err != nil || buy.FilledLots != lots {
		t.Errorf("平仓 taker 买: %+v %v", buy, err)
		if _, cerr := mw.Cancel(ctx, m, sell.OrderID); cerr != nil {
			t.Errorf("撤平仓挂单 %d: %v", sell.OrderID, cerr)
		}
		return
	}
	t.Logf("已平仓:%s × %s", m.Scale.Price(price), m.Scale.Size(lots))
}

func put[T any](ch chan T, v T) {
	select {
	case ch <- v:
	default:
	}
}

// wait 等 ch 里第一个满足 ok 的值,最多 20s。
func wait[T any](t *testing.T, ch chan T, what string, ok func(T) bool) T {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case v := <-ch:
			if ok(v) {
				return v
			}
		case <-deadline:
			t.Fatalf("20s 内没收到 %s", what)
			var zero T
			return zero
		}
	}
}
