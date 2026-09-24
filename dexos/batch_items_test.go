package dexos

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 这组测试钉住 dex-os #4(批量逐项结果)在 SDK 的判定:
//
//   - 折叠后的批量回执以 items 逐项给结局:身份是 (kind, inputIndex),撤单与下单各自从 0 编号;
//   - 逐项结局齐全且与输入、事件、聚合一致时,部分成功是**终局**:nonce 已消耗、会话不锁,
//     以 *BatchOutcomeError 带出被拒的那几项;
//   - 缺 items、项与输入对不上、与事件或聚合矛盾 = 证据不可信:ErrIncompleteBatch 并锁会话。
//
// 夹具:账户 42、市场 7,撤单 [3] + 下单 1 张(买 100 × 2,PostOnly)。

const itemCancelOK = `{"inputIndex":0,"kind":"cancel","accountId":42,"market":7,"recordSub":0,"orderId":"3","state":"canceled","canceledLots":2}`
const itemPlaceOK = `{"inputIndex":0,"kind":"place","accountId":42,"market":7,"recordSub":1,"orderId":"4","state":"accepted","filledLots":0,"resting":true}`
const itemPlaceRejected = `{"inputIndex":0,"kind":"place","accountId":42,"market":7,"recordSub":1,"state":"rejected","reason":"PostOnlyWouldCross"}`
const eventCanceled3 = `{"kind":"OrderCanceled","data":{"account":42,"market":7,"orderSeq":3,"remainingLots":2}}`
const eventAccepted4 = `{"kind":"OrderAccepted","data":{"account":42,"market":7,"orderSeq":4,"price":100,"lots":2,"filledLots":0,"resting":true}}`

func batchBody(items, events string, accepted, canceled, rejected int, status string) string {
	b := `{"status":"ok","seq":120,"sub":0,"folded":true,"submitted":1,"submittedCancels":1`
	if accepted >= 0 {
		b += `,"accepted":` + strconv.Itoa(accepted) + `,"canceled":` + strconv.Itoa(canceled) + `,"rejected":` + strconv.Itoa(rejected) + `,"batchStatus":"` + status + `"`
	}
	if items != "" {
		b += `,"items":[` + items + `]`
	}
	return b + `,"events":[` + events + `]}`
}

func TestCompleteItemsWholeSuccess(t *testing.T) {
	f := newReceiptFixture(t, http.StatusOK, batchBody(itemCancelOK+","+itemPlaceOK, eventCanceled3+","+eventAccepted4, 1, 1, 0, "ok"))
	result, err := f.session.ReplaceWithReceipt(context.Background(), 7, []uint64{3}, receiptOrders())
	if err != nil || result.Receipt == nil || len(result.Receipt.Items) != 2 {
		t.Fatalf("逐项全成功应当通过并保留逐项结果: %v %+v", err, result.Receipt)
	}
	if f.session.nonce != 1 {
		t.Fatalf("成功后 nonce 应前进到 1,实得 %d", f.session.nonce)
	}
}

