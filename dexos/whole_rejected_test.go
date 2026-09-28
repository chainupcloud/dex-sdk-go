package dexos

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// 整批被拒(每一项都被拒):网关回 200 + status:"rejected",但命令已执行、逐项结果齐全。
// 这时 SDK 仍按逐项判据核对,以 *BatchOutcomeError 带出每一项的拒因(Whole 给整批拒因与位号),
// 并且仍能按 *RejectedError 识别;与逐项或聚合矛盾 = 证据不可信,ErrIncompleteBatch 并锁会话。
//
// 样本取自本机 dex-node @ f8e426c 的原样回执(只撤一张不存在的单),账户/市场/单号换成夹具的 42/7/3。
const wholeRejectedCancel = `{"accepted":0,"batchStatus":"none","canceled":0,"events":[],"folded":true,` +
	`"items":[{"accountId":42,"inputIndex":0,"kind":"cancel","market":7,"orderId":"3","reason":"UnknownOrder","recordSub":0,"state":"rejected"}],` +
	`"reason":"UnknownOrder","rejected":1,"seq":120,"status":"rejected","sub":0,"submitted":0,"submittedCancels":1}`

func TestWholeRejectedReplaceWithItemsIsPerItemOutcome(t *testing.T) {
	f := newReceiptFixture(t, http.StatusOK, wholeRejectedCancel)
	result, err := f.session.ReplaceWithReceipt(context.Background(), 7, []uint64{3}, nil)
	var bo *BatchOutcomeError
	if !errors.As(err, &bo) || len(bo.Rejected) != 1 || bo.Rejected[0].Kind != "cancel" || deref(bo.Rejected[0].Reason) != "UnknownOrder" {
		t.Fatalf("逐项齐全的整批被拒应当以 *BatchOutcomeError 带出每一项,实得 %T %v", err, err)
	}
	var re *RejectedError
	if !errors.As(err, &re) || bo.Whole != re || re.Reason != "UnknownOrder" || re.Seq != 120 || re.Sub != 0 {
		t.Fatalf("整批被拒仍要能按 *RejectedError 识别并带出拒因与位号,实得 %+v", re)
	}
	if errors.Is(err, ErrSessionBlocked) || errors.Is(err, ErrIncompleteBatch) || f.session.blocked != nil {
		t.Fatalf("结局已确定,不该锁会话: %v", err)
	}
	if f.session.nonce != 1 {
		t.Fatalf("命令已执行,nonce 已消耗,应前进到 1,实得 %d", f.session.nonce)
	}
	if result.Receipt == nil || len(result.Receipt.Items) != 1 {
		t.Fatal("逐项结果要完整保留在回执里")
	}
}

// 整批被拒的回执与逐项或聚合矛盾:证据不可信,不能包装成逐项结局。
func TestWholeRejectedWithContradictingItemsBlocks(t *testing.T) {
	canceled := `{"accountId":42,"inputIndex":0,"kind":"cancel","market":7,"orderId":"3","recordSub":0,"state":"canceled","canceledLots":2}`
	for _, tc := range []struct{ name, body string }{
		// 回执说整批被拒,逐项却说撤掉了(事件也给了):有项生效就不是整批被拒
		{"item_took_effect", strings.NewReplacer(
			`{"accountId":42,"inputIndex":0,"kind":"cancel","market":7,"orderId":"3","reason":"UnknownOrder","recordSub":0,"state":"rejected"}`, canceled,
			`"events":[]`, `"events":[`+eventCanceled3+`]`,
			`"batchStatus":"none"`, `"batchStatus":"ok"`, `"canceled":0`, `"canceled":1`, `"rejected":1`, `"rejected":0`,
		).Replace(wholeRejectedCancel)},
		{"aggregate_contradicts_items", strings.Replace(wholeRejectedCancel, `"rejected":1`, `"rejected":0`, 1)},
		{"cancel_target_mismatch", strings.Replace(wholeRejectedCancel, `"orderId":"3"`, `"orderId":"5"`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReceiptFixture(t, http.StatusOK, tc.body)
			_, err := f.session.ReplaceWithReceipt(context.Background(), 7, []uint64{3}, nil)
			var bo *BatchOutcomeError
			if !errors.Is(err, ErrIncompleteBatch) || !errors.Is(err, ErrSessionBlocked) || errors.As(err, &bo) {
				t.Fatalf("%s: 证据不可信应当 ErrIncompleteBatch 且锁会话,实得 %v", tc.name, err)
			}
		})
	}
}

