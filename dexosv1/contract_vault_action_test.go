//go:build dexos_contract

package dexosv1

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 契约测试(sigVersion 2 金库信封,dex-os#532):对一台真实 dex-os 用 [ActionWriter] 以金库身份走四个写端点。
// manager 在认领金库所用的账户上授权一把新生成的 API 钱包,挂远离市价的只挂单,逐项核对 v2 语义,最后撤净并撤销授权。
// 默认不编译,要跑:
//
//	DEXOS_V1_URL=http://127.0.0.1:17807 DEXOS_V1_VAULT_MANAGER_KEY=0x… DEXOS_V1_VAULT=<金库号> \
//	  go test -tags dexos_contract -run ContractVaultActions ./dexosv1/
//
// DEXOS_V1_VAULT_MANAGER_KEY 是 manager 的 owner 主钱包私钥;金库要已认领、没熔断、有保证金。DEXOS_V1_WRITE_SYMBOL 可选,
// 金库有交易白名单时用它指定一个允许的永续市场。挂单价是标记价的一半,不会成交。
func TestContractVaultActions(t *testing.T) {
	base, managerKey, vaultID := os.Getenv("DEXOS_V1_URL"), os.Getenv("DEXOS_V1_VAULT_MANAGER_KEY"), os.Getenv("DEXOS_V1_VAULT")
	if base == "" || managerKey == "" || vaultID == "" {
		t.Skip("没给 DEXOS_V1_URL / DEXOS_V1_VAULT_MANAGER_KEY / DEXOS_V1_VAULT")
	}
	n, err := strconv.ParseUint(vaultID, 10, 16)
	if err != nil {
		t.Fatalf("DEXOS_V1_VAULT: %v", err)
	}
	id := uint16(n)
	c := NewClient(strings.TrimRight(base, "/"))
	c.HTTP = &http.Client{Timeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	manager, err := dexos.NewSigner(managerKey)
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.Vault(ctx, id)
	if err != nil {
		t.Fatalf("Vault %d: %v", id, err)
	}
	if v.Manager == nil || !strings.EqualFold(*v.Manager, manager.Address().Hex()) || v.ManagerSubaccount == nil || v.TradingHalt {
		t.Fatalf("金库 %d 不能用来测:manager %v / %v,熔断 %v", id, v.Manager, v.ManagerSubaccount, v.TradingHalt)
	}

	cat, err := c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m *Market
	if sym := os.Getenv("DEXOS_V1_WRITE_SYMBOL"); sym != "" {
		if m, err = cat.Market(sym); err != nil {
			t.Fatal(err)
		}
	} else {
		for i := range cat.Markets() {
			if mk := &cat.Markets()[i]; mk.Kind == "perp" && mk.MarkPrice != nil && *mk.MarkPrice > 1 {
				m = mk
				break
			}
		}
	}
	if m == nil || m.MarkPrice == nil {
		t.Fatal("找不到有标记价的永续市场")
	}
	far := *m.MarkPrice / 2
	minN, err := ParseFixed(m.MinNotional, m.Scale.PriceDecimals+m.Scale.SizeDecimals)
	if err != nil {
		t.Fatal(err)
	}
	lots := minN.Uint64()/uint64(far) + 1
	order := func(i uint32) OrderRequest {
		return OrderRequest{Market: m, Side: dexos.Buy, Price: far - i, Lots: lots, TIF: dexos.PostOnly}
	}

	gw, err := NewWallet(ctx, c, manager, *v.ManagerSubaccount)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := dexos.GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gw.ApproveAgent(ctx, agent.Address(), time.Now().Add(time.Hour), "sdk-vault-v2"); err != nil {
		t.Fatalf("ApproveAgent: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := gw.RevokeAgent(ctx, agent.Address()); err != nil {
			t.Errorf("RevokeAgent: %v", err)
		}
	}()
	ref := VaultAccount(id)
	aw, err := NewActionWriter(ctx, c, agent, ref, NewNonceSource(0))
	if err != nil {
		t.Fatalf("NewActionWriter(vault): %v", err)
	}
	if aw.Owner() != manager.Address() {
		t.Fatalf("金库信封的 account 应是 manager 的 owner: %s", aw.Owner().Hex())
	}
	expiry := func() time.Time { return time.Now().Add(30 * time.Second) }
	var placed []uint64
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		a, err := aw.SignCancelAll(time.Now().Add(time.Minute))
		if err == nil {
			_, err = c.SendCancelAll(ctx, a)
		}
		if err != nil {
			t.Errorf("收尾撤全部: %v", err)
		}
	}()
	keep := func(t *testing.T, r *BatchResult) {
		t.Helper()
		for _, it := range r.Items {
			if it.Kind == "cancel" {
				continue
			}
			if it.Status != "accepted" || it.OrderID == nil {
				t.Fatalf("%s 没挂上: %+v", it.Kind, it)
			}
			placed = append(placed, *it.OrderID)
		}
	}

	first, err := aw.SignPlace([]OrderRequest{order(0), order(1)}, expiry())
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.SendPlace(ctx, first)
	if err != nil {
		t.Fatalf("v2 金库下单: %v", err)
	}
	keep(t, r)
	again, err := c.SendPlace(ctx, first)
	if err != nil || !again.Duplicate || again.Seq != r.Seq {
		t.Fatalf("原样重发应回 duplicate 与原结论: %+v %v", again, err)
	}
	receipt, err := c.Request(ctx, first.Request.RequestID, first.Request.Query())
	if err != nil || receipt.Status != "executed" {
		t.Fatalf("按 signer scope 查回执: %+v %v", receipt, err)
	}
	orders, err := c.Orders(ctx, cat, ref, m)
	if err != nil {
		t.Fatalf("Orders(vault): %v", err)
	}
	if len(orders) < 2 {
		t.Fatalf("金库的挂单应在金库账户上: %+v", orders)
	}

	rep, err := aw.SignReplace(m, []uint64{placed[0]}, []ModifyRequest{{OrderID: placed[1], Price: far - 5, Lots: lots, TIF: dexos.PostOnly}}, []OrderRequest{order(2)}, expiry())
	if err != nil {
		t.Fatal(err)
	}
	rr, err := c.SendReplace(ctx, rep)
	if err != nil {
		t.Fatalf("v2 金库换单(撤 + 改 + 挂): %v", err)
	}
	placed = placed[:0]
	keep(t, rr)

	pending, err := aw.SignReplace(m, nil, nil, []OrderRequest{order(3)}, expiry())
	if err != nil {
		t.Fatal(err)
	}
	sentinel, err := aw.SignCancelAt(m, pending.Request.Nonce, []uint64{1<<48 - 1}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendCancel(ctx, sentinel); err == nil || ClassifyAction(err) != ActionRejected || !strings.Contains(err.Error(), "UnknownOrder") {
		t.Fatalf("金库占位撤单应整条被拒并消费 nonce: %v", err)
	}
	if _, err := c.SendReplace(ctx, pending); err == nil || ClassifyAction(err) != ActionNotExecuted || !strings.Contains(err.Error(), "NonceMismatch") {
		t.Fatalf("nonce 被占位撤单用掉后,原请求应以 NonceMismatch 拒在消费之前: %v", err)
	}
	if got, err := c.Request(ctx, pending.Request.RequestID, pending.Request.Query()); err != nil || got.ConflictingRequest == nil || !strings.EqualFold(*got.ConflictingRequest, sentinel.Request.RequestID) {
		t.Fatalf("原请求的回执应指向占位撤单: %+v %v", got, err)
	}

	one, err := aw.SignCancel(m, placed[:1], expiry())
	if err != nil {
		t.Fatal(err)
	}
	if cr, err := c.SendCancel(ctx, one); err != nil || cr.Items[0].Status != "canceled" {
		t.Fatalf("v2 金库撤单: %+v %v", cr, err)
	}

	all, err := aw.SignCancelAll(expiry())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendCancelAll(ctx, all); err != nil {
		t.Fatalf("v2 金库撤全部: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		left, err := c.Orders(ctx, cat, ref, m)
		if err != nil {
			t.Fatalf("Orders(vault): %v", err)
		}
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("撤全部之后金库仍有挂单: %+v", left)
		}
		time.Sleep(time.Second)
	}
}
