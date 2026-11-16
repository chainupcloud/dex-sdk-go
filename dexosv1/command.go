package dexosv1

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/chainupcloud/dex-sdk-go/dexos"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// Hash 32 字节哈希(networkId、actionId、grantId、签名摘要等)。
type Hash [32]byte

func (h Hash) Hex() string { return "0x" + hex.EncodeToString(h[:]) }

func (h Hash) String() string { return h.Hex() }

func (h Hash) IsZero() bool { return h == Hash{} }

// ParseHash 解析 0x 前缀的 64 位 hex。
func ParseHash(s string) (Hash, error) {
	var h Hash
	raw, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(raw) != 32 {
		return h, fmt.Errorf("dexosv1: %q 不是 32 字节 hex", s)
	}
	copy(h[:], raw)
	return h, nil
}

func (h *Hash) UnmarshalText(b []byte) error {
	v, err := ParseHash(string(b))
	if err != nil {
		return err
	}
	*h = v
	return nil
}

func (h Hash) MarshalText() ([]byte, error) { return []byte(h.Hex()), nil }

func keccak(parts ...[]byte) Hash {
	var h Hash
	copy(h[:], dexos.Keccak256(parts...))
	return h
}

func textHash(s string) Hash { return keccak([]byte(s)) }

// Lane 签名主体的 nonce 车道。车道由内层命令的业务类决定,选错账本拒 WrongAuthorizationDomain:
// 交易与风险命令走 Trade,动钱走 Funds,授权、开子账户等管理命令走 Management。
type Lane string

const (
	LaneTrade      Lane = "Trade"
	LaneFunds      Lane = "Funds"
	LaneManagement Lane = "Management"
)

func (l Lane) query() string { return strings.ToLower(string(l)) }

// PrincipalKind 139 的签名主体种类。Account = 账户根(主钱包,账户策略的成员);Agent = 持 grant 的代理(API 钱包)。
type PrincipalKind string

const (
	PrincipalAccount PrincipalKind = "Account"
	PrincipalAgent   PrincipalKind = "Agent"
)

// Principal 139 的签名主体。Policy 是账户策略号(代理填签发 grant 的那个策略),Epoch 是策略当前纪元;
// 账户根的 GrantID / GrantEpoch 必须为零,代理必须带上导入时拿到的 grant。
type Principal struct {
	Kind       PrincipalKind
	Policy     uint64
	Epoch      uint64
	GrantID    Hash
	GrantEpoch uint64
}

// AccountRoot 账户根主体。
func AccountRoot(policy, epoch uint64) Principal {
	return Principal{Kind: PrincipalAccount, Policy: policy, Epoch: epoch}
}

// AgentOf 代理主体。
func AgentOf(policy, epoch uint64, grant Hash, grantEpoch uint64) Principal {
	return Principal{Kind: PrincipalAgent, Policy: policy, Epoch: epoch, GrantID: grant, GrantEpoch: grantEpoch}
}

func (p Principal) check() error {
	switch p.Kind {
	case PrincipalAccount:
		if !p.GrantID.IsZero() || p.GrantEpoch != 0 {
			return fmt.Errorf("dexosv1: 账户根主体不能带 grant")
		}
	case PrincipalAgent:
		if p.GrantID.IsZero() {
			return fmt.Errorf("dexosv1: 代理主体必须带 grant")
		}
	default:
		return fmt.Errorf("dexosv1: 不支持的主体 %q", p.Kind)
	}
	return nil
}

// Network 签名要钉住的网络身份,读自 GET /v1/security/network。账本换代后这几项都会变,
// 旧网络上签的命令带着 ManifestHash 发出去会被网关以 503 upstream_manifest_mismatch 拒。
type Network struct {
	NetworkID    Hash
	GenesisHash  Hash
	LedgerEpoch  uint64
	ChainID      uint64
	Verifier     dexos.Address
	ManifestHash Hash
}

// Authorization 一条 139 的授权参数。ValidUntilMs 按账本认证时间判(GET /v1/security/time),
// 过期的原件永远不会被受理。
type Authorization struct {
	Principal    Principal
	Lane         Lane
	Nonce        uint64
	ValidUntilMs uint64
}

// Action 签名前的 139:actionId 与 EIP-712 摘要在这里就已确定。
type Action struct {
	Network           Network
	Auth              Authorization
	Payload           []byte
	PayloadHash       Hash
	PreconditionsHash Hash
	ActionID          Hash
	Digest            Hash
}

// Command 签好名、可以提交到 POST /v1/commands 的 139(或 179)。ActionID 就是回执查询用的 requestId。
type Command struct {
	Bytes        []byte
	ActionID     Hash
	Digest       Hash
	ManifestHash Hash
	ValidUntilMs uint64
}

