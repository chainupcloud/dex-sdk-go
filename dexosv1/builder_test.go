package dexosv1

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// 按内网地址连接、以公网域名作入口:读、写与 WS 握手的 Host 头都是 Client.Host,不是 BaseURL 的主机名。
func TestClientHostOnEveryRequest(t *testing.T) {
	f, c := newFake(t)
	c.Host = "dex.example"
	f.routes["/v1/builder"] = serve(t, 200, "builder.json")
	f.routes["/v1/orders"] = serve(t, 200, "w_batch_place.json")
	fw := newFakeWS(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.Builder(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.post(ctx, "/v1/orders", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	st, err := NewStream(c, &Catalog{}, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = st.Run(ctx) }()
	select {
	case <-fw.conns:
	case <-ctx.Done():
		t.Fatal("WS 没连上")
	}
	reqs := f.requests()
	if len(reqs) != 3 {
		t.Fatalf("请求 %d 个,要 3 个", len(reqs))
	}
	for _, r := range reqs {
		if r.Host != "dex.example" {
			t.Errorf("%s %s 的 Host 是 %q,要 dex.example", r.Method, r.URL.Path, r.Host)
		}
	}
}

// 没设 Host 时照旧用 BaseURL 的主机名。
func TestClientHostDefaultsToBaseURL(t *testing.T) {
	f, c := newFake(t)
	f.routes["/v1/builder"] = serve(t, 200, "builder.json")
	if _, err := c.Builder(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := f.requests()[0]
	if want := c.BaseURL[len("http://"):]; r.Host != want {
		t.Fatalf("Host 是 %q,要 %q", r.Host, want)
	}
}

func TestBuilder(t *testing.T) {
	f, c := newFake(t)
	f.routes["/v1/builder"] = serve(t, 200, "builder.json")
	b, err := c.Builder(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b.BuilderID != 1 || b.Status != "active" || b.Catalog.Version != 5 || b.AsOfSeq != 12753152 {
		t.Fatalf("builder %+v", b)
	}
	rows := b.Catalog.Markets
	if len(rows) != 3 || rows[0] != (BuilderCatalogRow{Symbol: "demo:BTC-USD", Display: "BTC-USD", Status: "listed"}) ||
		rows[1].Status != "closeOnly" || rows[2].Symbol != "demo:ETH-USDC" {
		t.Fatalf("目录 %+v", rows)
	}
	if b.Fees.Default == nil || *b.Fees.Default != (BuilderFeeRate{TakerRate: "0.0044", MakerRate: "0.0032"}) {
		t.Fatalf("默认费率 %+v", b.Fees.Default)
	}
	if len(b.Fees.Markets) != 1 || b.Fees.Markets[0] != (BuilderMarketFees{Symbol: "demo:ETH-USDC", BuilderFeeRate: BuilderFeeRate{TakerRate: "0.0003", MakerRate: "0.0001"}}) {
		t.Fatalf("按市场费率 %+v", b.Fees.Markets)
	}
	if q := f.requests()[0].URL.RawQuery; q != "" {
		t.Fatalf("按入口域名解析,不带参数;实际 %q", q)
	}
}

func TestBuilderNotFound(t *testing.T) {
	f, c := newFake(t)
	f.routes["/v1/builder"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"GatewayBuilderNotFound","details":{"domain":"127.0.0.1"},"message":"no builder for this request","num":10404}}`))
	}
	_, err := c.Builder(context.Background())
	var e *Error
	if !errors.As(err, &e) || e.Code != "GatewayBuilderNotFound" {
		t.Fatalf("要 GatewayBuilderNotFound,实际 %v", err)
	}
}
