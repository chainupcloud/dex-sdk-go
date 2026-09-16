package dexos

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const receiptEvents = `[{"kind":"OrderCanceled","data":{"account":42,"market":7,"orderSeq":3,"remainingLots":2}},{"kind":"OrderAccepted","data":{"account":42,"market":7,"orderSeq":4,"price":100,"lots":2,"filledLots":0,"resting":true}}]`

// batchStatus 值集来自 dex-os api.rs@9d942f2:578–586：ok / none / partial。
const completeReceipt = `{"status":"ok","seq":120,"sub":0,"folded":true,"submitted":1,"submittedCancels":1,"accepted":1,"canceled":1,"rejected":0,"batchStatus":"ok","events":` + receiptEvents + `}`

type receiptRequest struct {
	Agent     string `json:"agent"`
	Account   uint32 `json:"account"`
	Nonce     uint64 `json:"nonce"`
	NowMs     uint64 `json:"nowMs"`
	Signature string `json:"signature"`
}

type receiptFixture struct {
	mu      sync.Mutex
	calls   int
	request receiptRequest
	session *Session
}

func newReceiptFixture(t *testing.T, status int, body string) *receiptFixture {
	t.Helper()
	f := &receiptFixture{}
	agent, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if r.Method != http.MethodPost || r.URL.Path != "/replace" || r.URL.RawQuery != "wait=fold" {
			t.Errorf("回执路径被改成其他请求/重读 nonce: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&f.request); err != nil {
			t.Error(err)
			return
		}
		if body == "disconnect" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, 84532)
	c.Domain.Name, c.Domain.Version = "receipt-fixture", "9"
	c.Domain.VerifyingContract = Address{19: 17}
	f.session = &Session{Client: c, Agent: agent, Account: 42, nonce: 0}
	return f
}

func receiptOrders() []BatchOrder {
	return []BatchOrder{{Market: 7, Side: Buy, Price: 100, Lots: 2, TIF: PostOnly}}
}

