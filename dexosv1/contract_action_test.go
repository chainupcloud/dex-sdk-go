//go:build dexos_contract

package dexosv1

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 契约测试(sigVersion 2 预签写入):对一台真实 dex-os 用**测试账户**走一遍 [ActionWriter] —— 主钱包按账户授权一把新生成的
// API 钱包(与做市服务的报价代理同一种授权),挂远离市价的只挂单,逐项核对 v2 语义,最后撤净。默认不编译,要跑:
//
//	DEXOS_V1_URL=http://127.0.0.1:8080 DEXOS_V1_WRITE_KEY=0x… go test -tags dexos_contract -run ContractActions ./dexosv1/
//
// DEXOS_V1_WRITE_KEY 是测试账户主钱包私钥,账户里要有 USDC;DEXOS_V1_WRITE_SYMBOL 可选。挂单价是标记价的一半,不会成交。
func TestContractActions(t *testing.T) {
	base, key := os.Getenv("DEXOS_V1_URL"), os.Getenv("DEXOS_V1_WRITE_KEY")
	if base == "" || key == "" {
		t.Skip("没给 DEXOS_V1_URL / DEXOS_V1_WRITE_KEY")
	}
	c := NewClient(strings.TrimRight(base, "/"))
	c.HTTP = &http.Client{Timeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	owner, err := dexos.NewSigner(key)
	if err != nil {
		t.Fatal(err)
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
		t.Fatal("找不到有标记价的市场")
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

	w, err := NewWallet(ctx, c, owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := dexos.GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.ApproveAgent(ctx, agent.Address(), time.Now().Add(time.Hour), "sdk-contract-v2"); err != nil {
		t.Fatalf("ApproveAgent: %v", err)
	}
	aw, err := NewActionWriter(ctx, c, agent, w.Account, NewNonceSource(0))
	if err != nil {
		t.Fatalf("NewActionWriter: %v", err)
	}
	var placed []uint64
	defer func() {
		if len(placed) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		a, err := aw.SignCancel(m, placed, time.Now().Add(time.Minute))
		if err == nil {
			_, err = c.SendCancel(ctx, a)
		}
		if err != nil {
			t.Errorf("收尾撤单: %v", err)
		}
	}()
	place := func(t *testing.T, r *BatchResult) {
		t.Helper()
		for _, it := range r.Items {
			if it.Kind == "place" {
				if it.Status != "accepted" || it.OrderID == nil {
					t.Fatalf("挂单没挂上: %+v", it)
				}
				placed = append(placed, *it.OrderID)
			}
		}
	}
	expiry := func() time.Time { return time.Now().Add(30 * time.Second) }

	first, err := aw.SignReplace(m, nil, nil, []OrderRequest{order(0)}, expiry())
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.SendReplace(ctx, first)
	if err != nil {
		t.Fatalf("v2 换单: %v", err)
	}
	place(t, r)
	again, err := c.SendReplace(ctx, first)
	if err != nil || !again.Duplicate || again.Seq != r.Seq || again.Request.RequestID != first.Request.RequestID {
		t.Fatalf("原样重发应回 duplicate 与原结论: %+v %v", again, err)
	}
	receipt, err := c.Request(ctx, first.Request.RequestID, first.Request.Query())
	if err != nil || receipt.Status != "executed" {
		t.Fatalf("按 signer scope 查回执: %+v %v", receipt, err)
	}

	pending, err := aw.SignReplace(m, nil, nil, []OrderRequest{order(1)}, expiry())
	if err != nil {
		t.Fatal(err)
	}
	take, err := aw.SignCancelAt(m, pending.Request.Nonce, []uint64{placed[0]}, expiry())
	if err != nil {
		t.Fatal(err)
	}
	tr, err := c.SendCancel(ctx, take)
	if err != nil || tr.Items[0].Status != "canceled" {
		t.Fatalf("占位撤单: %+v %v", tr, err)
	}
	placed = placed[1:]
	_, err = c.SendReplace(ctx, pending)
	if err == nil || ClassifyAction(err) != ActionNotExecuted || !strings.Contains(err.Error(), "NonceMismatch") {
		t.Fatalf("被占位撤单占掉 nonce 的原请求应以 NonceMismatch 拒在消费之前: %v", err)
	}
	conflict, err := c.Request(ctx, pending.Request.RequestID, pending.Request.Query())
	if err != nil || conflict.ConflictingRequest == nil || !strings.EqualFold(*conflict.ConflictingRequest, take.Request.RequestID) {
		t.Fatalf("原请求的回执应指向占位撤单: %+v %v", conflict, err)
	}

	old, err := aw.SignCancelAt(m, uint64(time.Now().Add(-3*24*time.Hour).UnixMilli()), []uint64{1<<48 - 1}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendCancel(ctx, old); err == nil || ClassifyAction(err) != ActionNotExecuted || !strings.Contains(err.Error(), "NonceOutOfWindow") {
		t.Fatalf("出窗的 nonce: %v", err)
	}
	missing, err := aw.SignReplace(m, nil, nil, []OrderRequest{order(2)}, expiry())
	if err != nil {
		t.Fatal(err)
	}
	sentinel, err := aw.SignCancelAt(m, missing.Request.Nonce, []uint64{1<<48 - 1}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendCancel(ctx, sentinel); err == nil || ClassifyAction(err) != ActionRejected || !strings.Contains(err.Error(), "UnknownOrder") {
		t.Fatalf("单张撤一张不存在的单应整条被拒并消费 nonce: %v", err)
	}
	if _, err := c.SendReplace(ctx, missing); err == nil || ClassifyAction(err) != ActionNotExecuted || !strings.Contains(err.Error(), "NonceMismatch") {
		t.Fatalf("nonce 被撤占位单号的撤单用掉后，原请求应以 NonceMismatch 拒在消费之前: %v", err)
	}
	if got, err := c.Request(ctx, sentinel.Request.RequestID, sentinel.Request.Query()); err != nil || got.Status != "rejected" || !strings.Contains(string(got.Raw), `"nonceConsumed":true`) {
		t.Fatalf("撤占位单号的回执应是 rejected 且 nonce 已消费: %+v %v", got, err)
	}
	if got, err := c.Request(ctx, missing.Request.RequestID, missing.Request.Query()); err != nil || got.ConflictingRequest == nil || !strings.EqualFold(*got.ConflictingRequest, sentinel.Request.RequestID) {
		t.Fatalf("原请求的回执应指向撤占位单号的撤单: %+v %v", got, err)
	}
	expired, err := aw.SignReplace(m, nil, nil, []OrderRequest{order(3)}, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendReplace(ctx, expired); err == nil || ClassifyAction(err) != ActionNotExecuted {
		t.Fatalf("过期的签名应确定没执行: %v", err)
	}

	signed := make([]*SignedAction, 10)
	for i := range signed {
		if signed[i], err = aw.SignReplace(m, nil, nil, []OrderRequest{order(uint32(10 + i))}, expiry()); err != nil {
			t.Fatal(err)
		}
	}
	results := make([]*BatchResult, len(signed))
	errs := make([]error, len(signed))
	var wg sync.WaitGroup
	for i := len(signed) - 1; i >= 0; i-- {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(len(signed)-i) * 5 * time.Millisecond)
			results[i], errs[i] = c.SendReplace(ctx, signed[i])
		}()
	}
	wg.Wait()
	for i := range signed {
		if errs[i] != nil {
			t.Errorf("乱序第 %d 笔: %v", i, errs[i])
			continue
		}
		place(t, results[i])
	}
}
