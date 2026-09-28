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
