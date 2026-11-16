package dexosv1

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

const (
	tagPairedTradeSpec            byte = 0xD2
	tagPairedAuthenticatedCommand byte = 0xB3
	authorizedActionLen                = 32*9 + 8*6
)

// EncodePairedTradeSpec 成对成交两腿共签的载荷 0xD2:挂单方、吃单方账户,市场,挂单方方向,价格与手数。
func EncodePairedTradeSpec(makerAccount, takerAccount uint32, market uint16, makerSide dexos.Side, price uint32, lots uint64) []byte {
	b := []byte{0x01, tagPairedTradeSpec}
	b = le32(b, makerAccount)
	b = le32(b, takerAccount)
	b = le16(b, market)
	b = append(b, byte(makerSide))
	b = le32(b, price)
	return le64(b, lots)
}

type signedLeg struct {
	action  []byte
	payload []byte
	sigs    []byte
}

func splitLeg(c Command) (signedLeg, error) {
	b := c.Bytes
	if len(b) < 2+authorizedActionLen+4 || b[0] != 0x01 || b[1] != tagAuthenticatedCommand {
		return signedLeg{}, fmt.Errorf("dexosv1: 不是 139 命令")
	}
	rest := b[2+authorizedActionLen:]
	n := int(binary.LittleEndian.Uint32(rest))
	if len(rest) < 4+n+4 {
		return signedLeg{}, fmt.Errorf("dexosv1: 139 载荷长度不对")
	}
	payload := rest[4 : 4+n]
	tail := rest[4+n:]
	count := int(binary.LittleEndian.Uint32(tail))
	if len(tail) != 4+65*count+4 || binary.LittleEndian.Uint32(tail[4+65*count:]) != 0 {
		return signedLeg{}, fmt.Errorf("dexosv1: 139 签名段不对")
	}
	return signedLeg{action: b[2 : 2+authorizedActionLen], payload: payload, sigs: tail[:4+65*count]}, nil
}

// PairLegs 把两腿各自签好的 139(同一份 0xD2 载荷,都在 Trade 车道)组成 179。
// 回执按挂单腿的 actionId 查,返回的命令 ActionID 即挂单腿的。
func PairLegs(maker, taker Command) (Command, error) {
	m, err := splitLeg(maker)
	if err != nil {
		return Command{}, err
	}
	t, err := splitLeg(taker)
	if err != nil {
		return Command{}, err
	}
	if !bytes.Equal(m.payload, t.payload) || len(m.payload) < 2 || m.payload[1] != tagPairedTradeSpec {
		return Command{}, fmt.Errorf("dexosv1: 两腿必须签同一份 0xD2 载荷")
	}
	if maker.ManifestHash != taker.ManifestHash {
		return Command{}, fmt.Errorf("dexosv1: 两腿签在不同的网络上")
	}
	b := []byte{0x01, tagPairedAuthenticatedCommand}
	b = append(b, m.action...)
	b = append(b, t.action...)
	b = le32(b, uint32(len(m.payload)))
	b = append(b, m.payload...)
	b = append(b, m.sigs...)
	b = append(b, t.sigs...)
	return Command{Bytes: b, ActionID: maker.ActionID, Digest: maker.Digest, ManifestHash: maker.ManifestHash, ValidUntilMs: min(maker.ValidUntilMs, taker.ValidUntilMs)}, nil
}
