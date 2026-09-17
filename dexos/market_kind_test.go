package dexos

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// kind 值集来自 dex-os api.rs@80eb5de719172f3c9fc454babd4d11c0d1a04a0e
// markets(): r.spot.is_some() => "spot"，否则 "perp"。不根据 symbol 猜类型。
func TestMarketsPreserveExplicitKindWithoutGuessing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/markets" {
			t.Errorf("unexpected market request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"markets":[
 {"market":0,"kind":"perp","symbol":"BTC-USD","priceDecimals":0,"sizeDecimals":3,"quotePerTickLot":1000},
 {"market":1,"kind":"spot","symbol":"BTC-USD","priceDecimals":0,"sizeDecimals":3,"quotePerTickLot":1000},
 {"market":2,"kind":"future-kind","quotePerTickLot":1},
 {"market":3,"kind":null,"quotePerTickLot":1},
 {"market":4,"quotePerTickLot":1}]}`))
	}))
	defer server.Close()
	rows, err := NewClient(server.URL, 1).Markets(context.Background())
	if err != nil || len(rows) != 5 {
		t.Fatalf("catalogue=%+v err=%v", rows, err)
	}
	// 再序列化检查 SDK 对外的字段，而非从原始响应直接读取期望；旧版丢 kind 会真红。
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var projected []struct {
		Market        uint16 `json:"market"`
		Kind          string `json:"kind"`
		PriceDecimals *int   `json:"priceDecimals"`
	}
	if err := json.Unmarshal(raw, &projected); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"perp", "spot", "future-kind", "", ""} {
		if projected[i].Market != uint16(i) || projected[i].Kind != want {
			t.Fatalf("SDK 丢失或猜测市场类型: row=%d kind=%q want=%q", i, projected[i].Kind, want)
		}
	}
	if projected[0].PriceDecimals == nil || *projected[0].PriceDecimals != 0 {
		t.Fatal("市场类型改动丢失合法零位精度")
	}
}

func TestMarketsRejectWrongKindType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"markets":[{"market":0,"kind":7,"quotePerTickLot":1}]}`))
	}))
	defer server.Close()
	if _, err := NewClient(server.URL, 1).Markets(context.Background()); err == nil {
		t.Fatal("市场kind类型错误被忽略")
	}
}
