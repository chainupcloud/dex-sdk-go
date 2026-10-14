package dexosv1

import (
	"context"
	"errors"
	"net/http"
	"reflect"
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

	// 问 4 答 3、manager 与它的子账户序号只给一个、市场没有符号:都不猜
	f.routes["/v1/vaults/4"] = serve(t, 200, "vault.json")
	if _, err := c.Vault(ctx, 4); err == nil {
		t.Fatal("问 4 答 3 要报错")
	}
	for name, edit := range map[string][2]string{
		"manager 缺子账户": {`"managerSubaccount":2`, `"managerSubaccount":null`},
		"市场没有符号":       {`{"market":1,"symbol":"ETH-USD"}]`, `{"market":1,"symbol":null}]`},
	} {
		body := strings.Replace(string(fixture(t, "vault.json")), edit[0], edit[1], 1)
		if body == string(fixture(t, "vault.json")) {
			t.Fatalf("%s: fixture 里没有 %s", name, edit[0])
		}
		f.routes["/v1/vaults/3"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
		if _, err := c.Vault(ctx, 3); err == nil {
			t.Errorf("%s 要报错", name)
		}
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
// 流水补拉按 vault 读,WS 帧里的 vault_* 条目带上金库号。
func TestStreamVaultSubscriptions(t *testing.T) {
	h := newWSHarness(t, nil)
	fh := h.history(700, 701)
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
		conn.ack(t, q, testEpoch)
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

	frame := func(id, kind string) map[string]any {
		return map[string]any{"vault": 3, "id": id, "kind": kind, "token": "USDC", "amount": "50", "symbol": nil, "orderId": nil, "ts": 1}
	}
	conn.push(t, "ledger", frame("701-0-0", "vault_deposit"), 701, testEpoch, false)
	conn.push(t, "ledger", frame("701-0-1", "trade"), 701, testEpoch, false)
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
