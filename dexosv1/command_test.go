package dexosv1

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

func vectorNetwork(t *testing.T) Network {
	t.Helper()
	verifier, err := dexos.ParseAddress("0x" + strings.Repeat("03", 20))
	if err != nil {
		t.Fatal(err)
	}
	return Network{
		NetworkID:   mustHash(t, "0x"+strings.Repeat("01", 32)),
		GenesisHash: mustHash(t, "0x"+strings.Repeat("02", 32)),
		LedgerEpoch: 1,
		ChainID:     123,
		Verifier:    verifier,
	}
}

func mustHash(t *testing.T, s string) Hash {
	t.Helper()
	h, err := ParseHash(s)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func scalarSigner(t *testing.T, scalar uint64) *dexos.Signer {
	t.Helper()
	s, err := dexos.NewSigner(fmt.Sprintf("%064x", scalar))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func readVectors(t *testing.T, name string, out any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/security/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
}

type businessVector struct {
	Name           string   `json:"name"`
	PrincipalKind  string   `json:"principalKind"`
	PrincipalID    uint64   `json:"principalId"`
	AuthorityEpoch uint64   `json:"authorityEpoch"`
	GrantID        string   `json:"grantId"`
	GrantEpoch     uint64   `json:"grantEpoch"`
	Lane           string   `json:"lane"`
	Nonce          uint64   `json:"nonce"`
	ValidUntilMs   uint64   `json:"validUntilMs"`
	SignerScalars  []uint64 `json:"signerScalars"`
	Threshold      int      `json:"threshold"`
	ActionID       string   `json:"actionId"`
	Digest         string   `json:"digest"`
	PayloadHex     string   `json:"payloadHex"`
	CommandHex     string   `json:"commandHex"`
}

func TestAuthenticatedCommandMatchesDexOSBusinessVectors(t *testing.T) {
	var file struct {
		Cases []businessVector `json:"cases"`
	}
	readVectors(t, "business-auth-vectors.json", &file)
	net := vectorNetwork(t)
	seen := 0
	for _, c := range file.Cases {
		if c.PrincipalKind == "Service" {
			continue
		}
		seen++
		t.Run(c.Name, func(t *testing.T) {
			p := Principal{Kind: PrincipalKind(c.PrincipalKind), Policy: c.PrincipalID, Epoch: c.AuthorityEpoch, GrantID: mustHash(t, c.GrantID), GrantEpoch: c.GrantEpoch}
			act, err := net.Action(Authorization{Principal: p, Lane: Lane(c.Lane), Nonce: c.Nonce, ValidUntilMs: c.ValidUntilMs}, mustHex(t, c.PayloadHex))
			if err != nil {
				t.Fatal(err)
			}
			if act.ActionID.Hex() != c.ActionID || act.Digest.Hex() != c.Digest {
				t.Fatalf("actionId / digest 与金样不一致:\n got %s %s\nwant %s %s", act.ActionID.Hex(), act.Digest.Hex(), c.ActionID, c.Digest)
			}
			cmd, err := act.Sign(scalarSigner(t, c.SignerScalars[0]))
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Hex() != c.CommandHex {
				t.Fatalf("139 编码与金样不一致:\n got %s\nwant %s", cmd.Hex(), c.CommandHex)
			}
		})
	}
	if seen != 2 {
		t.Fatalf("金样里账户根与代理用例应为 2 条,读到 %d", seen)
	}
}

func TestPlaceOrderEncodingMatchesDexOSBusinessVector(t *testing.T) {
	var file struct {
		Cases []businessVector `json:"cases"`
	}
	readVectors(t, "business-auth-vectors.json", &file)
	got := hex.EncodeToString(dexos.EncodePlaceOrder(2, 0, dexos.Sell, 50000, 1, dexos.GTC, true, 0, 0, 0, 0))
	if "0x"+got != file.Cases[1].PayloadHex {
		t.Fatalf("PlaceOrder 内层编码与金样不一致:\n got 0x%s\nwant %s", got, file.Cases[1].PayloadHex)
	}
}

func TestWalletSignatureIsNormalizedAndCheckedAgainstTheSigner(t *testing.T) {
	net := vectorNetwork(t)
	agent := scalarSigner(t, 50)
	act, err := net.Action(Authorization{Principal: AgentOf(1, 1, mustHash(t, "0x"+strings.Repeat("55", 32)), 1), Lane: LaneTrade, Nonce: 7, ValidUntilMs: 10000},
		dexos.EncodeBatchCancel(2, []dexos.OrderID{dexos.NewOrderID(0, 9)}))
	if err != nil {
		t.Fatal(err)
	}
	want, err := act.Sign(agent)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := agent.SignHash(act.Digest[:])
	if err != nil {
		t.Fatal(err)
	}
	walletStyle := append([]byte(nil), sig[:]...)
	walletStyle[64] -= 27
	got, err := act.Complete(walletStyle, agent.Address())
	if err != nil {
		t.Fatal(err)
	}
	if got.Hex() != want.Hex() {
		t.Fatal("v 为 0/1 的钱包签名应归一成同一份原件")
	}
	if _, err := act.Complete(sig[:], scalarSigner(t, 51).Address()); err == nil {
		t.Fatal("签名者不符应报错")
	}
}

func TestAuthenticatedCommandRejectsMismatchedPrincipals(t *testing.T) {
	net := vectorNetwork(t)
	payload := dexos.EncodeBatchCancel(2, nil)
	if _, err := net.Action(Authorization{Principal: Principal{Kind: PrincipalAgent, Policy: 1, Epoch: 1}, Lane: LaneTrade}, payload); err == nil {
		t.Fatal("代理不带 grant 应拒")
	}
	if _, err := net.Action(Authorization{Principal: Principal{Kind: PrincipalAccount, Policy: 1, Epoch: 1, GrantEpoch: 1}, Lane: LaneTrade}, payload); err == nil {
		t.Fatal("账户根带 grant 应拒")
	}
	if _, err := net.Action(Authorization{Principal: AccountRoot(1, 1), Lane: "trade"}, payload); err == nil {
		t.Fatal("车道名大小写不对应拒")
	}
}

type accountActionVector struct {
	Name         string          `json:"name"`
	Lane         string          `json:"lane"`
	Nonce        uint64          `json:"nonce"`
	ValidUntilMs uint64          `json:"validUntilMs"`
	Inner        json.RawMessage `json:"inner"`
	GrantID      string          `json:"grantId"`
	SignerScalar uint64          `json:"signerScalar"`
	ActionID     string          `json:"actionId"`
	Digest       string          `json:"digest"`
	PayloadHex   string          `json:"payloadHex"`
	CommandHex   string          `json:"commandHex"`
}

func TestGrantAndRevokeMatchDexOSAccountActionVectors(t *testing.T) {
	var file struct {
		Cases []accountActionVector `json:"cases"`
	}
	readVectors(t, "account-actions-vectors.json", &file)
	net := vectorNetwork(t)
	byName := map[string]accountActionVector{}
	for _, c := range file.Cases {
		byName[c.Name] = c
	}
	grant, revoke := byName["account-root-grant-agent"], byName["account-root-revoke-agent"]
	var in struct {
		GrantID             string   `json:"grantId"`
		Salt                string   `json:"salt"`
		Agent               string   `json:"agent"`
		Actions             []uint16 `json:"actions"`
		Accounts            []uint32 `json:"accounts"`
		ExpiresAtMs         uint64   `json:"expiresAtMs"`
		ExpectedIssuerEpoch uint64   `json:"expectedIssuerEpoch"`
		Limits              struct {
			WindowMs uint64 `json:"windowMs"`
			Markets  []struct {
				Market            uint16 `json:"market"`
				MarketSpecVersion uint64 `json:"marketSpecVersion"`
				MinTick           uint32 `json:"minTick"`
				MaxTick           uint32 `json:"maxTick"`
			} `json:"markets"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(grant.Inner, &in); err != nil {
		t.Fatal(err)
	}
	agent, err := dexos.ParseAddress(in.Agent)
	if err != nil {
		t.Fatal(err)
	}
	g := GrantSpec{Salt: mustHash(t, in.Salt), Agent: agent, Actions: in.Actions, Accounts: in.Accounts, ExpiresAtMs: in.ExpiresAtMs,
		ExpectedIssuerEpoch: in.ExpectedIssuerEpoch, Limits: GrantLimits{WindowMs: in.Limits.WindowMs}}
	for _, m := range in.Limits.Markets {
		g.Limits.Markets = append(g.Limits.Markets, GrantMarket{Market: m.Market, SpecVersion: m.MarketSpecVersion, MinTick: m.MinTick, MaxTick: m.MaxTick})
	}
	payload, id, err := net.EncodeGrantCapability(1, 1, g)
	if err != nil {
		t.Fatal(err)
	}
	if id.Hex() != grant.GrantID || "0x"+hex.EncodeToString(payload) != grant.PayloadHex {
		t.Fatalf("141 与金样不一致:\n got %s 0x%x\nwant %s %s", id.Hex(), payload, grant.GrantID, grant.PayloadHex)
	}
	owner := scalarSigner(t, 40)
	for _, c := range []struct {
		v       accountActionVector
		payload []byte
	}{{grant, payload}, {revoke, EncodeRevokeCapability(id, 1)}} {
		act, err := net.Action(Authorization{Principal: AccountRoot(1, 1), Lane: Lane(c.v.Lane), Nonce: c.v.Nonce, ValidUntilMs: c.v.ValidUntilMs}, c.payload)
		if err != nil {
			t.Fatal(err)
		}
		cmd, err := act.Sign(owner)
		if err != nil {
			t.Fatal(err)
		}
		if cmd.ActionID.Hex() != c.v.ActionID || cmd.Digest.Hex() != c.v.Digest || cmd.Hex() != c.v.CommandHex {
			t.Fatalf("%s 与金样不一致", c.v.Name)
		}
	}
}

type pairedLegVector struct {
	PrincipalKind  string `json:"principalKind"`
	PrincipalID    uint64 `json:"principalId"`
	AuthorityEpoch uint64 `json:"authorityEpoch"`
	GrantID        string `json:"grantId"`
	GrantEpoch     uint64 `json:"grantEpoch"`
	Nonce          uint64 `json:"nonce"`
	ValidUntilMs   uint64 `json:"validUntilMs"`
	SignerScalar   uint64 `json:"signerScalar"`
	ActionID       string `json:"actionId"`
	SignedLegHex   string `json:"signedLegHex"`
}

func TestPairedTradeMatchesDexOSPairedVectors(t *testing.T) {
	var file struct {
		Cases []struct {
			Name string `json:"name"`
			Spec struct {
				MakerAccount uint32 `json:"makerAccount"`
				TakerAccount uint32 `json:"takerAccount"`
				Market       uint16 `json:"market"`
				MakerSide    string `json:"makerSide"`
				Price        uint32 `json:"price"`
				Lots         uint64 `json:"lots"`
			} `json:"spec"`
			PayloadHex string          `json:"payloadHex"`
			Maker      pairedLegVector `json:"maker"`
			Taker      pairedLegVector `json:"taker"`
			RequestID  string          `json:"requestId"`
			CommandHex string          `json:"commandHex"`
		} `json:"cases"`
	}
	readVectors(t, "paired-trade-vectors.json", &file)
	if len(file.Cases) != 2 {
		t.Fatalf("成对成交金样应为 2 条,读到 %d", len(file.Cases))
	}
	net := vectorNetwork(t)
	for _, c := range file.Cases {
		t.Run(c.Name, func(t *testing.T) {
			side := dexos.Buy
			if c.Spec.MakerSide == "sell" {
				side = dexos.Sell
			}
			payload := EncodePairedTradeSpec(c.Spec.MakerAccount, c.Spec.TakerAccount, c.Spec.Market, side, c.Spec.Price, c.Spec.Lots)
			if "0x"+hex.EncodeToString(payload) != c.PayloadHex {
				t.Fatalf("0xD2 载荷与金样不一致: 0x%x", payload)
			}
			leg := func(v pairedLegVector) Command {
				p := Principal{Kind: PrincipalKind(v.PrincipalKind), Policy: v.PrincipalID, Epoch: v.AuthorityEpoch, GrantID: mustHash(t, v.GrantID), GrantEpoch: v.GrantEpoch}
				act, err := net.Action(Authorization{Principal: p, Lane: LaneTrade, Nonce: v.Nonce, ValidUntilMs: v.ValidUntilMs}, payload)
				if err != nil {
					t.Fatal(err)
				}
				cmd, err := act.Sign(scalarSigner(t, v.SignerScalar))
				if err != nil {
					t.Fatal(err)
				}
				if cmd.Hex() != v.SignedLegHex {
					t.Fatalf("单腿 139 与金样不一致")
				}
				return cmd
			}
			paired, err := PairLegs(leg(c.Maker), leg(c.Taker))
			if err != nil {
				t.Fatal(err)
			}
			if paired.Hex() != c.CommandHex || paired.ActionID.Hex() != c.RequestID {
				t.Fatalf("179 与金样不一致:\n got %s\nwant %s", paired.Hex(), c.CommandHex)
			}
		})
	}
}

func TestDecodeCommandRecoversSignersAndRejectsTampering(t *testing.T) {
	var file struct {
		Cases []businessVector `json:"cases"`
	}
	readVectors(t, "business-auth-vectors.json", &file)
	net := vectorNetwork(t)
	c := file.Cases[1]
	raw := mustHex(t, c.CommandHex)
	got, err := DecodeCommand(net, raw)
	if err != nil {
		t.Fatal(err)
	}
	leg := got.Legs[0]
	if got.Paired || leg.Action.ActionID.Hex() != c.ActionID || leg.Action.Auth.Principal.Kind != PrincipalAgent || leg.Action.Auth.Nonce != 7 ||
		len(leg.Signers) != 1 || leg.Signers[0] != scalarSigner(t, 50).Address() || "0x"+hex.EncodeToString(leg.Action.Payload) != c.PayloadHex {
		t.Fatalf("解码结果不对: %+v", got)
	}
	tampered := append([]byte(nil), raw...)
	tampered[len(tampered)-80] ^= 1
	if _, err := DecodeCommand(net, tampered); err == nil {
		t.Fatal("改了载荷的命令应拒")
	}
	other := net
	other.LedgerEpoch = 2
	if _, err := DecodeCommand(other, raw); err == nil {
		t.Fatal("别的网络上签的命令应拒")
	}
	var paired struct {
		Cases []struct {
			CommandHex string `json:"commandHex"`
			RequestID  string `json:"requestId"`
		} `json:"cases"`
	}
	readVectors(t, "paired-trade-vectors.json", &paired)
	p, err := DecodeCommand(net, mustHex(t, paired.Cases[1].CommandHex))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Paired || len(p.Legs) != 2 || p.Legs[0].Action.ActionID.Hex() != paired.Cases[1].RequestID || p.Legs[1].Action.Auth.Principal.Kind != PrincipalAgent {
		t.Fatalf("179 解码不对: %+v", p)
	}
}

func TestAnalyticsEpochMatchesDexOSNamespace(t *testing.T) {
	fill := func(b byte) Hash {
		var h Hash
		for i := range h {
			h[i] = b
		}
		return h
	}
	for _, c := range []struct {
		net  Network
		want string
	}{
		{Network{NetworkID: fill(0x11), GenesisHash: fill(0x22), LedgerEpoch: 1}, "sec1:5f436038314c7044dab7c093e2dab4b5bd492baac3eeec3a44f895c754395b10"},
		{Network{NetworkID: fill(0x11), GenesisHash: fill(0x22), LedgerEpoch: 256}, "sec1:f8f02546e0fab624db26b7b0a1529677f4257a2d2c9c1b46b219db080d853893"},
	} {
		if got := c.net.AnalyticsEpoch(); got != c.want {
			t.Fatalf("纪元 %d: got %s want %s", c.net.LedgerEpoch, got, c.want)
		}
	}
}
