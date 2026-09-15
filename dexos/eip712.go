package dexos

import (
	"golang.org/x/crypto/sha3"
)

// EIP-712 结构化签名。与 crates/auth/src/lib.rs 一一对应。
//
// 每个 word 都是 32 字节**大端**右对齐 —— 注意这与规范命令编码的小端相反:
// 前者是以太坊的 ABI 约定,后者是内核自己的紧凑编码。同一个 SDK 里两种字节序
// 并存,是最容易混的地方,所以两边的原语分开命名(word* vs encoder.u*)。

const (
	domainType       = "EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"
	domainName       = "dex-os"
	domainVersion    = "1"
	approveAgentType = "ApproveAgent(address owner,uint32 master,address agent,uint64 validUntilMs,uint64 nonce)"
	revokeAgentType  = "RevokeAgent(address owner,uint32 master,address agent,uint64 nonce)"
	agentExecType    = "AgentExec(bytes32 commandHash,uint64 nonce)"
)

// Keccak256 以太坊口径的 keccak(不是 SHA3-256)。
func Keccak256(parts ...[]byte) []byte {
	h := sha3.NewLegacyKeccak256()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// wordUint 把大端字节右对齐进 32 字节 word。
func wordUint(be []byte) []byte {
	var w [32]byte
	copy(w[32-len(be):], be)
	return w[:]
}

func wordU64(v uint64) []byte {
	return wordUint([]byte{
		byte(v >> 56), byte(v >> 48), byte(v >> 40), byte(v >> 32),
		byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v),
	})
}

func wordU32(v uint32) []byte {
	return wordUint([]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// wordAddr 地址右对齐进 32 字节 word(左侧补 12 个零字节)。
func wordAddr(a Address) []byte {
	var w [32]byte
	copy(w[12:], a[:])
	return w[:]
}

// Domain EIP-712 域。
//
// VerifyingContract 当前恒为零地址 —— dex-os 的验签在 STF 内,没有链上验证合约,
// 填零地址是刻意的(与前端 ethers.ZeroAddress 一致)。但它**是个字段而不是常量**:
// 服务端的域里本来就有这一项,哪天它变成真地址,写死会让每一笔签名静默被拒,
// 而错误信息只有 401。零值就是零地址,老用法不受影响。
type Domain struct {
	Name              string
	Version           string
	ChainID           uint64
	VerifyingContract Address
}

func (d Domain) separator() []byte {
	return Keccak256(
		Keccak256([]byte(domainType)),
		Keccak256([]byte(d.Name)),
		Keccak256([]byte(d.Version)),
		wordU64(d.ChainID),
		wordAddr(d.VerifyingContract),
	)
}

// signingHash 组装 EIP-712 的最终待签哈希:keccak(0x19 0x01 ‖ domain ‖ structHash)。
func (d Domain) signingHash(structHash []byte) []byte {
	return Keccak256([]byte{0x19, 0x01}, d.separator(), structHash)
}

// ApproveAgentHash 「授权 API 钱包」的待签哈希。
func (d Domain) ApproveAgentHash(owner Address, master uint32, agent Address, validUntilMs, nonce uint64) []byte {
	sh := Keccak256(
		Keccak256([]byte(approveAgentType)),
		wordAddr(owner),
		wordU32(master),
		wordAddr(agent),
		wordU64(validUntilMs),
		wordU64(nonce),
	)
	return d.signingHash(sh)
}

// RevokeAgentHash 「撤销 API 钱包」的待签哈希。
func (d Domain) RevokeAgentHash(owner Address, master uint32, agent Address, nonce uint64) []byte {
	sh := Keccak256(
		Keccak256([]byte(revokeAgentType)),
		wordAddr(owner),
		wordU32(master),
		wordAddr(agent),
		wordU64(nonce),
	)
	return d.signingHash(sh)
}

// AgentExecHash 「agent 代执行」的待签哈希。
//
// commandHash = keccak256(**规范命令编码**),由 codec.go 的 Encode* 产出。
// 这一步把签名与具体命令字节绑死:改一个价格、少一张单,哈希就变,签名作废。
func (d Domain) AgentExecHash(commandBytes []byte, nonce uint64) []byte {
	sh := Keccak256(
		Keccak256([]byte(agentExecType)),
		Keccak256(commandBytes),
		wordU64(nonce),
	)
	return d.signingHash(sh)
}
