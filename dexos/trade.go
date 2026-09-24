package dexos

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"
)

var (
	// ErrAgentNotFound 该地址不在这个 master 的授权列表里(没授权,或授权给了别人)。
	ErrAgentNotFound = errors.New("dexos: agent 未在该 master 名下授权")
	// ErrAgentExpired 授权还在,但已过有效期。重新 ApproveAgent 即可续期。
	ErrAgentExpired = errors.New("dexos: agent 授权已过期")
)

// 用 API 钱包代主账号交易。三个写入口共用同一套机制:
//
//	规范编码内层命令 → keccak256 → 塞进 AgentExec 的 EIP-712 结构 → agent 签名
//
// 服务端会用**自己的**编码器重编内层命令再比对哈希,所以 codec.go 里的编码
// 必须与内核逐字节一致(golden 对拍守着这条)。
//
// nowMs 由**客户端**提供并进入签名。这不是随手加的字段:内核不读时钟,
// 一切非确定输入都由命令携带。它同时是订单的时间基准,填 0 会让带时效的
// 语义(GTT 等)失去参照,所以这里默认取本机时间。

// 每个写端点都带 `?wait=fold`:不带它,网关的同步回执只说「已定序」,events 恒为空、
// 业务拒绝也看不见 —— checkBatchOutcome 会把每一笔正常下单都判成 ErrIncompleteBatch。
// 此前只有 /replace 带了它。

// BatchPlace 批量下单(逐张独立判定,一张失败不拖累其余)。
//
// 单张下单就是 len(orders)==1 —— 没有单独的下单接口,因为批量的开销就是一次
// 共识轮,拆开只会多花往返。
func (c *Client) BatchPlace(
	ctx context.Context,
	agent *Signer,
	account uint32,
	orders []BatchOrder,
	nonce uint64,
) ([]EventEnvelope, error) {
	nowMs := uint64(time.Now().UnixMilli())
	cmd := EncodeBatchPlace(account, nowMs, orders)
	sig, err := agent.SignHashHex(c.Domain.AgentExecHash(cmd, nonce))
	if err != nil {
		return nil, err
	}
	var out writeResp
	err = c.do(ctx, http.MethodPost, "/batch?wait=fold", map[string]any{
		"agent":     agent.Address().Hex(),
		"account":   account,
		"orders":    ordersJSON(orders),
		"nowMs":     nowMs,
		"nonce":     nonce,
		"signature": sig,
	}, &out)
	if err == nil {
		err = checkBatchOutcome(out.WriteReceipt, account, orders, nil)
	}
	return out.Events, err
}

// BatchCancel 批量撤单(逐张尽力而为:不存在/已成交/非本人的跳过)。
//
// seqs 是订单序号 —— 用 Orders() 返回的 Order 字段经 OrderID(x).Seq() 取,
// 或直接记录下单时事件里的 order。market 与 seq 一起构成 OrderId。
func (c *Client) BatchCancel(
	ctx context.Context,
	agent *Signer,
	account uint32,
	market uint16,
	seqs []uint64,
	nonce uint64,
) ([]EventEnvelope, error) {
	ids := make([]OrderID, len(seqs))
	for i, s := range seqs {
		ids[i] = NewOrderID(market, s)
	}
	cmd := EncodeBatchCancel(account, ids)
	sig, err := agent.SignHashHex(c.Domain.AgentExecHash(cmd, nonce))
	if err != nil {
		return nil, err
	}
	var out writeResp
	err = c.do(ctx, http.MethodPost, "/cancel?wait=fold", map[string]any{
		"agent":     agent.Address().Hex(),
		"account":   account,
		"orders":    seqs,
		"market":    market,
		"nonce":     nonce,
		"signature": sig,
	}, &out)
	if err == nil {
		err = checkBatchOutcome(out.WriteReceipt, account, nil, ids)
	}
	return out.Events, err
}

// BatchReplace **原子换单:先撤后挂**。做市商刷新报价的基本原语。
//
// 与「先调 BatchCancel 再调 BatchPlace」的区别不是省一次往返,而是消掉两者之间
// 那个**没有报价**的窗口:两次独立提交是两条共识条目,若第二条被拒(保证金不足、
// 市场停牌),第一条已经生效 —— 你会发现自己的单被撤光却没挂上新的,单边裸露。
// 合成一条后撤与挂落在同一个状态机转移里,不存在中间态可被观测。
//
// 「原子」的准确含义是**没有中间窗口**,不是「全成功或全回滚」:逐项仍尽力而为,
// 否则一张单的保证金不足会废掉整轮报价刷新 —— 那比部分成功更糟。
func (c *Client) BatchReplace(
	ctx context.Context,
	agent *Signer,
	account uint32,
	market uint16,
	cancelSeqs []uint64,
	orders []BatchOrder,
	nonce uint64,
) ([]EventEnvelope, error) {
	result, err := c.BatchReplaceWithReceipt(ctx, agent, account, market, cancelSeqs, orders, nonce)
	return result.Events(), err
}

