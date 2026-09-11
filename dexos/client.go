package dexos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Client dex-os 网关的 REST 客户端。
//
// 两类写操作,签名主体不同:
//   - **主账号签名**:ApproveAgent / RevokeAgent —— 管理 API 钱包本身
//   - **API 钱包签名**:BatchPlace / BatchCancel / BatchReplace —— 日常交易
//
// 这个分界就是账号体系的全部:主账号密钥留在冷处,API 钱包密钥放在跑策略的机器上。
// API 钱包**动不了钱**(提款/转账不在 STF 的 agent scope 白名单内),
// 所以策略机被攻破的最坏后果是被代为交易,而不是资产被提走。
type Client struct {
	BaseURL string
	Domain  Domain
	HTTP    *http.Client
}

// NewClient baseURL 形如 http://127.0.0.1:8080。chainID 必须与节点一致
// (可用 Version() 核对),否则 EIP-712 域分隔符不同,签名一律被拒。
func NewClient(baseURL string, chainID uint64) *Client {
	return &Client{
		BaseURL: baseURL,
		Domain:  Domain{ChainID: chainID},
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

// APIError 网关返回的非 2xx。保留状态码与原始报文 —— 拒因是语义的一部分,
// 吞掉它只剩「请求失败」,调用方无从区分「保证金不足」和「签名不对」。
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("请求体序列化失败:%w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("响应解析失败(%s):%w", string(raw), err)
	}
	return nil
}

// ─────────────────────────── 只读 ───────────────────────────

// Version 节点的状态机语义指纹。SDK 启动时值得核一次 chainId 与 codecVer:
// codec 版本对不上意味着规范编码可能已经变了,而那种错误只表现为「签名一律被拒」。
type Version struct {
	Pkg         string `json:"pkg"`
	SnapshotVer int    `json:"snapshot_ver"`
	CodecVer    int    `json:"codec_ver"`
	BuildID     string `json:"build_id"`
}

func (c *Client) Version(ctx context.Context) (*Version, error) {
	var v Version
	return &v, c.do(ctx, http.MethodGet, "/version", nil, &v)
}

// Market 市场概览。价格均为 tick,数量均为 lot —— 换算成人类单位要用
// PriceDecimals / SizeDecimals,SDK 不替你做,避免在整数域外引入浮点。
type Market struct {
	Market                 uint16 `json:"market"`
	Symbol                 string `json:"symbol"`
	Status                 string `json:"status"`
	Oracle                 uint32 `json:"oracle"`
	Mark                   uint32 `json:"mark"`
	BestBid                uint32 `json:"bestBid"`
	BestAsk                uint32 `json:"bestAsk"`
	OpenInterestLots       int64  `json:"openInterestLots"`
	FundingRatePpm         int64  `json:"fundingRatePpm"`
	FundingIndex           string `json:"fundingIndex"`
	InitialMarginPpm       uint32 `json:"initialMarginPpm"`
	MaintenanceFractionPpm uint32 `json:"maintenanceFractionPpm"`
	QuotePerTickLot        uint64 `json:"quotePerTickLot"`
	PriceDecimals          int    `json:"priceDecimals"`
	SizeDecimals           int    `json:"sizeDecimals"`
}

func (c *Client) Markets(ctx context.Context) ([]Market, error) {
	var r struct {
		Markets []Market `json:"markets"`
	}
	return r.Markets, c.do(ctx, http.MethodGet, "/markets", nil, &r)
}

// Level 盘口一档:[价格, 数量]。
type Level [2]uint64

// Book 盘口快照。bids 按价降序、asks 按价升序。
type Book struct {
	Bids   []Level `json:"bids"`
	Asks   []Level `json:"asks"`
	Market uint16  `json:"market"`
	Oracle uint32  `json:"oracle"`
}

func (c *Client) Book(ctx context.Context, market uint16, depth int) (*Book, error) {
	var b Book
	p := "/book/" + strconv.Itoa(int(market)) + "?n=" + strconv.Itoa(depth)
	return &b, c.do(ctx, http.MethodGet, p, nil, &b)
}

// Position 账户在某市场的持仓。金额字段是字符串:内核用 i128,
// JSON number 装不下,强行转 float 会在大额上悄悄丢精度。
type Position struct {
	Market       uint16 `json:"market"`
	Lots         int64  `json:"lots"`
	CostBasis    string `json:"costBasis"`
	FundingIndex string `json:"fundingIndex"`
}

// Risk 账户实时风险视图。NC < MMR 即可被清算。
type Risk struct {
	Exists       bool       `json:"exists"`
	AccountID    uint32     `json:"accountId"`
	Collateral   string     `json:"collateral"`
	NC           string     `json:"nc"`
	IMR          string     `json:"imr"`
	MMR          string     `json:"mmr"`
	Withdrawable string     `json:"withdrawable"`
	NextNonce    uint64     `json:"nextNonce"`
	Positions    []Position `json:"positions"`
}

func (c *Client) Risk(ctx context.Context, account uint32) (*Risk, error) {
	var r Risk
	return &r, c.do(ctx, http.MethodGet, "/risk/"+strconv.Itoa(int(account)), nil, &r)
}

// NextNonce 取该账户的下一个可用 nonce。
//
// **主账号与 API 钱包的 nonce 不是同一个计数器**:
//   - ApproveAgent / RevokeAgent 消耗 **master 账户**的 nonce(本方法)
//   - agent 代执行消耗 **agent 地址**自己的 nonce(见 AgentNonce 说明)
//
// 混用会得到 BadNonce,而错误信息里看不出是哪个计数器错了。
func (c *Client) NextNonce(ctx context.Context, account uint32) (uint64, error) {
	r, err := c.Risk(ctx, account)
	if err != nil {
		return 0, err
	}
	return r.NextNonce, nil
}

// Agent 一个已授权的 API 钱包。
type Agent struct {
	Address      string `json:"address"`
	ValidUntilMs string `json:"validUntilMs"` // "0" = 永不过期
	Expired      bool   `json:"expired"`
}

// Agents 列出某账户授权过的 API 钱包。
//
// **过期条目照常返回**并带 Expired 标记 —— 它们仍占着那个地址(重授同一地址
// 会更新有效期而不是新建),所以要能看见才能管理。
func (c *Client) Agents(ctx context.Context, master uint32) ([]Agent, error) {
	var r struct {
		Agents []Agent `json:"agents"`
		NowMs  string  `json:"nowMs"`
	}
	return r.Agents, c.do(ctx, http.MethodGet, "/agents/"+strconv.Itoa(int(master)), nil, &r)
}

// Order 一张挂单。
//
// Seq 是**订单序号**,不是打包后的 OrderId —— 网关这一层就已经把 market 拆出来了。
// 撤单接口收的也正是 seq + market,所以不需要在客户端再打包一次。
type Order struct {
	Seq       uint64 `json:"seq"`
	Side      string `json:"side"`
	Price     uint32 `json:"price"`
	Lots      uint64 `json:"lots"`
	Remaining uint64 `json:"remaining"`
}

// Orders 账户在某市场的当前挂单。
func (c *Client) Orders(ctx context.Context, account uint32, market uint16) ([]Order, error) {
	var r struct {
		Orders []Order `json:"orders"`
	}
	p := fmt.Sprintf("/orders/%d?market=%d", account, market)
	return r.Orders, c.do(ctx, http.MethodGet, p, nil, &r)
}

// OrderSeqs 当前挂单的序号列表 —— 直接喂给 BatchCancel / BatchReplace。
//
// 网关已经在响应里给了这个数组,单独暴露一个方法是因为「撤掉我现在所有的单」
// 是做市链路最常见的一步,让调用方自己从 Orders() 里 map 一遍纯属噪音。
func (c *Client) OrderSeqs(ctx context.Context, account uint32, market uint16) ([]uint64, error) {
	var r struct {
		Seqs []uint64 `json:"seqs"`
	}
	p := fmt.Sprintf("/orders/%d?market=%d", account, market)
	return r.Seqs, c.do(ctx, http.MethodGet, p, nil, &r)
}

// EventEnvelope 写操作返回的事件。kind 见 docs/API.md 的事件目录。
type EventEnvelope struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type writeResp struct {
	Status string          `json:"status"`
	Events []EventEnvelope `json:"events"`
}

// AccountRef 地址 → 内核账户的映射结果。
type AccountRef struct {
	AccountID    uint32 `json:"accountId"`
	Collateral   string `json:"collateral"`
	NC           string `json:"nc"`
	IMR          string `json:"imr"`
	Withdrawable string `json:"withdrawable"`
	NextNonce    uint64 `json:"nextNonce"`
}

// ErrNotRegistered 这个地址还没有内核账户。
//
// **不是错误状态,是正常的起点。** 账户在第一次入金时由状态机懒创建 ——
// 没有注册接口,也没有审批。看到它就是「还没入过金」。
var ErrNotRegistered = errors.New("dexos: 地址尚未注册内核账户(先入金)")

// AccountByAddress 按 EVM 地址查内核账户号。
//
// 接入的第一步:你有钱包地址,但下单要填的是**内核账户号**,两者不是一回事。
// 账户号按入金先后分配,不能指定,也不能从地址推导 —— 只能查。
func (c *Client) AccountByAddress(ctx context.Context, addr string) (*AccountRef, error) {
	var r AccountRef
	err := c.do(ctx, http.MethodGet, "/account/by-address/"+addr, nil, &r)
	if err != nil {
		var ae *APIError
		// 网关对未注册地址回 404/400 —— 转成具名错误,免得调用方去比对报文字符串
		if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusBadRequest) {
			return nil, ErrNotRegistered
		}
		return nil, err
	}
	return &r, nil
}

// SubaccountsOf 某 owner 名下的全部账户(主账户 + 子账户)。
func (c *Client) SubaccountsOf(ctx context.Context, owner string) ([]uint32, error) {
	var r struct {
		Subaccounts []uint32 `json:"subaccounts"`
	}
	return r.Subaccounts, c.do(ctx, http.MethodGet, "/account/by-owner/"+owner, nil, &r)
}
