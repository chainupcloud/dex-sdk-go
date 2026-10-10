package dexos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// 这组测试钉住执行历史读接口(/fills /ledger /events)与 dex-os #1/#2/#5 的契约
// (crates/gateway/src/api.rs 的 fills / account_ledger / events_backfill / history_error):
//
//   - 续页与从游标续拉都必须带回第一页的 epoch + upper,服务端缺一即 400;
//   - 纪元不符是 409 → ErrHistoryEpochMismatch;与缺口相交是 503 → *HistoryUnavailableError(带缺口);
//   - 结果带回 epoch/upper,调用方据此知道「补到了哪」。

// historyFake 按网关同一套规则服务一个历史接口:严格大于 after、不超过 upper、多取一条判 hasMore。
type historyFake struct {
	mu       sync.Mutex
	epoch    string
	complete uint64
	field    string
	rows     []map[string]any
	gaps     []Gap
	requests []url.Values
}

func parseTriple(s string) ([3]uint64, bool) {
	parts := strings.Split(s, "-")
	if len(parts) != 3 {
		return [3]uint64{}, false
	}
	var out [3]uint64
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return [3]uint64{}, false
		}
		out[i] = v
	}
	return out, true
}

func tripleLess(a, b [3]uint64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func (f *historyFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	f.requests = append(f.requests, q)
	epoch, complete, rows, gaps := f.epoch, f.complete, f.rows, f.gaps
	f.mu.Unlock()
	fail := func(status int, body map[string]any) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		writeJSON(w, body)
	}
	if e := q.Get("epoch"); e != "" && e != epoch {
		fail(http.StatusConflict, map[string]any{"error": "history_epoch_mismatch"})
		return
	}
	if q.Has("after") && (!q.Has("epoch") || !q.Has("upper")) {
		fail(http.StatusBadRequest, map[string]any{"error": "resume requires epoch and upper from the first page"})
		return
	}
	var after [3]uint64
	hasAfter := false
	if a := q.Get("after"); a != "" {
		v, ok := parseTriple(a)
		if !ok {
			fail(http.StatusBadRequest, map[string]any{"error": "bad after cursor"})
			return
		}
		after, hasAfter = v, true
	}
	upper := [3]uint64{complete, 65535, 4294967295}
	if u := q.Get("upper"); u != "" {
		v, ok := parseTriple(u)
		if !ok {
			fail(http.StatusBadRequest, map[string]any{"error": "bad upper cursor"})
			return
		}
		upper = v
	}
	// 与网关 history_error 同一条:上界越过本副本的完整前缀 → 400
	if upper[0] > complete {
		fail(http.StatusBadRequest, map[string]any{"error": "history: requested upper is not yet complete"})
		return
	}
	for _, g := range gaps {
		lo := uint64(0)
		if hasAfter {
			lo = after[0]
		}
		if g.Last >= lo && g.First <= upper[0] {
			fail(http.StatusServiceUnavailable, map[string]any{
				"error": "history_unavailable: requested interval intersects missing history", "status": "history_unavailable",
				"gaps": []map[string]any{{"first": g.First, "last": g.Last}},
			})
			return
		}
	}
	limit := 200
	if l := q.Get("limit"); l != "" {
		limit, _ = strconv.Atoi(l)
	}
	var page []map[string]any
	for _, row := range rows {
		id, _ := parseTriple(row["id"].(string))
		if (hasAfter && !tripleLess(after, id)) || tripleLess(upper, id) {
			continue
		}
		page = append(page, row)
	}
	hasMore := len(page) > limit
	if hasMore {
		page = page[:limit]
	}
	var next any
	if len(page) > 0 {
		next = page[len(page)-1]["id"]
	}
	body := map[string]any{
		"epoch": epoch, "upper": fmt.Sprintf("%d-%d-%d", upper[0], upper[1], upper[2]),
		"next": next, "hasMore": hasMore, "finality": "raft_committed", f.field: page,
	}
	if f.field == "events" {
		body["completeSeq"] = complete
	}
	writeJSON(w, body)
}

func (f *historyFake) seen() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.requests...)
}

func newHistoryClient(t *testing.T, path string, f *historyFake) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(path, f)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, 31337)
}

