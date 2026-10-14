package dexosv1

import (
	"context"
	"fmt"
)

// 账户价值与盈亏历史(v1.1 §4.8,`GET /v1/account/portfolio`)的区间。
const (
	Period24h = "24h"
	Period7d  = "7d"
	Period30d = "30d"
	PeriodAll = "all"
)

// Portfolio 一个账户在一个区间里的价值与盈亏走势,两种口径各一份:
// Total = 全部资产(USDC 风险组净值 + 现货按 USDC 现货最新成交价 + 金库份额);
// Perp = 只看 USDC 风险组,现货买卖当作划转。
//
// 曲线从服务端开始采样起才有点(不回补);没有采样的桶不出点。金额是 USDC 十进制字符串。
type Portfolio struct {
	Address    string `json:"address"`
	Subaccount uint32 `json:"subaccount"`
	Epoch      string `json:"epoch"`
	Period     string `json:"period"`
	BucketMs   int64  `json:"bucketMs"`
	// FromMs / AsOfMs / SeriesStartMs nil = 还没有采样点。
	FromMs        *int64         `json:"fromMs"`
	AsOfMs        *int64         `json:"asOfMs"`
	SeriesStartMs *int64         `json:"seriesStartMs"`
	Total         PortfolioScope `json:"total"`
	Perp          PortfolioScope `json:"perp"`
	// UnvaluedTokens 没有 USDC 现货市场、因而没计入 Total 的 token。
	UnvaluedTokens []string `json:"unvaluedTokens"`
	// Gaps 采样中断跨过执行历史缺口的区间:这几段不计盈亏。
	Gaps []PortfolioGap `json:"gaps"`
}

// PortfolioScope 一种口径。区间统计 nil = 数据不足(不是 0)。
type PortfolioScope struct {
	History []PortfolioPoint `json:"history"`
	PnL     *string          `json:"pnl"`
	// ReturnRate 时间加权收益率;MaxDrawdown 收益指数相对高点的最大跌幅。都是比例(`0.01` = 1%)。
	ReturnRate  *string `json:"returnRate"`
	MaxDrawdown *string `json:"maxDrawdown"`
	Volume      *string `json:"volume"`
	Fees        *string `json:"fees"`
}

// PortfolioPoint 一个桶末的采样点。PnL 与 NetInflow 相对本窗口第一个点(第一个点为 0);AccountValue 是绝对值。
type PortfolioPoint struct {
	TS           int64  `json:"ts"`
	AccountValue string `json:"accountValue"`
	PnL          string `json:"pnl"`
	NetInflow    string `json:"netInflow"`
}

// PortfolioGap 一段不计盈亏的断点区间(毫秒)。
type PortfolioGap struct {
	FromMs int64 `json:"fromMs"`
	ToMs   int64 `json:"toMs"`
}

// Portfolio 账户价值与盈亏历史。period 取 Period24h / Period7d / Period30d / PeriodAll。
//
// 按分钟采样的分析数据,与写入没有读一致性关系,所以不带 `x-dexos-min-seq`。
func (c *Client) Portfolio(ctx context.Context, ref AccountRef, period string) (*Portfolio, error) {
	switch period {
	case Period24h, Period7d, Period30d, PeriodAll:
	default:
		return nil, fmt.Errorf("dexosv1: 未知的区间 %q", period)
	}
	q, err := ref.walletValues("portfolio")
	if err != nil {
		return nil, err
	}
	q.Set("period", period)
	var p Portfolio
	if err := c.get(ctx, "/v1/account/portfolio", q, marketRead, &p); err != nil {
		return nil, err
	}
	if p.Period != period || p.Epoch == "" {
		return nil, fmt.Errorf("dexosv1: 问 %s 区间,答的是 %q(纪元 %q)", period, p.Period, p.Epoch)
	}
	return &p, nil
}
