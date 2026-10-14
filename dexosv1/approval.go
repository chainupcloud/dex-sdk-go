package dexosv1

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 主钱包密钥不在本进程时的授权:主钱包在别处(浏览器钱包)对 Domain.ApproveAgentTypedData 签名,
// 本进程只提交签名。签名时的 nonce、signingAccountId、域都已签进去,所以这里不经 nonce 计数器。

// SignedApproval 一份已签好的「授权 API 钱包」。签的是
// Domain.ApproveAgentTypedData(Account.Address, SigningAccountID, Agent, ValidUntil, Nonce)。
type SignedApproval struct {
	Account AccountRef
	// SigningAccountID / Nonce 签名时从 `GET /v1/account/nonce`(主钱包作用域)读到的值。
	SigningAccountID uint32
	Nonce            uint64
	// Domain 签名时从 `GET /v1/exchange` 读到的域。
	Domain dexos.Domain
	Agent  dexos.Address
	// ValidUntil 毫秒;0 = 不过期。
	ValidUntil uint64
	// Signature 65 字节 hex(0x 开头)。
	Signature string
	// Label 只是网关上的标签(≤ 32 字符),不进签名。
	Label string
}

// SubmitApproval 提交一份已签好的授权。签名在签好之后被别的写用掉了 nonce 时,服务端拒因是 NonceMismatch
// (422 [*Error]):这份签名已作废,只能重签。结论未知(传输失败、5xx、202)原样返回错误,由调用方先读回授权再决定。
func (c *Client) SubmitApproval(ctx context.Context, a SignedApproval) (*Receipt, error) {
	if !strings.HasPrefix(a.Signature, "0x") || len(a.Signature) != 132 {
		return nil, fmt.Errorf("dexosv1: 授权签名应为 0x + 130 位十六进制,得到 %d 个字符", len(a.Signature))
	}
	if a.Agent.IsZero() || a.Agent == a.Account.Address {
		return nil, errors.New("dexosv1: 授权的 API 钱包地址为空或就是主钱包")
	}
	owner := a.Account.Address
	digest := a.Domain.ApproveAgentHash(owner, a.SigningAccountID, a.Agent, a.ValidUntil, a.Nonce)
	id := newIdentity(owner, fmt.Sprintf("account:%d", a.SigningAccountID), a.Nonce, digest, nil)
	env := &envelope{Account: owner.Hex(), Subaccount: a.Account.Subaccount, Nonce: a.Nonce, Signature: a.Signature}
	status, raw, err := c.post(ctx, "/v1/account/agents/approve", wireApproveReq{envelope: env, Agent: a.Agent.Hex(), ValidUntil: a.ValidUntil, Label: a.Label})
	if err != nil {
		return nil, err
	}
	out, err := receipt(status, raw, id)
	if err != nil {
		return nil, err
	}
	var wr struct {
		Agent string `json:"agent"`
	}
	if err := decodeInto(raw, &wr, "授权回执"); err != nil {
		return nil, err
	}
	if !strings.EqualFold(wr.Agent, a.Agent.Hex()) {
		return nil, fmt.Errorf("dexosv1: 授权 %s,回执是 %q", a.Agent.Hex(), wr.Agent)
	}
	return &out, nil
}

// Subaccount 主钱包名下的一个账户(`GET /v1/account/subaccounts`)。
// SigningAccountID **只供签名**(填 EIP-712 结构体的 accountId),不作寻址。
type Subaccount struct {
	Subaccount       uint32 `json:"subaccount"`
	IsMain           bool   `json:"isMain"`
	SigningAccountID uint32 `json:"signingAccountId"`
}

// Subaccounts 主钱包名下的全部账户(含主账户,子账户序号 0)。没开户的地址服务端回 404([*Error])。
func (c *Client) Subaccounts(ctx context.Context, owner dexos.Address) ([]Subaccount, error) {
	var resp struct {
		Address     string       `json:"address"`
		Subaccounts []Subaccount `json:"subaccounts"`
	}
	if err := c.get(ctx, "/v1/account/subaccounts", url.Values{"address": {owner.Hex()}}, accountRead, &resp); err != nil {
		return nil, err
	}
	if !strings.EqualFold(resp.Address, owner.Hex()) {
		return nil, fmt.Errorf("dexosv1: 问 %s 的账户表,答的是 %q", owner.Hex(), resp.Address)
	}
	return resp.Subaccounts, nil
}

// SigningAccountID 签进命令的账户号(建会话时从 `/v1/account/nonce` 取得)。写前日志据此重算命令核对。
func (s *Session) SigningAccountID() uint32 { return s.signingID }

// Domain 签名用的 EIP-712 域(建会话时从 `/v1/exchange` 取得)。
func (s *Session) Domain() dexos.Domain { return s.domain }
