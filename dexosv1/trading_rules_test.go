package dexosv1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func rulesMarketWire() map[string]any {
	return map[string]any{"symbol": "BTC-USD", "signingMarketId": 1, "kind": "perp", "status": "Active", "priceDecimals": 1, "sizeDecimals": 3, "quotePerTickLot": "100", "lastPrice": "80000.1", "tickSize": "1", "subticksPerTick": 10, "tradingRulesVersion": "7", "tickChangePending": false, "aggTicks": []int{10, 100, 1000}}
}

func readRulesMarket(t *testing.T, row map[string]any) (*Market, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/markets" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"markets": []any{row}})
	}))
	t.Cleanup(srv.Close)
	cat, err := NewClient(srv.URL).Catalog(context.Background())
	if err != nil {
		return nil, err
	}
	return cat.MarketByID(1)
}

func TestMarketTradingRulesPreserveEncodingAndAggregation(t *testing.T) {
	m, err := readRulesMarket(t, rulesMarketWire())
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.RequireTradingRules()
	if err != nil {
		t.Fatal(err)
	}
	if r.SubticksPerTick != 10 || r.Version != 7 || r.Pending || r.TickSize != "1" {
		t.Fatalf("rules: %+v", r)
	}
	if m.LastPrice != 800001 || m.Scale.Price(m.LastPrice) != "80000.1" || m.AggTicks[0] != 10 {
		t.Fatalf("price units/aggregation changed: %+v", m)
	}
}

func TestMarketTradingRulesMissingRemainExplicitlyUnknown(t *testing.T) {
	row := rulesMarketWire()
	for _, k := range []string{"subticksPerTick", "tradingRulesVersion", "tickChangePending"} {
		delete(row, k)
	}
	m, err := readRulesMarket(t, row)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.RequireTradingRules(); !errors.Is(err, ErrTradingRulesUnavailable) {
		t.Fatalf("unknown rules: %v", err)
	}
}

func TestMarketTradingRulesRejectPartialOrInconsistentData(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"missing version", func(m map[string]any) { delete(m, "tradingRulesVersion") }},
		{"missing pending", func(m map[string]any) { delete(m, "tickChangePending") }},
		{"missing step", func(m map[string]any) { delete(m, "subticksPerTick") }},
		{"missing tick", func(m map[string]any) { delete(m, "tickSize") }},
		{"null contract", func(m map[string]any) {
			m["subticksPerTick"], m["tradingRulesVersion"], m["tickChangePending"] = nil, nil, nil
		}},
		{"zero step", func(m map[string]any) { m["subticksPerTick"] = 0 }},
		{"wrong tick", func(m map[string]any) { m["tickSize"] = "0.1" }},
		{"version overflow", func(m map[string]any) { m["tradingRulesVersion"] = "18446744073709551616" }},
		{"version missing canonical", func(m map[string]any) { m["tradingRulesVersion"] = "01" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := rulesMarketWire()
			tc.edit(row)
			if _, err := readRulesMarket(t, row); err == nil {
				t.Fatal("accepted inconsistent trading rules")
			}
		})
	}
}
