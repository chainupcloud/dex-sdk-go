package dexosv1

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

const vaultAgentGrant = `[{"address":"` + agentAddr + `","scope":"account","label":"mm","validUntil":null,"expired":false}]`

func newVaultActionWriter(t *testing.T, agents string, now int64) (*writeFake, *ActionWriter, *Market) {
	t.Helper()
	f, c := vaultWriteFake(t, agents)
	w, err := NewActionWriter(context.Background(), c, signer(t, agentKey), VaultAccount(3), NewNonceSource(0))
	if err != nil {
		t.Fatal(err)
	}
	w.now = func() time.Time { return time.UnixMilli(now) }
	cat, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	btc, err := cat.Market("BTC-USD")
	if err != nil {
		t.Fatal(err)
	}
	return f, w, btc
}

func checkVaultAction(t *testing.T, f *writeFake, path string, a *SignedAction, expiry uint64) {
	t.Helper()
	if a.Path != path {
		t.Fatalf("路径 %s,应为 %s", a.Path, path)
	}
	body := decodeJSON(t, a.Body)
	conformsRequest(t, path, body)
	envs := []any{body}
	if items, ok := body.([]any); ok {
		envs = items
	}
	for i, it := range envs {
		env := m(it)
		if num(t, env["sigVersion"]) != 2 || env["account"] != ownerAddr || num(t, env["vault"]) != 3 || env["signer"] != agentAddr ||
			num(t, env["nonce"]) != a.Request.Nonce || num(t, env["expiresAt"]) != expiry {
			t.Fatalf("%s 第 %d 项的金库 v2 信封: %v", path, i, env)
		}
		if _, ok := env["subaccount"]; ok {
			t.Fatalf("%s 第 %d 项:金库信封不能带 subaccount(网关 400): %v", path, i, env)
		}
	}
	digest, who := gatewayAction(t, f.cat, path, body)
	if hex0x(digest) != a.Request.SigningHash || who != mustAddr(t, agentAddr) || recoverAddr(t, digest, m(envs[0])["signature"].(string)) != agentAddr {
		t.Fatalf("%s:网关按请求体重算的 VaultAction 摘要 %s 恢复不出 API 钱包,SDK 签的是 %s", path, hex0x(digest), a.Request.SigningHash)
	}
	if a.Request.Scope != "signer:"+agentAddr || a.Request.RequestID != gatewayRequestID(t, f.cat, path, body) || a.Request.CommandHash != "" {
		t.Fatalf("%s 的请求身份: %+v", path, a.Request)
	}
}