func fillRows(n int) []map[string]any {
	rows := make([]map[string]any, n)
	for i := range rows {
		rows[i] = map[string]any{
			"id": fmt.Sprintf("%d-0-0", i+1), "seq": i + 1, "sub": 0, "idx": 0, "market": 0,
			"price": 100, "lots": 1, "side": "buy", "role": "maker", "order": 40 + i, "counterOrder": 90, "fee": "-5", "ts": 1700000000000 + i,
		}
	}
	return rows
}

// 翻页必须把第一页的 epoch + upper 带到每一页:只带游标时服务端直接 400,
// 而旧实现恰恰只带游标 —— 第二页起永远拿不到。
func TestFillsCarryEpochAndUpperAcrossPages(t *testing.T) {
	f := &historyFake{epoch: "k1-1", complete: 50, field: "fills", rows: fillRows(7)}
	c := newHistoryClient(t, "/fills", f)
	got, err := c.Fills(context.Background(), 7, FillQuery{PageSize: 2})
	if err != nil {
		t.Fatalf("翻页补拉失败: %v", err)
	}
	if len(got.Fills) != 7 || got.Fills[6].ID != "7-0-0" {
		t.Fatalf("应当翻到底取回 7 笔,实得 %d", len(got.Fills))
	}
	if got.Epoch != "k1-1" || got.Upper != "50-65535-4294967295" {
		t.Fatalf("结果要带回第一页的纪元与上界,实得 %q %q", got.Epoch, got.Upper)
	}
	reqs := f.seen()
	if len(reqs) != 4 {
		t.Fatalf("7 笔每页 2 笔应当 4 次请求,实得 %d", len(reqs))
	}
	for i, q := range reqs[1:] {
		if q.Get("epoch") != "k1-1" || q.Get("upper") != "50-65535-4294967295" || q.Get("after") == "" {
			t.Fatalf("第 %d 页没有带回第一页的 epoch/upper/after: %v", i+2, q)
		}
	}
}

// 从已存游标续拉:游标属于哪个纪元只有调用方知道,缺了就在本地拒绝、一个请求都不发。
func TestFillsResumeRequiresTheCursorsEpoch(t *testing.T) {
	f := &historyFake{epoch: "k1-1", complete: 50, field: "fills", rows: fillRows(3)}
	c := newHistoryClient(t, "/fills", f)
	if _, err := c.Fills(context.Background(), 7, FillQuery{After: "1-0-0"}); err == nil {
		t.Fatal("续拉不带纪元应当报错")
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("本地就能判定的错误不该发请求,实发 %d", n)
	}
}

// 带纪元续拉:先以该纪元探一页拿当前上界,再从游标续到那个上界。
func TestFillsResumeProbesUpperThenContinues(t *testing.T) {
	f := &historyFake{epoch: "k1-1", complete: 50, field: "fills", rows: fillRows(6)}
	c := newHistoryClient(t, "/fills", f)
	got, err := c.Fills(context.Background(), 7, FillQuery{After: "3-0-0", Epoch: "k1-1"})
	if err != nil {
		t.Fatalf("续拉失败: %v", err)
	}
	if len(got.Fills) != 3 || got.Fills[0].ID != "4-0-0" {
		t.Fatalf("应当只取游标之后的 3 笔,实得 %+v", got.Fills)
	}
	reqs := f.seen()
	if len(reqs) != 2 || reqs[0].Has("after") || reqs[0].Get("epoch") != "k1-1" || reqs[1].Get("after") != "3-0-0" {
		t.Fatalf("应当先探上界再续拉,实际请求 %v", reqs)
	}
}

// 调用方自己给了上界(取自 /risk 的 asOfSeq 或 /book 的 seq):游标、纪元、上界一次带齐,
// 不发不带游标的探测页 —— 那一页会从开户位置查缺口,开户被裁出保留窗口后永远 503(dex-os #28)。
func TestFillsResumeWithExplicitUpperSkipsProbe(t *testing.T) {
	f := &historyFake{epoch: "k1-1", complete: 50, field: "fills", rows: fillRows(6), gaps: []Gap{{First: 1, Last: 2}}}
	c := newHistoryClient(t, "/fills", f)
	got, err := c.Fills(context.Background(), 7, FillQuery{After: "3-0-0", Epoch: "k1-1", Upper: "5-65535-4294967295", PageSize: 1})
	if err != nil {
		t.Fatalf("带显式上界续拉失败(缺口在游标之前,不该相交): %v", err)
	}
	if len(got.Fills) != 2 || got.Fills[0].ID != "4-0-0" || got.Fills[1].ID != "5-0-0" {
		t.Fatalf("应当只取游标之后、上界之内的 2 笔,实得 %+v", got.Fills)
	}
	if got.Epoch != "k1-1" || got.Upper != "5-65535-4294967295" {
		t.Fatalf("结果要带回给定的纪元与上界,实得 %q %q", got.Epoch, got.Upper)
	}
	for i, q := range f.seen() {
		if !q.Has("after") || q.Get("epoch") != "k1-1" || q.Get("upper") != "5-65535-4294967295" {
			t.Fatalf("第 %d 个请求没有带齐 after/epoch/upper(不能有不带游标的探测): %v", i+1, q)
		}
	}
}

