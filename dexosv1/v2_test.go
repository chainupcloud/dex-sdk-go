package dexosv1

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

type goldenDomain struct {
	ChainID           uint64 `json:"chainId"`
	VerifyingContract string `json:"verifyingContract"`
}

type goldenLeg struct {
	PrivateKey string `json:"privateKey"`
	Signer     string `json:"signer"`
	Account    string `json:"account"`
	Subaccount uint16 `json:"subaccount"`
	Nonce      uint64 `json:"nonce,string"`
	Expiry     uint64 `json:"expiry,string"`
	Inner      string `json:"inner"`
	Action     string `json:"action"`
	Digest     string `json:"digest"`
	Signature  string `json:"signature"`
	RequestID  string `json:"requestId"`
}

type goldenPair struct {
	Name      string       `json:"name"`
	Domain    goldenDomain `json:"domain"`
	Market    uint16       `json:"market"`
	MakerSide string       `json:"makerSide"`
	Price     uint32       `json:"price"`
	Lots      uint64       `json:"lots,string"`
	NowMs     uint64       `json:"nowMs,string"`
	Maker     goldenLeg    `json:"maker"`
	Taker     goldenLeg    `json:"taker"`
	RequestID string       `json:"requestId"`
}

type goldenScope struct {
	Domain         goldenDomain `json:"domain"`
	Owner          string       `json:"owner"`
	Agent          string       `json:"agent"`
	SubaccountMask uint64       `json:"subaccountMask,string"`
	ValidUntilMs   uint64       `json:"validUntilMs,string"`
	Nonce          uint64       `json:"nonce,string"`
	Digest         string       `json:"digest"`
	Signature      string       `json:"signature"`
	RequestID      string       `json:"requestId"`
}

type pairedGolden struct {
	CodecVersion      uint32        `json:"codecVersion"`
	ActorPlaceholder  uint32        `json:"actorPlaceholder"`
	PairedTrades      []goldenPair  `json:"pairedTrades"`
	ApproveAgentScope []goldenScope `json:"approveAgentScope"`
	RevokeAgentScope  []goldenScope `json:"revokeAgentScope"`
}

func loadPairedGolden(t *testing.T) pairedGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "paired_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g pairedGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.PairedTrades) == 0 || len(g.ApproveAgentScope) == 0 || len(g.RevokeAgentScope) == 0 {
		t.Fatal("金样为空")
	}
	return g
}

func (d goldenDomain) domain(t *testing.T) dexos.Domain {
	return dexos.Domain{Name: "dex-os", Version: "1", ChainID: d.ChainID, VerifyingContract: mustAddr(t, d.VerifyingContract)}
}

func hex0x(b []byte) string { return "0x" + hex.EncodeToString(b) }