func (c Command) Hex() string { return "0x" + hex.EncodeToString(c.Bytes) }

const (
	tagAuthenticatedCommand byte = 0x8B
	actionIDDomain               = "DEXOS_ACTION_ID_V1"
	authorizedActionType         = "AuthorizedAction(bytes32 networkId,bytes32 genesisHash,uint64 ledgerEpoch,bytes32 principalKind,bytes32 principalId,uint64 authorityEpoch,bytes32 grantId,uint64 grantEpoch,bytes32 nonceLane,uint64 nonce,bytes32 actionId,bytes32 payloadHash,bytes32 resourcePreconditionsHash,uint64 validAfterMs,uint64 validUntilMs)"
)

var emptyPreconditions = keccak(word(32), word(0))

// Action 组装一条待签的 139。payload 是内层命令的完整规范编码(以编码版本与 tag 开头)。
func (n Network) Action(a Authorization, payload []byte) (Action, error) {
	if err := a.Principal.check(); err != nil {
		return Action{}, err
	}
	switch a.Lane {
	case LaneTrade, LaneFunds, LaneManagement:
	default:
		return Action{}, fmt.Errorf("dexosv1: 未知车道 %q", a.Lane)
	}
	if len(payload) < 2 {
		return Action{}, fmt.Errorf("dexosv1: 内层命令为空")
	}
	act := Action{Network: n, Auth: a, Payload: append([]byte(nil), payload...), PayloadHash: keccak(payload), PreconditionsHash: emptyPreconditions}
	p := a.Principal
	act.ActionID = keccak(textHash(actionIDDomain).bytes(), n.NetworkID[:], n.GenesisHash[:], word(n.LedgerEpoch),
		textHash(string(p.Kind)).bytes(), word(p.Policy), word(p.Epoch), p.GrantID[:], word(p.GrantEpoch),
		textHash(string(a.Lane)).bytes(), word(a.Nonce), act.PayloadHash[:], act.PreconditionsHash[:])
	act.Digest = act.digest()
	return act, nil
}

func (h Hash) bytes() []byte { return h[:] }

func (a Action) domain() dexos.Domain {
	return dexos.Domain{Name: "DEXOS/" + string(a.Auth.Principal.Kind), Version: "1", ChainID: a.Network.ChainID, VerifyingContract: a.Network.Verifier}
}

func (a Action) digest() Hash {
	n, p := a.Network, a.Auth.Principal
	structHash := dexos.Keccak256(dexos.Keccak256([]byte(authorizedActionType)),
		n.NetworkID[:], n.GenesisHash[:], word(n.LedgerEpoch),
		textHash(string(p.Kind)).bytes(), word(p.Policy), word(p.Epoch), p.GrantID[:], word(p.GrantEpoch),
		textHash(string(a.Auth.Lane)).bytes(), word(a.Auth.Nonce), a.ActionID[:], a.PayloadHash[:], a.PreconditionsHash[:],
		word(0), word(a.Auth.ValidUntilMs))
	var h Hash
	copy(h[:], signingHash(a.domain(), structHash))
	return h
}

var authorizedActionFields = func() []dexos.TypedField {
	body := strings.TrimSuffix(strings.TrimPrefix(authorizedActionType, "AuthorizedAction("), ")")
	var out []dexos.TypedField
	for _, f := range strings.Split(body, ",") {
		t, name, _ := strings.Cut(f, " ")
		out = append(out, dexos.TypedField{Name: name, Type: t})
	}
	return out
}()

var typedDomainFields = []dexos.TypedField{
	{Name: "name", Type: "string"}, {Name: "version", Type: "string"},
	{Name: "chainId", Type: "uint256"}, {Name: "verifyingContract", Type: "address"},
}

// TypedData 交给浏览器钱包 eth_signTypedData_v4 的结构;钱包签出的就是 [Action.Digest]。
func (a Action) TypedData() dexos.TypedData {
	n, p := a.Network, a.Auth.Principal
	u := func(v uint64) string { return strconv.FormatUint(v, 10) }
	d := a.domain()
	return dexos.TypedData{
		Types:       map[string][]dexos.TypedField{"EIP712Domain": typedDomainFields, "AuthorizedAction": authorizedActionFields},
		PrimaryType: "AuthorizedAction",
		Domain:      dexos.TypedDomain{Name: d.Name, Version: d.Version, ChainID: d.ChainID, VerifyingContract: d.VerifyingContract.Hex()},
		Message: map[string]any{
			"networkId": n.NetworkID.Hex(), "genesisHash": n.GenesisHash.Hex(), "ledgerEpoch": u(n.LedgerEpoch),
			"principalKind": textHash(string(p.Kind)).Hex(), "principalId": Hash(word32(p.Policy)).Hex(), "authorityEpoch": u(p.Epoch),
			"grantId": p.GrantID.Hex(), "grantEpoch": u(p.GrantEpoch), "nonceLane": textHash(string(a.Auth.Lane)).Hex(),
			"nonce": u(a.Auth.Nonce), "actionId": a.ActionID.Hex(), "payloadHash": a.PayloadHash.Hex(),
			"resourcePreconditionsHash": a.PreconditionsHash.Hex(), "validAfterMs": "0", "validUntilMs": u(a.Auth.ValidUntilMs),
		},
	}
}

