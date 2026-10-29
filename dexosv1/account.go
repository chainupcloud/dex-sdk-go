package dexosv1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// Account 账户汇总(`GET /v1/account`)。金额是 USDC 十进制字符串。
//
// 风险字段与 Epoch / AsOfSeq 取自**同一次**风险视图(v1.1 §4.2):AsOfSeq 是这份视图覆盖到的日志位号,
// Epoch 是账本纪元 —— 纪元变了(账本重置、账户号重排)就要按新纪元重建本地状态,不拿旧游标续读。
type Account struct {
	Address           string `json:"address"`
	Subaccount        uint32 `json:"subaccount"`
	Epoch             string `json:"epoch"`
	AsOfSeq           uint64 `json:"asOfSeq"`
	Collateral        string `json:"collateral"`
	Equity            string `json:"equity"`
	MarginUsed        string `json:"marginUsed"`
	MaintenanceMargin string `json:"maintenanceMargin"`
	// OpenOrderMargin 在簿非只减仓挂单占用的初始保证金。
	OpenOrderMargin     string    `json:"openOrderMargin"`
	AvailableToTrade    string    `json:"availableToTrade"`
	AvailableToWithdraw string    `json:"availableToWithdraw"`
	Balances            []Balance `json:"balances"`
	// Groups 按结算 token 分的风险组(USDC 一组,另有其他抵押 token 的 dex 各一组)。
	Groups []RiskGroup `json:"groups"`
	// FeeTier / TakerFee / MakerFee nil = 服务端给了 null(市场级费率的市场看 Market 的 fees)。
	FeeTier        *int    `json:"feeTier"`
	TakerFee       *string `json:"takerFee"`
	MakerFee       *string `json:"makerFee"`
	PositionsCount int     `json:"positionsCount"`
	OrdersCount    int     `json:"ordersCount"`
	// Leverage 设过自定义杠杆的市场。
	Leverage []MarketLeverage `json:"leverage"`
	// Nonce 主钱包作用域的下一个 nonce(API 钱包的 nonce 用 [Client.Nonce] 带 signer 取)。
	Nonce uint64 `json:"nonce"`
	TS    int64  `json:"ts"`
	// Vault 以金库身份读([VaultAccount])时的金库号;此时服务端不给 Address / Subaccount。
	Vault *uint16 `json:"vault"`
}

// Balance 一种 token 的余额(十进制,token 单位)。
type Balance struct {
	Token     string `json:"token"`
	Amount    string `json:"amount"`
	Hold      string `json:"hold"`
	Available string `json:"available"`
}

// RiskGroup 一个风险组(同一结算 token)。金额是该 token 的十进制字符串。
type RiskGroup struct {
	Token           string `json:"token"`
	Collateral      string `json:"collateral"`
	Equity          string `json:"equity"`
	IMR             string `json:"imr"`
	MMR             string `json:"mmr"`
	OpenOrderMargin string `json:"openOrderMargin"`
	Withdrawable    string `json:"withdrawable"`
}

// MarketLeverage 一个市场上设过的杠杆倍数。
type MarketLeverage struct {
	Symbol   string `json:"symbol"`
	Leverage int    `json:"leverage"`
}

// Account 账户汇总。响应缺 epoch / asOfSeq 时报错:没有它们就判断不了纪元有没有变、视图新不新。
func (c *Client) Account(ctx context.Context, ref AccountRef) (*Account, error) {
	q, err := ref.values()
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := c.get(ctx, "/v1/account", q, accountRead, &raw); err != nil {
		return nil, err
	}
	return decodeAccount(raw, "/v1/account")
}

// decodeAccount 账户汇总的体(REST 响应与 WS `account` 帧同形)。
func decodeAccount(raw json.RawMessage, where string) (*Account, error) {
	var a Account
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("dexosv1: %s 解析失败: %w", where, err)
	}
	var head struct {
		Epoch   *string `json:"epoch"`
		AsOfSeq *uint64 `json:"asOfSeq"`
	}
	if err := json.Unmarshal(raw, &head); err != nil || head.Epoch == nil || *head.Epoch == "" || head.AsOfSeq == nil {
		return nil, fmt.Errorf("dexosv1: %s 缺 epoch / asOfSeq: %s", where, raw)
	}
	return &a, nil
}

