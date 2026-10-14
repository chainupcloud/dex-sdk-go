package dexosv1

import (
	"context"
	"fmt"
	"strconv"
)

// Vault 一个社区金库(`GET /v1/vaults`、`/v1/vaults/{id}`)里本包用得到的部分:身份与交易限制。地址是 owner 主钱包地址。
// 金额、份额、净值与收益指标本包不解码。
//
// 以金库身份交易([VaultAccount])时,API 钱包的授权挂在 manager 认领金库所用的那个账户上:
// AccountRef{Address: Manager, Subaccount: ManagerSubaccount}。
type Vault struct {
	Vault uint16
	// Name 展示名;没起名的金库网关给 "Vault #N"。
	Name string
	// Creator / CreatorSubaccount 建库的账户;nil = 没有 owner 的内部账户。
	Creator           *string
	CreatorSubaccount *uint32
	// Manager / ManagerSubaccount 认领金库的账户(子账户序号与 AccountRef.Subaccount 同一编号);待认领为 nil。
	Manager           *string
	ManagerSubaccount *uint32
	// TradingHalt 金库熔断中:已撤光挂单并强平,只能撤单。
	TradingHalt bool
	// HasWhitelist 为 false 时不限市场;为 true 时只许 Whitelist 里的市场(删空后仍为 true,哪个都不许)。
	HasWhitelist bool
	Whitelist    []string
	Blacklist    []string
	// MaxLeverages 各市场的杠杆上限(倍数);没设上限的市场不在表里。
	MaxLeverages map[string]uint32
	// DepositorWhitelist 出资人白名单(地址);空 = 不限出资人。
	DepositorWhitelist []string
	// ManagerLiqPortionPpm 熔断线(ppm):manager 自有资金占比低于它即停交易、全撤、强平;nil = 没设。
	ManagerLiqPortionPpm *uint32
}

// wireVaultMarket 白名单 / 黑名单的一项(`VaultMarket`)。
type wireVaultMarket struct {
	Symbol string `json:"symbol"`
}

// wireVaultLeverage 杠杆上限的一项(`VaultMaxLeverage`)。
type wireVaultLeverage struct {
	Symbol      string `json:"symbol"`
	MaxLeverage uint32 `json:"maxLeverage"`
}

// wireVaultConfig `VaultConfig` 里本包读的部分。
type wireVaultConfig struct {
	ManagerLiqPortionPpm *uint32 `json:"managerLiqPortionPpm"`
}

// wireVault `components.schemas.Vault` 里本包读的部分(详情 `VaultDetail` 是它加上详情字段)。
type wireVault struct {
	Vault             uint16              `json:"vault"`
	Name              string              `json:"name"`
	Creator           *string             `json:"creator"`
	CreatorSubaccount *uint32             `json:"creatorSubaccount"`
	Manager           *string             `json:"manager"`
	ManagerSubaccount *uint32             `json:"managerSubaccount"`
	TradingHalt       bool                `json:"tradingHalt"`
	Whitelist         *[]wireVaultMarket  `json:"whitelist"`
	Blacklist         []wireVaultMarket   `json:"blacklist"`
	MaxLeverages      []wireVaultLeverage `json:"maxLeverages"`
	DepositorWL       []string            `json:"depositorWhitelist"`
	Config            wireVaultConfig     `json:"config"`
}

// vault 换成本包的形状。manager 与它的子账户序号必须同时有或同时没有。
func (w wireVault) vault() (*Vault, error) {
	if (w.Manager == nil) != (w.ManagerSubaccount == nil) {
		return nil, fmt.Errorf("dexosv1: 金库 %d 的 manager 与 managerSubaccount 不是同时给出", w.Vault)
	}
	v := &Vault{Vault: w.Vault, Name: w.Name, Creator: w.Creator, CreatorSubaccount: w.CreatorSubaccount,
		Manager: w.Manager, ManagerSubaccount: w.ManagerSubaccount, TradingHalt: w.TradingHalt,
		HasWhitelist: w.Whitelist != nil, MaxLeverages: map[string]uint32{}, DepositorWhitelist: w.DepositorWL,
		ManagerLiqPortionPpm: w.Config.ManagerLiqPortionPpm}
	symbols := func(in []wireVaultMarket) []string {
		out := make([]string, 0, len(in))
		for _, m := range in {
			out = append(out, m.Symbol)
		}
		return out
	}
	if w.Whitelist != nil {
		v.Whitelist = symbols(*w.Whitelist)
	}
	v.Blacklist = symbols(w.Blacklist)
	for _, l := range w.MaxLeverages {
		v.MaxLeverages[l.Symbol] = l.MaxLeverage
	}
	return v, nil
}

// Vaults 金库列表。
func (c *Client) Vaults(ctx context.Context) ([]Vault, error) {
	var resp struct {
		Vaults []wireVault `json:"vaults"`
	}
	if err := c.get(ctx, "/v1/vaults", nil, accountRead, &resp); err != nil {
		return nil, err
	}
	out := make([]Vault, 0, len(resp.Vaults))
	for _, w := range resp.Vaults {
		v, err := w.vault()
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

// Vault 读一个金库;不存在是 404([*Error],Code GatewayNotFound)。
func (c *Client) Vault(ctx context.Context, id uint16) (*Vault, error) {
	var w wireVault
	if err := c.get(ctx, "/v1/vaults/"+strconv.FormatUint(uint64(id), 10), nil, accountRead, &w); err != nil {
		return nil, err
	}
	if w.Vault != id {
		return nil, fmt.Errorf("dexosv1: 问金库 %d,答的是 %d", id, w.Vault)
	}
	return w.vault()
}
