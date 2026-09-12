// session 演示**最小接入**:只要网关地址 + 一把 API 钱包私钥。
//
//	go run ./examples/session -url http://127.0.0.1:17807 -api-key 0x…
//
// 对照 Hyperliquid 要给「私钥 + 主钱包地址」两样 —— 这里账户号由状态机反查得出,
// 不必被带外告知(带外告知的东西迟早会抄错或过期)。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

func main() {
	url := flag.String("url", "", "网关地址")
	apiKey := flag.String("api-key", "", "API 钱包私钥(前端「API 钱包」里生成并授权)")
	flag.Parse()
	ctx := context.Background()

	s, err := dexos.New(ctx, *url, *apiKey)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("账户 %d(由 API 钱包 %s 反查得出)\n", s.Account, s.Agent.Address().Hex())
	fmt.Printf("链 %d · 入金地址 %s\n", s.Config.ChainID, s.Config.Deposit.SystemGateway)

	r, err := s.Risk(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("抵押 %s · 持仓 %d 个\n", r.Collateral, len(r.Positions))

	m, err := s.Markets(ctx)
	if err != nil || len(m) == 0 {
		log.Fatal("读不到市场")
	}
	// 注意:不用传账户号,也不用管 nonce
	ev, err := s.Place(ctx, dexos.BatchOrder{
		Market: m[0].Market, Side: dexos.Buy, Price: m[0].Oracle - 3000,
		Lots: 1, TIF: dexos.GTC,
	})
	if err != nil {
		log.Fatal("下单失败:", err)
	}
	fmt.Printf("下单 → %s\n", ev[0].Kind)

	seqs, _ := s.OrderSeqs(ctx, m[0].Market)
	fmt.Printf("在簿挂单 %d 张\n", len(seqs))
	if len(seqs) > 0 {
		ev, err = s.Cancel(ctx, m[0].Market, seqs...)
		if err != nil {
			log.Fatal("撤单失败:", err)
		}
		fmt.Printf("撤单 → %d 条事件\n", len(ev))
	}
	fmt.Println("\n✅ 全程只用了 URL 与一把 API 钱包私钥")
}
