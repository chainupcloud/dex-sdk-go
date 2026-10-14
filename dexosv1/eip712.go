package dexosv1

import (
	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 主钱包签名的三个 EIP-712 结构体:Order / Cancel / Leverage。与 dex-os crates/auth/src/lib.rs 逐字对应,
// 类型串同时钉在 OpenAPI 各写端点的 `x-eip712.typeString` 上(eip712_test.go 核对两边不漂移)。
//
// ApproveAgent 与 AgentExec 用 dexos 包已有的实现(Domain.ApproveAgentHash / AgentExecHash)。
// 这里另写域分隔符,是因为 dexos.Domain 的 separator / signingHash 没有导出;
// 金样(testdata/eip712_golden.json,ethers 生成)覆盖两个不同的域,两份实现不一致会在那里红。
const (
	eip712DomainType = "EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"
	orderType        = "Order(address trader,uint32 accountId,uint16 market,uint8 side,uint32 price,uint64 lots,uint8 tif,bool reduceOnly,uint64 nonce,uint64 expiry)"
	cancelType       = "Cancel(address trader,uint32 accountId,uint16 market,uint64 orderSeq,uint64 nonce)"
	leverageType     = "Leverage(address trader,uint32 accountId,uint16 market,uint32 customImfPpm,uint64 nonce)"
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
