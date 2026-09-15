// mmcheck 做市链路验收 —— 对着**真节点**跑,按 docs/MARKET-MAKING.md 的说法逐条压。
//
//	go run ./examples/mmcheck -url http://127.0.0.1:8080 -chain-id 31337 \
//	    -master-key 0x... -account 4 -iters 50
//
// 与 quickstart 的分工:那个验的是「签名路径通不通」(一次性生命周期),
// 这个验的是**持续报价**下才暴露的东西 —— 原子换单有没有中间窗口、
// 逐项对账对不对得上、nonce 连续推进几十轮会不会散、丢帧能不能恢复。
//
// 每条断言都同时检查「该发生什么」**和**「不该发生什么」。只查前者的话,
// 一个把 BatchReplace 实现成「先撤再挂两条命令」的服务端也能全绿通过 ——
// 而那正是做市商单边裸露的成因。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

type check struct {
	name string
	ok   bool
	note string
}

var checks []check

func report(name string, ok bool, format string, a ...any) {
	note := fmt.Sprintf(format, a...)
	checks = append(checks, check{name, ok, note})
	mark := "✗"
	if ok {
		mark = "✓"
	}
	fmt.Printf("  %s %s — %s\n", mark, name, note)
}

func main() {
	var (
		base      = flag.String("url", "http://127.0.0.1:8080", "网关地址")
		chainID   = flag.Uint64("chain-id", 31337, "EIP-712 域的 chainId")
		masterKey = flag.String("master-key", "", "主账号私钥(0x…)")
		account   = flag.Uint("account", 0, "内核账户号(master)")
		market    = flag.Uint("market", 0, "市场号")
		levels    = flag.Int("levels", 3, "每侧报价档数")
		lots      = flag.Uint64("lots", 5, "每档手数")
		iters     = flag.Int("iters", 50, "换单轮数")
		interval  = flag.Duration("interval", 200*time.Millisecond, "报价刷新间隔")
		takerKey  = flag.String("taker-key", "", "吃单方主账号私钥;给了才验成交与库存那半条链路")
		takerAcct = flag.Uint("taker-account", 0, "吃单方内核账户号")
	)
	flag.Parse()
	if *masterKey == "" {
		log.Fatal("必须给 -master-key")
	}
	ctx := context.Background()
	c := dexos.NewClient(*base, *chainID)
	mk := uint16(*market)
	acct := uint32(*account)

	v, err := c.Version(ctx)
	must(err, "读 /version")
	fmt.Printf("节点 pkg=%s snapshot=v%d codec=v%d · 账户 %d · 市场 %d\n\n",
		v.Pkg, v.SnapshotVer, v.CodecVer, acct, mk)

	owner, err := dexos.NewSigner(*masterKey)
	must(err, "解析主账号私钥")

	// ── 准备:授权 API 钱包 ──
	api, err := dexos.GenerateSigner()
	must(err, "生成 API 钱包")
	mn, err := c.NextNonce(ctx, acct)
	must(err, "读 master nonce")
	_, err = c.ApproveAgent(ctx, owner, acct, api.Address(), time.Now().Add(24*time.Hour), mn)
	must(err, "授权 API 钱包")
	fmt.Printf("API 钱包 %s 已授权\n\n", api.Address().Hex())

	nonce, err := c.AgentNonce(ctx, acct, api.Address())
	must(err, "读 agent nonce")

	// ── 事件流:做市必须先订阅再拉快照,否则中间的事件永远补不回来 ──
	st, err := c.Subscribe(ctx, dexos.WithMarkets(mk), dexos.WithAccounts(acct))
	must(err, "订阅事件流")
	evCount := map[string]int{}
	var lostFrames uint64
	go func() {
		for {
			select {
			case e, ok := <-st.Events:
				if !ok {
					return
				}
				evCount[e.Kind]++
			case n := <-st.Lost:
				lostFrames += n
			case <-st.Err:
				return
			}
		}
	}()

	// ── 1. 初始报价阶梯:挂进价差内侧,应当成为盘口最优 ──
	fmt.Println("── 报价上簿 ──")
	m0 := marketOf(ctx, c, mk)
	bid, ask := quotePrices(m0, *levels)
	orders := ladder(mk, bid, ask, *lots, *levels)
	ev, err := c.BatchPlace(ctx, api, acct, orders, nonce)
	must(err, "BatchPlace 初始阶梯")
	nonce++
	accepted := countKind(ev, "OrderAccepted")
	report("阶梯全部受理", accepted == len(orders),
		"提交 %d 张,OrderAccepted %d 条", len(orders), accepted)

	seqs, err := c.OrderSeqs(ctx, acct, mk)
	must(err, "读挂单序号")
	report("挂单在簿上", len(seqs) == len(orders),
		"节点侧挂单 %d 张", len(seqs))

	// 报价挂在价差内侧 → 盘口最优应当变成我们的
	m1 := marketOf(ctx, c, mk)
	report("成为盘口最优", m1.BestBid == bid && m1.BestAsk == ask,
		"盘口 bid=%d ask=%d,我们报 bid=%d ask=%d", m1.BestBid, m1.BestAsk, bid, ask)

	// ── 2. 持续原子换单 ──
	fmt.Printf("\n── 原子换单 ×%d(间隔 %s)──\n", *iters, *interval)
	var (
		lat          []time.Duration
		totalPlaced  int
		totalCancel  int
		emptyBookHit int // 换单后挂单数掉到 0 的次数 —— 中间窗口的证据
		shortRounds  int // 挂上的少于提交的轮数(逐项尽力而为,允许但要记账)
		fills        int
		rejects      int
	)
	mine := priceSet(orders)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for i := 0; i < *iters; i++ {
		<-ticker.C
		b, err := c.Book(ctx, mk, 10)
		must(err, "读盘口")
		nb, na, ok := quotePricesAround(b, mine, *levels)
		if !ok {
			continue // 簿的一侧空了,这轮不报
		}
		newOrders := ladder(mk, nb, na, *lots, *levels)

		t0 := time.Now()
		ev, err := c.BatchReplace(ctx, api, acct, mk, seqs, newOrders, nonce)
		d := time.Since(t0)
		if err != nil {
			rejects++
			log.Fatalf("换单结果需要核对，停止示例；不得换 nonce 重发 (nonce=%d): %v", nonce, err)
		}
		nonce++
		lat = append(lat, d)
		totalCancel += countKind(ev, "OrderCanceled")
		p := countKind(ev, "OrderAccepted")
		totalPlaced += p
		fills += countKind(ev, "Fill")
		if p < len(newOrders) {
			shortRounds++
			if shortRounds <= 2 { // 头两次把原始回执打出来,别猜拒因
				fmt.Printf("     [诊断] 第 %d 轮 提交 %d 张(bid=%d ask=%d)回执:\n",
					i, len(newOrders), nb, na)
				for _, e := range ev {
					fmt.Printf("       %s %s\n", e.Kind, string(e.Data))
				}
			}
		}

		// **中间窗口的检验**:换单之后立刻读挂单。原子换单下这个数永远是满的;
		// 若服务端把它实现成两条命令,这里会读到 0 或残缺。
		s2, err := c.OrderSeqs(ctx, acct, mk)
		must(err, "读挂单序号")
		if len(s2) == 0 {
			emptyBookHit++
		}
		seqs = s2
		mine = priceSet(newOrders)
	}

	report("换单无中间空窗", emptyBookHit == 0,
		"%d 轮里挂单数掉到 0 的次数 = %d", *iters, emptyBookHit)
	report("nonce 连续推进", rejects == 0,
		"%d 轮换单,提交被拒 %d 次", *iters, rejects)
	report("逐项对账一致", shortRounds == 0,
		"挂上少于提交的轮数 = %d(逐项尽力而为,非零需查保证金/价格带)", shortRounds)
	report("撤挂配平", totalCancel > 0 && totalPlaced > 0,
		"累计撤 %d 挂 %d,成交 %d", totalCancel, totalPlaced, fills)

	if len(lat) > 0 {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		fmt.Printf("     换单时延 p50=%s p95=%s max=%s(含 HTTP 往返与共识)\n",
			lat[len(lat)/2].Round(time.Microsecond),
			lat[len(lat)*95/100].Round(time.Microsecond),
			lat[len(lat)-1].Round(time.Microsecond))
	}

	// ── 3. 被吃单:做市的另一半 ──
	//
	// 没人吃单的做市测试只验了「挂得上」,验不了「成交后还能不能接着做」——
	// 而库存一旦出现,保证金、仓位镜像、下一轮报价全都要跟着变。
	// 这里让一个独立账户用 IOC 跨过我们的卖一,我们**作为 maker 被动成交**。
	if *takerKey != "" {
		fmt.Println("\n── 被吃单 ──")
		tOwner, err := dexos.NewSigner(*takerKey)
		must(err, "解析吃单方私钥")
		tAcct := uint32(*takerAcct)
		tApi, err := dexos.GenerateSigner()
		must(err, "生成吃单方 API 钱包")
		tmn, err := c.NextNonce(ctx, tAcct)
		must(err, "读吃单方 master nonce")
		_, err = c.ApproveAgent(ctx, tOwner, tAcct, tApi.Address(), time.Now().Add(time.Hour), tmn)
		must(err, "授权吃单方 API 钱包")
		tn, err := c.AgentNonce(ctx, tAcct, tApi.Address())
		must(err, "读吃单方 agent nonce")

		posBefore := positionLots(ctx, c, acct, mk)
		// 要吃**我们自己**那一档,否则吃到的是盘口上另一个做市商的单,
		// 我们的持仓不会变 —— 而断言会把这个误判成「节点没给我们成交」。
		myOrders, err := c.Orders(ctx, acct, mk)
		must(err, "读自己的挂单")
		var myAsk uint32
		for _, o := range myOrders {
			if o.Side == "sell" && (myAsk == 0 || o.Price < myAsk) {
				myAsk = o.Price
			}
		}
		if myAsk == 0 {
			log.Fatal("我们没有卖单在簿上,无法验被动成交")
		}
		bookNow, err := c.Book(ctx, mk, 5)
		must(err, "读盘口")
		if len(bookNow.Asks) == 0 || uint32(bookNow.Asks[0][0]) != myAsk {
			log.Fatalf("卖一不是我们的(盘口 %v,我们 %d)—— 先确保报价在最优档",
				bookNow.Asks[0][0], myAsk)
		}
		hit := uint64(myAsk)
		tev, err := c.BatchPlace(ctx, tApi, tAcct, []dexos.BatchOrder{{
			Market: mk, Side: dexos.Buy, Price: uint32(hit) + 2, Lots: *lots, TIF: dexos.IOC,
		}}, tn)
		must(err, "吃单方 IOC")
		takerFills := countKind(tev, "Fill")
		report("吃单成交", takerFills > 0, "吃单方回执 Fill %d 条(打在 %d)", takerFills, hit)

		time.Sleep(300 * time.Millisecond)
		posAfter := positionLots(ctx, c, acct, mk)
		report("做市方拿到库存", posAfter != posBefore,
			"做市持仓 %d → %d lots(被动成交应当是空头)", posBefore, posAfter)

		// 有库存之后接着报价:保证金要够、报价循环不能散
		seqs, err = c.OrderSeqs(ctx, acct, mk)
		must(err, "读挂单序号")
		if n2, e2 := c.AgentNonce(ctx, acct, api.Address()); e2 == nil {
			nonce = n2
		}
		okRounds := 0
		for i := 0; i < 10; i++ {
			time.Sleep(*interval)
			b, err := c.Book(ctx, mk, 10)
			must(err, "读盘口")
			nb, na, ok := quotePricesAround(b, mine, *levels)
			if !ok {
				continue
			}
			newOrders := ladder(mk, nb, na, *lots, *levels)
			ev, err := c.BatchReplace(ctx, api, acct, mk, seqs, newOrders, nonce)
			if err != nil {
				log.Fatalf("换单结果需要核对，停止示例；不得换 nonce 重发 (nonce=%d): %v", nonce, err)
			}
			nonce++
			if countKind(ev, "OrderAccepted") == *levels*2 {
				okRounds++
			}
			mine = priceSet(newOrders)
			seqs, err = c.OrderSeqs(ctx, acct, mk)
			must(err, "读挂单序号")
		}
		report("持仓状态下继续报价", okRounds == 10,
			"带库存换单 10 轮,全额挂上 %d 轮", okRounds)

		r := riskOf(ctx, c, acct)
		report("保证金随库存计提", r.IMR != "0" && r.MMR != "0",
			"IMR=%s MMR=%s(持仓后应当非零)", r.IMR, r.MMR)
	}

	// ── 4. 事件流 ──
	fmt.Println("\n── 事件流 ──")
	time.Sleep(500 * time.Millisecond) // 给 WS 一点追平时间
	report("收到自己的挂撤事件", evCount["OrderAccepted"] > 0 && evCount["OrderCanceled"] > 0,
		"OrderAccepted %d · OrderCanceled %d · Fill %d",
		evCount["OrderAccepted"], evCount["OrderCanceled"], evCount["Fill"])
	report("无丢帧", lostFrames == 0, "服务端报告丢弃 %d 条", lostFrames)

	// ── 5. 撤干净 + 撤销授权 ──
	fmt.Println("\n── 收摊 ──")
	_, err = c.BatchCancel(ctx, api, acct, mk, seqs, nonce)
	must(err, "BatchCancel 收摊")
	nonce++
	left, err := c.OrderSeqs(ctx, acct, mk)
	must(err, "读挂单序号")
	report("挂单已清空", len(left) == 0, "剩余挂单 %d 张", len(left))

	mn2, err := c.NextNonce(ctx, acct)
	must(err, "读 master nonce")
	_, err = c.RevokeAgent(ctx, owner, acct, api.Address(), mn2)
	must(err, "撤销授权")
	_, err = c.BatchPlace(ctx, api, acct, ladder(mk, bid, ask, *lots, 1), nonce)
	report("撤销后下单被拒", err != nil, "%v", err)

	r, err := c.Risk(ctx, acct)
	must(err, "读风险切面")
	fmt.Printf("\n账户终态:抵押 %s · NC %s · 持仓 %d 个\n", r.Collateral, r.NC, len(r.Positions))

	// ── 汇总 ──
	pass := 0
	for _, c := range checks {
		if c.ok {
			pass++
		}
	}
	fmt.Printf("\n")
	if pass == len(checks) {
		fmt.Printf("✅ %d/%d 通过\n", pass, len(checks))
		return
	}
	fmt.Printf("✗ %d/%d 通过\n", pass, len(checks))
	for _, c := range checks {
		if !c.ok {
			fmt.Printf("   失败:%s — %s\n", c.name, c.note)
		}
	}
	os.Exit(1)
}

