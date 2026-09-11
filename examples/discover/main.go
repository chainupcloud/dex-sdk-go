// discover 演示「只知道一个 URL」的接入形态。
//
//	go run ./examples/discover -url http://node.example:17807 -master-key 0x... -account 7
//
// 对照:以前你还需要被告知 chainId、系统地址、USDC 地址,任何一个填错或过期,
// 表现都极具迷惑性(签名全 401 而行情正常 / 钱打到没人监听的合约)。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

func main() {
	url := flag.String("url", "", "网关地址 —— 唯一需要被告知的参数")
	masterKey := flag.String("master-key", "", "主账号私钥")
	account := flag.Uint("account", 0, "内核账户号")
	flag.Parse()
	ctx := context.Background()

	// 一行拿到全部连接参数,并核对 codec 指纹
	c, cfg, err := dexos.Connect(ctx, *url)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("chainId      %d\n", cfg.ChainID)
	fmt.Printf("域           %s v%s · verifyingContract %s\n",
		cfg.Domain.Name, cfg.Domain.Version, cfg.Domain.VerifyingContract)
	fmt.Printf("入金地址     %s\n", cfg.Deposit.SystemGateway)
	for _, t := range cfg.Deposit.Tokens {
		fmt.Printf("token %d      %s (finalized=%v)\n", t.Token, t.ERC20, t.Finalized)
	}
	fmt.Printf("终局性       %s\n\n", cfg.FinalityMode)

	if *masterKey == "" {
		return
	}
	// 用发现到的域直接签一笔 —— 证明参数是对的,而不只是"看起来对"
	owner, err := dexos.NewSigner(*masterKey)
	if err != nil {
		log.Fatal(err)
	}
	api, _ := dexos.GenerateSigner()
	mn, err := c.NextNonce(ctx, uint32(*account))
	if err != nil {
		log.Fatal(err)
	}
	if _, err := c.ApproveAgent(ctx, owner, uint32(*account), api.Address(),
		time.Now().Add(time.Hour), mn); err != nil {
		log.Fatal("授权失败(域不对的话就挂在这):", err)
	}
	fmt.Printf("已用发现到的域授权 API 钱包 %s\n", api.Address().Hex())

	n, err := c.AgentNonce(ctx, uint32(*account), api.Address())
	if err != nil {
		log.Fatal(err)
	}
	m, err := c.Markets(ctx)
	if err != nil || len(m) == 0 {
		log.Fatal("读不到市场")
	}
	ev, err := c.BatchPlace(ctx, api, uint32(*account), []dexos.BatchOrder{{
		Market: m[0].Market, Side: dexos.Buy, Price: m[0].Oracle - 2000,
		Lots: 1, TIF: dexos.GTC,
	}}, n)
	if err != nil {
		log.Fatal("下单失败:", err)
	}
	fmt.Printf("下单回执 %d 条:%s\n", len(ev), ev[0].Kind)
	fmt.Println("\n✅ 只靠一个 URL 完成了 发现 → 授权 → 下单")
}
