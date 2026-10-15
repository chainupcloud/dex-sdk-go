# dex-sdk-go

DEX-OS(永续合约 + 现货)交易所的 Go SDK。围绕 **API 钱包(代理钱包)** 设计 —— 主钱包授权一个
派生密钥代为交易,该密钥**动不了钱**。

```bash
go get github.com/chainupcloud/dex-sdk-go
```

| 包 | 是什么 |
|---|---|
| [`dexosv1`](dexosv1/doc.go) | **开放 API v1(`/v1/*`)客户端,新接入一律用它。** 市场按符号(`BTC-USD`)寻址,账户按主钱包地址 + 子账户序号(或金库号)寻址 |
| [`dexos`](dexos/doc.go) | 签名([`Signer`](dexos/signer.go)、EIP-712 [`Domain`](dexos/eip712.go))、地址、规范命令编码、回执类型 —— `dexosv1` 建在它们上面。它的 HTTP 客户端(`Client`、`Session`、`Connect`、`Discover`)走 dex-os 的旧公开端点,dex-os 正在退役那些端点(`public-api-v1.md` §9),这几个入口已标 **Deprecated** |

## 示例与文档

| | 什么时候用 |
|---|---|
| [`examples/session`](examples/session/main.go) | **最小接入**:URL + 一把 API 钱包私钥,挂单撤单 |
| [`examples/onboard`](examples/onboard/main.go) | 从零接入:连接参数 → 地址 → 账户 →(可选)API 钱包交易 |
| [`examples/quickstart`](examples/quickstart/main.go) | API 钱包完整生命周期(授权 → 批量下单 → 原子换单 → 断线保护 → 撤全部 → 撤销 → 反证),也是对着真节点的集成自检 |
| [`examples/vault`](examples/vault/main.go) | **以金库身份交易**(测试栈:入金 → 建库 → manager 账户授权 API 钱包 → 金库身份会话 → 挂单 / 撤单);`GW=… ADMIN_TOKEN=… go run ./examples/vault -state <文件>` |
| [`examples/wslatency`](examples/wslatency/main.go) | 实测「写回执 ↔ 推送收到」的时延分布 |
| `dexosv1` 契约测试 | 对着真节点逐个响应对规格:读(`contract_test.go`)、写(`TestContractWrites`)、推送(`contract_ws_test.go`)、金库(`contract_vault_test.go`);`go test -tags dexos_contract`,环境变量见各文件头 |
| [`docs/`](docs/) | 写于旧接口客户端时期。概念部分(账号体系、原子换单、读一致性、拒因、做市完整性边界)仍然适用;**代码以本 README、`dexosv1` 文档注释与上面的示例为准** |

---

## 账号体系

```
主钱包(EVM 地址;名下账户按子账户序号区分,0 = 主账户)
  │  能做一切:入金、提款、授权 / 撤销 API 钱包、建金库
  └─ 授权 ─→ API 钱包(agent 地址)
              只能交易:下单 / 撤单 / 换单 / 撤全部 / 断线保护
              动不了钱:提款、转账不在执行层的 agent 白名单内
```

**为什么这么分。** 跑策略的机器早晚会被摸进去 —— 可能是依赖投毒、可能是运维失误。
把主钱包私钥放在那台机器上,被摸进去等于资产清零;放 API 钱包私钥,最坏后果是被人
代你交易,钱还在账上,撤销授权即止损。

这个边界是**在确定性状态机里强制**的,不是网关的一层检查:不在 agent 白名单里的命令一律
`AgentScopeViolation`。新增命令不会自动获得 agent 权限。

v1 不按内核账户号寻址,也不在响应里给它。签名结构体里要填的 `accountId` 由 SDK 从
`/v1/account/nonce` 读出来填,调用方不用管。

### 两种写入身份,两条 nonce

| 身份 | 做什么 | nonce |
|---|---|---|
| [`Session`](dexosv1/session.go)(API 钱包代签) | 批量下单 / 撤单 / 原子换单、撤全部、断线保护 —— 做市跑在这里 | API 钱包**自己的**计数器 |
| [`Wallet`](dexosv1/session.go)(主钱包自签) | 授权 / 撤销 API 钱包、单笔下单 / 撤单、设杠杆、金库的建库 / 存入 / 赎回 / 认领 / 改配置 | 主钱包作用域的计数器 |

两者都替你维护 nonce、把写串行化。混用两个计数器得到的是 `NonceMismatch`,错误信息里看不出是
哪个错了 —— 这是最常见的接入坑,也正是这两个类型要替你挡掉的东西。

