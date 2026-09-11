// onboard 演示开发者从零接入的完整路径:生成地址 → 找账户 → 连接 → 交易。
//
//	go run ./examples/onboard -url http://127.0.0.1:17807             # 只看参数与新地址
//	go run ./examples/onboard -url … -key 0x… -api-key 0x…            # 已有账户与 API 钱包
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

func main() {
	url := flag.String("url", "", "网关地址 —— 唯一需要被告知的参数")
	key := flag.String("key", "", "你的钱包私钥(留空则生成一个新的)")
	apiKey := flag.String("api-key", "", "API 钱包私钥(前端授权时拿到的)")
	flag.Parse()
	ctx := context.Background()

	// ── 1) 连接:参数自动发现 ──
	c, cfg, err := dexos.Connect(ctx, *url, 1)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("链 %d · 入金地址 %s · 终局性 %s\n",
		cfg.ChainID, cfg.Deposit.SystemGateway, cfg.FinalityMode)
	for _, t := range cfg.Deposit.Tokens {
		fmt.Printf("token %d = %s\n", t.Token, t.ERC20)
	}

	// ── 2) 地址:要么用你已有的,要么本地生成一个 ──
	var me *dexos.Signer
	if *key != "" {
		me, err = dexos.NewSigner(*key)
	} else {
		me, err = dexos.GenerateSigner()
		if err == nil {
			fmt.Printf("\n新钱包 %s\n  私钥 %s\n  (自己保存;交易所永远不会知道它)\n",
				me.Address().Hex(), me.PrivateKeyHex())
		}
	}
	if err != nil {
		log.Fatal(err)
	}

	// ── 3) 找账户:钱包地址 ≠ 内核账户号,只能查 ──
	acc, err := c.AccountByAddress(ctx, me.Address().Hex())
	if errors.Is(err, dexos.ErrNotRegistered) {
		fmt.Printf("\n%s 还没有内核账户。\n", me.Address().Hex())
		fmt.Printf("去打一笔 USDC 到 %s(裸 transfer,没有 deposit 方法),\n",
			cfg.Deposit.SystemGateway)
		fmt.Printf("中继观察到之后内核会自动开户 —— 没有注册接口,付钱即开户。\n")
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n内核账户 %d · 抵押 %s · 下一个 nonce %d\n",
		acc.AccountID, acc.Collateral, acc.NextNonce)

	// ── 4) 用 API 钱包交易 ──
	if *apiKey == "" {
		fmt.Println("\n给 -api-key 可继续演示下单(在前端「API 钱包」里生成并授权)")
		return
	}
	api, err := dexos.NewSigner(*apiKey)
	if err != nil {
		log.Fatal(err)
	}
	n, err := c.AgentNonce(ctx, acc.AccountID, api.Address())
	if err != nil {
		log.Fatal("读 agent nonce 失败(这把 API 钱包授权给这个账户了吗?):", err)
	}
	m, err := c.Markets(ctx)
	if err != nil || len(m) == 0 {
		log.Fatal("读不到市场")
	}
	ev, err := c.BatchPlace(ctx, api, acc.AccountID, []dexos.BatchOrder{{
		Market: m[0].Market, Side: dexos.Buy, Price: m[0].Oracle - 3000,
		Lots: 1, TIF: dexos.GTC,
	}}, n)
	if err != nil {
		log.Fatal("下单失败:", err)
	}
	fmt.Printf("下单回执:%s\n", ev[0].Kind)
}
