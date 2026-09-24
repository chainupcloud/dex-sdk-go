package dexos

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// 这组测试钉住 dex-os #3(按原请求身份查权威回执)与 #5(同水位权益快照)的读侧契约:
//
//   - /receipts 的六种结论(executed / rejected / pending / not_found / conflict / history_unavailable)
//     都是**答案**,按状态返回,不当成错误吞掉;存储故障与请求错误才是 error;
//   - 回执里的身份必须与本地原请求一致,不一致即报错(防止把别人的回执当成自己的);
//   - /equity 金额保持字符串原值;404 = 账户不存在;500 核对不等不能给出半个快照。

func serveOnce(t *testing.T, path string, status int, body string) (*Client, *[]string) {
	t.Helper()
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RequestURI())
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, 31337), &seen
}

const rid = "0x1111111111111111111111111111111111111111111111111111111111111111"

func TestReceiptStatusesAreAnswersNotErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   ReceiptStatus
	}{
		{"executed", 200, `{"status":"executed","epoch":"k1-1","seq":12,"sub":0,"nonceConsumed":true,"events":[{"seq":12,"sub":0,"idx":0,"kind":"OrderAccepted","data":{"account":7}}],"items":[{"inputIndex":0,"kind":"place","accountId":7,"market":0,"recordSub":1,"orderId":"12","state":"accepted","filledLots":0,"resting":true}],"source":"local","requestId":"` + rid + `","signer":"0x19e7e376e7c213b7e7e7e46cc70a5dd086daff2a","nonceScope":"agent:0x19e7e376e7c213b7e7e7e46cc70a5dd086daff2a","nonce":5}`, ReceiptExecuted},
		{"rejected", 200, `{"status":"rejected","epoch":"k1-1","seq":12,"sub":0,"nonceConsumed":false,"events":[],"items":[],"source":"local","reason":"NonceMismatch","requestId":"` + rid + `"}`, ReceiptRejected},
		{"pending", 200, `{"requestId":"` + rid + `","status":"pending","epoch":"k1-1","seq":12,"sub":0}`, ReceiptPending},
		{"not_found", 404, `{"requestId":"` + rid + `","status":"not_found","epoch":"k1-1","asOfSeq":30}`, ReceiptNotFound},
		{"conflict", 409, `{"requestId":"` + rid + `","status":"conflict","epoch":"k1-1","nonceScope":"agent:0x19e7e376e7c213b7e7e7e46cc70a5dd086daff2a","nonce":5,"conflictingRequest":"0x22","seq":9,"sub":0}`, ReceiptConflict},
		{"history_unavailable", 503, `{"requestId":"` + rid + `","status":"history_unavailable","epoch":"k1-1","gaps":[{"first":1,"last":8}]}`, ReceiptHistoryUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := serveOnce(t, "/receipts/", tc.status, tc.body)
			got, err := c.Receipt(context.Background(), rid, ReceiptQuery{})
			if err != nil {
				t.Fatalf("%s 是答案不是错误,实得 %v", tc.name, err)
			}
			if got.Status != tc.want || got.Epoch != "k1-1" {
				t.Fatalf("状态/纪元不对: %+v", got)
			}
			switch tc.want {
			case ReceiptExecuted:
				if got.Seq == nil || *got.Seq != 12 || got.NonceConsumed == nil || !*got.NonceConsumed || len(got.Items) != 1 || got.Items[0].OrderID == nil || *got.Items[0].OrderID != "12" || len(got.Events) != 1 || got.Events[0].ID() != "12-0-0" {
					t.Fatalf("执行回执字段丢失: %+v", got)
				}
			case ReceiptRejected:
				if got.Reason == nil || *got.Reason != "NonceMismatch" || got.NonceConsumed == nil || *got.NonceConsumed {
					t.Fatalf("拒绝回执字段丢失: %+v", got)
				}
			case ReceiptNotFound:
				if got.AsOfSeq == nil || *got.AsOfSeq != 30 {
					t.Fatalf("not_found 要带证明到的水位: %+v", got)
				}
			case ReceiptConflict:
				if got.ConflictingRequest == nil || *got.ConflictingRequest != "0x22" {
					t.Fatalf("冲突回执要带占用该 nonce 的请求: %+v", got)
				}
			case ReceiptHistoryUnavailable:
				if len(got.Gaps) != 1 || got.Gaps[0] != (Gap{First: 1, Last: 8}) {
					t.Fatalf("缺口要带回: %+v", got)
				}
			}
		})
	}
}

func TestReceiptFailuresAreErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"store_failure_503", 503, `{"error":"history: 连 Postgres 失败"}`},
		{"bad_request_400", 400, `{"error":"bad request id"}`},
		{"unknown_status", 200, `{"requestId":"` + rid + `","status":"maybe","epoch":"k1-1"}`},
		{"missing_epoch", 404, `{"requestId":"` + rid + `","status":"not_found","asOfSeq":3}`},
		{"echo_mismatch", 404, `{"requestId":"0x2222222222222222222222222222222222222222222222222222222222222222","status":"not_found","epoch":"k1-1","asOfSeq":3}`},
		{"executed_without_seq", 200, `{"status":"executed","epoch":"k1-1","nonceConsumed":true,"events":[],"items":[]}`},
		{"not_found_without_as_of", 404, `{"requestId":"` + rid + `","status":"not_found","epoch":"k1-1"}`},
		{"not_found_under_503", 503, `{"requestId":"` + rid + `","status":"not_found","epoch":"k1-1","asOfSeq":3}`},
		{"executed_under_404", 404, `{"status":"executed","epoch":"k1-1","seq":12,"sub":0,"nonceConsumed":true,"events":[],"items":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := serveOnce(t, "/receipts/", tc.status, tc.body)
			if got, err := c.Receipt(context.Background(), rid, ReceiptQuery{}); err == nil {
				t.Fatalf("%s 应当报错,实得 %+v", tc.name, got)
			}
		})
	}
}

func TestReceiptQueryCarriesScopeAndNonce(t *testing.T) {
	c, seen := serveOnce(t, "/receipts/", 404, `{"requestId":"`+rid+`","status":"not_found","epoch":"k1-1","asOfSeq":3}`)
	nonce := uint64(5)
	if _, err := c.Receipt(context.Background(), rid, ReceiptQuery{Scope: "agent:0x19e7e376e7c213b7e7e7e46cc70a5dd086daff2a", Nonce: &nonce}); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "scope=agent%3A0x19e7e376e7c213b7e7e7e46cc70a5dd086daff2a") || !strings.Contains((*seen)[0], "nonce=5") {
		t.Fatalf("scope/nonce 没有带上: %v", *seen)
	}
	if _, err := c.Receipt(context.Background(), rid, ReceiptQuery{Scope: "agent:0x19"}); err == nil {
		t.Fatal("scope 与 nonce 必须成对")
	}
}

// ReceiptOf:按本地原请求身份查询,回执声称的签名者 / nonce 与原请求不符即报错。
func TestReceiptOfChecksIdentity(t *testing.T) {
	signer, _ := NewSigner(testAgentKey)
	id := AgentRequestIdentity{Agent: signer.Address(), Account: 7, Nonce: 5, SigningHash: "0x" + strings.Repeat("ab", 32)}
	want, err := id.RequestID()
	if err != nil {
		t.Fatal(err)
	}
	body := func(signerHex string, nonce string) string {
		return `{"status":"executed","epoch":"k1-1","seq":12,"sub":0,"nonceConsumed":true,"events":[],"items":[],"source":"local","requestId":"` + want +
			`","signer":"` + signerHex + `","nonceScope":"agent:` + signerHex + `","nonce":` + nonce + `}`
	}
	c, seen := serveOnce(t, "/receipts/", 200, body(signer.Address().Hex(), "5"))
	got, err := c.ReceiptOf(context.Background(), id, "k1-1")
	if err != nil || got.Status != ReceiptExecuted {
		t.Fatalf("身份一致的回执应当通过: %+v %v", got, err)
	}
	if !strings.Contains((*seen)[0], "/receipts/"+want) || !strings.Contains((*seen)[0], "nonce=5") {
		t.Fatalf("应按本地推导的请求身份 + agent scope + nonce 查询: %v", *seen)
	}
	c2, _ := serveOnce(t, "/receipts/", 200, body("0x0000000000000000000000000000000000000001", "5"))
	if _, err := c2.ReceiptOf(context.Background(), id, "k1-1"); err == nil {
		t.Fatal("签名者不符的回执不能当成自己的")
	}
	c3, _ := serveOnce(t, "/receipts/", 200, body(signer.Address().Hex(), "6"))
	if _, err := c3.ReceiptOf(context.Background(), id, "k1-1"); err == nil {
		t.Fatal("nonce 不符的回执不能当成自己的")
	}
	if _, err := c.ReceiptOf(context.Background(), id, "k2-2"); !errors.Is(err, ErrHistoryEpochMismatch) {
		t.Fatalf("别的纪元的回执不能当成原请求的: %v", err)
	}
	if _, err := c.ReceiptOf(context.Background(), id, ""); err == nil {
		t.Fatal("不给原请求纪元应当在本地拒绝")
	}
}