---

## 初始化

**一个网关地址 + 一把 API 钱包私钥。**

```go
c := dexosv1.NewClient("http://<部署机 IP>:17807")
agent, _ := dexos.NewSigner("0x<API 钱包私钥>")

// 按 API 钱包地址反查它被授权的账户;只授权了一个账户时就是它
grants, _ := c.AgentGrants(ctx, agent.Address())
owner, _ := dexos.ParseAddress(grants[0].Address)
ref := dexosv1.AccountRef{Address: owner, Subaccount: *grants[0].Subaccount}

s, err := dexosv1.NewSession(ctx, c, agent, ref)   // 读 EIP-712 域、核对授权、读 nonce
if err != nil { log.Fatal(err) }

cat, _ := c.Catalog(ctx)
btc, _ := cat.Market("BTC-USD")
px, _ := btc.Scale.Ticks("70000")      // 十进制价 → tick,不整除就报错
sz, _ := btc.Scale.Lots("0.001")
res, err := s.Place(ctx, dexosv1.OrderRequest{
    Market: btc, Side: dexos.Buy, Price: px, Lots: sz, TIF: dexos.PostOnly,
})
```

一把 API 钱包可以被主账户与各个子账户**分别**授权(状态机按「账户 × agent」记账,撤销其中一个
不影响其余)。授权了不止一个账户时**必须指明** `AccountRef`,SDK 不替你挑 ——
挑错的表现是订单落到另一个子账户上,仓位、保证金、风险全记在别处,**而且没有任何报错**。
[`examples/session`](examples/session/main.go) 演示了过期授权先剔除、剩下不止一个就报错的挑法。

`NewSession` 替你做掉三件容易错的事:

| | 做错了会怎样 |
|---|---|
| 从 `/v1/exchange` 读 EIP-712 域(链号、合约地址) | 链号填错 → 每笔签名被拒,而盘口 / K 线全正常,像「只有下单坏了」 |
| 核对这把钥匙确实被 `ref` 授权、没过期 | 没授权 → 一片拒绝;本地先报 `ErrAgentNotAuthorized` / `ErrAgentExpired` |
| 读取并维护 **API 钱包自己的** nonce | 与主钱包的 nonce 混用 → `NonceMismatch` |

API 钱包私钥从前端拿:`/app` →「API 钱包」→ 生成 → 主钱包签一次授权,私钥只显示一次。

### 写回执与错误

写入一律等结论(`ack=result`,等待上限 `Client.AckWait`,最多 5s)。`Place` / `Cancel` / `Replace` 返回
[`*BatchResult`](dexosv1/write.go):逐项结局(`accepted` / `canceled` / `rejected`,带订单号、成交、拒因码)。

| 错误 | 含义 | 怎么办 |
|---|---|---|
| `*dexosv1.Error` | 业务拒绝或网关拒绝(`Code` 是拒因,如 `InsufficientMargin`) | 终局结论,会话不锁,改参数再发 |
| `*dexosv1.RateLimitedError` | 429:扣光的是 IP 桶还是账户桶、`RetryAfter` | SDK 不重试;怎么退让(例如只暂停一个币对)由你定 |
| `dexosv1.ErrPending` | 已定序,结论未知(202 / 传输失败 / 5xx) | **不要重发**。会话已锁,用 `s.Pending()` 的请求身份查 `c.Request`,核对后新建会话 |
| `dexosv1.ErrIncompleteBatch` | 批量回执的逐项结局对不上请求 | 批次已执行但说不清哪些成了:先按账户读取核对 |
| `dexosv1.ErrSessionBlocked` | 上一次写入结论未知,会话禁止继续消耗 nonce | 同 `ErrPending` |

要把「换单发出 → 结论」写进自己的持久化日志(进程崩溃后能对账),用 `Session.ReplaceJournaled`。

### 以金库身份交易

金库主账户由 manager 操盘:API 钱包授权在 manager **认领金库所用的那个账户**上
(`Vault.Manager` / `Vault.ManagerSubaccount`),会话用金库身份寻址:

```go
s, err := dexosv1.NewSession(ctx, c, agent, dexosv1.VaultAccount(vaultID))
```

读写的是金库主账户,nonce 仍是这把 API 钱包自己的计数器。金库身份只能交易(下单、撤单、换单、
撤全部、断线保护),资金类动作网关以 403 拒。读接口同样接受 `VaultAccount(id)`。
完整流程见 [`examples/vault`](examples/vault/main.go)。

---

