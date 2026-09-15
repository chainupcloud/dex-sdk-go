# 故障排查

按**症状**查,不是按错误码查 —— 接入时最难的正是「这个 401 到底是哪一环」。

---

## 401:签名被拒

`BadSignature` / `UnboundSigner` / `SignatureExpired` 都映射到 401。

**重试永远不会好。** 这一类是编码、域或密钥的问题,不是瞬时故障。

按可能性排序:

| 可能 | 怎么确认 | 怎么修 |
|---|---|---|
| `chainId` 不对 | 与部署方给的值核对 | `NewClient` 传对 |
| codec 版本变了 | `c.Version(ctx).CodecVer` 与 SDK 所针对的值比 | 升级 SDK,重新生成金样 |
| JSON 与签名的命令不一致 | 见下 | 保证同一条命令 |
| v 位序错(仅移植时) | 恢复出的地址与预期不符 | `r‖s‖v`,不是 `v‖r‖s` |

### 「JSON 与签名的命令不一致」怎么查

签名承诺的是**规范编码的哈希**,而服务端按**请求体 JSON** 重建命令再重编比对。
两者描述的必须是同一条命令。

最常见的具体错法:

- `nowMs` 签名时用了 `t1`,JSON 里写了 `t2`(比如各调了一次 `time.Now()`)
- `tif` 的枚举值对不上(`0` GTC / `1` IOC / `2` PostOnly / `3` FOK)
- 撤单的 `market` 与 `seq` 组合成的 `OrderId` 与签名时不同

自查方法:把 `EncodeBatchPlace(...)` 的结果打出来,与你 JSON 里的字段逐项对。

```go
cmd := dexos.EncodeBatchPlace(account, nowMs, orders)
fmt.Printf("%x\n", cmd)                       // 与 golden.json 的布局对照
fmt.Printf("%x\n", dexos.Keccak256(cmd))      // 这就是 commandHash
```

---

## 409:NonceMismatch

重读 nonce 不能恢复原请求回执。先保留原请求身份并停止发单，核对是否已有执行结果；
禁止换 nonce 盲重发。Session 已锁定时 Resync 也会拒绝；不能通过新建 Session 绕过。

先确认你用的是哪个计数器:

| 操作 | 用谁的 nonce |
|---|---|
| `ApproveAgent` / `RevokeAgent` | `c.NextNonce(ctx, master)` |
| `BatchPlace` / `BatchCancel` / `BatchReplace` | `c.AgentNonce(ctx, master, agentAddr)` |

**混用是最常见的接入坑**,而错误信息里看不出是哪个计数器错了。

其他常见成因:

- 多个进程/实例用同一把 API 钱包 → 各自维护本地计数器必冲突。
  **一把钥匙一个进程**,要并行就多授权几把。
- 撤销后重新授权,以为 nonce 归零 → **不会重置**(防旧签名重放),必须重读。
- 本地自增时某一轮实际失败了,计数器却加了 → 出错路径别加。

---

## 422:业务拒绝

| 拒因 | 含义 | 处置 |
|---|---|---|
| `UnknownAgent` | 未授权,或已被撤销 | 重新授权(要主账号密钥) |
| `AgentExpired` | 授权过期 | 同上,重复授权 = 更新有效期 |
| `AgentScopeViolation` | 该命令不在 agent 白名单(如提款) | 用主账号做,不是 API 钱包能做的事 |
| `UnknownAgent` | 这个账户没有授权过这把 API 钱包(或已撤销) | 去前端为**那个账户**授权 |
| `InsufficientMargin` | 前置保证金检查未过 | 减小规模,或看 `Risk().NC/IMR` |
| `InsufficientCollateral` | 可提额不够 | 可提 = NC − IMR |
| `PostOnlyWouldCross` | PostOnly 会立即穿越 | 价格挂到对手价之外 |
| `FokInsufficientLiquidity` | FOK 流动性不足 | 换 IOC,或减量 |
| `ReduceOnlyInvalid` | 无持仓或同向加仓 | 检查持仓方向 |
| `MarketNotTrading` | 市场状态门控 | 看 `Markets()[i].Status`,退避 |
| `NotAligned` | 价格/数量未对齐撮合粒度 | 按 `subticksPerTick` / `stepBaseQuantums` 取整 |