// 金样由 dex-os 的 Rust 实现导出(生成程序不入库,做法见 testdata/README.md),与这里的 Go 实现互相独立:
// PairedTradeLeg 的规范编码、v2 Action 的摘要、签名与请求身份,任何一处与执行层不同都在这里红。
func TestPairedTradeGoldenVectors(t *testing.T) {
	g := loadPairedGolden(t)
	if g.CodecVersion != dexos.CodecVersion || g.ActorPlaceholder != dexos.ActorPlaceholder {
		t.Fatalf("金样的编码版本 %d / 占位值 %d,本包 %d / %d", g.CodecVersion, g.ActorPlaceholder, dexos.CodecVersion, dexos.ActorPlaceholder)
	}
	for _, v := range g.PairedTrades {
		d := v.Domain.domain(t)
		side := map[string]dexos.Side{"buy": dexos.Buy, "sell": dexos.Sell}[v.MakerSide]
		for _, c := range []struct {
			role        string
			taker       bool
			me, counter goldenLeg
		}{{"maker", false, v.Maker, v.Taker}, {"taker", true, v.Taker, v.Maker}} {
			inner := dexos.EncodePairedTradeLeg(dexos.PairedTradeLeg{
				Taker: c.taker, Market: v.Market, MakerSide: side, Price: v.Price, Lots: v.Lots,
				Counterparty: mustAddr(t, c.counter.Account), CounterpartySubaccount: c.counter.Subaccount,
			}, v.NowMs)
			if got := hex0x(inner); got != c.me.Inner {
				t.Errorf("%s %s 编码 %s,金样 %s", v.Name, c.role, got, c.me.Inner)
				continue
			}
			if got := hex0x(dexos.Keccak256(inner)); got != c.me.Action {
				t.Errorf("%s %s action %s,金样 %s", v.Name, c.role, got, c.me.Action)
			}
			digest := d.ActionHash(mustAddr(t, c.me.Account), c.me.Subaccount, inner, c.me.Nonce, c.me.Expiry)
			if got := hex0x(digest); got != c.me.Digest {
				t.Errorf("%s %s 摘要 %s,金样 %s", v.Name, c.role, got, c.me.Digest)
				continue
			}
			key := signer(t, c.me.PrivateKey)
			if key.Address().Hex() != c.me.Signer {
				t.Fatalf("%s %s 签名者 %s,私钥推出 %s", v.Name, c.role, c.me.Signer, key.Address().Hex())
			}
			if sig, err := key.SignHashHex(digest); err != nil || sig != c.me.Signature {
				t.Errorf("%s %s 签名 %s(%v),金样 %s", v.Name, c.role, sig, err, c.me.Signature)
			}
			if id := newIdentity(key.Address(), "signer:"+key.Address().Hex(), c.me.Nonce, digest, nil); id.RequestID != c.me.RequestID {
				t.Errorf("%s %s 请求身份 %s,金样 %s", v.Name, c.role, id.RequestID, c.me.RequestID)
			}
		}
		// 成对成交的请求身份 = 挂单腿的签名身份(dex-os request_digest 对 PairedTrade 的定义)
		if v.RequestID != v.Maker.RequestID {
			t.Errorf("%s:整条请求身份 %s 不是挂单腿的 %s", v.Name, v.RequestID, v.Maker.RequestID)
		}
	}
}

func TestApproveAgentScopeGoldenVectors(t *testing.T) {
	g := loadPairedGolden(t)
	owner := signer(t, ownerKey)
	for i, v := range g.ApproveAgentScope {
		d := v.Domain.domain(t)
		if v.Owner != owner.Address().Hex() {
			t.Fatalf("向量 %d 的 owner %s 不是 %s", i, v.Owner, ownerAddr)
		}
		digest := d.ApproveAgentScopeHash(owner.Address(), mustAddr(t, v.Agent), v.SubaccountMask, v.ValidUntilMs, v.Nonce)
		if got := hex0x(digest); got != v.Digest {
			t.Errorf("向量 %d 摘要 %s,金样 %s", i, got, v.Digest)
			continue
		}
		if sig, err := owner.SignHashHex(digest); err != nil || sig != v.Signature {
			t.Errorf("向量 %d 签名 %s(%v),金样 %s", i, sig, err, v.Signature)
		}
		td := d.ApproveAgentScopeTypedData(owner.Address(), mustAddr(t, v.Agent), v.SubaccountMask, v.ValidUntilMs, v.Nonce)
		if td.Message["subaccountMask"] != strconv.FormatUint(v.SubaccountMask, 10) || td.Message["nonce"] != strconv.FormatUint(v.Nonce, 10) {
			t.Errorf("向量 %d 的钱包待签结构: %v", i, td.Message)
		}
		if id := newIdentity(owner.Address(), "signer:"+ownerAddr, v.Nonce, digest, nil); id.RequestID != v.RequestID {
			t.Errorf("向量 %d 请求身份 %s,金样 %s", i, id.RequestID, v.RequestID)
		}
	}
}

