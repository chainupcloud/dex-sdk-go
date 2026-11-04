// subaccounts 演示**多子账户做市**的接入方式(dex-os#335):主钱包在网页「API」页勾选几个子账户,
// 每个子账户各生成一把 API 钱包并导出密钥文件;程序读这个文件,每把钥匙在自己的子账户上并发签单。
//
//	go run ./examples/subaccounts -keys api-wallets-xxxx.json [-url http://127.0.0.1:17807] [-rounds 5]
//
// 每一轮:每把钥匙各挂一张远离市价的买单(sigVersion 2,签名者窗口 nonce,互不排队)→ 读每个子账户的挂单,
// 核对每张单只出现在下单的那个子账户里 → 各自撤掉。跑多轮时,中途在网页上撤销某一把,
// 下一轮它会被执行层拒(UnknownAgent),其余照常。
//
// 子账户要先有保证金(网页「子账户」页从主账户划转),否则挂单被拒 InsufficientMargin。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
	"github.com/chainupcloud/dex-sdk-go/dexosv1"
)

type bot struct {
	w      dexosv1.KeyFileWallet
	ref    dexosv1.AccountRef
	writer *dexosv1.ActionWriter
	err    error
}

type outcome struct {
	orderID *uint64
	place   string
	cancel  string
}

func main() {
	keys := flag.String("keys", "", "网页「API」页导出的密钥文件")
	url := flag.String("url", "", "网关地址(不带 /v1);缺省用密钥文件里的 api")
	symbol := flag.String("symbol", "", "交易的永续市场符号;缺省挑第一个有标记价的活跃永续")
	rounds := flag.Int("rounds", 1, "跑几轮(挂单 → 核对 → 撤单)")
	interval := flag.Duration("interval", 15*time.Second, "两轮之间的间隔")
	flag.Parse()
	ctx := context.Background()

	raw, err := os.ReadFile(*keys)
	must(err, "读密钥文件")
	kf, err := dexosv1.ParseKeyFile(raw)
	must(err, "解析密钥文件")
	base := strings.TrimRight(*url, "/")
	if base == "" {
		base = kf.API
	}
	c := dexosv1.NewClient(base)
	fmt.Printf("owner %s · %d 把 API 钱包 · 网关 %s\n", kf.Owner.Hex(), len(kf.Wallets), base)

	cat, err := c.Catalog(ctx)
	must(err, "读市场目录")
	m, price, lots, err := farBid(cat, *symbol)
	must(err, "挑市场")
	fmt.Printf("市场 %s:挂买 %s × %s(标记价的一半,不会成交)\n\n", m.Symbol, m.Scale.Price(price), m.Scale.Size(lots))

	bots := make([]*bot, len(kf.Wallets))
	for i, w := range kf.Wallets {
		b := &bot{w: w, ref: w.Account(kf.Owner)}
		b.writer, b.err = dexosv1.NewActionWriter(ctx, c, w.Signer, b.ref, dexosv1.NewNonceSource(0))
		bots[i] = b
	}

	violations := 0
	for r := 1; r <= *rounds; r++ {
		if r > 1 {
			time.Sleep(*interval)
		}
		fmt.Printf("── 第 %d 轮 ──\n", r)
		out := make([]outcome, len(bots))
		var wg sync.WaitGroup
		for i, b := range bots {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out[i] = placeOne(ctx, c, b, m, price, lots)
			}()
		}
		wg.Wait()

		violations += checkIsolation(ctx, c, cat, m, bots, out)

		for i, b := range bots {
			if out[i].orderID == nil {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				out[i].cancel = cancelOne(ctx, c, b, m, *out[i].orderID)
			}()
		}
		wg.Wait()
		for i, b := range bots {
			fmt.Printf("  子账户 %-2d %-24s %s  下单:%s", b.w.Subaccount, label(b.w), short(b.w.Signer.Address()), out[i].place)
			if out[i].cancel != "" {
				fmt.Printf("  撤单:%s", out[i].cancel)
			}
			fmt.Println()
		}
	}
	if violations > 0 {
		log.Fatalf("\n❌ %d 张单出现在不该出现的子账户里", violations)
	}
	fmt.Println("\n✅ 每张单都只落在下单的那个子账户里")
}