func word32(v uint64) [32]byte {
	var w [32]byte
	copy(w[:], word(v))
	return w
}

// Sign 用本地私钥签名并组装成可提交的命令。
func (a Action) Sign(s *dexos.Signer) (Command, error) {
	sig, err := s.SignHash(a.Digest[:])
	if err != nil {
		return Command{}, err
	}
	return a.Complete(sig[:], s.Address())
}

// Complete 用外部(如浏览器钱包)签好的 65 字节签名组装命令:v 归一到 27/28、s 归一到低半区,
// 恢复出的地址必须是 signer,否则报错(钱包连错账户时在这里就能发现)。
func (a Action) Complete(signature []byte, signer dexos.Address) (Command, error) {
	sig, err := normalizeSignature(a.Digest, signature, signer)
	if err != nil {
		return Command{}, err
	}
	n, p := a.Network, a.Auth.Principal
	b := make([]byte, 0, 2+32*9+8*5+4+len(a.Payload)+4+65+4)
	b = append(b, 0x01, tagAuthenticatedCommand)
	b = append(b, n.NetworkID[:]...)
	b = append(b, n.GenesisHash[:]...)
	b = le64(b, n.LedgerEpoch)
	b = append(b, textHash(string(p.Kind)).bytes()...)
	w := word32(p.Policy)
	b = append(b, w[:]...)
	b = le64(b, p.Epoch)
	b = append(b, p.GrantID[:]...)
	b = le64(b, p.GrantEpoch)
	b = append(b, textHash(string(a.Auth.Lane)).bytes()...)
	b = le64(b, a.Auth.Nonce)
	b = append(b, a.ActionID[:]...)
	b = append(b, a.PayloadHash[:]...)
	b = append(b, a.PreconditionsHash[:]...)
	b = le64(b, 0)
	b = le64(b, a.Auth.ValidUntilMs)
	b = le32(b, uint32(len(a.Payload)))
	b = append(b, a.Payload...)
	b = le32(b, 1)
	b = append(b, sig[:]...)
	b = le32(b, 0)
	return Command{Bytes: b, ActionID: a.ActionID, Digest: a.Digest, ManifestHash: n.ManifestHash, ValidUntilMs: a.Auth.ValidUntilMs}, nil
}

func le64(b []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(b, v) }
func le32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }
func le16(b []byte, v uint16) []byte { return binary.LittleEndian.AppendUint16(b, v) }

func normalizeSignature(digest Hash, signature []byte, signer dexos.Address) ([65]byte, error) {
	var out [65]byte
	if len(signature) != 65 {
		return out, fmt.Errorf("dexosv1: 签名应为 65 字节,得到 %d", len(signature))
	}
	v := signature[64]
	if v >= 27 {
		v -= 27
	}
	if v > 1 {
		return out, fmt.Errorf("dexosv1: 签名的 v 值 %d 非法", signature[64])
	}
	var r, s secp256k1.ModNScalar
	if r.SetByteSlice(signature[:32]) || s.SetByteSlice(signature[32:64]) || r.IsZero() || s.IsZero() {
		return out, fmt.Errorf("dexosv1: 签名的 r / s 越界")
	}
	if s.IsOverHalfOrder() {
		s.Negate()
		v ^= 1
	}
	rb, sb := r.Bytes(), s.Bytes()
	compact := make([]byte, 65)
	compact[0] = 27 + v
	copy(compact[1:33], rb[:])
	copy(compact[33:65], sb[:])
	pub, _, err := ecdsa.RecoverCompact(compact, digest[:])
	if err != nil {
		return out, fmt.Errorf("dexosv1: 签名恢复失败: %w", err)
	}
	if got := pubAddress(pub); got != signer {
		return out, fmt.Errorf("dexosv1: 签名者是 %s,应为 %s", got.Hex(), signer.Hex())
	}
	copy(out[:32], rb[:])
	copy(out[32:64], sb[:])
	out[64] = 27 + v
	return out, nil
}

func pubAddress(pub *secp256k1.PublicKey) dexos.Address {
	var a dexos.Address
	copy(a[:], dexos.Keccak256(pub.SerializeUncompressed()[1:])[12:])
	return a
}
