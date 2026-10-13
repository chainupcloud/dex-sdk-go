package dexos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testManager = "0x3333333333333333333333333333333333333333"
	// 金库 3:主账户 30;manager 地址的主账户是 12
	testVaultBody = `{"vault":3,"mainAccount":30,"manager":"` + testManager + `","hasManager":true,"tradingHalt":false,
		"creator":"` + testManager + `","whitelist":null,"depositorWhitelist":["` + testManager + `"],"nextNonce":0}`
)

type vaultGW struct {
	url   string
	seen  []string
	posts map[string][]byte
}

// vaultGateway 模拟开金库会话要读的接口。routes 覆盖或补充默认响应,值以 "404:" 开头时回 404;
// 未登记的路径直接判失败 —— 多读一个接口也是要看见的行为变化。
func vaultGateway(t *testing.T, agent Address, grants string, routes map[string]string) *vaultGW {
	t.Helper()
	all := map[string]string{
		"/config": fmt.Sprintf(`{"chainId":31337,"domain":{"name":"dex-os","version":"1",
			"verifyingContract":"0x0000000000000000000000000000000000000000"},
			"deposit":{"tokens":[]},"codecVer":%d,"snapshotVer":20,"finalityMode":"head"}`, CodecVersion),
		"/agent/" + agent.Hex():              `{"agent":"` + agent.Hex() + `","nextNonce":41,"grants":` + grants + `}`,
		"/vaults/3":                          testVaultBody,
		"/account/by-address/" + testManager: `{"accountId":12,"collateral":"0","nc":"0","imr":"0","withdrawable":"0","nextNonce":0}`,
	}
	for k, v := range routes {
		all[k] = v
	}
	g := &vaultGW{posts: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.seen = append(g.seen, r.URL.Path)
		if r.Method == http.MethodPost {
			g.posts[r.URL.Path], _ = io.ReadAll(r.Body)
		}
		body, ok := all[r.URL.Path]
		if !ok {
			t.Errorf("不该请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.Header().Set("content-type", "application/json")
		if rest, notFound := strings.CutPrefix(body, "404:"); notFound {
			w.WriteHeader(http.StatusNotFound)
			body = rest
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	g.url = srv.URL
	return g
}

func grantsJSON(gs ...Grant) string {
	b, _ := json.Marshal(gs)
	return string(b)
}

// 以金库身份交易:授权按 manager 账户核,会话账户是金库主账户,nonce 仍是 agent 自己的。
// 这把 key 还授权了别的账户(5)—— 不指明时是歧义,ForVault 已经说清代谁,不该再报歧义。
func TestForVaultTradesAsMainAccountWithManagerGrant(t *testing.T) {
	agent, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	g := vaultGateway(t, agent.Address(),
		grantsJSON(Grant{Master: 5, ValidUntilMs: "0"}, Grant{Master: 12, ValidUntilMs: "4102444800000"}),
		map[string]string{
			"/batch": `{"status":"ok","seq":12,"items":[{"inputIndex":0,"kind":"place","accountId":30,"market":7,"recordSub":0,"orderId":"9","state":"accepted","filledLots":0,"resting":true}],
				"events":[{"kind":"OrderAccepted","data":{"account":30,"market":7,"orderSeq":9,"price":100,"lots":1,"filledLots":0,"resting":true}}]}`,
			"/agents/12": `{"master":12,"nowMs":"1","agents":[{"address":"` + agent.Address().Hex() + `","validUntilMs":"4102444800000","expired":false,"nextNonce":42}]}`,
		})
	s, err := New(context.Background(), g.url, agent.PrivateKeyHex(), ForVault(3))
	if err != nil {
		t.Fatalf("开金库会话失败: %v", err)
	}
	if s.Account != 30 || s.ValidUntilMs != 4102444800000 || s.nonce != 41 {
		t.Fatalf("会话身份不对: account=%d validUntil=%d nonce=%d", s.Account, s.ValidUntilMs, s.nonce)
	}

	// 签名里的账户就是金库主账户:服务端按请求体重建命令再比对哈希,这里核请求体。
	if _, err := s.Place(context.Background(), BatchOrder{Market: 7, Side: Buy, Price: 100, Lots: 1, TIF: PostOnly}); err != nil {
		t.Fatalf("以金库身份下单失败: %v", err)
	}
	var sent struct {
		Account uint32 `json:"account"`
		Nonce   uint64 `json:"nonce"`
	}
	if err := json.Unmarshal(g.posts["/batch"], &sent); err != nil || sent.Account != 30 || sent.Nonce != 41 {
		t.Fatalf("下单要以金库主账户 30、agent nonce 41 签,实际 %+v %v", sent, err)
	}
	if s.nonce != 42 {
		t.Fatalf("下单后 agent nonce 应前进到 42,得到 %d", s.nonce)
	}

	// Resync 要去授权所在的 manager 账户读 agent nonce;金库主账户名下没有这把 key 的授权。
	if err := s.Resync(context.Background()); err != nil || s.nonce != 42 {
		t.Fatalf("Resync 失败: nonce=%d err=%v", s.nonce, err)
	}
}

func TestForVaultRejections(t *testing.T) {
	agent, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	live := func(master uint32) Grant { return Grant{Master: master, ValidUntilMs: "0"} }
	for _, tc := range []struct {
		name   string
		grants string
		routes map[string]string
		check  func(error) bool
	}{
		{"金库不存在", grantsJSON(live(12)), map[string]string{"/vaults/3": `404:{"error":"unknown vault"}`},
			func(err error) bool { return errors.Is(err, ErrVaultNotFound) }},
		{"金库未认领", grantsJSON(live(12)), map[string]string{"/vaults/3": `{"vault":3,"mainAccount":30,"manager":null,"hasManager":false,"tradingHalt":false}`},
			func(err error) bool { return errors.Is(err, ErrVaultUnclaimed) }},
		// 授权批在金库主账户 30 上是用不上的(dex-os 按 manager 查),不能拿它凑数
		{"manager 没授权这把 key", grantsJSON(live(5), live(30)), nil,
			func(err error) bool {
				return contains(err.Error(), "没有授权") && contains(err.Error(), "manager 账户 12")
			}},
		{"manager 的授权已过期", grantsJSON(live(5), Grant{Master: 12, ValidUntilMs: "1", Expired: true}), nil,
			func(err error) bool {
				return contains(err.Error(), "过期") && contains(err.Error(), "manager 账户 12")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := vaultGateway(t, agent.Address(), tc.grants, tc.routes)
			s, err := New(context.Background(), g.url, agent.PrivateKeyHex(), ForVault(3))
			if err == nil || !tc.check(err) {
				t.Fatalf("要拒绝,得到 session=%v err=%v", s, err)
			}
		})
	}
}

// ForAccount 与 ForVault 互斥:前者会话账户就是授权账户,后者是金库主账户。
// 同时给就是调用方自己没想清楚,开会话前就拒,不发任何请求。
func TestForVaultConflictsWithForAccount(t *testing.T) {
	agent, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	g := vaultGateway(t, agent.Address(), grantsJSON(Grant{Master: 12, ValidUntilMs: "0"}), nil)
	for _, opts := range [][]Option{{ForAccount(12), ForVault(3)}, {ForVault(3), ForAccount(12)}} {
		if _, err := New(context.Background(), g.url, agent.PrivateKeyHex(), opts...); err == nil || !contains(err.Error(), "ForVault") {
			t.Fatalf("同时指定 ForAccount 与 ForVault 应当报错,得到 %v", err)
		}
	}
	if len(g.seen) != 0 {
		t.Fatalf("冲突的选项不该发出请求: %v", g.seen)
	}
}

// /vaults/:id 的身份字段缺失或自相矛盾时不能猜:主账户缺失会把单签到账户 0 上。
func TestVaultRejectsMissingOrInconsistentIdentity(t *testing.T) {
	for name, body := range map[string]string{
		"缺 vault":        `{"mainAccount":30,"manager":"` + testManager + `","hasManager":true,"tradingHalt":false}`,
		"金库号不符":          `{"vault":4,"mainAccount":30,"manager":"` + testManager + `","hasManager":true,"tradingHalt":false}`,
		"缺 mainAccount":  `{"vault":3,"manager":"` + testManager + `","hasManager":true,"tradingHalt":false}`,
		"缺 hasManager":   `{"vault":3,"mainAccount":30,"manager":"` + testManager + `","tradingHalt":false}`,
		"缺 manager":      `{"vault":3,"mainAccount":30,"hasManager":false,"tradingHalt":false}`,
		"缺 tradingHalt":  `{"vault":3,"mainAccount":30,"manager":"` + testManager + `","hasManager":true}`,
		"有 manager 却没地址": `{"vault":3,"mainAccount":30,"manager":null,"hasManager":true,"tradingHalt":false}`,
		"无 manager 却有地址": `{"vault":3,"mainAccount":30,"manager":"` + testManager + `","hasManager":false,"tradingHalt":false}`,
		"manager 地址非法":   `{"vault":3,"mainAccount":30,"manager":"0x12","hasManager":true,"tradingHalt":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := serveOnce(t, "/vaults/3", 200, body)
			if v, err := c.Vault(context.Background(), 3); err == nil {
				t.Fatalf("应当拒绝,得到 %+v", v)
			}
		})
	}
	c, _ := serveOnce(t, "/vaults/3", 200, `{"vault":3,"mainAccount":30,"manager":"`+testManager+`","hasManager":true,"tradingHalt":true}`)
	v, err := c.Vault(context.Background(), 3)
	if err != nil || v.Vault != 3 || v.MainAccount != 30 || v.Manager == nil || v.Manager.Hex() != testManager || !v.TradingHalt {
		t.Fatalf("金库视图不完整: %+v %v", v, err)
	}
}
