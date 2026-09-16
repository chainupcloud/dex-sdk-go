# 做市接入的完整性边界

源码核查基线：dex-os `1ff707ba523f356901bbd1a7ddf30a96e61d2db8`。软件测试使用进程内 HTTP/WS，不是已执行真实交易的证据。

## 连接与身份

使用 `Connect` 每次发现 chainId、完整 EIP-712 域、合约地址及版本；缺失/非法立即报错。`NewClientFromConfig` 现返回 `(*Client, error)`，调用方必须检查错误；手工构造 `Domain` 时需显式填 Name/Version。EIP-712 编码协议和 codec 没有改变。

`AccountByAddress` 只把 404 解释为未登记；`AgentGrants` 校验返回代理、每条账户/有效期及代理共享 nonce。代理不能由主账户地址代替。主钱包私钥不得传给策略；授权、入金由 owner 在策略外完成。

`Market.PriceDecimals` / `SizeDecimals` 现为 `*int`：无元数据市场保留在目录中，精度为 nil；使用某个市场前须核对这两项，缺失时明确报告市场 ID 并停止该市场转换。不能把未知精度当 0，也不因无关市场缺元数据丢掉整个目录。

## 批量订单与未知结果

`BatchPlace` / `BatchCancel` / `BatchReplace` 返回原始成功事件；缺任意输入项成功证据时同时返回 `ErrIncompleteBatch`。部分成功不能按压缩后的数组索引或价量配对，也不能把失败当未执行。

`Session` 在会话内串行写入，任意写错误锁定后续写入及 `Resync`（`ErrSessionBlocked`），保留原 nonce 与事件供核查。锁定并不声称明确拒绝也是未知执行，只表示调用方尚未核对上次结果。不得通过创建新 Session、重读 nonce 或换新 nonce 盲重发绕过。

这是进程内约束；**调用方仍须持久化未决请求并按 API 代理地址持有跨进程独占租约**。本 SDK 没有提供持久 nonce 协调或可靠重启恢复；服务端原回执契约完成前，交易接入保持 gated。

### 单请求换单的证据接口

对照 dex-os `9d942f24aeb48212aae138d31febe66f0e240d32` 的公开 `/replace` 响应，新增
`Client.BatchReplaceWithReceipt` / `Session.ReplaceWithReceipt`，仍只发送一次相同的 `/replace` 请求。
旧 `BatchReplace` / `Replace` 签名不变，委托同一实现并返回事件；不是先撤再下两个请求。
仅下单时空撤单列表序列化为 `[]`，而不是 Rust Vec 不接受的 `null`。

`BatchSubmission` 同时返回：

- `Request`：本地实际用于签名的代理、账户、nonce、nowMs、codec 版本、域、命令哈希和签名摘要。
  SDK 不把私钥或请求签名复制进这份身份。它不是服务端已接受证明，也不是跨部署唯一键。
- `Receipt`：实际完整解析的响应，包括 seq/sub、folded、聚合统计、拒因和原始事件。
  非 nil 不等于业务成功，必须同时检查 error；坏 JSON/类型错/无响应时为 nil，不暴露半解码内容。
- `Events()`：兼容旧消费者的事件视图；错误发生时仍可能有已成功的部分事件，不能丢弃。

可选字段保持 nil 与合法0/false的区别。seq/sub 是请求所在日志条目及子项，不是唯一成交 ID。
`folded=false` 返回 `ErrExecutionPending` 并经 Session 锁定；不会自动加 wait=fold、等待或重发。
聚合统计只能否定成功，不能代替逐项事件证据；已提供的统计与输入/事件矛盾也返回错误。
该固定源码的 batchStatus 值集为 `ok` / `none` / `partial`，完整成功为 `ok`，不是 `all`。
缺 sub/folded/统计的旧版回执仍可按原有事件检查工作，不给缺失字段补零。

即使返回 error，也要保留本次已经取得的 Request/Receipt。会话已阻断、context 预先取消或授权
已到期时，本次调用不会建立新 Request；这不证明上一请求未执行，不应被当作恢复结果。

**这不是发送前日志。** 身份随调用结果返回；进程在返回前崩溃仍可能失去它，尚需后续 SDK
准备/落账接线和获批持久化方案。没有原回执查询、跨进程 nonce 协调或自动重启恢复能力，
更不能据此解除 dex-os #1–#5、账户独占和 live 窗口前置。EIP-712、codec、签名算法均未改变。

## WebSocket

`Subscribe` 同步建立连接。坏帧、缺 envelope 字段、Lagged、seq 倒退或断线会终止流并报告 `ErrStreamGap`；取消 context 关闭静默连接。三个通道均关闭，消费方必须检查通道 ok 或在 Err 后结束。

不再自动重连。只有从可靠历史补齐逐笔事件、对账完成后才能建立新连接；当前 `/risk` 或 `/book` 快照不能证明成交历史完整。seq 可以重复或跳号，禁止作为唯一成交 ID。

## 服务端前置

- [成交身份、订单关联、实际费用与完整分页 #1](https://github.com/chainupcloud/dex-os/issues/1)
- [历史补拉及投影完整性 #2](https://github.com/chainupcloud/dex-os/issues/2)
- [原请求回执恢复 #3](https://github.com/chainupcloud/dex-os/issues/3)
- [批次逐项结果 #4](https://github.com/chainupcloud/dex-os/issues/4)
- [资金流水与权益对账 #5](https://github.com/chainupcloud/dex-os/issues/5)

HTTP/head 实例不提供生产安全与最终确认保证。`ScheduleCancel` 是账户级全撤，使用前必须核实专用账户及其他策略占用，取得 owner 的 live 窗口与硬顶。
