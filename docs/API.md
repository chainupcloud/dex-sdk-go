# REST 接口参考

网关默认监听 `:8080`。所有价格为 tick、数量为 lot、金额为 quote quantum(字符串)。

> 金额字段一律是**字符串**:内核用 i128,JSON number 装不下,转 float 会在大额上悄悄丢精度。

---

## 只读

### `GET /health`

存活探测。返回纯文本 `ok`。

### `GET /version`

状态机**语义指纹**。接入时值得核一次 —— `codecVer` 对不上意味着规范编码可能已经变了,
而那种错误只表现为「签名一律被拒」。

```json
{"pkg":"0.1.0","snapshot_ver":19,"codec_ver":1,"build_id":"dev"}
```

### `GET /config`

**连接参数发现。** 有了它,接入方只需要知道一个网关地址 —— SDK 的
`dexos.Connect` / `dexos.Discover` 就是读这里。

```json
{
  "chainId": 11155111,
  "domain": {"name":"dex-os","version":"1","verifyingContract":"0x0000…0000"},
  "deposit": {
    "systemGateway": "0x5E34…5d68",
    "tokens": [{"token":0,"erc20":"0xc607…1c86","extraWeiDecimals":0,"finalized":true}]
  },
  "codecVer": 1, "snapshotVer": 19, "finalityMode": "head"
}
```

`token` 映射取自**内核**(`LinkToken` 写进状态机,入金识别按它判定),不是另存的副本。

`deposit.systemGateway` **可能缺失** —— 它是宿主/中继层概念,内核不认识它,
只有部署信息被注入网关时才给得出。缺失意味着「这个节点没告诉你」,
而不是「不需要入金地址」:此时应当向运营方索要,**不要猜**。

### `GET /agent/:addr`

这把 API 钱包代表哪个账户。`dexos.New` 用它把身份补全 —— 所以接入方**不需要
被告知内核账户号**。

```json
{"agent":"0xd864…d001","master":4,"validUntilMs":"1789…","expired":false,"nextNonce":3}
```

未授权或已撤销一律 **404**(两者在这里是同一件事:这把钥匙现在不代表任何人)。
`nextNonce` 是 **agent 地址自己的**计数器,与 master 的 nonce 无关。

### `GET /markets`

```json
{"markets":[{
  "market":0,"symbol":"BTC-USD","status":"Active",
  "oracle":118000,"mark":118003,"bestBid":117977,"bestAsk":118023,
  "openInterestLots":200,"fundingRatePpm":0,"fundingIndex":"0",
  "initialMarginPpm":50000,"maintenanceFractionPpm":600000,
  "quotePerTickLot":1000,"priceDecimals":0,"sizeDecimals":3
}]}
```

- `mark` 是标记价:`clamp(median3(oracle, median3(bid,ask,last), oracle+basis), oracle±2%)`。
  **盘口对称且无成交时 `mark == oracle`** —— 两个数一样不是 bug。
- 清算判定用的是 `oracle`,不是 `mark`。

### `GET /book/:market?n=<档数>`

```json
{"bids":[[117977,20],[117953,20]],"asks":[[118023,20]],"market":0,"oracle":118000}
```

每档是 `[价格, 数量]`。bids 按价降序、asks 按价升序。

### `GET /risk/:account`

```json
{"exists":true,"accountId":3,"collateral":"49992707060","nc":"49992707060",
 "imr":"0","mmr":"0","withdrawable":"49992707060","nextNonce":0,
 "positions":[{"market":0,"lots":200,"costBasis":"23625880000","fundingIndex":"0"}]}
```

- `nc < mmr` 即可被清算。**这是权威判定**,前端展示的强平价只是估算。
- `nextNonce` 是 **master 账户**的计数器(授权/撤销/提款用),不是 agent 的。
- `collateral` 是现金:买入已付全额名义,所以持仓账户常为负;`nc = collateral + Σ仓位市值 + Σ未结算资金费`。

### `GET /orders/:account?market=<m>`

```json
{"account":3,"market":0,"seqs":[337,336],
 "orders":[{"seq":337,"side":"buy","price":117867,"lots":1,"remaining":1}]}
```

`seqs` 是便捷数组,直接喂给撤单/换单。

### `GET /agents/:master`

该账户授权过的 API 钱包。

```json
{"master":3,"nowMs":"1788333385329","agents":[
  {"address":"0x…","validUntilMs":"1803887990472","expired":false,"nextNonce":2}]}
```

- **过期条目照常返回**并带 `expired` —— 它们仍占着那个地址(重授同一地址是更新有效期
  而不是新建),看得见才管得着。
- `nextNonce` 是**这个 agent 自己的**计数器,代执行签名要用它。

### 其他

`GET /account/:id` · `/account/by-address/:addr` · `/account/by-owner/:addr` ·
`/candles?market=&tf=&limit=` · `/trades?account=&limit=` · `/state-root` · `/proof/:id` ·
`/liquidity-tiers` · `/vaults`

