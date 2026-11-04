package dexosv1

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseKeyFileGolden(t *testing.T) {
	b, err := os.ReadFile("testdata/api_wallets_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	f, err := ParseKeyFile(b)
	if err != nil {
		t.Fatalf("ParseKeyFile: %v", err)
	}
	if f.Owner.Hex() != "0x70997970c51812dc3a010c7d01b50e0d17dc79c8" || f.API != "https://dev.example" {
		t.Fatalf("owner / api = %s / %s", f.Owner.Hex(), f.API)
	}
	if len(f.Wallets) != 2 {
		t.Fatalf("wallets = %d", len(f.Wallets))
	}
	w := f.Wallets[1]
	if w.Subaccount != 2 || w.Label != "grid-2" || w.Signer.Address().Hex() != "0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc" {
		t.Fatalf("wallet 2 = %+v", w)
	}
	if !w.ValidUntil.Equal(time.UnixMilli(1793000000000)) || !f.Wallets[0].ValidUntil.IsZero() {
		t.Fatalf("validUntil = %v / %v", w.ValidUntil, f.Wallets[0].ValidUntil)
	}
	ref := w.Account(f.Owner)
	if ref.Address != f.Owner || ref.Subaccount != 2 || ref.Vault != nil {
		t.Fatalf("account ref = %s", ref)
	}
}

func TestParseKeyFileRefusesBadFiles(t *testing.T) {
	b, err := os.ReadFile("testdata/api_wallets_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	golden := string(b)
	cases := map[string]struct{ from, to, want string }{
		"别的文件":       {`"kind": "dex-os-api-wallets"`, `"kind": "something-else"`, "kind"},
		"版本不认识":      {`"version": 1`, `"version": 2`, "version"},
		"私钥与地址对不上":   {"0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC", "0x90F79bf6EB2c4f870365E785982E1f101E93b906", "不是这把私钥"},
		"子账户编号重复":    {`"subaccount": 2`, `"subaccount": 1`, "重复"},
		"子账户编号越界":    {`"subaccount": 2`, `"subaccount": 64`, "0..63"},
		"owner 不是地址": {"0x70997970C51812dc3A010C7d01b50e0d17dc79C8", "0x7099", "owner"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			bad := strings.Replace(golden, c.from, c.to, 1)
			if bad == golden {
				t.Fatalf("替换没生效:%s", c.from)
			}
			if _, err := ParseKeyFile([]byte(bad)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v,想要含 %q", err, c.want)
			}
		})
	}
	if _, err := ParseKeyFile([]byte(strings.Replace(golden, `"wallets": [`, `"wallets": [], "x": [`, 1))); err == nil {
		t.Fatal("一把钥匙都没有也该拒")
	}
}
