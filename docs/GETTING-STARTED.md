# 接入指南

从零到第一笔成交。每一步都给出**怎么确认它成功了** —— 接入过程中最耗时的不是写代码,
是搞不清上一步到底成没成。

---

## 0 · 前置

```bash
go get github.com/chainupcloud/dex-sdk-go
```

需要三样东西:

| 东西 | 从哪来 |
|---|---|
| 网关地址 | 如 `http://127.0.0.1:8080` |
| `chainId` | `GET /version` 里没有,由部署方给;本地 anvil 是 `31337` |
| 主账号私钥 | 你自己的 EVM 钱包私钥 |

**第一件事是核指纹**,别急着写业务:

```go
c := dexos.NewClient("http://127.0.0.1:8080", 31337)
v, err := c.Version(ctx)
// {Pkg:0.1.0 SnapshotVer:19 CodecVer:1 BuildID:dev}
```

`CodecVer` 对不上你这个 SDK 版本所针对的值,说明**规范编码可能已经变了**。
那种错误的唯一表现是「签名一律被 401 拒」,不会有任何指向根因的信息 ——
在这里撞上比在业务里撞上便宜太多。

---

## 1 · 有一个内核账户

SDK **不负责开户与入金** —— 那是链上动作,走的是「向系统地址转账」这条路,
与交易 API 是两条不同的信任路径。

入金唯一入口:**用户对系统地址的裸 `transfer`**。中继观察到之后注入内核,
账户在首次入金时被惰性创建。

```bash
# 本地 dev-up 环境:USDC_ADDR / SYSTEM_GATEWAY 从 data/dev/deploy.env 取
cast send $USDC_ADDR "transfer(address,uint256)" $SYSTEM_GATEWAY 50000000000 \
  --private-key $YOUR_KEY --rpc-url http://127.0.0.1:8545
```

**怎么确认成功:**

```go
// 地址 → 内核账户号
var acct struct{ AccountID uint32 `json:"accountId"` }
// GET /account/by-address/0x…
```

拿到 `accountId` 就成了。注入不是即时的 —— 中继要等链上终局性,本地约 1–3 秒,
生产按 `FINALITY_MODE` 而定(默认 `finalized`,真链上是分钟级)。

---

## 2 · 生成并授权 API 钱包

```go
owner, _ := dexos.NewSigner(masterPrivHex)
api, _ := dexos.GenerateSigner()          // 私钥只在本进程内存里

fmt.Println("保存好,只有这一次:", api.PrivateKeyHex())

n, _ := c.NextNonce(ctx, master)          // ← master 账户的 nonce
_, err := c.ApproveAgent(ctx, owner, master, api.Address(),
    time.Now().AddDate(0, 6, 0), n)       // 半年;传零值 time.Time = 永不过期
```

**怎么确认成功:**

```go
agents, _ := c.Agents(ctx, master)
// [{Address:0x… ValidUntilMs:1803887990472 Expired:false NextNonce:0}]
```

