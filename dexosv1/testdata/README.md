- `openapi-v1.json`:dex-os 开放 API v1.1 规格的副本,取自 chainupcloud/dex-os `web/openapi-v1.json`
  @ `2c5d2c2`(main,含 v1.1 §4.9 补缺 PR 9b #83、§4.10 补缺 PR 9c-A #90 与 K 线 / tickers #82),逐字节相同。
  dex-os 改了 v1 接口面就同步更新这份副本,再跑 `go test ./dexosv1/`;`write_test.go` 的 `specDeviations`
  登记已知的规格与实现差异,条目过期(规格已修好)测试会失败,提醒删掉。
- `fixtures/`:假服务的响应样例。形状按开发环境的真实响应整理,地址换成了假地址;
  每个文件都在 `schema_test.go` 的 `fixtureSchemas` 里登记了要符合的 schema,并由测试校验。
- `fixtures/w_*.json`、`fixtures/err4xx_*.json`:写接口的回执与错误样例,形状照原生 v1
  (dex-os `crates/gateway/src/api/v1_write.rs` @ `724acfc`)整理;地址是测试私钥 `0x42…42`(主钱包)与
  `0x43…43`(API 钱包)推出来的。登记在 `schema_test.go` 的 `fixtureSchemas` / `postFixtureSchemas`。
  回执里的 `requestId` 写成占位值 `0xabab…ab`:假网关按线上请求体独立重算请求身份(`gatewayRequestID`)
  再替换进去,所以 SDK 本地算的身份与"网关"算的对不上就会被测出来。
- v1.1 新增的只读样例:`prices.json`(24h 统计)、`candles.json`(`1M` 周期)、`portfolio.json`、
  `agent_grants.json`(`/v1/agents/{address}`)、`my_vaults.json`(`/v1/account/vaults`)。
- 金库身份(v1.1 §4.3)样例:`vault.json` / `vaults.json`(v1 形状 §4.9-10:USDC 金额十进制、净值是比值、没有
  `mainAccount` 与市场号;manager 认领在子账户 2、creator 在子账户 1;列表里另有一个待认领的金库)、
  `account_vault.json` / `ledger_vault.json`(`?vault=3` 读,身份字段只有 `vault`)、`w_nonce_vault.json`(`?vault=3&signer=`,`signingAccountId` 31 是金库主账户)。
- v1.1 §4.9(PR 9b)补的样例:`markets.json` 加 `quotePerTickLot` / `strictIsolated` 与一个现货市场(`baseToken` /
  `quoteToken` / `basePerLot`),`book.json` 带 `depth`,`exchange.json` 带 `fundingTickSec`;`tickers.json`
  (`/v1/tickers`)、`err400_incomplete.json`(`400 GatewayHistoryIncomplete`);`w_sequenced_202.json` 按 `Sequenced` 校验。
- v1.1 §4.10(PR 9c-A)补的字段:`prices.json` 带资金费时间表(`fundingIndex` / `lastFundingMs` / `nextFundingMs`,
  ETH 还没结算过);`orders.json` 带 `tif` / `reduceOnly` / `trigger`,另有一张待触发的仓位止损条件单。
- 主钱包的金库写与撤销授权回执:`w_vault_create.json` / `w_vault_deposit.json` / `w_vault_redeem.json` / `w_vault_claim.json` /
  `w_vault_config.json` / `w_revoke.json`,形状照 dex-os `v1_write.rs`(`create_vault` / `vault_money` / `vault_config` /
  `agent_revoke`)加 `conclude` 的公共回执字段整理。
- `eip712_golden.json`:主钱包签名结构体(Order / Cancel / Leverage、金库的 CreateManagedVault / VaultDeposit / VaultRedeem /
  ClaimVaultManager / VaultConfigChange、RevokeAgent)的摘要与签名金样,由 ethers 6.17 的 `TypedDataEncoder.hash` /
  `Wallet.signTypedData` 生成(私钥 `0x42…42`,两个不同的域),与本包的 Go 实现互相独立。类型串取 dex-os
  `crates/auth/src/lib.rs`,与钉住规格的 `x-eip712.typeString` 逐字一致(`eip712_test.go` 核对)。白名单 / 市场摘要(`depositorWhitelistHash` / `marketsHash`)也由生成脚本按 dex-os 的定义
  (升序去重后紧凑拼接再 keccak,空表为全零)独立算出,原始输入记在向量的 `inputs` 里。
  重新生成:对每个向量按 `eip712.go` 的类型串组 `types`,调用上面两个函数,输出同样的 JSON。
