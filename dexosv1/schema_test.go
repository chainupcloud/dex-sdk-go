package dexosv1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 契约钉在 testdata/openapi-v1.json(dex-os 的 web/openapi-v1.json 副本,来源见 testdata/README.md)。
// 这里做两件事:
//   - 假服务用的每个 fixture 都先过一遍对应响应的 schema —— 解码测试才证明「按规格的响应能解」,
//     而不是「按我想象的响应能解」;
//   - 本包解码用到的每个 json 字段都必须是 schema 里有的属性 —— 字段名拼错、读了规格外的字段,在这里红。
//
// 校验器只实现规格里实际用到的 JSON Schema 子集:$ref、type(含类型数组)、required、properties、
// items、prefixItems、enum、const、pattern、allOf、oneOf(按「至少一支通过」判:规格里的分支都是
// 没有 additionalProperties 限制的宽松对象,严格的「恰好一支」会把合法响应判错)。

func loadSpec(t testing.TB) map[string]any {
	t.Helper()
	return loadJSON(t, filepath.Join("testdata", "openapi-v1.json")).(map[string]any)
}

func loadJSON(t testing.TB, path string) any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return decodeJSON(t, raw)
}

func decodeJSON(t testing.TB, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	return v
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }

// componentSchema `#/components/schemas/<name>`。
func componentSchema(t testing.TB, spec map[string]any, name string) map[string]any {
	t.Helper()
	s := obj(obj(obj(spec["components"])["schemas"])[name])
	if s == nil {
		t.Fatalf("规格里没有 schema %q", name)
	}
	return s
}

// responseSchema GET <path> 的 <status> 响应体 schema。
func responseSchema(t testing.TB, spec map[string]any, path, status string) map[string]any {
	t.Helper()
	op := obj(obj(obj(spec["paths"])[path])["get"])
	s := obj(obj(obj(obj(obj(op["responses"])[status])["content"])["application/json"])["schema"])
	if s == nil {
		t.Fatalf("规格里没有 GET %s 的 %s 响应 schema", path, status)
	}
	return s
}

func deref(spec, s map[string]any) map[string]any {
	for {
		ref, ok := s["$ref"].(string)
		if !ok {
			return s
		}
		name, found := strings.CutPrefix(ref, "#/components/schemas/")
		if !found {
			panic("不支持的 $ref: " + ref)
		}
		s = obj(obj(obj(spec["components"])["schemas"])[name])
	}
}

func jsonType(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		if strings.ContainsAny(x.String(), ".eE") {
			return "number"
		}
		return "integer"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func typeOK(want any, got string) bool {
	match := func(w string) bool { return w == got || (w == "number" && got == "integer") }
	switch w := want.(type) {
	case string:
		return match(w)
	case []any:
		for _, x := range w {
			if s, _ := x.(string); match(s) {
				return true
			}
		}
		return false
	}
	return true
}

// requestSchema POST <path> 的请求体 schema。
func requestSchema(t testing.TB, spec map[string]any, path string) map[string]any {
	t.Helper()
	op := obj(obj(obj(spec["paths"])[path])["post"])
	s := obj(obj(obj(obj(op["requestBody"])["content"])["application/json"])["schema"])
	if s == nil {
		t.Fatalf("规格里没有 POST %s 的请求体 schema", path)
	}
	return s
}

// postResponseSchema POST <path> 的 <status> 响应体 schema。
func postResponseSchema(t testing.TB, spec map[string]any, path, status string) map[string]any {
	t.Helper()
	op := obj(obj(obj(spec["paths"])[path])["post"])
	s := obj(obj(obj(obj(obj(op["responses"])[status])["content"])["application/json"])["schema"])
	if s == nil {
		t.Fatalf("规格里没有 POST %s 的 %s 响应 schema", path, status)
	}
	return s
}

// validate 返回全部违反处(空 = 通过)。
func validate(spec, schema map[string]any, v any, at string) []string {
	s := deref(spec, schema)
	var errs []string
	for _, sub := range asSlice(s["allOf"]) {
		errs = append(errs, validate(spec, obj(sub), v, at)...)
	}
	if alts := asSlice(s["oneOf"]); len(alts) > 0 {
		var why []string
		ok := false
		for i, sub := range alts {
			e := validate(spec, obj(sub), v, at)
			if len(e) == 0 {
				ok = true
				break
			}
			why = append(why, fmt.Sprintf("分支 %d: %s", i, strings.Join(e, "; ")))
		}
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: oneOf 没有一支通过(%s)", at, strings.Join(why, " | ")))
		}
	}
	if want, ok := s["type"]; ok && !typeOK(want, jsonType(v)) {
		return []string{fmt.Sprintf("%s: 类型应为 %v,实为 %s", at, want, jsonType(v))}
	}
	if c, ok := s["const"]; ok && fmt.Sprint(c) != fmt.Sprint(v) {
		errs = append(errs, fmt.Sprintf("%s: 应为常量 %v,实为 %v", at, c, v))
	}
	if enum, ok := s["enum"].([]any); ok && v != nil {
		found := false
		for _, e := range enum {
			found = found || fmt.Sprint(e) == fmt.Sprint(v)
		}
		if !found {
			errs = append(errs, fmt.Sprintf("%s: %v 不在枚举 %v 里", at, v, enum))
		}
	}
	if p, ok := s["pattern"].(string); ok {
		if str, isStr := v.(string); isStr && !regexp.MustCompile(p).MatchString(str) {
			errs = append(errs, fmt.Sprintf("%s: %q 不匹配 %s", at, str, p))
		}
	}
	switch x := v.(type) {
	case map[string]any:
		for _, r := range asSlice(s["required"]) {
			if _, ok := x[r.(string)]; !ok {
				errs = append(errs, fmt.Sprintf("%s: 缺必填字段 %s", at, r))
			}
		}
		props := obj(s["properties"])
		for k, child := range x {
			if ps := obj(props[k]); ps != nil {
				errs = append(errs, validate(spec, ps, child, at+"."+k)...)
			}
		}
	case []any:
		prefix := asSlice(s["prefixItems"])
		for i, child := range x {
			if i < len(prefix) {
				errs = append(errs, validate(spec, obj(prefix[i]), child, fmt.Sprintf("%s[%d]", at, i))...)
			} else if items := obj(s["items"]); items != nil {
				errs = append(errs, validate(spec, items, child, fmt.Sprintf("%s[%d]", at, i))...)
			}
		}
	}
	return errs
}

