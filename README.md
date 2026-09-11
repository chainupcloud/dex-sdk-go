# dex-sdk-go

DEX-OS 永续合约交易所的 Go SDK。围绕 **API 钱包(代理钱包)** 设计 —— 主账号授权一个
派生密钥代为交易,该密钥**动不了钱**。

```bash
go get github.com/chainupcloud/dex-sdk-go
```

## 文档

| 文档 | 什么时候读 |
|---|---|
| [接入指南](docs/GETTING-STARTED.md) | **从这里开始** —— 从零到第一笔成交,每步都给出怎么确认成功 |
| [`examples/onboard`](examples/onboard/main.go) | **从零接入**:生成地址 → 找账户 → 连接 → 交易 |
| [`examples/discover`](examples/discover/main.go) | 只给一个 URL 完成 发现 → 授权 → 下单 |
| [做市接入](docs/MARKET-MAKING.md) | 高频报价:报价循环、原子换单、丢帧处置、错误恢复 |
| [REST 参考](docs/API.md) | 接口字段与拒因表 |
| [WebSocket 参考](docs/WEBSOCKET.md) | 事件流协议、事件目录、时延实测 |
| [技术文档](docs/ARCHITECTURE.md) | 要改 SDK、移植到别的语言、或遇到「签名被拒但看不出为什么」 |
| [故障排查](docs/TROUBLESHOOTING.md) | 按**症状**查,不是按错误码查 |

---

## 账号体系

```
主账号(EVM 地址 + 内核账户号 master)
  │  能做一切:入金、提款、授权/撤销 API 钱包
  └─ 授权 ─→ API 钱包(agent 地址)
              只能交易:下单 / 撤单 / 换单 / 改杠杆 / 条件单 / TWAP
              动不了钱:提款、转账不在 STF 的 agent scope 白名单内
```

**为什么这么分。** 跑策略的机器早晚会被摸进去 —— 可能是依赖投毒、可能是运维失误。
把主账号私钥放在那台机器上,被摸进去等于资产清零;放 API 钱包私钥,最坏后果是被人
代你交易,钱还在账上,撤销授权即止损。

这个边界是**在确定性状态机里强制**的,不是网关的一层检查:`command_actor` 是一份
显式白名单,不在其中的命令一律 `AgentScopeViolation`。新增命令不会自动获得 agent 权限。

三条容易踩的边界:

| 情形 | 行为 |
|---|---|
| 一个 agent 地址绑到别的 master | `AgentAlreadyBound` —— 换地址,重试无用 |
| 同一 master 重复授权同一地址 | **更新有效期**,不是新建 |
| 撤销后重新授权 | agent 的 nonce **不重置**(防旧签名重放),要重读 |

### 两个 nonce,别混

| 操作 | 用谁的 nonce | 怎么读 |
|---|---|---|
| `ApproveAgent` / `RevokeAgent` | **master 账户**的 | `client.NextNonce(ctx, master)` |
| `BatchPlace` / `BatchCancel` / `BatchReplace` | **agent 地址**自己的 | `client.AgentNonce(ctx, master, agentAddr)` |

混用得到的是 `NonceMismatch`,错误信息里看不出是哪个计数器错了 —— 这是最常见的接入坑。

---

## 连接

**只需要一个网关地址。** 其余参数(chainId、EIP-712 域、入金地址、token 映射)
由 `Connect` 向 `/config` 自动发现:

```go
ctx := context.Background()
c, cfg, err := dexos.Connect(ctx, "http://127.0.0.1:8080")
if err != nil { log.Fatal(err) }
fmt.Println("链", cfg.ChainID, "入金地址", cfg.Deposit.SystemGateway)
```

为什么不建议把这些值写死在自己的配置里 —— **填错的后果都很隐蔽**:

| 填错什么 | 你会看到什么 |
|---|---|
| `chainId` | 每一笔签名 401,而盘口、K 线、持仓(只读)**全部正常** —— 看起来像「只有下单坏了」 |
| 入金地址(运营方重新部署过合约) | 钱打到没人监听的合约:链上扣了,账户里没有 |
| codec 版本 | 规范编码变了,agent 代执行的哈希对不上,同样是一片 401 |

最后一项 `Connect` 会自动挡下:它拿节点的 `codecVer` 和 **SDK 自己实现的版本**
(`dexos.CodecVersion`)比对,对不上**当场失败**,而不是让你在生产里对着 401 查半天。
确实要跳过核对(例如只读行情、不签任何东西)用 `ConnectUnchecked`。

仍然可以手动构造(知道自己在做什么时):

```go
c := dexos.NewClient("http://127.0.0.1:8080", 31337)
```

---

## 连接 test 环境

test 环境接以太坊 **Sepolia** 测试网,合约与账本都是真的,只是钱不值钱。

