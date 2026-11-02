- `openapi-v1.json`:dex-os 开放 API v1 规格的副本,取自 chainupcloud/dex-os `web/openapi-v1.json` @ `c5ce1049`(main,含成对成交
  `POST /v1/orders/paired` 与子账户模型 v2 的 `sigVersion: 2` 信封、owner 级授权),逐字节相同。
  dex-os 改了 v1 接口面就整份同步这份副本,再跑 `go test ./dexosv1/`;`write_test.go` 的 `specDeviations`
  登记已知的规格与实现差异,条目过期(规格已修好)测试会失败,提醒删掉。
- 整份同步到 `c5ce1049` 时,规格给既有响应加的必填字段在样例里按同一含义补齐(本包不读它们):`markets.json` 的
  `tags` / `priceSource`;`account.json` / `account_vault.json` 的 `crossEquity` / `crossMarginUsed` / `crossMaintenanceMargin`
  (样例里没有逐仓,与全账户同值)、`leverage[].type`、`groups[].isolatedEquity`;`positions.json` 的 `marginMode` 与逐仓字段
  (全仓为 null);`exchange.json` 的 `vault`、`deliveryReadSigning` 与 `deposit` 的 `protocol` / `routing` / `ledgerEpoch` / `routes`;
  `vault.json` / `vaults.json` 的 `authorizedManager` / `confirmedTradingHalt` / `pendingClaim`。
- `fixtures/`:假服务的响应样例。形状按开发环境的真实响应整理,地址换成了假地址;
  每个文件都在 `schema_test.go` 的 `fixtureSchemas` 里登记了要符合的 schema,并由测试校验。
- `fixtures/w_*.json`、`fixtures/err4xx_*.json`:写接口的回执与错误样例,形状照原生 v1
  (dex-os `crates/gateway/src/api/v1_write.rs` @ `724acfc`)整理;地址是测试私钥 `0x42…42`(主钱包)与
  `0x43…43`(API 钱包)推出来的。登记在 `schema_test.go` 的 `fixtureSchemas` / `postFixtureSchemas`。
  回执里的 `requestId` 写成占位值 `0xabab…ab`:假网关按线上请求体独立重算请求身份(`gatewayRequestID`)
  再替换进去,所以 SDK 本地算的身份与"网关"算的对不上就会被测出来。
- `account.json` / `account_vault.json` 的 `fees`:前者经 builder 1 入口读(BTC-USD 档位费率、账户层 builder 费 0;ETH-USD 市场级费率、builder 默认层),后者不经 builder。
- `builder.json`:`/v1/builder` 样例,形状照开发环境 builder 1 的真实响应(目录里改了一行 `closeOnly`、加了一条按市场费率)。
- v1.1 新增的只读样例:`prices.json`(24h 统计)、`candles.json`(`1M` 周期)、`portfolio.json`、
  `agent_grants.json`(`/v1/agents/{address}`)、`my_vaults.json`(`/v1/account/vaults`)。
- 金库身份(v1.1 §4.3)样例:`vault.json` / `vaults.json`(v1 形状 §4.9-10:USDC 金额十进制、净值是比值、没有
  `mainAccount` 与市场号;manager 认领在子账户 2、creator 在子账户 1;列表里另有一个待认领的金库)、
  `account_vault.json` / `ledger_vault.json`(`?vault=3` 读,身份字段只有 `vault`)、`w_nonce_vault.json`(`?vault=3&signer=`,`signingAccountId` 31 是金库主账户)。
- `markets.json` 的 `aggTicks`:BTC-USD 配了 1 / 10 / 100 / 1000 tick、ETH-USD 未配置(null)、BTC-USDC 配了 10 / 100。
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
- `paired_golden.json`:成对成交与 v2 签名的金样 —— `PairedTradeLeg` 规范编码、`action` = keccak256(编码)、v2 `Action` 与
  `ApproveAgentScope` 的 EIP-712 摘要、签名与请求身份,由 dex-os 的 Rust 实现生成(dex-os @ `c5ce1049`),与本包的 Go 实现
  互相独立。生成程序是一个临时 cargo 程序(不入库),以 path 依赖引用 dex-os 的 `crates/core`(feature `auth`)与
  `crates/crypto`:内层用 `engine::SignedPairedLeg::inner` + `codec::encode_command` 编码,摘要取 `auth::Action` /
  `auth::ApproveAgentScope` 的 `signing_hash`,签名用 k256 `SigningKey::sign_prehash_recoverable`(v = 27 + recid,
  再经 `auth::recover` 回验),请求身份取 `engine::request_digest(..).request_id()`(整条 `PairedTrade` 的身份 = 挂单腿)。
  私钥 `0x42…42`(owner)、`0x43…43`(API 钱包)、`0x44…44`(另一个 owner);两个域(11155111 + 零地址,421614 + 非零地址)。
  三组成对成交:第一组参数同 dex-os `crates/gateway/tests/api_e2e.rs::paired_body`(owner 本人签两个主账户);第二组是画线形态
  (一把 API 钱包签同一 owner 的子账户 1 / 2,ETH-USD 的市场号 1,nonce 从 nowMs 起递增 —— `paired_test.go` 用它核对会话
  发出的请求体);第三组各字段取最大值。两条 `ApproveAgentScope`:位图 {1, 2}、全 1(带有效期);两条 `revokeAgentScope`
  按网关 `agent_revoke` 的 v2 分支构造(位图与有效期都为 0,两个域)。
  摘要、签名与请求身份另用 ethers 6.17 的 `TypedDataEncoder.hash` / `Wallet.signTypedData` 交叉核过一遍。
  重新生成:按上面的调用对每组参数输出同样的 JSON。
- 子账户模型 v2 的样例:`w_agents_owner.json`(`/v1/account/agents`,一条按账户授权加一条 owner 级授权,位图 {1, 2},
  形状照 dex-os `crates/gateway/src/api/v1.rs::agents`)、`w_approve_scope.json`(`sigVersion: 2` 授权的回执,
  照 `v1_write.rs::agent_approve` 的 v2 分支)、`subaccounts.json` 的 `name`(网关标签,没起过名为空串)。
- 成对成交的样例:`w_paired.json`(200 回执,ETH-USD 子账户 1 挂买、2 吃卖,形状照 dex-os `v1_write.rs::paired_result`
  加 `conclude_try` 的公共字段)、`err422_paired_spread.json`(`PairedTradeOutsideSpread`,num 20127)、
  `err403_paired.json`(名单外,`details = {reason: paired_trade_not_allowed, leg}`)、`err404_route.json`(接口未开)。
  规格里 200 响应只是 `{type: object}`,所以本包读的字段登记在 `write_test.go` 的 `specDeviations`。