**授权完就把主账号私钥收起来。** 之后日常交易只用 `api`,主账号密钥只在续期或撤销时
再拿出来一次。这是 API 钱包全部的意义 —— 见 [ARCHITECTURE.md](ARCHITECTURE.md#信任边界)。

**要用同一把 key 交易子账户?** 再授权一次即可 —— 授权按 `(账户, agent)` 对记,
每个账户各自同意。此后 `dexos.New` 需要你指明代谁:

```go
s, _ := dexos.New(ctx, url, api.PrivateKeyHex(), dexos.ForAccount(subAccountID))
```

不指明会得到 `ErrAmbiguousAccount` —— 它不替你挑默认值,因为挑错的表现是订单
落到另一个子账户上,**而且没有任何报错**。

---

## 3 · 第一笔单

```go
an, _ := c.AgentNonce(ctx, master, api.Address())   // ← agent 自己的 nonce,不是上面那个

ev, err := c.BatchPlace(ctx, api, master, []dexos.BatchOrder{
    {Market: 0, Side: dexos.Buy, Price: 117900, Lots: 1, TIF: dexos.PostOnly},
}, an)
```

没有单张下单接口 —— 批量的开销就是一次共识轮,拆开只会多花往返。`len(orders)==1` 即单张。

**怎么确认成功:** 返回的 `ev` 里有 `OrderAccepted`,`data.orderSeq` 就是订单序号。
或者:

```go
seqs, _ := c.OrderSeqs(ctx, master, 0)    // [337]
```

### 价格与数量的单位

**SDK 不做单位换算**,一切在整数域:

```go
ms, _ := c.Markets(ctx)
m := ms[0]
// 先确认 m.PriceDecimals / m.SizeDecimals 非 nil（缺元数据不能当0）。
// 人类价 = Price / 10^(*m.PriceDecimals)，人类量同理；金额转换使用十进制数。
```

不替你转是刻意的:引入浮点之后,`0.1+0.2` 那类误差会直接进到订单价上。

---

## 4 · 撤单与换单

```go
seqs, _ := c.OrderSeqs(ctx, master, 0)
an, _ := c.AgentNonce(ctx, master, api.Address())
c.BatchCancel(ctx, api, master, 0, seqs, an)
```

刷新报价用**原子换单**,不要「先撤再挂」:

```go
c.BatchReplace(ctx, api, master, 0, seqs, newQuotes, an)
```

语义与限制见 [MARKET-MAKING.md](MARKET-MAKING.md#换单与逐项结果)。

---

## 5 · 接事件流

```go
st, err := c.Subscribe(ctx)
if err != nil { return err }
for {
    select {
    case ev, ok := <-st.Events:
        if !ok { return <-st.Err }
        switch ev.Kind {
        case "Fill":          // 成交
        case "OrderAccepted": // 挂上
        case "OrderCanceled": // 撤掉
        case "OracleUpdated": // 喂价
        }
    case n, ok := <-st.Lost:
        if ok { log.Printf("丢帧 %d 条", n) }
        return <-st.Err        // 终止;用 s.Fills(FillQuery{After: 最后一个 ev.ID(), Epoch: 纪元}) 补齐再重连
    case err := <-st.Err:
        return err
    }
}
```

流终止后的补齐(断线 / Lagged / 坏帧都走这条):

```go
var lastID string                 // 循环里每收到一条 ev 就更新:lastID = ev.ID()
h, err := s.Fills(ctx, dexos.FillQuery{After: lastID, Epoch: epoch}) // 自动翻到上界;epoch = 上次结果的 h.Epoch
for _, f := range h.Fills { apply(f) }                                 // 与流上已处理的按 f.ID 去重
st, err = s.Subscribe(ctx, markets...)                        // 再重连
```

`ev.ID()` 与 `/fills` 里同一笔的 `id` 逐字符相同,所以它既是去重键也是续拉游标。持仓 / 盘口
快照(`Risk` / `Book`)只能校准**状态**,补不回逐笔成交。详见 [做市完整性边界](LIQUIDITY-INTEGRATION.md)。

---

## 6 · 上线前的检查清单

- [ ] `Version().CodecVer` 与 SDK 版本匹配
- [ ] `chainId` 与节点一致(不一致时域分隔符不同,签名一律 401)
- [ ] 两个 nonce 分清了:master 的用于授权/撤销,agent 的用于交易
- [ ] API 钱包私钥**不在**代码库/日志/环境变量明文里
- [ ] 主账号私钥**不在**跑策略的机器上
- [ ] 授权设了有效期,不是永久
- [ ] `Stream.Lost` 有处理分支,不是丢弃
- [ ] 断线/坏帧后停止；可靠历史补齐并核对后才能重建连接（SDK 不自动重连）
- [ ] 金额字段按**字符串**解析,没有转 float
- [ ] 拒因按**错误类型**分类:`*RejectedError`(业务拒绝,200 里,终局)/ `ErrExecutionPending`(已定序结论未知,别重发)/ `*APIError`(401 签名、421 打错节点、5xx)

---

## 完整可跑的例子

```bash
go run ./examples/quickstart -master-key 0x… -account 3
```

它走完授权 → 下单 → 换单 → 撤单 → 撤销,最后一步是**反证**:撤销后同一把钥匙再下单
必须被拒。没有那一步,前面全绿也不能说明授权真的起了作用。

时延自测:

```bash
go run ./examples/wslatency -master-key 0x… -account 3 -n 300
```
