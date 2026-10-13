package dexos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// ErrVaultNotFound 这个金库号不存在(/vaults/:id 回 404)。
var ErrVaultNotFound = errors.New("dexos: 金库不存在")

// ErrVaultUnclaimed 金库还没有 manager(fresh vault 待认领)。
//
// 金库主账户没有 owner,代它签名的只能是 manager;没人认领就没有人能以金库身份交易。
var ErrVaultUnclaimed = errors.New("dexos: 金库未认领(没有 manager),不能以金库身份交易")

// Vault 金库视图里以金库身份交易要用的字段(GET /vaults/:id)。
//
// 这个接口每次调用都要在分析库里扫事件,只在开会话、准入这类低频场合读,不要轮询。
type Vault struct {
	Vault uint16
	// MainAccount 金库主账户:以金库身份交易时下单、撤单签的就是这个账户号。
	MainAccount uint32
	// Manager manager 账户 owner 的地址;待认领(fresh)金库为 nil。
	Manager *Address
	// TradingHalt 金库熔断中:dex-os 已撤光挂单并强平,只能撤单。
	TradingHalt bool
}

// Vault 读一个金库。404 返回 ErrVaultNotFound;身份字段缺失或自相矛盾一律报错,不补默认值 ——
// 主账户号补成 0 就是把单签到别人的账户上。
func (c *Client) Vault(ctx context.Context, id uint16) (*Vault, error) {
	var wire struct {
		Vault       *uint16         `json:"vault"`
		MainAccount *uint32         `json:"mainAccount"`
		Manager     json.RawMessage `json:"manager"`
		HasManager  *bool           `json:"hasManager"`
		TradingHalt *bool           `json:"tradingHalt"`
	}
	if err := c.do(ctx, http.MethodGet, "/vaults/"+strconv.FormatUint(uint64(id), 10), nil, &wire); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %d", ErrVaultNotFound, id)
		}
		return nil, err
	}
	if wire.Vault == nil || wire.MainAccount == nil || wire.HasManager == nil || wire.TradingHalt == nil || wire.Manager == nil {
		return nil, fmt.Errorf("dexos: 金库 %d 的视图缺 vault/mainAccount/manager/hasManager/tradingHalt", id)
	}
	if *wire.Vault != id {
		return nil, fmt.Errorf("dexos: 金库视图身份不符: 请求 %d 得到 %d", id, *wire.Vault)
	}
	v := &Vault{Vault: id, MainAccount: *wire.MainAccount, TradingHalt: *wire.TradingHalt}
	var manager *string
	if err := json.Unmarshal(wire.Manager, &manager); err != nil {
		return nil, fmt.Errorf("dexos: 金库 %d 的 manager 字段无法解析: %w", id, err)
	}
	if (manager != nil) != *wire.HasManager {
		return nil, fmt.Errorf("dexos: 金库 %d 的 manager 与 hasManager 不一致", id)
	}
	if manager != nil {
		a, err := ParseAddress(*manager)
		if err != nil || a.IsZero() {
			return nil, fmt.Errorf("dexos: 金库 %d 的 manager 地址不合法", id)
		}
		v.Manager = &a
	}
	return v, nil
}