func asSlice(v any) []any { s, _ := v.([]any); return s }

// fixtureSchemas 每个 fixture 应当符合的响应 schema。
var fixtureSchemas = []struct {
	file, path, status, component string
}{
	{"markets.json", "/v1/markets", "200", ""},
	{"book.json", "/v1/markets/{symbol}/book", "200", ""},
	{"account.json", "/v1/account", "200", ""},
	{"positions.json", "/v1/account/positions", "200", ""},
	{"orders.json", "/v1/account/orders", "200", ""},
	{"fills_p1.json", "/v1/account/fills", "200", ""},
	{"fills_p2.json", "/v1/account/fills", "200", ""},
	{"ledger.json", "/v1/account/ledger", "200", ""},
	{"nonce.json", "/v1/account/nonce", "200", ""},
	{"nonce_agent.json", "/v1/account/nonce", "200", ""},
	{"agents.json", "/v1/account/agents", "200", ""},
	{"exchange.json", "/v1/exchange", "200", ""},
	{"request_executed.json", "/v1/requests/{requestId}", "200", ""},
	{"err404.json", "", "", "Error"},
	{"err429.json", "", "", "Error"},
	{"err409_epoch.json", "", "", "Error"},
	{"err503_gaps.json", "", "", "Error"},
	{"request_not_found.json", "", "", "Error"},
	{"w_agents.json", "/v1/account/agents", "200", ""},
	{"w_nonce_agent.json", "/v1/account/nonce", "200", ""},
	{"w_nonce_account.json", "/v1/account/nonce", "200", ""},
	{"err422_margin.json", "", "", "Error"},
	{"err422_nonce.json", "", "", "Error"},
	{"err422_unknown_agent.json", "", "", "Error"},
	{"err401_sig.json", "", "", "Error"},
	{"err429_account.json", "", "", "Error"},
	{"prices.json", "/v1/prices", "200", ""},
	{"candles.json", "/v1/markets/{symbol}/candles", "200", ""},
	{"portfolio.json", "/v1/account/portfolio", "200", ""},
	{"agent_grants.json", "/v1/agents/{address}", "200", ""},
	{"my_vaults.json", "/v1/account/vaults", "200", ""},
	{"subaccounts.json", "/v1/account/subaccounts", "200", ""},
	{"vault.json", "/v1/vaults/{id}", "200", ""},
	{"vaults.json", "/v1/vaults", "200", ""},
	{"account_vault.json", "/v1/account", "200", ""},
	{"ledger_vault.json", "/v1/account/ledger", "200", ""},
	{"w_nonce_vault.json", "/v1/account/nonce", "200", ""},
	{"tickers.json", "/v1/tickers", "200", ""},
	{"err400_incomplete.json", "", "", "Error"},
	{"w_sequenced_202.json", "", "", "Sequenced"},
}

