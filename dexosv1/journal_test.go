package dexosv1

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// recordJournal 记下两个回调看到的东西;before / after 非 nil 时按它们的返回值答。
type recordJournal struct {
	f          *writeFake
	intent     *ReplaceIntent
	sentBefore int // BeforeSend 时假网关已经收到的写请求数
	result     *BatchResult
	sendErr    error
	afterCalls int
	before     error
	after      error
}

func (j *recordJournal) BeforeSend(_ context.Context, in ReplaceIntent) error {
	j.intent = &in
	j.sentBefore = j.f.count()
	return j.before
}

func (j *recordJournal) AfterReceive(_ context.Context, in ReplaceIntent, r *BatchResult, err error) error {
	if j.intent == nil || in.Request != j.intent.Request {
		panic("AfterReceive 的 intent 不是 BeforeSend 那一份")
	}
	j.result, j.sendErr = r, err
	j.afterCalls++
	return j.after
}

// 写前日志在发送之前拿到完整的请求身份与命令,两者能独立重算;收到结论后再记一次。
func TestReplaceJournaledHooks(t *testing.T) {
	f, s, cat := newTestSession(t)
	btc, _ := cat.Market("BTC-USD")
	f.answer(t, "/v1/orders/replace", 200, "w_replace.json")
	j := &recordJournal{f: f}
	res, err := s.ReplaceJournaled(context.Background(), btc, []uint64{101}, []OrderRequest{{Market: btc, Side: dexos.Buy, Price: 64900, Lots: 200, TIF: dexos.PostOnly}}, j)
	if err != nil {
		t.Fatal(err)
	}
	in := j.intent
	if in == nil || j.sentBefore != 0 || f.count() != 1 {
		t.Fatalf("BeforeSend 应在发送之前调用: before=%d sent=%d", j.sentBefore, f.count())
	}
	if in.SigningAccountID != signingID || in.Domain != testDomain || in.Market != btc || in.Request.Nonce != 41 || len(in.Cancels) != 1 || in.Cancels[0] != 101 {
		t.Fatalf("intent: %+v", in)
	}
	cmd := dexos.EncodeBatchReplace(in.SigningAccountID, in.NowMs, []dexos.OrderID{dexos.NewOrderID(btc.SigningMarketID, 101)}, in.Orders)
	if in.Request.CommandHash != "0x"+hex.EncodeToString(dexos.Keccak256(cmd)) ||
		in.Request.SigningHash != "0x"+hex.EncodeToString(in.Domain.AgentExecHash(cmd, in.Request.Nonce)) {
		t.Fatal("intent 的命令摘要不能由它自己的字段重算出来")
	}
	// 发出去的就是 intent 里的那条命令
	body := m(f.last(t).body)
	if num(t, body["nowMs"]) != in.NowMs || recoverAddr(t, in.Domain.AgentExecHash(cmd, 41), body["signature"].(string)) != agentAddr {
		t.Fatal("线上请求与 intent 不是同一条命令")
	}
	if j.afterCalls != 1 || j.result != res || j.sendErr != nil || res.Request.RequestID != in.Request.RequestID {
		t.Fatalf("AfterReceive: calls=%d result=%v err=%v", j.afterCalls, j.result, j.sendErr)
	}
	if s.Nonce() != 42 {
		t.Fatalf("成功之后 nonce 应为 42,得到 %d", s.Nonce())
	}
}

