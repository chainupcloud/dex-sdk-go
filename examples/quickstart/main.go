// quickstart 走一遍 API 钱包的完整生命周期,对着真实节点跑。
//
//	go run ./examples/quickstart -url http://127.0.0.1:17807 -master-key 0x…
//
// 它同时是 SDK 的**集成自检**:金样对拍只能证明规范编码正确,而签名路径
// (secp256k1 的 v 位序、地址派生、EIP-712 域分隔符)只有对着真节点才验得了 ——
// 那三处任何一处错了,表现都是服务端拒签,静态检查看不出来。
//
// 主钱包账户里要有 USDC(挂单要占保证金)。挂的单价格是标记价的一半,不会成交;收尾全撤。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
	"github.com/chainupcloud/dex-sdk-go/dexosv1"
)

func main() {
	var (
		url        = flag.String("url", "http://127.0.0.1:8080", "网关地址(不带 /v1)")
		masterKey  = flag.String("master-key", "", "主钱包私钥(0x…);只用于授权 / 撤销 API 钱包")
		subaccount = flag.Uint("subaccount", 0, "子账户序号(0 = 主账户)")
	)
	flag.Parse()
	if *masterKey == "" {
		log.Fatal("必须给 -master-key(主钱包私钥)")
	}
	ctx := context.Background()
	c := dexosv1.NewClient(strings.TrimRight(*url, "/"))

	// 0) 指纹核对。编码版本对不上意味着规范编码变了,而那种错误只表现为「签名一律被拒」
	ex, err := c.Exchange(ctx)
	must(err, "读连接参数")
	if ex.Version.CodecVer != dexos.CodecVersion {
		log.Fatalf("网关的命令编码是 v%d,本 SDK 实现 v%d", ex.Version.CodecVer, dexos.CodecVersion)
	}
	fmt.Printf("链 %d · 命令编码 v%d\n", ex.ChainID, ex.Version.CodecVer)

	owner, err := dexos.NewSigner(*masterKey)
	must(err, "解析主钱包私钥")
	w, err := dexosv1.NewWallet(ctx, c, owner, uint32(*subaccount))
	must(err, "建主钱包写入")
	fmt.Printf("主钱包 %s · 账户 %s\n", owner.Address().Hex(), w.Account)

	// 1) 本地生成 API 钱包。私钥只在本进程里,交易所只拿到地址
	api, err := dexos.GenerateSigner()
	must(err, "生成 API 钱包")

	// 2) 主钱包签名授权。这一步用**主钱包作用域**的 nonce(Wallet 替你管)
	_, err = w.ApproveAgent(ctx, api.Address(), time.Now().Add(time.Hour), "quickstart")
	must(err, "授权 API 钱包")
	agents, err := c.Agents(ctx, w.Account)
	must(err, "列出 API 钱包")
	fmt.Printf("\n已授权 %s(1 小时);账户上共 %d 把 API 钱包\n", api.Address().Hex(), len(agents))

	// 3) 用 API 钱包交易。这一步用 **API 钱包自己的** nonce —— 与上面那个是两个计数器(Session 替你管)
	s, err := dexosv1.NewSession(ctx, c, api, w.Account)
	must(err, "建会话")
	cat, err := c.Catalog(ctx)
	must(err, "读市场目录")
	m, price, lots, err := farBid(cat)
	must(err, "挑市场")
	bid := func(px uint32) dexosv1.OrderRequest {
		return dexosv1.OrderRequest{Market: m, Side: dexos.Buy, Price: px, Lots: lots, TIF: dexos.PostOnly}
	}
	placed, err := s.Place(ctx, bid(price), bid(price-1))
	must(err, "批量下单")
	ids := orderIDs(placed)
	fmt.Printf("\n%s 批量挂买 %s / %s × %s → 订单 %v\n", m.Symbol, m.Scale.Price(price), m.Scale.Price(price-1), m.Scale.Size(lots), ids)

	// 4) 原子换单:撤掉刚才两张、挂一张更低的 —— 撤与挂在同一次状态转移里,中间没有「没有报价」的窗口
	rep, err := s.Replace(ctx, m, ids, []dexosv1.OrderRequest{bid(price - 2)})
	must(err, "原子换单")
	for _, it := range rep.Items {
		fmt.Printf("换单 %s → %s\n", it.Kind, it.Status)
	}

	// 5) 断线保护(dead man's switch):做市每轮报价顺手往后续一次,进程挂了就到点全撤
	_, err = s.ScheduleCancel(ctx, time.Now().Add(30*time.Second))
	must(err, "设置断线保护")
	_, err = s.ScheduleCancel(ctx, time.Time{})
	must(err, "清除断线保护")

	// 6) 清干净
	_, err = s.CancelAll(ctx)
	must(err, "撤全部")
	fmt.Println("断线保护设置 / 清除、撤全部 → 完成")

	// 7) 撤销授权。撤完 API 钱包立刻失效
	_, err = w.RevokeAgent(ctx, api.Address())
	must(err, "撤销 API 钱包")
	fmt.Println("\n已撤销")

	// 8) 反证:撤销后同一把钥匙再下单必须被拒。没有这一步,前面全绿也不能说明授权真的起了作用
	if _, err := s.Place(ctx, bid(price)); err == nil {
		fmt.Println("\n✗ 撤销后仍能下单 —— 授权没有真正生效")
		os.Exit(1)
	} else {
		fmt.Printf("撤销后再下单 → 被拒(%v)\n\n✅ API 钱包全链路通过\n", err)
	}
}

func orderIDs(r *dexosv1.BatchResult) []uint64 {
	var ids []uint64
	for _, it := range r.Items {
		if it.Status != "accepted" || it.OrderID == nil {
			log.Fatalf("没挂上:%+v", it)
		}
		ids = append(ids, *it.OrderID)
	}
	return ids
}

// farBid 挑一个有标记价的永续,给出远离市价的买单:价格是标记价的一半(不会成交),
// 数量取满足最小名义额的最小值再加 1 手(名义额 = ticks × lots / 10^(priceDecimals+sizeDecimals))。
func farBid(cat *dexosv1.Catalog) (*dexosv1.Market, uint32, uint64, error) {
	for i := range cat.Markets() {
		m := &cat.Markets()[i]
		if m.Kind != "perp" || m.Status != "Active" || m.MarkPrice == nil || *m.MarkPrice < 8 {
			continue
		}
		price := *m.MarkPrice / 2
		minN, err := dexosv1.ParseFixed(m.MinNotional, m.Scale.PriceDecimals+m.Scale.SizeDecimals)
		if err != nil {
			return nil, 0, 0, err
		}
		return m, price, minN.Uint64()/uint64(price-2) + 1, nil
	}
	return nil, 0, 0, fmt.Errorf("目录里没有带标记价的活跃永续")
}

func must(err error, what string) {
	if err != nil {
		log.Fatalf("%s 失败:%v", what, err)
	}
}
