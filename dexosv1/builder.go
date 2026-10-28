package dexosv1

import "context"

// Builder 这个入口所属的 builder(`GET /v1/builder`,dex-os builder-tenant-model §5.4):它上架的市场与前端费率。
// 入口由请求的 Host 决定(白标前端的域名);按内网地址连接时用 [Client.Host] 带上公网域名。
type Builder struct {
	BuilderID uint32 `json:"builderId"`
	// Status "active" | "disabled"。
	Status  string         `json:"status"`
	Catalog BuilderCatalog `json:"catalog"`
	Fees    BuilderFees    `json:"fees"`
	AsOfSeq uint64         `json:"asOfSeq"`
}

// BuilderCatalog 上架目录:只出 listed 与 closeOnly 的市场(hidden 不出),按 sort 再按符号排。
type BuilderCatalog struct {
	// Version 目录版本(0 = 从没写过)。
	Version uint64              `json:"version"`
	Markets []BuilderCatalogRow `json:"markets"`
}

// BuilderCatalogRow 目录一行。Symbol 是完整市场符号(商户 dex 上是 `<dex 简称>:BASE-QUOTE`),下单、撤单都用它;
// Display 去掉了简称前缀,只供展示,不能反解回符号。Status "listed" | "closeOnly"(closeOnly = 只收只减仓单)。
type BuilderCatalogRow struct {
	Symbol  string `json:"symbol"`
	Display string `json:"display"`
	Status  string `json:"status"`
}

// BuilderFees builder 自己的前端费率(十进制,名义额的比例,与 [MarketFees] 同口径):Default 为 nil 即没设,
// Markets 是按市场的覆盖。按账户的那一层不公开。实收 = min(费率, 市场的 builderRate 上限);makerRate 可为负
// (永续上给挂单方的补贴,不受上限约束,现货按 0)。
type BuilderFees struct {
	Default *BuilderFeeRate     `json:"default"`
	Markets []BuilderMarketFees `json:"markets"`
}

// BuilderFeeRate 一层费率。
type BuilderFeeRate struct {
	TakerRate string `json:"takerRate"`
	MakerRate string `json:"makerRate"`
}

// BuilderMarketFees 某个市场的费率覆盖。
type BuilderMarketFees struct {
	Symbol string `json:"symbol"`
	BuilderFeeRate
}

// Builder 读这个入口所属的 builder(按入口域名解析)。解析不到是 404 `GatewayBuilderNotFound`(*[Error])。
func (c *Client) Builder(ctx context.Context) (*Builder, error) {
	var b Builder
	if err := c.get(ctx, "/v1/builder", nil, marketRead, &b); err != nil {
		return nil, err
	}
	return &b, nil
}
