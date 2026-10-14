# dexosv1 测试数据

- `openapi-v1.json`:dex-os 开放 API v1 规格的副本,取自 chainupcloud/dex-os `web/openapi-v1.json`
  @ `62ba2b9`(main;该文件最后一次修改在 `b0386f6`,与分支 `feat/api-v1-native-merge` @ `e3b507d` 逐字节相同)。
  dex-os 改了 v1 接口面就同步更新这份副本,再跑 `go test ./dexosv1/`。
- `fixtures/`:假服务的响应样例。形状按开发环境的真实响应整理,地址换成了假地址;
  每个文件都在 `schema_test.go` 的 `fixtureSchemas` 里登记了要符合的 schema,并由测试校验。
- `fixtures/w_*.json`、`fixtures/err4xx_*.json`:写接口的回执与错误样例,形状照原生 v1
  (dex-os `crates/gateway/src/api/v1_write.rs` @ `314ab2a`)整理;地址是测试私钥 `0x42…42`(主钱包)与
  `0x43…43`(API 钱包)推出来的。登记在 `schema_test.go` 的 `fixtureSchemas` / `postFixtureSchemas`。
- `eip712_golden.json`:主钱包签名结构体(Order / Cancel / Leverage)的摘要与签名金样,由 ethers 6.17 的
  `TypedDataEncoder.hash` / `Wallet.signTypedData` 生成(私钥 `0x42…42`,两个不同的域),与本包的 Go 实现互相独立。
  重新生成:对每个向量按 `eip712.go` 的类型串组 `types`,调用上面两个函数,输出同样的 JSON。