```go
c, cfg, err := dexos.Connect(ctx, "http://<部署机 IP>:17807")
```

| 参数 | 值 | 怎么来的 |
|---|---|---|
| 网关地址 | `http://<部署机 IP>:17807` | 唯一需要别人告诉你的 |
| chainId | `11155111`(Sepolia) | `Connect` 自动发现 |
| 入金系统地址 | 随部署变化 | `cfg.Deposit.SystemGateway` |
| USDC | 随部署变化 | `cfg.Deposit.Tokens[0].ERC20` |

**别把地址抄进代码。** 运营方每次重建环境都会重新部署合约,地址随之改变 ——
抄下来的那份很快就是过期的,而往过期的系统地址打钱是收不回来的。

### 开发者从零接入的四步

```bash
go run ./examples/onboard -url http://<部署机 IP>:17807
```

**1. 地址** —— 用你已有的钱包,或让 SDK 生成一个:

```go
me, _ := dexos.GenerateSigner()          // 本地生成,私钥不出本进程
fmt.Println(me.Address().Hex(), me.PrivateKeyHex())
```

**2. 入金** —— 往 `cfg.Deposit.SystemGateway` 发一笔 USDC 的**裸 `transfer`**。

没有桥合约、没有 `deposit()` 方法。中继观察到之后,内核**自动开户**并绑定你的
EVM 地址 —— 没有注册接口,也没有审批,付钱即开户。

> **这一步 SDK 做不了。** 本 SDK 只管交易所 API,不含 EVM 能力(不签链上交易、
> 不发 ERC20 transfer)。用前端页面、钱包,或 `go-ethereum` 自己发。

**3. 找账户** —— 钱包地址 ≠ 内核账户号,两者不能互推,只能查:

```go
acc, err := c.AccountByAddress(ctx, me.Address().Hex())
if errors.Is(err, dexos.ErrNotRegistered) {
    // 还没入金。这不是错误,是正常起点。
}
fmt.Println("内核账户", acc.AccountID)
```

账户号**按入金先后分配**,不能指定。子账户用 `c.SubaccountsOf(ctx, addr)` 列。

**4. 授权 API 钱包,然后交易**

两条路都行:

- **浏览器**(推荐给人用):`http://<部署机 IP>:17807/app` → 「API 钱包」→ 生成 →
  命名 → 选有效期 → 主钱包签一次。私钥只显示一次,页面同时给出可粘贴的接入代码。
- **代码**(推荐给自动化):见下方「快速开始」,`ApproveAgent` 一次即可。

```go
api, _ := dexos.NewSigner(apiWalletPrivHex)   // 上一步拿到的
n, _ := c.AgentNonce(ctx, acc.AccountID, api.Address())
c.BatchPlace(ctx, api, acc.AccountID, []dexos.BatchOrder{
    {Market: 0, Side: dexos.Buy, Price: 70000, Lots: 1, TIF: dexos.GTC},
}, n)
```

之后主账号私钥就可以收起来了 —— 日常交易只用 API 钱包,它**动不了钱**。

### 拿到账户与 API 钱包

1. 浏览器打开 `http://<部署机 IP>:17807/app`,连接你自己的钱包
2. 领水龙头拿 Sepolia ETH(gas)→ 铸 USDC → 入金

   入金就是往系统地址发一笔**裸 `transfer`**,没有桥合约、没有 deposit 方法。
   中继观察到之后,内核**自动开户**并把你的 EVM 地址绑上去 —— 没有注册接口,
   也没有审批,付钱即开户。

3. 页面里打开「API 钱包」:点「生成」→ 命名 → 选有效期 → 主钱包签一次授权

   私钥**只显示这一次**,页面同时给出可直接粘贴的接入代码(含你的账户号)。
   私钥不上传、不写入本地存储,刷新即丢失 —— 丢了重新生成一个再授权即可。

4. 查自己的内核账户号:

   ```bash
   curl http://<部署机 IP>:17807/account/by-address/0x<你的地址>
   # → {"accountId":7,"collateral":"50000000000",...}
   ```

   入金前查会返回 `address not registered`,那是正常的,不是错误。

### 三个容易踩的

- **没入金就下单** → `HTTP 400 {"error":"unknown trader (deposit first)"}`。
  账户是入金时懒创建的,没有账户就没有可授权、可交易的对象。
- **账户号是按入金先后分配的**,不能指定。低位账户号可能是保险基金、手续费账户、
  做市账户 —— 后者由运营方的做市程序驱动,**它会周期性撤光该账户的全部挂单**。
  拿到别人的账户号交易会看到「单挂上了又消失」,那不是 bug。
