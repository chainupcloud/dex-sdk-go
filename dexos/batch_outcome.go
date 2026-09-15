package dexos

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrIncompleteBatch 表示只有部分成功证据。返回的事件必须保留供核查，不能按价量猜失败项。
var ErrIncompleteBatch = errors.New("dexos: 批次未取得全部输入项的成功证据")

// checkBatchOutcome 只证明完整成功；当前服务端未返回失败输入索引，部分成功须由调用方冻结核查。
func checkBatchOutcome(events []EventEnvelope, account uint32, orders []BatchOrder, cancels []OrderID) error {
	type accepted struct {
		Account *uint32 `json:"account"`
		Market  *uint16 `json:"market"`
		Seq     *uint64 `json:"orderSeq"`
		Price   *uint32 `json:"price"`
		Lots    *uint64 `json:"lots"`
		Filled  *uint64 `json:"filledLots"`
		Resting *bool   `json:"resting"`
	}
	var placed []accepted
	wantCancel := make(map[OrderID]bool, len(cancels))
	for _, id := range cancels {
		wantCancel[id] = false
	}
	seen := make(map[OrderID]bool)
	for _, ev := range events {
		switch ev.Kind {
		case "OrderAccepted":
			var a accepted
			if json.Unmarshal(ev.Data, &a) != nil || a.Account == nil || a.Market == nil || a.Seq == nil || *a.Seq == 0 || *a.Seq > 1<<48-1 || a.Price == nil || a.Lots == nil || a.Filled == nil || a.Resting == nil || *a.Account != account || *a.Filled > *a.Lots {
				return fmt.Errorf("%w: OrderAccepted 字段缺失或身份不符", ErrIncompleteBatch)
			}
			id := NewOrderID(*a.Market, *a.Seq)
			if seen[id] {
				return fmt.Errorf("%w: 重复订单身份", ErrIncompleteBatch)
			}
			seen[id] = true
			placed = append(placed, a)
		case "OrderCanceled":
			var a accepted
			if json.Unmarshal(ev.Data, &a) != nil || a.Account == nil || a.Market == nil || a.Seq == nil || *a.Seq > 1<<48-1 || *a.Account != account {
				return fmt.Errorf("%w: OrderCanceled 字段缺失或身份不符", ErrIncompleteBatch)
			}
			id := NewOrderID(*a.Market, *a.Seq)
			if _, ok := wantCancel[id]; ok {
				wantCancel[id] = true
			}
		}
	}
	canceled := 0
	for _, ok := range wantCancel {
		if ok {
			canceled++
		}
	}
	if len(placed) != len(orders) || canceled != len(wantCancel) {
		return fmt.Errorf("%w: 下单 %d/%d，撤单 %d/%d；失败项无法可靠关联", ErrIncompleteBatch, len(placed), len(orders), canceled, len(wantCancel))
	}
	// 仅在所有项均接受后，才可按内核 batch_place 的输入顺序校验，部分成功绝不压缩索引。
	for i, a := range placed {
		o := orders[i]
		if *a.Market != o.Market || *a.Price != o.Price || *a.Lots != o.Lots {
			return fmt.Errorf("%w: 第 %d 个完整成功回执与输入不一致", ErrIncompleteBatch, i)
		}
	}
	return nil
}