// CheckReplace 对权威回执用与写回执同一套逐项判据:全成功 nil、部分成功 *BatchOutcomeError、
// 证据矛盾 ErrIncompleteBatch;不是「已执行且 nonce 已消耗」的结论不是批次结局。
func TestReceiptCheckReplaceUsesBatchItemRules(t *testing.T) {
	req := ReplaceRequest{Identity: AgentRequestIdentity{Account: 42, Nonce: 5, SigningHash: "0x" + strings.Repeat("cd", 32)}, Market: 7, Cancels: []uint64{3}, Orders: receiptOrders()}
	mine, _ := req.Identity.RequestID()
	receipt := func(status string, consumed bool, items, events string) *RequestReceipt {
		var r RequestReceipt
		body := `{"status":"` + status + `","epoch":"k1-1","seq":120,"sub":0,"nonceConsumed":` + strconv.FormatBool(consumed) + `,"requestId":"` + mine + `","nonce":5,"items":[` + items + `],"events":[` + events + `]}`
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatal(err)
		}
		return &r
	}
	withSeq := func(ev string) string { return strings.Replace(ev, `{"kind"`, `{"seq":120,"sub":0,"idx":0,"kind"`, 1) }
	if err := receipt("executed", true, itemCancelOK+","+itemPlaceOK, withSeq(eventCanceled3)+","+withSeq(eventAccepted4)).CheckReplace(req); err != nil {
		t.Fatalf("逐项全成功的回执应当通过: %v", err)
	}
	var bo *BatchOutcomeError
	if err := receipt("executed", true, itemCancelOK+","+itemPlaceRejected, withSeq(eventCanceled3)).CheckReplace(req); !errors.As(err, &bo) || len(bo.Rejected) != 1 {
		t.Fatalf("逐项齐全的部分成功应当是 *BatchOutcomeError: %v", err)
	}
	if err := receipt("executed", true, itemCancelOK, withSeq(eventCanceled3)).CheckReplace(req); !errors.Is(err, ErrIncompleteBatch) {
		t.Fatalf("缺下单项的回执不可信: %v", err)
	}
	// 逐项结果一模一样、但身份是另一个请求的回执不能冒充这个请求
	other, nonce := receipt("executed", true, itemCancelOK+","+itemPlaceOK, withSeq(eventCanceled3)+","+withSeq(eventAccepted4)), uint64(6)
	other.RequestID = "0x" + strings.Repeat("ef", 32)
	if err := other.CheckReplace(req); err == nil {
		t.Fatal("别的请求的回执冒充了这个请求")
	}
	other.RequestID, other.Nonce = mine, &nonce
	if err := other.CheckReplace(req); err == nil {
		t.Fatal("nonce 不符的回执冒充了这个请求")
	}
	for _, r := range []*RequestReceipt{receipt("executed", false, itemCancelOK+","+itemPlaceOK, withSeq(eventCanceled3)+","+withSeq(eventAccepted4)), receipt("rejected", true, "", "")} {
		if err := r.CheckReplace(req); err == nil || errors.As(err, &bo) {
			t.Fatalf("%s/nonceConsumed=%v 不是批次结局: %v", r.Status, *r.NonceConsumed, err)
		}
	}
}

func TestRequestIDRejectsMalformedSigningHash(t *testing.T) {
	for _, h := range []string{"", "0x12", strings.Repeat("ab", 32), "0x" + strings.Repeat("zz", 32)} {
		if _, err := (AgentRequestIdentity{SigningHash: h}).RequestID(); err == nil {
			t.Fatalf("签名摘要 %q 不合法,应当报错", h)
		}
	}
}

const equityBody = `{"account":7,"epoch":"k1-1","viewEpoch":4,"upper":"16-65535-4294967295","finality":"raft_committed",
"groups":[{"token":0,"balance":"4999000000","equity":"5000200000","netInflow":"5000000000","realizedPnl":"0","unrealizedPnl":"200000","accruedFunding":"0"}],
"balances":[{"token":0,"balance":"4999000000","frozen":"0","netInflow":"5000000000","byKind":{"deposit":"5000000000","taker_fee":"-1000000"}}],
"positions":[{"market":0,"token":0,"lots":2,"oracle":"100100","quotePerTickLot":"1000","value":"200200000","costBasis":"200000000","unrealizedPnl":"200000","accruedFunding":"0"}]}`