func TestReplaceReceiptPreservesRequestAndMetadata(t *testing.T) {
	f := newReceiptFixture(t, http.StatusOK, completeReceipt)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := f.session.ReplaceWithReceipt(ctx, 7, []uint64{3}, receiptOrders())
	if err != nil || result.Request == nil || result.Receipt == nil || result.Receipt.Seq == nil || result.Receipt.Sub == nil || result.Receipt.Folded == nil {
		t.Fatalf("SDK 丢失原请求身份或回执元数据: request=%t receipt=%t err=%v", result.Request != nil, result.Receipt != nil, err)
	}
	if *result.Receipt.Seq != 120 || *result.Receipt.Sub != 0 || !*result.Receipt.Folded || result.Receipt.Rejected == nil || *result.Receipt.Rejected != 0 || len(result.Events()) != 2 {
		t.Fatal("合法零值或原始事件没有保留")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	request := f.request
	identity := result.Request
	if identity.Account != request.Account || identity.Account != 42 || identity.Agent != f.session.Agent.Address() || identity.Agent.Hex() != request.Agent || identity.Nonce != 0 || identity.Nonce != request.Nonce || identity.NowMs != request.NowMs || identity.CodecVersion != CodecVersion || identity.Domain != f.session.Client.Domain || f.calls != 1 {
		t.Fatal("本地请求身份不是本次实际发送的身份")
	}
	// codec 由原 Rust 金样独立约束；此处证明元数据来自这份真实传输的签名输入。
	command := EncodeBatchReplace(42, request.NowMs, []OrderID{NewOrderID(7, 3)}, receiptOrders())
	if identity.CommandHash != "0x"+hex.EncodeToString(Keccak256(command)) || identity.SigningHash != "0x"+hex.EncodeToString(identity.Domain.AgentExecHash(command, request.Nonce)) {
		t.Fatal("请求摘要与实际传输内容不一致")
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(identity.SigningHash, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	signature, err := f.session.Agent.SignHashHex(digest)
	if err != nil || signature != request.Signature {
		t.Fatal("请求身份中的摘要不是实际已签摘要")
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), request.Signature) || strings.Contains(string(encoded), f.session.Agent.PrivateKeyHex()) {
		t.Fatal("回执证据包含私钥或可重放签名")
	}
}

func TestReplaceReceiptDistinguishesAbsentOptionalFields(t *testing.T) {
	f := newReceiptFixture(t, http.StatusOK, `{"status":"ok","seq":120,"events":`+receiptEvents+`}`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := f.session.ReplaceWithReceipt(ctx, 7, []uint64{3}, receiptOrders())
	if err != nil || result.Receipt == nil || result.Receipt.Seq == nil || result.Receipt.Sub != nil || result.Receipt.Folded != nil || result.Receipt.Accepted != nil || result.Receipt.Rejected != nil {
		t.Fatalf("缺元数据被填成合法零值或破坏旧协议: %v", err)
	}
}

func TestExplicitPendingNeverSucceedsEvenWithEvents(t *testing.T) {
	f := newReceiptFixture(t, http.StatusOK, strings.Replace(completeReceipt, `"folded":true`, `"folded":false`, 1))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := f.session.ReplaceWithReceipt(ctx, 7, []uint64{3}, receiptOrders())
	if !errors.Is(err, ErrExecutionPending) || !errors.Is(err, ErrSessionBlocked) {
		t.Fatalf("明确 pending 被当成执行成功: %v", err)
	}
	if result.Receipt == nil || result.Receipt.Folded == nil || *result.Receipt.Folded || len(result.Events()) != 2 || f.session.nonce != 0 {
		t.Fatal("pending 回执被丢弃或错误推进 nonce")
	}
}

func TestReceiptFailurePreservesAvailableEvidenceAndBlocks(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantParsed bool
		wantEvents int
	}{
		{"partial", http.StatusOK, `{"status":"ok","seq":120,"sub":2,"folded":true,"batchStatus":"partial","events":[{"kind":"OrderCanceled","data":{"account":42,"market":7,"orderSeq":3}}]}`, true, 1},
		{"pending", http.StatusOK, `{"status":"ok","seq":120,"sub":2,"folded":false,"events":[]}`, true, 0},
		{"rejected", http.StatusOK, `{"status":"rejected","seq":120,"sub":2,"folded":true,"reason":"InsufficientMargin","events":[]}`, true, 0},
		{"missing_seq", http.StatusOK, `{"status":"ok","events":[]}`, true, 0},
		{"bad_seq_type", http.StatusOK, `{"status":"ok","seq":"bad","events":[]}`, false, 0},
		{"bad_sub_type", http.StatusOK, `{"status":"ok","seq":120,"sub":65536,"events":[]}`, false, 0},
		{"broken_json", http.StatusOK, `{"status":"ok","seq":120,`, false, 0},
		{"null_body", http.StatusOK, `null`, false, 0},
		{"http_failure", http.StatusServiceUnavailable, `unavailable`, false, 0},
		{"disconnect", http.StatusOK, `disconnect`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReceiptFixture(t, tc.status, tc.body)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := f.session.ReplaceWithReceipt(ctx, 7, []uint64{3}, receiptOrders())
			if !errors.Is(err, ErrSessionBlocked) || result.Request == nil || (result.Receipt != nil) != tc.wantParsed || len(result.Events()) != tc.wantEvents || f.session.nonce != 0 {
				t.Fatalf("失败时丢失/伪造请求或回执证据: request=%t receipt=%t events=%d nonce=%d err=%v", result.Request != nil, result.Receipt != nil, len(result.Events()), f.session.nonce, err)
			}
			if tc.name == "rejected" && (result.Receipt.Reason == nil || *result.Receipt.Reason != "InsufficientMargin") {
				t.Fatal("明确拒因被丢失")
			}
			if tc.name == "missing_seq" && result.Receipt.Seq != nil {
				t.Fatal("缺失 seq 被补成0")
			}
			blocked, nextErr := f.session.ReplaceWithReceipt(ctx, 7, []uint64{3}, receiptOrders())
			if !errors.Is(nextErr, ErrSessionBlocked) || blocked.Request != nil || blocked.Receipt != nil {
				t.Fatal("已阻断调用创建了新请求证据或冒充恢复")
			}
			if err := f.session.Resync(ctx); !errors.Is(err, ErrSessionBlocked) {
				t.Fatal("未决请求允许重读 nonce")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.calls != 1 || result.Request.Nonce != f.request.Nonce || result.Request.NowMs != f.request.NowMs {
				t.Fatal("请求失败后重发/重读或回填了另一请求身份")
			}
		})
	}
}

func TestReceiptAggregateClaimsCannotOverrideEvents(t *testing.T) {
	for _, field := range []string{"submitted", "submittedCancels", "accepted", "canceled", "rejected", "batchStatus", "events"} {
		t.Run(field, func(t *testing.T) {
			var body map[string]json.RawMessage
			if err := json.Unmarshal([]byte(completeReceipt), &body); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "batchStatus":
				body[field] = json.RawMessage(`"partial"`)
			case "events":
				body[field] = json.RawMessage(`[]`)
			case "rejected":
				body[field] = json.RawMessage(`1`)
			default:
				body[field] = json.RawMessage(`9`)
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			f := newReceiptFixture(t, http.StatusOK, string(raw))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := f.session.ReplaceWithReceipt(ctx, 7, []uint64{3}, receiptOrders())
			if !errors.Is(err, ErrIncompleteBatch) || !errors.Is(err, ErrSessionBlocked) || result.Receipt == nil {
				t.Fatalf("聚合声明覆盖逐项证据或矛盾回执被接受: %s err=%v", field, err)
			}
		})
	}
}

func TestReceiptNullMetadataRemainsUnknown(t *testing.T) {
	body := `{"status":"ok","seq":120,"sub":null,"folded":null,"accepted":null,"rejected":null,"events":` + receiptEvents + `}`
	f := newReceiptFixture(t, http.StatusOK, body)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := f.session.ReplaceWithReceipt(ctx, 7, []uint64{3}, receiptOrders())
	if err != nil || result.Receipt == nil || result.Receipt.Sub != nil || result.Receipt.Folded != nil || result.Receipt.Accepted != nil || result.Receipt.Rejected != nil {
		t.Fatalf("null 元数据被补值: %v", err)
	}
}

func TestReceiptNotConstructedBeforeSessionAdmission(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "expired"}[expired], func(t *testing.T) {
			f := newReceiptFixture(t, http.StatusOK, completeReceipt)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if expired {
				f.session.ValidUntilMs = uint64(time.Now().Add(-time.Minute).UnixMilli())
			} else {
				cancel()
			}
			result, err := f.session.ReplaceWithReceipt(ctx, 7, []uint64{3}, receiptOrders())
			if err == nil || result.Request != nil || result.Receipt != nil || f.session.nonce != 0 {
				t.Fatal("准入拒绝仍创建请求或消耗 nonce")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.calls != 0 {
				t.Fatal("准入拒绝仍访问场所")
			}
		})
	}
}

