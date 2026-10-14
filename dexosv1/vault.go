package dexosv1

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/chainupcloud/dex-sdk-go/dexos"
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

// ── 金库写:主钱包签名([Wallet]) ──
//
// 金额是 USDC 十进制字符串(`"100.5"`,最多 6 位小数,不整除就报错、不舍入);份额是整数份额单位的字符串,与读接口
// 里的 `lpShares` / `managerShares` / [VaultPosition] 的 Shares 同一单位;比例是 ppm。签名签的是原子单位的整数,
// 线上按 6 位小数的十进制发。金库写只接受主钱包(API 钱包 403),也不接受金库身份。

// vaultDecimals 金库的金额(USDC)与份额上线时的小数位。
const vaultDecimals = 6

// vaultAmount 解析 USDC 十进制金额;positive 时必须大于 0。返回原子单位与上线的十进制写法。
func vaultAmount(what, s string, positive bool) (*big.Int, string, error) {
	v, err := ParseFixed(s, vaultDecimals)
	if err != nil {
		return nil, "", fmt.Errorf("dexosv1: %s %q: %w", what, s, err)
	}
	if v.Sign() < 0 || (positive && v.Sign() == 0) || v.BitLen() > 128 {
		return nil, "", fmt.Errorf("dexosv1: %s %q 应为正数且不超过 uint128", what, s)
	}
	return v, FormatFixed(v, vaultDecimals), nil
}

// VaultCreate 新建金库(`POST /v1/vaults`)的参数。零值的可选项 = 不设。比例、最短存期与存取窗口建库后不能改;
// 存款上限、交易白黑名单与杠杆上限用 [Wallet.ChangeVaultConfig] 改。
type VaultCreate struct {
	// Name 展示名,全局唯一(重名 409);不进签名。
	Name        string
	Description string
	// Manager 指定 manager 的主钱包;nil = 待认领,之后由人用 [Wallet.ClaimVault] 认领。
	Manager *dexos.Address
	// ProfitSharePpm manager 的业绩分成(ppm,200000 = 20%)。
	ProfitSharePpm uint32
	// DepositMinDurationMs 每笔存入的锁定期(毫秒)。
	DepositMinDurationMs uint64
	// ManagerMinPortionPpm / ManagerLiqPortionPpm manager 自有资金占比的下限与熔断线(ppm);熔断线要低于下限。
	ManagerMinPortionPpm uint32
	ManagerLiqPortionPpm uint32
	// WithdrawWindowS / WithdrawDurationS 赎回窗口的周期与每期开放时长(秒),两个一起设或都不设。
	WithdrawWindowS   uint32
	WithdrawDurationS uint32
	// DepositCap 存款上限(USDC);"" = 不限。
	DepositCap string
	// SeedAmount 建库时 creator 的首笔存入(USDC);"" = 不存。
	SeedAmount string
	// DepositorWhitelist 出资人白名单;空 = 谁都能存。
	DepositorWhitelist []dexos.Address
}

// VaultCreated 建库的结论:回执与新金库号。
type VaultCreated struct {
	Receipt
	Vault uint16
}

type wireVaultCreateReq struct {
	*envelope
	Name                 string   `json:"name"`
	Description          string   `json:"description,omitempty"`
	Manager              string   `json:"manager,omitempty"`
	ProfitShare          string   `json:"profitShare,omitempty"`
	DepositMinDurationMs uint64   `json:"depositMinDurationMs,omitempty"`
	ManagerMinPortion    string   `json:"managerMinPortion,omitempty"`
	ManagerLiqPortion    string   `json:"managerLiqPortion,omitempty"`
	WithdrawWindowS      uint32   `json:"withdrawWindowS,omitempty"`
	WithdrawDurationS    uint32   `json:"withdrawDurationS,omitempty"`
	DepositCap           string   `json:"depositCap,omitempty"`
	SeedAmount           string   `json:"seedAmount,omitempty"`
	DepositorWhitelist   []string `json:"depositorWhitelist,omitempty"`
}

// ratio ppm → 线上的十进制比例(200000 → "0.2");0 = 不设,不上线。
func ratio(ppm uint32) string {
	if ppm == 0 {
		return ""
	}
	return FormatFixed(big.NewInt(int64(ppm)), vaultDecimals)
}

