// quickstart 走一遍 API 钱包的完整生命周期,对着真实节点跑。
//
//	go run ./examples/quickstart -master-key 0x... -account 3
//
// 它同时是 SDK 的**集成自检**:金样对拍只能证明规范编码正确,而签名路径
// (secp256k1 的 v 位序、地址派生、EIP-712 域分隔符)只有对着真节点才验得了 ——
// 那三处任何一处错了,表现都是「服务端回 401/UnboundSigner」,静态检查看不出来。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

func main() {
	var (
		base      = flag.String("url", "http://127.0.0.1:8080", "网关地址")
		chainID   = flag.Uint64("chain-id", 31337, "EIP-712 域的 chainId")
		masterKey = flag.String("master-key", "", "主账号私钥(0x…);只用于授权/撤销")
		account   = flag.Uint("account", 0, "内核账户号(master)")
		market    = flag.Uint("market", 0, "市场号")
		px        = flag.Uint64("price", 0, "挂单价(tick);0 = 按盘口自动取")
	)
	flag.Parse()
	if *masterKey == "" {
		log.Fatal("必须给 -master-key(主账号私钥)")
	}
	ctx := context.Background()
	c := dexos.NewClient(*base, *chainID)

	// 0) 指纹核对。codec 版本对不上意味着规范编码可能已经变了,
	//    而那种错误只表现为「签名一律被拒」—— 先撞上这里比撞上 401 好懂得多。
	v, err := c.Version(ctx)
	must(err, "读 /version")
	fmt.Printf("节点 pkg=%s snapshot=v%d codec=v%d build=%s\n",
		v.Pkg, v.SnapshotVer, v.CodecVer, v.BuildID)

	owner, err := dexos.NewSigner(*masterKey)
	must(err, "解析主账号私钥")
	fmt.Printf("主账号地址 %s · 内核账户 %d\n", owner.Address(), *account)

	// 1) 本地生成 API 钱包。私钥只存在于本进程,交易所只拿到地址。
	api, err := dexos.GenerateSigner()
	must(err, "生成 API 钱包")
	fmt.Printf("\n生成 API 钱包 %s\n  私钥 %s\n  (只打印这一次;交易所永远不会知道它)\n",
		api.Address(), api.PrivateKeyHex())

	// 2) 主账号签名授权。这一步用 **master 账户**的 nonce。
	mn, err := c.NextNonce(ctx, uint32(*account))
	must(err, "读 master nonce")
	_, err = c.ApproveAgent(ctx, owner, uint32(*account), api.Address(),
		time.Now().Add(180*24*time.Hour), mn)
	must(err, "授权 API 钱包")
	fmt.Println("\n已授权(180 天)")

	// 3) 读回列表
	agents, err := c.Agents(ctx, uint32(*account))
	must(err, "列出 API 钱包")
	for _, a := range agents {
		fmt.Printf("  %s  有效期至 %s  过期=%v\n", a.Address, a.ValidUntilMs, a.Expired)
	}

	// 4) 用 API 钱包下单。这一步用 **agent 自己的** nonce —— 与上面那个是两个计数器。
	price := uint32(*px)
	if price == 0 {
		bk, err := c.Book(ctx, uint16(*market), 1)
		must(err, "读盘口")
		if len(bk.Bids) == 0 {
			log.Fatal("盘口没有买盘,用 -price 显式指定挂单价")
		}
		price = uint32(bk.Bids[0][0]) - 100 // 挂在买一之下,避免立刻成交
	}
	an, err := c.AgentNonce(ctx, uint32(*account), api.Address())
	must(err, "读 agent nonce")
	orders := []dexos.BatchOrder{
		{Market: uint16(*market), Side: dexos.Buy, Price: price, Lots: 1, TIF: dexos.PostOnly},
		{Market: uint16(*market), Side: dexos.Buy, Price: price - 10, Lots: 1, TIF: dexos.PostOnly},
	}
	ev, err := c.BatchPlace(ctx, api, uint32(*account), orders, an)
	must(err, "API 钱包批量下单")
	fmt.Printf("\n批量下单 → %d 条事件 %s\n", len(ev), kinds(ev))

	// 5) 看挂单
	seqs, err := c.OrderSeqs(ctx, uint32(*account), uint16(*market))
	must(err, "读挂单")
	fmt.Printf("当前挂单 %d 张 seq=%v\n", len(seqs), seqs)

	// 6) 原子换单:撤掉刚才那些,换一组更低的价 —— 中间没有「没有报价」的窗口
	an, err = c.AgentNonce(ctx, uint32(*account), api.Address())
	must(err, "读 agent nonce")
	ev, err = c.BatchReplace(ctx, api, uint32(*account), uint16(*market), seqs,
		[]dexos.BatchOrder{
			{Market: uint16(*market), Side: dexos.Buy, Price: price - 50, Lots: 2, TIF: dexos.PostOnly},
		}, an)
	must(err, "原子换单")
	fmt.Printf("原子换单 → %d 条事件 %s\n", len(ev), kinds(ev))

	// 7) 清干净
	seqs, err = c.OrderSeqs(ctx, uint32(*account), uint16(*market))
	must(err, "读挂单")
	if len(seqs) > 0 {
		an, err = c.AgentNonce(ctx, uint32(*account), api.Address())
		must(err, "读 agent nonce")
		ev, err = c.BatchCancel(ctx, api, uint32(*account), uint16(*market), seqs, an)
		must(err, "批量撤单")
		fmt.Printf("批量撤单 → %d 条事件 %s\n", len(ev), kinds(ev))
	}

	// 8) 撤销授权。撤完 agent 立刻失效 —— 是全局失效,不是标记过期。
	mn, err = c.NextNonce(ctx, uint32(*account))
	must(err, "读 master nonce")
	_, err = c.RevokeAgent(ctx, owner, uint32(*account), api.Address(), mn)
	must(err, "撤销 API 钱包")
	agents, err = c.Agents(ctx, uint32(*account))
	must(err, "列出 API 钱包")
	fmt.Printf("\n已撤销,剩余 API 钱包 %d 个\n", len(agents))

	// 9) 反证:撤销后同一把钥匙再下单必须被拒。
	//    没有这一步,前面 8 步全绿也不能说明授权真的起了作用。
	_, err = c.BatchPlace(ctx, api, uint32(*account), orders, 0)
	if err == nil {
		fmt.Println("\n✗ 撤销后仍能下单 —— 授权没有真正生效")
		os.Exit(1)
	}
	fmt.Printf("撤销后再下单 → 被拒(%v)\n\n✅ API 钱包全链路通过\n", err)
}

func kinds(ev []dexos.EventEnvelope) string {
	s := ""
	for i, e := range ev {
		if i > 0 {
			s += ","
		}
		s += e.Kind
	}
	if s == "" {
		return "(无)"
	}
	return "[" + s + "]"
}

func must(err error, what string) {
	if err != nil {
		log.Fatalf("%s 失败:%v", what, err)
	}
}
