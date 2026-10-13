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

// Vault 金库视图里以金库身份交易、做准入要用的字段(GET /vaults/:id、GET /vaults)。
//
// 这两个接口每次调用都要在分析库里扫事件,只在开会话、导入、准入这类低频场合读,不要轮询。
type Vault struct {
	Vault uint16
	// Name 展示名;没起名的金库网关给 "Vault #N"。
	Name string
	// MainAccount 金库主账户:以金库身份交易时下单、撤单签的就是这个账户号。
	MainAccount uint32
	// Manager manager 账户 owner 的地址;待认领(fresh)金库为 nil。
	Manager *Address
	// TradingHalt 金库熔断中:dex-os 已撤光挂单并强平,只能撤单。
	TradingHalt bool
	// HasWhitelist 为 false 时不限市场;为 true 时只许 Whitelist 里的市场 ——
	// 白名单删空后仍是 true,此时 Whitelist 为空,哪个市场都不许。
	HasWhitelist bool
	Whitelist    []uint16
	// Blacklist 禁止交易的市场。
	Blacklist []uint16
	// MaxLeverages 各市场的杠杆上限(倍数);没设上限的市场不在表里。
	MaxLeverages map[uint16]uint32
	// DepositorWhitelist 出资人白名单(地址);空表 = 不限出资人。
	DepositorWhitelist []Address
	// ManagerLiqPortionPpm 熔断线(ppm):manager 自有资金占比在块边界低于它即停交易、全撤、强平;
	// nil = 没设,永不熔断。建库后不可改。
	ManagerLiqPortionPpm *uint32
}

// Vault 读一个金库。404 返回 ErrVaultNotFound;字段缺失或自相矛盾一律报错,不补默认值 ——
// 主账户号补成 0 就是把单签到别人的账户上,白名单缺失补成「不限」就是放过了限制。
func (c *Client) Vault(ctx context.Context, id uint16) (*Vault, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/vaults/"+strconv.FormatUint(uint64(id), 10), nil, &raw); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %d", ErrVaultNotFound, id)
		}
		return nil, err
	}
	v, err := parseVault(raw)
	if err != nil {
		return nil, err
	}
	if v.Vault != id {
		return nil, fmt.Errorf("dexos: 金库视图身份不符: 请求 %d 得到 %d", id, v.Vault)
	}
	return v, nil
}

// Vaults 列出全部金库,逐行与 Vault 同一套解析与校验;任何一行不合格整次报错。
func (c *Client) Vaults(ctx context.Context) ([]Vault, error) {
	var r struct {
		Vaults *[]json.RawMessage `json:"vaults"`
	}
	if err := c.do(ctx, http.MethodGet, "/vaults", nil, &r); err != nil {
		return nil, err
	}
	if r.Vaults == nil {
		return nil, errors.New("dexos: 金库列表缺失")
	}
	out := make([]Vault, 0, len(*r.Vaults))
	seen := make(map[uint16]bool, len(*r.Vaults))
	for _, raw := range *r.Vaults {
		v, err := parseVault(raw)
		if err != nil {
			return nil, err
		}
		if seen[v.Vault] {
			return nil, fmt.Errorf("dexos: 金库列表里金库 %d 重复", v.Vault)
		}
		seen[v.Vault] = true
		out = append(out, *v)
	}
	return out, nil
}

type vaultMarketRef struct {
	Market *uint16 `json:"market"`
}

func parseVault(raw json.RawMessage) (*Vault, error) {
	var wire struct {
		Vault       *uint16           `json:"vault"`
		Name        *string           `json:"name"`
		MainAccount *uint32           `json:"mainAccount"`
		Manager     json.RawMessage   `json:"manager"`
		HasManager  *bool             `json:"hasManager"`
		TradingHalt *bool             `json:"tradingHalt"`
		Whitelist   json.RawMessage   `json:"whitelist"`
		Blacklist   *[]vaultMarketRef `json:"blacklist"`
		MaxLevs     *[]struct {
			Market      *uint16 `json:"market"`
			MaxLeverage *uint32 `json:"maxLeverage"`
		} `json:"maxLeverages"`
		Depositors *[]string `json:"depositorWhitelist"`
		Config     *struct {
			ManagerLiqPortionPpm json.RawMessage `json:"managerLiqPortionPpm"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("dexos: 金库视图无法解析: %w", err)
	}
	if wire.Vault == nil {
		return nil, errors.New("dexos: 金库视图缺 vault")
	}
	id := *wire.Vault
	if wire.Name == nil || wire.MainAccount == nil || wire.Manager == nil || wire.HasManager == nil ||
		wire.TradingHalt == nil || wire.Whitelist == nil || wire.Blacklist == nil || wire.MaxLevs == nil ||
		wire.Depositors == nil || wire.Config == nil || wire.Config.ManagerLiqPortionPpm == nil {
		return nil, fmt.Errorf("dexos: 金库 %d 的视图缺 name/mainAccount/manager/hasManager/tradingHalt/"+
			"whitelist/blacklist/maxLeverages/depositorWhitelist/config.managerLiqPortionPpm", id)
	}
	v := &Vault{Vault: id, Name: *wire.Name, MainAccount: *wire.MainAccount, TradingHalt: *wire.TradingHalt}

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

	var whitelist *[]vaultMarketRef
	if err := json.Unmarshal(wire.Whitelist, &whitelist); err != nil {
		return nil, fmt.Errorf("dexos: 金库 %d 的 whitelist 无法解析: %w", id, err)
	}
	var err error
	if whitelist != nil {
		v.HasWhitelist = true
		if v.Whitelist, err = vaultMarkets(id, "whitelist", *whitelist); err != nil {
			return nil, err
		}
	}
	if v.Blacklist, err = vaultMarkets(id, "blacklist", *wire.Blacklist); err != nil {
		return nil, err
	}

	v.MaxLeverages = make(map[uint16]uint32, len(*wire.MaxLevs))
	for _, l := range *wire.MaxLevs {
		if l.Market == nil || l.MaxLeverage == nil {
			return nil, fmt.Errorf("dexos: 金库 %d 的 maxLeverages 缺 market/maxLeverage", id)
		}
		if _, dup := v.MaxLeverages[*l.Market]; dup {
			return nil, fmt.Errorf("dexos: 金库 %d 的 maxLeverages 里市场 %d 重复", id, *l.Market)
		}
		v.MaxLeverages[*l.Market] = *l.MaxLeverage
	}

	v.DepositorWhitelist = make([]Address, 0, len(*wire.Depositors))
	for _, s := range *wire.Depositors {
		a, err := ParseAddress(s)
		if err != nil || a.IsZero() {
			return nil, fmt.Errorf("dexos: 金库 %d 的出资人白名单地址不合法: %q", id, s)
		}
		v.DepositorWhitelist = append(v.DepositorWhitelist, a)
	}

	if err := json.Unmarshal(wire.Config.ManagerLiqPortionPpm, &v.ManagerLiqPortionPpm); err != nil {
		return nil, fmt.Errorf("dexos: 金库 %d 的 config.managerLiqPortionPpm 无法解析: %w", id, err)
	}
	return v, nil
}

func vaultMarkets(id uint16, field string, refs []vaultMarketRef) ([]uint16, error) {
	out := make([]uint16, 0, len(refs))
	for _, r := range refs {
		if r.Market == nil {
			return nil, fmt.Errorf("dexos: 金库 %d 的 %s 缺 market", id, field)
		}
		out = append(out, *r.Market)
	}
	return out, nil
}
