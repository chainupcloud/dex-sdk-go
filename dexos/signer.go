package dexos

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// Address 20 字节以太坊地址。
type Address [20]byte

// ParseAddress 解析 0x 前缀或裸 hex 的 20 字节地址。
func ParseAddress(s string) (Address, error) {
	var a Address
	b, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x"))
	if err != nil {
		return a, fmt.Errorf("地址不是合法 hex:%w", err)
	}
	if len(b) != 20 {
		return a, fmt.Errorf("地址应为 20 字节,得到 %d", len(b))
	}
	copy(a[:], b)
	return a, nil
}

// Hex 返回小写 0x 形式。
//
// 刻意不做 EIP-55 校验和大小写:网关按小写比对,混入大小写只会制造
// 「同一个地址两种写法」的不必要分支。
func (a Address) Hex() string { return "0x" + hex.EncodeToString(a[:]) }

func (a Address) String() string { return a.Hex() }

// IsZero 判空。零地址在本 SDK 里只用作 EIP-712 的 verifyingContract。
func (a Address) IsZero() bool { return a == Address{} }

// Signer 一把 secp256k1 私钥。
//
// **主账号密钥与 API 钱包密钥都用这个类型**,区别只在于你把哪一把交给了什么:
// 主账号密钥能授权/撤销 API 钱包、能提款;API 钱包密钥只能交易。
// 这正是 API 钱包存在的意义 —— 泄漏它的最坏后果是被代为交易,而不是被提走资产。
type Signer struct {
	priv *secp256k1.PrivateKey
	addr Address
}

// NewSigner 从 32 字节私钥(0x 前缀可选)构造。
func NewSigner(privHex string) (*Signer, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(privHex), "0x"))
	if err != nil {
		return nil, fmt.Errorf("私钥不是合法 hex:%w", err)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("私钥应为 32 字节,得到 %d", len(b))
	}
	var scalar secp256k1.ModNScalar
	if scalar.SetByteSlice(b) || scalar.IsZero() {
		return nil, errors.New("私钥标量必须在 secp256k1 有效范围内")
	}
	priv := secp256k1.NewPrivateKey(&scalar)
	return &Signer{priv: priv, addr: addressFromPub(priv.PubKey())}, nil
}

// GenerateSigner 生成一把新密钥 —— 用于创建 API 钱包。
//
// 私钥只存在于本进程内存:把它交给谁、存到哪,是调用方的决定。
// 交易所只需要知道地址(见 Client.ApproveAgent)。
func GenerateSigner() (*Signer, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("读随机源失败:%w", err)
	}
	priv := secp256k1.PrivKeyFromBytes(b[:])
	if priv.Key.IsZero() {
		return nil, errors.New("生成到零私钥(概率上不可能,出现即随机源有问题)")
	}
	return &Signer{priv: priv, addr: addressFromPub(priv.PubKey())}, nil
}

// Address 返回该密钥对应的地址。
func (s *Signer) Address() Address { return s.addr }

// PrivateKeyHex 导出私钥。**只在你确实要保存/移交这把 API 钱包时调用。**
func (s *Signer) PrivateKeyHex() string {
	return "0x" + hex.EncodeToString(s.priv.Serialize())
}

// addressFromPub 以太坊地址 = keccak256(未压缩公钥去掉 0x04 前缀) 的后 20 字节。
func addressFromPub(pub *secp256k1.PublicKey) Address {
	un := pub.SerializeUncompressed() // 65 字节:0x04 ‖ X ‖ Y
	h := Keccak256(un[1:])
	var a Address
	copy(a[:], h[12:])
	return a
}

// SignHash 对 32 字节哈希做可恢复签名,返回以太坊格式的 65 字节 r‖s‖v(v ∈ {27,28})。
//
// decred 的 SignCompact 产出的是 v‖r‖s 且 v 已含 27 偏移,这里重排成以太坊顺序。
// 弄错顺序的表现是「签名格式合法但恢复出别的地址」→ 服务端回 UnboundSigner,
// 而不是一个能指向根因的错误,所以这段值得单独看一眼。
func (s *Signer) SignHash(hash []byte) ([65]byte, error) {
	var out [65]byte
	if len(hash) != 32 {
		return out, fmt.Errorf("待签哈希应为 32 字节,得到 %d", len(hash))
	}
	compact := ecdsa.SignCompact(s.priv, hash, false) // false = 不压缩公钥,v 基准 27
	if len(compact) != 65 {
		return out, fmt.Errorf("签名长度异常:%d", len(compact))
	}
	copy(out[0:64], compact[1:65]) // r‖s
	out[64] = compact[0]           // v
	return out, nil
}

// SignHashHex 同 SignHash,返回 0x 前缀的 hex —— REST 请求体里就用这个形式。
func (s *Signer) SignHashHex(hash []byte) (string, error) {
	sig, err := s.SignHash(hash)
	if err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(sig[:]), nil
}
