# 做市接入的完整性边界

源码核查基线：dex-os `1ff707ba523f356901bbd1a7ddf30a96e61d2db8`。软件测试使用进程内 HTTP/WS，不是已执行真实交易的证据。

## 连接与身份

使用 `Connect` 每次发现 chainId、完整 EIP-712 域、合约地址及版本；缺失/非法立即报错。`NewClientFromConfig` 现返回 `(*Client, error)`，调用方必须检查错误；手工构造 `Domain` 时需显式填 Name/Version。EIP-712 编码协议和 codec 没有改变。

`AccountByAddress` 只把 404 解释为未登记；`AgentGrants` 校验返回代理、每条账户/有效期及代理共享 nonce。代理不能由主账户地址代替。主钱包私钥不得传给策略；授权、入金由 owner 在策略外完成。

`Market.PriceDecimals` / `SizeDecimals` 现为 `*int`：无元数据市场保留在目录中，精度为 nil；使用某个市场前须核对这两项，缺失时明确报告市场 ID 并停止该市场转换。不能把未知精度当 0，也不因无关市场缺元数据丢掉整个目录。

`Market.Kind` 保留服务端明确的 `perp` / `spot`；源码出处为 dex-os
`80eb5de719172f3c9fc454babd4d11c0d1a04a0e` 的 `api.rs::markets`。
新版目录可同时包含永续与现货，禁止靠 symbol 或已有精度猜它是哪一种。
缺失/null 保持空串，未知字符串不改成已知类型；整份目录仍可读，由策略逐行拒绝不支持的市场。
这只是只读目录字段透传，不改变签名/codec，不宣称新增现货执行与账务能力。
验证：`TestMarketsPreserveExplicitKindWithoutGuessing` 在加字段前 rc=1（“SDK 丢失或猜测市场类型”），
`TestMarketsRejectWrongKindType` 同样先红；加字段后定向 race 绿，缺失并不被填为 perp。

## 批量订单与未知结果

