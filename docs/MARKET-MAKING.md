# 做市接入

针对高频报价场景。前提是你已经跑通了 [接入指南](GETTING-STARTED.md)。

---

## 为什么必须用原子换单

刷新报价有两种写法,差别不是省一次往返:

```go
// ✗ 两条共识条目
c.BatchCancel(ctx, api, master, mk, oldSeqs, n)
c.BatchPlace(ctx, api, master, newQuotes, n+1)

// ✓ 一条
c.BatchReplace(ctx, api, master, mk, oldSeqs, newQuotes, n)
```

分两次提交有两个真实后果:

1. **中间存在一个没有报价的窗口。** 窗口期内你不在盘口上,该吃的单没吃到。
2. **第二条若被拒,第一条已经生效。** 保证金不足、市场进 `CancelOnly`、
   nonce 撞了 —— 任何一种,结果都是**报价被撤光却没挂上新的**,单边裸露。

合成一条后,撤与挂落在同一个状态机转移里,不存在中间态可被观测。

### 「原子」的准确含义

**是「没有中间窗口」,不是「全成功或全回滚」。**

逐项仍然尽力而为:撤不掉的跳过(已成交、已撤、不存在),挂不上的跳过。
这是刻意的 —— 全回滚语义下,报价阶梯里一张单的保证金不足会废掉**整轮**刷新,
那比部分成功糟得多。

所以每轮之后要对账,不能假设你提交的都挂上了:

```go
ev, _ := c.BatchReplace(...)
accepted := 0
for _, e := range ev {
    if e.Kind == "OrderAccepted" { accepted++ }
}
if accepted != len(newQuotes) {
    // 有单没挂上 —— 通常是保证金或价格带
}
```

---

## 报价循环

```go
func quoteLoop(ctx context.Context, c *dexos.Client, api *dexos.Signer, master uint32, mk uint16) error {
    st, err := c.Subscribe(ctx)          // 先订阅
    if err != nil { return err }
    state, err := snapshot(ctx, c, master, mk)   // 再拉快照
    if err != nil { return err }

    nonce, err := c.AgentNonce(ctx, master, api.Address())
    if err != nil { return err }

    ticker := time.NewTicker(200 * time.Millisecond)
    defer ticker.Stop()

    for {
        select {
        case ev := <-st.Events:
            state.apply(ev)              // 成交/撤单/喂价 → 更新本地镜像

        case n := <-st.Lost:
            // 丢帧:本地镜像已不可信。不能靠事件补,必须重拉。
            log.Printf("丢帧 %d,resync", n)
            if state, err = snapshot(ctx, c, master, mk); err != nil { return err }
            if nonce, err = c.AgentNonce(ctx, master, api.Address()); err != nil { return err }

        case <-ticker.C:
            quotes := state.desiredQuotes()
            ev, err := c.BatchReplace(ctx, api, master, mk, state.openSeqs, quotes, nonce)
            if err != nil {
                nonce, err = recover(ctx, c, api, master, err, nonce)
                if err != nil { return err }
                continue
            }
            nonce++
            state.applyAll(ev)

        case err := <-st.Err:
            return err
        }
    }
}
```

三个要点:

**先订阅再拉快照。** 反过来会在两步之间丢事件,而且丢得无声无息。

**nonce 本地自增,不是每轮去读。** 每轮多一次 HTTP 会把报价周期拖长几毫秒。
只在出错或 resync 后重读。

**报价由 ticker 驱动,不是由事件驱动。** 事件驱动会在行情剧烈时把你打成
「每笔成交都重报一次」,请求量爆炸而报价并不更优。事件负责更新镜像,
ticker 负责决定何时重报。

---

## 反应延迟

实测(本机 dev-up、300 样本、单客户端),「下单请求返回 ↔ 同一事件从 WS 收到」:

```
p50 -35.4µs   p90 -7.1µs   p99 74.4µs   max 105.6µs
```

**负数是真的** —— WS 事件比同一条命令的 HTTP 响应更早到达:内核发出事件即进广播,
而 HTTP 响应还要序列化再回。

对做市的直接含义:**别等 `BatchReplace` 返回再更新本地镜像**,事件流已经到了。
返回值用来对账(几张挂上了),不用来驱动下一步。

---

## 丢帧就是持仓错了

这条比时延重要。

服务端广播通道容量 8192。你消费不过来时,中间那些事件**永远拿不到** —— 不补发。
而 `seq` 推不出丢帧(它是 Raft 日志序号,同命令的多事件共享、无事件的日志跳号),
所以唯一信号是服务端显式下发的:

```json
{"kind": "Lagged", "dropped": 3366}
```

**丢一条自己的 `Fill`,你的持仓镜像就错了,之后一直拿错的仓位报价,业务层完全看不出来。**

所以 `Stream.Lost` 必须有处理分支,而且处理方式只有一种:**重拉快照**。
不要试图从后续事件里推断丢了什么。

同理:**重连本身就意味着丢帧**,断开期间的事件不补发。SDK 的自动重连不会替你 resync。

---

## 错误恢复

```go
func recover(ctx context.Context, c *dexos.Client, api *dexos.Signer,
    master uint32, err error, nonce uint64) (uint64, error) {

    var ae *dexos.APIError
    if !errors.As(err, &ae) { return nonce, err }   // 网络错误 → 交给上层重试

    switch ae.Status {
    case 409:  // NonceMismatch —— 本地计数器与链上不同步
        return c.AgentNonce(ctx, master, api.Address())
    case 401:  // 签名问题 —— 重试没用,是编码/域/密钥的问题
        return nonce, fmt.Errorf("签名被拒,检查 chainId 与 codec 版本:%w", err)
    case 422:  // 业务拒绝
        // UnknownAgent / AgentExpired → 授权没了,要重新授权(需主账号密钥)
        // InsufficientMargin        → 减小报价规模
        // MarketNotTrading          → 市场停牌,退避
        return nonce, err
    }
    return nonce, err
}
```

**409 是唯一「重读后重试即可」的类别。** 401 重试永远不会好;422 要按拒因分别处置。

---

## 授权续期

授权有有效期,过期后所有代执行返回 `AgentExpired`。续期 = 用同一地址再授权一次
(同 master 重复授权是**更新有效期**,不是新建):

```go
agents, _ := c.Agents(ctx, master)
for _, a := range agents {
    if a.Address != api.Address().Hex() { continue }
    until, _ := strconv.ParseUint(a.ValidUntilMs, 10, 64)
    if until != 0 && time.Until(time.UnixMilli(int64(until))) < 7*24*time.Hour {
        // 快过期了,续
    }
}
```

续期要主账号密钥。**建议做成独立的运维动作**,别让策略进程持有主账号密钥 ——
那样等于绕过了 API 钱包的全部意义。

---

## 规模上限与已知边界

| 项 | 现状 |
|---|---|
| 事件流过滤 | **无** —— 全量推送,所有市场所有账户。单市场无所谓,几十个市场时客户端的带宽与 JSON 解析会成为瓶颈 |
| 订阅协议 | 无。代价是带宽,收益是「重连即完整」 |
| 广播容量 | 8192 条 |
| 批量单笔上限 | 未设硬限,受保证金与请求体大小约束 |
| 单张下单接口 | 无,用 `len(orders)==1` |

真到多市场高频的规模,值得加的是**按 market/account 的服务端过滤**(连接时带 filter 参数,
而不是完整的订阅协议)—— 这样能保住「重连即完整」这个性质。目前没有。
