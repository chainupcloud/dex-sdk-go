package dexos

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

const testManager = "0x3333333333333333333333333333333333333333"

// vaultRow 一行与网关 vault_json 同形的金库视图(主账户 30,manager 地址的主账户是 12);edit 改字段,
// 删键用 delete。
func vaultRow(id uint16, edit func(map[string]any)) string {
	m := map[string]any{
		"vault": id, "name": "做市金库", "mainAccount": 30, "manager": testManager, "hasManager": true,
		"tradingHalt": false, "creator": testManager,
		"whitelist": nil, "blacklist": []any{}, "maxLeverages": []any{}, "depositorWhitelist": []any{testManager},
		"config": map[string]any{"profitSharePpm": 0, "managerMinPortionPpm": nil, "managerLiqPortionPpm": nil},
	}
	if edit != nil {
		edit(m)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestVaultCarriesAdmissionFields(t *testing.T) {
	c, seen := serveOnce(t, "/vaults/3", 200, vaultRow(3, func(m map[string]any) {
		m["tradingHalt"] = true
		m["whitelist"] = []any{map[string]any{"market": 0, "symbol": "BTC-USD"}, map[string]any{"market": 1, "symbol": "ETH-USD"}}
		m["blacklist"] = []any{map[string]any{"market": 2, "symbol": nil}}
		m["maxLeverages"] = []any{map[string]any{"market": 0, "symbol": "BTC-USD", "maxLeverage": 5}}
		m["config"] = map[string]any{"managerLiqPortionPpm": 100000}
	}))
	v, err := c.Vault(context.Background(), 3)
	if err != nil {
		t.Fatalf("读金库失败: %v", err)
	}
	if (*seen)[0] != "/vaults/3" {
		t.Fatalf("请求路径不对: %v", *seen)
	}
	if v.Vault != 3 || v.Name != "做市金库" || v.MainAccount != 30 || v.Manager == nil || v.Manager.Hex() != testManager || !v.TradingHalt {
		t.Fatalf("身份字段不对: %+v", v)
	}
	if !v.HasWhitelist || !slices.Equal(v.Whitelist, []uint16{0, 1}) || !slices.Equal(v.Blacklist, []uint16{2}) {
		t.Fatalf("市场名单不对: has=%v white=%v black=%v", v.HasWhitelist, v.Whitelist, v.Blacklist)
	}
	if len(v.MaxLeverages) != 1 || v.MaxLeverages[0] != 5 {
		t.Fatalf("杠杆上限不对: %v", v.MaxLeverages)
	}
	if len(v.DepositorWhitelist) != 1 || v.DepositorWhitelist[0].Hex() != testManager {
		t.Fatalf("出资人白名单不对: %v", v.DepositorWhitelist)
	}
	if v.ManagerLiqPortionPpm == nil || *v.ManagerLiqPortionPpm != 100000 {
		t.Fatalf("熔断线不对: %v", v.ManagerLiqPortionPpm)
	}
}

// 白名单 null 是「不限市场」;白名单删空后是 [],意思是「哪个市场都不许」—— 两者不能混成一个空切片。
func TestVaultWhitelistUnsetVersusEmpty(t *testing.T) {
	for _, tc := range []struct {
		name      string
		whitelist any
		has       bool
	}{{"null 不限", nil, false}, {"[] 全禁", []any{}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := serveOnce(t, "/vaults/3", 200, vaultRow(3, func(m map[string]any) { m["whitelist"] = tc.whitelist }))
			v, err := c.Vault(context.Background(), 3)
			if err != nil || v.HasWhitelist != tc.has || len(v.Whitelist) != 0 {
				t.Fatalf("要 HasWhitelist=%v 且名单为空,得到 %+v %v", tc.has, v, err)
			}
			if v.ManagerLiqPortionPpm != nil || len(v.MaxLeverages) != 0 || len(v.Blacklist) != 0 {
				t.Fatalf("没设的限制应当为空: %+v", v)
			}
		})
	}
}

// 字段缺失或自相矛盾时不能猜:主账户缺失会把单签到账户 0 上,白名单缺失补成「不限」就放过了限制。
func TestVaultRejectsMissingOrInconsistentFields(t *testing.T) {
	cases := map[string]func(map[string]any){
		"金库号不符":            func(m map[string]any) { m["vault"] = 4 },
		"有 manager 却没地址":   func(m map[string]any) { m["manager"] = nil },
		"无 manager 却有地址":   func(m map[string]any) { m["hasManager"] = false },
		"manager 地址非法":     func(m map[string]any) { m["manager"] = "0x12" },
		"blacklist 为 null": func(m map[string]any) { m["blacklist"] = nil },
		"whitelist 项缺市场":   func(m map[string]any) { m["whitelist"] = []any{map[string]any{"symbol": "BTC-USD"}} },
		"blacklist 项缺市场":   func(m map[string]any) { m["blacklist"] = []any{map[string]any{"symbol": "BTC-USD"}} },
		"杠杆上限缺倍数":          func(m map[string]any) { m["maxLeverages"] = []any{map[string]any{"market": 0}} },
		"杠杆上限市场重复": func(m map[string]any) {
			m["maxLeverages"] = []any{map[string]any{"market": 0, "maxLeverage": 5}, map[string]any{"market": 0, "maxLeverage": 3}}
		},
		"出资人地址非法":                   func(m map[string]any) { m["depositorWhitelist"] = []any{"0x12"} },
		"熔断线不是非负整数":                 func(m map[string]any) { m["config"] = map[string]any{"managerLiqPortionPpm": -1} },
		"缺 config.熔断线":              func(m map[string]any) { m["config"] = map[string]any{"profitSharePpm": 0} },
		"config 为 null":             func(m map[string]any) { m["config"] = nil },
		"depositorWhitelist 为 null": func(m map[string]any) { m["depositorWhitelist"] = nil },
	}
	for _, k := range []string{"vault", "name", "mainAccount", "manager", "hasManager", "tradingHalt",
		"whitelist", "blacklist", "maxLeverages", "depositorWhitelist", "config"} {
		cases["缺 "+k] = func(m map[string]any) { delete(m, k) }
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := serveOnce(t, "/vaults/3", 200, vaultRow(3, edit))
			if v, err := c.Vault(context.Background(), 3); err == nil {
				t.Fatalf("应当拒绝,得到 %+v", v)
			}
		})
	}
}