`BatchPlace` / `BatchCancel` / `BatchReplace` 以折叠回执的逐项结果 `WriteReceipt.Items`(dex-os #4)判结局：身份是 `(Kind, InputIndex)`,撤单与下单各自从 0 编号，对应调用方两个输入切片的下标；事件与聚合计数只作交叉核对。

- 每个输入恰好一项、账户/市场/撤单目标与输入一致、被接受的下单有同号且价量一致的 `OrderAccepted`、被撤的单有 `OrderCanceled`、事件里没有多出的接受、聚合与逐项一致 —— 全部成立才采信；
- 采信且全部成功 → nil；采信但有项被拒 → `*BatchOutcomeError`(终局：命令已执行、nonce 已消耗，`Rejected` 带出被拒项与拒因，其余项已生效，不要重发)；
- 缺 `items` 或任何一条不成立 → `ErrIncompleteBatch`(证据不可信)。不退回旧的按事件推断。

`Session` 在会话内串行写入,**结论未知**的写错误(传输失败、`ErrExecutionPending`、`NonceMismatch`、`ErrIncompleteBatch`)锁定后续写入及 `Resync`(`ErrSessionBlocked`),保留原 nonce 与事件供核查;终局结论不锁：状态机的业务拒绝(`*RejectedError`)——内核在 nonce 写入之后才判业务且 apply 不回滚,本地计数器 ++;发生在 nonce 之前的拒绝(scope 越权、未授权)计数器不动;逐项结局齐全的部分成功(`*BatchOutcomeError`)计数器 ++。锁定并不声称明确拒绝也是未知执行，只表示调用方尚未核对上次结果。不得通过创建新 Session、重读 nonce 或换新 nonce 盲重发绕过；按原请求身份查 `Client.ReceiptOf`。

这是进程内约束；**调用方仍须持久化未决请求并按 API 代理地址持有跨进程独占租约**。SDK 提供原回执查询(`ReceiptOf`),但不提供持久 nonce 协调或自动重启恢复。

### 单请求换单的证据接口

对照 dex-os `9d942f24aeb48212aae138d31febe66f0e240d32` 的公开 `/replace` 响应，新增
`Client.BatchReplaceWithReceipt` / `Session.ReplaceWithReceipt`。本轮对齐 dex-os `3f02f2eb2cb5affa3df73a7bcd12cd30e7e8e7f5`
的双环执行契约，换单仍只发送一次 `/replace?wait=fold` 请求。
旧 `BatchReplace` / `Replace` 签名不变，委托同一实现并返回事件；不是先撤再下两个请求。
仅下单时空撤单列表序列化为 `[]`，而不是 Rust Vec 不接受的 `null`。

`BatchSubmission` 同时返回：

- `Request`：本地实际用于签名的代理、账户、nonce、nowMs、codec 版本、域、命令哈希和签名摘要。
  SDK 不把私钥或请求签名复制进这份身份。它不是服务端已接受证明，也不是跨部署唯一键。
- `Receipt`：实际完整解析的响应，包括 seq/sub、folded、聚合统计、拒因和原始事件。
  非 nil 不等于业务成功，必须同时检查 error；坏 JSON/类型错/无响应时为 nil，不暴露半解码内容。
- `Events()`：兼容旧消费者的事件视图；错误发生时仍可能有已成功的部分事件，不能丢弃。

可选字段保持 nil 与合法0/false的区别。seq/sub 是请求所在日志条目及子项，不是唯一成交 ID。
`folded=false` 返回 `ErrExecutionPending` 并经 Session 锁定；同次请求虽已带 `wait=fold`，
预算内仍可能等不到结果。不会再发第二次请求、自动轮询或重发。
聚合统计只能与逐项结果一致，不能代替它；已提供的统计与逐项/事件矛盾返回 `ErrIncompleteBatch`。
batchStatus 值集为 `ok` / `none` / `partial`(dex-os api.rs `annotate_batch_outcome`)。
缺 sub/folded/统计的回执不给缺失字段补零；缺 `items` 的批量回执不再被接受。

即使返回 error，也要保留本次已经取得的 Request/Receipt。会话已阻断、context 预先取消或授权
已到期时，本次调用不会建立新 Request；这不证明上一请求未执行，不应被当作恢复结果。

**仅调用 WithReceipt 仍不是发送前日志。** 身份随调用结果返回；进程在返回前崩溃仍可能失去它。
需要发送前持久化的调用方应使用下述 `ReplaceWithJournal` 并提供真实存储。SDK 没有跨进程 nonce 协调或自动重启恢复能力，
不能据此解除账户独占和 live 窗口前置。EIP-712、codec、签名算法均未改变。

### 发送前原请求日志

`Session.ReplaceWithJournal(ctx, market, cancels, orders, journal)` 与其他写操作共用同一 Session nonce 锁：

1. 固定无签名 `ReplaceRequest`（Identity、Market、Cancels、Orders），向 `BeforeSend` 交独立副本；没有网络写入。
2. 调用方先在持久事务中记录原请求、每个输入位置对应的本地订单身份，并核验代理独占/账号围栏/风险准入。
3. 回调成功后重新核验授权期限与会话身份，SDK 使用同一原命令/nowMs/nonce 签名，仅发送一次 `/replace?wait=fold`。
4. `AfterReceive` 收到独立的结果副本与错误，包括 pending、部分成功、坏响应或断线已取得的证据。
5. 只有场所完整成功且结果记录成功后推进本 Session nonce；任一日志错误（含提交结果未知）均锁定会话。BeforeSend 失败不发送，AfterReceive 失败不重发。

回调不得重入该 Session。传入 slice、日志回调中的 slice、返回证据副本均不能改写已固定的实际命令。
记录中不含私钥或可重放签名；不要把业务 ClientID 当成服务端已有的幂等键。
这些接入点不是 SDK 内置数据库，不能据此声称已经解决跨进程租约、部署纪元隔离或服务端持久回执查询。
重启遇到 prepared 也不能视为“未发送”：进程可能在发出后、记录响应前退出，只能按原身份取得权威结论。

验证回执（2026-09-16）：实现及修复目标 `a1ad25ce72fcf0d3e2c8c0def99f012225e68d17`，
base `444ca6a`。`GOWORK=off go test -race -count=1 -timeout=90s ./...` 与 `go vet ./...` 全绿。
独立 checker s1940、实际 kimi-k3：初审发现 batchStatus 被误写为 all；以固定服务端 ok
夹具复现红后修复，定向复核结论 0 blocking。不是实盘验收。

新增风险检查的真实红证据与复跑位置：

| 缺陷/变异 | 定向测试 | 实际红因 |
|---|---|---|
| 只保留事件，丢请求与回执元数据 | TestReplaceReceiptPreservesRequestAndMetadata | SDK 丢失原请求身份或回执元数据 |
| 忽略显式 folded=false | TestExplicitPendingNeverSucceedsEvenWithEvents | 明确 pending 被当成执行成功 |
| 错误放行值 all，不接受服务端 ok | TestReplaceReceiptPreservesRequestAndMetadata | ErrIncompleteBatch: batchStatus=ok |
| 跳过聚合核对(2026-09-24 并入 checkBatchAggregates) | TestReceiptAggregateClaimsCannotOverrideEvents | 聚合声明覆盖逐项证据或矛盾回执被接受 |
| 不经 decoded 检查直接暴露 WriteReceipt | TestReceiptFailurePreservesAvailableEvidenceAndBlocks | 失败时丢失/伪造请求或回执证据 |

各项为可编译代码缺陷的实际 rc=1，不把坏输入测试或编译失败当变异成功；末两项注入后逐字还原，
`git diff --exit-code -- dexos/trade.go` 为0并整包复绿。签名/codec 金样沿用原独立 Rust 来源。

## WebSocket

`Subscribe` 同步建立连接。坏帧、缺 envelope 字段、Lagged 或断线会终止流并报告 `ErrStreamGap`(seq 回退不终止:网关不承诺 seq 单调,系统命令事件与订单事件派生路径不同)；取消 context 关闭静默连接。三个通道均关闭，消费方必须检查通道 ok 或在 Err 后结束。

不再自动重连。只有从可靠历史补齐逐笔事件、对账完成后才能建立新连接；当前 `/risk` 或 `/book` 快照不能证明成交历史完整。seq 可以重复或跳号，禁止作为唯一成交 ID。

## 执行历史读接口与原请求回执(dex-os #1/#2/#3/#5)

对照 dex-os `5ee4759`(`crates/gateway/src/api.rs` 的 fills / account_ledger / events_backfill / equity_snapshot / request_receipt)。

- `Fills` / `Ledger` / `Events` 自动翻页到上界：第一页给出 `epoch` + `upper`,续页原样带回(服务端缺一即 400)。结果带回 `Epoch` / `Upper`:「没有更多」只表示到 `Upper` 为止。从已存游标续拉必须同时给出游标所属纪元(`After` + `Epoch`),SDK 先以该纪元探一页取当前上界；缺纪元在本地拒绝、不发请求。页间纪元或上界漂移即报错，不把两段历史拼成一份。
- 纪元不符(409)→ `ErrHistoryEpochMismatch`:账本换纪元，旧游标作废。与缺口相交(503)→ `*HistoryUnavailableError`(带缺口区间，仍可 `errors.As` 取 `*APIError`)—— 证明不了有没有，不是空结果。
- `Fill.TS` 为 `*int64`:null = 服务端不知道成交时刻(之前没有过区块),不是 1970 年。
- `Equity(account, epoch)` 同水位快照，金额保持最小单位字符串；404 → `ErrNotRegistered`,500(`ledger_mismatch` / `valuation_mismatch`)与 503 一律是错误，不给快照。
- `AgentRequestIdentity.RequestID()` = keccak256(代理地址 ‖ SigningHash),与 dex-os `SignedRequest::request_id` 同一定义(`testdata/request_id_golden.json` 是真节点上服务端推导的金样)。`ReceiptOf(identity)` 按 agent scope + 原 nonce 查询，并核对回执声称的签名者与 nonce。六种结论(executed / rejected / pending / not_found / conflict / history_unavailable)按状态返回；**只有无缺口的副本才会给 not_found**,history_unavailable 不能当成「没执行」。

真节点验证:`examples/contracts` 对本机全新纪元 dex-node 走一遍(两个新账户挂单吃单、逐项全成功与部分成功、原请求回执、not_found、成交翻页与续拉、纪元不符、Σ流水 == 权益快照余额、事件补拉),全部 PASS。同一节点上旧版 `Fills`(每页 1 笔)第二页即 `HTTP 400: resume requires epoch and upper from the first page`。

新增风险断言的真实缺陷红(2026-09-24,注入后逐字节还原并整包复绿):

| 缺陷形态 | 定向测试 | 实际红因 |
|---|---|---|
| 续页不带回 epoch/upper(原始缺陷) | TestFillsCarryEpochAndUpperAcrossPages | HTTP 400: resume requires epoch and upper from the first page |
| 逐项齐全的部分成功仍锁会话 | TestPartialBatchWithCompleteItemsIsFinalAndAdvancesNonce | 结局已确定,不该锁会话 |
| 缺 items 退回按事件判成功 | TestUntrustworthyItemsBlockSession/missing_items | 证据不可信应当 ErrIncompleteBatch 且锁会话,实得 nil |
| 不核事件里多出的接受 | TestUntrustworthyItemsBlockSession/unexplained_accept_event | 同上,实得 *BatchOutcomeError |
| 撤单目标不核 | TestUntrustworthyItemsBlockSession/cancel_target_mismatch | 同上,实得 nil |
| ReceiptOf 不核签名者 | TestReceiptOfChecksIdentity | 签名者不符的回执不能当成自己的 |
| 缺口当成 not_found | TestReceiptStatusesAreAnswersNotErrors/history_unavailable | 状态不对(not_found) |
| 请求身份只哈希摘要 | TestRequestIDMatchesServerDerivation | 本地请求身份与服务端不一致 |

## 服务端前置

dex-os #1–#5 已由 dex-os PR #6 合入 main(2026-09-24):

- [成交身份、订单关联、实际费用与完整分页 #1](https://github.com/chainupcloud/dex-os/issues/1) —— `Session.Fills` + `Event.ID()`
- [历史补拉及投影完整性 #2](https://github.com/chainupcloud/dex-os/issues/2) —— `Client.Events` / `Session.Fills`
- [原请求回执恢复 #3](https://github.com/chainupcloud/dex-os/issues/3) —— `Client.ReceiptOf`
- [批次逐项结果 #4](https://github.com/chainupcloud/dex-os/issues/4) —— `WriteReceipt.Items` / `*BatchOutcomeError`
- [资金流水与权益对账 #5](https://github.com/chainupcloud/dex-os/issues/5) —— `Client.Ledger` / `Client.Equity`

升级前就存在的账本带一段永久缺口(dex-os `docs/deploy.md` §6c′):那样的实例上 `/equity` 恒 503、未落地的请求恒 history_unavailable,做市对接须用全新纪元的实例。

HTTP/head 实例不提供生产安全与最终确认保证。`ScheduleCancel` 是账户级全撤，使用前必须核实专用账户及其他策略占用，取得 owner 的 live 窗口与硬顶。