// quotePrices 在当前价差**内侧**各让一个 tick 报价 —— 这样我们应当成为盘口最优,
// 从而能验证「报价真的上了簿」而不只是「命令被受理」。
func quotePrices(m dexos.Market, levels int) (bid, ask uint32) {
	bid, ask = m.BestBid, m.BestAsk
	if bid == 0 || ask == 0 { // 空簿:围着 oracle 报
		return m.Oracle - uint32(levels) - 1, m.Oracle + uint32(levels) + 1
	}
	bid++
	ask--
	// 阶梯要放得下,且不能自成交
	if ask <= bid+uint32(levels) {
		mid := (bid + ask) / 2
		bid = mid - uint32(levels)
		ask = mid + uint32(levels)
	}
	return bid, ask
}

// quotePricesAround 从**盘口**推参考价,并排除自己的挂单。
//
// 两个坑都必须同时避开,只避一个仍然跑不通:
//
//   - 不排除自己 → 跟自己的报价互相追价,每轮各内收一 tick,几十轮后两侧撞上。
//   - 用 oracle 当锚(看似干净)→ **会跨价**。盘口上另一个做市商按它自己那份
//     **滞后的** oracle 报价,行情下跌时它的陈旧买价会高过我们按新 oracle 算出的
//     卖价;PostOnly 于是被拒。表现为「提交 6 张只挂上 3 张」而回执里没有任何
//     拒绝事件 —— 极难归因。真实做市必须看**簿**,不能看指数。
func quotePricesAround(b *dexos.Book, mine map[uint32]bool, levels int) (bid, ask uint32, ok bool) {
	refBid, refAsk := uint32(0), uint32(0)
	for _, l := range b.Bids {
		if p := uint32(l[0]); !mine[p] {
			refBid = p
			break
		}
	}
	for _, l := range b.Asks {
		if p := uint32(l[0]); !mine[p] {
			refAsk = p
			break
		}
	}
	if refBid == 0 || refAsk == 0 || refAsk <= refBid {
		return 0, 0, false
	}
	span := uint32(levels) + 1
	// 价差放得下就挂进内侧(成为盘口最优);放不下就平价挂上去,PostOnly 不跨价。
	if refAsk-refBid > 2*span {
		return refBid + 1, refAsk - 1, true
	}
	return refBid, refAsk, true
}