func TestLegacyReplaceKeepsOneWriteAndSessionNonce(t *testing.T) {
	f := newReceiptFixture(t, http.StatusOK, completeReceipt)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	events, err := f.session.Replace(ctx, 7, []uint64{3}, receiptOrders())
	if err != nil || len(events) != 2 || f.session.nonce != 1 {
		t.Fatalf("旧 Replace 行为被破坏: events=%d nonce=%d err=%v", len(events), f.session.nonce, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls != 1 {
		t.Fatal("兼容包装额外发送了写请求")
	}
}

func TestReplaceNilCancelsAreAnEmptyJSONArray(t *testing.T) {
	agent, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient("http://example.invalid", 84532)
	c.HTTP.Transport = safetyTransport(func(r *http.Request) (*http.Response, error) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body["cancels"]) != "[]" || string(body["orders"]) != "[]" {
			t.Fatal("空列表编码为 null，不符合 Rust Vec 请求契约")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"status":"ok","seq":1,"events":[]}`)), Header: make(http.Header)}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.BatchReplace(ctx, agent, 42, 7, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptNonSuccessBatchStatusCannotPass(t *testing.T) {
	for _, status := range []string{"none", "partial", "all", "unknown"} {
		t.Run(status, func(t *testing.T) {
			body := strings.Replace(completeReceipt, `"batchStatus":"ok"`, `"batchStatus":"`+status+`"`, 1)
			f := newReceiptFixture(t, http.StatusOK, body)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := f.session.ReplaceWithReceipt(ctx, 7, []uint64{3}, receiptOrders())
			if !errors.Is(err, ErrIncompleteBatch) || !errors.Is(err, ErrSessionBlocked) || result.Receipt == nil || result.Receipt.BatchStatus == nil || *result.Receipt.BatchStatus != status {
				t.Fatalf("非成功/未知 batchStatus 被放行或丢失: %s err=%v", status, err)
			}
		})
	}
}