// 部分成功在逐项证据齐全时是终局结论:不锁会话、nonce 前进,被拒项按原输入坐标带出。
func TestPartialBatchWithCompleteItemsIsFinalAndAdvancesNonce(t *testing.T) {
	f := newReceiptFixture(t, http.StatusOK, batchBody(itemCancelOK+","+itemPlaceRejected, eventCanceled3, 0, 1, 1, "partial"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := f.session.ReplaceWithReceipt(ctx, 7, []uint64{3}, receiptOrders())
	var bo *BatchOutcomeError
	if !errors.As(err, &bo) {
		t.Fatalf("逐项齐全的部分成功应当是 *BatchOutcomeError,实得 %T %v", err, err)
	}
	if errors.Is(err, ErrSessionBlocked) || errors.Is(err, ErrIncompleteBatch) {
		t.Fatalf("结局已确定,不该锁会话: %v", err)
	}
	if len(bo.Rejected) != 1 || bo.Rejected[0].Kind != "place" || bo.Rejected[0].InputIndex != 0 || bo.Rejected[0].Reason == nil || *bo.Rejected[0].Reason != "PostOnlyWouldCross" {
		t.Fatalf("被拒项要按原输入坐标带出拒因: %+v", bo.Rejected)
	}
	if result.Receipt == nil || len(result.Receipt.Items) != 2 {
		t.Fatal("逐项结果要完整保留在回执里")
	}
	if f.session.nonce != 1 {
		t.Fatalf("命令已执行,nonce 已消耗,应前进到 1,实得 %d", f.session.nonce)
	}
	if _, err := f.session.ReplaceWithReceipt(ctx, 7, []uint64{3}, receiptOrders()); errors.Is(err, ErrSessionBlocked) {
		t.Fatal("部分成功之后下一笔写应当照常发出")
	}
	if f.request.Nonce != 1 {
		t.Fatalf("下一笔应当用 nonce 1,实发 %d", f.request.Nonce)
	}
}

// 证据不可信的各种形态:一律 ErrIncompleteBatch 并锁会话,不按价量或事件条数猜。
func TestUntrustworthyItemsBlockSession(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"missing_items", batchBody("", eventCanceled3+","+eventAccepted4, 1, 1, 0, "ok")},
		{"duplicate_index", batchBody(itemCancelOK+","+itemPlaceOK+","+itemPlaceOK, eventCanceled3+","+eventAccepted4, 1, 1, 0, "ok")},
		{"missing_place_item", batchBody(itemCancelOK, eventCanceled3+","+eventAccepted4, 1, 1, 0, "ok")},
		{"index_out_of_range", batchBody(itemCancelOK+","+strings.Replace(itemPlaceOK, `"inputIndex":0`, `"inputIndex":1`, 1), eventCanceled3+","+eventAccepted4, 1, 1, 0, "ok")},
		{"foreign_account", batchBody(itemCancelOK+","+strings.Replace(itemPlaceOK, `"accountId":42`, `"accountId":43`, 1), eventCanceled3+","+eventAccepted4, 1, 1, 0, "ok")},
		{"foreign_market", batchBody(strings.Replace(itemCancelOK, `"market":7`, `"market":8`, 1)+","+itemPlaceOK, eventCanceled3+","+eventAccepted4, 1, 1, 0, "ok")},
		{"cancel_target_mismatch", batchBody(strings.Replace(itemCancelOK, `"orderId":"3"`, `"orderId":"5"`, 1)+","+itemPlaceOK, eventCanceled3+","+eventAccepted4, 1, 1, 0, "ok")},
		{"accepted_without_order", batchBody(itemCancelOK+","+strings.Replace(itemPlaceOK, `"orderId":"4",`, ``, 1), eventCanceled3+","+eventAccepted4, 1, 1, 0, "ok")},
		{"accepted_without_event", batchBody(itemCancelOK+","+strings.Replace(itemPlaceOK, `"orderId":"4"`, `"orderId":"99"`, 1), eventCanceled3+","+eventAccepted4, 1, 1, 0, "ok")},
		{"state_kind_mismatch", batchBody(strings.Replace(itemCancelOK, `"state":"canceled"`, `"state":"accepted"`, 1)+","+itemPlaceOK, eventCanceled3+","+eventAccepted4, 1, 1, 0, "ok")},
		{"rejected_without_reason", batchBody(itemCancelOK+","+strings.Replace(itemPlaceRejected, `,"reason":"PostOnlyWouldCross"`, ``, 1), eventCanceled3, 0, 1, 1, "partial")},
		{"aggregate_contradicts_items", batchBody(itemCancelOK+","+itemPlaceRejected, eventCanceled3, 1, 1, 0, "ok")},
		{"unexplained_accept_event", batchBody(itemCancelOK+","+itemPlaceRejected, eventCanceled3+","+eventAccepted4, 0, 1, 1, "partial")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReceiptFixture(t, http.StatusOK, tc.body)
			_, err := f.session.ReplaceWithReceipt(context.Background(), 7, []uint64{3}, receiptOrders())
			if !errors.Is(err, ErrIncompleteBatch) || !errors.Is(err, ErrSessionBlocked) {
				t.Fatalf("%s: 证据不可信应当 ErrIncompleteBatch 且锁会话,实得 %v", tc.name, err)
			}
			var bo *BatchOutcomeError
			if errors.As(err, &bo) {
				t.Fatalf("%s: 不可信的证据不能包装成终局部分成功", tc.name)
			}
		})
	}
}
