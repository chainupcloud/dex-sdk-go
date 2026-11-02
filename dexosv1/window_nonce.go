package dexosv1

import (
	"sync"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 窗口 nonce(dex-os 子账户模型 v2 §3.3):`sigVersion: 2` 的 nonce 是签名者自己取的毫秒时间戳。执行层按签名者记最近
// 100 个,只收块时间前 2 天到后 1 天之内、没用过的值(记满了还要大于其中最小的)。所以不用先查计数器,
// 一次被拒也不会让后面的 nonce 错位。
//
// 本进程内同一签名者的 nonce 严格递增:取 max(当前毫秒, 上次 + 1)。同一把钥匙不要在两个进程里同时签 v2:
// 两边可能在同一毫秒取到同一个值,后到的被拒(NonceMismatch)。

type windowNonces struct {
	mu   sync.Mutex
	last map[dexos.Address]uint64
}

// processNonces 本进程的窗口 nonce:[WindowNonce] 与 [PairedSession] 共用,同一签名者的值不会重复。
var processNonces windowNonces

func (w *windowNonces) next(signer dexos.Address, nowMs uint64) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		w.last = map[dexos.Address]uint64{}
	}
	n := max(nowMs, w.last[signer]+1)
	w.last[signer] = n
	return n
}

// WindowNonce 签名者 signer 的下一个窗口 nonce(本进程内严格递增)。给在别处签名的 v2 结构体选 nonce 用,
// 例如交给主钱包签的 [dexos.Domain.ApproveAgentScopeTypedData];[PairedSession] 自己取,不用它。
func WindowNonce(signer dexos.Address) uint64 { return processNonces.next(signer, nowMs()) }
