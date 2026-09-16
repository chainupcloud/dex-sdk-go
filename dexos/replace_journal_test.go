package dexos

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

type journalFixture struct {
	before func(context.Context, ReplaceRequest) error
	after  func(context.Context, BatchSubmission, error) error
}

func (j journalFixture) BeforeSend(ctx context.Context, request ReplaceRequest) error {
	return j.before(ctx, request)
}

func (j journalFixture) AfterReceive(ctx context.Context, result BatchSubmission, err error) error {
	return j.after(ctx, result, err)
}

func TestReplaceJournalPrecedesSingleFoldedWrite(t *testing.T) {
	agent, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var steps []string
	var saved ReplaceRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/replace" || r.URL.RawQuery != "wait=fold" {
			t.Errorf("批量请求未等待同一次写的执行结果: %s %s", r.Method, r.URL)
		}
		var wire struct {
			receiptRequest
			Cancels []uint64 `json:"cancels"`
			Orders  []struct {
				Price uint32 `json:"price"`
				Lots  uint64 `json:"lots"`
			} `json:"orders"`
		}
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(steps, []string{"prepare"}) || wire.NowMs != saved.Identity.NowMs || wire.Nonce != saved.Identity.Nonce || wire.Cancels[0] != 3 || wire.Orders[0].Price != 100 || wire.Orders[0].Lots != 2 {
			t.Error("发送先于落账或已准备的原请求被改写")
		}
		steps = append(steps, "send")
		_, _ = io.WriteString(w, completeReceipt)
	}))
	defer srv.Close()
	s := &Session{Client: NewClient(srv.URL, 84532), Agent: agent, Account: 42, nonce: 9}
	cancels, orders := []uint64{3}, receiptOrders()
	journal := journalFixture{
		before: func(_ context.Context, request ReplaceRequest) error {
			mu.Lock()
			defer mu.Unlock()
			saved = request
			// 回调视图与调用方输入都不能改写已经准备好的请求。
			request.Cancels[0], request.Orders[0].Price = 88, 999
			cancels[0], orders[0].Lots = 77, 999
			steps = append(steps, "prepare")
			return nil
		},
		after: func(_ context.Context, result BatchSubmission, sendErr error) error {
			mu.Lock()
			defer mu.Unlock()
			if sendErr != nil || result.Request == nil || *result.Request != saved.Identity || result.Receipt == nil || result.Receipt.Folded == nil || !*result.Receipt.Folded {
				t.Fatalf("记账没有收到本次实际请求/完整结果: %v", sendErr)
			}
			steps = append(steps, "result")
			return nil
		},
	}
	result, err := s.ReplaceWithJournal(context.Background(), 7, cancels, orders, journal)
	if err != nil || result.Request == nil || s.nonce != 10 || !reflect.DeepEqual(steps, []string{"prepare", "send", "result"}) {
		t.Fatalf("批次发送/记账顺序错误: steps=%v nonce=%d err=%v", steps, s.nonce, err)
	}
}

func TestReplaceJournalFailureFreezesWithoutResending(t *testing.T) {
	for _, stage := range []string{"prepare", "result"} {
		t.Run(stage, func(t *testing.T) {
			f := newReceiptFixture(t, http.StatusOK, completeReceipt)
			storeErr := errors.New("journal commit unavailable")
			prepares, results := 0, 0
			journal := journalFixture{
				before: func(context.Context, ReplaceRequest) error {
					prepares++
					if stage == "prepare" {
						return storeErr
					}
					return nil
				},
				after: func(context.Context, BatchSubmission, error) error { results++; return storeErr },
			}
			result, err := f.session.ReplaceWithJournal(context.Background(), 7, []uint64{3}, receiptOrders(), journal)
			wantWrites := 0
			if stage == "result" {
				wantWrites = 1
			}
			if !errors.Is(err, storeErr) || !errors.Is(err, ErrSessionBlocked) || result.Request == nil || f.calls != wantWrites || f.session.nonce != 0 || prepares != 1 || results != wantWrites {
				t.Fatalf("日志失败后发送/推进或丢失原身份: calls=%d err=%v", f.calls, err)
			}
			_, err = f.session.ReplaceWithJournal(context.Background(), 7, nil, receiptOrders(), journal)
			if !errors.Is(err, ErrSessionBlocked) || f.calls != wantWrites || prepares != 1 || f.session.Resync(context.Background()) == nil {
				t.Fatal("日志失败后重新创建请求、重试或重读 nonce")
			}
		})
	}
}

func TestReplaceJournalKeepsErrorEvidence(t *testing.T) {
	f := newReceiptFixture(t, http.StatusOK, `{"status":"ok","seq":120,"sub":0,"folded":false,"events":[]}`)
	seen := false
	journal := journalFixture{
		before: func(context.Context, ReplaceRequest) error { return nil },
		after: func(_ context.Context, result BatchSubmission, err error) error {
			seen = errors.Is(err, ErrExecutionPending) && result.Request != nil && result.Receipt != nil && result.Receipt.Folded != nil && !*result.Receipt.Folded
			return nil
		},
	}
	result, err := f.session.ReplaceWithJournal(context.Background(), 7, []uint64{3}, receiptOrders(), journal)
	if !seen || !errors.Is(err, ErrExecutionPending) || !errors.Is(err, ErrSessionBlocked) || result.Receipt == nil {
		t.Fatalf("未知执行结果没有随证据进入日志: %v", err)
	}
}

func TestReplaceJournalRechecksIdentityAndExpiry(t *testing.T) {
	for _, change := range []string{"account", "domain", "endpoint", "expiry"} {
		t.Run(change, func(t *testing.T) {
			f := newReceiptFixture(t, http.StatusOK, completeReceipt)
			if change == "expiry" {
				f.session.ValidUntilMs = uint64(time.Now().Add(time.Second).UnixMilli())
			}
			journal := journalFixture{before: func(context.Context, ReplaceRequest) error {
				switch change {
				case "account":
					f.session.Account++
				case "domain":
					f.session.Client.Domain.Name = "changed-after-prepare"
				case "endpoint":
					f.session.Client.BaseURL += "/other-instance"
				case "expiry":
					time.Sleep(time.Until(time.UnixMilli(int64(f.session.ValidUntilMs))) + 10*time.Millisecond)
				}
				return nil
			}, after: func(context.Context, BatchSubmission, error) error {
				t.Fatal("未发送不应出现已收到结果")
				return nil
			}}
			result, err := f.session.ReplaceWithJournal(context.Background(), 7, []uint64{3}, receiptOrders(), journal)
			if !errors.Is(err, ErrSessionBlocked) || result.Request == nil || f.calls != 0 {
				t.Fatalf("记账后身份/授权变化仍然发出: %s %v", change, err)
			}
		})
	}
}

func TestReplaceJournalCannotRewriteReturnedEvidence(t *testing.T) {
	f := newReceiptFixture(t, http.StatusOK, completeReceipt)
	journal := journalFixture{before: func(context.Context, ReplaceRequest) error { return nil }, after: func(_ context.Context, result BatchSubmission, _ error) error {
		result.Request.Nonce = 999
		*result.Receipt.Seq = 999
		result.Receipt.Events[0].Data[0] = 'X'
		return nil
	}}
	result, err := f.session.ReplaceWithJournal(context.Background(), 7, []uint64{3}, receiptOrders(), journal)
	if err != nil || result.Request.Nonce != 0 || *result.Receipt.Seq != 120 || result.Receipt.Events[0].Data[0] != '{' {
		t.Fatalf("日志回调改写 SDK 回执证据: %v", err)
	}
}
