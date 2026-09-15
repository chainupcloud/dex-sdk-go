# 做市接入

完整接入需要成交、订单恢复、实际费用和资金流闭环。当前软件及服务端前置见 [做市完整性边界](LIQUIDITY-INTEGRATION.md)。

## 换单与逐项结果

`BatchReplace` 把撤旧与挂新放在一次状态机转移，仍可能逐项部分成功。当前内核跳过失败项；成功 HTTP 响应不是整批成功保证。

SDK 返回原始事件，并在没有全部输入项成功证据时返回 `ErrIncompleteBatch`。部分成功后必须冻结核查，不按价量、事件数量或压缩数组索引猜归属。

```go
events, err := c.BatchReplace(ctx, api, account, market, oldSeqs, quotes, nonce)
if err != nil {
    // 保留原请求身份、nonce 和事件到调用方的持久存储，进入未决核查。
    // 不换 nonce 重发，不把当前挂单列表当成原回执。
    return err
}
// 这里只取得当前批次完整成功事件，尚不能证明逐笔账务或重启恢复完成。
consumeReceipt(events)
```

## nonce 与恢复

nonce 按 API 代理地址共享。多进程或多个账户共用同一代理时，发单必须由一个持有独占租约的协调方串行处理。`Session` 的互斥仅覆盖该 Session；它不提供持久化与跨进程排他。

网络错误、无效响应和批次部分成功后，`Session` 锁定后续写入及 Resync。重读 nonce 不能恢复原请求回执；不能通过重启/新建 Session 清除未决风险。服务端恢复契约见 [dex-os #3](https://github.com/chainupcloud/dex-os/issues/3)。

## 事件流与账务

过滤参数为 `markets` / `accounts`，两者取并集。seq 是日志位号，同命令多事件共享，不能作为成交唯一身份。

SDK 遇坏帧、Lagged、倒退或断线即终止并报 `ErrStreamGap`。重连只收未来事件，快照不能补齐逐笔成交。可靠历史补拉尚待 [dex-os #1](https://github.com/chainupcloud/dex-os/issues/1) 与 [#2](https://github.com/chainupcloud/dex-os/issues/2)。

交易落账须取得稳定成交 ID、订单关联、逐笔实际费用与最终状态；持仓差只能用于对账，不能变造成交。权益用 NC 的明确口径；collateral 不是总权益，缺净入金/资金费不得填零。

## 账户与停止

主钱包私钥留在策略之外。先查地址映射，再检查 API 代理对该账户的授权和有效期；授权范围以已核实服务端契约为准，不能从地址猜测。

`ScheduleCancel` 撤销整个账户的订单、条件单和 TWAP。必须核实账户专用性及其他策略占用。停止与重启还需要撤单回执、权威挂单核对、在途请求与历史补拉收口；单次全撤调用成功不等于全部收口。

所有真实测试须先有 owner 的账户授权、书面窗口和硬顶。HTTP/head 测试环境不能提供生产安全或最终确认保证。