// postFixtureSchemas 写接口回执的 fixture(形状照原生 v1 的 v1_write.rs 整理)。202 的回执按 `Sequenced`
// 校验,登记在 fixtureSchemas。
var postFixtureSchemas = []struct {
	file, path, status string
}{
	{"w_batch_place.json", "/v1/orders", "200"},
	{"w_order.json", "/v1/orders", "200"},
	{"w_batch_cancel.json", "/v1/orders/cancel", "200"},
	{"w_cancel_one.json", "/v1/orders/cancel", "200"},
	{"w_replace.json", "/v1/orders/replace", "200"},
	{"w_replace_paired.json", "/v1/orders/replace", "200"},
	{"w_cancel_all.json", "/v1/orders/cancel-all", "200"},
	{"w_schedule.json", "/v1/orders/schedule-cancel", "200"},
	{"w_leverage.json", "/v1/account/leverage", "200"},
	{"w_approve.json", "/v1/account/agents/approve", "200"},
	{"w_revoke.json", "/v1/account/agents/revoke", "200"},
	{"w_vault_create.json", "/v1/vaults", "200"},
	{"w_vault_deposit.json", "/v1/vaults/{id}/deposit", "200"},
	{"w_vault_redeem.json", "/v1/vaults/{id}/redeem", "200"},
	{"w_vault_claim.json", "/v1/vaults/{id}/claim", "200"},
	{"w_vault_config.json", "/v1/vaults/{id}/config", "200"},
}

