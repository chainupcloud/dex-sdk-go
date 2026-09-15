package dexos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// AgentInfo 是代理地址的完整公开授权视图；nonce 属于代理地址，不属于单个授权账户。
type AgentInfo struct {
	Agent     Address
	NextNonce uint64
	Grants    []Grant
}

// AgentGrants 只读查询代理授权，不生成、签署或提交任何授权。
func (c *Client) AgentGrants(ctx context.Context, agent Address) (*AgentInfo, error) {
	if agent.IsZero() {
		return nil, errors.New("dexos: API 代理不能为零地址")
	}
	var response struct {
		Agent     string  `json:"agent"`
		NextNonce *uint64 `json:"nextNonce"`
		Grants    *[]struct {
			Master       *uint32 `json:"master"`
			ValidUntilMs string  `json:"validUntilMs"`
			Expired      *bool   `json:"expired"`
		} `json:"grants"`
	}
	if err := c.do(ctx, http.MethodGet, "/agent/"+agent.Hex(), nil, &response); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil, ErrAgentNotAuthorized
		}
		return nil, err
	}
	returned, err := ParseAddress(response.Agent)
	if err != nil || returned != agent || response.NextNonce == nil || response.Grants == nil {
		return nil, errors.New("dexos: 代理身份响应缺失或不一致")
	}
	out := &AgentInfo{Agent: agent, NextNonce: *response.NextNonce, Grants: make([]Grant, 0, len(*response.Grants))}
	seen := make(map[uint32]bool)
	for _, g := range *response.Grants {
		if g.Master == nil || g.Expired == nil {
			return nil, errors.New("dexos: 授权缺账户或过期状态")
		}
		if _, err := strconv.ParseUint(g.ValidUntilMs, 10, 64); err != nil {
			return nil, fmt.Errorf("dexos: 授权期限非法: %w", err)
		}
		if seen[*g.Master] {
			return nil, errors.New("dexos: 授权响应包含重复账户")
		}
		seen[*g.Master] = true
		out.Grants = append(out.Grants, Grant{Master: *g.Master, ValidUntilMs: g.ValidUntilMs, Expired: *g.Expired})
	}
	return out, nil
}