---

## 事件流

### 收不到任何事件

1. `BaseURL` 是 `http://`(SDK 自动转 `ws://`),不要自己写 `ws://`
2. 走了代理?`http_proxy` / `all_proxy` 会把 `127.0.0.1` 也代理掉。
   本机连接设 `no_proxy=127.0.0.1,localhost`
3. `Stream.Err` 里有没有东西

### 收到 `Lost`

本地镜像与历史完整性已不可信。流会终止，快照不能补回逐笔成交。

```go
case n, ok := <-st.Lost:
    if ok { log.Printf("丢帧 %d 条", n) }
    return <-st.Err
```

丢帧的根因通常是**你消费太慢**:`Events` 通道满了 → SDK 的读循环阻塞 →
服务端广播积压 → 丢。处理事件的逻辑要快,重活丢给别的 goroutine。

### 持仓/盘口对不上

按顺序排除:

1. 有没有漏处理 `Lost`(最常见)
2. 有没有在**拉快照之后**才订阅(两步之间的事件丢了,且无声无息)
3. 断线后有没有停止；重新连接前是否已用可靠历史补齐逐笔事件并核对（SDK 不自动重连）

---

## 授权相关

### 授权成功但下单报 `UnknownAgent`

授权是按 **(账户, agent) 对**记的:给主账户授权过,不等于给子账户也授权了。
`account` 参数指的那个账户,需要**它自己**授权这把 key。

用 `GET /agent/<agent 地址>` 看这把 key 到底被哪些账户授权了,或
`Agents(ctx, master)` 从账户那一侧查。

### `ErrAmbiguousAccount`

这把 key 被**多个**账户授权,而 `New` 没被告知代谁。它刻意不挑默认值 ——
挑错的表现是订单落到另一个子账户上,仓位、保证金、风险全记在别处,
**而且没有任何报错**。

```go
s, err := dexos.New(ctx, url, apiKey, dexos.ForAccount(7))
```

报错里会列出候选账户号。若你以为只授权过一个,那说明另一个账户也授权了同一把
key —— 去前端逐个撤销多余的,或直接指明。

### 授权后 `Agents()` 里看不到

- 查的是不是同一个 `master`
- 授权那个请求真的返回 200 了吗(`ApproveAgent` 的 error 有没有被忽略)

---

## 单位与精度

### 价格差了几个数量级

`Price` 是 **tick**,不是人类价:

```go
m := markets[0]
if m.PriceDecimals == nil { return fmt.Errorf("市场 %d 缺价格精度", m.Market) }
// 人类价格 = order.Price × 10^(-*m.PriceDecimals)，使用 decimal 等精确十进制实现。
```

`Lots` 同理,除以 `10^SizeDecimals`。

### 金额算出来不对

`Collateral` / `NC` / `CostBasis` 这些是**字符串**,不是数字。内核用 i128,
JSON number 装不下,转 float 会在大额上悄悄丢精度 —— 用 `big.Int` 解析。

---

## 环境

### `go test ./dexos/` 报「读金样失败」

金样是从 dex-os 生成的,仓库里应当已有 `dexos/testdata/golden.json`。
自己重新生成:

```bash
cd /path/to/dex-os
cargo run -q --example sdk_golden > /path/to/dex-sdk-go/dexos/testdata/golden.json
```

### 金样对拍失败

内核改了规范编码而 SDK 没跟上。看失败条目的 Rust/Go 两行 hex,逐字节对
—— 布局见 [ARCHITECTURE.md](ARCHITECTURE.md#规范编码的具体布局)。

**不要改金样去迁就 SDK。** 金样是从内核生成的,它才是真理源。