---

## 主账号签名(管理 API 钱包)

EIP-712 域固定为:

```json
{"name":"dex-os","version":"1","chainId":<节点 chainId>,"verifyingContract":"0x0…0"}
```

`verifyingContract` 恒为零地址 —— 验签在 STF 内完成,没有链上验证合约。

### `POST /agent/approve`

```
ApproveAgent(address owner,uint32 master,address agent,uint64 validUntilMs,uint64 nonce)
```

```json
{"owner":"0x…","master":3,"agent":"0x…","validUntilMs":1803887990472,
 "nonce":0,"signature":"0x…"}
```

`validUntilMs` 为 `0` 表示永不过期。nonce 用 **master 账户**的。

同一把 agent 可被**多个账户各自授权**;对同一 (账户, agent) 对重复授权 = 顺延有效期。
拒因:`AgentExpired`(给的有效期已是过去)。

### `POST /agent/revoke`

```
RevokeAgent(address owner,uint32 master,address agent,uint64 nonce)
```

幂等。撤销是**全局失效**,不是标记过期 —— 撤完该 agent 的任何签名立刻返回 `UnknownAgent`。

---

## API 钱包签名(交易)

三个写入口共用同一套机制:

```
规范编码内层命令 → keccak256 → AgentExec(bytes32 commandHash,uint64 nonce) → agent 签名
```

服务端按请求体里的 JSON **重建**内层命令,再用自己的编码器重编、比对哈希。
所以 JSON 字段与你签名时编码的命令必须描述同一条命令,否则 401。

nonce 用 **agent 自己的**(`/agents/:master` 里的 `nextNonce`)。

### `POST /batch` — 批量下单

内层命令 `BatchPlace`。逐张独立判定,一张失败不拖累其余。

```json
{"agent":"0x…","account":3,"nowMs":1788333385329,"nonce":0,"signature":"0x…",
 "orders":[{"market":0,"side":"buy","price":117900,"lots":1,"tif":2,"reduceOnly":false}]}
```

`tif`:`0` GTC · `1` IOC · `2` PostOnly · `3` FOK。

`nowMs` 由**客户端**提供并进入签名 —— 内核不读时钟,一切非确定输入由命令携带。

### `POST /cancel` — 批量撤单

内层命令 `BatchCancel`。逐张尽力而为(不存在/已成交/非本人的跳过)。

```json
{"agent":"0x…","account":3,"market":0,"orders":[337,336],"nonce":1,"signature":"0x…"}
```

注意这条**没有 `nowMs`** —— 撤单不需要时间语义,编码里也没有这个字段。

### `POST /replace` — 原子换单(先撤后挂)

内层命令 `BatchReplace`。

```json
{"agent":"0x…","account":3,"market":0,"cancels":[337,336],
 "orders":[{"market":0,"side":"buy","price":117850,"lots":2,"tif":2,"reduceOnly":false}],
 "nowMs":1788333385329,"nonce":2,"signature":"0x…"}
```

事件序列必然是先 `OrderCanceled` 后 `OrderAccepted` —— 顺序即语义。反过来会让新旧报价
短暂同时在簿上,名义敞口瞬时翻倍,可能撞上保证金检查让新单被拒。

---

## 拒因

写操作失败时返回 `{"error":"<拒因>"}`,HTTP 状态:

| 状态 | 含义 |
|---|---|
| 400 | 请求体格式问题(地址不合法、签名不是 65 字节、tif 越界) |
| 401 | 签名验证失败 —— **通常是规范编码或域分隔符不对**,不是密钥问题 |
| 409 | nonce 不匹配(陈旧或跳号) |
| 422 | 业务拒绝,见 `error` 字段 |

常见业务拒因:

| 拒因 | 含义 |
|---|---|
| `UnknownAgent` | agent 未授权或已被撤销 |
| `AgentExpired` | 授权已过有效期 |
| `AgentScopeViolation` | 该命令不在 agent 白名单内(如提款) |
| `AgentMasterMismatch` | 试图以非授权 master 的名义行动 |
| `InsufficientMargin` | 前置保证金检查未过 |
| `PostOnlyWouldCross` | PostOnly 会立即穿越 |
| `FokInsufficientLiquidity` | FOK 流动性不足 |
| `ReduceOnlyInvalid` | 无持仓或同向加仓 |
| `MarketNotTrading` | 市场状态门控(Paused/CancelOnly/FinalSettlement) |

---

## 接入检查清单

1. `GET /config` 取连接参数并核 `codecVer` —— 对不上先停,别去调签名
   (`GET /version` 也给同样的指纹,但不含链与合约信息)
2. `chainId` 与节点一致,否则域分隔符不同,签名一律被拒
3. 分清两个 nonce:master 的用于授权/撤销,agent 的用于交易
4. 撤销后重新授权,agent nonce **不重置**,必须重读
5. 金额字段按字符串解析,别用 float
