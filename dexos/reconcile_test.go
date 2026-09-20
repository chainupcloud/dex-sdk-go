package dexos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 这组测试钉住写回执三态与会话锁的边界:
//
//   - 业务拒绝是 200 + status:"rejected" —— 必须变成带拒因的 *RejectedError,不能泛化成
//     「结果未知」;它是终局结论,**不锁会话**,本地 nonce 按内核检查顺序决定 ++ 与否。
//   - NonceMismatch / folded:false / 传输失败仍然锁定(结论未知或计数器不可信)。
//   - 经 /agent/exec 的会话包装方法全部走 Session.write。
//   - /fills 补拉自动翻页到底、从游标续拉、游标不前进即报错。

type fakeGateway struct {
	*httptest.Server

	mu           sync.Mutex
	writes       []recordedWrite
	reply        func(n int) any
	serverNonce  uint64
	writeStatus  int
	fillsHandler http.HandlerFunc
}

type recordedWrite struct {
	Path  string
	Query string
	Nonce uint64
}

// 测试用 API 钱包私钥(仅用于本地假网关,不对应任何真实资产)。
const testAgentKey = "0x1111111111111111111111111111111111111111111111111111111111111111"

func testAgentAddr() string {
	s, err := NewSigner(testAgentKey)
	if err != nil {
		panic(err)
	}
	return s.Address().Hex()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newFakeGateway(t *testing.T, startNonce uint64) *fakeGateway {
	t.Helper()
	g := &fakeGateway{serverNonce: startNonce}
	mux := http.NewServeMux()
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"chainId": uint64(31337), "codecVer": CodecVersion, "snapshotVer": 22, "finalityMode": "head",
			"domain": map[string]any{
				"name": "dex-os", "version": "1",
				"verifyingContract": "0x0000000000000000000000000000000000000000",
			},
		})
	})
	mux.HandleFunc("/agent/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost { // /agent/exec
			g.serveWrite(w, r)
			return
		}
		g.mu.Lock()
		n := g.serverNonce
		g.mu.Unlock()
		writeJSON(w, map[string]any{
			"agent":     testAgentAddr(),
			"grants":    []map[string]any{{"master": 7, "validUntilMs": "0", "expired": false}},
			"nextNonce": n,
		})
	})
	mux.HandleFunc("/agents/", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		n := g.serverNonce
		g.mu.Unlock()
		writeJSON(w, map[string]any{
			"agents": []map[string]any{{"address": testAgentAddr(), "nextNonce": n, "expired": false}},
		})
	})
	for _, p := range []string{"/batch", "/cancel", "/replace"} {
		mux.HandleFunc(p, g.serveWrite)
	}
	mux.HandleFunc("/fills", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		h := g.fillsHandler
		g.mu.Unlock()
		if h == nil {
			writeJSON(w, map[string]any{"fills": []any{}, "hasMore": false})
			return
		}
		h(w, r)
	})
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