// Position 一个持仓。Lots 带符号:多为正、空为负(同 Hyperliquid 的 szi),与 Side 一致;MarkPrice 是 tick。
// 开仓均价是派生的平均值,未必落在 tick 上,保持十进制字符串。
type Position struct {
	Symbol        string
	Side          string // "long" | "short"
	Lots          int64
	EntryPrice    string
	MarkPrice     uint32
	Notional      string // USDC
	UnrealizedPnl string // USDC,带符号
	FundingIndex  string
}

type wirePosition struct {
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`
	Size          string `json:"size"`
	EntryPrice    string `json:"entryPrice"`
	MarkPrice     string `json:"markPrice"`
	Notional      string `json:"notional"`
	UnrealizedPnl string `json:"unrealizedPnl"`
	FundingIndex  string `json:"fundingIndex"`
}

func (c *Client) Positions(ctx context.Context, cat *Catalog, ref AccountRef) ([]Position, error) {
	q, err := ref.values()
	if err != nil {
		return nil, err
	}
	var resp struct {
		Positions []wirePosition `json:"positions"`
	}
	if err := c.get(ctx, "/v1/account/positions", q, accountRead, &resp); err != nil {
		return nil, err
	}
	return positionsFrom(cat, resp.Positions)
}

// positionsFrom 持仓行(REST 响应与 WS `positions` 帧同形)换算成 tick / lot。
func positionsFrom(cat *Catalog, in []wirePosition) ([]Position, error) {
	out := make([]Position, 0, len(in))
	for _, w := range in {
		m, err := cat.rowMarket(w.Symbol)
		if err != nil {
			return nil, err
		}
		p := Position{Symbol: w.Symbol, Side: w.Side, EntryPrice: w.EntryPrice, Notional: w.Notional,
			UnrealizedPnl: w.UnrealizedPnl, FundingIndex: w.FundingIndex}
		if p.Lots, err = m.Scale.signedLots(w.Size); err != nil {
			return nil, fmt.Errorf("dexosv1: 持仓 %s size: %w", w.Symbol, err)
		}
		// 方向与符号要一致:服务端按 lots > 0 为 long、否则 short 给 side(gateway positions_json);对不上不猜
		if (w.Side != "long" && w.Side != "short") || (w.Side == "long") != (p.Lots > 0) {
			return nil, fmt.Errorf("dexosv1: 持仓 %s 的 side %q 与 size %q 的符号不一致", w.Symbol, w.Side, w.Size)
		}
		if p.MarkPrice, err = m.Scale.Ticks(w.MarkPrice); err != nil {
			return nil, fmt.Errorf("dexosv1: 持仓 %s markPrice: %w", w.Symbol, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// Order 一张挂单:在簿的限价单,或待触发的条件单(Trigger 非 nil;止损 / 止盈,含仓位 TP/SL 腿)。
// 条件单的 Side / Price / Lots / TIF 是触发后子单的,Status 同样是 open,也能按 OrderID 撤(v1.1 §4.10-7)。
// OrderID 是执行层订单序号(与 dexos 包的订单 seq 同一个数)。
type Order struct {
	OrderID uint64
	// ClientOrderID nil = 没有(只有带 clientOrderId 且等结论下单的单才有映射)。
	ClientOrderID *string
	Symbol        string
	Side          string // "buy" | "sell"
	Price         uint32
	Lots          uint64
	FilledLots    uint64
	RemainingLots uint64
	Status        string
	// TIF "gtc" | "ioc" | "alo" | "fok",只有条件单有;nil = 在簿挂单(执行层不保留簿上单的 tif,`alo` 进簿后同 `gtc`)。
	TIF *string
	// ReduceOnly nil = 账户的挂单台账里没有这张单的记录。
	ReduceOnly *bool
	// Trigger nil = 在簿挂单。
	Trigger *OrderTrigger
}

// OrderTrigger 条件单的触发条件:预言机价越过 Price 时,子单按限价(Order 的 Side / Price / Lots / TIF)进簿。
type OrderTrigger struct {
	Price uint32
	// Above true = 预言机价 ≥ Price 时触发;false = ≤ Price 时触发。
	Above bool
	// PositionTpsl 仓位级 TP/SL:仓位归零时自动撤。
	PositionTpsl bool
}

type wireOrder struct {
	OrderID       string  `json:"orderId"`
	ClientOrderID *string `json:"clientOrderId"`
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`
	Price         string  `json:"price"`
	Size          string  `json:"size"`
	Filled        string  `json:"filled"`
	Remaining     string  `json:"remaining"`
	Status        string  `json:"status"`
	TIF           *string `json:"tif"`
	ReduceOnly    *bool   `json:"reduceOnly"`
	// Trigger 条件单的触发条件;在簿挂单为 null
	Trigger *wireOrderTrigger `json:"trigger"`
}

