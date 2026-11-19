package dexosv1

import (
	"fmt"
	"math/big"
	"slices"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

const (
	tagGrantCapability  byte = 0x8D
	tagRevokeCapability byte = 0x8E
	grantIDDomain            = "DEXOS/GrantId/v1"
)

// 代理可被授予的交易类动作(命名空间 1 的命令 tag)。带 clientOrderId 的变体按基础 tag 计。
const (
	ActionPlaceOrder    uint16 = 0x05
	ActionCancelOrder   uint16 = 0x06
	ActionSetLeverage   uint16 = 0x0A
	ActionBatchCancel   uint16 = 0x0F
	ActionModifyOrder   uint16 = 0x15
	ActionBatchPlace    uint16 = 0x31
	ActionBatchReplace  uint16 = 0x3E
	ActionBatchReplace2 uint16 = 0x88
	ActionBatchReplace3 uint16 = 0xE3
	ActionPairedTrade   uint16 = 0xD2
)

// GrantMarket grant 里一个市场的限制。SpecVersion 取 GET /v1/markets 的 marketSpecVersion,
// 和账本不一致时代理的单被拒 PolicyVersionMismatch;价格须落在 [MinTick, MaxTick]。
type GrantMarket struct {
	Market      uint16
	SpecVersion uint64
	MinTick     uint32
	MaxTick     uint32
}

// GrantLimits grant 的额度。指针为 nil = 不设该项上限;Markets 不能为空。
type GrantLimits struct {
	MaxRequestCount     *uint64
	WindowMs            uint64
	MaxOrderLots        *uint64
	MaxOrderQuoteAtoms  *big.Int
	MaxWindowQuoteAtoms *big.Int
	MaxFeeQuoteAtoms    *big.Int
	Markets             []GrantMarket
}

// GrantSpec 账户根发给代理的 Accounts grant(141 GrantCapability)。
type GrantSpec struct {
	Salt                Hash
	Agent               dexos.Address
	Actions             []uint16
	Accounts            []uint32
	ExpiresAtMs         uint64
	Limits              GrantLimits
	ExpectedIssuerEpoch uint64
}

// GrantID grant 的身份,由签发策略、策略纪元与 salt 确定;签名前就能算出,用来回查 grant 是否已生效。
func (n Network) GrantID(policy, policyEpoch uint64, salt Hash) Hash {
	return keccak(textHash(grantIDDomain).bytes(), n.NetworkID[:], n.GenesisHash[:], word(n.LedgerEpoch),
		textHash(string(PrincipalAccount)).bytes(), word(policy), word(policyEpoch), salt[:])
}

// EncodeGrantCapability 141 的规范编码。policy / policyEpoch 是签发 grant 的账户策略(与签它的账户根主体相同)。
func (n Network) EncodeGrantCapability(policy, policyEpoch uint64, g GrantSpec) ([]byte, Hash, error) {
	if len(g.Actions) == 0 || len(g.Accounts) == 0 || len(g.Limits.Markets) == 0 {
		return nil, Hash{}, fmt.Errorf("dexosv1: grant 的动作、账户、市场都不能为空")
	}
	actions := slices.Clone(g.Actions)
	slices.Sort(actions)
	if len(slices.Compact(actions)) != len(g.Actions) {
		return nil, Hash{}, fmt.Errorf("dexosv1: grant 的动作有重复")
	}
	id := n.GrantID(policy, policyEpoch, g.Salt)
	b := []byte{0x01, tagGrantCapability}
	b = append(b, id[:]...)
	b = append(b, g.Salt[:]...)
	b = append(b, 0x01)
	b = append(b, g.Agent[:]...)
	b = le32(b, uint32(len(actions)))
	for _, a := range actions {
		b = append(b, 0x01)
		b = le16(b, a)
	}
	b = append(b, 0x01)
	b = le32(b, uint32(len(g.Accounts)))
	for _, a := range g.Accounts {
		b = le32(b, a)
	}
	b = le64(b, 0)
	b = le64(b, 0)
	b = le64(b, g.ExpiresAtMs)
	b = le64(b, 1)
	l := g.Limits
	b = optU64(b, l.MaxRequestCount)
	b = le64(b, l.WindowMs)
	b = optU64(b, l.MaxOrderLots)
	var err error
	for _, v := range []*big.Int{l.MaxOrderQuoteAtoms, l.MaxWindowQuoteAtoms, l.MaxFeeQuoteAtoms} {
		if b, err = optU128(b, v); err != nil {
			return nil, Hash{}, err
		}
	}
	b = le32(b, uint32(len(l.Markets)))
	for _, m := range l.Markets {
		b = le16(b, m.Market)
		b = le64(b, m.SpecVersion)
		b = le32(b, m.MinTick)
		b = le32(b, m.MaxTick)
	}
	b = le64(b, g.ExpectedIssuerEpoch)
	return b, id, nil
}

// EncodeRevokeCapability 142 的规范编码;expectedEpoch 是 grant 当前纪元。
func EncodeRevokeCapability(grant Hash, expectedEpoch uint64) []byte {
	b := []byte{0x01, tagRevokeCapability}
	b = append(b, grant[:]...)
	return le64(b, expectedEpoch)
}

func optU64(b []byte, v *uint64) []byte {
	if v == nil {
		return append(b, 0)
	}
	return le64(append(b, 1), *v)
}

func optU128(b []byte, v *big.Int) ([]byte, error) {
	if v == nil {
		return append(b, 0), nil
	}
	if v.Sign() < 0 || v.BitLen() > 128 {
		return nil, fmt.Errorf("dexosv1: 额度 %s 超出 u128", v)
	}
	var be [16]byte
	v.FillBytes(be[:])
	b = append(b, 1)
	for i := 15; i >= 0; i-- {
		b = append(b, be[i])
	}
	return b, nil
}