## 连接参数

链号、EIP-712 域、入金系统地址、token 表、命令编码版本都由 `/v1/exchange` 给:

```go
ex, err := c.Exchange(ctx)
fmt.Println("链", ex.ChainID, "入金地址", ex.Deposit.SystemGateway)
if ex.Version.CodecVer != dexos.CodecVersion {
    log.Fatal("网关的命令编码与本 SDK 不同,签出来的命令它不认 —— 先升级 SDK")
}
```

为什么不建议把这些值写死在自己的配置里 —— **填错的后果都很隐蔽**:

| 填错什么 | 你会看到什么 |
|---|---|
| 链号 | 每一笔签名被拒,而盘口、K 线、持仓(只读)**全部正常** |
| 入金地址(运营方重新部署过合约) | 钱打到没人监听的合约:链上扣了,账户里没有 |
| 命令编码版本 | 规范编码变了,API 钱包代签的哈希对不上,同样是一片拒签 |

不同部署的链不同(本地 anvil 是 31337,test 档接 Sepolia 是 11155111);运营方每次重建环境
都会重新部署合约,地址随之改变。**别把地址抄进代码**,往过期的系统地址打钱是收不回来的。

---

## 从零接入

```bash
go run ./examples/onboard -url http://<部署机 IP>:17807
```

**1. 地址** —— 用你已有的钱包,或让 SDK 生成一个:

```go
me, _ := dexos.GenerateSigner()          // 本地生成,私钥不出本进程
```

**2. 入金** —— 往 `ex.Deposit.SystemGateway` 发一笔 USDC 的**裸 `transfer`**。没有桥合约、没有
`deposit()` 方法。中继观察到之后,执行层**自动开户**并绑定你的 EVM 地址 —— 没有注册接口,也没有
审批,付钱即开户。

> **这一步 SDK 做不了。** 本 SDK 只管交易所 API,不含 EVM 能力(不签链上交易、不发 ERC20 transfer)。
> 用前端页面、钱包,或 `go-ethereum` 自己发。

**3. 确认开户** —— 入金之前账户表是空的(账户类读取答 404),那是正常起点,不是错误:

```go
subs, _ := c.Subaccounts(ctx, me.Address())
if len(subs) == 0 {
    // 还没入金
}
acct, _ := c.Account(ctx, dexosv1.AccountRef{Address: me.Address()})
fmt.Println("抵押", acct.Collateral, "USDC")
```

**4. 授权 API 钱包,然后交易。** 三条路都行:

- **浏览器**(推荐给人用):`http://<部署机 IP>:17807/app` →「API 钱包」→ 生成 → 命名 → 选有效期 →
  主钱包签一次。私钥只显示一次。
- **代码**(推荐给自动化):

  ```go
  owner, _ := dexos.NewSigner(masterPrivHex)
  w, _ := dexosv1.NewWallet(ctx, c, owner, 0)          // 0 = 主账户
  api, _ := dexos.GenerateSigner()                     // 私钥不出本进程,交易所只拿到地址
  w.ApproveAgent(ctx, api.Address(), time.Now().AddDate(0, 6, 0), "my-bot")
  s, _ := dexosv1.NewSession(ctx, c, api, w.Account)   // 主钱包私钥到这里就可以收起来了
  ```

- **主钱包密钥不在服务端**(如后台生成 API 钱包、用户用浏览器钱包签):`Domain.ApproveAgentTypedData(...)`
  给出 `eth_signTypedData_v4` 可签的结构(签名所需的 `signingAccountId` 与 nonce 从 `c.Nonce(ctx, ref, nil)` 读),
  拿回签名后用 `c.SubmitApproval(...)` 提交,参数须与签名时逐字一致。

完整生命周期(授权 → 下单 → 换单 → 撤单 → 撤销 → 反证)见 [`examples/quickstart`](examples/quickstart/main.go):

```bash
go run ./examples/quickstart -url http://127.0.0.1:17807 -master-key 0x…
```

### 三个容易踩的

- **没入金就下单** → 没有账户就没有可授权、可交易的对象:`Subaccounts` 答空表,账户类读取答 404。
- **入金到账时间随终局性配置变化**:`head` 是秒级,`finalized` 要等两个 epoch(Sepolia 约 13 分钟)。
  别把「还没到账」当成「入金失败」。
- **偶发的 412 不是故障。** 生产是读写分层的(写打 Raft voter,读打只读副本),412 表示副本还没追上
  你刚写的那条日志 —— 它宁可明说落后,也不返回旧值。账户类读取 SDK 自动带上位号头(`x-dexos-min-seq`)
  把这条链挂住;真拿到 412 就是所有副本都落后了,重试即可。