- **入金到账时间随终局性配置变化**:`head` 是秒级,`finalized` 要等两个 epoch
  (Sepolia 约 13 分钟)。`cfg.FinalityMode` 会告诉你当前是哪种,
  别把「还没到账」当成「入金失败」。

---

## 快速开始

```go
c := dexos.NewClient("http://127.0.0.1:8080", 31337)

// 1) 本地生成 API 钱包。私钥不出本进程,交易所只拿到地址。
api, _ := dexos.GenerateSigner()
fmt.Println("保存好:", api.PrivateKeyHex())

// 2) 主账号签名授权(用 master 的 nonce)
owner, _ := dexos.NewSigner(masterPrivHex)
n, _ := c.NextNonce(ctx, master)
c.ApproveAgent(ctx, owner, master, api.Address(), time.Now().AddDate(0, 6, 0), n)

// 3) 之后主账号密钥就可以收起来了。日常交易只用 api。
an, _ := c.AgentNonce(ctx, master, api.Address())
c.BatchPlace(ctx, api, master, []dexos.BatchOrder{
    {Market: 0, Side: dexos.Buy, Price: 117900, Lots: 1, TIF: dexos.PostOnly},
}, an)
```

完整生命周期(授权 → 下单 → 换单 → 撤单 → 撤销 → 反证)见
[`examples/quickstart`](examples/quickstart/main.go):

```bash
go run ./examples/quickstart -master-key 0x… -account 3
```

---

## 原子换单

做市商刷新报价的基本原语。**先撤后挂,一次状态机转移完成。**

```go
seqs, _ := c.OrderSeqs(ctx, master, marketID)     // 当前挂单
an, _ := c.AgentNonce(ctx, master, api.Address())
c.BatchReplace(ctx, api, master, marketID, seqs, newQuotes, an)
```

与「先 `BatchCancel` 再 `BatchPlace`」的区别不是省一次往返:

- 两次独立提交是**两条共识条目**,中间存在一个没有报价的窗口
- 第二条若被拒(保证金不足、市场停牌),第一条**已经生效** —— 你的单被撤光却没挂上
  新的,单边裸露

合成一条后,撤与挂落在同一个转移里,不存在中间态可被观测。

「原子」的准确含义是**没有中间窗口**,不是「全成功或全回滚」:逐项仍尽力而为
(撤不掉的跳过、挂不上的跳过),否则一张单的保证金不足会废掉整轮报价刷新 ——
那对做市商比部分成功更糟。

---

## 单位

SDK **不做单位换算**,一切都在整数域:

| 概念 | 单位 | 换算 |
|---|---|---|
| 价格 | tick | `人类价 = tick / 10^priceDecimals` |
| 数量 | lot | `人类量 = lot / 10^sizeDecimals` |
| 金额 | quote quantum | USDC 6 位小数;字段是**字符串**(内核用 i128,JSON number 装不下) |

不替你转是刻意的:一旦引入浮点,`0.1 + 0.2` 那类误差就会进到订单价上。
`priceDecimals` / `sizeDecimals` 从 `Markets()` 拿。

---

## 规范编码与金样对拍

agent 代执行的签名承诺的是

```
commandHash = keccak256(内核规范编码字节)
```

服务端会用**自己的**编码器重编内层命令再比对。差一个字节 → 签名对不上 → 401,
**不会静默出错,但也没有任何线索**。所以 `dexos/codec.go` 必须与
`crates/kernel/src/codec.rs` 逐字节一致。

这件事不能靠 review 保证,靠对拍:

```bash
# 在 dex-os 仓库
cargo run -q --example sdk_golden > ../dex-sdk-go/dexos/testdata/golden.json
# 在本仓库
go test ./dexos/
```

金样覆盖空集合、极值、只撤不挂、只挂不撤等边界。内核改了编码而 SDK 没跟上时,
这组测试会红 —— 它抓到的第一个 bug 就是漏了 `COMMAND_CODEC_VERSION` 那个前缀字节,
比对结构体定义永远发现不了。

两种字节序在同一个 SDK 里并存,是最容易混的地方:

- **规范命令编码** → 小端(内核的紧凑编码)
- **EIP-712 word** → 大端 32 字节右对齐(以太坊 ABI 约定)

---

## 验证

```bash
go test ./dexos/          # 金样对拍 + OrderID 打包
go vet ./...
go run ./examples/quickstart -master-key 0x…   # 对着真节点的集成自检
```

金样只能证明编码正确;签名路径(secp256k1 的 v 位序、地址派生、EIP-712 域分隔符)
只有对着真节点才验得了 —— 那三处任何一处错了,表现都是服务端回 401,静态检查看不出来。
所以 quickstart 最后一步是**反证**:撤销授权后同一把钥匙再下单必须被拒,
否则前面全绿也不能说明授权真的起了作用。

---

## 许可

Apache-2.0
