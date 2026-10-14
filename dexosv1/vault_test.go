package dexosv1

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 金库详情与列表:manager / creator 认领所用的子账户序号、交易限制按符号给出;待认领的金库没有 manager。
func TestVaultDetailAndList(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	f.routes["/v1/vaults/3"] = serve(t, 200, "vault.json")
	f.routes["/v1/vaults"] = serve(t, 200, "vaults.json")
	v, err := c.Vault(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if v.Vault != 3 || *v.Manager != ownerAddr || *v.ManagerSubaccount != 2 || *v.Creator != testAddr || *v.CreatorSubaccount != 1 ||
		v.TradingHalt || !v.HasWhitelist || *v.ManagerLiqPortionPpm != 200000 {
		t.Fatalf("金库详情: %+v", v)
	}
	if !reflect.DeepEqual(v.Whitelist, []string{"BTC-USD", "ETH-USD"}) || !reflect.DeepEqual(v.Blacklist, []string{"ETH-USD"}) ||
		!reflect.DeepEqual(v.MaxLeverages, map[string]uint32{"BTC-USD": 5}) || !reflect.DeepEqual(v.DepositorWhitelist, []string{ownerAddr}) {
		t.Fatalf("金库交易限制: %+v", v)
	}
	vs, err := c.Vaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || *vs[0].ManagerSubaccount != 2 {
		t.Fatalf("金库列表: %+v", vs)
	}
	if fresh := vs[1]; fresh.Vault != 4 || fresh.Manager != nil || fresh.ManagerSubaccount != nil || fresh.HasWhitelist ||
		fresh.Whitelist != nil || fresh.ManagerLiqPortionPpm != nil || *fresh.CreatorSubaccount != 1 {
		t.Fatalf("待认领的金库: %+v", fresh)
	}

	// 问 4 答 3、manager 与它的子账户序号只给一个:都不猜
	f.routes["/v1/vaults/4"] = serve(t, 200, "vault.json")
	if _, err := c.Vault(ctx, 4); err == nil {
		t.Fatal("问 4 答 3 要报错")
	}
	body := strings.Replace(string(fixture(t, "vault.json")), `"managerSubaccount":2`, `"managerSubaccount":null`, 1)
	f.routes["/v1/vaults/3"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
	if _, err := c.Vault(ctx, 3); err == nil {
		t.Error("manager 缺子账户要报错")
	}
}

// 以金库身份读(v1.1 §4.3):只带 vault=,不带 address / subaccount;响应里的身份是 vault,vault_* 流水带 vault。
func TestVaultAddressedReads(t *testing.T) {
	f, c := newFake(t)
	cat := catalog(t, f, c)
	ctx := context.Background()
	ref := VaultAccount(3)
	f.routes["/v1/account"] = serve(t, 200, "account_vault.json")
	f.routes["/v1/account/positions"] = serve(t, 200, "positions.json")
	f.routes["/v1/account/orders"] = serve(t, 200, "orders.json")
	f.routes["/v1/account/fills"] = serve(t, 200, "fills_p2.json")
	f.routes["/v1/account/ledger"] = serve(t, 200, "ledger_vault.json")
	f.routes["/v1/account/nonce"] = serve(t, 200, "w_nonce_vault.json")

	a, err := c.Account(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if a.Vault == nil || *a.Vault != 3 || a.Address != "" || a.Equity != "200" {
		t.Fatalf("金库账户汇总: %+v", a)
	}
	if _, err := c.Positions(ctx, cat, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Orders(ctx, cat, ref, "ETH-USD"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fills(ctx, cat, ref, "", HistoryQuery{}); err != nil {
		t.Fatal(err)
	}
	l, err := c.Ledger(ctx, ref, LedgerQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries) != 2 || l.Entries[0].Kind != "vault_deposit" || l.Entries[0].Vault == nil || *l.Entries[0].Vault != 3 || l.Entries[1].Vault != nil {
		t.Fatalf("金库流水的 vault: %+v", l.Entries)
	}
	agent := mustAddr(t, agentAddr)
	n, err := c.Nonce(ctx, ref, &agent)
	if err != nil {
		t.Fatal(err)
	}
	if n.SigningAccountID != vaultMainID || n.Nonce != 9 {
		t.Fatalf("金库 nonce: %+v", n)
	}
	reqs := f.requests()[1:] // 第一个是市场目录
	if len(reqs) != 6 {
		t.Fatalf("应发 6 个读请求,发了 %d 个", len(reqs))
	}
	for _, r := range reqs {
		if q := r.URL.Query(); q.Get("vault") != "3" || q.Has("address") || q.Has("subaccount") {
			t.Errorf("%s 的寻址参数应只有 vault: %v", r.URL.Path, q)
		}
	}

	// 不接受金库身份的端点、金库号同时带地址 / 子账户:本地报错,不发请求
	if _, err := c.Agents(ctx, ref); err == nil {
		t.Error("Agents 不接受金库身份")
	}
	if _, err := c.Portfolio(ctx, ref, Period7d); err == nil {
		t.Error("Portfolio 不接受金库身份")
	}
	withSub, withAddr := VaultAccount(3), VaultAccount(3)
	withSub.Subaccount, withAddr.Address = 1, mustAddr(t, testAddr)
	for _, bad := range []AccountRef{withSub, withAddr} {
		if _, err := c.Account(ctx, bad); err == nil {
			t.Errorf("%+v 应报错", bad)
		}
	}
	if got := len(f.requests()); got != 7 {
		t.Fatalf("本地拒绝不该发请求,共发了 %d 个", got)
	}
}

// vaultWriteFake 金库 3(manager = ownerAddr,认领在子账户 2 上)与 manager 那个账户上的授权列表。
func vaultWriteFake(t *testing.T, agents string) (*writeFake, *Client) {
	t.Helper()
	f, c := newWriteFake(t)
	f.routes["/v1/vaults/3"] = serve(t, 200, "vault.json")
	f.routes["/v1/account/agents"] = func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("address") != ownerAddr || q.Get("subaccount") != "2" || q.Has("vault") {
			t.Errorf("授权应在 manager 认领金库所用的账户上查: %v", q)
		}
		_, _ = w.Write([]byte(`{"address":"` + ownerAddr + `","subaccount":2,"agents":` + agents + `}`))
	}
	f.routes["/v1/account/nonce"] = func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("vault") != "3" || q.Get("signer") != agentAddr || q.Has("address") {
			t.Errorf("金库会话的 nonce 应按 vault + signer 取: %v", q)
		}
		serve(t, 200, "w_nonce_vault.json")(w, r)
	}
	return f, c
}

// 以金库身份写:信封的 account 是 manager 主钱包、带 vault 不带 subaccount;内层命令的 account 是金库主账户,
// 假网关按请求体重建命令、重算 AgentExec 摘要并恢复出 API 钱包。
func TestVaultSessionSignsAsVault(t *testing.T) {
	f, c := vaultWriteFake(t, `[{"address":"`+agentAddr+`","label":"mm","validUntil":null,"expired":false}]`)
	ctx := context.Background()
	cat, err := c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(ctx, c, signer(t, agentKey), VaultAccount(3))
	if err != nil {
		t.Fatal(err)
	}
	if s.Nonce() != 9 {
		t.Fatalf("金库会话的 agent nonce 应取 9,得到 %d", s.Nonce())
	}
	btc, _ := cat.Market("BTC-USD")
	f.answer(t, "/v1/orders", 200, "w_batch_place.json")
	if _, err := s.Place(ctx,
		OrderRequest{Market: btc, Side: dexos.Buy, Price: 65000, Lots: 500, TIF: dexos.GTC},
		OrderRequest{Market: btc, Side: dexos.Sell, Price: 66000, Lots: 250, TIF: dexos.PostOnly, ReduceOnly: true, ClientOrderID: "q-1"},
	); err != nil {
		t.Fatal(err)
	}
	req := f.last(t)
	conformsRequest(t, "/v1/orders", req.body)
	items := req.body.([]any)
	orders := make([]dexos.BatchOrder, len(items))
	for i, it := range items {
		env := m(it)
		if env["account"] != ownerAddr || num(t, env["vault"]) != 3 || env["signer"] != agentAddr || num(t, env["nonce"]) != 9 {
			t.Fatalf("第 %d 项信封: %v", i, env)
		}
		if _, ok := env["subaccount"]; ok {
			t.Fatalf("金库信封不能带 subaccount(网关 400): %v", env)
		}
		orders[i] = gatewayOrder(t, cat, env)
	}
	first := m(items[0])
	digest := testDomain.AgentExecHash(dexos.EncodeBatchPlace(vaultMainID, num(t, first["nowMs"]), orders), 9)
	if recoverAddr(t, digest, first["signature"].(string)) != agentAddr {
		t.Fatal("签名恢复不出 API 钱包:内层命令的 account 应是金库主账户")
	}

	f.answer(t, "/v1/orders/cancel-all", 200, "w_cancel_all.json")
	if _, err := s.CancelAll(ctx); err != nil {
		t.Fatal(err)
	}
	body := m(f.last(t).body)
	conformsRequest(t, "/v1/orders/cancel-all", body)
	if _, ok := body["subaccount"]; ok || num(t, body["vault"]) != 3 || body["account"] != ownerAddr || s.Nonce() != 11 {
		t.Fatalf("撤全部的信封 %v,nonce %d", body, s.Nonce())
	}
}

// 金库待认领、API 钱包没授权在 manager 认领所用的账户上:建不了会话,不发写请求。
func TestVaultSessionRejections(t *testing.T) {
	ctx := context.Background()
	f, c := vaultWriteFake(t, `[]`)
	if _, err := NewSession(ctx, c, signer(t, agentKey), VaultAccount(3)); !errors.Is(err, ErrAgentNotAuthorized) {
		t.Fatalf("没授权应报 ErrAgentNotAuthorized: %v", err)
	}
	unclaimed := strings.NewReplacer(`"manager":"`+ownerAddr+`"`, `"manager":null`, `"managerSubaccount":2`, `"managerSubaccount":null`).
		Replace(string(fixture(t, "vault.json")))
	f.routes["/v1/vaults/3"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(unclaimed)) }
	if _, err := NewSession(ctx, c, signer(t, agentKey), VaultAccount(3)); err == nil || !strings.Contains(err.Error(), "待认领") {
		t.Fatalf("待认领的金库应报错: %v", err)
	}
	bad := VaultAccount(3)
	bad.Subaccount = 2
	if _, err := NewSession(ctx, c, signer(t, agentKey), bad); err == nil {
		t.Fatal("金库号同时带子账户应报错")
	}
	if f.count() != 0 {
		t.Fatalf("不该有写请求,发了 %d 个", f.count())
	}
}

// 按金库订阅私有频道:订阅参数只有 vault;帧按 vault 路由,与同一连接上按地址的订阅互不串;
// 流水补拉按 vault 读,WS 帧的条目与 REST 相同(vault_* 条目带金库号)。
func TestStreamVaultSubscriptions(t *testing.T) {
	h := newWSHarness(t, nil)
	fh := h.history(701)
	fh.add("/v1/account/ledger", ledgerRow("700-0-0"))
	vault := VaultAccount(3)
	if _, err := h.st.SubscribeOrders(AccountRef{Vault: vault.Vault, Subaccount: 1}, func(OrdersUpdate) {}); err == nil {
		t.Fatal("金库号同时带子账户应拒绝")
	}
	vaultOrders, walletOrders := make(chan OrdersUpdate, 8), make(chan OrdersUpdate, 8)
	accounts, entries := make(chan *Account, 8), make(chan LedgerEntry, 8)
	vo, _ := h.st.SubscribeOrders(vault, func(u OrdersUpdate) { vaultOrders <- u })
	wo, _ := h.st.SubscribeOrders(h.ref, func(u OrdersUpdate) { walletOrders <- u })
	as, _ := h.st.SubscribeAccount(vault, func(a *Account) { accounts <- a })
	ls, _ := h.st.SubscribeLedger(vault, HistoryCursor{After: "699-0-0", Epoch: testEpoch}, func(e LedgerEntry) { entries <- e })
	h.run()
	conn := h.fw.accept(t)
	var vaultReqs []wsReq
	for range 4 {
		q := conn.next(t)
		if q.Params["vault"] != nil {
			vaultReqs = append(vaultReqs, q)
		}
		conn.ack(t, q, testEpoch, 700)
	}
	for _, q := range vaultReqs {
		if len(q.Params) != 2 || q.Params["vault"] != float64(3) {
			t.Errorf("按金库订阅的参数应只有 channel + vault: %v", q.Params)
		}
	}
	if len(vaultReqs) != 3 {
		t.Fatalf("应有 3 个按金库的订阅,得到 %d", len(vaultReqs))
	}
	if e := recv(t, entries); e.ID != "700-0-0" {
		t.Fatalf("补拉 %+v", e)
	}
	if q := fh.asked("/v1/account/ledger")[0]; q.Get("vault") != "3" || q.Has("address") {
		t.Fatalf("流水补拉应按金库读: %v", q)
	}

	conn.push(t, "orders", map[string]any{"vault": 3, "orders": []any{}}, 700, testEpoch, true)
	conn.push(t, "orders", map[string]any{"address": testAddr, "subaccount": 0, "orders": []any{}}, 700, testEpoch, true)
	conn.push(t, "account", jsonObj(t, fixture(t, "account_vault.json"), nil), 700, testEpoch, true)
	for _, s := range []*Subscription{vo, wo, as, ls} {
		h.waitState(s, StateLive)
	}
	recv(t, vaultOrders)
	recv(t, walletOrders)
	if a := recv(t, accounts); a.Vault == nil || *a.Vault != 3 {
		t.Fatalf("account 帧的身份: %+v", a)
	}
	conn.push(t, "orders", map[string]any{"vault": 3, "orderId": "9", "clientOrderId": nil, "symbol": "ETH-USD", "event": "accepted",
		"status": "open", "side": "buy", "price": "2492.6", "size": "0.1", "filled": "0", "remaining": "0.1", "ts": 1}, 701, testEpoch, false)
	if u := recv(t, vaultOrders); u.Event == nil || u.Event.OrderID != 9 || len(u.Open) != 1 {
		t.Fatalf("金库订单事件: %+v", u)
	}
	quiet(t, walletOrders, 30*time.Millisecond)

	// 一条命令一帧:身份是 vault,条目与 REST 相同 —— vault_* 条目带 vault,其余不带
	deposit, trade := ledgerRow("701-0-0"), ledgerRow("701-0-1")
	deposit["kind"], deposit["vault"] = "vault_deposit", 3
	conn.push(t, "ledger", map[string]any{"vault": 3, "entries": []any{deposit, trade}}, 701, testEpoch, false)
	if e := recv(t, entries); e.ID != "701-0-0" || e.Vault == nil || *e.Vault != 3 {
		t.Fatalf("金库 vault_* 流水帧: %+v", e)
	}
	if e := recv(t, entries); e.ID != "701-0-1" || e.Vault != nil {
		t.Fatalf("非 vault_* 流水帧不该带 vault: %+v", e)
	}
	if len(h.connErrors()) != 0 {
		t.Fatalf("连接错误: %v", h.connErrors())
	}
}

// gatewayVaultDigest 假网关按金库写的请求体重建待签结构体,做法同 dex-os v1_write.rs:十进制金额 / 份额按 6 位换成
// 原子单位,比例换成 ppm,市场符号换成市场号;改配置时与 kind 无关的字段置零。
func gatewayVaultDigest(t *testing.T, cat *Catalog, path string, owner dexos.Address, env map[string]any, nonce uint64) []byte {
	t.Helper()
	atoms := func(k string) *big.Int {
		s, _ := env[k].(string)
		if s == "" {
			return new(big.Int)
		}
		v, err := ParseFixed(s, 6)
		if err != nil {
			t.Fatalf("%s %s %q: %v", path, k, s, err)
		}
		return v
	}
	u := func(k string) uint64 {
		if env[k] == nil {
			return 0
		}
		return num(t, env[k])
	}
	if path == "/v1/vaults" {
		mv := managedVault{owner: owner, accountID: signingID, profitSharePpm: uint32(atoms("profitShare").Uint64()),
			depositMinDurationMs: u("depositMinDurationMs"), managerMinPortionPpm: uint32(atoms("managerMinPortion").Uint64()),
			managerLiqPortionPpm: uint32(atoms("managerLiqPortion").Uint64()), withdrawWindowS: uint32(u("withdrawWindowS")),
			withdrawDurationS: uint32(u("withdrawDurationS")), depositCap: atoms("depositCap"), seedAmount: atoms("seedAmount"), nonce: nonce}
		if s, ok := env["manager"].(string); ok {
			mv.manager = mustAddr(t, s)
		}
		for _, a := range asSlice(env["depositorWhitelist"]) {
			mv.depositorWhitelist = append(mv.depositorWhitelist, mustAddr(t, a.(string)))
		}
		return createManagedVaultHash(testDomain, mv)
	}
	id, action, _ := strings.Cut(strings.TrimPrefix(path, "/v1/vaults/"), "/")
	n, err := strconv.ParseUint(id, 10, 16)
	if err != nil {
		t.Fatalf("金库写的路径 %s", path)
	}
	vault := uint16(n)
	switch action {
	case "deposit":
		return vaultAmountHash(testDomain, vaultDepositType, owner, vault, signingID, atoms("amount"), nonce)
	case "redeem":
		return vaultAmountHash(testDomain, vaultRedeemType, owner, vault, signingID, atoms("shares"), nonce)
	case "claim":
		return vaultAmountHash(testDomain, claimVaultManagerType, owner, vault, signingID, atoms("amount"), nonce)
	case "config":
		kind := vaultConfigKinds[env["kind"].(string)]
		var ids []uint16
		for _, s := range asSlice(env["markets"]) {
			mk, err := cat.Market(s.(string))
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, mk.SigningMarketID)
		}
		limit, lev, markets := new(big.Int), uint32(0), make([]byte, 32)
		if kind == 0 {
			limit = atoms("cap")
		}
		if kind == 6 {
			lev = uint32(u("maxLeverage"))
		}
		if kind != 0 && kind != 3 {
			markets = marketsHash(ids)
		}
		return vaultConfigChangeHash(testDomain, owner, vault, signingID, kind, limit, lev, markets, nonce)
	}
	t.Fatalf("假网关不会算 %s 的请求身份", path)
	return nil
}

// 主钱包的金库写与撤销授权:签 dex-os 的具名结构体,假网关按请求体重建验签;请求体符合钉住的规格,回执的金库号 /
// API 钱包要对得上;本地能判的错不发请求、不动 nonce。
func TestWalletVaultWrites(t *testing.T) {
	ctx := context.Background()
	f, c := newWriteFake(t)
	cat, err := c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	btc, _ := cat.Market("BTC-USD")
	eth, _ := cat.Market("ETH-USD")
	w, err := NewWallet(ctx, c, signer(t, ownerKey), 0)
	if err != nil {
		t.Fatal(err)
	}
	owner, other, agent := mustAddr(t, ownerAddr), mustAddr(t, testAddr), mustAddr(t, agentAddr)
	// sent 最近一个写请求:符合规格、签名能按请求体恢复出主钱包
	sent := func(path, specPath string) map[string]any {
		t.Helper()
		req := f.last(t)
		if req.path != path {
			t.Fatalf("应写 %s,写的是 %s", path, req.path)
		}
		body := m(req.body)
		conformsRequest(t, specPath, body)
		var digest []byte
		if path == "/v1/account/agents/revoke" {
			digest = testDomain.RevokeAgentHash(owner, signingID, mustAddr(t, body["agent"].(string)), num(t, body["nonce"]))
		} else {
			digest = gatewayVaultDigest(t, cat, path, owner, body, num(t, body["nonce"]))
		}
		if body["account"] != ownerAddr || num(t, body["subaccount"]) != 0 || body["signer"] != nil || body["vault"] != nil ||
			recoverAddr(t, digest, body["signature"].(string)) != ownerAddr {
			t.Fatalf("%s 的信封 / 签名: %v", path, body)
		}
		return body
	}

	f.answer(t, "/v1/vaults", 200, "w_vault_create.json")
	created, err := w.CreateVault(ctx, VaultCreate{Name: "sdk-vault", Description: "d", Manager: &owner, ProfitSharePpm: 200000,
		DepositMinDurationMs: 86400000, ManagerMinPortionPpm: 100000, ManagerLiqPortionPpm: 50000, WithdrawWindowS: 604800,
		WithdrawDurationS: 86400, DepositCap: "1000", SeedAmount: "200.5", DepositorWhitelist: []dexos.Address{other, owner, other}})
	if err != nil {
		t.Fatal(err)
	}
	body := sent("/v1/vaults", "/v1/vaults")
	if created.Vault != 5 || created.Request.Scope != "account:7" || body["profitShare"] != "0.2" || body["managerMinPortion"] != "0.1" ||
		body["managerLiqPortion"] != "0.05" || body["depositCap"] != "1000" || body["seedAmount"] != "200.5" || body["manager"] != ownerAddr ||
		len(asSlice(body["depositorWhitelist"])) != 3 {
		t.Fatalf("建库: %+v / %v", created, body)
	}
	// 待认领、什么都不设:可选字段不上线,签名里是 0 / 零地址 / 全零摘要
	if _, err := w.CreateVault(ctx, VaultCreate{Name: "fresh", SeedAmount: "0"}); err != nil {
		t.Fatal(err)
	}
	body = sent("/v1/vaults", "/v1/vaults")
	for _, k := range []string{"manager", "profitShare", "depositCap", "seedAmount", "depositorWhitelist", "withdrawWindowS"} {
		if _, ok := body[k]; ok {
			t.Fatalf("没设的 %s 不该上线: %v", k, body)
		}
	}

	f.answer(t, "/v1/vaults/3/deposit", 200, "w_vault_deposit.json")
	if _, err := w.DepositToVault(ctx, 3, "100.50"); err != nil {
		t.Fatal(err)
	}
	if body = sent("/v1/vaults/3/deposit", "/v1/vaults/{id}/deposit"); body["amount"] != "100.5" {
		t.Fatalf("存入: %v", body)
	}
	f.answer(t, "/v1/vaults/3/redeem", 200, "w_vault_redeem.json")
	if _, err := w.RedeemFromVault(ctx, 3, "123456789"); err != nil {
		t.Fatal(err)
	}
	if body = sent("/v1/vaults/3/redeem", "/v1/vaults/{id}/redeem"); body["shares"] != "123.456789" {
		t.Fatalf("赎回的份额按 6 位小数上线: %v", body)
	}
	f.answer(t, "/v1/vaults/4/claim", 200, "w_vault_claim.json")
	if _, err := w.ClaimVault(ctx, 4, "50"); err != nil {
		t.Fatal(err)
	}
	sent("/v1/vaults/4/claim", "/v1/vaults/{id}/claim")

	f.answer(t, "/v1/vaults/3/config", 200, "w_vault_config.json")
	for _, cc := range []VaultConfigChange{
		{Kind: VaultConfigAddBlacklist, Markets: []*Market{eth, btc}},
		{Kind: VaultConfigDepositCap, Cap: "500"},
		{Kind: VaultConfigDepositCap},
		{Kind: VaultConfigSetMaxLeverage, Markets: []*Market{btc}, MaxLeverage: 5},
		{Kind: VaultConfigClearWhitelist},
	} {
		if _, err := w.ChangeVaultConfig(ctx, 3, cc); err != nil {
			t.Fatalf("%+v: %v", cc, err)
		}
		body = sent("/v1/vaults/3/config", "/v1/vaults/{id}/config")
		if body["kind"] != cc.Kind || len(asSlice(body["markets"])) != len(cc.Markets) {
			t.Fatalf("改配置 %+v: %v", cc, body)
		}
	}

	f.answer(t, "/v1/account/agents/revoke", 200, "w_revoke.json")
	if _, err := w.RevokeAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if body = sent("/v1/account/agents/revoke", "/v1/account/agents/revoke"); body["agent"] != agentAddr {
		t.Fatalf("撤销授权: %v", body)
	}
	if w.Nonce() != 2+11 {
		t.Fatalf("十一次成功写之后 nonce 应为 13,得到 %d", w.Nonce())
	}

	// 本地能判的错:不发请求、不动 nonce
	before := f.count()
	for name, call := range map[string]func() error{
		"没名字": func() error { _, err := w.CreateVault(ctx, VaultCreate{Name: " "}); return err },
		"种子没对齐": func() error {
			_, err := w.CreateVault(ctx, VaultCreate{Name: "x", SeedAmount: "1.0000001"})
			return err
		},
		"存入 0":    func() error { _, err := w.DepositToVault(ctx, 3, "0"); return err },
		"存入负数":    func() error { _, err := w.DepositToVault(ctx, 3, "-1"); return err },
		"认领不是十进制": func() error { _, err := w.ClaimVault(ctx, 4, "1e3"); return err },
		"份额带小数":   func() error { _, err := w.RedeemFromVault(ctx, 3, "1.5"); return err },
		"份额 0":    func() error { _, err := w.RedeemFromVault(ctx, 3, "0"); return err },
		"未知种类":    func() error { _, err := w.ChangeVaultConfig(ctx, 3, VaultConfigChange{Kind: "nope"}); return err },
		"清白名单带市场": func() error {
			_, err := w.ChangeVaultConfig(ctx, 3, VaultConfigChange{Kind: VaultConfigClearWhitelist, Markets: []*Market{btc}})
			return err
		},
		"加黑名单带上限": func() error {
			_, err := w.ChangeVaultConfig(ctx, 3, VaultConfigChange{Kind: VaultConfigAddBlacklist, Markets: []*Market{btc}, Cap: "1"})
			return err
		},
		"上限带杠杆": func() error {
			_, err := w.ChangeVaultConfig(ctx, 3, VaultConfigChange{Kind: VaultConfigDepositCap, MaxLeverage: 3})
			return err
		},
		"市场为空": func() error {
			_, err := w.ChangeVaultConfig(ctx, 3, VaultConfigChange{Kind: VaultConfigAddWhitelist, Markets: []*Market{nil}})
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s 应本地报错", name)
		}
	}
	if f.count() != before || w.Nonce() != 13 {
		t.Fatalf("本地报错不该发请求、动 nonce: 请求 %d → %d,nonce %d", before, f.count(), w.Nonce())
	}

	// 回执的金库号对不上:写已执行但说不清是哪个金库,按结论未知锁住
	f.answer(t, "/v1/vaults/9/deposit", 200, "w_vault_deposit.json")
	if _, err := w.DepositToVault(ctx, 9, "1"); !errors.Is(err, ErrSessionBlocked) || w.Pending() == nil {
		t.Fatalf("回执金库号对不上应锁住: %v", err)
	}
}
