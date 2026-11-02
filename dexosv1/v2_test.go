package dexosv1

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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

type pairedGolden struct {
	CodecVersion      uint32       `json:"codecVersion"`
	ActorPlaceholder  uint32       `json:"actorPlaceholder"`
	PairedTrades      []goldenPair `json:"pairedTrades"`
	ApproveAgentScope []struct {
		Domain         goldenDomain `json:"domain"`
		Owner          string       `json:"owner"`
		Agent          string       `json:"agent"`
		SubaccountMask uint64       `json:"subaccountMask,string"`
		ValidUntilMs   uint64       `json:"validUntilMs,string"`
		Nonce          uint64       `json:"nonce,string"`
		Digest         string       `json:"digest"`
		Signature      string       `json:"signature"`
		RequestID      string       `json:"requestId"`
	} `json:"approveAgentScope"`
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
	if len(g.PairedTrades) == 0 || len(g.ApproveAgentScope) == 0 {
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
