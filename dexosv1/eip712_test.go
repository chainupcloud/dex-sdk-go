package dexosv1

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 金样由 ethers 的 TypedDataEncoder / Wallet.signTypedData 生成(生成脚本不入库,做法见 testdata/README.md),
// 与这里的 Go 实现互相独立:字段顺序、word 编码、域分隔符任何一处不同,摘要就对不上。
func TestEIP712GoldenVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "eip712_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Signer     string `json:"signer"`
		PrivateKey string `json:"privateKey"`
		Vectors    []struct {
			Type   string `json:"type"`
			Domain struct {
				ChainID           uint64 `json:"chainId"`
				VerifyingContract string `json:"verifyingContract"`
			} `json:"domain"`
			Message   map[string]any `json:"message"`
			Digest    string         `json:"digest"`
			Signature string         `json:"signature"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	signer, err := dexos.NewSigner(g.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if signer.Address().Hex() != g.Signer {
		t.Fatalf("金样签名者 %s,私钥推出 %s", g.Signer, signer.Address().Hex())
	}
	types := map[string]bool{}
	for i, v := range g.Vectors {
		types[v.Type] = true
		d := dexos.Domain{Name: "dex-os", Version: "1", ChainID: v.Domain.ChainID, VerifyingContract: mustAddr(t, v.Domain.VerifyingContract)}
		u := func(k string) uint64 {
			n, err := strconv.ParseUint(v.Message[k].(string), 10, 64)
			if err != nil {
				t.Fatalf("向量 %d 字段 %s: %v", i, k, err)
			}
			return n
		}
		trader := mustAddr(t, v.Message["trader"].(string))
		var h []byte
		switch v.Type {
		case "Order":
			h = orderHash(d, trader, uint32(u("accountId")), uint16(u("market")), dexos.Side(u("side")),
				uint32(u("price")), u("lots"), dexos.TimeInForce(u("tif")), v.Message["reduceOnly"].(bool), u("nonce"), u("expiry"))
		case "Cancel":
			h = cancelHash(d, trader, uint32(u("accountId")), uint16(u("market")), u("orderSeq"), u("nonce"))
		case "Leverage":
			h = leverageHash(d, trader, uint32(u("accountId")), uint16(u("market")), uint32(u("customImfPpm")), u("nonce"))
		default:
			t.Fatalf("未知类型 %s", v.Type)
		}
		if got := "0x" + hex.EncodeToString(h); got != v.Digest {
			t.Errorf("向量 %d %s(chainId %d)摘要 %s,金样 %s", i, v.Type, v.Domain.ChainID, got, v.Digest)
			continue
		}
		sig, err := signer.SignHashHex(h)
		if err != nil {
			t.Fatal(err)
		}
		if sig != v.Signature {
			t.Errorf("向量 %d %s 签名 %s,金样 %s", i, v.Type, sig, v.Signature)
		}
	}
	for _, want := range []string{"Order", "Cancel", "Leverage"} {
		if !types[want] {
			t.Errorf("金样里没有 %s", want)
		}
	}
}

// 类型串与钉住的 OpenAPI `x-eip712.typeString` 逐字一致:服务端改了结构体而这里没跟,签名会被拒且只报 401。
func TestEIP712TypeStringsMatchPinnedSpec(t *testing.T) {
	spec := loadSpec(t)
	for path, want := range map[string]string{
		"/v1/orders":                 orderType,
		"/v1/orders/cancel":          cancelType,
		"/v1/account/leverage":       leverageType,
		"/v1/orders/replace":         "AgentExec(bytes32 commandHash,uint64 nonce)",
		"/v1/orders/cancel-all":      "AgentExec(bytes32 commandHash,uint64 nonce)",
		"/v1/orders/schedule-cancel": "AgentExec(bytes32 commandHash,uint64 nonce)",
		"/v1/account/agents/approve": "ApproveAgent(address owner,uint32 master,address agent,uint64 validUntilMs,uint64 nonce)",
	} {
		got := obj(obj(obj(obj(spec["paths"])[path])["post"])["x-eip712"])["typeString"]
		if got != want {
			t.Errorf("POST %s 的 typeString 是 %v,本包签 %s", path, got, want)
		}
	}
}