type wireOrderTrigger struct {
	Price        string `json:"price"`
	Above        bool   `json:"above"`
	PositionTpsl bool   `json:"positionTpsl"`
}

// Orders 账户的挂单(含待触发的条件单,按 Order.Trigger 区分);symbol 为空 = 全市场。
func (c *Client) Orders(ctx context.Context, cat *Catalog, ref AccountRef, symbol string) ([]Order, error) {
	q, err := ref.values()
	if err != nil {
		return nil, err
	}
	if symbol != "" {
		q.Set("symbol", symbol)
	}
	var resp struct {
		Orders []wireOrder `json:"orders"`
	}
	if err := c.get(ctx, "/v1/account/orders", q, accountRead, &resp); err != nil {
		return nil, err
	}
	out := make([]Order, 0, len(resp.Orders))
	for _, w := range resp.Orders {
		o, err := w.order(cat)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// order 挂单行(REST 响应与 WS `orders` 快照同形)换算成 tick / lot。
func (w wireOrder) order(cat *Catalog) (Order, error) {
	m, err := cat.rowMarket(w.Symbol)
	if err != nil {
		return Order{}, err
	}
	o := Order{ClientOrderID: w.ClientOrderID, Symbol: w.Symbol, Side: w.Side, Status: w.Status, TIF: w.TIF, ReduceOnly: w.ReduceOnly}
	if o.OrderID, err = strconv.ParseUint(w.OrderID, 10, 64); err != nil {
		return Order{}, fmt.Errorf("dexosv1: 挂单 orderId %q: %w", w.OrderID, err)
	}
	if o.Price, err = m.Scale.Ticks(w.Price); err != nil {
		return Order{}, fmt.Errorf("dexosv1: 挂单 %s price: %w", w.OrderID, err)
	}
	for _, f := range []struct {
		name string
		in   string
		out  *uint64
	}{{"size", w.Size, &o.Lots}, {"filled", w.Filled, &o.FilledLots}, {"remaining", w.Remaining, &o.RemainingLots}} {
		if *f.out, err = m.Scale.Lots(f.in); err != nil {
			return Order{}, fmt.Errorf("dexosv1: 挂单 %s %s: %w", w.OrderID, f.name, err)
		}
	}
	if w.Trigger != nil {
		o.Trigger = &OrderTrigger{Above: w.Trigger.Above, PositionTpsl: w.Trigger.PositionTpsl}
		if o.Trigger.Price, err = m.Scale.Ticks(w.Trigger.Price); err != nil {
			return Order{}, fmt.Errorf("dexosv1: 挂单 %s trigger.price: %w", w.OrderID, err)
		}
	}
	return o, nil
}

// Nonce 下一个 nonce。Scope "account" = 主钱包作用域,"agent" = API 钱包作用域(两个独立计数器,
// 混用得到 NonceMismatch)。SigningAccountID **只供签名**:填 EIP-712 结构体的 accountId。
type Nonce struct {
	Scope            string `json:"scope"`
	Nonce            uint64 `json:"nonce"`
	SigningAccountID uint32 `json:"signingAccountId"`
}

// Nonce 取下一个 nonce;signer 非 nil 时取这把 API 钱包在该账户上的 nonce。
// 金库身份时 SigningAccountID 是金库主账户的。
func (c *Client) Nonce(ctx context.Context, ref AccountRef, signer *dexos.Address) (*Nonce, error) {
	q, err := ref.values()
	if err != nil {
		return nil, err
	}
	if signer != nil {
		q.Set("signer", signer.Hex())
	}
	var n Nonce
	if err := c.get(ctx, "/v1/account/nonce", q, accountRead, &n); err != nil {
		return nil, err
	}
	want := "account"
	if signer != nil {
		want = "agent"
	}
	if n.Scope != want {
		return nil, fmt.Errorf("dexosv1: nonce 作用域应为 %s,服务端答 %q", want, n.Scope)
	}
	return &n, nil
}

// Agent 一把已授权的 API 钱包。ValidUntil nil = 不过期。
type Agent struct {
	Address    string `json:"address"`
	Label      string `json:"label"`
	ValidUntil *int64 `json:"validUntil"`
	Expired    bool   `json:"expired"`
}

// Agents 账户上授权的 API 钱包。不接受金库身份:代金库交易的 API 钱包授权在 manager 认领金库所用的
// 账户上([Vault] 的 Manager / ManagerSubaccount)。
func (c *Client) Agents(ctx context.Context, ref AccountRef) ([]Agent, error) {
	q, err := ref.walletValues("API 钱包列表")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Agents []Agent `json:"agents"`
	}
	if err := c.get(ctx, "/v1/account/agents", q, accountRead, &resp); err != nil {
		return nil, err
	}
	return resp.Agents, nil
}

// AgentGrant 一把 API 钱包被授权代表的一个账户(`GET /v1/agents/{address}`)。
// Address 是被代表账户的主钱包地址;金库授权时是 manager 的主钱包,Vault 给出金库号、Subaccount 为 nil。
type AgentGrant struct {
	Address    string  `json:"address"`
	Subaccount *uint32 `json:"subaccount"`
	Vault      *uint16 `json:"vault"`
	Label      string  `json:"label"`
	ValidUntil *int64  `json:"validUntil"`
	// Expired 过期的授权也列出(需要续期,不是被撤销)。
	Expired bool `json:"expired"`
}

// AgentGrants 按 API 钱包地址反查它被授权的账户。只凭 API 钱包私钥起步的进程用它找到要交易的账户。
func (c *Client) AgentGrants(ctx context.Context, agent dexos.Address) ([]AgentGrant, error) {
	var resp struct {
		Agent  string       `json:"agent"`
		Grants []AgentGrant `json:"grants"`
	}
	if err := c.get(ctx, "/v1/agents/"+url.PathEscape(agent.Hex()), nil, accountRead, &resp); err != nil {
		return nil, err
	}
	if !strings.EqualFold(resp.Agent, agent.Hex()) {
		return nil, fmt.Errorf("dexosv1: 问 %s 的授权,答的是 %q", agent.Hex(), resp.Agent)
	}
	return resp.Grants, nil
}

// VaultPosition 账户在一个金库里的头寸(`GET /v1/account/vaults`)。金额是 USDC 十进制字符串。
type VaultPosition struct {
	Vault uint16 `json:"vault"`
	Name  string `json:"name"`
	// Class "lp" | "manager"。
	Class         string `json:"class"`
	Shares        string `json:"shares"`
	BalanceShare  string `json:"balanceShare"`
	Equity        string `json:"equity"`
	InitialEquity string `json:"initialEquity"`
	AllTimePnl    string `json:"allTimePnl"`
	LockUntilMs   int64  `json:"lockUntilMs"`
	Locked        bool   `json:"locked"`
	LockedShares  string `json:"lockedShares"`
	Withdrawable  struct {
		OK     bool    `json:"ok"`
		Reason *string `json:"reason"`
	} `json:"withdrawable"`
}

// MyVaults 主钱包名下在各金库的头寸。
func (c *Client) MyVaults(ctx context.Context, owner dexos.Address) ([]VaultPosition, error) {
	var resp struct {
		Positions []VaultPosition `json:"positions"`
	}
	if err := c.get(ctx, "/v1/account/vaults", url.Values{"address": {owner.Hex()}}, accountRead, &resp); err != nil {
		return nil, err
	}
	return resp.Positions, nil
}
