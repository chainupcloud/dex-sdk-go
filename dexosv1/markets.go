package dexosv1

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Exchange 连接参数(`GET /v1/exchange`)里本包用得到的部分:EIP-712 域、token 表与资金费结算周期。
type Exchange struct {
	ChainID uint64 `json:"chainId"`
	Domain  struct {
		Name              string `json:"name"`
		Version           string `json:"version"`
		VerifyingContract string `json:"verifyingContract"`
	} `json:"domain"`
	Deposit struct {
		// SystemGateway 入金系统地址:往它裸 transfer 已链接的 ERC20 即入金(中继按 from 记账,首次入金即开户)。
		// 空 = 节点没配。
		SystemGateway string  `json:"systemGateway"`
		Tokens        []Token `json:"tokens"`
	} `json:"deposit"`
	ServerTime int64 `json:"serverTime"`
	// FundingTickSec 资金费结算周期(秒,≥ 1)。全局:所有永续市场同一个周期(v1.1 §4.9-12)。
	// 节点刚启动、还不知道它时整个响应是 503 GatewayUnavailable,稍后重读。
	FundingTickSec uint64 `json:"fundingTickSec"`
	// Version 网关的版本指纹。CodecVer 是它认的规范命令编码版本,与 dexos.CodecVersion 不同就签不出它认的命令。
	Version struct {
		CodecVer uint32 `json:"codecVer"`
	} `json:"version"`
}

// Token 一个资产。Token 是签名结构体里的数字 id(Withdrawal / Transfer 用),Decimals 是金额的小数位:
// 余额、费用这类十进制金额换成原子单位用 ParseFixed(amount, Decimals)。
type Token struct {
	Token     uint32 `json:"token"`
	Symbol    string `json:"symbol"`
	Decimals  uint8  `json:"decimals"`
	ERC20     string `json:"erc20"`
	Finalized bool   `json:"finalized"`
}

