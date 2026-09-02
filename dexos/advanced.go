package dexos

import (
	"context"
	"encoding/hex"
	"net/http"
	"time"
)

// 经**通用入口** POST /agent/exec 执行的命令。
//
// 内核的 agent scope 白名单有 13 条命令,而网关固定形状的入口(/batch /cancel /replace)
// 只覆盖 3 条。通用入口收的是规范编码字节,网关只做解码转发、不参与语义,
// 于是白名单里的其余命令一次性全部可达。
//
// **它没有绕开任何权限。** 签名证明该 agent 授权了这一条具体命令,而内核的白名单
// 限定 agent 能做什么 —— 提款/转账一律 AgentScopeViolation。收任意规范字节
// 之所以安全,全靠后者。

// AgentExec 通用代执行:把任意一条**规范编码**过的命令用 agent 密钥签名后提交。
//
// 白名单外的命令会被内核拒(422 AgentScopeViolation)。嵌套代执行被网关直接拒(400)。
//
// 一般用不到它 —— 下面的 ScheduleCancel / SetLeverage 等便捷方法已经包好了。
// 直接用它的场景是:内核加了新命令而 SDK 还没跟上,你可以自己编码后从这里发。
func (c *Client) AgentExec(ctx context.Context, agent *Signer, command []byte, nonce uint64) ([]EventEnvelope, error) {
	sig, err := agent.SignHashHex(c.Domain.AgentExecHash(command, nonce))
	if err != nil {
		return nil, err
	}
	var out writeResp
	err = c.do(ctx, http.MethodPost, "/agent/exec", map[string]any{
		"agent":     agent.Address().Hex(),
		"command":   "0x" + hex.EncodeToString(command),
		"nonce":     nonce,
		"signature": sig,
	}, &out)
	return out.Events, err
}

// ScheduleCancel 断线保护(dead man's switch):到 triggerAt 时撤销该账户**全部**
// 挂单、条件单与 TWAP 母单。传零值 time.Time 清除开关。
//
// 做市必备。断线时报价留在簿上被单边吃穿,是做市商最怕的场景 —— 而行情走了之后
// 你的报价还在原地,那不是滑点问题,是白送。
//
// 用法是**每轮报价顺手往后续一次**(比如设成 now+30s),正常跑时不断刷新,
// 一旦进程挂了/网络断了,到点自动全撤:
//
//	c.ScheduleCancel(ctx, api, master, time.Now().Add(30*time.Second), nonce)
//
// 「至少 N 秒后」「每日次数上限」这类是宿主策略,内核只存确定性状态 ——
// 所以这里传什么时刻就是什么时刻,没有下限保护,别设成过去。
func (c *Client) ScheduleCancel(ctx context.Context, agent *Signer, account uint32,
	triggerAt time.Time, nonce uint64) ([]EventEnvelope, error) {
	var triggerMs uint64
	if !triggerAt.IsZero() {
		triggerMs = uint64(triggerAt.UnixMilli())
	}
	cmd := EncodeScheduleCancel(account, triggerMs, uint64(time.Now().UnixMilli()))
	return c.AgentExec(ctx, agent, cmd, nonce)
}

// SetLeverage 自定义杠杆。customImfPpm 是初始保证金率(ppm),200000 = 20% = 5×。
//
// **只能调高保证金要求(降杠杆)**,低于市场基础 IMF 会被拒(InvalidLeverage)。
// 它只抬高 IMR、不动 MMR —— 所以调杠杆不会把一个健康账户直接推进可清算区间。
func (c *Client) SetLeverage(ctx context.Context, agent *Signer, account uint32,
	market uint16, customImfPpm uint32, nonce uint64) ([]EventEnvelope, error) {
	return c.AgentExec(ctx, agent, EncodeSetLeverage(account, market, customImfPpm), nonce)
}

// PlaceOrderGTT 带过期时刻的单张下单。
//
// BatchPlace 的单项没有 good_til_ms 字段,需要 GTT 时用这个。
// goodTilMs 那一刻**仍然有效**,越过之后才失效(闭开区间)。
func (c *Client) PlaceOrderGTT(ctx context.Context, agent *Signer, account uint32,
	market uint16, side Side, price uint32, lots uint64, tif TimeInForce,
	reduceOnly bool, goodTil time.Time, nonce uint64) ([]EventEnvelope, error) {
	var goodTilMs uint64
	if !goodTil.IsZero() {
		goodTilMs = uint64(goodTil.UnixMilli())
	}
	cmd := EncodePlaceOrder(account, market, side, price, lots, tif, reduceOnly,
		goodTilMs, uint64(time.Now().UnixMilli()), 0, 0)
	return c.AgentExec(ctx, agent, cmd, nonce)
}

// ModifyOrder 改单 = **撤旧建新,非原子**。
//
// 关键后果是**丢失时间优先**:同价位上原本排在前面的单,改完排到了后面。
// 这不是实现瑕疵而是语义。做市要保住队列位置就别用它,用 BatchReplace 整轮换 ——
// 反正队列位置在换价时本来就保不住。
func (c *Client) ModifyOrder(ctx context.Context, agent *Signer, account uint32,
	order OrderID, price uint32, lots uint64, tif TimeInForce, reduceOnly bool,
	goodTil time.Time, nonce uint64) ([]EventEnvelope, error) {
	var goodTilMs uint64
	if !goodTil.IsZero() {
		goodTilMs = uint64(goodTil.UnixMilli())
	}
	cmd := EncodeModifyOrder(account, order, price, lots, tif, reduceOnly,
		goodTilMs, uint64(time.Now().UnixMilli()))
	return c.AgentExec(ctx, agent, cmd, nonce)
}

// CancelConditional 撤销一张未触发的条件单(止盈/止损)。
func (c *Client) CancelConditional(ctx context.Context, agent *Signer, account uint32,
	order OrderID, nonce uint64) ([]EventEnvelope, error) {
	return c.AgentExec(ctx, agent, EncodeCancelConditional(account, order), nonce)
}

// CancelTwap 撤销 TWAP 母单:已成交的留下,未执行的切片彻底停掉、挂单额度立即释放。
func (c *Client) CancelTwap(ctx context.Context, agent *Signer, account uint32,
	order OrderID, nonce uint64) ([]EventEnvelope, error) {
	return c.AgentExec(ctx, agent, EncodeCancelTwap(account, order), nonce)
}