func TestVaultsListsEveryVault(t *testing.T) {
	c, seen := serveOnce(t, "/vaults", 200, `{"vaults":[`+vaultRow(3, nil)+`,`+vaultRow(4, func(m map[string]any) {
		m["mainAccount"], m["manager"], m["hasManager"] = 31, nil, false
	})+`],"navPerShareScale":1000000}`)
	vs, err := c.Vaults(context.Background())
	if err != nil || len(vs) != 2 || (*seen)[0] != "/vaults" {
		t.Fatalf("金库列表不对: %+v %v %v", vs, err, *seen)
	}
	if vs[0].Vault != 3 || vs[0].Manager == nil || vs[1].Vault != 4 || vs[1].MainAccount != 31 || vs[1].Manager != nil {
		t.Fatalf("逐行解析不对: %+v", vs)
	}
}

// 列表与单个金库同一套校验:一行不合格整次报错,不跳过 —— 跳过会让准入少看一个金库。
func TestVaultsRejectsBadList(t *testing.T) {
	for name, body := range map[string]string{
		"缺 vaults": `{"navPerShareScale":1000000}`,
		"一行缺主账户":   `{"vaults":[` + vaultRow(3, nil) + `,` + vaultRow(4, func(m map[string]any) { delete(m, "mainAccount") }) + `]}`,
		"金库号重复":    `{"vaults":[` + vaultRow(3, nil) + `,` + vaultRow(3, nil) + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := serveOnce(t, "/vaults", 200, body)
			if vs, err := c.Vaults(context.Background()); err == nil {
				t.Fatalf("应当拒绝,得到 %+v", vs)
			}
		})
	}
}
