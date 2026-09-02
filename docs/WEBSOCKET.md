# WebSocket 参考

```
ws://<host>/ws
```

---

## 协议:没有订阅报文

连上就开始收,服务端推**内核事件的全量流**。没有 subscribe/unsubscribe,没有频道。

这不是简化,是取舍:

- **代价**是带宽 —— 你会收到所有市场、所有账户的事件,过滤在客户端做。
- **收益**是没有「订阅状态」这个东西。重连即完整,不需要在重连后重放订阅,
  也不会出现「以为订上了其实没有」的静默失败。

对做市和风控这类必须看到每一笔的场景,全量流反而是对的:少收一条成交,
本地持仓镜像就错了,而错法是安静的。

---

## 消息格式

```json
{"seq": 12345, "kind": "Fill", "data": { … }}
```

`seq` 是内核事件序号,**单调递增且无洞**。

断线重连后,若新的首条 `seq` 不等于上次 +1,说明中间**丢了事件** ——
本地的持仓/盘口镜像已不可信,应当重新拉快照(`/risk/:id`、`/book/:m`)。

SDK 把这个差值单独投到 `Stream.Gap`,不静默吞掉:

```go
st, _ := c.Subscribe(ctx)
for {
    select {
    case ev := <-st.Events:
        handle(ev)
    case g := <-st.Gap:
        log.Printf("丢事件 seq %d..%d,重拉快照", g[0], g[1]-1)
        resync()
    case err := <-st.Err:
        return err
    }
}
```

`Subscribe` 自带指数退避重连(250ms → 8s 封顶),直到 ctx 取消。
单条消息解析失败会跳过而不断流 —— 一条坏消息不该让整条流挂掉。

---

## 事件类型

按用途分组。`data` 的字段随 kind 而异。

### 撮合

| kind | 何时 | 关键字段 |
|---|---|---|
| `OrderAccepted` | 下单被接受(可能同时部分成交) | `orderSeq` `account` `market` `side` `price` `lots` `filledLots` `resting` |
| `Fill` | 一笔成交 | `market` `price` `lots` `maker` `taker` `takerIsBuy` |
| `OrderCanceled` | 撤单成功 | `orderSeq` `account` `remainingLots` |
| `OrderExpired` | 挂单过期被清 | `orderSeq` `account` `remainingLots` |

> 撤单要用 `orderSeq`(已拆出 market),不是打包后的 OrderId。

### 风险与清算

| kind | 何时 |
|---|---|
| `Liquidation` | 清算执行 |
| `Deleverage` | ADL 减仓 |
| `SocializedLoss` | 坏账社会化 |
| `InsuranceFundChanged` | 保险基金变动 |

### 资金费与预言机

| kind | 何时 |
|---|---|
| `OracleUpdated` | 喂价 |
| `PremiumSampled` | 资金费采样(只累积,不动钱) |
| `FundingSettled` | 跨周期边界,指数推进 |

> 资金费是**惰性结算**的:指数一路推进,真正划钱要等仓位被触碰。
> 所以看到 `FundingSettled` 不代表账户余额已经变了。

### 资产与账户

| kind | 何时 |
|---|---|
| `Deposited` / `Withdrawn` | 出入金 |
| `Transferred` | 账户间划转 |
| `AgentAddrApproved` / `AgentRevoked` | API 钱包授权/撤销 |

### 市场与治理

| kind | 何时 |
|---|---|
| `MarketCreated` / `MarketStatusChanged` | 建市 / 状态机 |
| `MarketParamsUpdated` | 参数更新(**存量仓位的 IMR/MMR 立即重算**) |
| `FinalSettled` | 下市最终结算 |

---

## 与 REST 的配合

事件流给增量,REST 给快照。正确的姿势是:

1. 先 `Subscribe`,开始缓冲事件
2. 再拉快照(`/markets` `/book/:m` `/risk/:id`)
3. 用快照里的状态作基线,回放缓冲区里 `seq` 大于基线的事件

反过来(先拉快照再订阅)会在两步之间丢事件,而且丢得无声无息。

写操作的 HTTP 响应里**也带事件**(`{"status":"ok","events":[…]}`),那是这条命令
直接产生的那几条 —— 用它做即时确认比等 WS 回环更快,但它不能替代事件流:
别人的成交不会出现在你的响应里。