func placeOne(ctx context.Context, c *dexosv1.Client, b *bot, m *dexosv1.Market, price uint32, lots uint64) outcome {
	if b.err != nil {
		if errors.Is(b.err, dexosv1.ErrAgentNotAuthorized) {
			return outcome{place: "没有授权(已撤销或从未授权)"}
		}
		return outcome{place: "建签名器失败:" + b.err.Error()}
	}
	a, err := b.writer.SignReplace(m, nil, nil, []dexosv1.OrderRequest{{Market: m, Side: dexos.Buy, Price: price, Lots: lots, TIF: dexos.PostOnly}}, time.Now().Add(30*time.Second))
	if err != nil {
		return outcome{place: "签名失败:" + err.Error()}
	}
	res, err := c.SendReplace(ctx, a)
	if err != nil {
		return outcome{place: describe(err)}
	}
	for _, it := range res.Items {
		if it.Kind != "place" {
			continue
		}
		if it.Status == "accepted" && it.OrderID != nil {
			return outcome{orderID: it.OrderID, place: fmt.Sprintf("挂上 #%d", *it.OrderID)}
		}
		return outcome{place: "被拒 " + it.Code + hint(it.Code)}
	}
	return outcome{place: "回执里没有下单项"}
}

func cancelOne(ctx context.Context, c *dexosv1.Client, b *bot, m *dexosv1.Market, id uint64) string {
	a, err := b.writer.SignCancel(m, []uint64{id}, time.Now().Add(30*time.Second))
	if err != nil {
		return "签名失败:" + err.Error()
	}
	res, err := c.SendCancel(ctx, a)
	if err != nil {
		return describe(err)
	}
	if len(res.Items) == 0 {
		return "回执里没有撤单项"
	}
	if it := res.Items[0]; it.Status != "canceled" {
		return "没撤掉 " + it.Code
	}
	return "已撤"
}

// checkIsolation 读每个子账户在这个市场上的挂单:本轮挂上的单只能出现在下单的那个子账户里。
func checkIsolation(ctx context.Context, c *dexosv1.Client, cat *dexosv1.Catalog, m *dexosv1.Market, bots []*bot, out []outcome) int {
	owner := map[uint64]uint32{}
	for i, b := range bots {
		if out[i].orderID != nil {
			owner[*out[i].orderID] = b.w.Subaccount
		}
	}
	if len(owner) == 0 {
		return 0
	}
	bad := 0
	for _, b := range bots {
		open, err := c.Orders(ctx, cat, b.ref, m)
		if err != nil {
			fmt.Printf("  读子账户 %d 的挂单失败:%v\n", b.w.Subaccount, err)
			continue
		}
		for _, o := range open {
			if sub, ours := owner[o.OrderID]; ours && sub != b.w.Subaccount {
				fmt.Printf("  ✗ 子账户 %d 的挂单里出现了子账户 %d 下的 #%d\n", b.w.Subaccount, sub, o.OrderID)
				bad++
			}
		}
	}
	return bad
}

func describe(err error) string {
	var e *dexosv1.Error
	if errors.As(err, &e) && e.Code != "" {
		return fmt.Sprintf("被拒 %s(HTTP %d)%s", e.Code, e.Status, hint(e.Code))
	}
	switch dexosv1.ClassifyAction(err) {
	case dexosv1.ActionUnknown:
		return "结论未知:" + err.Error()
	default:
		return "没执行:" + err.Error()
	}
}

func hint(code string) string {
	switch code {
	case "InsufficientMargin":
		return " —— 先给这个子账户划转保证金"
	case "UnknownAgent":
		return " —— 这把钥匙的授权已撤销"
	case "AgentExpired":
		return " —— 授权已到期,在网页上续期"
	}
	return ""
}

func label(w dexosv1.KeyFileWallet) string {
	if w.SubaccountName != "" && w.SubaccountName != w.Label {
		return w.Label + " (" + w.SubaccountName + ")"
	}
	return w.Label
}

func short(a dexos.Address) string {
	h := a.Hex()
	return h[:6] + "…" + h[len(h)-4:]
}

// farBid 挑市场并给出远离市价的买单:价格是标记价的一半,数量取满足最小名义额的最小值再加 1 手。
func farBid(cat *dexosv1.Catalog, symbol string) (*dexosv1.Market, uint32, uint64, error) {
	for i := range cat.Markets() {
		m := &cat.Markets()[i]
		if m.Kind != "perp" || m.Status != "Active" || m.MarkPrice == nil || *m.MarkPrice < 2 {
			continue
		}
		if symbol != "" && m.Symbol != symbol {
			continue
		}
		price := *m.MarkPrice / 2
		minN, err := dexosv1.ParseFixed(m.MinNotional, m.Scale.PriceDecimals+m.Scale.SizeDecimals)
		if err != nil {
			return nil, 0, 0, err
		}
		return m, price, minN.Uint64()/uint64(price) + 1, nil
	}
	if symbol != "" {
		return nil, 0, 0, fmt.Errorf("目录里没有带标记价的活跃永续 %s", symbol)
	}
	return nil, 0, 0, fmt.Errorf("目录里没有带标记价的活跃永续")
}

func must(err error, what string) {
	if err != nil {
		log.Fatalf("%s 失败:%v", what, err)
	}
}
