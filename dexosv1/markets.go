package dexosv1

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Exchange 连接参数(`GET /v1/exchange`)里本包用得到的部分:EIP-712 域与 token 表。
type Exchange struct {
	ChainID uint64 `json:"chainId"`
	Domain  struct {
		Name              string `json:"name"`
		Version           string `json:"version"`
		VerifyingContract string `json:"verifyingContract"`
	} `json:"domain"`
	Deposit struct {
		Tokens []Token `json:"tokens"`
	} `json:"deposit"`
	ServerTime int64 `json:"serverTime"`
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
	Scale  Scale
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
}

// wireMarket `components.schemas.Market` 的线上形状。
type wireMarket struct {
	Symbol          string  `json:"symbol"`
	SigningMarketID uint16  `json:"signingMarketId"`
	Kind            string  `json:"kind"`
	Status          string  `json:"status"`
	PriceDecimals   uint8   `json:"priceDecimals"`
	SizeDecimals    uint8   `json:"sizeDecimals"`
	MinNotional     string  `json:"minNotional"`
	MaxLeverage     *uint32 `json:"maxLeverage"`
	LastPrice       string  `json:"lastPrice"`
	OraclePrice     *string `json:"oraclePrice"`
	MarkPrice       *string `json:"markPrice"`
	BestBid         *string `json:"bestBid"`
	BestAsk         *string `json:"bestAsk"`
	OpenInterest    *string `json:"openInterest"`
	FundingRate     *string `json:"fundingRate"`
}

func (w wireMarket) market() (Market, error) {
	m := Market{
		Symbol:          w.Symbol,
		SigningMarketID: w.SigningMarketID,
		Kind:            w.Kind,
		Status:          w.Status,
		Scale:           Scale{PriceDecimals: w.PriceDecimals, SizeDecimals: w.SizeDecimals},
		MinNotional:     w.MinNotional,
		MaxLeverage:     w.MaxLeverage,
		FundingRate:     w.FundingRate,
	}
	if w.Symbol == "" {
		return Market{}, fmt.Errorf("dexosv1: 市场缺 symbol")
	}
	var err error
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
	cat := &Catalog{bySymbol: make(map[string]int, len(resp.Markets))}
	for _, w := range resp.Markets {
		m, err := w.market()
		if err != nil {
			return nil, err
		}
		key := strings.ToUpper(m.Symbol)
		if _, dup := cat.bySymbol[key]; dup {
			return nil, fmt.Errorf("dexosv1: 市场目录里符号 %q 重复", m.Symbol)
		}
		cat.bySymbol[key] = len(cat.markets)
		cat.markets = append(cat.markets, m)
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
	Bids   []Level // 价格降序
	Asks   []Level // 价格升序
	Seq    uint64
	TS     int64
}

// Book 盘口(`/v1/markets/{symbol}/book`)。depth 0 = 服务端缺省(20),上限 200。
func (c *Client) Book(ctx context.Context, m *Market, depth int) (*Book, error) {
	q := url.Values{}
	if depth > 0 {
		q.Set("depth", strconv.Itoa(depth))
	}
	var w struct {
		Symbol string      `json:"symbol"`
		Bids   [][2]string `json:"bids"`
		Asks   [][2]string `json:"asks"`
		Seq    uint64      `json:"seq"`
		TS     int64       `json:"ts"`
	}
	if err := c.get(ctx, "/v1/markets/"+url.PathEscape(m.Symbol)+"/book", q, marketRead, &w); err != nil {
		return nil, err
	}
	if !strings.EqualFold(w.Symbol, m.Symbol) {
		return nil, fmt.Errorf("dexosv1: 问 %s 的盘口,答的是 %q", m.Symbol, w.Symbol)
	}
	b := &Book{Symbol: w.Symbol, Seq: w.Seq, TS: w.TS}
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
