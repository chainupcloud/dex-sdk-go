// wslatency 实测 v1 推送的端到端时延 —— 做市接入前该先跑一遍的东西。
//
//	go run ./examples/wslatency -url http://127.0.0.1:17807 -master-key 0x… -n 200
//
// 量的是**下单请求返回 ↔ 同一张单的 accepted 帧从 WS(`orders` 频道)收到**之间的差。这个口径的含义是:
// 「我提交后,多久能从推送里看到它」—— 做市重新报价的反应延迟就取决于它。负数 = 推送比写回执先到。
//
// 不量「网关收到 ↔ 推出」是因为那个数好看但没用:做市商关心的是自己这一侧
// 什么时候能据此行动,中间任何一段慢都得算进去。
//
// 主钱包账户里要有 USDC。挂的单价格远离市价、不会成交;收尾全撤并撤销临时 API 钱包。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
	"github.com/chainupcloud/dex-sdk-go/dexosv1"
)

func main() {
	var (
		url        = flag.String("url", "http://127.0.0.1:8080", "网关地址(不带 /v1)")
		masterKey  = flag.String("master-key", "", "主钱包私钥")
		subaccount = flag.Uint("subaccount", 0, "子账户序号(0 = 主账户)")
		n          = flag.Int("n", 100, "采样次数")
	)
	flag.Parse()
	if *masterKey == "" {
		log.Fatal("必须给 -master-key")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := dexosv1.NewClient(strings.TrimRight(*url, "/"))

	owner, err := dexos.NewSigner(*masterKey)
	must(err)
	w, err := dexosv1.NewWallet(ctx, c, owner, uint32(*subaccount))
	must(err)
	api, err := dexos.GenerateSigner()
	must(err)
	_, err = w.ApproveAgent(ctx, api.Address(), time.Now().Add(time.Hour), "wslatency")
	must(err)
	s, err := dexosv1.NewSession(ctx, c, api, w.Account)
	must(err)
	defer func() {
		if _, err := s.CancelAll(context.Background()); err != nil {
			log.Printf("收尾撤全部失败:%v", err)
		}
		if _, err := w.RevokeAgent(context.Background(), api.Address()); err != nil {
			log.Printf("收尾撤销 API 钱包失败:%v", err)
		}
	}()

	cat, err := c.Catalog(ctx)
	must(err)
	m, base, lots, err := farBid(cat, *n)
	must(err)

	// 先订阅、等到 Live 再下单。反过来会在两步之间丢帧,而且丢得无声无息
	var mu sync.Mutex
	seen := map[uint64]time.Time{} // 订单号 → accepted 帧收到时刻
	st, err := dexosv1.NewStream(c, cat, dexosv1.StreamOptions{
		OnConnError: func(err error) { log.Printf("WS 连接:%v", err) },
	})
	must(err)
	sub, err := st.SubscribeOrders(w.Account, func(u dexosv1.OrdersUpdate) {
		if u.Event == nil || u.Event.Kind != "accepted" {
			return
		}
		mu.Lock()
		if _, dup := seen[u.Event.OrderID]; !dup {
			seen[u.Event.OrderID] = time.Now()
		}
		mu.Unlock()
	})
	must(err)
	go func() {
		if err := st.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("WS 已停:%v", err)
		}
	}()
	for deadline := time.Now().Add(15 * time.Second); sub.State() != dexosv1.StateLive; time.Sleep(50 * time.Millisecond) {
		if sub.State() == dexosv1.StateFailed || time.Now().After(deadline) {
			log.Fatalf("orders 订阅没进 Live(%s)", sub.State())
		}
	}

	fmt.Printf("%s 采样 %d 次(每次一张远离市价的只挂单)…\n", m.Symbol, *n)
	sent := map[uint64]time.Time{}
	for i := 0; i < *n; i++ {
		res, err := s.Place(ctx, dexosv1.OrderRequest{
			Market: m, Side: dexos.Buy, Price: base - uint32(i), Lots: lots, TIF: dexos.PostOnly,
		})
		if err != nil {
			log.Fatalf("第 %d 次下单失败:%v", i, err)
		}
		at := time.Now()
		if it := res.Items[0]; it.Status == "accepted" && it.OrderID != nil {
			sent[*it.OrderID] = at
		}
	}

	time.Sleep(1500 * time.Millisecond) // 等最后几帧
	mu.Lock()
	defer mu.Unlock()
	deltas := make([]time.Duration, 0, len(sent))
	missing := 0
	for id, t := range sent {
		at, ok := seen[id]
		if !ok {
			missing++
			continue
		}
		deltas = append(deltas, at.Sub(t))
	}
	if len(deltas) == 0 {
		log.Fatal("一条都没从 WS 收到 —— 推送没通")
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
	pct := func(p float64) time.Duration { return deltas[int(float64(len(deltas)-1)*p)] }
	fmt.Printf("\n样本 %d / 未收到 %d\n", len(deltas), missing)
	fmt.Printf("  p50 %v\n  p90 %v\n  p99 %v\n  max %v\n", pct(0.50), pct(0.90), pct(0.99), deltas[len(deltas)-1])
	if missing > 0 {
		fmt.Printf("\n⚠ 有 %d 张单的 accepted 帧没从推送里收到 —— 看上面的 WS 连接日志\n", missing)
	}
}

// farBid 挑一个有标记价的永续:起始价是标记价的一半,往下每次减 1 tick 共 n 张,都不会成交;
// 数量按最低的那张满足最小名义额(名义额 = ticks × lots / 10^(priceDecimals+sizeDecimals))。
func farBid(cat *dexosv1.Catalog, n int) (*dexosv1.Market, uint32, uint64, error) {
	for i := range cat.Markets() {
		m := &cat.Markets()[i]
		if m.Kind != "perp" || m.Status != "Active" || m.MarkPrice == nil || *m.MarkPrice/2 <= uint32(n)*2 {
			continue
		}
		base := *m.MarkPrice / 2
		minN, err := dexosv1.ParseFixed(m.MinNotional, m.Scale.PriceDecimals+m.Scale.SizeDecimals)
		if err != nil {
			return nil, 0, 0, err
		}
		return m, base, minN.Uint64()/uint64(base-uint32(n)) + 1, nil
	}
	return nil, 0, 0, fmt.Errorf("目录里没有标记价够高的活跃永续")
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
