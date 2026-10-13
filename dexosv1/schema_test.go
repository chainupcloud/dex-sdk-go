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
// items、prefixItems、enum、const、pattern。

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

// validate 返回全部违反处(空 = 通过)。
func validate(spec, schema map[string]any, v any, at string) []string {
	s := deref(spec, schema)
	var errs []string
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
		{"Fill", reflect.TypeFor[wireFill](), schema("Fill")},
		{"LedgerEntry", reflect.TypeFor[LedgerEntry](), schema("LedgerEntry")},
		{"Nonce", reflect.TypeFor[Nonce](), schema("Nonce")},
		{"Agent", reflect.TypeFor[Agent](), schema("Agent")},
		{"Exchange", reflect.TypeFor[Exchange](), exchange},
		{"Exchange.domain", reflect.TypeFor[Exchange]().Field(1).Type, props(obj(exchange["domain"]))},
		{"Exchange.deposit.tokens[]", reflect.TypeFor[Token](), props(obj(obj(props(obj(exchange["deposit"]))["tokens"])["items"]))},
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