func TestFixturesConformToPinnedSpec(t *testing.T) {
	spec := loadSpec(t)
	seen := map[string]bool{}
	for _, f := range fixtureSchemas {
		seen[f.file] = true
		var schema map[string]any
		if f.component != "" {
			schema = componentSchema(t, spec, f.component)
		} else {
			schema = responseSchema(t, spec, f.path, f.status)
		}
		if errs := validate(spec, schema, loadJSON(t, filepath.Join("testdata", "fixtures", f.file)), f.file); len(errs) > 0 {
			t.Errorf("%s 不符合规格:\n  %s", f.file, strings.Join(errs, "\n  "))
		}
	}
	for _, f := range postFixtureSchemas {
		seen[f.file] = true
		if errs := validate(spec, postResponseSchema(t, spec, f.path, f.status), loadJSON(t, filepath.Join("testdata", "fixtures", f.file)), f.file); len(errs) > 0 {
			t.Errorf("%s 不符合规格:\n  %s", f.file, strings.Join(errs, "\n  "))
		}
	}
	// 新增 fixture 必须登记在上表,否则它可能不符合规格而没人发现
	entries, err := os.ReadDir(filepath.Join("testdata", "fixtures"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !seen[e.Name()] {
			t.Errorf("fixture %s 没有登记要符合哪个 schema", e.Name())
		}
	}
}

// 本包解码用到的 json 字段都必须是规格里有的属性。
func TestDecodedFieldsExistInPinnedSpec(t *testing.T) {
	spec := loadSpec(t)
	props := func(s map[string]any) map[string]any { return obj(deref(spec, s)["properties"]) }
	schema := func(name string) map[string]any { return props(componentSchema(t, spec, name)) }
	exchange := schema("Exchange")
	portfolio := schema("Portfolio")
	myVault := schema("MyVaultPosition")
	cases := []struct {
		name  string
		typ   reflect.Type
		props map[string]any
	}{
		{"Market", reflect.TypeFor[wireMarket](), schema("Market")},
		{"Account", reflect.TypeFor[Account](), schema("Account")},
		{"Balance", reflect.TypeFor[Balance](), schema("Balance")},
		{"Account.leverage[]", reflect.TypeFor[MarketLeverage](), props(obj(obj(schema("Account")["leverage"])["items"]))},
		{"Position", reflect.TypeFor[wirePosition](), schema("Position")},
		{"Order", reflect.TypeFor[wireOrder](), schema("Order")},
		{"Order.trigger", reflect.TypeFor[wireOrderTrigger](), props(obj(schema("Order")["trigger"]))},
		{"Fill", reflect.TypeFor[wireFill](), schema("Fill")},
		{"LedgerEntry", reflect.TypeFor[LedgerEntry](), schema("LedgerEntry")},
		{"Nonce", reflect.TypeFor[Nonce](), schema("Nonce")},
		{"Agent", reflect.TypeFor[Agent](), schema("Agent")},
		{"Exchange", reflect.TypeFor[Exchange](), exchange},
		{"Exchange.domain", field[Exchange](t, "Domain"), props(obj(exchange["domain"]))},
		{"Exchange.version", field[Exchange](t, "Version"), props(obj(exchange["version"]))},
		{"Exchange.deposit.tokens[]", reflect.TypeFor[Token](), props(obj(obj(props(obj(exchange["deposit"]))["tokens"])["items"]))},
		{"Account.groups[]", reflect.TypeFor[RiskGroup](), props(obj(obj(schema("Account")["groups"])["items"]))},
		{"Book", reflect.TypeFor[wireBook](), schema("Book")},
		{"Price", reflect.TypeFor[wirePrice](), schema("Price")},
		{"Ticker", reflect.TypeFor[wireTicker](), schema("Ticker")},
		{"prices.window24h", reflect.TypeFor[Window24h](), props(obj(obj(responseSchema(t, spec, "/v1/prices", "200")["properties"])["window24h"]))},
		{"Candle", reflect.TypeFor[wireCandle](), schema("Candle")},
		{"Portfolio", reflect.TypeFor[Portfolio](), portfolio},
		{"PortfolioScope", reflect.TypeFor[PortfolioScope](), schema("PortfolioScope")},
		{"PortfolioPoint", reflect.TypeFor[PortfolioPoint](), schema("PortfolioPoint")},
		{"Portfolio.gaps[]", reflect.TypeFor[PortfolioGap](), props(obj(obj(portfolio["gaps"])["items"]))},
		{"AgentGrant", reflect.TypeFor[AgentGrant](), schema("AgentGrant")},
		{"MyVaultPosition", reflect.TypeFor[VaultPosition](), myVault},
		{"MyVaultPosition.withdrawable", field[VaultPosition](t, "Withdrawable"), props(obj(myVault["withdrawable"]))},
		{"Vault", reflect.TypeFor[wireVault](), schema("Vault")},
		{"VaultMarket", reflect.TypeFor[wireVaultMarket](), schema("VaultMarket")},
		{"VaultMaxLeverage", reflect.TypeFor[wireVaultLeverage](), schema("VaultMaxLeverage")},
		{"VaultConfig", reflect.TypeFor[wireVaultConfig](), schema("VaultConfig")},
		{"Market.fees", reflect.TypeFor[MarketFees](), props(obj(schema("Market")["fees"]))},
		{"Subaccount", reflect.TypeFor[Subaccount](), schema("Subaccount")},
	}
	for _, c := range cases {
		if c.props == nil {
			t.Fatalf("%s: 规格里找不到属性表", c.name)
		}
		var missing []string
		for i := range c.typ.NumField() {
			tag, _, _ := strings.Cut(c.typ.Field(i).Tag.Get("json"), ",")
			if tag == "" || tag == "-" {
				continue
			}
			if _, ok := c.props[tag]; !ok {
				missing = append(missing, tag)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s 读了规格里没有的字段: %v", c.name, missing)
		}
	}
}

// field 结构体 T 里名为 name 的字段的类型(内嵌的匿名结构体按名字取,不依赖字段顺序)。
func field[T any](t *testing.T, name string) reflect.Type {
	t.Helper()
	f, ok := reflect.TypeFor[T]().FieldByName(name)
	if !ok {
		t.Fatalf("%s 没有字段 %s", reflect.TypeFor[T](), name)
	}
	return f.Type
}

// 本包认的 K 线周期与规格的枚举逐一相同:规格加了周期,这里红,提醒补常量。
func TestIntervalsMatchPinnedSpec(t *testing.T) {
	spec := loadSpec(t)
	op := obj(obj(obj(spec["paths"])["/v1/markets/{symbol}/candles"])["get"])
	var want []string
	for _, p := range asSlice(op["parameters"]) {
		if obj(p)["name"] == "interval" {
			for _, v := range asSlice(obj(obj(p)["schema"])["enum"]) {
				want = append(want, v.(string))
			}
		}
	}
	var got []string
	for iv := range intervals {
		got = append(got, iv)
	}
	sort.Strings(want)
	sort.Strings(got)
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("本包的周期 %v,规格 %v", got, want)
	}
}

// 校验器本身要能拦住违规,否则上面两项测试恒真。
func TestValidatorCatchesViolations(t *testing.T) {
	spec := loadSpec(t)
	markets := responseSchema(t, spec, "/v1/markets", "200")
	for _, c := range []struct{ name, from, to, want string }{
		{"枚举外的值", `"marginMode":"cross"`, `"marginMode":"normal"`, "不在枚举"},
		{"类型不符", `"kind":"perp"`, `"kind":7`, "类型应为"},
		{"十进制格式", `"lastPrice":"83952"`, `"lastPrice":"8e4"`, "不匹配"},
		{"缺必填", `"symbol":"BTC-USD",`, ``, "缺必填字段 symbol"},
	} {
		body := strings.Replace(string(fixture(t, "markets.json")), c.from, c.to, 1)
		errs := validate(spec, markets, decodeJSON(t, []byte(body)), "markets")
		if !strings.Contains(strings.Join(errs, "\n"), c.want) {
			t.Errorf("%s 没被拦住: %v", c.name, errs)
		}
	}
}
