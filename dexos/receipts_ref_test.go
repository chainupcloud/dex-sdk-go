package dexos

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestReceiptCheckReplaceResolvesCancelsByCommandReference(t *testing.T) {
	ref := &CommandRef{Market: 7, Seq: 77, Sub: 2, Item: 1}
	req := ReplaceRequest{Identity: AgentRequestIdentity{Account: 42, Nonce: 5, SigningHash: "0x" + strings.Repeat("cd", 32)}, Market: 7,
		Cancels: []uint64{0}, CancelRefs: []*CommandRef{ref}, Orders: receiptOrders()}
	mine, _ := req.Identity.RequestID()
	const echo = `"ref":{"market":7,"seq":77,"sub":2,"item":1}`
	receipt := func(cancel string, events ...string) *RequestReceipt {
		body := `{"status":"executed","epoch":"k1-1","seq":120,"sub":0,"nonceConsumed":true,"requestId":"` + mine + `","nonce":5,"items":[` + cancel + `,` + itemPlaceOK + `],"events":[` +
			strings.Join(append(events, `{"seq":120,"sub":0,"idx":1,"kind":"OrderAccepted","data":{"account":42,"market":7,"orderSeq":4,"price":100,"lots":2,"filledLots":0,"resting":true}}`), ",") + `]}`
		var r RequestReceipt
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatal(err)
		}
		return &r
	}
	canceled9 := `{"seq":120,"sub":0,"idx":0,"kind":"OrderCanceled","data":{"account":42,"market":7,"orderSeq":9,"remainingLots":2}}`
	resolved := `{"inputIndex":0,"kind":"cancel","accountId":42,"market":7,"recordSub":0,"orderId":"9","state":"canceled","canceledLots":2,` + echo + `}`

	r := receipt(resolved, canceled9)
	if err := r.CheckReplace(req); err != nil {
		t.Fatalf("按引用撤掉、事件对得上解析出的单号,应当通过: %v", err)
	}
	if r.Items[0].Ref == nil || *r.Items[0].Ref != *ref {
		t.Fatalf("回执项应当保留引用回显: %+v", r.Items[0].Ref)
	}

	var bo *BatchOutcomeError
	outOfRange := `{"inputIndex":0,"kind":"cancel","accountId":42,"market":7,"recordSub":null,"state":"rejected","reason":"ReferenceOutOfRange",` + echo + `}`
	if err := receipt(outOfRange).CheckReplace(req); !errors.As(err, &bo) || *bo.Rejected[0].Reason != "ReferenceOutOfRange" {
		t.Fatalf("引用解析不了的撤单项是逐项被拒: %v", err)
	}

	for name, rc := range map[string]*RequestReceipt{
		"回显的引用不同":  receipt(strings.Replace(resolved, `"item":1`, `"item":0`, 1), canceled9),
		"没回显引用":    receipt(strings.Replace(resolved, ","+echo, "", 1), canceled9),
		"撤掉了却没单号":  receipt(strings.Replace(resolved, `"orderId":"9",`, "", 1), canceled9),
		"事件撤的是别的单": receipt(resolved, strings.Replace(canceled9, `"orderSeq":9`, `"orderSeq":8`, 1)),
	} {
		if err := rc.CheckReplace(req); !errors.Is(err, ErrIncompleteBatch) {
			t.Fatalf("%s: 应当是证据矛盾: %v", name, err)
		}
	}

	byID := req
	byID.Cancels, byID.CancelRefs = []uint64{9}, nil
	if err := receipt(resolved, canceled9).CheckReplace(byID); !errors.Is(err, ErrIncompleteBatch) {
		t.Fatalf("按单号撤的输入不该接受带引用回显的项: %v", err)
	}
	short := req
	short.CancelRefs = []*CommandRef{ref, ref}
	if err := receipt(resolved, canceled9).CheckReplace(short); err == nil || errors.Is(err, ErrIncompleteBatch) {
		t.Fatalf("CancelRefs 与撤单项不等长是调用错误: %v", err)
	}
}

func TestReplaceRequestWithoutReferencesKeepsItsStoredForm(t *testing.T) {
	raw, err := json.Marshal(ReplaceRequest{Market: 7, Cancels: []uint64{3}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "CancelRefs") {
		t.Fatalf("不带引用的原请求序列化不该多出 CancelRefs: %s", raw)
	}
}
