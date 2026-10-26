package dexosv1

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 带改单项的原子换单(dex-os#256):modifies 非空就整条签 BatchReplace2。改单项不带方向上线,
// 回执里改单项按 (modify, index) 回来、带旧单号;一个签名、一个 nonce。
func TestSessionReplaceModify(t *testing.T) {
	f, s, cat := newTestSession(t)
	btc, _ := cat.Market("BTC-USD")
	f.answer(t, "/v1/orders/replace", 200, "w_replace_modify.json")
	res, err := s.ReplaceModify(context.Background(), btc, []uint64{104},
		[]ModifyRequest{
			{OrderID: 101, Price: 64950, Lots: 500, TIF: dexos.PostOnly, ReduceOnly: true},
			{OrderID: 103, Price: 65100, Lots: 500, TIF: dexos.GTC},
		},
		[]OrderRequest{{Market: btc, Side: dexos.Buy, Price: 64800, Lots: 100, TIF: dexos.GTC, ClientOrderID: "p-1"}})
	if err != nil {
		t.Fatal(err)
	}
	req := f.last(t)
	conformsRequest(t, "/v1/orders/replace", req.body)
	body := m(req.body)
	mods := body["modifies"].([]any)
	m0 := m(mods[0])
	if m0["orderId"] != "101" || m0["tif"] != "alo" || m0["reduceOnly"] != true || len(mods) != 2 {
		t.Fatalf("改单项线上形状: %v", mods)
	}
	cmd := dexos.EncodeBatchReplace2(signingID, num(t, body["nowMs"]),
		[]dexos.OrderID{dexos.NewOrderID(0, 104)},
		[]dexos.BatchModify{
			{Order: dexos.NewOrderID(0, 101), Price: 64950, Lots: 500, TIF: dexos.PostOnly, ReduceOnly: true},
			{Order: dexos.NewOrderID(0, 103), Price: 65100, Lots: 500, TIF: dexos.GTC},
		},
		[]dexos.BatchOrder{{Market: 0, Side: dexos.Buy, Price: 64800, Lots: 100, TIF: dexos.GTC}})
	if got := recoverAddr(t, testDomain.AgentExecHash(cmd, 41), body["signature"].(string)); got != agentAddr {
		t.Fatalf("BatchReplace2 的签名者 %s", got)
	}
	// 旧单 101 已不在:该项 UnknownOrder、没有撤到量;103 撤旧挂新
	find := func(kind string, i int) *BatchItem {
		for k := range res.Items {
			if res.Items[k].Kind == kind && res.Items[k].Index == i {
				return &res.Items[k]
			}
		}
		t.Fatalf("回执缺 %s/%d: %+v", kind, i, res.Items)
		return nil
	}
	gone, ok := find("modify", 0), find("modify", 1)
	if gone.Code != "UnknownOrder" || *gone.ReplacedOrderID != 101 || gone.CanceledLots != nil || gone.OrderID != nil {
		t.Fatalf("旧单已不在的改单项: %+v", gone)
	}
	if ok.Status != "accepted" || *ok.ReplacedOrderID != 103 || *ok.CanceledLots != 500 || *ok.OrderID != 105 || !*ok.Resting {
		t.Fatalf("改成的改单项: %+v", ok)
	}
	if *find("place", 0).ClientOrderID != "p-1" || find("cancel", 0).Status != "canceled" {
		t.Fatalf("撤单项 / 下单项: %+v", res.Items)
	}
	if s.Nonce() != 42 {
		t.Fatalf("一条命令一个 nonce:应为 42,得到 %d", s.Nonce())
	}
}

// 不带改单项时请求体与签名都同 BatchReplace(没有 modifies 字段)。
func TestReplaceWithoutModifiesStaysBatchReplace(t *testing.T) {
	f, s, cat := newTestSession(t)
	btc, _ := cat.Market("BTC-USD")
	f.answer(t, "/v1/orders/replace", 200, "w_replace.json")
	if _, err := s.ReplaceModify(context.Background(), btc, []uint64{101}, nil,
		[]OrderRequest{{Market: btc, Side: dexos.Buy, Price: 64900, Lots: 200, TIF: dexos.GTC}}); err != nil {
		t.Fatal(err)
	}
	body := m(f.last(t).body)
	if _, has := body["modifies"]; has {
		t.Fatalf("没有改单项不该出现 modifies: %v", body)
	}
	cmd := dexos.EncodeBatchReplace(signingID, num(t, body["nowMs"]), []dexos.OrderID{dexos.NewOrderID(0, 101)},
		[]dexos.BatchOrder{{Market: 0, Side: dexos.Buy, Price: 64900, Lots: 200, TIF: dexos.GTC}})
	if got := recoverAddr(t, testDomain.AgentExecHash(cmd, 41), body["signature"].(string)); got != agentAddr {
		t.Fatalf("BatchReplace 的签名者 %s", got)
	}
}

// 改单项形状错、同一张单重复在本地就拦下:不发请求、不碰 nonce。
func TestReplaceModifyLocalErrors(t *testing.T) {
	f, s, cat := newTestSession(t)
	btc, _ := cat.Market("BTC-USD")
	ok := ModifyRequest{OrderID: 102, Price: 64900, Lots: 100, TIF: dexos.GTC}
	for name, c := range map[string]struct {
		cancels  []uint64
		modifies []ModifyRequest
	}{
		"价格为 0":      {nil, []ModifyRequest{{OrderID: 102, Lots: 100}}},
		"未知 TIF":     {nil, []ModifyRequest{{OrderID: 102, Price: 64900, Lots: 100, TIF: 9}}},
		"两个改单项同一张":   {nil, []ModifyRequest{ok, ok}},
		"撤单项与改单项同一张": {[]uint64{102}, []ModifyRequest{ok}},
	} {
		if _, err := s.ReplaceModify(context.Background(), btc, c.cancels, c.modifies, nil); err == nil {
			t.Errorf("%s:应在本地报错", name)
		}
	}
	if len(f.writes) != 0 || s.Nonce() != 41 {
		t.Fatalf("本地错误不该发请求、不该动 nonce:发了 %d 次,nonce %d", len(f.writes), s.Nonce())
	}
}

// 写前日志里记下改单项:调用方凭 intent 能独立重算出同一条命令。
func TestReplaceModifyJournaledRecordsModifies(t *testing.T) {
	f, s, cat := newTestSession(t)
	btc, _ := cat.Market("BTC-USD")
	f.answer(t, "/v1/orders/replace", 200, "w_replace_modify.json")
	j := &recordJournal{f: f}
	_, err := s.ReplaceModifyJournaled(context.Background(), btc, []uint64{104},
		[]ModifyRequest{
			{OrderID: 101, Price: 64950, Lots: 500, TIF: dexos.PostOnly, ReduceOnly: true},
			{OrderID: 103, Price: 65100, Lots: 500, TIF: dexos.GTC},
		},
		[]OrderRequest{{Market: btc, Side: dexos.Buy, Price: 64800, Lots: 100, TIF: dexos.GTC, ClientOrderID: "p-1"}}, j)
	if err != nil {
		t.Fatal(err)
	}
	in := j.intent
	if len(in.Modifies) != 2 || in.Modifies[1].Order != dexos.NewOrderID(0, 103) {
		t.Fatalf("intent.Modifies: %+v", in.Modifies)
	}
	ids := make([]dexos.OrderID, len(in.Cancels))
	for i, c := range in.Cancels {
		ids[i] = dexos.NewOrderID(in.Market.SigningMarketID, c)
	}
	cmd := dexos.EncodeBatchReplace2(in.SigningAccountID, in.NowMs, ids, in.Modifies, in.Orders)
	if in.Request.CommandHash != "0x"+hex.EncodeToString(dexos.Keccak256(cmd)) {
		t.Fatalf("凭 intent 重算的命令哈希对不上")
	}
}
