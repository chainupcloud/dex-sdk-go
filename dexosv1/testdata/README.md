# dexosv1 测试数据

- `openapi-v1.json`:dex-os 开放 API v1 规格的副本,取自 chainupcloud/dex-os `web/openapi-v1.json`
  @ `62ba2b9`(main;该文件最后一次修改在 `b0386f6`,与分支 `feat/api-v1-native-merge` @ `e3b507d` 逐字节相同)。
  dex-os 改了 v1 接口面就同步更新这份副本,再跑 `go test ./dexosv1/`。
- `fixtures/`:假服务的响应样例。形状按开发环境的真实响应整理,地址换成了假地址;
  每个文件都在 `schema_test.go` 的 `fixtureSchemas` 里登记了要符合的 schema,并由测试校验。
