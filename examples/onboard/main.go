// onboard 演示开发者从零接入:只知道网关地址 → 连接参数 → 钱包地址 → 账户 →(可选)用 API 钱包交易。
//
//	go run ./examples/onboard -url http://127.0.0.1:17807                    # 看连接参数,生成一个新地址
//	go run ./examples/onboard -url … -key 0x…                               # 已有钱包:查账户
//	go run ./examples/onboard -url … -key 0x… -api-key 0x…                  # 再用 API 钱包挂一张单、撤掉
//
// v1 按**主钱包地址 + 子账户序号**寻址,不用、也拿不到内核账户号。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/chainupcloud/dex-sdk-go/dexos"
	"github.com/chainupcloud/dex-sdk-go/dexosv1"
)

func main() {
	url := flag.String("url", "", "网关地址 —— 唯一需要被告知的参数")
	key := flag.String("key", "", "你的钱包私钥(留空则生成一个新的)")
	apiKey := flag.String("api-key", "", "API 钱包私钥(主钱包授权过的那把)")
	flag.Parse()
	ctx := context.Background()
	c := dexosv1.NewClient(strings.TrimRight(*url, "/"))

	// ── 1) 连接参数:链、EIP-712 域、入金地址、token 表都由网关给,不抄进配置 ──
	ex, err := c.Exchange(ctx)
	must(err, "读连接参数")
	if ex.Version.CodecVer != dexos.CodecVersion {
		log.Fatalf("网关的命令编码是 v%d,本 SDK 实现 v%d —— 签出来的命令它不认,先升级 SDK", ex.Version.CodecVer, dexos.CodecVersion)
	}
	fmt.Printf("链 %d · 入金系统地址 %s\n", ex.ChainID, ex.Deposit.SystemGateway)
	for _, t := range ex.Deposit.Tokens {
		fmt.Printf("token %d = %s(%d 位小数)@ %s\n", t.Token, t.Symbol, t.Decimals, t.ERC20)
	}

	// ── 2) 地址:要么用你已有的,要么本地生成一个 ──
	var me *dexos.Signer
	if *key != "" {
		me, err = dexos.NewSigner(*key)
	} else {
		me, err = dexos.GenerateSigner()
		if err == nil {
			fmt.Printf("\n新钱包 %s\n  私钥 %s\n  (自己保存;交易所永远不会知道它)\n", me.Address().Hex(), me.PrivateKeyHex())
		}
	}
	must(err, "准备钱包")

	// ── 3) 找账户:首次入金才开户,没有注册接口 ──
	subs, err := c.Subaccounts(ctx, me.Address())
	var apiErr *dexosv1.Error
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		fmt.Printf("\n%s 还没有账户。\n", me.Address().Hex())
		fmt.Printf("往 %s 打一笔 USDC(裸 transfer,没有 deposit 方法),\n", ex.Deposit.SystemGateway)
		fmt.Println("中继观察到之后自动开户 —— 付钱即开户。")
		return
	}
	must(err, "读账户表")
	fmt.Println()
	for _, s := range subs {
		fmt.Printf("子账户 %d(主账户=%v)\n", s.Subaccount, s.IsMain)
	}
	ref := dexosv1.AccountRef{Address: me.Address()}
	acct, err := c.Account(ctx, ref)
	must(err, "读主账户")
	fmt.Printf("主账户:抵押 %s USDC · 可提 %s · 持仓 %d 个\n", acct.Collateral, acct.AvailableToWithdraw, acct.PositionsCount)

	// ── 4) 用 API 钱包交易。主钱包私钥到这里就可以收起来了 ──
	if *apiKey == "" {
		fmt.Println("\n给 -api-key 可继续演示下单(在前端「API 钱包」里生成并授权,或见 examples/quickstart)")
		return
	}
	agent, err := dexos.NewSigner(*apiKey)
	must(err, "解析 API 钱包私钥")
	s, err := dexosv1.NewSession(ctx, c, agent, ref)
	must(err, "建会话(这把 API 钱包授权给这个账户了吗?)")
	cat, err := c.Catalog(ctx)
	must(err, "读市场目录")
	m, price, lots, err := farBid(cat)
	must(err, "挑市场")
	res, err := s.Place(ctx, dexosv1.OrderRequest{Market: m, Side: dexos.Buy, Price: price, Lots: lots, TIF: dexos.PostOnly})
	must(err, "下单")
	it := res.Items[0]
	if it.Status != "accepted" || it.OrderID == nil {
		log.Fatalf("没挂上:%+v", it)
	}
	fmt.Printf("%s 挂买 %s × %s → 订单 %d\n", m.Symbol, m.Scale.Price(price), m.Scale.Size(lots), *it.OrderID)
	_, err = s.Cancel(ctx, m, *it.OrderID)
	must(err, "撤单")
	fmt.Println("已撤单")
}

// farBid 挑一个有标记价的永续,给出远离市价的买单:价格是标记价的一半(不会成交),
// 数量取满足最小名义额的最小值再加 1 手(名义额 = ticks × lots / 10^(priceDecimals+sizeDecimals))。
func farBid(cat *dexosv1.Catalog) (*dexosv1.Market, uint32, uint64, error) {
	for i := range cat.Markets() {
		m := &cat.Markets()[i]
		if m.Kind != "perp" || m.Status != "Active" || m.MarkPrice == nil || *m.MarkPrice < 2 {
			continue
		}
		price := *m.MarkPrice / 2
		minN, err := dexosv1.ParseFixed(m.MinNotional, m.Scale.PriceDecimals+m.Scale.SizeDecimals)
		if err != nil {
			return nil, 0, 0, err
		}
		return m, price, minN.Uint64()/uint64(price) + 1, nil
	}
	return nil, 0, 0, fmt.Errorf("目录里没有带标记价的活跃永续")
}

func must(err error, what string) {
	if err != nil {
		log.Fatalf("%s 失败:%v", what, err)
	}
}
