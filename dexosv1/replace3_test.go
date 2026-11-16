package dexosv1

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

func replace3Inputs(btc *Market) ([]OrderTarget, []ModifyTarget, []OrderRequest) {
	old := uint64(101)
	return []OrderTarget{{OrderID: &old}, {Ref: &CommandRef{Seq: 77, Sub: 2, Item: 1}}},
		[]ModifyTarget{{Target: OrderTarget{Ref: &CommandRef{Seq: 76, Sub: 0, Item: 0}}, Price: 64900, Lots: 200, TIF: dexos.PostOnly}},
		[]OrderRequest{{Market: btc, Side: dexos.Sell, Price: 65100, Lots: 100, TIF: dexos.GTC}}
}

func TestActionWriterSignsBatchReplace3WithCommandReferences(t *testing.T) {
	f, w, btc := newActionWriter(t, 0, 1793680000123)
	cancels, modifies, orders := replace3Inputs(btc)
	a, err := w.SignReplace3(btc, cancels, modifies, orders, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	old := dexos.NewOrderID(btc.SigningMarketID, 101)
	want := dexos.EncodeBatchReplace3(dexos.ActorPlaceholder, a.NowMs,
		[]dexos.OrderTarget{{OrderID: &old}, {Ref: &dexos.CommandRef{Market: btc.SigningMarketID, Seq: 77, Sub: 2, Item: 1}}},
		[]dexos.BatchModify3{{Target: dexos.OrderTarget{Ref: &dexos.CommandRef{Market: btc.SigningMarketID, Seq: 76}}, Price: 64900, Lots: 200, TIF: dexos.PostOnly}},
		[]dexos.BatchOrder{{Market: btc.SigningMarketID, Side: dexos.Sell, Price: 65100, Lots: 100, TIF: dexos.GTC}})
	if hex0x(a.Command) != hex0x(want) {
		t.Fatalf("内层命令不是 BatchReplace3:\n got %s\nwant %s", hex0x(a.Command), hex0x(want))
	}
	body := m(decodeJSON(t, a.Body))
	conformsRequest(t, "/v1/orders/replace", body)
	cancelsWire := body["cancels"].([]any)
	if num(t, body["replaceVersion"]) != 3 || cancelsWire[0] != "101" || len(m(cancelsWire[1])) != 1 ||
		num(t, m(m(cancelsWire[1])["ref"])["seq"]) != 77 || num(t, m(m(cancelsWire[1])["ref"])["item"]) != 1 {
		t.Fatalf("请求体: %v", body)
	}
	mod := m(body["modifies"].([]any)[0])
	if _, has := mod["orderId"]; has || num(t, m(mod["ref"])["seq"]) != 76 {
		t.Fatalf("改单项应只带 ref: %v", mod)
	}
	if got := gatewayRequestID(t, f.cat, "/v1/orders/replace", decodeJSON(t, a.Body)); got != a.Request.RequestID {
		t.Fatalf("网关按请求体算出的身份 %s,本地 %s", got, a.Request.RequestID)
	}
	if a.Cancels != 2 || a.Modifies != 1 || a.Places != 1 {
		t.Fatalf("项数: %+v", a)
	}
}

func TestActionWriterSignsBatchReplace3EvenWithoutReferences(t *testing.T) {
	_, w, btc := newActionWriter(t, 0, 1793680000123)
	a, err := w.SignReplace3(btc, nil, nil, []OrderRequest{{Market: btc, Side: dexos.Buy, Price: 64000, Lots: 100, TIF: dexos.PostOnly}}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	body := m(decodeJSON(t, a.Body))
	conformsRequest(t, "/v1/orders/replace", body)
	want := dexos.EncodeBatchReplace3(dexos.ActorPlaceholder, a.NowMs, nil, nil,
		[]dexos.BatchOrder{{Market: btc.SigningMarketID, Side: dexos.Buy, Price: 64000, Lots: 100, TIF: dexos.PostOnly}})
	if num(t, body["replaceVersion"]) != 3 || hex0x(a.Command) != hex0x(want) {
		t.Fatalf("只挂新单也要签成 BatchReplace3,后面的换单才能引用它: %v %s", body, hex0x(a.Command))
	}
}

func TestActionWriterRejectsMalformedReplace3Targets(t *testing.T) {
	_, w, btc := newActionWriter(t, 0, 1793680000123)
	old := uint64(101)
	ref := &CommandRef{Seq: 77, Sub: 2, Item: 1}
	for name, cancels := range map[string][]OrderTarget{
		"两个都没给":      {{}},
		"两个都给了":      {{OrderID: &old, Ref: ref}},
		"同一个引用出现两次":  {{Ref: ref}, {Ref: &CommandRef{Seq: 77, Sub: 2, Item: 1}}},
		"同一个订单号出现两次": {{OrderID: &old}, {OrderID: &old}},
	} {
		if _, err := w.SignReplace3(btc, cancels, nil, nil, time.Time{}); err == nil {
			t.Fatalf("%s: 应在签名之前就拒", name)
		}
	}
	if w.nonces.Last() != 0 {
		t.Fatalf("签名之前就拒的不该动 nonce: %d", w.nonces.Last())
	}
}

func TestSendReplaceReadsTheReferenceEchoOfEachItem(t *testing.T) {
	f, w, btc := newActionWriter(t, 0, 1793680000123)
	f.answer(t, "/v1/orders/replace", 200, "w_replace3.json")
	cancels, modifies, orders := replace3Inputs(btc)
	a, err := w.SignReplace3(btc, cancels, modifies, orders, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Client.SendReplace(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]BatchItem{}
	for _, it := range got.Items {
		byKind[it.Kind+string(rune('0'+it.Index))] = it
	}
	resolved, missed := byKind["cancel1"], byKind["modify0"]
	if resolved.Ref == nil || *resolved.Ref != (CommandRef{Seq: 77, Sub: 2, Item: 1}) || resolved.OrderID == nil || *resolved.OrderID != 120 {
		t.Fatalf("解析成功的引用撤单项应回显引用并给出订单号: %+v", resolved)
	}
	if missed.Ref == nil || *missed.Ref != (CommandRef{Seq: 76}) || missed.Code != "ReferenceOutOfRange" || missed.ReplacedOrderID != nil {
		t.Fatalf("解析失败的引用改单项: %+v", missed)
	}
	if byKind["cancel0"].Ref != nil {
		t.Fatalf("按订单号的项不带引用: %+v", byKind["cancel0"])
	}
}

func TestSendSequencedReturnsTheLogPositionWithoutWaitingForTheOutcome(t *testing.T) {
	f, w, btc := newActionWriter(t, 0, 1793680000123)
	f.answerAck(t, "/v1/orders/replace", "sequenced", 202, fixture(t, "w_sequenced_202.json"))
	cancels, modifies, orders := replace3Inputs(btc)
	a, err := w.SignReplace3(btc, cancels, modifies, orders, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := w.Client.SendSequenced(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if r.Seq != 509 || r.Sub != 0 || r.Request.RequestID != a.Request.RequestID {
		t.Fatalf("定序回执: %+v", r)
	}
	f.answerAck(t, "/v1/orders/replace", "sequenced", 422, fixture(t, "err422_nonce.json"))
	if _, err := w.Client.SendSequenced(context.Background(), a); err == nil || errors.Is(err, ErrPending) {
		t.Fatalf("定序前就被拒的要原样报错: %v", err)
	}
}