func TestEquityParsesSnapshotAsStrings(t *testing.T) {
	c, seen := serveOnce(t, "/equity", 200, equityBody)
	got, err := c.Equity(context.Background(), 7, "k1-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Epoch != "k1-1" || got.ViewEpoch != 4 || got.Upper != "16-65535-4294967295" || len(got.Groups) != 1 || got.Groups[0].Equity != "5000200000" || got.Groups[0].NetInflow != "5000000000" {
		t.Fatalf("快照字段不对: %+v", got)
	}
	if got.Balances[0].ByKind["taker_fee"] != "-1000000" || got.Positions[0].Lots != 2 || got.Positions[0].CostBasis != "200000000" {
		t.Fatalf("分类合计 / 仓位丢失: %+v", got)
	}
	if !strings.Contains((*seen)[0], "account=7") || !strings.Contains((*seen)[0], "epoch=k1-1") {
		t.Fatalf("查询条件没带上: %v", *seen)
	}
}

func TestEquityFailuresAreNotSnapshots(t *testing.T) {
	c, _ := serveOnce(t, "/equity", 404, `{"error":"account not found","viewEpoch":4,"upper":"16-65535-4294967295"}`)
	if _, err := c.Equity(context.Background(), 7, ""); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("404 应当是 ErrNotRegistered,实得 %v", err)
	}
	c, _ = serveOnce(t, "/equity", 500, `{"error":"ledger_mismatch","token":0,"viewEpoch":4,"ledger":"1","balance":"2"}`)
	if got, err := c.Equity(context.Background(), 7, ""); err == nil || got != nil {
		t.Fatalf("服务端核对不等不能给出快照,实得 %+v %v", got, err)
	}
	c, _ = serveOnce(t, "/equity", 200, `{"account":7,"epoch":"k1-1","viewEpoch":4,"groups":[],"balances":[],"positions":[]}`)
	if _, err := c.Equity(context.Background(), 7, ""); err == nil {
		t.Fatal("缺 upper 的快照没法与流水对齐,应当报错")
	}
}

// 金样来自真节点(testdata/request_id_golden.json 的 source):服务端按命令字节推导的 requestId,
// 与 SDK 用本地签名摘要算出的必须逐字节相同 —— 否则按原请求身份永远查不到自己的回执。
func TestRequestIDMatchesServerDerivation(t *testing.T) {
	raw, err := os.ReadFile("testdata/request_id_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct{ Agent, SigningHash, RequestID string }
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	agent, err := ParseAddress(g.Agent)
	if err != nil {
		t.Fatal(err)
	}
	got, err := AgentRequestIdentity{Agent: agent, SigningHash: g.SigningHash}.RequestID()
	if err != nil || got != g.RequestID {
		t.Fatalf("本地请求身份 %s 与服务端 %s 不一致: %v", got, g.RequestID, err)
	}
}

// 真节点实录的部分成功回执(撤 1 挂 2,其中一张 PostOnly 穿价被拒)能被完整解析。
func TestReceiptParsesLivePartialSample(t *testing.T) {
	raw, err := os.ReadFile("testdata/receipt_partial_live.json")
	if err != nil {
		t.Fatal(err)
	}
	var sample struct{ RequestID string }
	_ = json.Unmarshal(raw, &sample)
	c, _ := serveOnce(t, "/receipts/", 200, string(raw))
	got, err := c.Receipt(context.Background(), sample.RequestID, ReceiptQuery{})
	if err != nil || got.Status != ReceiptExecuted || len(got.Items) != 3 || got.Items[2].State != "rejected" || *got.Items[2].Reason != "PostOnlyWouldCross" || got.Items[0].Kind != "cancel" || got.Items[0].CanceledLots == nil {
		t.Fatalf("实录回执解析不对: %+v %v", got, err)
	}
}

func TestEquityParsesLiveSample(t *testing.T) {
	raw, err := os.ReadFile("testdata/equity_live.json")
	if err != nil {
		t.Fatal(err)
	}
	var sample struct{ Account uint32 }
	_ = json.Unmarshal(raw, &sample)
	c, _ := serveOnce(t, "/equity", 200, string(raw))
	got, err := c.Equity(context.Background(), sample.Account, "")
	if err != nil || len(got.Groups) != 1 || got.Groups[0].NetInflow != "5000000000" {
		t.Fatalf("实录权益快照解析不对: %+v %v", got, err)
	}
}