func TestVaultActionWriterSignsEveryTradingWrite(t *testing.T) {
	const now, expiry = 1793680000123, 1793680005123
	f, w, btc := newVaultActionWriter(t, vaultAgentGrant, now)
	ctx := context.Background()
	if w.Owner() != mustAddr(t, ownerAddr) {
		t.Fatalf("金库写的 account 应是 manager 的主钱包: %s", w.Owner().Hex())
	}
	until := time.UnixMilli(expiry)

	f.answer(t, "/v1/orders", 200, "w_batch_place.json")
	place, err := w.SignPlace([]OrderRequest{
		{Market: btc, Side: dexos.Buy, Price: 65000, Lots: 500, TIF: dexos.GTC},
		{Market: btc, Side: dexos.Sell, Price: 66000, Lots: 250, TIF: dexos.PostOnly, ReduceOnly: true, ClientOrderID: "q-1"},
	}, until)
	if err != nil {
		t.Fatal(err)
	}
	checkVaultAction(t, f, "/v1/orders", place, expiry)
	placed, err := w.Client.SendPlace(ctx, place)
	if err != nil {
		t.Fatal(err)
	}
	if len(placed.Items) != 2 || *placed.Items[0].OrderID != 101 || placed.Items[1].Code != "InsufficientMargin" || placed.Request.RequestID != place.Request.RequestID {
		t.Fatalf("下单回执: %+v", placed)
	}

	f.answer(t, "/v1/orders/replace", 200, "w_replace_modify.json")
	replace, err := w.SignReplace(btc, []uint64{104},
		[]ModifyRequest{{OrderID: 101, Price: 64950, Lots: 500, TIF: dexos.PostOnly, ReduceOnly: true}, {OrderID: 103, Price: 65100, Lots: 500, TIF: dexos.GTC}},
		[]OrderRequest{{Market: btc, Side: dexos.Buy, Price: 64800, Lots: 100, TIF: dexos.GTC, ClientOrderID: "p-1"}}, until)
	if err != nil {
		t.Fatal(err)
	}
	checkVaultAction(t, f, "/v1/orders/replace", replace, expiry)
	replaced, err := w.Client.SendReplace(ctx, replace)
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced.Items) != 4 || replaced.Items[2].Kind != "modify" || *replaced.Items[2].OrderID != 105 {
		t.Fatalf("换单回执: %+v", replaced.Items)
	}

	f.answer(t, "/v1/orders/cancel", 200, "w_batch_cancel.json")
	cancels, err := w.SignCancel(btc, []uint64{101, 999999}, until)
	if err != nil {
		t.Fatal(err)
	}
	checkVaultAction(t, f, "/v1/orders/cancel", cancels, expiry)
	if _, err := w.Client.SendCancel(ctx, cancels); err != nil {
		t.Fatal(err)
	}
	one, err := w.SignCancelAt(btc, replace.Request.Nonce, []uint64{106}, until)
	if err != nil {
		t.Fatal(err)
	}
	checkVaultAction(t, f, "/v1/orders/cancel", one, expiry)

	f.answer(t, "/v1/orders/cancel-all", 200, "w_cancel_all.json")
	all, err := w.SignCancelAll(until)
	if err != nil {
		t.Fatal(err)
	}
	checkVaultAction(t, f, "/v1/orders/cancel-all", all, expiry)
	if num(t, m(decodeJSON(t, all.Body))["nowMs"]) != now {
		t.Fatalf("撤全部要带 nowMs(触发时刻 = nowMs): %s", all.Body)
	}
	r, err := w.Client.SendCancelAll(ctx, all)
	if err != nil {
		t.Fatal(err)
	}
	if r.Seq != 507 || r.Request.RequestID != all.Request.RequestID {
		t.Fatalf("撤全部回执: %+v", r)
	}

	nonces := []uint64{place.Request.Nonce, replace.Request.Nonce, cancels.Request.Nonce, all.Request.Nonce}
	for i := 1; i < len(nonces); i++ {
		if nonces[i] <= nonces[i-1] {
			t.Fatalf("同一签名者的窗口 nonce 应递增: %v", nonces)
		}
	}
}

func TestVaultActionWriterAcceptsOwnerLevelGrantOnTheManagerIndex(t *testing.T) {
	covers := `[{"address":"` + agentAddr + `","scope":"owner","subaccountMask":"4","subaccounts":[2],"label":"","validUntil":null,"expired":false}]`
	newVaultActionWriter(t, covers, 1793680000123)
	misses := `[{"address":"` + agentAddr + `","scope":"owner","subaccountMask":"2","subaccounts":[1],"label":"","validUntil":null,"expired":false}]`
	_, c := vaultWriteFake(t, misses)
	if _, err := NewActionWriter(context.Background(), c, signer(t, agentKey), VaultAccount(3), NewNonceSource(0)); !errors.Is(err, ErrAgentNotAuthorized) {
		t.Fatalf("owner 级授权不覆盖 manager 的编号: %v", err)
	}
}

func TestVaultActionWriterRefusals(t *testing.T) {
	ctx := context.Background()
	f, c := vaultWriteFake(t, `[]`)
	if _, err := NewActionWriter(ctx, c, signer(t, agentKey), VaultAccount(3), NewNonceSource(0)); !errors.Is(err, ErrAgentNotAuthorized) {
		t.Fatalf("没授权在 manager 账户上: %v", err)
	}
	unclaimed := strings.NewReplacer(`"manager":"`+ownerAddr+`"`, `"manager":null`, `"managerSubaccount":2`, `"managerSubaccount":null`).
		Replace(string(fixture(t, "vault.json")))
	f.routes["/v1/vaults/3"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(unclaimed)) }
	if _, err := NewActionWriter(ctx, c, signer(t, agentKey), VaultAccount(3), NewNonceSource(0)); err == nil || !strings.Contains(err.Error(), "待认领") {
		t.Fatalf("待认领的金库: %v", err)
	}
	bad := VaultAccount(3)
	bad.Subaccount = 2
	if _, err := NewActionWriter(ctx, c, signer(t, agentKey), bad, NewNonceSource(0)); err == nil {
		t.Fatal("金库号同时带子账户应报错")
	}
	if f.count() != 0 {
		t.Fatalf("不该有写请求,发了 %d 个", f.count())
	}
}
