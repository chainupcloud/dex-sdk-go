# WebSocket 参考

```
ws://<host>/ws
```

---

## 协议:过滤是连接参数,不是订阅报文

```
ws://<host>/ws                          全量
ws://<host>/ws?markets=0,1              只要这些市场
ws://<host>/ws?accounts=3               只要这些账户
ws://<host>/ws?markets=0&accounts=3     并集:两者命中任一即投递
```

没有 subscribe/unsubscribe,没有频道,没有运行时改条件。这不是简化,是取舍:

- **代价**是改过滤条件要重连。对做市不是问题 —— 关注面在进程生命周期内不变。
- 过滤只作用于当前连接；重连只收未来事件，不回放断线期间历史。

### 语义

**并集不是交集。** 做市的典型诉求是「市场 0 的全部行情,加上我自己账户的全部动静」
—— 若做成交集,「别人在市场 0 的成交」会被滤掉,而那正是最该看到的东西。

**无作用域的事件始终投递**(BlockBegun、治理类配置变更):它们稀少且便宜,
漏掉反而会让客户端错过系统级信号。

**解不动的过滤条件退化为不过滤**,而不是滤掉全部 —— 后者是「静默什么都收不到」,
比多收难查得多。

```go
st, _ := c.Subscribe(ctx, dexos.WithMarkets(0), dexos.WithAccounts(master))
```

实测(docker 栈,同时开三条流,在市场 0 造 152 张单):

```
全量               327 条  BlockBegun 14 · Oracle 7 · Funding 2 · OrderAccepted 152 · OrderCanceled 152
markets=0          327 条  同上
accounts=9999      14 条   只有 BlockBegun(无作用域)
```

> 这条实测抓出过一个 bug:`OrderCanceled` 等五种订单事件的 JSON 里**没有 market 字段**
> (OrderId 里有,只是没写出来),导致按市场过滤的做市商收不到自己的撤单。已修。

---

## 消息格式

```json
{"seq": 12345, "kind": "Fill", "data": { … }}
```

## seq 不能用来做丢帧检测

`seq` 是 **Raft 日志序号**,不是逐事件计数器。两个后果:

- 同一条命令产出的多个事件**共享同一个 seq**。实测一条批量撤单的 22 条
  `OrderCanceled` 全是同一个值。
- 没有产生事件的日志条目会让它**跳号**。

所以 `seq` 只保证**非递减**,既不连续也不唯一。拿它做丢帧检测既会疯狂误报,
又**根本区分不出**「跳号是因为无事件的日志」还是「因为丢了帧」。

## 丢帧由服务端显式下发

```json
{"kind": "Lagged", "dropped": 128}
```

服务端的广播通道容量 8192。客户端消费不过来时通道积压,`dropped` 条事件
**永远拿不到了** —— 不会补发。

收到它意味着本地镜像与历史完整性已不可信；快照不能补回逐笔成交，必须停止并补拉历史。
对做市这是**正确性问题不是性能问题**:丢一条自己的 Fill,之后就一直拿着错的仓位
报价,而业务层完全看不出来。

```go
st, _ := c.Subscribe(ctx)
for {
    select {
    case ev, ok := <-st.Events:
        if !ok { return <-st.Err }
        handle(ev)
    case n, ok := <-st.Lost:
        if ok { log.Printf("丢帧 %d 条", n) }
        return <-st.Err
    case err := <-st.Err:
        return err
    }
}
```

`Subscribe` 同步建连,断线、坏帧或 Lagged 终止流并报告 `ErrStreamGap`;**seq 回退不终止** —— 它不单调(keeper 的 OracleUpdated / FundingSampled 走另一条派生路径,真节点上每几分钟回退一次),事件身份是 `Event.ID()` 的三段。
不自动重连 —— 重连会掩盖成交缺口。**缺口有正路可补**:每条事件带完整身份 `Event.ID()`
(`"<seq>-<sub>-<idx>"`,与 `/fills` 里同一笔逐字符相同),记住最后一个(连同纪元),`s.Fills(ctx,
dexos.FillQuery{After: lastID, Epoch: epoch})` 从服务端成交账本把断开期间的成交拉齐、按 id 去重,然后再
`Subscribe`。快照(`/risk` `/book`)只能校准状态,补不回逐笔成交。取消 context 会关闭静默连接，
三个通道均关闭。详见 [做市完整性边界](LIQUIDITY-INTEGRATION.md)。

## 时延实测

`examples/wslatency` 量的是「下单请求返回 ↔ 同一事件从 WS 收到」:

```
样本 200 / 未收到 0
p50 -30.4µs   p90 -6.9µs   p99 22.6µs   max 42.4µs
```

**负数是真的**:WS 事件比同一条命令的 HTTP 响应更早到达 —— 内核发出事件即进广播,
而 HTTP 响应还要序列化再回。做市的反应链路不必等请求返回。

(本机 dev-up、单客户端、无竞争。生产要在目标机型上重跑。)

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