// 不带游标、只给上界:首页就带上它,结果固定在这个上界,纪元由服务端给出。
func TestFillsFirstPageCarriesExplicitUpper(t *testing.T) {
	f := &historyFake{epoch: "k1-1", complete: 50, field: "fills", rows: fillRows(6)}
	c := newHistoryClient(t, "/fills", f)
	got, err := c.Fills(context.Background(), 7, FillQuery{Upper: "4-65535-4294967295", PageSize: 3})
	if err != nil {
		t.Fatalf("带上界的首页失败: %v", err)
	}
	if len(got.Fills) != 4 || got.Epoch != "k1-1" || got.Upper != "4-65535-4294967295" {
		t.Fatalf("应当翻到给定上界为止的 4 笔并带回纪元,实得 %+v", got)
	}
	reqs := f.seen()
	if len(reqs) != 2 || reqs[0].Has("after") || reqs[0].Get("upper") != "4-65535-4294967295" {
		t.Fatalf("首页应当不带游标、带上界,实际请求 %v", reqs)
	}
}

// 实时视图比执行历史超前时,给定的上界可能还不完整(网关 400):必须可判别,调用方按暂不可用重试。
func TestHistoryUpperNotYetCompleteIsTyped(t *testing.T) {
	f := &historyFake{epoch: "k1-1", complete: 50, field: "fills", rows: fillRows(3)}
	c := newHistoryClient(t, "/fills", f)
	_, err := c.Fills(context.Background(), 7, FillQuery{After: "1-0-0", Epoch: "k1-1", Upper: "60-65535-4294967295"})
	if !errors.Is(err, ErrHistoryUpperIncomplete) {
		t.Fatalf("上界还不完整应当是 ErrHistoryUpperIncomplete,实得 %T %v", err, err)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
		t.Fatalf("仍应能取到 400 的 *APIError,实得 %v", err)
	}
	if n := len(f.seen()); n != 1 {
		t.Fatalf("给了上界就不该有探测页,实发 %d 个请求", n)
	}
}

// 只读外部资金流水(入金、出金、转账、金库):带 external=true,游标、纪元、上界一次带齐。
func TestLedgerExternalWithExplicitUpper(t *testing.T) {
	rows := []map[string]any{
		{"id": "3-0-0", "seq": 3, "sub": 0, "idx": 0, "kind": "deposit", "token": 0, "amount": "5000000000", "market": nil, "counterparty": nil, "order": nil, "counterOrder": nil, "reference": nil, "ts": 1},
		{"id": "12-0-0", "seq": 12, "sub": 0, "idx": 0, "kind": "send_asset", "token": 0, "amount": "-100", "market": nil, "counterparty": nil, "order": nil, "counterOrder": nil, "reference": "77", "ts": 3},
	}
	f := &historyFake{epoch: "k1-1", complete: 20, field: "entries", rows: rows}
	c := newHistoryClient(t, "/ledger", f)
	got, err := c.Ledger(context.Background(), 7, LedgerQuery{After: "2-65535-4294967295", Epoch: "k1-1", Upper: "15-65535-4294967295", External: true})
	if err != nil {
		t.Fatalf("外部资金流水增量读失败: %v", err)
	}
	if len(got.Entries) != 2 || got.Upper != "15-65535-4294967295" || got.Epoch != "k1-1" {
		t.Fatalf("应当取回游标之后、上界之内的 2 条并带回纪元与上界,实得 %+v", got)
	}
	reqs := f.seen()
	if len(reqs) != 1 || reqs[0].Get("external") != "true" || reqs[0].Get("after") == "" || reqs[0].Get("upper") != "15-65535-4294967295" || reqs[0].Has("kind") {
		t.Fatalf("应当一次带齐 external/after/epoch/upper,实际请求 %v", reqs)
	}
}

