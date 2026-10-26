package dexosv1

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 带配对的原子换单(dex-os#256):下单项带 Replaces 就与那条撤单项配成一对,整条签 BatchReplacePaired。
// 配对项不带方向上线,签名按配对编码;回执两项各自按 (kind, index) 回来;一个签名、一个 nonce。
func TestSessionReplacePaired(t *testing.T) {
	f, s, cat := newTestSession(t)
	btc, _ := cat.Market("BTC-USD")
	f.answer(t, "/v1/orders/replace", 200, "w_replace_paired.json")
	zero, one := 0, 1
	res, err := s.Replace(context.Background(), btc, []uint64{101, 103, 104}, []OrderRequest{
		// Side 填了也不上线、不进签名:方向从旧单继承
		{Market: btc, Side: dexos.Buy, Price: 64950, Lots: 500, TIF: dexos.PostOnly, ReduceOnly: true, Replaces: &zero},
		{Market: btc, Price: 65100, Lots: 500, TIF: dexos.GTC, ClientOrderID: "p-1", Replaces: &one},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := f.last(t)
	conformsRequest(t, "/v1/orders/replace", req.body)
	body := m(req.body)
	orders := body["orders"].([]any)
	o0, o1 := m(orders[0]), m(orders[1])
	if _, has := o0["side"]; has {
		t.Fatalf("配对项不该带方向: %v", o0)
	}
	if num(t, o0["replaces"]) != 0 || num(t, o1["replaces"]) != 1 || o1["clientOrderId"] != "p-1" || o0["tif"] != "alo" {
		t.Fatalf("配对项线上形状: %v", orders)
	}
	r0, r1 := uint32(0), uint32(1)
	cmd := dexos.EncodeBatchReplacePaired(signingID, num(t, body["nowMs"]),
		[]dexos.OrderID{dexos.NewOrderID(0, 101), dexos.NewOrderID(0, 103), dexos.NewOrderID(0, 104)},
		[]dexos.ReplaceItem{
			{Order: dexos.BatchOrder{Price: 64950, Lots: 500, TIF: dexos.PostOnly, ReduceOnly: true}, Replaces: &r0},
			{Order: dexos.BatchOrder{Price: 65100, Lots: 500, TIF: dexos.GTC}, Replaces: &r1},
		})
	if got := recoverAddr(t, testDomain.AgentExecHash(cmd, 41), body["signature"].(string)); got != agentAddr {
		t.Fatalf("配对换单的签名者 %s", got)
	}
	// 旧单 101 已不在:这一对两项都 UnknownOrder、不挂新单;103 那一对撤旧挂新
	var place1, cancel0 *BatchItem
	for i := range res.Items {
		it := &res.Items[i]
		switch {
		case it.Kind == "place" && it.Index == 1:
			place1 = it
		case it.Kind == "cancel" && it.Index == 0:
			cancel0 = it
		}
	}
	if len(res.Items) != 5 || cancel0 == nil || cancel0.Code != "UnknownOrder" || place1 == nil || *place1.OrderID != 105 || *place1.ClientOrderID != "p-1" {
		t.Fatalf("配对换单回执: %+v", res.Items)
	}
	if s.Nonce() != 42 {
		t.Fatalf("一条命令一个 nonce:应为 42,得到 %d", s.Nonce())
	}
}

// 配对形状错在本地就拦下:不发请求、不碰 nonce;下单接口不收 Replaces。
func TestReplacePairedLocalErrors(t *testing.T) {
	f, s, cat := newTestSession(t)
	btc, _ := cat.Market("BTC-USD")
	at := func(i int) *int { return &i }
	o := func(r *int) OrderRequest {
		return OrderRequest{Market: btc, Side: dexos.Buy, Price: 64900, Lots: 100, TIF: dexos.GTC, Replaces: r}
	}
	for name, orders := range map[string][]OrderRequest{
		"越界":       {o(at(2))},
		"负数":       {o(at(-1))},
		"同一撤单项配两次": {o(at(0)), o(at(0))},
	} {
		if _, err := s.Replace(context.Background(), btc, []uint64{101, 102}, orders); err == nil {
			t.Errorf("%s:应在本地报错", name)
		}
	}
	if _, err := s.Place(context.Background(), o(at(0))); err == nil {
		t.Error("下单不收 Replaces")
	}
	if len(f.writes) != 0 || s.Nonce() != 41 {
		t.Fatalf("本地错误不该发请求、不该动 nonce:发了 %d 次,nonce %d", len(f.writes), s.Nonce())
	}
}

// 写前日志里记下配对:调用方凭 intent 能独立重算出同一条命令。
func TestReplaceJournaledRecordsPairs(t *testing.T) {
	f, s, cat := newTestSession(t)
	btc, _ := cat.Market("BTC-USD")
	f.answer(t, "/v1/orders/replace", 200, "w_replace_paired.json")
	zero, one := 0, 1
	j := &recordJournal{f: f}
	_, err := s.ReplaceJournaled(context.Background(), btc, []uint64{101, 103, 104}, []OrderRequest{
		{Market: btc, Price: 64950, Lots: 500, TIF: dexos.PostOnly, ReduceOnly: true, Replaces: &zero},
		{Market: btc, Price: 65100, Lots: 500, TIF: dexos.GTC, ClientOrderID: "p-1", Replaces: &one},
	}, j)
	if err != nil {
		t.Fatal(err)
	}
	in := j.intent
	if len(in.Pairs) != 2 || *in.Pairs[0] != 0 || *in.Pairs[1] != 1 {
		t.Fatalf("intent.Pairs: %+v", in.Pairs)
	}
	ids := make([]dexos.OrderID, len(in.Cancels))
	for i, c := range in.Cancels {
		ids[i] = dexos.NewOrderID(in.Market.SigningMarketID, c)
	}
	items := make([]dexos.ReplaceItem, len(in.Orders))
	for i := range in.Orders {
		items[i] = dexos.ReplaceItem{Order: in.Orders[i], Replaces: in.Pairs[i]}
	}
	cmd := dexos.EncodeBatchReplacePaired(in.SigningAccountID, in.NowMs, ids, items)
	if in.Request.CommandHash != "0x"+hex.EncodeToString(dexos.Keccak256(cmd)) {
		t.Fatalf("凭 intent 重算的命令哈希对不上")
	}
}
