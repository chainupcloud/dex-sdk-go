package dexos

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// ErrIncompleteBatch 批量回执的逐项证据缺失或自相矛盾:结局不可信,会话锁定待人工 / 按原请求身份核查。
// 返回的回执与事件必须保留供核查,不能按价量猜失败项。
var ErrIncompleteBatch = errors.New("dexos: 批次逐项结果缺失或与输入/事件矛盾")

// BatchItem 一项批量输入的结局(dex-os #4;crates/raft-node/src/ring/publisher.rs batch_item)。
//
// 身份是 (Kind, InputIndex):撤单与下单**各自**从 0 编号,对应调用方传入的两个切片的下标。
// OrderID 是订单序号的十进制串:下单项 = 进了簿的新单号(被拒的没有),撤单项 = 要撤的那张单。
type BatchItem struct {
	InputIndex   uint32  `json:"inputIndex"`
	Kind         string  `json:"kind"`
	AccountID    uint32  `json:"accountId"`
	Market       uint16  `json:"market"`
	RecordSub    *uint16 `json:"recordSub"`
	OrderID      *string `json:"orderId,omitempty"`
	State        string  `json:"state"`
	Reason       *string `json:"reason,omitempty"`
	FilledLots   *uint64 `json:"filledLots,omitempty"`
	Resting      *bool   `json:"resting,omitempty"`
	CanceledLots *uint64 `json:"canceledLots,omitempty"`
}

