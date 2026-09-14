package dexos

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// API 钱包(HL 口径的「代理钱包」)的授权管理。
//
// 账号体系是两层的:
//
//	主账号(owner 地址 + master 账户号)
//	  └─ 授权 ─→ API 钱包(agent 地址)
//
// 主账号密钥能做一切,包括提款;API 钱包**只能交易**。所以正确的接入姿势是:
// 主账号密钥留在冷处,只在授权/撤销时用一次;跑策略的机器上只放 API 钱包密钥。
//
// 三条容易踩的边界:
//   - 一个 agent 地址可以被**多个账户各自**授权(主账户与各子账户分别同意),
//     授权是按 (agent, master) 一条一条记的。所以「这把 key 代谁」不再总能推出来 ——
//     多于一条时 New() 会要求用 ForAccount 指明,而不是替你挑一个。
//   - 授权与撤销都是**单条**的:撤销 master A 不影响 master B 那条。
//   - 同一 master 重复授权同一地址 = **更新有效期**,不是新建。
//   - 撤销是**删除那一条**,不是标记过期;但 agent 的 nonce 不跟着重置
//     (防旧签名重放),所以重授后要重新读一次 nextNonce。

// ApproveAgent 用**主账号密钥**授权一个 API 钱包地址。
//
// validUntil 为零值表示永不过期。nonce 用的是 **master 账户**的计数器(NextNonce),
// 不是 agent 的 —— 这一步 agent 还不存在,自然也没有它的计数器。
func (c *Client) ApproveAgent(
	ctx context.Context,
	owner *Signer,
	master uint32,
	agent Address,
	validUntil time.Time,
	nonce uint64,
) ([]EventEnvelope, error) {
	var validUntilMs uint64
	if !validUntil.IsZero() {
		validUntilMs = uint64(validUntil.UnixMilli())
	}
	sig, err := owner.SignHashHex(
		c.Domain.ApproveAgentHash(owner.Address(), master, agent, validUntilMs, nonce))
	if err != nil {
		return nil, err
	}
	var out writeResp
	err = c.do(ctx, http.MethodPost, "/agent/approve", map[string]any{
		"owner":        owner.Address().Hex(),
		"master":       master,
		"agent":        agent.Hex(),
		"validUntilMs": validUntilMs,
		"nonce":        nonce,
		"signature":    sig,
	}, &out)
	return out.Events, err
}

// RevokeAgent 用**主账号密钥**撤销一个 API 钱包。幂等。
//
// 撤的是 (agent, master) **这一条**:同一把 key 若还被别的账户授权着,那些仍然有效。
// 要彻底停用一把 key,得对每个授权过它的账户各撤一次(GET /agent/<地址> 列得出来)。
func (c *Client) RevokeAgent(
	ctx context.Context,
	owner *Signer,
	master uint32,
	agent Address,
	nonce uint64,
) ([]EventEnvelope, error) {
	sig, err := owner.SignHashHex(
		c.Domain.RevokeAgentHash(owner.Address(), master, agent, nonce))
	if err != nil {
		return nil, err
	}
	var out writeResp
	err = c.do(ctx, http.MethodPost, "/agent/revoke", map[string]any{
		"owner":     owner.Address().Hex(),
		"master":    master,
		"agent":     agent.Hex(),
		"nonce":     nonce,
		"signature": sig,
	}, &out)
	return out.Events, err
}

// AgentNonce 读取某个 API 钱包**自己的**下一个 nonce。
//
// 这是 agent 代执行(BatchPlace/BatchCancel/BatchReplace)要用的计数器,
// 与 master 的 nonce 是两个独立的东西。
//
// **它按 agent 地址计,不按账户计。** 同一把 key 被多个账户授权时,几个账户
// **共用**这一条序列 —— 所以一把 key 同时跑两个会话会互相踩 nonce(表现是
// NonceMismatch/409,而不是任何与授权有关的报错)。要并行跑,给每个账户
// 各生成一把 key;非要共用,就把发单收敛到一处串行。它单调递增且与授权项生命周期解耦 ——
// 撤销后重新授权**不会**把它重置(防旧签名重放),所以重授后必须重读。
//
// 找不到该 agent 时返回 ErrAgentNotFound:可能是没授权、也可能只授权给了别的账户 ——
// 授权是按 (agent, master) 记的,查 master A 看不到 master B 那条。
func (c *Client) AgentNonce(ctx context.Context, master uint32, agent Address) (uint64, error) {
	var r struct {
		Agents []struct {
			Address   string `json:"address"`
			NextNonce uint64 `json:"nextNonce"`
			Expired   bool   `json:"expired"`
		} `json:"agents"`
	}
	if err := c.do(ctx, http.MethodGet, "/agents/"+strconv.Itoa(int(master)), nil, &r); err != nil {
		return 0, err
	}
	want := agent.Hex()
	for _, a := range r.Agents {
		if a.Address == want {
			if a.Expired {
				return a.NextNonce, ErrAgentExpired
			}
			return a.NextNonce, nil
		}
	}
	return 0, ErrAgentNotFound
}
