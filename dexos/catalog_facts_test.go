package dexos

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 字段形态来自 dex-os：/config 的 fundingTickSec 是 Option<u64>（读模型未就绪 = null，
// gateway api.rs config()）；/markets 的 minNotional 与 oiCapNotional 同口径，
// 计价币原子单位的十进制字串，"0" = 不设名义额下限。缺失与公布为 0 是两件事。

const catalogFactsConfig = `{"chainId":11155111,"codecVer":1,"snapshotVer":19,"finalityMode":"head",` +
	`"domain":{"name":"dex-os","version":"1","verifyingContract":"0x0000000000000000000000000000000000000000"}%s}`

func discoverWith(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	body := strings.Replace(catalogFactsConfig, "%s", extra, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/config" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	return Discover(context.Background(), srv.URL)
}

func TestDiscoverFundingTickDistinguishesAbsentFromZero(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		want        *uint64
	}{
		{"公布 3600 秒", `,"fundingTickSec":3600`, u64(3600)},
		{"公布 0", `,"fundingTickSec":0`, u64(0)},
		{"缺失即未公布", ``, nil},
		{"null 即未公布", `,"fundingTickSec":null`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := discoverWith(t, tc.extra)
			if err != nil {
				t.Fatal(err)
			}
			if (cfg.FundingTickSec == nil) != (tc.want == nil) ||
				(tc.want != nil && *cfg.FundingTickSec != *tc.want) {
				t.Fatalf("fundingTickSec=%v want %v", cfg.FundingTickSec, tc.want)
			}
		})
	}
}

func TestDiscoverRejectsMalformedFundingTick(t *testing.T) {
	for _, bad := range []string{`-1`, `"3600"`, `1.5`, `true`} {
		t.Run(bad, func(t *testing.T) {
			if _, err := discoverWith(t, `,"fundingTickSec":`+bad); err == nil {
				t.Fatalf("坏的 fundingTickSec %s 被放行", bad)
			}
		})
	}
}

func marketsWith(t *testing.T, field string) ([]Market, error) {
	t.Helper()
	body := `{"markets":[{"market":0,"kind":"perp","symbol":"BTC-USD","status":"Active",` +
		`"priceDecimals":0,"sizeDecimals":3,"quotePerTickLot":1000,"initialMarginPpm":50000` + field + `}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	return NewClient(srv.URL, 1).Markets(context.Background())
}

func TestMarketsMinNotionalDistinguishesAbsentFromZero(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		want        *string
	}{
		{"公布 0 = 无下限", `,"minNotional":"0"`, str("0")},
		{"公布正数", `,"minNotional":"10000000"`, str("10000000")},
		{"超出 u64 的 u128 原值原样保留", `,"minNotional":"340282366920938463463374607431768211455"`,
			str("340282366920938463463374607431768211455")},
		{"缺失即未公布", ``, nil},
		{"null 即未公布", `,"minNotional":null`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			markets, err := marketsWith(t, tc.field)
			if err != nil || len(markets) != 1 {
				t.Fatalf("markets=%+v err=%v", markets, err)
			}
			got := markets[0].MinNotional
			if (got == nil) != (tc.want == nil) || (tc.want != nil && *got != *tc.want) {
				t.Fatalf("minNotional=%v want %v", got, tc.want)
			}
			if markets[0].InitialMarginPpm != 50000 {
				t.Fatalf("解析 minNotional 丢了同行其它字段: %+v", markets[0])
			}
		})
	}
}

func TestMarketsRejectMalformedMinNotional(t *testing.T) {
	for _, bad := range []string{`"-1"`, `"1.5"`, `""`, `" 1"`, `"+1"`, `"1e6"`, `0`} {
		t.Run(bad, func(t *testing.T) {
			if _, err := marketsWith(t, `,"minNotional":`+bad); err == nil {
				t.Fatalf("坏的 minNotional %s 被放行", bad)
			}
		})
	}
}

func u64(v uint64) *uint64 { return &v }

func str(v string) *string { return &v }
