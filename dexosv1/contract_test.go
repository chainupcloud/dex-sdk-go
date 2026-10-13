//go:build dexos_contract

package dexosv1

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 契约测试:对一台真实 dex-os 跑只读接口 —— 每个接口既用本包解码,也把原始响应对钉住的规格做
// schema 校验。默认不编译,要跑:
//
//	DEXOS_V1_URL=http://127.0.0.1:17807 DEXOS_V1_ADDRESS=0x… go test -tags dexos_contract -run Contract ./dexosv1/
//
// DEXOS_V1_ADDRESS 可选,给了才测账户类接口。只发十几个读请求(翻页 Limit 很小),不调 /v1/events。
func TestContractReadOnly(t *testing.T) {
	base := os.Getenv("DEXOS_V1_URL")
	if base == "" {
		t.Skip("没给 DEXOS_V1_URL")
	}
	spec := loadSpec(t)
	c := NewClient(strings.TrimRight(base, "/"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// conforms 原样取一次响应,对规格里 GET specPath 的 200 schema 校验。
	conforms := func(path string, q url.Values, specPath string) {
		t.Helper()
		status, _, raw, err := c.roundTrip(ctx, path, q, marketRead)
		if err != nil || status != 200 {
			t.Fatalf("GET %s: HTTP %d, %v: %s", path, status, err, raw)
		}
		if errs := validate(spec, responseSchema(t, spec, specPath, "200"), decodeJSON(t, raw), path); len(errs) > 0 {
			t.Errorf("GET %s 的响应不符合钉住的规格:\n  %s", path, strings.Join(errs, "\n  "))
		}
	}

	if _, err := c.Exchange(ctx); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	conforms("/v1/exchange", nil, "/v1/exchange")

	cat, err := c.Catalog(ctx)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(cat.Markets()) == 0 {
		t.Fatal("市场目录为空")
	}
	conforms("/v1/markets", nil, "/v1/markets")
	m := cat.Markets()[0]
	if _, err := c.Book(ctx, &m, 5); err != nil {
		t.Fatalf("Book %s: %v", m.Symbol, err)
	}
	conforms("/v1/markets/"+url.PathEscape(m.Symbol)+"/book", url.Values{"depth": {"5"}}, "/v1/markets/{symbol}/book")

	addr := os.Getenv("DEXOS_V1_ADDRESS")
	if addr == "" {
		t.Log("没给 DEXOS_V1_ADDRESS,跳过账户类接口")
		return
	}
	a, err := dexos.ParseAddress(addr)
	if err != nil {
		t.Fatal(err)
	}
	ref := AccountRef{Address: a}
	q := ref.values()
	if _, err := c.Account(ctx, ref); err != nil {
		t.Fatalf("Account: %v", err)
	}
	conforms("/v1/account", q, "/v1/account")
	if _, err := c.Positions(ctx, cat, ref); err != nil {
		t.Fatalf("Positions: %v", err)
	}
	conforms("/v1/account/positions", q, "/v1/account/positions")
	if _, err := c.Orders(ctx, cat, ref, ""); err != nil {
		t.Fatalf("Orders: %v", err)
	}
	conforms("/v1/account/orders", q, "/v1/account/orders")
	if _, err := c.Fills(ctx, cat, ref, "", HistoryQuery{PageSize: 3, Limit: 3}); err != nil {
		t.Fatalf("Fills: %v", err)
	}
	small := cloneValues(q)
	small.Set("limit", "3")
	conforms("/v1/account/fills", small, "/v1/account/fills")
	if _, err := c.Ledger(ctx, ref, LedgerQuery{HistoryQuery: HistoryQuery{PageSize: 3, Limit: 3}}); err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	conforms("/v1/account/ledger", small, "/v1/account/ledger")
	if _, err := c.Nonce(ctx, ref, nil); err != nil {
		t.Fatalf("Nonce: %v", err)
	}
	conforms("/v1/account/nonce", q, "/v1/account/nonce")
	if _, err := c.Agents(ctx, ref); err != nil {
		t.Fatalf("Agents: %v", err)
	}
	conforms("/v1/account/agents", q, "/v1/account/agents")
}
