// session 演示**最小接入**:网关地址 + 一把 API 钱包私钥,挂一张远离市价的单再撤掉。
//
//	go run ./examples/session -url http://127.0.0.1:17807 -api-key 0x…
//
// 要交易的账户不用带外告知:按 API 钱包地址反查它被授权的账户(`GET /v1/agents/{address}`)。
// 同一把钥匙被几个账户授权时,用 -owner / -subaccount 指明 —— 这里不替你挑:
// 挑错的表现是订单落到另一个账户上,而且没有任何报错。以金库身份交易用 -vault(授权挂在 manager 的账户上,
// 建会话时核对)。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"

	"github.com/chainupcloud/dex-sdk-go/dexos"
	"github.com/chainupcloud/dex-sdk-go/dexosv1"
)

func main() {
	url := flag.String("url", "", "网关地址(不带 /v1)")
	apiKey := flag.String("api-key", "", "API 钱包私钥(主钱包授权过的那把)")
	owner := flag.String("owner", "", "被授权账户的主钱包地址;一把钥匙授权给多个账户时用它指明")
	sub := flag.Uint("subaccount", 0, "子账户序号,配合 -owner")
	vault := flag.Int("vault", -1, "以金库身份交易时的金库号")
	flag.Parse()
	ctx := context.Background()
	c := dexosv1.NewClient(strings.TrimRight(*url, "/"))

	agent, err := dexos.NewSigner(*apiKey)
	must(err, "解析 API 钱包私钥")
	ref, err := pickAccount(ctx, c, agent.Address(), *owner, uint32(*sub), *vault)
	must(err, "找要交易的账户")
	fmt.Printf("API 钱包 %s → 账户 %s\n", agent.Address().Hex(), ref)

	// 会话替你读 EIP-712 域、核对授权、维护 API 钱包自己的 nonce
	s, err := dexosv1.NewSession(ctx, c, agent, ref)
	must(err, "建会话")
	acct, err := c.Account(ctx, ref)
	must(err, "读账户")
	fmt.Printf("抵押 %s USDC · 权益 %s · 持仓 %d 个 · 挂单 %d 张\n",
		acct.Collateral, acct.Equity, acct.PositionsCount, acct.OrdersCount)

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

	open, err := c.Orders(ctx, cat, ref, m.Symbol)
	must(err, "读挂单")
	fmt.Printf("%s 在挂 %d 张\n", m.Symbol, len(open))

	cr, err := s.Cancel(ctx, m, *it.OrderID)
	must(err, "撤单")
	fmt.Printf("撤单 → %s\n", cr.Items[0].Status)
	fmt.Println("\n✅ 全程只用了 URL 与一把 API 钱包私钥")
}

// pickAccount 从这把 API 钱包的授权里挑出要交易的账户。过期的授权先剔除;
// 剩下不止一个又没指明时报错并列出候选,不挑默认值。
func pickAccount(ctx context.Context, c *dexosv1.Client, agent dexos.Address, owner string, sub uint32, vault int) (dexosv1.AccountRef, error) {
	if vault >= 0 {
		return dexosv1.VaultAccount(uint16(vault)), nil
	}
	grants, err := c.AgentGrants(ctx, agent)
	if err != nil {
		return dexosv1.AccountRef{}, err
	}
	var hit []dexosv1.AgentGrant
	for _, g := range grants {
		switch {
		case g.Expired:
		case owner != "":
			if g.Vault == nil && strings.EqualFold(g.Address, owner) && g.Subaccount != nil && *g.Subaccount == sub {
				hit = append(hit, g)
			}
		default:
			hit = append(hit, g)
		}
	}
	if len(hit) != 1 {
		var all []string
		for _, g := range grants {
			all = append(all, describe(g))
		}
		return dexosv1.AccountRef{}, fmt.Errorf("匹配到 %d 个可用授权(全部授权:%s);用 -owner/-subaccount 指明", len(hit), strings.Join(all, ";"))
	}
	g := hit[0]
	if g.Vault != nil {
		return dexosv1.VaultAccount(*g.Vault), nil
	}
	addr, err := dexos.ParseAddress(g.Address)
	if err != nil {
		return dexosv1.AccountRef{}, err
	}
	return dexosv1.AccountRef{Address: addr, Subaccount: *g.Subaccount}, nil
}

func describe(g dexosv1.AgentGrant) string {
	s := g.Address
	if g.Vault != nil {
		s = fmt.Sprintf("金库 %d(manager %s)", *g.Vault, g.Address)
	} else if g.Subaccount != nil {
		s = fmt.Sprintf("%s/%d", g.Address, *g.Subaccount)
	}
	if g.Expired {
		s += " 已过期"
	}
	return s
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