// 批量撤单与批量下单走同一条判据:看守撤单撤到一张刚成交的单也能拿到逐项结局。
func TestWholeRejectedCancelAndPlaceWithItemsArePerItemOutcomes(t *testing.T) {
	g := newFakeGateway(t, 10)
	s := newTestSession(t, g)
	var body map[string]any
	g.reply = func(int) any {
		return body
	}
	cancel := strings.NewReplacer(`"accountId":42`, `"accountId":7`, `"market":7`, `"market":0`).Replace(wholeRejectedCancel)
	if err := json.Unmarshal([]byte(cancel), &body); err != nil {
		t.Fatal(err)
	}
	_, err := s.Cancel(context.Background(), 0, 3)
	var bo *BatchOutcomeError
	if !errors.As(err, &bo) || len(bo.Rejected) != 1 || bo.Whole == nil || errors.Is(err, ErrSessionBlocked) {
		t.Fatalf("整批被拒的撤单应当是逐项结局且不锁会话,实得 %T %v", err, err)
	}
	place := `{"accepted":0,"batchStatus":"none","canceled":0,"events":[],"folded":true,` +
		`"items":[{"accountId":7,"inputIndex":0,"kind":"place","market":0,"reason":"InsufficientMargin","recordSub":0,"state":"rejected"}],` +
		`"reason":"InsufficientMargin","rejected":1,"seq":121,"status":"rejected","sub":0,"submitted":1,"submittedCancels":0}`
	body = nil
	if err := json.Unmarshal([]byte(place), &body); err != nil {
		t.Fatal(err)
	}
	_, err = s.Place(context.Background(), BatchOrder{Market: 0, Side: Buy, Price: 1, Lots: 1, TIF: GTC})
	var re *RejectedError
	if !errors.As(err, &bo) || bo.Whole == nil || !errors.As(err, &re) || re.Reason != "InsufficientMargin" || errors.Is(err, ErrSessionBlocked) {
		t.Fatalf("整批被拒的下单应当是逐项结局、带整批拒因且不锁会话,实得 %T %v", err, err)
	}
	if w := g.recorded(); len(w) != 2 || w[0].Nonce != 10 || w[1].Nonce != 11 {
		t.Fatalf("两次整批被拒都已消耗 nonce,应当依次 10、11,实得 %+v", w)
	}
}

// /receipts 对整批被拒的请求同样给出逐项结果(status=rejected、nonceConsumed=true):CheckReplace 按同一判据核。
func TestReceiptCheckReplaceAcceptsWholeRejectedItems(t *testing.T) {
	req := ReplaceRequest{Identity: AgentRequestIdentity{Account: 42, Nonce: 5, SigningHash: "0x" + strings.Repeat("cd", 32)}, Market: 7, Cancels: []uint64{3}}
	mine, _ := req.Identity.RequestID()
	receipt := func(consumed string, items string) *RequestReceipt {
		var r RequestReceipt
		body := `{"status":"rejected","epoch":"k1-1","seq":120,"sub":0,"nonceConsumed":` + consumed + `,"requestId":"` + mine + `","nonce":5,"reason":"UnknownOrder","items":[` + items + `],"events":[]}`
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatal(err)
		}
		return &r
	}
	item := `{"accountId":42,"inputIndex":0,"kind":"cancel","market":7,"orderId":"3","reason":"UnknownOrder","recordSub":0,"state":"rejected"}`
	err := receipt("true", item).CheckReplace(req)
	var bo *BatchOutcomeError
	var re *RejectedError
	if !errors.As(err, &bo) || len(bo.Rejected) != 1 || !errors.As(err, &re) || re.Reason != "UnknownOrder" || re.Seq != 120 {
		t.Fatalf("逐项齐全的整批被拒回执应当是带整批拒因的 *BatchOutcomeError: %T %v", err, err)
	}
	if err := receipt("false", item).CheckReplace(req); err == nil || errors.As(err, &bo) {
		t.Fatalf("nonce 没消耗的拒绝不是批次结局: %v", err)
	}
	if err := receipt("true", strings.Replace(item, `"orderId":"3"`, `"orderId":"5"`, 1)).CheckReplace(req); !errors.Is(err, ErrIncompleteBatch) {
		t.Fatalf("撤单目标与原请求不符的回执不可信: %v", err)
	}
}
