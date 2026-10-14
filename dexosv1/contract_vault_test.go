//go:build dexos_contract

package dexosv1

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 契约测试(金库身份,v1.1 §4.3):对一台真实 dex-os 以金库身份读、写、订阅。
//
//   - 读:金库详情 / 列表的 managerSubaccount、creatorSubaccount;按 `vault=` 读汇总、持仓、挂单、成交、流水、nonce,
//     原始响应对钉住的规格做 schema 校验;vault_* 流水带 vault;不支持金库身份的端点回 400。
//   - 写:manager 给一把新生成的 API 钱包授权(挂在 manager 认领金库所用的账户上),会话以金库身份挂远离市价的
//     只挂单再换单、撤单、断线保护、撤全部;没授权的钥匙建不了会话;最后核对本地 nonce 与服务端一致,并撤销这把 API 钱包。
//   - 金库管理(新建的金库才做):manager 主钱包建库、存入并按新得的份额赎回、加删一个黑名单市场;
//     每个写的原始回执对钉住的规格做 schema 校验。
//   - WS:按金库订阅 orders / account / positions / fills / ledger,帧按金库路由;taker 吃掉金库一张最优价只挂单,
//     核对金库的订单事件、成交帧与仓位帧,之后用一笔反向成交平掉。
//
// 默认不编译,要跑:
//
//	DEXOS_V1_URL=http://127.0.0.1:17807 DEXOS_V1_VAULT_MANAGER_KEY=0x… DEXOS_V1_TAKER_KEY=0x… \
//	  go test -tags dexos_contract -run ContractVault ./dexosv1/
//
// 两把私钥都必须是测试账户的主钱包,账户里要有 USDC。DEXOS_V1_VAULT 可选:给了就用这个金库(manager 必须是
// DEXOS_V1_VAULT_MANAGER_KEY 的钱包、金库里要有保证金);不给就由 manager 新建一个(manager 是自己、出资人白名单
// 只有自己、种子 200 USDC 从 manager 主账户扣)。DEXOS_V1_WRITE_SYMBOL 同 [TestContractStream]。
func TestContractVault(t *testing.T) {
	base, managerKey, takerKey := os.Getenv("DEXOS_V1_URL"), os.Getenv("DEXOS_V1_VAULT_MANAGER_KEY"), os.Getenv("DEXOS_V1_TAKER_KEY")
	if base == "" || managerKey == "" || takerKey == "" {
		t.Skip("没给 DEXOS_V1_URL / DEXOS_V1_VAULT_MANAGER_KEY / DEXOS_V1_TAKER_KEY")
	}
	spec := loadSpec(t)
	rec := &capture{next: http.DefaultTransport}
	c := NewClient(strings.TrimRight(base, "/"))
	c.HTTP = &http.Client{Timeout: 30 * time.Second, Transport: rec}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// conforms 原样取一次响应,对规格里 GET specPath 的 status schema 校验。
	conforms := func(path string, q url.Values, specPath string, status int) []byte {
		t.Helper()
		got, _, raw, err := c.roundTrip(ctx, path, q, accountRead)
		if err != nil || got != status {
			t.Fatalf("GET %s?%s: HTTP %d, %v: %s", path, q.Encode(), got, err, raw)
		}
		schema := responseSchema(t, spec, specPath, strconv.Itoa(status))
		if errs := validate(spec, schema, decodeJSON(t, raw), path); len(errs) > 0 {
			t.Errorf("GET %s?%s 的响应不符合钉住的规格:\n  %s", path, q.Encode(), strings.Join(errs, "\n  "))
		}
		return raw
	}

	manager, err := dexos.NewSigner(managerKey)
	if err != nil {
		t.Fatal(err)
	}
	managerMain := AccountRef{Address: manager.Address()}
	managerHead, err := c.HistoryHead(ctx, managerMain)
	if err != nil {
		t.Fatal(err)
	}
	var id uint16
	created := os.Getenv("DEXOS_V1_VAULT") == ""
	if created {
		id = contractCreateVault(t, ctx, c, manager, "200")
	} else {
		n, err := strconv.ParseUint(os.Getenv("DEXOS_V1_VAULT"), 10, 16)
		if err != nil {
			t.Fatalf("DEXOS_V1_VAULT: %v", err)
		}
		id = uint16(n)
	}
	ref := VaultAccount(id)
	vq := url.Values{"vault": {strconv.Itoa(int(id))}}

	// ── 金库详情 / 列表:manager 与它认领所用的子账户 ──
	v, err := c.Vault(ctx, id)
	if err != nil {
		t.Fatalf("Vault %d: %v", id, err)
	}
	if v.Manager == nil || *v.Manager != manager.Address().Hex() || v.ManagerSubaccount == nil || v.TradingHalt {
		t.Fatalf("金库 %d 不能用来测:manager %v / %v,熔断 %v", id, v.Manager, v.ManagerSubaccount, v.TradingHalt)
	}
	if created && (v.CreatorSubaccount == nil || *v.CreatorSubaccount != 0 || *v.ManagerSubaccount != 0) {
		t.Errorf("新建金库的 creator / manager 子账户应为主账户 0: %v / %v", v.CreatorSubaccount, *v.ManagerSubaccount)
	}
	for _, path := range []string{"/v1/vaults/" + strconv.Itoa(int(id)), "/v1/vaults"} {
		specPath := map[bool]string{true: "/v1/vaults", false: "/v1/vaults/{id}"}[path == "/v1/vaults"]
		raw := string(conforms(path, nil, specPath, 200))
		if !strings.Contains(raw, `"managerSubaccount"`) || !strings.Contains(raw, `"creatorSubaccount"`) {
			t.Errorf("GET %s 没有 managerSubaccount / creatorSubaccount", path)
		}
	}
	vaults, err := c.Vaults(ctx)
	if err != nil {
		t.Fatalf("Vaults: %v", err)
	}
	listed := false
	for _, x := range vaults {
		if x.Vault == id {
			listed = *x.ManagerSubaccount == *v.ManagerSubaccount && *x.Manager == *v.Manager
		}
	}
	if !listed {
		t.Errorf("金库列表里的金库 %d 与详情对不上", id)
	}

	// ── 以金库身份读 ──
	cat, err := c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := c.Account(ctx, ref)
	if err != nil {
		t.Fatalf("Account(vault): %v", err)
	}
	if acc.Vault == nil || *acc.Vault != id || acc.Address != "" || acc.Epoch != managerHead.Epoch {
		t.Errorf("金库账户汇总的身份 / 纪元: vault %v address %q epoch %q", acc.Vault, acc.Address, acc.Epoch)
	}
	conforms("/v1/account", vq, "/v1/account", 200)
	if _, err := c.Positions(ctx, cat, ref); err != nil {
		t.Fatalf("Positions(vault): %v", err)
	}
	conforms("/v1/account/positions", vq, "/v1/account/positions", 200)
	if _, err := c.Orders(ctx, cat, ref, ""); err != nil {
		t.Fatalf("Orders(vault): %v", err)
	}
	conforms("/v1/account/orders", vq, "/v1/account/orders", 200)
	if _, err := c.Fills(ctx, cat, ref, "", HistoryQuery{PageSize: 3, Limit: 3}); err != nil {
		t.Fatalf("Fills(vault): %v", err)
	}
	small := cloneValues(vq)
	small.Set("limit", "3")
	conforms("/v1/account/fills", small, "/v1/account/fills", 200)
	conforms("/v1/account/ledger", small, "/v1/account/ledger", 200)
	if _, err := c.Nonce(ctx, ref, nil); err != nil {
		t.Fatalf("Nonce(vault): %v", err)
	}
	conforms("/v1/account/nonce", vq, "/v1/account/nonce", 200)
	// vault_* 流水带 vault:以金库身份查 = 该金库;按 manager 查 = 对手金库。新建时种子入库两边各有一条
	vaultEntries := func(who AccountRef, q LedgerQuery) int {
		t.Helper()
		h, err := c.Ledger(ctx, who, q)
		if err != nil {
			t.Fatalf("Ledger(%s): %v", who, err)
		}
		n := 0
		for _, e := range h.Entries {
			switch {
			case !strings.HasPrefix(e.Kind, "vault_"):
				if e.Vault != nil {
					t.Errorf("%s 的非金库流水带了 vault: %+v", who, e)
				}
			case e.Vault != nil && *e.Vault == id:
				n++
			}
		}
		return n
	}
	if n := vaultEntries(ref, LedgerQuery{HistoryQuery: HistoryQuery{Limit: 200}}); created && n == 0 {
		t.Error("新建金库的流水里没有 vault = 本金库的 vault_* 条目")
	}
	if n := vaultEntries(managerMain, LedgerQuery{HistoryQuery: HistoryQuery{After: managerHead.After, Epoch: managerHead.Epoch}}); created && n == 0 {
		t.Error("manager 的流水里没有对手为本金库的 vault_* 条目")
	}
	// 不支持金库身份的端点:本包先拦,服务端 400
	if _, err := c.Agents(ctx, ref); err == nil {
		t.Error("Agents 不该接受金库身份")
	}
	if _, err := c.Portfolio(ctx, ref, Period7d); err == nil {
		t.Error("Portfolio 不该接受金库身份")
	}
	for _, path := range []string{"/v1/account/agents", "/v1/account/portfolio", "/v1/account/withdrawals"} {
		if got, _, raw, err := c.roundTrip(ctx, path, vq, accountRead); err != nil || got != http.StatusBadRequest {
			t.Errorf("GET %s?vault= 应 400,得到 %d %v: %s", path, got, err, raw)
		}
	}

	// ── 以金库身份写 ──
	m := contractMarket(t, cat)
	stranger, err := dexos.GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSession(ctx, c, stranger, ref); !errors.Is(err, ErrAgentNotAuthorized) {
		t.Fatalf("没授权的钥匙应报 ErrAgentNotAuthorized: %v", err)
	}
	gw, err := NewWallet(ctx, c, manager, *v.ManagerSubaccount)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := dexos.GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gw.ApproveAgent(ctx, agent.Address(), time.Now().Add(time.Hour), "sdk-vault-ct"); err != nil {
		t.Fatalf("ApproveAgent: %v", err)
	}
	// 最后(撤单、平仓之后)撤销这把 API 钱包,manager 账户回到测试前的样子
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := gw.RevokeAgent(ctx, agent.Address()); err != nil {
			t.Errorf("RevokeAgent: %v", err)
			return
		}
		grant := AccountRef{Address: manager.Address(), Subaccount: *v.ManagerSubaccount}
		agents, err := c.Agents(ctx, grant)
		if err != nil {
			t.Errorf("撤销后读授权列表: %v", err)
		}
		for _, a := range agents {
			if a.Address == agent.Address().Hex() {
				t.Errorf("撤销后 %s 仍在授权列表里", a.Address)
			}
		}
	}()
	s, err := NewSession(ctx, c, agent, ref)
	if err != nil {
		t.Fatalf("NewSession(vault): %v", err)
	}
	defer func() {
		if _, err := s.CancelAll(context.Background()); err != nil {
			t.Errorf("收尾撤全部: %v", err)
		}
	}()

	vaultHead, err := c.HistoryHead(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		failed []StateEvent
	)
	st, err := NewStream(c, cat, StreamOptions{
		OnState: func(e StateEvent) {
			t.Logf("订阅 %s %v → %s(%v)", e.Sub.Channel(), e.Sub.Params(), e.State, e.Err)
			var lag *LaggedError
			if e.Err != nil && !errors.As(e.Err, &lag) {
				mu.Lock()
				failed = append(failed, e)
				mu.Unlock()
			}
		},
		OnConnError: func(err error) { t.Errorf("连接错误: %v", err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	orders, accounts, positions := make(chan OrdersUpdate, 256), make(chan *Account, 256), make(chan []Position, 256)
	fills, ledger := make(chan Fill, 256), make(chan LedgerEntry, 256)
	var subs []*Subscription
	must := func(s *Subscription, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, s)
	}
	must(st.SubscribeOrders(ref, func(u OrdersUpdate) { put(orders, u) }))
	must(st.SubscribeAccount(ref, func(a *Account) { put(accounts, a) }))
	must(st.SubscribePositions(ref, func(p []Position) { put(positions, p) }))
	must(st.SubscribeFills(ref, vaultHead, func(f Fill) { put(fills, f) }))
	must(st.SubscribeLedger(ref, vaultHead, func(e LedgerEntry) { put(ledger, e) }))
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- st.Run(runCtx) }()
	defer func() {
		stop()
		<-done
	}()
	deadline := time.Now().Add(30 * time.Second)
	for _, sub := range subs {
		for sub.State() != StateLive {
			if time.Now().After(deadline) {
				t.Fatalf("%s %v 30s 内没有就绪(%s)", sub.Channel(), sub.Params(), sub.State())
			}
			time.Sleep(20 * time.Millisecond)
		}
		if p := sub.Params(); p["vault"] == nil || p["address"] != nil {
			t.Errorf("%s 的订阅参数应只按金库: %v", sub.Channel(), p)
		}
	}
	wait(t, orders, "金库 orders 快照", func(u OrdersUpdate) bool { return u.Snapshot })
	if a := wait(t, accounts, "金库 account", func(*Account) bool { return true }); a.Vault == nil || *a.Vault != id {
		t.Errorf("account 帧的身份: %+v", a.Vault)
	}

	// 远离市价的只挂单 → 换单 → 撤单 → 断线保护
	far := *m.MarkPrice / 2
	o := OrderRequest{Market: m, Side: dexos.Buy, Price: far, Lots: minLots(t, m, far), TIF: dexos.PostOnly}
	placed, err := s.Place(ctx, o)
	if err != nil {
		t.Fatalf("Place(vault): %v", err)
	}
	if it := placed.Items[0]; it.Status != "accepted" || it.OrderID == nil || it.Resting == nil || !*it.Resting {
		t.Fatalf("金库挂单没有挂上: %+v", it)
	}
	oid := *placed.Items[0].OrderID
	wait(t, orders, "金库挂单进集合", func(u OrdersUpdate) bool { return hasOrder(u.Open, oid) })
	if rest, err := c.Orders(ctx, cat, ref, m.Symbol); err != nil || !hasOrder(rest, oid) {
		t.Fatalf("REST 按金库读挂单: %v %v", rest, err)
	}
	o2 := o
	o2.Price = far - 1
	o2.ClientOrderID = fmt.Sprintf("sdk-vct-%d", time.Now().UnixMilli())
	rep, err := s.Replace(ctx, m, []uint64{oid}, []OrderRequest{o2})
	if err != nil {
		t.Fatalf("Replace(vault): %v", err)
	}
	var newID uint64
	for _, it := range rep.Items {
		if it.Kind == "cancel" && it.Status != "canceled" || it.Kind == "place" && it.Status != "accepted" {
			t.Fatalf("换单项: %+v", it)
		}
		if it.Kind == "place" {
			newID = *it.OrderID
		}
	}
	if cr, err := s.Cancel(ctx, m, newID); err != nil || cr.Items[0].Status != "canceled" {
		t.Fatalf("Cancel(vault): %+v %v", cr, err)
	}
	wait(t, orders, "金库撤单离开集合", func(u OrdersUpdate) bool {
		return u.Event != nil && u.Event.OrderID == newID && !hasOrder(u.Open, newID)
	})
	if _, err := s.ScheduleCancel(ctx, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("ScheduleCancel(vault): %v", err)
	}
	if _, err := s.ScheduleCancel(ctx, time.Time{}); err != nil {
		t.Fatalf("ScheduleCancel(vault) 清除: %v", err)
	}

	// 一笔真实成交:金库挂最优买价的只挂单,taker IOC 卖单吃掉它
	taker, err := dexos.NewSigner(takerKey)
	if err != nil {
		t.Fatal(err)
	}
	tw, err := NewWallet(ctx, c, taker, 0)
	if err != nil {
		t.Fatal(err)
	}
	price := bestInside(t, ctx, c, m, dexos.Buy)
	lots := minLots(t, m, price)
	bid, err := s.Place(ctx, OrderRequest{Market: m, Side: dexos.Buy, Price: price, Lots: lots, TIF: dexos.PostOnly})
	if err != nil || bid.Items[0].Status != "accepted" || bid.Items[0].Resting == nil || !*bid.Items[0].Resting {
		t.Fatalf("金库挂最优买价: %+v %v", bid, err)
	}
	bidID := *bid.Items[0].OrderID
	took, err := tw.PlaceOrder(ctx, OrderRequest{Market: m, Side: dexos.Sell, Price: price, Lots: lots, TIF: dexos.IOC}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("taker 吃单: %v", err)
	}
	if took.FilledLots == 0 {
		t.Fatalf("taker 没吃到金库的单: %+v", took)
	}
	defer contractUnwind(t, c, m, s, tw, took.FilledLots)
	wait(t, orders, "金库挂单成交", func(u OrdersUpdate) bool { return u.Event != nil && u.Event.OrderID == bidID && u.Event.Kind == "fill" })
	if f := wait(t, fills, "金库成交", func(f Fill) bool { return f.OrderID != nil && *f.OrderID == bidID }); f.Role != "maker" || f.Side != "buy" || f.Price != price {
		t.Errorf("金库成交帧: %+v", f)
	}
	wait(t, positions, "金库仓位", func(ps []Position) bool {
		for _, p := range ps {
			if p.Symbol == m.Symbol && p.Lots > 0 {
				return true
			}
		}
		return false
	})
	for len(ledger) > 0 {
		if e := <-ledger; e.ID == "" || e.Kind == "" || (e.Vault != nil && *e.Vault != id) {
			t.Errorf("金库流水帧: %+v", e)
		}
	}

	// 金库资金与配置(只对本测试新建的金库:manager 就是 creator,没有锁定期与赎回窗口):manager 存入、按新得的份额赎回,
	// 加一个与做市无关的黑名单市场再删掉
	if created {
		contractVaultMoney(t, ctx, c, gw, manager.Address(), id)
		var other *Market
		for i := range cat.Markets() {
			if mk := &cat.Markets()[i]; mk.Symbol != m.Symbol {
				other = mk
				break
			}
		}
		if other != nil {
			for _, kind := range []string{VaultConfigAddBlacklist, VaultConfigRemoveBlacklist} {
				if _, err := gw.ChangeVaultConfig(ctx, id, VaultConfigChange{Kind: kind, Markets: []*Market{other}}); err != nil {
					t.Fatalf("ChangeVaultConfig %s %s: %v", kind, other.Symbol, err)
				}
				v, err := c.Vault(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				listed := slices.ContainsFunc(v.Blacklist, func(s string) bool { return strings.EqualFold(s, other.Symbol) })
				if listed != (kind == VaultConfigAddBlacklist) {
					t.Errorf("%s %s 之后黑名单是 %v", kind, other.Symbol, v.Blacklist)
				}
			}
		}
	}

	// 本地 nonce 与服务端一致
	addr := agent.Address()
	if n, err := c.Nonce(ctx, ref, &addr); err != nil || n.Nonce != s.Nonce() {
		t.Errorf("金库会话 agent nonce:服务端 %+v(%v),本地 %d", n, err, s.Nonce())
	}
	aq := cloneValues(vq)
	aq.Set("signer", addr.Hex())
	conforms("/v1/account/nonce", aq, "/v1/account/nonce", 200)

	mu.Lock()
	for _, e := range failed {
		t.Errorf("订阅 %s %v 报错: %s %v", e.Sub.Channel(), e.Sub.Params(), e.State, e.Err)
	}
	mu.Unlock()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, r := range rec.got {
		if r.status != 200 {
			t.Errorf("POST %s 回 %d: %s", r.path, r.status, r.body)
			continue
		}
		// 金库写的路径带金库号,规格里是 /v1/vaults/{id}/…
		path := vaultIDPath.ReplaceAllString(r.path, "/v1/vaults/{id}/")
		if errs := validate(spec, postResponseSchema(t, spec, path, "200"), decodeJSON(t, r.body), r.path); len(errs) > 0 {
			t.Errorf("POST %s 的响应不符合钉住的规格:\n  %s\n  原文 %s", r.path, strings.Join(errs, "\n  "), r.body)
		}
	}
}

var vaultIDPath = regexp.MustCompile(`^/v1/vaults/[0-9]+/`)

// contractCreateVault owner 主钱包([Wallet.CreateVault])新建金库:manager 是自己(认领在主账户上)、出资人白名单
// 只有自己、不设熔断线与存取期限,种子 seed USDC 从主账户扣。返回新金库号。
func contractCreateVault(t *testing.T, ctx context.Context, c *Client, owner *dexos.Signer, seed string) uint16 {
	t.Helper()
	w, err := NewWallet(ctx, c, owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	me := owner.Address()
	name := fmt.Sprintf("sdk-ct-%d", time.Now().UnixMilli())
	r, err := w.CreateVault(ctx, VaultCreate{Name: name, Description: "dex-sdk-go dexosv1 金库身份契约测试", Manager: &me,
		SeedAmount: seed, DepositorWhitelist: []dexos.Address{me}})
	if err != nil {
		t.Fatalf("建金库: %v", err)
	}
	t.Logf("新建金库 %d(%s),种子 %s USDC", r.Vault, name, seed)
	return r.Vault
}

// contractMarket DEXOS_V1_WRITE_SYMBOL 或目录里第一个有标记价的永续。
func contractMarket(t *testing.T, cat *Catalog) *Market {
	t.Helper()
	if sym := os.Getenv("DEXOS_V1_WRITE_SYMBOL"); sym != "" {
		m, err := cat.Market(sym)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for i := range cat.Markets() {
		if mk := &cat.Markets()[i]; mk.Kind == "perp" && mk.MarkPrice != nil && *mk.MarkPrice > 1 {
			return mk
		}
	}
	t.Fatal("找不到有标记价的市场")
	return nil
}

// minLots 在 price 上满足最小名义额的最小手数再加 1。
func minLots(t *testing.T, m *Market, price uint32) uint64 {
	t.Helper()
	minN, err := ParseFixed(m.MinNotional, m.Scale.PriceDecimals+m.Scale.SizeDecimals)
	if err != nil {
		t.Fatal(err)
	}
	return minN.Uint64()/uint64(price) + 1
}

// bestInside side 一侧挂得进最优价的只挂单价格:买 = 最优买 + 1,卖 = 最优卖 - 1(不穿过对手价)。
func bestInside(t *testing.T, ctx context.Context, c *Client, m *Market, side dexos.Side) uint32 {
	t.Helper()
	book, err := c.Book(ctx, m, 1)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case len(book.Bids) == 0 && len(book.Asks) == 0:
		mk, err := c.Market(ctx, m.Symbol)
		if err != nil || mk.MarkPrice == nil {
			t.Fatalf("读标记价: %v", err)
		}
		return *mk.MarkPrice
	case len(book.Bids) > 0 && len(book.Asks) > 0 && book.Asks[0].Price-book.Bids[0].Price <= 1:
		t.Fatalf("%s 盘口价差只有一档,挂不进最优价;用 DEXOS_V1_WRITE_SYMBOL 换个市场", m.Symbol)
	case side == dexos.Buy && len(book.Bids) > 0:
		return book.Bids[0].Price + 1
	case side == dexos.Buy:
		return book.Asks[0].Price - 1
	case len(book.Asks) > 0:
		return book.Asks[0].Price - 1
	}
	return book.Bids[0].Price + 1
}

func hasOrder(open []Order, id uint64) bool {
	for _, o := range open {
		if o.OrderID == id {
			return true
		}
	}
	return false
}

// contractUnwind 金库挂最优卖价的只挂单、taker IOC 买单吃掉,平掉本测试开出的仓位。
func contractUnwind(t *testing.T, c *Client, m *Market, s *Session, tw *Wallet, lots uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	price := bestInside(t, ctx, c, m, dexos.Sell)
	ask, err := s.Place(ctx, OrderRequest{Market: m, Side: dexos.Sell, Price: price, Lots: lots, TIF: dexos.PostOnly})
	if err != nil || ask.Items[0].Status != "accepted" {
		t.Errorf("平仓金库挂卖: %+v %v", ask, err)
		return
	}
	buy, err := tw.PlaceOrder(ctx, OrderRequest{Market: m, Side: dexos.Buy, Price: price, Lots: lots, TIF: dexos.IOC}, time.Now().Add(time.Minute))
	if err != nil || buy.FilledLots != lots {
		t.Errorf("平仓 taker 买: %+v %v;金库多、taker 空各剩 %s 要手工平", buy, err, m.Scale.Size(lots))
		return
	}
	t.Logf("已平仓:%s × %s", m.Scale.Price(price), m.Scale.Size(lots))
}

// contractVaultMoney manager 主钱包向金库存入 10 USDC,等份额铸出(下一次折叠)后按新得的份额赎回。
func contractVaultMoney(t *testing.T, ctx context.Context, c *Client, w *Wallet, owner dexos.Address, id uint16) {
	t.Helper()
	shares := func() *big.Int {
		ps, err := c.MyVaults(ctx, owner)
		if err != nil {
			t.Fatalf("MyVaults: %v", err)
		}
		total := new(big.Int)
		for _, p := range ps {
			if p.Vault == id {
				n, ok := new(big.Int).SetString(p.Shares, 10)
				if !ok {
					t.Fatalf("金库 %d 的份额 %q 不是整数", id, p.Shares)
				}
				total.Add(total, n)
			}
		}
		return total
	}
	before := shares()
	if _, err := w.DepositToVault(ctx, id, "10"); err != nil {
		t.Fatalf("DepositToVault: %v", err)
	}
	minted := new(big.Int)
	for deadline := time.Now().Add(20 * time.Second); minted.Sign() <= 0; time.Sleep(500 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("存入 20s 后份额没有增加(之前 %s)", before)
		}
		minted.Sub(shares(), before)
	}
	if _, err := w.RedeemFromVault(ctx, id, minted.String()); err != nil {
		t.Fatalf("RedeemFromVault %s: %v", minted, err)
	}
	t.Logf("manager 向金库 %d 存入 10 USDC、赎回新得的 %s 份额", id, minted)
}
