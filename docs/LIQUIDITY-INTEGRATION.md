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
