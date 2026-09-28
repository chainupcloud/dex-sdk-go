package dexos

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 响应形状取自 dex-os feat/account-fee-rates（fe23391 /book seq、d93fb8f /account/:id 费率、7370ba2 读模型未就绪 503）。
func TestBookCarriesStatePointSeq(t *testing.T) {
	answers := map[string]struct {
		code int
		body string
	}{
		"/book/0": {200, `{"market":0,"oracle":100000,"seq":8812,"bids":[[99900,1000]],"asks":[]}`},
		"/book/1": {200, `{"market":1,"oracle":100000,"bids":[],"asks":[]}`},
		"/book/2": {503, `{"error":"read_model_not_ready"}`},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := answers[r.URL.Path]
		w.WriteHeader(a.code)
		_, _ = w.Write([]byte(a.body))
	}))
	defer server.Close()
	c := NewClient(server.URL, 1)
	book, err := c.Book(context.Background(), 0, 10)
	if err != nil || book.Seq == nil || *book.Seq != 8812 || len(book.Bids) != 1 || len(book.Asks) != 0 {
		t.Fatalf("盘口没有带出状态点序号: %+v %v", book, err)
	}
	if old, err := c.Book(context.Background(), 1, 10); err != nil || old.Seq != nil {
		t.Fatalf("不带 seq 的旧实例应当得到 nil 而不是 0: %+v %v", old, err)
	}
	var api *APIError
	if _, err := c.Book(context.Background(), 2, 10); !errors.As(err, &api) || api.Status != http.StatusServiceUnavailable {
		t.Fatalf("读模型未就绪必须是错误，不能当空盘口: %v", err)
	}
}