// 撤销 owner 级授权的金样按网关 agent_revoke 的 v2 分支导出:位图与有效期都恒为 0。
func TestRevokeAgentScopeGoldenVectors(t *testing.T) {
	g := loadPairedGolden(t)
	owner := signer(t, ownerKey)
	for i, v := range g.RevokeAgentScope {
		if v.SubaccountMask != 0 || v.ValidUntilMs != 0 || v.Owner != ownerAddr {
			t.Fatalf("撤销向量 %d 的形状: %+v", i, v)
		}
		d := v.Domain.domain(t)
		digest := d.RevokeAgentScopeHash(owner.Address(), mustAddr(t, v.Agent), v.Nonce)
		if got := hex0x(digest); got != v.Digest {
			t.Errorf("撤销向量 %d 摘要 %s,金样 %s", i, got, v.Digest)
			continue
		}
		if got := hashTypedDataV4(t, d.RevokeAgentScopeTypedData(owner.Address(), mustAddr(t, v.Agent), v.Nonce)); hex0x(got) != v.Digest {
			t.Errorf("撤销向量 %d 的钱包待签结构摘要 %x,金样 %s", i, got, v.Digest)
		}
		if sig, err := owner.SignHashHex(digest); err != nil || sig != v.Signature {
			t.Errorf("撤销向量 %d 签名 %s(%v),金样 %s", i, sig, err, v.Signature)
		}
		if id := newIdentity(owner.Address(), "signer:"+ownerAddr, v.Nonce, digest, nil); id.RequestID != v.RequestID {
			t.Errorf("撤销向量 %d 请求身份 %s,金样 %s", i, id.RequestID, v.RequestID)
		}
	}
}

// v2 的类型串与钉住的 OpenAPI `x-eip712.v2TypeString` 逐字一致。Action 的类型串由金样摘要钉住,
// 这里核对规格两边没漂移;ApproveAgentScope 的类型串从钱包待签结构的字段表拼出来核对。
func TestV2TypeStringsMatchPinnedSpec(t *testing.T) {
	spec := loadSpec(t)
	td := testDomain.ApproveAgentScopeTypedData(dexos.Address{}, dexos.Address{}, 0, 0, 0)
	parts := make([]string, 0, len(td.Types[td.PrimaryType]))
	for _, f := range td.Types[td.PrimaryType] {
		parts = append(parts, f.Type+" "+f.Name)
	}
	scope := td.PrimaryType + "(" + strings.Join(parts, ",") + ")"
	const action = "Action(address account,uint16 subaccount,bytes32 action,uint64 nonce,uint64 expiry)"
	for path, want := range map[string]string{
		"/v1/orders":                 action,
		"/v1/orders/replace":         action,
		"/v1/orders/cancel-all":      action,
		"/v1/account/agents/approve": scope,
		"/v1/account/agents/revoke":  scope,
	} {
		if got := obj(obj(obj(obj(spec["paths"])[path])["post"])["x-eip712"])["v2TypeString"]; got != want {
			t.Errorf("POST %s 的 v2TypeString 是 %v,本包签 %s", path, got, want)
		}
	}
}