func (g *fakeGateway) serveWrite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Nonce uint64 `json:"nonce"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	g.mu.Lock()
	n := len(g.writes)
	g.writes = append(g.writes, recordedWrite{Path: r.URL.Path, Query: r.URL.RawQuery, Nonce: body.Nonce})
	reply, status := g.reply, g.writeStatus
	g.mu.Unlock()
	if status != 0 {
		http.Error(w, "boom", status)
		return
	}
	if reply == nil {
		writeJSON(w, map[string]any{"status": "ok", "seq": 1, "sub": 0, "folded": true, "events": []any{}})
		return
	}
	writeJSON(w, reply(n))
}

func (g *fakeGateway) recorded() []recordedWrite {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]recordedWrite(nil), g.writes...)
}

// serveFills 装一个按游标分页的 /fills,行为与网关一致:严格大于 after、按 id 升序、多取一条判 hasMore。
func (g *fakeGateway) serveFills(all []map[string]any, pageSize int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fillsHandler = func(w http.ResponseWriter, r *http.Request) {
		after := r.URL.Query().Get("after")
		start := 0
		if after != "" {
			for i, f := range all {
				if f["id"] == after {
					start = i + 1
					break
				}
			}
		}
		end := min(start+pageSize, len(all))
		page := all[start:end]
		next := ""
		if len(page) > 0 {
			next = page[len(page)-1]["id"].(string)
		}
		writeJSON(w, map[string]any{"account": 7, "fills": page, "next": next, "hasMore": end < len(all)})
	}
}

func newTestSession(t *testing.T, g *fakeGateway) *Session {
	t.Helper()
	s, err := New(context.Background(), g.URL, testAgentKey)
	if err != nil {
		t.Fatalf("会话初始化失败:%v", err)
	}
	return s
}

func rejected(reason string) func(int) any {
	return func(int) any {
		return map[string]any{"status": "rejected", "reason": reason, "seq": 12, "sub": 3, "folded": true, "events": []any{}}
	}
}

// 业务拒绝:带拒因的具名错误;终局结论,nonce 已消耗,会话不锁。
func TestBusinessRejectionIsTypedConsumesNonceAndDoesNotBlock(t *testing.T) {
	g := newFakeGateway(t, 10)
	s := newTestSession(t, g)
	g.reply = rejected("InsufficientMargin")

	_, err := s.Place(context.Background(), BatchOrder{Market: 0, Side: Buy, Price: 1, Lots: 1, TIF: GTC})
	var re *RejectedError
	if !errors.As(err, &re) {
		t.Fatalf("业务拒绝应当是 *RejectedError,实际 %T:%v", err, err)
	}
	if re.Reason != "InsufficientMargin" || re.Seq != 12 || re.Sub != 3 {
		t.Fatalf("拒因与身份要原样带出,实得 %+v", re)
	}
	if errors.Is(err, ErrSessionBlocked) {
		t.Fatal("终局的业务拒绝不该锁会话")
	}

	g.reply = nil
	if _, err := s.ScheduleCancel(context.Background(), time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("业务拒绝之后下一笔写应当照常发出,实得 %v", err)
	}
	w := g.recorded()
	if len(w) != 2 || w[0].Nonce != 10 || w[1].Nonce != 11 {
		t.Fatalf("业务拒绝在 nonce 写入之后,计数器应当 ++:实得 %+v", w)
	}
}

// nonce 之前的拒绝(scope 越权等):计数器没动,同样不锁。
func TestPreNonceRejectionKeepsNonceAndDoesNotBlock(t *testing.T) {
	g := newFakeGateway(t, 10)
	s := newTestSession(t, g)
	g.reply = rejected("AgentScopeViolation")
	if _, err := s.ScheduleCancel(context.Background(), time.Time{}); err == nil {
		t.Fatal("scope 越权必须报错")
	}
	g.reply = nil
	if _, err := s.ScheduleCancel(context.Background(), time.Time{}); err != nil {
		t.Fatalf("不该被锁:%v", err)
	}
	w := g.recorded()
	if len(w) != 2 || w[0].Nonce != 10 || w[1].Nonce != 10 {
		t.Fatalf("nonce 之前的拒绝没消耗计数器,应当重用 10:实得 %+v", w)
	}
}

// NonceMismatch 与 folded:false 仍然锁定 —— 计数器不可信 / 结论未知。
func TestNonceMismatchAndPendingStillBlock(t *testing.T) {
	for name, reply := range map[string]func(int) any{
		"nonce_mismatch": rejected(ReasonNonceMismatch),
		"pending": func(int) any {
			return map[string]any{"status": "ok", "seq": 12, "sub": 0, "folded": false, "events": []any{}}
		},
	} {
		g := newFakeGateway(t, 10)
		s := newTestSession(t, g)
		g.reply = reply
		if _, err := s.ScheduleCancel(context.Background(), time.Time{}); err == nil {
			t.Fatalf("%s:必须报错", name)
		}
		if _, err := s.ScheduleCancel(context.Background(), time.Time{}); !errors.Is(err, ErrSessionBlocked) {
			t.Fatalf("%s:之后的写必须被 ErrSessionBlocked 挡住,实得 %v", name, err)
		}
		if len(g.recorded()) != 1 {
			t.Fatalf("%s:锁定后不得再发请求", name)
		}
	}
}

// 经 /agent/exec 的会话方法全部走 Session.write:带 ?wait=fold、成功即 ++。
func TestSessionAgentExecWrappersGoThroughWrite(t *testing.T) {
	g := newFakeGateway(t, 10)
	s := newTestSession(t, g)
	ctx := context.Background()
	calls := []func() ([]EventEnvelope, error){
		func() ([]EventEnvelope, error) { return s.SetLeverage(ctx, 0, ImfPpmForLeverage(5)) },
		func() ([]EventEnvelope, error) {
			return s.PlaceGTT(ctx, BatchOrder{Market: 0, Side: Buy, Price: 1, Lots: 1, TIF: GTC}, time.Time{})
		},
		func() ([]EventEnvelope, error) { return s.Modify(ctx, 0, 7, 2, 1, GTC, false, time.Time{}) },
		func() ([]EventEnvelope, error) {
			return s.PlaceConditional(ctx, Conditional{Market: 0, Side: Sell, Price: 1, Lots: 1, TriggerPrice: 2})
		},
		func() ([]EventEnvelope, error) { return s.CancelConditional(ctx, 0, 8) },
		func() ([]EventEnvelope, error) {
			return s.PlaceTwap(ctx, Twap{Market: 0, Side: Buy, TotalLots: 10, Slices: 2, IntervalMs: 1000})
		},
		func() ([]EventEnvelope, error) { return s.CancelTwap(ctx, 0, 9) },
		func() ([]EventEnvelope, error) {
			return s.PlaceTpslPair(ctx, TpslPair{Market: 0, CloseSide: Sell, Lots: 1, TpTrigger: 3, SlTrigger: 1, PositionTpsl: true})
		},
	}
	for i, f := range calls {
		if _, err := f(); err != nil {
			t.Fatalf("第 %d 个会话方法失败:%v", i, err)
		}
	}
	w := g.recorded()
	if len(w) != len(calls) {
		t.Fatalf("期望 %d 次写,收到 %d 次", len(calls), len(w))
	}
	for i, r := range w {
		if r.Path != "/agent/exec" || !strings.Contains(r.Query, "wait=fold") || r.Nonce != 10+uint64(i) {
			t.Errorf("第 %d 次写:path=%s query=%q nonce=%d", i, r.Path, r.Query, r.Nonce)
		}
	}
	if ImfPpmForLeverage(5) != 200000 || ImfPpmForLeverage(0) != 0 {
		t.Errorf("ImfPpmForLeverage 换算错")
	}
}

func TestFillsBackfillPagesToCompletion(t *testing.T) {
	g := newFakeGateway(t, 0)
	all := make([]map[string]any, 12)
	for i := range all {
		all[i] = map[string]any{
			"id": fmt.Sprintf("%d-0-0", i+1), "seq": i + 1, "sub": 0, "idx": 0,
			"market": 0, "price": 70000, "lots": 1, "side": "buy", "role": "taker",
			"order": 100 + i, "counterOrder": 900 + i, "fee": "12", "ts": 1700000000000 + i,
		}
	}
	g.serveFills(all, 5)
	s := newTestSession(t, g)
	got, err := s.Fills(context.Background(), FillQuery{})
	if err != nil || len(got) != 12 {
		t.Fatalf("应当拉全 12 笔,实得 %d 笔 err=%v", len(got), err)
	}
	if got[0].ID != "1-0-0" || got[11].ID != "12-0-0" || got[0].Order != 100 || got[0].Fee == nil || *got[0].Fee != "12" {
		t.Errorf("顺序 / 订单关联 / 费要原样带出:%+v", got[0])
	}
}

func TestFillsResumeFromCursor(t *testing.T) {
	g := newFakeGateway(t, 0)
	all := make([]map[string]any, 6)
	for i := range all {
		all[i] = map[string]any{"id": fmt.Sprintf("%d-0-0", i+1), "seq": i + 1, "sub": 0, "idx": 0}
	}
	g.serveFills(all, 10)
	s := newTestSession(t, g)
	got, err := s.Fills(context.Background(), FillQuery{After: "3-0-0"})
	if err != nil || len(got) != 3 || got[0].ID != "4-0-0" {
		t.Fatalf("续拉必须严格大于断点:%v %+v", err, got)
	}
}

func TestFillsStopsOnNonAdvancingCursor(t *testing.T) {
	g := newFakeGateway(t, 0)
	g.fillsHandler = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"account": 7, "fills": []any{map[string]any{"id": "1-0-0"}}, "next": "1-0-0", "hasMore": true})
	}
	s := newTestSession(t, g)
	if _, err := s.Fills(context.Background(), FillQuery{}); err == nil {
		t.Fatal("游标不前进必须报错,不能无限翻页")
	}
}

func TestEventCarriesFullIdentity(t *testing.T) {
	var ev Event
	if err := json.Unmarshal([]byte(`{"seq":4821,"sub":2,"idx":3,"kind":"Fill","data":{"id":"4821-2-3"}}`), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.ID() != "4821-2-3" {
		t.Errorf("ID() 要与服务端的 id 逐字符相同,实得 %q", ev.ID())
	}
}