func TestAccountReadsEffectiveFeeRates(t *testing.T) {
	answers := map[string]string{
		"/account/7": `{"accountId":7,"exists":true,"collateral":"250000000","balances":[],"nc":"250000000","imr":"0","withdrawable":"250000000","feeTier":2,"takerFeePpm":350,"makerFeePpm":-20}`,
		"/account/8": `{"accountId":8,"exists":true,"collateral":"0","balances":[],"nc":"0","imr":"0","withdrawable":"0","feeTier":null,"takerFeePpm":500,"makerFeePpm":0}`,
		"/account/9": `{"accountId":9,"exists":false,"collateral":"0","balances":[],"nc":"0","imr":"0","withdrawable":"0","feeTier":null,"takerFeePpm":null,"makerFeePpm":null}`,
		"/account/5": `{"accountId":6,"exists":true,"collateral":"0","balances":[],"nc":"0","imr":"0","withdrawable":"0","feeTier":null,"takerFeePpm":500,"makerFeePpm":0}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(answers[r.URL.Path]))
	}))
	defer server.Close()
	c := NewClient(server.URL, 1)
	ctx := context.Background()
	a, err := c.Account(ctx, 7)
	if err != nil || !a.Exists || a.FeeTier == nil || *a.FeeTier != 2 || a.TakerFeePpm == nil || *a.TakerFeePpm != 350 ||
		a.MakerFeePpm == nil || *a.MakerFeePpm != -20 || a.Collateral != "250000000" {
		t.Fatalf("账户费率没有按原样带出: %+v %v", a, err)
	}
	if base, err := c.Account(ctx, 8); err != nil || base.FeeTier != nil || base.MakerFeePpm == nil || *base.MakerFeePpm != 0 {
		t.Fatalf("基准费率账户：档位为空、费率照常给出，0 不能丢成 nil: %+v %v", base, err)
	}
	if missing, err := c.Account(ctx, 9); err != nil || missing.Exists || missing.TakerFeePpm != nil || missing.MakerFeePpm != nil {
		t.Fatalf("账户不存在时费率必须为空，不能当 0: %+v %v", missing, err)
	}
	if _, err := c.Account(ctx, 5); err == nil {
		t.Fatal("响应的账户号与请求不一致必须报错")
	}
}

// 响应形状取自 dex-os 0694ac7（PR #10，/account/:id?market=N 的 marketFees）。
func TestAccountInMarketReadsBilledRates(t *testing.T) {
	const view = `"collateral":"250000000","balances":[],"nc":"250000000","imr":"0","withdrawable":"250000000","feeTier":2,"takerFeePpm":350,"makerFeePpm":-20`
	const fees = `"rateSource":"market","protocolTakerPpm":600,"protocolMakerPpm":-15,"dexFeeScalePpm":500000,"deployerPpm":40,"builderPpm":25,"takerTotalPpm":940,"takerTotalWithBuilderPpm":965,"makerTotalPpm":-15`
	answers := map[string]struct {
		code int
		body string
	}{
		"/account/7?market=0": {200, `{"accountId":7,"exists":true,` + view + `,"marketFees":{"market":0,` + fees + `}}`},
		"/account/9?market=0": {200, `{"accountId":9,"exists":false,"collateral":"0","balances":[],"nc":"0","imr":"0","withdrawable":"0","feeTier":null,"takerFeePpm":null,"makerFeePpm":null,"marketFees":null}`},
		"/account/7?market=5": {404, `{"error":"unknown market"}`},
		"/account/7?market=6": {503, `{"error":"read_model_not_ready"}`},
		"/account/1?market=0": {200, `{"accountId":1,"exists":true,` + view + `,"marketFees":null}`},
		"/account/2?market=0": {200, `{"accountId":2,"exists":true,` + view + `}`},
		"/account/3?market=0": {200, `{"accountId":3,"exists":true,` + view + `,"marketFees":{"market":0,"rateSource":"market","protocolTakerPpm":600,"protocolMakerPpm":-15,"dexFeeScalePpm":0,"deployerPpm":0,"builderPpm":0,"takerTotalPpm":600,"takerTotalWithBuilderPpm":600}}`},
		"/account/4?market=0": {200, `{"accountId":4,"exists":true,` + view + `,"marketFees":{"market":1,` + fees + `}}`},
		"/account/5?market=0": {200, `{"accountId":5,"exists":true,` + view + `,"marketFees":{"market":0,"rateSource":"vip",` + fees[len(`"rateSource":"market",`):] + `}}`},
		"/account/8?market=0": {200, `{"accountId":6,"exists":true,` + view + `,"marketFees":{"market":0,` + fees + `}}`},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := answers[r.URL.Path+"?"+r.URL.RawQuery]
		if !ok {
			t.Errorf("意外请求 %s", r.URL)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(a.code)
		_, _ = w.Write([]byte(a.body))
	}))
	defer server.Close()
	c := NewClient(server.URL, 1)
	ctx := context.Background()

	a, f, err := c.AccountInMarket(ctx, 7, 0)
	want := MarketFees{Market: 0, RateSource: "market", ProtocolTakerPpm: 600, ProtocolMakerPpm: -15, DexFeeScalePpm: 500000,
		DeployerPpm: 40, BuilderPpm: 25, TakerTotalPpm: 940, TakerTotalWithBuilderPpm: 965, MakerTotalPpm: -15}
	if err != nil || f == nil || *f != want || !a.Exists || a.Collateral != "250000000" || a.MakerFeePpm == nil || *a.MakerFeePpm != -20 {
		t.Fatalf("市场实收费率与同一代账户视图没有按原样带出: %+v %+v %v", a, f, err)
	}
	if missing, f, err := c.AccountInMarket(ctx, 9, 0); err != nil || missing.Exists || f != nil {
		t.Fatalf("账户不存在时市场费率必须为空: %+v %+v %v", missing, f, err)
	}
	var api *APIError
	if _, _, err := c.AccountInMarket(ctx, 7, 5); !errors.As(err, &api) || api.Status != http.StatusNotFound {
		t.Fatalf("市场不存在必须是错误: %v", err)
	}
	if _, _, err := c.AccountInMarket(ctx, 7, 6); !errors.As(err, &api) || api.Status != http.StatusServiceUnavailable {
		t.Fatalf("读模型未就绪必须是错误: %v", err)
	}
	for _, bad := range []struct {
		account uint32
		why     string
	}{
		{1, "账户存在而市场费率为 null"},
		{2, "账户存在而没有 marketFees（旧实例）"},
		{3, "缺 makerTotalPpm 不能当 0"},
		{4, "费率的市场号与请求不符"},
		{5, "费率来源不是 market/tier"},
		{8, "账户号与请求不符"},
	} {
		if _, f, err := c.AccountInMarket(ctx, bad.account, 0); err == nil {
			t.Fatalf("%s，必须报错，却得到 %+v", bad.why, f)
		}
	}
}
