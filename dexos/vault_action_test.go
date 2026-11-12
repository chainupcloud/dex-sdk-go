package dexos

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestVaultActionMatchesDexOSGolden(t *testing.T) {
	if got := hex.EncodeToString(Keccak256([]byte(vaultActionType))); got != "bdf23f26b62bd32c017a221c2cebb43339332af7bf2b995ce840fea9b0033c29" {
		t.Fatalf("VaultAction 类型哈希 %s", got)
	}
	d := Domain{Name: "dex-os", Version: "1", ChainID: 31337, VerifyingContract: Address(bytes.Repeat([]byte{0x11}, 20))}
	account := Address(bytes.Repeat([]byte{0xAA}, 20))
	digest := d.vaultActionHash(account, 7, bytes.Repeat([]byte{0x0C}, 32), 1_700_000_000_123, 1_700_000_005_000)
	if got := hex.EncodeToString(digest); got != "1f234071dd71ae1ca1f66ad877a8b3eb7de8b7eff2af854917d368a0ad010a57" {
		t.Fatalf("VaultAction 摘要 %s", got)
	}
	command := []byte{1, 2, 3}
	if !bytes.Equal(d.VaultActionHash(account, 7, command, 5, 6), d.vaultActionHash(account, 7, Keccak256(command), 5, 6)) {
		t.Fatal("VaultActionHash 的 action 应是内层命令的 keccak256")
	}
	if bytes.Equal(d.VaultActionHash(account, 7, command, 5, 6), d.ActionHash(account, 7, command, 5, 6)) {
		t.Fatal("同字段的 VaultAction 与 Action 摘要不能相同")
	}
}