// BatchReplaceWithReceipt 与 BatchReplace 使用同一请求，额外保留签名身份和回执。
// 它不查询/重试旧请求，也不提供发送前持久化；即使返回 error 也应保留结果供核查。
func (c *Client) BatchReplaceWithReceipt(ctx context.Context, agent *Signer, account uint32, market uint16, cancelSeqs []uint64, orders []BatchOrder, nonce uint64) (BatchSubmission, error) {
	return c.batchReplaceWithJournal(ctx, agent, account, market, cancelSeqs, orders, nonce, nil)
}

func (c *Client) batchReplaceWithJournal(ctx context.Context, agent *Signer, account uint32, market uint16, cancelSeqs []uint64, orders []BatchOrder, nonce uint64, journal ReplaceJournal) (BatchSubmission, error) {
	var result BatchSubmission
	if c == nil || c.HTTP == nil || agent == nil {
		return result, errors.New("dexos: 缺客户端或代理签名者")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// 固定命令后不再读取调用方的 slice；日志回调也只拿到另一份副本。
	orders = append([]BatchOrder(nil), orders...)
	nowMs := uint64(time.Now().UnixMilli())
	ids := make([]OrderID, len(cancelSeqs))
	// Rust Vec 接收空数组，不接收 null；仅下单时也须保留同一 /replace 入口。
	seqs := make([]uint64, len(cancelSeqs))
	copy(seqs, cancelSeqs)
	for i, s := range cancelSeqs {
		if s >= 1<<48 {
			return result, errors.New("dexos: 撤单序号超过48位，不能截断")
		}
		ids[i] = NewOrderID(market, s)
	}
	cmd := EncodeBatchReplace(account, nowMs, ids, orders)
	domain := c.Domain
	endpoint := c.BaseURL
	digest := domain.AgentExecHash(cmd, nonce)
	result.Request = &AgentRequestIdentity{
		Agent: agent.Address(), Account: account, Nonce: nonce, NowMs: nowMs,
		CodecVersion: CodecVersion, Domain: domain,
		CommandHash: "0x" + hex.EncodeToString(Keccak256(cmd)), SigningHash: "0x" + hex.EncodeToString(digest),
	}
	if journal != nil {
		request := ReplaceRequest{Identity: *result.Request, Market: market,
			Cancels: append([]uint64{}, seqs...), Orders: append([]BatchOrder{}, orders...)}
		if err := journal.BeforeSend(ctx, request); err != nil {
			return result, journalError{fmt.Errorf("dexos: 原请求记账失败，未发送: %w", err)}
		}
		if c.Domain != domain || c.BaseURL != endpoint {
			return result, errors.New("dexos: 请求准备后场所或签名域变化，未发送")
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
	}
	sig, err := agent.SignHashHex(digest)
	if err != nil {
		return result, err
	}
	var out writeResp
	// dex-os 3f02f2e 的双环同步 ack 没有业务事件；等待仍属于同一次写，不另发或重试。
	err = c.do(ctx, http.MethodPost, "/replace?wait=fold", map[string]any{
		"agent":     agent.Address().Hex(),
		"account":   account,
		"cancels":   seqs,
		"market":    market,
		"orders":    ordersJSON(orders),
		"nowMs":     nowMs,
		"nonce":     nonce,
		"signature": sig,
	}, &out)
	result.Receipt = out.evidence()
	if err == nil {
		err = checkBatchOutcome(out.WriteReceipt, account, orders, ids)
	}
	if journal != nil {
		copied, copyErr := copySubmission(result)
		if copyErr != nil {
			return result, errors.Join(err, journalError{fmt.Errorf("dexos: 回执证据复制失败: %w", copyErr)})
		}
		if journalErr := journal.AfterReceive(ctx, copied, err); journalErr != nil {
			err = errors.Join(err, journalError{fmt.Errorf("dexos: 执行结果记账失败: %w", journalErr)})
		}
	}
	return result, err
}

// ordersJSON 网关的 JSON 字段名与内核的规范编码是两套东西:
// 签名走编码,传输走 JSON。两者的字段**语义**必须一致,否则服务端按 JSON
// 重建的命令与你签的那条不是同一条,哈希对不上 → 401。
func ordersJSON(orders []BatchOrder) []map[string]any {
	out := make([]map[string]any, len(orders))
	for i, o := range orders {
		out[i] = map[string]any{
			"market":     o.Market,
			"side":       o.Side.String(),
			"price":      o.Price,
			"lots":       o.Lots,
			"tif":        uint8(o.TIF),
			"reduceOnly": o.ReduceOnly,
		}
	}
	return out
}