// CreateVault 以本账户为 creator 新建金库(主钱包签 CreateManagedVault)。建库费与种子资金从本账户扣。
func (w *Wallet) CreateVault(ctx context.Context, p VaultCreate) (*VaultCreated, error) {
	if strings.TrimSpace(p.Name) == "" {
		return nil, errors.New("dexosv1: 金库要有名字")
	}
	mv := managedVault{owner: w.Owner.Address(), accountID: w.signingID, profitSharePpm: p.ProfitSharePpm,
		depositMinDurationMs: p.DepositMinDurationMs, managerMinPortionPpm: p.ManagerMinPortionPpm,
		managerLiqPortionPpm: p.ManagerLiqPortionPpm, withdrawWindowS: p.WithdrawWindowS, withdrawDurationS: p.WithdrawDurationS,
		depositCap: new(big.Int), seedAmount: new(big.Int), depositorWhitelist: p.DepositorWhitelist}
	req := wireVaultCreateReq{Name: p.Name, Description: p.Description, ProfitShare: ratio(p.ProfitSharePpm),
		DepositMinDurationMs: p.DepositMinDurationMs, ManagerMinPortion: ratio(p.ManagerMinPortionPpm),
		ManagerLiqPortion: ratio(p.ManagerLiqPortionPpm), WithdrawWindowS: p.WithdrawWindowS, WithdrawDurationS: p.WithdrawDurationS}
	if p.Manager != nil {
		mv.manager, req.Manager = *p.Manager, p.Manager.Hex()
	}
	for _, a := range []struct {
		what string
		in   string
		out  **big.Int
		wire *string
	}{{"存款上限", p.DepositCap, &mv.depositCap, &req.DepositCap}, {"种子资金", p.SeedAmount, &mv.seedAmount, &req.SeedAmount}} {
		if a.in == "" {
			continue
		}
		v, wire, err := vaultAmount(a.what, a.in, false)
		if err != nil {
			return nil, err
		}
		if v.Sign() > 0 {
			*a.out, *a.wire = v, wire
		}
	}
	for _, a := range p.DepositorWhitelist {
		req.DepositorWhitelist = append(req.DepositorWhitelist, a.Hex())
	}
	var out *VaultCreated
	err := w.guard.run(ctx, func(nonce uint64) (*RequestIdentity, error) {
		mv.nonce = nonce
		env, id, err := w.sign(nonce, createManagedVaultHash(w.domain, mv))
		if err != nil {
			return id, err
		}
		req.envelope = env
		status, raw, err := w.Client.post(ctx, "/v1/vaults", req)
		if err != nil {
			return id, err
		}
		r, err := receipt(status, raw, id)
		if err != nil {
			return id, err
		}
		var wr struct {
			Vault *uint16 `json:"vault"`
		}
		if err := decodeInto(raw, &wr, "建库回执"); err != nil {
			return id, err
		}
		if wr.Vault == nil {
			return id, fmt.Errorf("dexosv1: 建库回执没有金库号: %s", raw)
		}
		out = &VaultCreated{Receipt: r, Vault: *wr.Vault}
		return id, nil
	})
	return out, err
}

// wireVaultAmountReq 存入 / 认领的请求体;wireVaultSharesReq 赎回的。
type wireVaultAmountReq struct {
	*envelope
	Amount string `json:"amount"`
}

type wireVaultSharesReq struct {
	*envelope
	Shares string `json:"shares"`
}

