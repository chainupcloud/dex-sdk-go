// Package dexos 是 DEX-OS 永续合约交易所的 Go 客户端。
//
// 它围绕 **API 钱包(代理钱包)** 设计:主账号授权一个派生密钥代为交易,
// 而该密钥动不了钱 —— 提款与转账不在确定性状态机的 agent scope 白名单内。
// 跑策略的机器上只放 API 钱包私钥,被摸进去的最坏后果是被代为交易,
// 不是资产清零。
//
// # 两类写操作
//
// 签名主体不同,这个分界就是账号体系的全部:
//
//	主账号签名   ApproveAgent / RevokeAgent      管理 API 钱包本身
//	API 钱包签名 BatchPlace / BatchCancel /
//	             BatchReplace                    日常交易
//
// # 两个 nonce
//
// 最常见的接入坑。它们是两个独立的计数器:
//
//	ApproveAgent / RevokeAgent  →  Client.NextNonce(ctx, master)
//	agent 代执行                →  Client.AgentNonce(ctx, master, agentAddr)
//
// 混用得到 NonceMismatch(HTTP 409),而错误信息里看不出是哪个计数器错了。
// agent 的 nonce 与授权项生命周期解耦:撤销后重新授权**不会**重置它
// (防旧签名重放),所以重授后必须重读。
//
// # 最短可用路径
//
//	c := dexos.NewClient("http://127.0.0.1:8080", 31337)
//
//	api, _ := dexos.GenerateSigner()          // 私钥不出本进程
//	owner, _ := dexos.NewSigner(masterPrivHex)
//
//	n, _ := c.NextNonce(ctx, master)
//	c.ApproveAgent(ctx, owner, master, api.Address(), time.Now().AddDate(0, 6, 0), n)
//
//	an, _ := c.AgentNonce(ctx, master, api.Address())
//	c.BatchPlace(ctx, api, master, []dexos.BatchOrder{
//	    {Market: 0, Side: dexos.Buy, Price: 117900, Lots: 1, TIF: dexos.PostOnly},
//	}, an)
//
// # 原子换单
//
// 做市刷新报价用 BatchReplace,不要「先撤再挂」:后者是两条共识条目,
// 中间存在一个没有报价的窗口,且第二条被拒时第一条已经生效 ——
// 报价被撤光却没挂上新的,单边裸露。
//
// # 单位
//
// 一切在整数域,SDK 不做换算:价格是 tick、数量是 lot、金额是 quote quantum。
// 金额字段是**字符串**(内核用 i128,JSON number 装不下,转 float 会在大额上
// 悄悄丢精度)。换算系数从 Client.Markets 的 PriceDecimals / SizeDecimals 取。
//
// # 事件流
//
// Client.Subscribe 返回全量事件流,没有订阅报文 —— 重连即完整。
//
// seq 是 Raft 日志序号而非逐事件计数器(同一条命令的多个事件共享它,
// 无事件的日志条目会让它跳号),**不能用来做丢帧检测**。丢帧由服务端显式下发,
// SDK 投递到 Stream.Lost;收到即应重拉快照,本地镜像已不可信。
//
// # 规范编码
//
// agent 代执行的签名承诺的是 keccak256(内核规范编码字节),服务端会用自己的
// 编码器重编再比对。差一个字节 → 401 且没有任何指向根因的信息。codec.go 的
// 正确性由与 Rust 内核的金样对拍守着(见 codec_test.go)。
//
// 详见仓库内 docs/:GETTING-STARTED(接入)、MARKET-MAKING(做市)、
// ARCHITECTURE(内部机制与移植)、API / WEBSOCKET(接口参考)、TROUBLESHOOTING。
package dexos
