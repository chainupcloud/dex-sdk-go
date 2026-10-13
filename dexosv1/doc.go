// Package dexosv1 是 DEX-OS 开放 API v1(`/v1/*`)的 Go 客户端。
//
// 与 [github.com/chainupcloud/dex-sdk-go/dexos] 的分工:dexos 走旧接口(数字市场号 / 账户号);
// 本包走 v1 —— 市场按符号(`BTC-USD`)寻址,账户按主钱包地址 + 子账户寻址([AccountRef]),
// 线上的价量是十进制字符串。
//
// # 价量仍是整数
//
// 对外的价格是 tick(uint32)、数量是 lot(uint64),与 dexos 包和 EIP-712 结构体同一个数量空间。
// 十进制字符串只在线上出现:进出本包时按市场的 priceDecimals / sizeDecimals 精确换算([Scale]),
// 不整除就报错,**从不四舍五入** —— 一个被悄悄舍入的价格签出来就是另一张单。
//
// 不是委托价 / 成交价的派生量(开仓均价、名义额、盈亏、费用、余额)保持十进制字符串,
// 需要原子单位时用 [ParseFixed] 按 token 小数位换。
//
// # 阶段
//
// 目前只有只读接口:市场目录与盘口、账户汇总 / 持仓 / 挂单、成交与资金流水(游标翻页)、
// nonce、API 钱包列表、请求回执。写接口与会话在 dex-os v1.1 规格定稿后补上。
//
// # 限流
//
// 429 解析成 [*RateLimitedError](带 RetryAfter 与限流头),本包不重试、不等待 ——
// 怎么退让(例如只暂停一个币对)由调用方决定。
package dexosv1
