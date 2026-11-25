package dexosv1

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSecurityNetworkReportsTheNetworkProfile(t *testing.T) {
	for raw, want := range map[string]NetworkProfile{
		`1`:  ProfileProduction,
		`2`:  ProfileTestnetBacked,
		`3`:  ProfileTestnetSimulated,
		`""`: 0,
	} {
		f, c := newFake(t)
		f.routes[networkPath] = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", "application/json")
			body := map[string]json.RawMessage{
				"networkId":    json.RawMessage(`"0x` + strings.Repeat("11", 32) + `"`),
				"genesisHash":  json.RawMessage(`"0x` + strings.Repeat("22", 32) + `"`),
				"ledgerEpoch":  json.RawMessage(`"1"`),
				"nodeChainId":  json.RawMessage(`"0x` + strings.Repeat("00", 29) + `aa36a7"`),
				"nodeVerifier": json.RawMessage(`"` + testAgent + `"`),
				"manifestHash": json.RawMessage(`"0x` + strings.Repeat("33", 32) + `"`),
			}
			if raw != `""` {
				body["profile"] = json.RawMessage(raw)
			}
			_ = json.NewEncoder(w).Encode(body)
		}
		n, err := c.SecurityNetwork(context.Background())
		if err != nil {
			t.Fatalf("profile %s: %v", raw, err)
		}
		if n.Profile != want {
			t.Fatalf("profile %s read as %d, want %d", raw, n.Profile, want)
		}
	}
}
