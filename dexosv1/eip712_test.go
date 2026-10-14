package dexosv1

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
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
			Message map[string]any `json:"message"`
			Inputs  struct {
				DepositorWhitelist []string `json:"depositorWhitelist"`
				Markets            []uint16 `json:"markets"`
			} `json:"inputs"`
			Digest    string `json:"digest"`
			Signature string `json:"signature"`
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
		n128 := func(k string) *big.Int {
			n, ok := new(big.Int).SetString(v.Message[k].(string), 10)
			if !ok {
				t.Fatalf("向量 %d 字段 %s 不是整数", i, k)
			}
			return n
		}
		addr := func(k string) dexos.Address { return mustAddr(t, v.Message[k].(string)) }
		// 白名单 / 市场摘要由生成脚本独立算出,这里按原始输入重算核对
		bytes32 := func(k string, got []byte) []byte {
			if want := v.Message[k].(string); "0x"+hex.EncodeToString(got) != want {
				t.Errorf("向量 %d %s:%s 本包算出 %x,金样 %s", i, v.Type, k, got, want)
			}
			return got
		}
		var h []byte
		switch v.Type {
		case "Order":
			h = orderHash(d, addr("trader"), uint32(u("accountId")), uint16(u("market")), dexos.Side(u("side")),
				uint32(u("price")), u("lots"), dexos.TimeInForce(u("tif")), v.Message["reduceOnly"].(bool), u("nonce"), u("expiry"))
		case "Cancel":
			h = cancelHash(d, addr("trader"), uint32(u("accountId")), uint16(u("market")), u("orderSeq"), u("nonce"))
		case "Leverage":
			h = leverageHash(d, addr("trader"), uint32(u("accountId")), uint16(u("market")), uint32(u("customImfPpm")), u("nonce"))
		case "CreateManagedVault":
			mv := managedVault{owner: addr("owner"), accountID: uint32(u("accountId")), manager: addr("manager"),
				profitSharePpm: uint32(u("profitSharePpm")), depositMinDurationMs: u("depositMinDurationMs"),
				managerMinPortionPpm: uint32(u("managerMinPortionPpm")), managerLiqPortionPpm: uint32(u("managerLiqPortionPpm")),
				withdrawWindowS: uint32(u("withdrawWindowS")), withdrawDurationS: uint32(u("withdrawDurationS")),
				depositCap: n128("depositCap"), seedAmount: n128("seedAmount"), nonce: u("nonce")}
			for _, a := range v.Inputs.DepositorWhitelist {
				mv.depositorWhitelist = append(mv.depositorWhitelist, mustAddr(t, a))
			}
			bytes32("depositorWhitelistHash", depositorWhitelistHash(mv.depositorWhitelist))
			h = createManagedVaultHash(d, mv)
		case "VaultDeposit":
			h = vaultAmountHash(d, vaultDepositType, addr("trader"), uint16(u("vault")), uint32(u("accountId")), n128("amount"), u("nonce"))
		case "VaultRedeem":
			h = vaultAmountHash(d, vaultRedeemType, addr("trader"), uint16(u("vault")), uint32(u("accountId")), n128("shares"), u("nonce"))
		case "ClaimVaultManager":
			h = vaultAmountHash(d, claimVaultManagerType, addr("trader"), uint16(u("vault")), uint32(u("accountId")), n128("amount"), u("nonce"))
		case "VaultConfigChange":
			markets := make([]byte, 32)
			if kind := u("kind"); kind != 0 && kind != 3 {
				markets = marketsHash(v.Inputs.Markets)
			}
			h = vaultConfigChangeHash(d, addr("owner"), uint16(u("vault")), uint32(u("accountId")), uint8(u("kind")), n128("cap"),
				uint32(u("maxLeverage")), bytes32("marketsHash", markets), u("nonce"))
		case "RevokeAgent":
			h = d.RevokeAgentHash(addr("owner"), uint32(u("master")), addr("agent"), u("nonce"))
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
	for _, want := range []string{"Order", "Cancel", "Leverage", "CreateManagedVault", "VaultDeposit", "VaultRedeem",
		"ClaimVaultManager", "VaultConfigChange", "RevokeAgent"} {
		if !types[want] {
			t.Errorf("金样里没有 %s", want)
		}
	}
}

// typeStringDeviations 钉住的规格里写错的类型串(本包按 dex-os crates/auth 的实际结构体签):路径 → 规格里的错写法。
// 规格改好、同步副本后测试会红,提醒删掉条目。
//   - /v1/vaults:规格写 `uint64 withdrawWindowS,uint64 withdrawDurationS`,auth 的 CREATE_MANAGED_VAULT_TYPE 是 uint32
//     (旧 web/openapi.json 也是 uint32);按规格签会被 401。
var typeStringDeviations = map[string]string{
	"/v1/vaults": "CreateManagedVault(address owner,uint32 accountId,address manager,uint32 profitSharePpm,uint64 depositMinDurationMs,uint32 managerMinPortionPpm,uint32 managerLiqPortionPpm,uint64 withdrawWindowS,uint64 withdrawDurationS,uint128 depositCap,uint128 seedAmount,bytes32 depositorWhitelistHash,uint64 nonce)",
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
		"/v1/account/agents/revoke":  "RevokeAgent(address owner,uint32 master,address agent,uint64 nonce)",
		"/v1/vaults":                 createManagedVaultType,
		"/v1/vaults/{id}/deposit":    vaultDepositType,
		"/v1/vaults/{id}/redeem":     vaultRedeemType,
		"/v1/vaults/{id}/claim":      claimVaultManagerType,
		"/v1/vaults/{id}/config":     vaultConfigChangeType,
	} {
		got := obj(obj(obj(obj(spec["paths"])[path])["post"])["x-eip712"])["typeString"]
		dev, known := typeStringDeviations[path]
		switch {
		case known && got == want:
			t.Errorf("POST %s 的 typeString 已经改对,从 typeStringDeviations 里删掉", path)
		case known && got != dev:
			t.Errorf("POST %s 的 typeString 是 %v,登记的错写法是 %s,本包签 %s", path, got, dev, want)
		case !known && got != want:
			t.Errorf("POST %s 的 typeString 是 %v,本包签 %s", path, got, want)
		}
	}
}
