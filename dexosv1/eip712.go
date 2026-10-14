package dexosv1

import (
	"bytes"
	"math/big"
	"slices"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 主钱包签名的 EIP-712 结构体:Order / Cancel / Leverage 与金库的 CreateManagedVault / VaultDeposit / VaultRedeem /
// ClaimVaultManager / VaultConfigChange。与 dex-os crates/auth/src/lib.rs 逐字对应,类型串同时钉在 OpenAPI
// 各写端点的 `x-eip712.typeString` 上(eip712_test.go 核对两边不漂移)。
//
// ApproveAgent / RevokeAgent 与 AgentExec 用 dexos 包已有的实现(Domain.ApproveAgentHash / RevokeAgentHash / AgentExecHash)。
// 这里另写域分隔符,是因为 dexos.Domain 的 separator / signingHash 没有导出;
// 金样(testdata/eip712_golden.json,ethers 生成)覆盖两个不同的域,两份实现不一致会在那里红。
const (
	eip712DomainType = "EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"
	orderType        = "Order(address trader,uint32 accountId,uint16 market,uint8 side,uint32 price,uint64 lots,uint8 tif,bool reduceOnly,uint64 nonce,uint64 expiry)"
	cancelType       = "Cancel(address trader,uint32 accountId,uint16 market,uint64 orderSeq,uint64 nonce)"
	leverageType     = "Leverage(address trader,uint32 accountId,uint16 market,uint32 customImfPpm,uint64 nonce)"

	createManagedVaultType = "CreateManagedVault(address owner,uint32 accountId,address manager,uint32 profitSharePpm,uint64 depositMinDurationMs,uint32 managerMinPortionPpm,uint32 managerLiqPortionPpm,uint32 withdrawWindowS,uint32 withdrawDurationS,uint128 depositCap,uint128 seedAmount,bytes32 depositorWhitelistHash,uint64 nonce)"
	vaultDepositType       = "VaultDeposit(address trader,uint16 vault,uint32 accountId,uint128 amount,uint64 nonce)"
	vaultRedeemType        = "VaultRedeem(address trader,uint16 vault,uint32 accountId,uint128 shares,uint64 nonce)"
	claimVaultManagerType  = "ClaimVaultManager(address trader,uint16 vault,uint32 accountId,uint128 amount,uint64 nonce)"
	vaultConfigChangeType  = "VaultConfigChange(address owner,uint16 vault,uint32 accountId,uint8 kind,uint128 cap,uint32 maxLeverage,bytes32 marketsHash,uint64 nonce)"
)

// word 无符号整数右对齐进 32 字节大端 word(uint8 … uint64 与 bool 都走它)。
func word(v uint64) []byte {
	var w [32]byte
	for i := range 8 {
		w[31-i] = byte(v >> (8 * i))
	}
	return w[:]
}

func wordAddr(a dexos.Address) []byte {
	var w [32]byte
	copy(w[12:], a[:])
	return w[:]
}

func wordBool(b bool) []byte {
	if b {
		return word(1)
	}
	return word(0)
}

// signingHash keccak256(0x19 0x01 ‖ domainSeparator ‖ structHash)。
func signingHash(d dexos.Domain, structHash []byte) []byte {
	sep := dexos.Keccak256(
		dexos.Keccak256([]byte(eip712DomainType)),
		dexos.Keccak256([]byte(d.Name)),
		dexos.Keccak256([]byte(d.Version)),
		word(d.ChainID),
		wordAddr(d.VerifyingContract),
	)
	return dexos.Keccak256([]byte{0x19, 0x01}, sep, structHash)
}

// orderHash 主钱包单笔下单的待签哈希。expiry 0 = 不过期。
func orderHash(d dexos.Domain, trader dexos.Address, accountID uint32, market uint16, side dexos.Side,
	price uint32, lots uint64, tif dexos.TimeInForce, reduceOnly bool, nonce, expiry uint64) []byte {
	return signingHash(d, dexos.Keccak256(
		dexos.Keccak256([]byte(orderType)),
		wordAddr(trader),
		word(uint64(accountID)),
		word(uint64(market)),
		word(uint64(side)),
		word(uint64(price)),
		word(lots),
		word(uint64(tif)),
		wordBool(reduceOnly),
		word(nonce),
		word(expiry),
	))
}

// cancelHash 主钱包单笔撤单的待签哈希。orderSeq 是订单序号(v1 的 orderId),不是打包的 OrderID。
func cancelHash(d dexos.Domain, trader dexos.Address, accountID uint32, market uint16, orderSeq, nonce uint64) []byte {
	return signingHash(d, dexos.Keccak256(
		dexos.Keccak256([]byte(cancelType)),
		wordAddr(trader),
		word(uint64(accountID)),
		word(uint64(market)),
		word(orderSeq),
		word(nonce),
	))
}

// leverageHash 设杠杆的待签哈希。customImfPpm 0 = 清除自定义,回到市场基础 IMF。
func leverageHash(d dexos.Domain, trader dexos.Address, accountID uint32, market uint16, customImfPpm uint32, nonce uint64) []byte {
	return signingHash(d, dexos.Keccak256(
		dexos.Keccak256([]byte(leverageType)),
		wordAddr(trader),
		word(uint64(accountID)),
		word(uint64(market)),
		word(uint64(customImfPpm)),
		word(nonce),
	))
}

// wordBig 非负整数(≤ 256 位)右对齐进 32 字节大端 word(uint128 金额走它)。
func wordBig(v *big.Int) []byte {
	var w [32]byte
	return v.FillBytes(w[:])
}

// managedVault CreateManagedVault 的字段;可选项用 0 / 零地址表示没设(与 dex-os 的哨兵一致)。
type managedVault struct {
	owner                                      dexos.Address
	accountID                                  uint32
	manager                                    dexos.Address
	profitSharePpm                             uint32
	depositMinDurationMs                       uint64
	managerMinPortionPpm, managerLiqPortionPpm uint32
	withdrawWindowS, withdrawDurationS         uint32
	depositCap, seedAmount                     *big.Int
	depositorWhitelist                         []dexos.Address
	nonce                                      uint64
}

// createManagedVaultHash 建库的待签哈希。
func createManagedVaultHash(d dexos.Domain, v managedVault) []byte {
	return signingHash(d, dexos.Keccak256(
		dexos.Keccak256([]byte(createManagedVaultType)),
		wordAddr(v.owner),
		word(uint64(v.accountID)),
		wordAddr(v.manager),
		word(uint64(v.profitSharePpm)),
		word(v.depositMinDurationMs),
		word(uint64(v.managerMinPortionPpm)),
		word(uint64(v.managerLiqPortionPpm)),
		word(uint64(v.withdrawWindowS)),
		word(uint64(v.withdrawDurationS)),
		wordBig(v.depositCap),
		wordBig(v.seedAmount),
		depositorWhitelistHash(v.depositorWhitelist),
		word(v.nonce),
	))
}

// depositorWhitelistHash keccak(升序去重的 20 字节地址紧凑拼接);空表 = 32 个零字节(dex-os auth::depositor_whitelist_hash)。
func depositorWhitelistHash(addrs []dexos.Address) []byte {
	if len(addrs) == 0 {
		return make([]byte, 32)
	}
	v := slices.Clone(addrs)
	slices.SortFunc(v, func(a, b dexos.Address) int { return bytes.Compare(a[:], b[:]) })
	v = slices.Compact(v)
	var buf []byte
	for _, a := range v {
		buf = append(buf, a[:]...)
	}
	return dexos.Keccak256(buf)
}

// marketsHash keccak(升序去重的 u16 大端紧凑拼接);空表 = 32 个零字节(dex-os auth::markets_hash)。
func marketsHash(markets []uint16) []byte {
	if len(markets) == 0 {
		return make([]byte, 32)
	}
	v := slices.Clone(markets)
	slices.Sort(v)
	v = slices.Compact(v)
	buf := make([]byte, 0, 2*len(v))
	for _, m := range v {
		buf = append(buf, byte(m>>8), byte(m))
	}
	return dexos.Keccak256(buf)
}

// vaultAmountHash 存入 / 赎回 / 认领三个结构体同形(trader, vault, accountId, 金额或份额, nonce),只差类型串。
func vaultAmountHash(d dexos.Domain, typ string, trader dexos.Address, vault uint16, accountID uint32, amount *big.Int, nonce uint64) []byte {
	return signingHash(d, dexos.Keccak256(
		dexos.Keccak256([]byte(typ)),
		wordAddr(trader),
		word(uint64(vault)),
		word(uint64(accountID)),
		wordBig(amount),
		word(nonce),
	))
}

// vaultConfigChangeHash 改金库配置的待签哈希。存款上限 / maxLeverage / marketsHash 由调用方按 kind 置零。
func vaultConfigChangeHash(d dexos.Domain, owner dexos.Address, vault uint16, accountID uint32, kind uint8, depositCap *big.Int,
	maxLeverage uint32, markets []byte, nonce uint64) []byte {
	return signingHash(d, dexos.Keccak256(
		dexos.Keccak256([]byte(vaultConfigChangeType)),
		wordAddr(owner),
		word(uint64(vault)),
		word(uint64(accountID)),
		word(uint64(kind)),
		wordBig(depositCap),
		word(uint64(maxLeverage)),
		markets,
		word(nonce),
	))
}