---

## 原子换单

做市商刷新报价的基本原语。**先撤后挂,一次状态机转移完成。**

```go
res, err := s.Replace(ctx, btc, oldOrderIDs, newQuotes)
```

与「先 `Cancel` 再 `Place`」的区别不是省一次往返:

- 两次独立提交是**两条共识条目**,中间存在一个没有报价的窗口
- 第二条若被拒(保证金不足、市场停牌),第一条**已经生效** —— 你的单被撤光却没挂上新的,单边裸露

合成一条后,撤与挂落在同一个转移里,不存在中间态可被观测。

「原子」的准确含义是**没有中间窗口**,不是「全成功或全回滚」:逐项仍尽力而为(撤不掉的跳过、挂不上的
跳过),否则一张单的保证金不足会废掉整轮报价刷新 —— 那对做市商比部分成功更糟。

断线保护:`s.ScheduleCancel(ctx, time.Now().Add(30*time.Second))` 到点撤掉账户全部挂单,每轮报价顺手往后续
一次;零值时间 = 清除。`s.CancelAll` 在下一个块边界生效,其间新挂的单同样会被撤。

---

## 推送(WebSocket)

一条连接承载全部订阅,断线按指数退避重连并重订:

```go
st, _ := dexosv1.NewStream(c, cat, dexosv1.StreamOptions{OnState: func(e dexosv1.StateEvent) { /* … */ }})
st.SubscribeOrders(ref, func(u dexosv1.OrdersUpdate) { /* u.Open = 应用之后的全部在挂单 */ })
st.SubscribeFills(ref, cursor, func(f dexosv1.Fill) { /* 从游标起不缺不重 */ })
go st.Run(ctx)
```

每个订阅有就绪信号:`Syncing` 期间本地状态不可信,做市应暂停对应币对;`Live` 之后按序应用即与服务端
一致;`Failed` 是终态(订阅被拒、账本纪元变了)。失效时不悄悄改用轮询,原因都经 `OnState` 报出。
成交与资金流水的断线缺口由 SDK 用 REST 按游标补拉拼接。

---

## 单位

| 概念 | SDK 里 | 线上 |
|---|---|---|
| 价格 | tick(`uint32`) | 十进制字符串,`price = tick / 10^priceDecimals` |
| 数量 | lot(`uint64`) | 十进制字符串,`size = lot / 10^sizeDecimals` |
| 金额、费用、盈亏 | 十进制字符串 | 同左;要原子单位用 `dexosv1.ParseFixed(amount, decimals)` |

委托价与数量在 SDK 里保持整数,与 EIP-712 结构体同一个数量空间:进出时按市场的 `Scale` 精确换算,
不整除就报错,**从不四舍五入** —— 一个被悄悄舍入的价格签出来就是另一张单。

---

## 规范编码与金样对拍

API 钱包代签的签名承诺的是

```
commandHash = keccak256(内核规范编码字节)
```

服务端会用**自己的**编码器重编内层命令再比对。差一个字节 → 签名对不上 → 拒签,
**不会静默出错,但也没有任何线索**。所以 `dexos/codec.go` 必须与 dex-os 的规范编码逐字节一致。

这件事不能靠 review 保证,靠对拍:

```bash
# 在 dex-os 仓库
cargo run -q --example sdk_golden > ../dex-sdk-go/dexos/testdata/golden.json
# 在本仓库
go test ./dexos/
```

金样覆盖空集合、极值、只撤不挂、只挂不撤等边界。两种字节序在同一个 SDK 里并存,是最容易混的地方:

- **规范命令编码** → 小端(内核的紧凑编码)
- **EIP-712 word** → 大端 32 字节右对齐(以太坊 ABI 约定)

---

## 验证

```bash
go test ./...      # 金样对拍、v1 线上形状与规格、会话 / 推送状态机、示例只用 v1
go vet ./...
go run ./examples/quickstart -url … -master-key 0x…   # 对着真节点的集成自检
```

金样只能证明编码正确;签名路径(secp256k1 的 v 位序、地址派生、EIP-712 域分隔符)只有对着真节点才
验得了 —— 那三处任何一处错了,表现都是服务端拒签,静态检查看不出来。所以 quickstart 最后一步是
**反证**:撤销授权后同一把钥匙再下单必须被拒,否则前面全绿也不能说明授权真的起了作用。

---

## 许可

Apache-2.0