// 代理列表同时给按账户授权与 owner 级授权;owner 级授权的位图以十进制字符串上线。
func TestAgentsListOwnerLevelGrants(t *testing.T) {
	f, c := newWriteFake(t)
	f.routes["/v1/account/agents"] = serve(t, 200, "w_agents_owner.json")
	agents, err := c.Agents(context.Background(), AccountRef{Address: mustAddr(t, ownerAddr), Subaccount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("应有 2 条授权: %+v", agents)
	}
	legacy, owner := agents[0], agents[1]
	if legacy.Scope != "account" || legacy.SubaccountMask != 0 || legacy.Subaccounts != nil || legacy.ValidUntil != nil {
		t.Fatalf("按账户授权: %+v", legacy)
	}
	if owner.Scope != "owner" || owner.SubaccountMask != 6 || !slices.Equal(owner.Subaccounts, []uint32{1, 2}) ||
		owner.Address != agentAddr || owner.Label != "paint" || *owner.ValidUntil != 1893456000000 {
		t.Fatalf("owner 级授权: %+v", owner)
	}
}

// 能不能代一个账户签,与执行层 agent_authorized 同口径:按账户授权或位图覆盖这个编号的 owner 级授权,任一未过期即可。
// 服务端在每个编号的列表里都列出全部 owner 级授权,不覆盖这个编号的不算。
func TestNewSessionHonorsOwnerLevelMask(t *testing.T) {
	ownerRow := `{"address":"` + agentAddr + `","scope":"owner","subaccountMask":"6","subaccounts":[1,2],"label":"","validUntil":null,"expired":false}`
	expiredLegacy := `{"address":"` + agentAddr + `","scope":"account","label":"","validUntil":1,"expired":true}`
	for _, c := range []struct {
		name string
		sub  uint32
		rows string
		want error
	}{
		{"位图覆盖", 1, ownerRow, nil},
		{"位图不覆盖", 0, ownerRow, ErrAgentNotAuthorized},
		{"旧授权过期但位图覆盖", 2, expiredLegacy + "," + ownerRow, nil},
		{"旧授权过期且位图不覆盖", 3, expiredLegacy + "," + ownerRow, ErrAgentExpired},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, cl := newWriteFake(t)
			f.routes["/v1/account/agents"] = func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"address":"` + ownerAddr + `","subaccount":0,"agents":[` + c.rows + `]}`))
			}
			s, err := NewSession(context.Background(), cl, signer(t, agentKey), AccountRef{Address: mustAddr(t, ownerAddr), Subaccount: c.sub})
			if !errors.Is(err, c.want) || (c.want == nil) != (s != nil) {
				t.Fatalf("应为 %v,得到 %v", c.want, err)
			}
			if s != nil && !s.ValidUntil.IsZero() {
				t.Fatalf("owner 级授权不过期,会话却有到期时刻 %s", s.ValidUntil)
			}
		})
	}
}

// 窗口 nonce:取当前毫秒,同一签名者在进程内严格递增;不同签名者互不影响。
func TestWindowNonceStrictlyIncreasing(t *testing.T) {
	var w windowNonces
	a, b := mustAddr(t, agentAddr), mustAddr(t, ownerAddr)
	if got := []uint64{w.next(a, 1000), w.next(a, 1000), w.next(a, 999), w.next(b, 1000), w.next(a, 5000)}; !slices.Equal(got, []uint64{1000, 1001, 1002, 1000, 5000}) {
		t.Fatalf("窗口 nonce: %v", got)
	}
	var mu sync.Mutex
	seen := map[uint64]bool{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				n := w.next(a, 7000)
				mu.Lock()
				seen[n] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != 800 {
		t.Fatalf("并发取了 800 个,不同的只有 %d 个", len(seen))
	}
	if n, now := WindowNonce(b), uint64(time.Now().UnixMilli()); n < now-1000 || n > now+1000 {
		t.Fatalf("WindowNonce 应取当前毫秒附近: %d vs %d", n, now)
	}
}

// owner 级授权:主钱包在别处签 ApproveAgentScope,本进程只提交。假网关按请求体独立重算摘要并恢复签名者。
func TestSubmitAgentScope(t *testing.T) {
	f, c := newWriteFake(t)
	f.answer(t, "/v1/account/agents/approve", 200, "w_approve_scope.json")
	owner := signer(t, ownerKey)
	agent := mustAddr(t, agentAddr)
	nonce := WindowNonce(owner.Address())
	digest := testDomain.ApproveAgentScopeHash(owner.Address(), agent, 6, 0, nonce)
	if got := hashTypedDataV4(t, testDomain.ApproveAgentScopeTypedData(owner.Address(), agent, 6, 0, nonce)); !bytes.Equal(got, digest) {
		t.Fatalf("待签载荷与提交时核对的摘要不同: %x != %x", got, digest)
	}
	sig, err := owner.SignHashHex(digest)
	if err != nil {
		t.Fatal(err)
	}
	a := SignedAgentScope{Owner: owner.Address(), Domain: testDomain, Agent: agent, SubaccountMask: 6, Nonce: nonce, Signature: sig, Label: "paint"}
	r, err := c.SubmitAgentScope(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	body := m(f.last(t).body)
	conformsRequest(t, "/v1/account/agents/approve", body)
	if num(t, body["sigVersion"]) != 2 || body["account"] != ownerAddr || body["subaccountMask"] != "6" || body["agent"] != agentAddr ||
		body["signer"] != nil || body["subaccount"] != nil || body["validUntil"] != nil || num(t, body["nonce"]) != nonce || body["label"] != "paint" {
		t.Fatalf("v2 授权的请求体: %v", body)
	}
	if recoverAddr(t, digest, body["signature"].(string)) != ownerAddr {
		t.Fatal("网关恢复不出主钱包")
	}
	if r.Seq != 511 || r.Request.Scope != "signer:"+ownerAddr || r.Request.Nonce != nonce || r.Request.CommandHash != "" {
		t.Fatalf("回执: %+v / %+v", r, r.Request)
	}

	sent := f.count()
	for name, bad := range map[string]SignedAgentScope{
		"位图为 0":      {Owner: owner.Address(), Domain: testDomain, Agent: agent, Nonce: nonce, Signature: sig},
		"签名格式":       {Owner: owner.Address(), Domain: testDomain, Agent: agent, SubaccountMask: 6, Nonce: nonce, Signature: "0x12"},
		"授权给主钱包自己":   {Owner: owner.Address(), Domain: testDomain, Agent: owner.Address(), SubaccountMask: 6, Nonce: nonce, Signature: sig},
		"API 钱包地址为空": {Owner: owner.Address(), Domain: testDomain, SubaccountMask: 6, Nonce: nonce, Signature: sig},
	} {
		if _, err := c.SubmitAgentScope(context.Background(), bad); err == nil {
			t.Errorf("%s: 应在本地报错", name)
		}
	}
	if f.count() != sent {
		t.Fatal("本地校验失败不该发请求")
	}

	// 回显的位图与提交的不同:场所落下的不是这份授权
	f.answerRaw(t, "/v1/account/agents/approve", 200, []byte(strings.Replace(string(fixture(t, "w_approve_scope.json")), `"subaccountMask":"6"`, `"subaccountMask":"7"`, 1)))
	if _, err := c.SubmitAgentScope(context.Background(), a); err == nil || !strings.Contains(err.Error(), "subaccountMask") {
		t.Fatalf("位图回显不符应报错: %v", err)
	}
}

// hashTypedDataV4 按钱包待签结构通用地重算 EIP-712 摘要(只覆盖 address 与 uintN),不经 ApproveAgentScopeHash 的手写字段顺序。
func hashTypedDataV4(t *testing.T, td dexos.TypedData) []byte {
	t.Helper()
	enc := func(name string, values map[string]any) []byte {
		parts := make([]string, 0, len(td.Types[name]))
		for _, f := range td.Types[name] {
			parts = append(parts, f.Type+" "+f.Name)
		}
		words := [][]byte{dexos.Keccak256([]byte(name + "(" + strings.Join(parts, ",") + ")"))}
		for _, f := range td.Types[name] {
			switch v := values[f.Name].(type) {
			case string:
				if f.Type == "address" {
					a := mustAddr(t, v)
					words = append(words, append(make([]byte, 12), a[:]...))
				} else if f.Type == "string" {
					words = append(words, dexos.Keccak256([]byte(v)))
				} else {
					n, ok := new(big.Int).SetString(v, 10)
					if !ok {
						t.Fatalf("%s 不是十进制整数: %q", f.Name, v)
					}
					words = append(words, n.FillBytes(make([]byte, 32)))
				}
			case uint64:
				words = append(words, new(big.Int).SetUint64(v).FillBytes(make([]byte, 32)))
			default:
				t.Fatalf("%s 的类型 %T 不在覆盖范围", f.Name, v)
			}
		}
		return dexos.Keccak256(words...)
	}
	domain := enc("EIP712Domain", map[string]any{"name": td.Domain.Name, "version": td.Domain.Version, "chainId": td.Domain.ChainID,
		"verifyingContract": td.Domain.VerifyingContract})
	return dexos.Keccak256([]byte{0x19, 0x01}, domain, enc(td.PrimaryType, td.Message))
}

// 撤销 owner 级授权走 `POST /v1/account/agents/revoke`(网关的 v2 approve 拒位图 0):信封 sigVersion 2,只带 agent;
// 假网关按网关的做法重算位图 0、有效期 0 的摘要并恢复签名者。
func TestSubmitAgentScopeRevoke(t *testing.T) {
	f, c := newWriteFake(t)
	f.answer(t, "/v1/account/agents/revoke", 200, "w_revoke.json")
	owner := signer(t, ownerKey)
	agent := mustAddr(t, agentAddr)
	nonce := WindowNonce(owner.Address())
	sig, err := owner.SignHashHex(testDomain.RevokeAgentScopeHash(owner.Address(), agent, nonce))
	if err != nil {
		t.Fatal(err)
	}
	rv := SignedAgentScopeRevoke{Owner: owner.Address(), Domain: testDomain, Agent: agent, Nonce: nonce, Signature: sig}
	r, err := c.SubmitAgentScopeRevoke(context.Background(), rv)
	if err != nil {
		t.Fatal(err)
	}
	body := m(f.last(t).body)
	conformsRequest(t, "/v1/account/agents/revoke", body)
	if num(t, body["sigVersion"]) != 2 || body["account"] != ownerAddr || body["agent"] != agentAddr || num(t, body["nonce"]) != nonce ||
		body["subaccountMask"] != nil || body["validUntil"] != nil || body["signer"] != nil || body["subaccount"] != nil {
		t.Fatalf("v2 撤销的请求体: %v", body)
	}
	if recoverAddr(t, testDomain.ApproveAgentScopeHash(owner.Address(), agent, 0, 0, nonce), body["signature"].(string)) != ownerAddr {
		t.Fatal("网关恢复不出主钱包")
	}
	if r.Seq != 512 || r.Request.Scope != "signer:"+ownerAddr || r.Request.Nonce != nonce {
		t.Fatalf("回执: %+v / %+v", r, r.Request)
	}

	sent := f.count()
	for name, bad := range map[string]SignedAgentScopeRevoke{
		"签名格式":       {Owner: owner.Address(), Domain: testDomain, Agent: agent, Nonce: nonce, Signature: "0x12"},
		"撤销主钱包自己":    {Owner: owner.Address(), Domain: testDomain, Agent: owner.Address(), Nonce: nonce, Signature: sig},
		"API 钱包地址为空": {Owner: owner.Address(), Domain: testDomain, Nonce: nonce, Signature: sig},
	} {
		if _, err := c.SubmitAgentScopeRevoke(context.Background(), bad); err == nil {
			t.Errorf("%s: 应在本地报错", name)
		}
	}
	if f.count() != sent {
		t.Fatal("本地校验失败不该发请求")
	}
	f.answerRaw(t, "/v1/account/agents/revoke", 200, []byte(strings.Replace(string(fixture(t, "w_revoke.json")), `"revoked":true`, `"revoked":false`, 1)))
	if _, err := c.SubmitAgentScopeRevoke(context.Background(), rv); err == nil {
		t.Fatal("回执没说撤掉应报错")
	}
	// 位图 0 不走授权入口:网关的 v2 approve 回 400 让改走撤销
	a := SignedAgentScope{Owner: owner.Address(), Domain: testDomain, Agent: agent, Nonce: nonce, Signature: sig}
	if _, err := c.SubmitAgentScope(context.Background(), a); err == nil || !strings.Contains(err.Error(), "SubmitAgentScopeRevoke") {
		t.Fatalf("位图 0 的授权应指向撤销入口: %v", err)
	}
}