// ladder 构造 levels 档双边报价。PostOnly:做市商绝不吃单 ——
// 真跨价了要被拒而不是成交,否则做市变成了拿反向仓位。
func ladder(mk uint16, bid, ask uint32, lots uint64, levels int) []dexos.BatchOrder {
	out := make([]dexos.BatchOrder, 0, levels*2)
	for i := 0; i < levels; i++ {
		out = append(out, dexos.BatchOrder{
			Market: mk, Side: dexos.Buy, Price: bid - uint32(i), Lots: lots, TIF: dexos.PostOnly,
		})
		out = append(out, dexos.BatchOrder{
			Market: mk, Side: dexos.Sell, Price: ask + uint32(i), Lots: lots, TIF: dexos.PostOnly,
		})
	}
	return out
}

// priceSet 本轮自己报出去的价位 —— 推参考价时要把它们排除掉。
func priceSet(orders []dexos.BatchOrder) map[uint32]bool {
	m := make(map[uint32]bool, len(orders))
	for _, o := range orders {
		m[o.Price] = true
	}
	return m
}

func countKind(ev []dexos.EventEnvelope, kind string) int {
	n := 0
	for _, e := range ev {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// positionLots 取账户在某市场的持仓手数(无仓位 = 0)。
func positionLots(ctx context.Context, c *dexos.Client, acct uint32, mk uint16) int64 {
	r := riskOf(ctx, c, acct)
	for _, p := range r.Positions {
		if p.Market == mk {
			return p.Lots
		}
	}
	return 0
}

func riskOf(ctx context.Context, c *dexos.Client, acct uint32) *dexos.Risk {
	r, err := c.Risk(ctx, acct)
	must(err, "读风险切面")
	return r
}

func marketOf(ctx context.Context, c *dexos.Client, mk uint16) dexos.Market {
	ms, err := c.Markets(ctx)
	must(err, "读 /markets")
	for _, m := range ms {
		if m.Market == mk {
			return m
		}
	}
	log.Fatalf("市场 %d 不存在", mk)
	return dexos.Market{}
}

func must(err error, what string) {
	if err != nil {
		log.Fatalf("%s:%v", what, err)
	}
}

var _ = json.Marshal
