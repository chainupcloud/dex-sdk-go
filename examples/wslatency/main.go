// wslatency 实测事件流的端到端时延与吞吐 —— 做市接入前该先跑一遍的东西。
//
//	go run ./examples/wslatency -master-key 0x… -account 3 -n 200
//
// 量的是**下单请求返回 ↔ 同一事件从 WS 收到**之间的差。这个口径的含义是:
// 「我提交后,多久能从事件流里看到它」—— 做市重新报价的反应延迟就取决于它。
//
// 不量「网关收到 ↔ 推出」是因为那个数好看但没用:做市商关心的是自己这一侧
// 什么时候能据此行动,中间任何一段慢都得算进去。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

func main() {
	var (
		base      = flag.String("url", "http://127.0.0.1:8080", "网关地址")
		chainID   = flag.Uint64("chain-id", 31337, "chainId")
		masterKey = flag.String("master-key", "", "主账号私钥")
		account   = flag.Uint("account", 0, "内核账户号")
		market    = flag.Uint("market", 0, "市场号")
		n         = flag.Int("n", 100, "采样次数")
	)
	flag.Parse()
	if *masterKey == "" {
		log.Fatal("必须给 -master-key")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := dexos.NewClient(*base, *chainID)

	owner, err := dexos.NewSigner(*masterKey)
	must(err)
	api, err := dexos.GenerateSigner()
	must(err)
	mn, err := c.NextNonce(ctx, uint32(*account))
	must(err)
	_, err = c.ApproveAgent(ctx, owner, uint32(*account), api.Address(),
		time.Now().Add(time.Hour), mn)
	must(err)
	defer func() {
		mn, _ := c.NextNonce(context.Background(), uint32(*account))
		_, _ = c.RevokeAgent(context.Background(), owner, uint32(*account), api.Address(), mn)
	}()

	// 先订阅再下单。反过来会在两步之间丢事件,而且丢得无声无息。
	st, err := c.Subscribe(ctx)
	must(err)
	time.Sleep(300 * time.Millisecond) // 等连接建立

	var mu sync.Mutex
	seen := map[uint64]time.Time{} // orderSeq → 收到时刻
	go func() {
		for {
			select {
			case ev, ok := <-st.Events:
				if !ok {
					return
				}
				if ev.Kind != "OrderAccepted" {
					continue
				}
				var d struct {
					OrderSeq uint64 `json:"orderSeq"`
				}
				if json.Unmarshal(ev.Data, &d) != nil {
					continue
				}
				mu.Lock()
				if _, dup := seen[d.OrderSeq]; !dup {
					seen[d.OrderSeq] = time.Now()
				}
				mu.Unlock()
			case n := <-st.Lost:
				if n == 0 {
					log.Printf("⚠ seq 倒退 —— 事件顺序不可信")
				} else {
					log.Printf("⚠ 服务端报告丢帧 %d 条 —— 本地镜像已不可信,应重拉快照", n)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	bk, err := c.Book(ctx, uint16(*market), 1)
	must(err)
	if len(bk.Bids) == 0 {
		log.Fatal("盘口无买盘")
	}
	base0 := uint32(bk.Bids[0][0]) - 500

	an, err := c.AgentNonce(ctx, uint32(*account), api.Address())
	must(err)

	fmt.Printf("采样 %d 次(每次一张 POST_ONLY 挂单)…\n", *n)
	sent := make([]time.Time, 0, *n)
	seqs := make([]uint64, 0, *n)
	for i := 0; i < *n; i++ {
		px := base0 - uint32(i) // 每张不同价,避免自成交与撮合
		t0 := time.Now()
		ev, err := c.BatchPlace(ctx, api, uint32(*account), []dexos.BatchOrder{
			{Market: uint16(*market), Side: dexos.Buy, Price: px, Lots: 1, TIF: dexos.PostOnly},
		}, an+uint64(i))
		if err != nil {
			log.Fatalf("第 %d 次下单失败:%v", i, err)
		}
		t1 := time.Now()
		for _, e := range ev {
			if e.Kind != "OrderAccepted" {
				continue
			}
			var d struct {
				OrderSeq uint64 `json:"orderSeq"`
			}
			if json.Unmarshal(e.Data, &d) == nil {
				sent = append(sent, t1)
				seqs = append(seqs, d.OrderSeq)
			}
		}
		_ = t0
	}

	time.Sleep(1500 * time.Millisecond) // 收尾
	mu.Lock()
	defer mu.Unlock()

	deltas := make([]time.Duration, 0, len(seqs))
	missing := 0
	for i, s := range seqs {
		at, ok := seen[s]
		if !ok {
			missing++
			continue
		}
		deltas = append(deltas, at.Sub(sent[i]))
	}
	if len(deltas) == 0 {
		log.Fatal("一条都没从 WS 收到 —— 事件流没通")
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
	pct := func(p float64) time.Duration { return deltas[int(float64(len(deltas)-1)*p)] }

	fmt.Printf("\n样本 %d / 未收到 %d\n", len(deltas), missing)
	fmt.Printf("  p50 %v\n  p90 %v\n  p99 %v\n  max %v\n",
		pct(0.50), pct(0.90), pct(0.99), deltas[len(deltas)-1])
	if missing > 0 {
		fmt.Printf("\n⚠ 有 %d 条事件没从流里收到 —— 检查上面的丢帧告警\n", missing)
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
