package dexosv1

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 带写前日志的原子换单:做市进程崩溃重启后,要能凭本地记录找回每一个可能已经发出的请求。
//
// 普通的 [Session.Replace] 在内存里签名、发送、推进 nonce;进程在「发出之后、记下之前」崩溃,
// 这次请求就只存在于场所。[Session.ReplaceJournaled] 在同一把写锁里多两个边界:
// 签好之后、发送之前调 BeforeSend(把请求身份与原始命令落到调用方的存储),
// 拿到结论之后、推进 nonce 之前调 AfterReceive(记下结论,含错误)。本包不实现存储。

// ErrRequestDeclined 写前日志在**本地还没留下任何记录**时决定不发这次请求(例如风控准入不过)。
// BeforeSend 返回包着它的错误时,请求不发出、会话不锁、nonce 不动。已经落了账再拒绝不能用它。
var ErrRequestDeclined = errors.New("dexosv1: 写前日志在落账前拒绝了这次请求")

// ReplaceIntent 一次原子换单在发出之前就确定的全部内容。CommandHash / SigningHash(在 Request 里)
// 由 SigningAccountID、NowMs、Market.SigningMarketID、Cancels、Orders 按规范编码与 Domain 唯一确定,
// 调用方可以独立重算核对。
type ReplaceIntent struct {
	Request *RequestIdentity
	// SigningAccountID 签进命令的账户号(`/v1/account/nonce` 给的 signingAccountId)。
	SigningAccountID uint32
	// NowMs 签进命令的时刻。
	NowMs  uint64
	Domain dexos.Domain
	Market *Market
	// Cancels 要撤的订单号(v1 的 orderId,即订单序号),顺序即撤单项的 index。
	Cancels []uint64
	// Orders 新单的执行层参数,顺序即下单项的 index。
	Orders []dexos.BatchOrder
}

// ReplaceJournal 原子换单的写前日志。两个回调都在会话的写锁里调用,不要在回调里再用本会话写。
//
//   - BeforeSend 必须先把 intent 提交到本地存储再返回 nil。返回其他错误即锁定会话(本地可能已留下记录,
//     结论要经回执核对);唯一例外是包着 [ErrRequestDeclined] 的错误,见它的说明。
//   - AfterReceive 记下结论:result 非 nil 时 err 为 nil;err 是发送的错误(类型化的 [*Error]、
//     [*RateLimitedError]、[ErrPending] 等,与 [Session.Replace] 返回的相同)。AfterReceive 返回错误即锁定会话
//     (结论只在内存里),nonce 不推进。
type ReplaceJournal interface {
	BeforeSend(ctx context.Context, intent ReplaceIntent) error
	AfterReceive(ctx context.Context, intent ReplaceIntent, result *BatchResult, err error) error
}

// journalError 写前日志自身的失败:不论场所的结论是什么,本地没记下来就只能锁定会话。
type journalError struct{ err error }

func (e *journalError) Error() string { return "dexosv1: 写前日志失败: " + e.err.Error() }
func (e *journalError) Unwrap() error { return e.err }

// ReplaceJournaled 同 [Session.Replace],另在发送前、收到结论后各调一次 journal(见 [ReplaceJournal])。
// nonce 只在请求进了日志且 AfterReceive 成功之后按 [Session.Replace] 的规则推进。
func (s *Session) ReplaceJournaled(ctx context.Context, m *Market, cancels []uint64, orders []OrderRequest, journal ReplaceJournal) (*BatchResult, error) {
	if journal == nil {
		return nil, errors.New("dexosv1: 缺写前日志")
	}
	if err := sameMarket(m, orders); err != nil {
		return nil, err
	}
	ids := make([]dexos.OrderID, len(cancels))
	wireCancels := make([]string, len(cancels))
	for i, seq := range cancels {
		ids[i] = dexos.NewOrderID(m.SigningMarketID, seq)
		wireCancels[i] = strconv.FormatUint(seq, 10)
	}
	items := make([]wireOrderReq, len(orders))
	batch := make([]dexos.BatchOrder, len(orders))
	for i, o := range orders {
		w, err := o.wire(nil)
		if err != nil {
			return nil, err
		}
		items[i], batch[i] = w, o.batchOrder()
	}
	var out *BatchResult
	err := s.write(ctx, func(nonce uint64) (*RequestIdentity, error) {
		now := nowMs()
		cmd := dexos.EncodeBatchReplace(s.signingID, now, ids, batch)
		agent := s.Agent.Address()
		digest := s.domain.AgentExecHash(cmd, nonce)
		id := newIdentity(agent, "agent:"+agent.Hex(), nonce, digest, dexos.Keccak256(cmd))
		intent := ReplaceIntent{Request: id, SigningAccountID: s.signingID, NowMs: now, Domain: s.domain, Market: m,
			Cancels: append([]uint64{}, cancels...), Orders: append([]dexos.BatchOrder{}, batch...)}
		if err := journal.BeforeSend(ctx, intent); err != nil {
			if errors.Is(err, ErrRequestDeclined) {
				return nil, fmt.Errorf("%w: %w", errNotSent, err)
			}
			return id, &journalError{err: err}
		}
		// exec 按同一 nonce 与命令重算出同一个请求身份(确定性签名),发出去的就是日志里那一条。
		_, status, raw, sendErr := s.exec(ctx, nonce, cmd, "/v1/orders/replace", func(env *envelope) any {
			env.NowMs = now
			return wireReplaceReq{envelope: env, Symbol: m.Symbol, Cancels: wireCancels, Orders: items}
		})
		if sendErr == nil {
			var r Receipt
			if r, sendErr = receipt(status, raw, id); sendErr == nil {
				out, sendErr = batchResult(m, r, raw, len(orders), len(cancels))
			}
		}
		if sendErr != nil {
			out = nil
		}
		if err := journal.AfterReceive(ctx, intent, out, sendErr); err != nil {
			out = nil
			if sendErr != nil {
				return id, &journalError{err: fmt.Errorf("%w(发送结论: %v)", err, sendErr)}
			}
			return id, &journalError{err: err}
		}
		return id, sendErr
	})
	return out, err
}