func (c *Client) Exchange(ctx context.Context) (*Exchange, error) {
	var e Exchange
	if err := c.get(ctx, "/v1/exchange", nil, marketRead, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Market 一个市场。价格字段是 tick、数量字段是 lot(按本市场的 Scale 换算过);
// 不是委托价 / 成交价的量(费率、名义额、资金费率)保持十进制字符串。
type Market struct {
	Symbol string
	// SigningMarketID **只供签名**:EIP-712 结构体里的 market 字段。v1 的寻址一律用 Symbol。
	SigningMarketID uint16
	// Kind "perp" | "spot"。
	Kind string
	// Status Active / Paused / CancelOnly / PostOnly / FinalSettlement / Delisted。
	Status string
	// MarginMode "cross" | "isolated"。
	MarginMode string
	// StrictIsolated 部署方模式是 strictIsolated:逐仓,且持仓期间不能改杠杆。与 MarginMode 合起来还原部署方模式:
	// cross = normal,isolated + false = noCross,isolated + true = strictIsolated。
	StrictIsolated bool
	Scale          Scale
	// QuotePerTickLot 每 tick × lot 折多少计价币原子单位:名义额(原子单位)= ticks × lots × QuotePerTickLot。
	QuotePerTickLot uint64
	// BaseToken / QuoteToken 现货的 base / quote token 号(与 [Token] 的 Token 同一编号),
	// BasePerLot 现货每手折多少 base 原子单位;永续三项都是 nil。
	BaseToken  *uint32
	QuoteToken *uint32
	BasePerLot *uint64
	// MinNotional 最小名义额(USDC 十进制);"0" = 不限。
	MinNotional string
	// MaxLeverage nil = 现货。
	MaxLeverage *uint32
	LastPrice   uint32
	// 以下 nil = 服务端给了 null(没有预言机 / 盘口空 / 现货没有持仓量)。
	OraclePrice  *uint32
	MarkPrice    *uint32
	BestBid      *uint32
	BestAsk      *uint32
	OpenInterest *uint64
	// FundingRate 当前小时资金费率(十进制,`0.0001` = 0.01%);nil = 现货。
	FundingRate *string
	// Fees 本市场的费率;nil = 服务端没给。RateSource 为 "market" 时 taker / maker 按这里收,
	// 为 "tier" 时按账户档位([Account] 的 TakerFee / MakerFee)收。
	Fees *MarketFees
}

// MarketFees 市场的费率(十进制比例,maker 为负 = 返佣)。
type MarketFees struct {
	// RateSource "market" | "tier"。
	RateSource string `json:"rateSource"`
	TakerRate  string `json:"takerRate"`
	MakerRate  string `json:"makerRate"`
	// BuilderRate 前端(builder)费率的上限:归属某个 builder 的单,前端费 = min(该 builder 的费率([Client.Builder]), 本上限)。
	BuilderRate string `json:"builderRate"`
}

// wireMarket `components.schemas.Market` 的线上形状。
type wireMarket struct {
	Symbol          string      `json:"symbol"`
	SigningMarketID uint16      `json:"signingMarketId"`
	Kind            string      `json:"kind"`
	Status          string      `json:"status"`
	MarginMode      string      `json:"marginMode"`
	StrictIsolated  bool        `json:"strictIsolated"`
	PriceDecimals   uint8       `json:"priceDecimals"`
	SizeDecimals    uint8       `json:"sizeDecimals"`
	QuotePerTickLot string      `json:"quotePerTickLot"`
	BaseToken       *uint32     `json:"baseToken"`
	QuoteToken      *uint32     `json:"quoteToken"`
	BasePerLot      *string     `json:"basePerLot"`
	MinNotional     string      `json:"minNotional"`
	MaxLeverage     *uint32     `json:"maxLeverage"`
	LastPrice       string      `json:"lastPrice"`
	OraclePrice     *string     `json:"oraclePrice"`
	MarkPrice       *string     `json:"markPrice"`
	BestBid         *string     `json:"bestBid"`
	BestAsk         *string     `json:"bestAsk"`
	OpenInterest    *string     `json:"openInterest"`
	FundingRate     *string     `json:"fundingRate"`
	Fees            *MarketFees `json:"fees"`
}

func (w wireMarket) market() (Market, error) {
	m := Market{
		Symbol:          w.Symbol,
		SigningMarketID: w.SigningMarketID,
		Kind:            w.Kind,
		Status:          w.Status,
		MarginMode:      w.MarginMode,
		StrictIsolated:  w.StrictIsolated,
		Scale:           Scale{PriceDecimals: w.PriceDecimals, SizeDecimals: w.SizeDecimals},
		BaseToken:       w.BaseToken,
		QuoteToken:      w.QuoteToken,
		MinNotional:     w.MinNotional,
		MaxLeverage:     w.MaxLeverage,
		FundingRate:     w.FundingRate,
		Fees:            w.Fees,
	}
	if w.Symbol == "" {
		return Market{}, fmt.Errorf("dexosv1: 市场缺 symbol")
	}
	var err error
	if m.QuotePerTickLot, err = strconv.ParseUint(w.QuotePerTickLot, 10, 64); err != nil {
		return Market{}, fmt.Errorf("dexosv1: %s quotePerTickLot: %w", w.Symbol, err)
	}
	if w.BasePerLot != nil {
		n, err := strconv.ParseUint(*w.BasePerLot, 10, 64)
		if err != nil {
			return Market{}, fmt.Errorf("dexosv1: %s basePerLot: %w", w.Symbol, err)
		}
		m.BasePerLot = &n
	}
	if m.LastPrice, err = m.Scale.Ticks(w.LastPrice); err != nil {
		return Market{}, fmt.Errorf("dexosv1: %s lastPrice: %w", w.Symbol, err)
	}
	for _, p := range []struct {
		name string
		in   *string
		out  **uint32
	}{
		{"oraclePrice", w.OraclePrice, &m.OraclePrice},
		{"markPrice", w.MarkPrice, &m.MarkPrice},
		{"bestBid", w.BestBid, &m.BestBid},
		{"bestAsk", w.BestAsk, &m.BestAsk},
	} {
		if p.in == nil {
			continue
		}
		t, err := m.Scale.Ticks(*p.in)
		if err != nil {
			return Market{}, fmt.Errorf("dexosv1: %s %s: %w", w.Symbol, p.name, err)
		}
		*p.out = &t
	}
	if w.OpenInterest != nil {
		l, err := m.Scale.Lots(*w.OpenInterest)
		if err != nil {
			return Market{}, fmt.Errorf("dexosv1: %s openInterest: %w", w.Symbol, err)
		}
		m.OpenInterest = &l
	}
	return m, nil
}

// Catalog 市场目录:符号 → 市场。账户类读取的响应按符号给价量,换算要靠它。
//
// 目录是一次读取的快照;新上的市场不在旧目录里,响应里出现目录外的符号时读取直接报错
// (不猜精度),调用方重新取一次目录即可。
type Catalog struct {
	markets  []Market
	bySymbol map[string]int
}

// Markets 目录里的全部市场(服务端顺序)。
func (c *Catalog) Markets() []Market { return c.markets }

// Market 按符号取市场,大小写不敏感。
func (c *Catalog) Market(symbol string) (*Market, error) {
	i, ok := c.bySymbol[strings.ToUpper(symbol)]
	if !ok {
		return nil, fmt.Errorf("dexosv1: 市场目录里没有 %q(新上市场请重新取目录)", symbol)
	}
	return &c.markets[i], nil
}

// Catalog 拉 `/v1/markets` 建目录。
func (c *Client) Catalog(ctx context.Context) (*Catalog, error) {
	var resp struct {
		Markets []wireMarket `json:"markets"`
	}
	if err := c.get(ctx, "/v1/markets", nil, marketRead, &resp); err != nil {
		return nil, err
	}
	return newCatalog(resp.Markets)
}

func newCatalog(ws []wireMarket) (*Catalog, error) {
	cat := &Catalog{bySymbol: make(map[string]int, len(ws))}
	for _, w := range ws {
		m, err := w.market()
		if err != nil {
			return nil, err
		}
		cat.markets = append(cat.markets, m)
		// 下市后名称释放、可再上同名新市场（dex-os deploy.md §6i）：按符号只索引没下市的市场，两个没下市的同名才是目录错误。
		if m.Status == "Delisted" {
			continue
		}
		key := strings.ToUpper(m.Symbol)
		if _, dup := cat.bySymbol[key]; dup {
			return nil, fmt.Errorf("dexosv1: 市场目录里符号 %q 重复", m.Symbol)
		}
		cat.bySymbol[key] = len(cat.markets) - 1
	}
	return cat, nil
}

// Market 单个市场(`/v1/markets/{symbol}`)。
func (c *Client) Market(ctx context.Context, symbol string) (*Market, error) {
	var w wireMarket
	if err := c.get(ctx, "/v1/markets/"+url.PathEscape(symbol), nil, marketRead, &w); err != nil {
		return nil, err
	}
	m, err := w.market()
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// Level 盘口一档。
type Level struct {
	Price uint32
	Lots  uint64
}

// Book 盘口快照。Seq 是这份快照所在的状态点(与写回执的 seq 同一条轴)。
type Book struct {
	Symbol string
	// Depth 这份快照按多少档取的(REST 的 depth 或 WS 订阅的 depth;没给时是服务端缺省)。
	Depth int
	Bids  []Level // 价格降序
	Asks  []Level // 价格升序
	Seq   uint64
	TS    int64
}

// Book 盘口(`/v1/markets/{symbol}/book`)。depth 0 = 服务端缺省(20),上限 200。
func (c *Client) Book(ctx context.Context, m *Market, depth int) (*Book, error) {
	q := url.Values{}
	if depth > 0 {
		q.Set("depth", strconv.Itoa(depth))
	}
	var w wireBook
	if err := c.get(ctx, "/v1/markets/"+url.PathEscape(m.Symbol)+"/book", q, marketRead, &w); err != nil {
		return nil, err
	}
	return w.book(m)
}

// wireBook 盘口的线上形状(REST 响应与 WS `book` 帧同形)。
type wireBook struct {
	Symbol string      `json:"symbol"`
	Depth  int         `json:"depth"`
	Bids   [][2]string `json:"bids"`
	Asks   [][2]string `json:"asks"`
	Seq    uint64      `json:"seq"`
	TS     int64       `json:"ts"`
}

func (w wireBook) book(m *Market) (*Book, error) {
	if !strings.EqualFold(w.Symbol, m.Symbol) {
		return nil, fmt.Errorf("dexosv1: 问 %s 的盘口,答的是 %q", m.Symbol, w.Symbol)
	}
	b := &Book{Symbol: w.Symbol, Depth: w.Depth, Seq: w.Seq, TS: w.TS}
	var err error
	if b.Bids, err = levels(m.Scale, w.Bids); err != nil {
		return nil, fmt.Errorf("dexosv1: %s bids: %w", m.Symbol, err)
	}
	if b.Asks, err = levels(m.Scale, w.Asks); err != nil {
		return nil, fmt.Errorf("dexosv1: %s asks: %w", m.Symbol, err)
	}
	return b, nil
}

func levels(s Scale, in [][2]string) ([]Level, error) {
	out := make([]Level, 0, len(in))
	for _, l := range in {
		p, err := s.Ticks(l[0])
		if err != nil {
			return nil, err
		}
		n, err := s.Lots(l[1])
		if err != nil {
			return nil, err
		}
		out = append(out, Level{Price: p, Lots: n})
	}
	return out, nil
}

// Price 一个市场的实时价与滚动 24h 统计(`GET /v1/prices`)。价格是 tick、数量是 lot;
// nil = 服务端给了 null(没有预言机 / 盘口空 / 24h 统计不可用 / 窗口里没有成交)。
type Price struct {
	Symbol       string
	Last         uint32
	Mark         *uint32
	Oracle       *uint32
	BestBid      *uint32
	BestAsk      *uint32
	FundingRate  *string
	OpenInterest *uint64
	// FundingIndex 最近一次结算后的累计资金费指数(整数串,与 `/v1/markets` 的 fundingIndex 同值);nil = 现货。
	FundingIndex *string
	// LastFundingMs 最近一次资金费结算的区块时间(Unix 毫秒);NextFundingMs 下一次应结的时刻
	// (LastFundingMs + Exchange.FundingTickSec×1000,在第一个时间到点的区块结,实际可能晚一个块)。
	// nil = 现货或还没结算过。
	LastFundingMs *int64
	NextFundingMs *int64
	// PrevDayPrice / High24h / Low24h 滚动 24h 窗口的起点价、最高、最低。
	PrevDayPrice *uint32
	High24h      *uint32
	Low24h       *uint32
	// Change24h 涨跌幅(比例,十进制,`0.01` = 1%)。
	Change24h *string
	// Volume24h 基础币成交量;QuoteVolume24h 计价币成交额(十进制)。
	Volume24h      *uint64
	QuoteVolume24h *string
}

type wirePrice struct {
	Symbol         string  `json:"symbol"`
	Last           string  `json:"last"`
	Mark           *string `json:"mark"`
	Oracle         *string `json:"oracle"`
	BestBid        *string `json:"bestBid"`
	BestAsk        *string `json:"bestAsk"`
	FundingRate    *string `json:"fundingRate"`
	FundingIndex   *string `json:"fundingIndex"`
	LastFundingMs  *int64  `json:"lastFundingMs"`
	NextFundingMs  *int64  `json:"nextFundingMs"`
	OpenInterest   *string `json:"openInterest"`
	PrevDayPrice   *string `json:"prevDayPrice"`
	Change24h      *string `json:"change24h"`
	High24h        *string `json:"high24h"`
	Low24h         *string `json:"low24h"`
	Volume24h      *string `json:"volume24h"`
	QuoteVolume24h *string `json:"quoteVolume24h"`
}

// Window24h 24h 统计覆盖的窗口(毫秒):From 含、按分钟对齐;To 是分析面最近一个区块的时刻。
type Window24h struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// Prices `GET /v1/prices` 的结果。Window nil = 24h 统计不可用(此时各项的 24h 字段都是 nil)。
type Prices struct {
	Prices []Price
	TS     int64
	Window *Window24h
}

// Prices 全部市场的实时价与 24h 统计。响应里出现目录外的符号时报错(不猜精度)。
func (c *Client) Prices(ctx context.Context, cat *Catalog) (*Prices, error) {
	var w wirePrices
	if err := c.get(ctx, "/v1/prices", nil, marketRead, &w); err != nil {
		return nil, err
	}
	return w.prices(cat)
}

// wirePrices `/v1/prices` 的线上形状(REST 响应与 WS `prices` 帧同形)。
type wirePrices struct {
	Prices []wirePrice `json:"prices"`
	TS     int64       `json:"ts"`
	Window *Window24h  `json:"window24h"`
}

func (w wirePrices) prices(cat *Catalog) (*Prices, error) {
	out := &Prices{TS: w.TS, Window: w.Window, Prices: make([]Price, 0, len(w.Prices))}
	for _, wp := range w.Prices {
		m, err := cat.Market(wp.Symbol)
		if err != nil {
			return nil, err
		}
		p := Price{Symbol: wp.Symbol, FundingRate: wp.FundingRate, FundingIndex: wp.FundingIndex, LastFundingMs: wp.LastFundingMs,
			NextFundingMs: wp.NextFundingMs, Change24h: wp.Change24h, QuoteVolume24h: wp.QuoteVolume24h}
		if p.Last, err = m.Scale.Ticks(wp.Last); err != nil {
			return nil, fmt.Errorf("dexosv1: %s last: %w", wp.Symbol, err)
		}
		for _, f := range []struct {
			name string
			in   *string
			out  **uint32
		}{
			{"mark", wp.Mark, &p.Mark}, {"oracle", wp.Oracle, &p.Oracle},
			{"bestBid", wp.BestBid, &p.BestBid}, {"bestAsk", wp.BestAsk, &p.BestAsk},
			{"prevDayPrice", wp.PrevDayPrice, &p.PrevDayPrice}, {"high24h", wp.High24h, &p.High24h}, {"low24h", wp.Low24h, &p.Low24h},
		} {
			if f.in == nil {
				continue
			}
			t, err := m.Scale.Ticks(*f.in)
			if err != nil {
				return nil, fmt.Errorf("dexosv1: %s %s: %w", wp.Symbol, f.name, err)
			}
			*f.out = &t
		}
		for _, f := range []struct {
			name string
			in   *string
			out  **uint64
		}{{"openInterest", wp.OpenInterest, &p.OpenInterest}, {"volume24h", wp.Volume24h, &p.Volume24h}} {
			if f.in == nil {
				continue
			}
			l, err := m.Scale.Lots(*f.in)
			if err != nil {
				return nil, fmt.Errorf("dexosv1: %s %s: %w", wp.Symbol, f.name, err)
			}
			*f.out = &l
		}
		out.Prices = append(out.Prices, p)
	}
	return out, nil
}

// Ticker 一个市场最近 24 小时的成交统计(`GET /v1/tickers`)。价格是 tick、数量是 lot;
// 窗口里没有成交时 Trades 为 0、量为 0、四个价格都是 nil。
type Ticker struct {
	Symbol                string
	Open, High, Low, Last *uint32
	// Volume 基础币成交量;QuoteVolume 计价币成交额(十进制)。
	Volume      uint64
	QuoteVolume string
	Trades      int
}

type wireTicker struct {
	Symbol      string  `json:"symbol"`
	Open        *string `json:"open"`
	High        *string `json:"high"`
	Low         *string `json:"low"`
	Last        *string `json:"last"`
	Volume      string  `json:"volume"`
	QuoteVolume string  `json:"quoteVolume"`
	Trades      int     `json:"trades"`
}

// Tickers `GET /v1/tickers` 的结果。窗口是 [WindowStart, WindowEnd)(毫秒),WindowEnd = 服务端当前时刻;
// 按成交表算,与 [Prices] 的 24h 字段口径不同(那边锚在最近一个区块、按 1m 桶对齐)。
type Tickers struct {
	Tickers     []Ticker
	WindowStart int64
	WindowEnd   int64
}

// Tickers 全部市场最近 24 小时的成交统计。响应里出现目录外的符号时报错(不猜精度)。
func (c *Client) Tickers(ctx context.Context, cat *Catalog) (*Tickers, error) {
	var w struct {
		Tickers     []wireTicker `json:"tickers"`
		WindowStart int64        `json:"windowStart"`
		WindowEnd   int64        `json:"windowEnd"`
	}
	if err := c.get(ctx, "/v1/tickers", nil, marketRead, &w); err != nil {
		return nil, err
	}
	out := &Tickers{WindowStart: w.WindowStart, WindowEnd: w.WindowEnd, Tickers: make([]Ticker, 0, len(w.Tickers))}
	for _, wt := range w.Tickers {
		m, err := cat.Market(wt.Symbol)
		if err != nil {
			return nil, err
		}
		t := Ticker{Symbol: wt.Symbol, QuoteVolume: wt.QuoteVolume, Trades: wt.Trades}
		for _, f := range []struct {
			name string
			in   *string
			out  **uint32
		}{{"open", wt.Open, &t.Open}, {"high", wt.High, &t.High}, {"low", wt.Low, &t.Low}, {"last", wt.Last, &t.Last}} {
			if f.in == nil {
				continue
			}
			p, err := m.Scale.Ticks(*f.in)
			if err != nil {
				return nil, fmt.Errorf("dexosv1: %s ticker %s: %w", wt.Symbol, f.name, err)
			}
			*f.out = &p
		}
		if t.Volume, err = m.Scale.Lots(wt.Volume); err != nil {
			return nil, fmt.Errorf("dexosv1: %s ticker volume: %w", wt.Symbol, err)
		}
		out.Tickers = append(out.Tickers, t)
	}
	return out, nil
}

// K 线周期(v1.1 §4.6)。1w 起于周一,1M / 1Y 按 UTC 自然月、自然年分桶。
const (
	Interval1m  = "1m"
	Interval5m  = "5m"
	Interval15m = "15m"
	Interval30m = "30m"
	Interval1h  = "1h"
	Interval2h  = "2h"
	Interval4h  = "4h"
	Interval8h  = "8h"
	Interval12h = "12h"
	Interval1d  = "1d"
	Interval1w  = "1w"
	Interval1M  = "1M"
	Interval1Y  = "1Y"
)

var intervals = map[string]bool{
	Interval1m: true, Interval5m: true, Interval15m: true, Interval30m: true, Interval1h: true, Interval2h: true,
	Interval4h: true, Interval8h: true, Interval12h: true, Interval1d: true, Interval1w: true, Interval1M: true, Interval1Y: true,
}

// Candle 一根 K 线。Open 含、Close 不含(= 下一根的起点;1M / 1Y 是那个自然月 / 年的长度)。
type Candle struct {
	Open     int64
	Close    int64
	Interval string
	O, H, L  uint32
	C        uint32
	// Volume 基础币成交量(lot)。
	Volume uint64
}

type wireCandle struct {
	T0 int64  `json:"t"`
	T1 int64  `json:"T"`
	I  string `json:"i"`
	O  string `json:"o"`
	H  string `json:"h"`
	L  string `json:"l"`
	C  string `json:"c"`
	V  string `json:"v"`
}

// CandleQuery K 线查询。StartTime / EndTime 是毫秒,0 = 不限;Limit 0 = 服务端缺省(上限 1000)。
type CandleQuery struct {
	Interval  string
	StartTime int64
	EndTime   int64
	Limit     int
}

// Candles 一个市场的 K 线(`/v1/markets/{symbol}/candles`)。周期不在 v1 的集合里本地就报错,不发请求。
func (c *Client) Candles(ctx context.Context, m *Market, q CandleQuery) ([]Candle, error) {
	if !intervals[q.Interval] {
		return nil, fmt.Errorf("dexosv1: 未知的 K 线周期 %q", q.Interval)
	}
	v := url.Values{"interval": {q.Interval}}
	if q.StartTime > 0 {
		v.Set("startTime", strconv.FormatInt(q.StartTime, 10))
	}
	if q.EndTime > 0 {
		v.Set("endTime", strconv.FormatInt(q.EndTime, 10))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	var w struct {
		Symbol   string       `json:"symbol"`
		Interval string       `json:"interval"`
		Candles  []wireCandle `json:"candles"`
	}
	if err := c.get(ctx, "/v1/markets/"+url.PathEscape(m.Symbol)+"/candles", v, marketRead, &w); err != nil {
		return nil, err
	}
	if !strings.EqualFold(w.Symbol, m.Symbol) || w.Interval != q.Interval {
		return nil, fmt.Errorf("dexosv1: 问 %s %s 的 K 线,答的是 %q %q", m.Symbol, q.Interval, w.Symbol, w.Interval)
	}
	out := make([]Candle, 0, len(w.Candles))
	for _, wc := range w.Candles {
		k := Candle{Open: wc.T0, Close: wc.T1, Interval: wc.I}
		for _, f := range []struct {
			name string
			in   string
			out  *uint32
		}{{"o", wc.O, &k.O}, {"h", wc.H, &k.H}, {"l", wc.L, &k.L}, {"c", wc.C, &k.C}} {
			t, err := m.Scale.Ticks(f.in)
			if err != nil {
				return nil, fmt.Errorf("dexosv1: %s K 线 %d %s: %w", m.Symbol, wc.T0, f.name, err)
			}
			*f.out = t
		}
		var err error
		if k.Volume, err = m.Scale.Lots(wc.V); err != nil {
			return nil, fmt.Errorf("dexosv1: %s K 线 %d v: %w", m.Symbol, wc.T0, err)
		}
		out = append(out, k)
	}
	return out, nil
}
