package dexos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBatchPartialSuccessIsNotWholeSuccess(t *testing.T) {
	// 两张下单只接受一张:逐项结果齐全 → 终局的 *BatchOutcomeError;缺逐项结果 → ErrIncompleteBatch。
	for _, tc := range []struct {
		name  string
		items string
		want  func(error) bool
	}{
		{"items_complete", `,"items":[{"inputIndex":0,"kind":"place","accountId":42,"market":7,"recordSub":0,"orderId":"9","state":"accepted","filledLots":0,"resting":true},{"inputIndex":1,"kind":"place","accountId":42,"market":7,"recordSub":1,"state":"rejected","reason":"InsufficientMargin"}]`,
			func(err error) bool { var bo *BatchOutcomeError; return errors.As(err, &bo) }},
		{"items_missing", ``, func(err error) bool { return errors.Is(err, ErrIncompleteBatch) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `{"status":"ok","seq":12,"folded":true,"submitted":2`+tc.items+`,"events":[{"kind":"OrderAccepted","data":{"account":42,"market":7,"orderSeq":9,"price":100,"lots":1,"filledLots":0,"resting":true}}]}`)
			}))
			defer srv.Close()
			signer, err := GenerateSigner()
			if err != nil {
				t.Fatal(err)
			}
			orders := []BatchOrder{{Market: 7, Price: 100, Lots: 1, Side: Buy, TIF: PostOnly}, {Market: 7, Price: 200, Lots: 1, Side: Sell, TIF: PostOnly}}
			ev, err := NewClient(srv.URL, 1).BatchPlace(context.Background(), signer, 42, orders, 3)
			if err == nil || !tc.want(err) {
				t.Fatalf("仅一项成功不能当整批成功,且要分清终局与不可信: %v", err)
			}
			if len(ev) != 1 {
				t.Fatal("部分成功证据被丢弃")
			}
		})
	}
}

func TestBatchOutcomeKernelBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name             string
		seq, lots        uint64
		reduceOnly, fail bool
	}{
		{"first_order", 0, 10, false, false},
		{"reduce_only_clamped", 9, 3, true, false},
		{"reduce_only_cannot_increase", 9, 11, true, true},
		{"non_reduce_cannot_shrink", 9, 3, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := fmt.Sprintf(`{"account":42,"market":7,"orderSeq":%d,"price":100,"lots":%d,"filledLots":0,"resting":true}`, tc.seq, tc.lots)
			order := fmt.Sprint(tc.seq)
			filled, resting := uint64(0), true
			receipt := WriteReceipt{
				Items:  []BatchItem{{InputIndex: 0, Kind: "place", AccountID: 42, Market: 7, OrderID: &order, State: "accepted", FilledLots: &filled, Resting: &resting}},
				Events: []EventEnvelope{{Kind: "OrderAccepted", Data: json.RawMessage(data)}},
			}
			err := checkBatchOutcome(receipt, 42, []BatchOrder{{Market: 7, Price: 100, Lots: 10, ReduceOnly: tc.reduceOnly}}, nil)
			if (err != nil) != tc.fail {
				t.Fatalf("正常内核边界与不完整判据不符: %v", err)
			}
		})
	}
	id := NewOrderID(7, 9)
	target := "9"
	receipt := WriteReceipt{
		Items: []BatchItem{
			{InputIndex: 0, Kind: "cancel", AccountID: 42, Market: 7, OrderID: &target, State: "canceled"},
			{InputIndex: 1, Kind: "cancel", AccountID: 42, Market: 7, OrderID: &target, State: "canceled"},
		},
		Events: []EventEnvelope{{Kind: "OrderCanceled", Data: json.RawMessage(`{"account":42,"market":7,"orderSeq":9}`)}},
	}
	err := checkBatchOutcome(receipt, 42, nil, []OrderID{id, id})
	if !errors.Is(err, ErrIncompleteBatch) {
		t.Fatalf("同一张单被报撤掉两次却当完整成功: %v", err)
	}
}

func TestSignerRejectsInvalidScalar(t *testing.T) {
	for _, key := range []string{"0000000000000000000000000000000000000000000000000000000000000000", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"} {
		if _, err := NewSigner(key); err == nil {
			t.Fatal("零或越界私钥被接受")
		}
	}
}