// UnmarshalJSON 坐标、归属与结局缺一即解析失败:缺字段的项不能被零值冒充成「第 0 项 / 账户 0」。
func (b *BatchItem) UnmarshalJSON(raw []byte) error {
	type plain BatchItem
	var wire struct {
		plain
		InputIndex *uint32 `json:"inputIndex"`
		Kind       *string `json:"kind"`
		AccountID  *uint32 `json:"accountId"`
		Market     *uint16 `json:"market"`
		State      *string `json:"state"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	if wire.InputIndex == nil || wire.Kind == nil || wire.AccountID == nil || wire.Market == nil || wire.State == nil {
		return errors.New("dexos: 逐项结果缺 inputIndex/kind/accountId/market/state")
	}
	*b = BatchItem(wire.plain)
	b.InputIndex, b.Kind, b.AccountID, b.Market, b.State = *wire.InputIndex, *wire.Kind, *wire.AccountID, *wire.Market, *wire.State
	return nil
}

// BatchOutcomeError 批次已执行(nonce 已消耗),逐项结局齐全且可信,其中有项被拒。
//
// 它是**终局**结论,和 *RejectedError 一样不锁会话:被拒的是哪几项、为什么,全在 Rejected 里;
// 其余项已按回执生效(完整逐项结果见 WriteReceipt.Items)。不要重发被接受的那部分。
type BatchOutcomeError struct {
	Rejected []BatchItem
}

func (e *BatchOutcomeError) Error() string {
	return fmt.Sprintf("dexos: 批次部分成功,%d 项被拒(首项 %s#%d: %s)", len(e.Rejected), e.Rejected[0].Kind, e.Rejected[0].InputIndex, deref(e.Rejected[0].Reason))
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// checkBatchOutcome 以逐项结果判批次结局,并用事件与聚合交叉核对。
//
//   - 每个输入恰好一项,坐标、账户、市场、撤单目标与输入逐一对上;
//   - 被接受的下单必须在事件里有同号的 OrderAccepted 且价量与输入一致,被撤的单必须有 OrderCanceled;
//     事件里不能多出逐项结果解释不了的接受;
//   - 聚合计数只能与逐项一致,不能替代它。
//
// 任何一条不成立 → ErrIncompleteBatch(证据不可信,锁会话);全部成立且有被拒项 → *BatchOutcomeError(终局)。
func checkBatchOutcome(receipt WriteReceipt, account uint32, orders []BatchOrder, cancels []OrderID) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrIncompleteBatch}, args...)...)
	}
	if receipt.Items == nil {
		return bad("折叠后的批量回执缺 items")
	}
	places := make([]*BatchItem, len(orders))
	cancelItems := make([]*BatchItem, len(cancels))
	canceledTargets := make(map[OrderID]bool)
	var rejected []BatchItem
	for i := range receipt.Items {
		it := &receipt.Items[i]
		var slot []*BatchItem
		var market uint16
		switch it.Kind {
		case "place":
			slot = places
			if int(it.InputIndex) < len(orders) {
				market = orders[it.InputIndex].Market
			}
		case "cancel":
			slot = cancelItems
			if int(it.InputIndex) < len(cancels) {
				market = cancels[it.InputIndex].Market()
			}
		default:
			return bad("未知的项类型 %q", it.Kind)
		}
		if int(it.InputIndex) >= len(slot) || slot[it.InputIndex] != nil {
			return bad("%s#%d 越界或重复", it.Kind, it.InputIndex)
		}
		if it.AccountID != account || it.Market != market {
			return bad("%s#%d 的账户/市场与输入不符", it.Kind, it.InputIndex)
		}
		slot[it.InputIndex] = it
		switch {
		case it.State == "rejected":
			if it.Reason == nil || *it.Reason == "" {
				return bad("%s#%d 被拒却没有拒因", it.Kind, it.InputIndex)
			}
			rejected = append(rejected, *it)
		case it.Kind == "place" && it.State == "accepted":
			if it.OrderID == nil || it.FilledLots == nil || it.Resting == nil {
				return bad("place#%d 被接受却缺 orderId/filledLots/resting", it.InputIndex)
			}
		case it.Kind == "cancel" && it.State == "canceled":
			// 同一张单只能被撤一次:两项都报撤掉是自相矛盾
			if canceledTargets[cancels[it.InputIndex]] {
				return bad("cancel#%d 的目标已被本批另一项撤掉", it.InputIndex)
			}
			canceledTargets[cancels[it.InputIndex]] = true
		default:
			return bad("%s#%d 的结局 %q 与类型不符", it.Kind, it.InputIndex, it.State)
		}
		if it.Kind == "cancel" && (it.OrderID == nil || *it.OrderID != strconv.FormatUint(cancels[it.InputIndex].Seq(), 10)) {
			return bad("cancel#%d 的目标单号与输入不符", it.InputIndex)
		}
	}
	for i, it := range places {
		if it == nil {
			return bad("place#%d 没有逐项结果", i)
		}
	}
	for i, it := range cancelItems {
		if it == nil {
			return bad("cancel#%d 没有逐项结果", i)
		}
	}
	if err := checkItemEvents(receipt.Events, account, orders, cancels, places, cancelItems); err != nil {
		return err
	}
	if err := checkBatchAggregates(receipt, places, cancelItems, len(rejected)); err != nil {
		return err
	}
	if len(rejected) > 0 {
		return &BatchOutcomeError{Rejected: rejected}
	}
	return nil
}

// checkItemEvents 事件是逐项结果的独立旁证:被接受 / 被撤的每一项都要有对应事件,事件也不能多出接受。
func checkItemEvents(events []EventEnvelope, account uint32, orders []BatchOrder, cancels []OrderID, places, cancelItems []*BatchItem) error {
	type orderEvent struct {
		Account *uint32 `json:"account"`
		Market  *uint16 `json:"market"`
		Seq     *uint64 `json:"orderSeq"`
		Price   *uint32 `json:"price"`
		Lots    *uint64 `json:"lots"`
		Filled  *uint64 `json:"filledLots"`
		Resting *bool   `json:"resting"`
	}
	accepted := make(map[OrderID]orderEvent)
	canceled := make(map[OrderID]bool)
	for _, ev := range events {
		switch ev.Kind {
		case "OrderAccepted":
			var a orderEvent
			if json.Unmarshal(ev.Data, &a) != nil || a.Account == nil || a.Market == nil || a.Seq == nil || *a.Seq > 1<<48-1 || a.Price == nil || a.Lots == nil || *a.Lots == 0 || a.Filled == nil || a.Resting == nil || *a.Account != account || *a.Filled > *a.Lots {
				return fmt.Errorf("%w: OrderAccepted 字段缺失或身份不符", ErrIncompleteBatch)
			}
			id := NewOrderID(*a.Market, *a.Seq)
			if _, dup := accepted[id]; dup {
				return fmt.Errorf("%w: 重复订单身份", ErrIncompleteBatch)
			}
			accepted[id] = a
		case "OrderCanceled":
			var a orderEvent
			if json.Unmarshal(ev.Data, &a) != nil || a.Account == nil || a.Market == nil || a.Seq == nil || *a.Seq > 1<<48-1 || *a.Account != account {
				return fmt.Errorf("%w: OrderCanceled 字段缺失或身份不符", ErrIncompleteBatch)
			}
			canceled[NewOrderID(*a.Market, *a.Seq)] = true
		}
	}
	explained := 0
	for i, it := range places {
		if it.State != "accepted" {
			continue
		}
		seq, err := strconv.ParseUint(*it.OrderID, 10, 64)
		if err != nil || seq > 1<<48-1 {
			return fmt.Errorf("%w: place#%d 的单号 %q 不合法", ErrIncompleteBatch, i, *it.OrderID)
		}
		a, ok := accepted[NewOrderID(orders[i].Market, seq)]
		o := orders[i]
		lotsMatch := ok && (*a.Lots == o.Lots || (o.ReduceOnly && *a.Lots < o.Lots))
		if !ok || *a.Price != o.Price || !lotsMatch || *a.Filled != *it.FilledLots || *a.Resting != *it.Resting {
			return fmt.Errorf("%w: place#%d 被接受却没有一致的 OrderAccepted", ErrIncompleteBatch, i)
		}
		explained++
	}
	if explained != len(accepted) {
		return fmt.Errorf("%w: 事件里有逐项结果解释不了的 OrderAccepted", ErrIncompleteBatch)
	}
	for i, it := range cancelItems {
		if it.State == "canceled" && !canceled[cancels[i]] {
			return fmt.Errorf("%w: cancel#%d 被撤却没有 OrderCanceled", ErrIncompleteBatch, i)
		}
	}
	return nil
}

// checkBatchAggregates 聚合字段只能与逐项一致;给了就核,缺了不补。
func checkBatchAggregates(receipt WriteReceipt, places, cancels []*BatchItem, rejected int) error {
	count := func(items []*BatchItem, state string) uint64 {
		n := uint64(0)
		for _, it := range items {
			if it.State == state {
				n++
			}
		}
		return n
	}
	status := "ok"
	if total := len(places) + len(cancels); rejected == total && total > 0 {
		status = "none"
	} else if rejected > 0 {
		status = "partial"
	}
	if receipt.BatchStatus != nil && *receipt.BatchStatus != status {
		return fmt.Errorf("%w: batchStatus=%s 与逐项结果(%s)矛盾", ErrIncompleteBatch, *receipt.BatchStatus, status)
	}
	for _, field := range []struct {
		name string
		got  *uint64
		want uint64
	}{
		{"submitted", receipt.Submitted, uint64(len(places))},
		{"submittedCancels", receipt.SubmittedCancels, uint64(len(cancels))},
		{"accepted", receipt.Accepted, count(places, "accepted")},
		{"canceled", receipt.Canceled, count(cancels, "canceled")},
		{"rejected", receipt.Rejected, uint64(rejected)},
	} {
		if field.got != nil && *field.got != field.want {
			return fmt.Errorf("%w: %s 与逐项结果矛盾", ErrIncompleteBatch, field.name)
		}
	}
	return nil
}
