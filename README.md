# dex-sdk-go

DEX-OS 永续合约交易所的 Go SDK。围绕 **API 钱包(代理钱包)** 设计 —— 主账号授权一个
派生密钥代为交易,该密钥**动不了钱**。

```bash
go get github.com/chainupcloud/dex-sdk-go
```

- REST 接口参考 → [docs/API.md](docs/API.md)
- WebSocket 参考 → [docs/WEBSOCKET.md](docs/WEBSOCKET.md)

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
