package dexosv1

import (
	"encoding/binary"
	"fmt"

	"github.com/chainupcloud/dex-sdk-go/dexos"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// DecodedAction 从规范编码还原的一条已签授权:签名者是按摘要恢复出的地址,未核对它是否有权签。
type DecodedAction struct {
	Action  Action
	Signers []dexos.Address
}

// DecodedCommand 解码后的 139(Legs 一条)或 179(Legs 两条:挂单腿、吃单腿,共用一份载荷)。
type DecodedCommand struct {
	Paired bool
	Legs   []DecodedAction
}

var principalKinds = map[Hash]PrincipalKind{
	textHash(string(PrincipalAccount)): PrincipalAccount,
	textHash(string(PrincipalAgent)):   PrincipalAgent,
}

var lanes = map[Hash]Lane{
	textHash(string(LaneTrade)):      LaneTrade,
	textHash(string(LaneFunds)):      LaneFunds,
	textHash(string(LaneManagement)): LaneManagement,
}

type wireReader struct {
	b   []byte
	err error
}

func (c *wireReader) take(n int) []byte {
	if c.err != nil {
		return nil
	}
	if len(c.b) < n {
		c.err = fmt.Errorf("dexosv1: 命令编码截断")
		return nil
	}
	out := c.b[:n]
	c.b = c.b[n:]
	return out
}

func (c *wireReader) hash() Hash {
	var h Hash
	copy(h[:], c.take(32))
	return h
}

func (c *wireReader) u64() uint64 {
	if b := c.take(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

func (c *wireReader) u32() uint32 {
	if b := c.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

type rawAction struct {
	net                Network
	kind, pid, lane    Hash
	auth               Authorization
	actionID, payloadH Hash
	pre                Hash
	validAfter         uint64
}

func (c *wireReader) action() rawAction {
	var r rawAction
	r.net.NetworkID, r.net.GenesisHash, r.net.LedgerEpoch = c.hash(), c.hash(), c.u64()
	r.kind, r.pid = c.hash(), c.hash()
	r.auth.Principal.Epoch = c.u64()
	r.auth.Principal.GrantID, r.auth.Principal.GrantEpoch = c.hash(), c.u64()
	r.lane = c.hash()
	r.auth.Nonce = c.u64()
	r.actionID, r.payloadH, r.pre = c.hash(), c.hash(), c.hash()
	r.validAfter, r.auth.ValidUntilMs = c.u64(), c.u64()
	return r
}

func (c *wireReader) signatures() [][]byte {
	n := int(c.u32())
	if n == 0 || n > 32 {
		if c.err == nil {
			c.err = fmt.Errorf("dexosv1: 签名个数 %d 非法", n)
		}
		return nil
	}
	out := make([][]byte, n)
	for i := range out {
		out[i] = c.take(65)
	}
	return out
}

// DecodeCommand 解码 139 / 179 并按网络身份重算 actionId 与摘要、恢复签名者;编码不规范、身份对不上或
// 网络不是 net 时报错。账本侧的授权(grant、nonce、有效期)不在这里核。
func DecodeCommand(net Network, b []byte) (DecodedCommand, error) {
	if len(b) < 2 || b[0] != 0x01 {
		return DecodedCommand{}, fmt.Errorf("dexosv1: 不是编码版本 1 的命令")
	}
	c := &wireReader{b: b[2:]}
	var raws []rawAction
	var payload []byte
	var sigs [][][]byte
	switch b[1] {
	case tagAuthenticatedCommand:
		raws = []rawAction{c.action()}
		payload = c.take(int(c.u32()))
		sigs = [][][]byte{c.signatures()}
		if c.u32() != 0 && c.err == nil {
			c.err = fmt.Errorf("dexosv1: 业务 139 不带见证")
		}
	case tagPairedAuthenticatedCommand:
		raws = []rawAction{c.action(), c.action()}
		payload = c.take(int(c.u32()))
		sigs = [][][]byte{c.signatures(), c.signatures()}
	default:
		return DecodedCommand{}, fmt.Errorf("dexosv1: 不支持的顶层命令 0x%02x", b[1])
	}
	if c.err != nil {
		return DecodedCommand{}, c.err
	}
	if len(c.b) != 0 {
		return DecodedCommand{}, fmt.Errorf("dexosv1: 命令尾部多出 %d 字节", len(c.b))
	}
	out := DecodedCommand{Paired: len(raws) == 2}
	for i, r := range raws {
		kind, ok := principalKinds[r.kind]
		if !ok {
			return DecodedCommand{}, fmt.Errorf("dexosv1: 不支持的签名主体")
		}
		lane, ok := lanes[r.lane]
		if !ok {
			return DecodedCommand{}, fmt.Errorf("dexosv1: 未知车道")
		}
		if r.net.NetworkID != net.NetworkID || r.net.GenesisHash != net.GenesisHash || r.net.LedgerEpoch != net.LedgerEpoch {
			return DecodedCommand{}, fmt.Errorf("dexosv1: 命令签在别的网络上")
		}
		var policy [32]byte = r.pid
		for _, x := range policy[:24] {
			if x != 0 {
				return DecodedCommand{}, fmt.Errorf("dexosv1: principalId 超出 u64")
			}
		}
		r.auth.Principal.Kind = kind
		r.auth.Principal.Policy = binary.BigEndian.Uint64(policy[24:])
		r.auth.Lane = lane
		if r.validAfter != 0 || r.pre != emptyPreconditions {
			return DecodedCommand{}, fmt.Errorf("dexosv1: 业务命令的 validAfterMs 与前提必须为空")
		}
		act, err := net.Action(r.auth, payload)
		if err != nil {
			return DecodedCommand{}, err
		}
		if act.ActionID != r.actionID || act.PayloadHash != r.payloadH {
			return DecodedCommand{}, fmt.Errorf("dexosv1: actionId 与内容对不上")
		}
		leg := DecodedAction{Action: act}
		for _, s := range sigs[i] {
			var sv secp256k1.ModNScalar
			if sv.SetByteSlice(s[32:64]) || sv.IsOverHalfOrder() {
				return DecodedCommand{}, fmt.Errorf("dexosv1: 签名不是低 s 规范形")
			}
			compact := make([]byte, 65)
			compact[0] = s[64]
			copy(compact[1:], s[:64])
			pub, _, err := ecdsa.RecoverCompact(compact, act.Digest[:])
			if err != nil {
				return DecodedCommand{}, fmt.Errorf("dexosv1: 签名恢复失败: %w", err)
			}
			leg.Signers = append(leg.Signers, pubAddress(pub))
		}
		out.Legs = append(out.Legs, leg)
	}
	return out, nil
}
