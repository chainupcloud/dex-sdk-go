package dexos

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadCollectionsReturnDecodedValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("只读请求出现 %s", r.Method)
		}
		switch r.URL.Path {
		case "/markets":
			io.WriteString(w, `{"markets":[{"market":7,"symbol":"BTC-USD","priceDecimals":0,"sizeDecimals":3,"quotePerTickLot":1000}]}`)
		case "/agents/42":
			io.WriteString(w, `{"agents":[{"address":"test"}]}`)
		case "/orders/42":
			io.WriteString(w, `{"orders":[{"seq":9}],"seqs":[9]}`)
		case "/account/by-owner/test":
			io.WriteString(w, `{"subaccounts":[42]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, 1)
	ctx := context.Background()
	markets, err := c.Markets(ctx)
	if err != nil || len(markets) != 1 {
		t.Errorf("markets=%v err=%v", markets, err)
	}
	agents, err := c.Agents(ctx, 42)
	if err != nil || len(agents) != 1 {
		t.Errorf("agents=%v err=%v", agents, err)
	}
	orders, err := c.Orders(ctx, 42, 7)
	if err != nil || len(orders) != 1 {
		t.Errorf("orders=%v err=%v", orders, err)
	}
	seqs, err := c.OrderSeqs(ctx, 42, 7)
	if err != nil || len(seqs) != 1 {
		t.Errorf("seqs=%v err=%v", seqs, err)
	}
	accounts, err := c.SubaccountsOf(ctx, "test")
	if err != nil || len(accounts) != 1 {
		t.Errorf("accounts=%v err=%v", accounts, err)
	}
}

func TestReadIdentityAndPrecisionCannotDefaultMissingFields(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/markets", `{"markets":[{"market":7,"symbol":"BTC-USD","sizeDecimals":3,"quotePerTickLot":1000}]}`},
		{"/markets", `{"markets":[{"market":7,"symbol":"BTC-USD","priceDecimals":0,"quotePerTickLot":1000}]}`},
		{"/account/by-address/0x1111111111111111111111111111111111111111", `{"nc":"0"}`},
	} {
		t.Run(tc.body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.body) }))
			defer srv.Close()
			c := NewClient(srv.URL, 1)
			var err error
			if tc.path == "/markets" {
				_, err = c.Markets(context.Background())
			} else {
				_, err = c.AccountByAddress(context.Background(), "0x1111111111111111111111111111111111111111")
			}
			if err == nil {
				t.Fatal("缺身份/精度字段被默认成 0")
			}
		})
	}
}

type safetyTransport func(*http.Request) (*http.Response, error)

func (f safetyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failedBody struct{}

func (failedBody) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failedBody) Close() error               { return nil }

func TestBodyReadFailureIsNeverSuccess(t *testing.T) {
	c := NewClient("http://example.invalid", 1)
	c.HTTP.Transport = safetyTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: failedBody{}, Header: make(http.Header)}, nil
	})
	if err := c.do(context.Background(), http.MethodPost, "/cancel", nil, nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("响应读取失败未暴露: %v", err)
	}
}

func TestInvalidAccountAddressIsNotUnregistered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "bad address", 400) }))
	defer srv.Close()
	_, err := NewClient(srv.URL, 1).AccountByAddress(context.Background(), "bad")
	if err == nil || errors.Is(err, ErrNotRegistered) {
		t.Fatalf("非法地址被当成未登记: %v", err)
	}
}

func TestConnectRejectsIncompleteDiscovery(t *testing.T) {
	for _, replace := range []string{`"name":""`, `"version":""`, `"verifyingContract":"invalid"`} {
		t.Run(replace, func(t *testing.T) {
			cfg := `{"chainId":1,"codecVer":1,"snapshotVer":20,"finalityMode":"head","domain":{"name":"dex-os","version":"1","verifyingContract":"0x0000000000000000000000000000000000000000"}}`
			key := strings.SplitN(replace, ":", 2)[0]
			start := strings.Index(cfg, key+":")
			end := start + len(key) + 2 + strings.Index(cfg[start+len(key)+2:], `"`) + 1
			cfg = cfg[:start] + replace + cfg[end:]
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, cfg) }))
			defer srv.Close()
			if _, _, err := Connect(context.Background(), srv.URL); err == nil {
				t.Fatal("不完整签名域被放行")
			}
		})
	}
}