// external 与 kind 是两种互斥的筛选(网关同时给即 400):本地就拒绝,一个请求都不发。
func TestLedgerExternalWithKindRejectedLocally(t *testing.T) {
	f := &historyFake{epoch: "k1-1", complete: 20, field: "entries"}
	c := newHistoryClient(t, "/ledger", f)
	if _, err := c.Ledger(context.Background(), 7, LedgerQuery{External: true, Kind: "deposit"}); err == nil {
		t.Fatal("external 与 kind 同时给应当报错")
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("本地就能判定的错误不该发请求,实发 %d", n)
	}
}

// 纪元变了(账本换纪元)旧游标整体作废:必须是可判别的错误,不能当成「没有新成交」。
func TestFillsEpochMismatchIsTyped(t *testing.T) {
	f := &historyFake{epoch: "k2-1", complete: 50, field: "fills", rows: fillRows(3)}
	c := newHistoryClient(t, "/fills", f)
	_, err := c.Fills(context.Background(), 7, FillQuery{After: "1-0-0", Epoch: "k1-1"})
	if !errors.Is(err, ErrHistoryEpochMismatch) {
		t.Fatalf("纪元不符应当是 ErrHistoryEpochMismatch,实得 %T %v", err, err)
	}
}

// 与缺口相交:带出缺口区间,仍能按 *APIError 取状态码。
func TestHistoryGapIsTypedWithGaps(t *testing.T) {
	f := &historyFake{epoch: "k1-1", complete: 50, field: "fills", rows: fillRows(3), gaps: []Gap{{First: 1, Last: 20}}}
	c := newHistoryClient(t, "/fills", f)
	_, err := c.Fills(context.Background(), 7, FillQuery{})
	var hu *HistoryUnavailableError
	if !errors.As(err, &hu) || len(hu.Gaps) != 1 || hu.Gaps[0] != (Gap{First: 1, Last: 20}) {
		t.Fatalf("缺口应当是带区间的 *HistoryUnavailableError,实得 %T %v", err, err)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable {
		t.Fatalf("仍应能取到 503 的 *APIError,实得 %v", err)
	}
}

// ts 为 null 表示服务端不知道成交时刻,不能变成 0(1970 年)。
func TestFillTimestampNullStaysUnknown(t *testing.T) {
	rows := fillRows(1)
	rows[0]["ts"] = nil
	f := &historyFake{epoch: "k1-1", complete: 5, field: "fills", rows: rows}
	c := newHistoryClient(t, "/fills", f)
	got, err := c.Fills(context.Background(), 7, FillQuery{})
	if err != nil || len(got.Fills) != 1 || got.Fills[0].TS != nil {
		t.Fatalf("null 时刻应当保持未知,实得 %+v %v", got, err)
	}
}

// 页间纪元或上界漂移 = 服务端异常,不能把两段不同的历史拼成一份。
func TestFillsRejectDriftingPageBounds(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/fills", func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(w, map[string]any{
			"epoch": "k1-1", "upper": fmt.Sprintf("%d-65535-4294967295", 10+calls), "hasMore": calls < 3,
			"next": fmt.Sprintf("%d-0-0", calls), "fills": []any{fillRows(3)[calls-1]},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if _, err := NewClient(srv.URL, 31337).Fills(context.Background(), 7, FillQuery{}); err == nil {
		t.Fatal("上界在页间变化应当报错")
	}
}

func TestLedgerPagesAndKeepsOptionalFields(t *testing.T) {
	rows := []map[string]any{
		{"id": "3-0-0", "seq": 3, "sub": 0, "idx": 0, "kind": "deposit", "token": 0, "amount": "5000000000", "market": nil, "counterparty": nil, "order": nil, "counterOrder": nil, "reference": nil, "ts": 1},
		{"id": "9-0-1", "seq": 9, "sub": 0, "idx": 1, "kind": "maker_rebate", "token": 0, "amount": "12", "market": 0, "counterparty": 1, "order": 41, "counterOrder": 90, "reference": nil, "ts": 2},
		{"id": "12-0-0", "seq": 12, "sub": 0, "idx": 0, "kind": "send_asset", "token": 0, "amount": "-100", "market": nil, "counterparty": nil, "order": nil, "counterOrder": nil, "reference": "77", "ts": 3, "outboxStatus": "pending"},
	}
	f := &historyFake{epoch: "k1-1", complete: 20, field: "entries", rows: rows}
	c := newHistoryClient(t, "/ledger", f)
	token := uint32(0)
	got, err := c.Ledger(context.Background(), 7, LedgerQuery{Token: &token, PageSize: 1})
	if err != nil {
		t.Fatalf("流水翻页失败: %v", err)
	}
	if len(got.Entries) != 3 || got.Epoch != "k1-1" || got.Upper != "20-65535-4294967295" {
		t.Fatalf("应当翻到底并带回纪元与上界,实得 %+v", got)
	}
	e := got.Entries[1]
	if e.Kind != "maker_rebate" || e.Amount != "12" || e.Market == nil || *e.Market != 0 || e.Order == nil || *e.Order != 41 {
		t.Fatalf("返佣腿字段丢失: %+v", e)
	}
	if got.Entries[0].Market != nil || got.Entries[2].OutboxStatus == nil || *got.Entries[2].OutboxStatus != "pending" || *got.Entries[2].Reference != "77" {
		t.Fatalf("可空字段要区分 null 与值: %+v %+v", got.Entries[0], got.Entries[2])
	}
	if q := f.seen()[0]; q.Get("token") != "0" || q.Get("account") != "7" {
		t.Fatalf("查询条件没有带到服务端: %v", q)
	}
}

func TestEventsBackfillPages(t *testing.T) {
	rows := []map[string]any{
		{"id": "5-0-0", "seq": 5, "sub": 0, "idx": 0, "kind": "OrderAccepted", "data": map[string]any{"account": 7}},
		{"id": "5-0-1", "seq": 5, "sub": 0, "idx": 1, "kind": "Fill", "data": map[string]any{"id": "5-0-1"}},
	}
	f := &historyFake{epoch: "k1-1", complete: 9, field: "events", rows: rows}
	c := newHistoryClient(t, "/events", f)
	got, err := c.Events(context.Background(), EventQuery{PageSize: 1})
	if err != nil {
		t.Fatalf("事件补拉失败: %v", err)
	}
	if len(got.Events) != 2 || got.Events[1].ID() != "5-0-1" || got.CompleteSeq != 9 || got.Upper != "9-65535-4294967295" {
		t.Fatalf("事件补拉结果不对: %+v", got)
	}
}

// Limit 截断时必须标 Truncated:Upper 仍是整轮的上界,不标就会被当成「已补齐到 Upper」。
func TestFillsLimitTruncationIsMarked(t *testing.T) {
	f := &historyFake{epoch: "k1-1", complete: 50, field: "fills", rows: fillRows(5)}
	c := newHistoryClient(t, "/fills", f)
	got, err := c.Fills(context.Background(), 7, FillQuery{PageSize: 2, Limit: 3})
	if err != nil || len(got.Fills) != 3 || !got.Truncated {
		t.Fatalf("截断的结果没有标 Truncated: %+v %v", got, err)
	}
	for _, limit := range []int{5, 9} {
		got, err = c.Fills(context.Background(), 7, FillQuery{PageSize: 2, Limit: limit})
		if err != nil || len(got.Fills) != 5 || got.Truncated {
			t.Fatalf("Limit=%d 已翻到底,不算截断: %+v %v", limit, got, err)
		}
	}
}

// 真节点实录的一页成交(testdata/fills_page_live.json)能被完整解析,且游标/纪元/上界原样带回。
func TestFillsParseLiveSample(t *testing.T) {
	raw, err := os.ReadFile("testdata/fills_page_live.json")
	if err != nil {
		t.Fatal(err)
	}
	var sample struct {
		Account uint32
		Epoch   string
		Upper   string
	}
	_ = json.Unmarshal(raw, &sample)
	c, _ := serveOnce(t, "/fills", 200, string(raw))
	got, err := c.Fills(context.Background(), sample.Account, FillQuery{Limit: 2})
	if err != nil || len(got.Fills) != 2 || !got.Truncated || got.Epoch != sample.Epoch || got.Upper != sample.Upper || got.Fills[0].Fee == nil || got.Fills[0].Order == 0 || got.Fills[0].Role != "maker" {
		t.Fatalf("实录成交页解析不对: %+v %v", got, err)
	}
}