// vaultWrite 主钱包签一条针对金库 vault 的命令,提交到 `/v1/vaults/{vault}/{action}`;回执里的金库号要是这一个。
func (w *Wallet) vaultWrite(ctx context.Context, vault uint16, action string, digest func(nonce uint64) []byte, body func(env *envelope) any) (*Receipt, error) {
	path := "/v1/vaults/" + strconv.FormatUint(uint64(vault), 10) + "/" + action
	var out Receipt
	err := w.guard.run(ctx, func(nonce uint64) (*RequestIdentity, error) {
		env, id, err := w.sign(nonce, digest(nonce))
		if err != nil {
			return id, err
		}
		status, raw, err := w.Client.post(ctx, path, body(env))
		if err != nil {
			return id, err
		}
		if out, err = receipt(status, raw, id); err != nil {
			return id, err
		}
		var wr struct {
			Vault *uint16 `json:"vault"`
		}
		if err := decodeInto(raw, &wr, "金库回执"); err != nil {
			return id, err
		}
		if wr.Vault == nil || *wr.Vault != vault {
			return id, fmt.Errorf("dexosv1: %s 金库 %d,回执是 %s", action, vault, raw)
		}
		return id, nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DepositToVault 向金库存入 amount USDC(主钱包签 VaultDeposit)。份额在下一次折叠铸出,读 [Client.MyVaults] 看结果。
func (w *Wallet) DepositToVault(ctx context.Context, vault uint16, amount string) (*Receipt, error) {
	v, wire, err := vaultAmount("存入金额", amount, true)
	if err != nil {
		return nil, err
	}
	return w.vaultWrite(ctx, vault, "deposit", func(nonce uint64) []byte {
		return vaultAmountHash(w.domain, vaultDepositType, w.Owner.Address(), vault, w.signingID, v, nonce)
	}, func(env *envelope) any { return wireVaultAmountReq{envelope: env, Amount: wire} })
}

// RedeemFromVault 赎回 shares 份额(整数份额单位,主钱包签 VaultRedeem)。锁定期内、赎回窗口外服务端 422。
func (w *Wallet) RedeemFromVault(ctx context.Context, vault uint16, shares string) (*Receipt, error) {
	v, err := ParseFixed(shares, 0)
	if err != nil || v.Sign() <= 0 || v.BitLen() > 128 {
		return nil, fmt.Errorf("dexosv1: 赎回份额 %q 应为正整数份额单位", shares)
	}
	wire := FormatFixed(v, vaultDecimals)
	return w.vaultWrite(ctx, vault, "redeem", func(nonce uint64) []byte {
		return vaultAmountHash(w.domain, vaultRedeemType, w.Owner.Address(), vault, w.signingID, v, nonce)
	}, func(env *envelope) any { return wireVaultSharesReq{envelope: env, Shares: wire} })
}

// ClaimVault 认领待认领的金库成为 manager,amount 是 manager 的首笔存入(USDC,主钱包签 ClaimVaultManager)。
// 已有 manager 时服务端 422 VaultAlreadyHasManager。
func (w *Wallet) ClaimVault(ctx context.Context, vault uint16, amount string) (*Receipt, error) {
	v, wire, err := vaultAmount("认领存入金额", amount, true)
	if err != nil {
		return nil, err
	}
	return w.vaultWrite(ctx, vault, "claim", func(nonce uint64) []byte {
		return vaultAmountHash(w.domain, claimVaultManagerType, w.Owner.Address(), vault, w.signingID, v, nonce)
	}, func(env *envelope) any { return wireVaultAmountReq{envelope: env, Amount: wire} })
}

// 金库可变配置的种类([VaultConfigChange] 的 Kind),编号是签名里的 kind。
const (
	VaultConfigDepositCap        = "depositCap"
	VaultConfigAddWhitelist      = "addWhitelist"
	VaultConfigRemoveWhitelist   = "removeWhitelist"
	VaultConfigClearWhitelist    = "clearWhitelist"
	VaultConfigAddBlacklist      = "addBlacklist"
	VaultConfigRemoveBlacklist   = "removeBlacklist"
	VaultConfigSetMaxLeverage    = "setMaxLeverage"
	VaultConfigRemoveMaxLeverage = "removeMaxLeverage"
)

var vaultConfigKinds = map[string]uint8{
	VaultConfigDepositCap: 0, VaultConfigAddWhitelist: 1, VaultConfigRemoveWhitelist: 2, VaultConfigClearWhitelist: 3,
	VaultConfigAddBlacklist: 4, VaultConfigRemoveBlacklist: 5, VaultConfigSetMaxLeverage: 6, VaultConfigRemoveMaxLeverage: 7,
}

// VaultConfigChange 金库的一项可变配置(`POST /v1/vaults/{id}/config`,只有 creator 能改)。
// 与 Kind 无关的字段必须是零值(服务端不看它们,也不签进去)。
type VaultConfigChange struct {
	Kind string
	// Markets 涉及的市场:加 / 删白名单与黑名单、设 / 删杠杆上限时给。
	Markets []*Market
	// Cap 存款上限(USDC,只用于 VaultConfigDepositCap);"" = 取消上限。
	Cap string
	// MaxLeverage 杠杆上限倍数(只用于 VaultConfigSetMaxLeverage)。
	MaxLeverage uint32
}

type wireVaultConfigReq struct {
	*envelope
	Kind        string   `json:"kind"`
	Markets     []string `json:"markets,omitempty"`
	Cap         string   `json:"cap,omitempty"`
	MaxLeverage uint32   `json:"maxLeverage,omitempty"`
}

// ChangeVaultConfig 改金库的一项可变配置(主钱包签 VaultConfigChange)。下一笔交易起生效。
func (w *Wallet) ChangeVaultConfig(ctx context.Context, vault uint16, c VaultConfigChange) (*Receipt, error) {
	kind, ok := vaultConfigKinds[c.Kind]
	if !ok {
		return nil, fmt.Errorf("dexosv1: 未知的金库配置种类 %q", c.Kind)
	}
	usesMarkets := c.Kind != VaultConfigDepositCap && c.Kind != VaultConfigClearWhitelist
	switch {
	case !usesMarkets && len(c.Markets) > 0, c.Kind != VaultConfigDepositCap && c.Cap != "",
		c.Kind != VaultConfigSetMaxLeverage && c.MaxLeverage != 0:
		return nil, fmt.Errorf("dexosv1: 金库配置 %s 带了与它无关的字段: %+v", c.Kind, c)
	}
	req := wireVaultConfigReq{Kind: c.Kind, MaxLeverage: c.MaxLeverage}
	limit := new(big.Int)
	if c.Cap != "" {
		v, wire, err := vaultAmount("存款上限", c.Cap, false)
		if err != nil {
			return nil, err
		}
		if v.Sign() > 0 {
			limit, req.Cap = v, wire
		}
	}
	ids := make([]uint16, 0, len(c.Markets))
	for _, m := range c.Markets {
		if m == nil {
			return nil, errors.New("dexosv1: 金库配置的市场为空")
		}
		ids, req.Markets = append(ids, m.SigningMarketID), append(req.Markets, m.Symbol)
	}
	markets := make([]byte, 32)
	if usesMarkets {
		markets = marketsHash(ids)
	}
	return w.vaultWrite(ctx, vault, "config", func(nonce uint64) []byte {
		return vaultConfigChangeHash(w.domain, w.Owner.Address(), vault, w.signingID, kind, limit, c.MaxLeverage, markets, nonce)
	}, func(env *envelope) any {
		req.envelope = env
		return req
	})
}