func TestReplaceJournaledOutcomes(t *testing.T) {
	declined := errors.New("准入不过")
	stored := errors.New("库写失败")
	cases := []struct {
		name          string
		status        int
		fixture       string
		before, after error
		sent          bool
		nonce         uint64
		blocked       bool
		check         func(*testing.T, error, *recordJournal)
	}{
		{name: "落账前拒绝:不发、不锁、nonce 不动", before: errorsJoin(ErrRequestDeclined, declined), nonce: 41,
			check: func(t *testing.T, err error, j *recordJournal) {
				if !errors.Is(err, ErrRequestDeclined) || !errors.Is(err, declined) || j.afterCalls != 0 {
					t.Fatalf("应原样报出拒绝且不调 AfterReceive: %v", err)
				}
			}},
		{name: "BeforeSend 失败:不发、锁定", before: stored, nonce: 41, blocked: true},
		{name: "AfterReceive 失败:已发、锁定、nonce 不推进", status: 200, fixture: "w_replace.json", after: stored, sent: true, nonce: 41, blocked: true,
			check: func(t *testing.T, err error, j *recordJournal) {
				if !errors.Is(err, stored) || j.result == nil {
					t.Fatalf("应报出日志错误,AfterReceive 拿到结论: %v", err)
				}
			}},
		{name: "限流:日志看到类型化错误,不锁、nonce 不动", status: 429, fixture: "err429_account.json", sent: true, nonce: 41,
			check: func(t *testing.T, err error, j *recordJournal) {
				var rl *RateLimitedError
				if !errors.As(err, &rl) || !errors.As(j.sendErr, &rl) || rl.Bucket != "account" || j.result != nil {
					t.Fatalf("应把 429 交给日志并原样返回: %v / %v", err, j.sendErr)
				}
			}},
		{name: "业务拒绝:nonce 已消耗,不锁", status: 422, fixture: "err422_margin.json", sent: true, nonce: 42,
			check: func(t *testing.T, err error, j *recordJournal) {
				var e *Error
				if !errors.As(j.sendErr, &e) || e.Code != "InsufficientMargin" {
					t.Fatalf("日志应看到 422 拒因: %v", j.sendErr)
				}
			}},
		{name: "业务拒绝但日志失败:锁定", status: 422, fixture: "err422_margin.json", after: stored, sent: true, nonce: 41, blocked: true},
		{name: "202:结论未知,锁定", status: 202, fixture: "w_sequenced_202.json", sent: true, nonce: 41, blocked: true,
			check: func(t *testing.T, err error, j *recordJournal) {
				if !errors.Is(j.sendErr, ErrPending) {
					t.Fatalf("日志应看到 ErrPending: %v", j.sendErr)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, s, cat := newTestSession(t)
			btc, _ := cat.Market("BTC-USD")
			if c.fixture != "" {
				f.answer(t, "/v1/orders/replace", c.status, c.fixture)
			}
			j := &recordJournal{f: f, before: c.before, after: c.after}
			orders := []OrderRequest{{Market: btc, Side: dexos.Buy, Price: 64900, Lots: 200, TIF: dexos.PostOnly}}
			_, err := s.ReplaceJournaled(context.Background(), btc, []uint64{101}, orders, j)
			if err == nil {
				t.Fatal("应报错")
			}
			if (f.count() == 1) != c.sent {
				t.Fatalf("发送了 %d 次", f.count())
			}
			if s.Nonce() != c.nonce {
				t.Fatalf("nonce 应为 %d,得到 %d", c.nonce, s.Nonce())
			}
			if blocked := errors.Is(err, ErrSessionBlocked); blocked != c.blocked {
				t.Fatalf("锁定应为 %v: %v", c.blocked, err)
			}
			if c.blocked && (s.Pending() == nil || s.Pending().RequestID != j.intent.Request.RequestID) {
				t.Fatalf("锁定后 Pending 应是这次请求: %+v", s.Pending())
			}
			if c.check != nil {
				c.check(t, err, j)
			}
		})
	}
}

func errorsJoin(sentinel, cause error) error { return errors.Join(sentinel, cause) }

// 浏览器钱包签好的授权:提交时不经 nonce 计数器,请求身份按主钱包与签名摘要算,与网关核对。
func TestSubmitApproval(t *testing.T) {
	f, c := newWriteFake(t)
	owner, agent := mustAddr(t, ownerAddr), mustAddr(t, agentAddr)
	digest := testDomain.ApproveAgentHash(owner, signingID, agent, 0, 5)
	sig, err := signer(t, ownerKey).SignHashHex(digest)
	if err != nil {
		t.Fatal(err)
	}
	a := SignedApproval{Account: AccountRef{Address: owner}, SigningAccountID: signingID, Nonce: 5, Domain: testDomain, Agent: agent, Signature: sig, Label: "mm"}
	f.answer(t, "/v1/account/agents/approve", 200, "w_approve.json")
	r, err := c.SubmitApproval(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	body := m(f.last(t).body)
	conformsRequest(t, "/v1/account/agents/approve", body)
	if body["signer"] != nil || num(t, body["nonce"]) != 5 || body["signature"] != sig || body["label"] != "mm" || r.Seq != 506 || r.Request.Scope != "account:7" {
		t.Fatalf("提交的授权: %v / %+v", body, r)
	}
	f.answer(t, "/v1/account/agents/approve", 422, "err422_nonce.json")
	var e *Error
	if _, err := c.SubmitApproval(context.Background(), a); !errors.As(err, &e) || e.Code != "NonceMismatch" {
		t.Fatalf("nonce 已被用掉应报 NonceMismatch: %v", err)
	}
	before := f.count()
	a.Signature = "0x12"
	if _, err := c.SubmitApproval(context.Background(), a); err == nil || f.count() != before {
		t.Fatalf("签名形状不对应本地报错、不发请求: %v", err)
	}
}

func TestExchangeVersionSubaccountsAndMarketFees(t *testing.T) {
	f, c := newFake(t)
	f.routes["/v1/account/subaccounts"] = serve(t, http.StatusOK, "subaccounts.json")
	f.routes["/v1/markets"] = serve(t, http.StatusOK, "markets.json")
	subs, err := c.Subaccounts(context.Background(), mustAddr(t, ownerAddr))
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 || !subs[0].IsMain || subs[0].SigningAccountID != 7 || subs[1].Subaccount != 1 || subs[1].SigningAccountID != 9 {
		t.Fatalf("账户表: %+v", subs)
	}
	if _, err := c.Subaccounts(context.Background(), mustAddr(t, agentAddr)); err == nil {
		t.Fatal("答的不是所问地址时应报错")
	}
	f.routes["/v1/exchange"] = serve(t, http.StatusOK, "exchange.json")
	ex, err := c.Exchange(context.Background())
	if err != nil || ex.Version.CodecVer != dexos.CodecVersion {
		t.Fatalf("网关的编码版本应为 %d: %+v / %v", dexos.CodecVersion, ex, err)
	}
	cat, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	btc, _ := cat.Market("BTC-USD")
	eth, _ := cat.Market("ETH-USD")
	if btc.Fees == nil || btc.Fees.RateSource != "tier" || eth.Fees.RateSource != "market" || eth.Fees.MakerRate != "-0.0001" || eth.Fees.TakerRate != "0.0005" {
		t.Fatalf("市场费率: %+v / %+v", btc.Fees, eth.Fees)
	}
}
