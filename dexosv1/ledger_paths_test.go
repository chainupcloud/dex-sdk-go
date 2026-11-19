package dexosv1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// dex-os#690 去掉了 /v1/security 前缀、没有兼容层:账本读口只能走新的资源路径(假服务对旧路径记意外请求)。
func TestLedgerReadsUseTheResourcePaths(t *testing.T) {
	f, c := newFake(t)
	f.routes["/v1/network"] = serveNetwork("1", strings.Repeat("22", 32))
	f.routes["/v1/time/certified"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"hostLogIndex":"900","manifestHash":"0x%s","time":{"timestampMs":"5000"}}`, strings.Repeat("33", 32))
	}
	f.routes["/v1/nonces"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"floor":"3","high":null,"next":"4"}`)
	}
	f.routes["/v1/grants"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("policy") != "7" {
			t.Errorf("grants 查询参数 %q", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"policyId":"7","issuerEpoch":"1","liveGrants":0,"occupiedGrants":0,"maxGrantsPerIssuer":8,"maxLiveGrantsPerPolicy":4,"grants":[]}`)
	}
	ctx := context.Background()
	if _, err := c.SecurityNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	if tm, err := c.SecurityTime(ctx); err != nil || tm.TimestampMs != 5000 || tm.HostLogIndex != 900 {
		t.Fatalf("认证时间 %+v %v", tm, err)
	}
	if n, err := c.SecurityNonce(ctx, AccountRoot(1, 7), LaneTrade); err != nil || n.Next != 4 {
		t.Fatalf("nonce %+v %v", n, err)
	}
	if g, err := c.SecurityGrants(ctx, 7); err != nil || g.PolicyID != 7 {
		t.Fatalf("grants %+v %v", g, err)
	}
}

// /v1/accounts?member= 分页(dex-os#690 §5):按 next 翻到 null 为止,一页都不能漏。
func TestSecurityAccountsPagesUntilNextIsNull(t *testing.T) {
	f, c := newFake(t)
	member := mustAddr(t, testAgent)
	var afters []string
	f.routes["/v1/accounts"] = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if !strings.EqualFold(q.Get("member"), member.Hex()) {
			t.Errorf("member 参数 %q", q.Get("member"))
		}
		afters = append(afters, q.Get("after"))
		row := func(id, policy int) string {
			return fmt.Sprintf(`{"accountId":%d,"policyId":"%d","policyEpoch":"1","members":[%q],"threshold":1,"soleSigner":true}`, id, policy, member.Hex())
		}
		switch q.Get("after") {
		case "":
			fmt.Fprintf(w, `{"member":%q,"accounts":[%s,%s],"next":"12"}`, member.Hex(), row(1, 11), row(2, 12))
		case "12":
			fmt.Fprintf(w, `{"member":%q,"accounts":[%s],"next":null}`, member.Hex(), row(3, 13))
		default:
			t.Errorf("意外的 after %q", q.Get("after"))
		}
	}
	got, err := c.SecurityAccounts(context.Background(), member)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[2].AccountID != 3 || got[2].PolicyID != 13 {
		t.Fatalf("三行都要读到: %+v", got)
	}
	if strings.Join(afters, ",") != ",12" {
		t.Fatalf("翻页顺序 %q", afters)
	}
}

// 账本市场并进了 /v1/markets(dex-os#690 §4):市场号字段是 signingMarketId;还没有目录名的市场也在账本上,照样列出。
func TestSecurityMarketsReadTheLedgerFieldsOfTheMergedMarkets(t *testing.T) {
	f, c := newFake(t)
	f.routes["/v1/markets"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"manifestHash":"0x`+strings.Repeat("33", 32)+`","markets":[`+
			`{"symbol":"ACME:BTC-USDC","kind":"perp","signingMarketId":0,"dex":1,"marketSpecVersion":"2","admission":1},`+
			`{"symbol":null,"signingMarketId":4,"dex":1,"marketSpecVersion":"1","admission":0}]}`)
	}
	got, err := c.SecurityMarkets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []SecurityMarket{{Dex: 1, Market: 0, SpecVersion: 2, Admission: 1}, {Dex: 1, Market: 4, SpecVersion: 1, Admission: 0}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

// /v1/markets 现在列出账本上每个市场,还没有目录名的 symbol 为 null(dex-os#690 §4):目录跳过它们,不能因此整份读失败。
func TestCatalogSkipsLedgerMarketsWithoutASymbol(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(fixture(t, "markets.json"), &doc); err != nil {
		t.Fatal(err)
	}
	markets := doc["markets"].([]any)
	doc["markets"] = append(markets, map[string]any{"symbol": nil, "signingMarketId": 99, "dex": 1, "marketSpecVersion": "1", "admission": 0, "kind": nil, "status": nil})
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	f, c := newFake(t)
	f.routes["/v1/markets"] = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }
	cat, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatalf("没有名字的账本市场不该让目录读失败: %v", err)
	}
	if len(cat.Markets()) != len(markets) {
		t.Fatalf("目录 %d 个市场,样例 %d 个", len(cat.Markets()), len(markets))
	}
	if _, err := cat.MarketByID(99); err == nil {
		t.Fatal("没有名字的市场不该进目录")
	}
}
