package dexosv1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// CommandRef 引用本账户此前一次 BatchReplace3 换单挂出的新单(dex-os#612):Seq / Sub 是那次换单定序回执里的位号
// ([Client.SendSequenced]),Item 是那次换单里第几个输入(改单项在前、新单在后,共用一个编号,从 0 起;撤单项不编号)。
// 市场取本次换单的市场。只能引用本账户最近 64 次、日志时间 60 秒内的 BatchReplace3;超出范围那一项以
// ReferenceOutOfRange 被拒,这时那张单可能还挂着,要改按订单号处理。
type CommandRef struct {
	Seq  uint64
	Sub  uint16
	Item uint16
}

// OrderTarget BatchReplace3 的撤单、改单目标:OrderID(本市场的订单序号)与 Ref 恰好给一个。
type OrderTarget struct {
	OrderID *uint64
	Ref     *CommandRef
}

// ModifyTarget BatchReplace3 的改单项,语义同 [ModifyRequest],目标可以是引用。
type ModifyTarget struct {
	Target     OrderTarget
	Price      uint32
	Lots       uint64
	TIF        dexos.TimeInForce
	ReduceOnly bool
}

type wireRef struct {
	Seq  uint64 `json:"seq"`
	Sub  uint16 `json:"sub"`
	Item uint16 `json:"item"`
}

type wireRefTarget struct {
	Ref wireRef `json:"ref"`
}

type wireModify3Req struct {
	OrderID    string   `json:"orderId,omitempty"`
	Ref        *wireRef `json:"ref,omitempty"`
	Price      string   `json:"price"`
	Size       string   `json:"size"`
	TIF        string   `json:"tif"`
	ReduceOnly bool     `json:"reduceOnly"`
}

type wireReplace3Req struct {
	*envelope
	Symbol         string           `json:"symbol"`
	Kind           string           `json:"kind,omitempty"`
	ReplaceVersion int              `json:"replaceVersion"`
	Cancels        []any            `json:"cancels"`
	Modifies       []wireModify3Req `json:"modifies,omitempty"`
	Orders         []wireOrderReq   `json:"orders"`
}

func (t OrderTarget) encode(m *Market) (dexos.OrderTarget, any, error) {
	switch {
	case (t.OrderID == nil) == (t.Ref == nil):
		return dexos.OrderTarget{}, nil, errors.New("dexosv1: 换单目标的订单号与引用必须恰好给一个")
	case t.Ref != nil:
		r := *t.Ref
		return dexos.OrderTarget{Ref: &dexos.CommandRef{Market: m.SigningMarketID, Seq: r.Seq, Sub: r.Sub, Item: r.Item}},
			wireRef{Seq: r.Seq, Sub: r.Sub, Item: r.Item}, nil
	default:
		id := dexos.NewOrderID(m.SigningMarketID, *t.OrderID)
		return dexos.OrderTarget{OrderID: &id}, strconv.FormatUint(*t.OrderID, 10), nil
	}
}

// SignReplace3 签一次 BatchReplace3 换单(撤单 → 改单 → 下单,同 [ActionWriter.SignReplace]),撤单与改单目标可以按命令位置
// 引用此前 BatchReplace3 挂出的新单。只有 BatchReplace3 挂出的单能被引用,所以要被后续引用的换单(哪怕只挂新单)也用它签。
// 目标非法或重复(同一订单号或同一引用在撤单项与改单项里出现不止一次)在签名之前就拒,不动 nonce。
func (w *ActionWriter) SignReplace3(m *Market, cancels []OrderTarget, modifies []ModifyTarget, orders []OrderRequest, expiresAt time.Time) (*SignedAction, error) {
	if err := sameMarket(m, orders); err != nil {
		return nil, err
	}
	seen := make(map[any]bool, len(cancels)+len(modifies))
	target := func(t OrderTarget) (dexos.OrderTarget, any, error) {
		enc, wire, err := t.encode(m)
		if err != nil {
			return enc, nil, err
		}
		var key any
		if t.OrderID != nil {
			key = *t.OrderID
		} else {
			key = *t.Ref
		}
		if seen[key] {
			return enc, nil, fmt.Errorf("dexosv1: 换单目标 %v 在撤单项与改单项里出现了不止一次", key)
		}
		seen[key] = true
		return enc, wire, nil
	}
	cancelTargets := make([]dexos.OrderTarget, len(cancels))
	wireCancels := make([]any, len(cancels))
	for i, c := range cancels {
		enc, wire, err := target(c)
		if err != nil {
			return nil, err
		}
		cancelTargets[i] = enc
		if ref, ok := wire.(wireRef); ok {
			wireCancels[i] = wireRefTarget{Ref: ref}
		} else {
			wireCancels[i] = wire
		}
	}
	mods := make([]dexos.BatchModify3, len(modifies))
	var wireMods []wireModify3Req
	for i, x := range modifies {
		enc, wire, err := target(x.Target)
		if err != nil {
			return nil, err
		}
		tif, ok := tifWire[x.TIF]
		if !ok {
			return nil, fmt.Errorf("dexosv1: 未知的 TIF %d", x.TIF)
		}
		if x.Price == 0 || x.Lots == 0 {
			return nil, errors.New("dexosv1: 改单的价格与数量都必须大于 0")
		}
		item := wireModify3Req{Price: m.Scale.Price(x.Price), Size: m.Scale.Size(x.Lots), TIF: tif, ReduceOnly: x.ReduceOnly}
		if ref, ok := wire.(wireRef); ok {
			item.Ref = &ref
		} else {
			item.OrderID = wire.(string)
		}
		wireMods = append(wireMods, item)
		mods[i] = dexos.BatchModify3{Target: enc, Price: x.Price, Lots: x.Lots, TIF: x.TIF, ReduceOnly: x.ReduceOnly}
	}
	items := make([]wireOrderReq, len(orders))
	batch := make([]dexos.BatchOrder, len(orders))
	for i, o := range orders {
		wire, err := o.wire(nil)
		if err != nil {
			return nil, err
		}
		items[i], batch[i] = wire, o.batchOrder()
	}
	now := w.now()
	nonce, err := w.nonce(now)
	if err != nil {
		return nil, err
	}
	nowMs := uint64(now.UnixMilli())
	cmd := dexos.EncodeBatchReplace3(dexos.ActorPlaceholder, nowMs, cancelTargets, mods, batch)
	env, id, err := w.sign(cmd, nonce, expiresAt)
	if err != nil {
		return nil, err
	}
	env.NowMs = nowMs
	body, err := json.Marshal(wireReplace3Req{envelope: env, Symbol: m.Symbol, Kind: m.Kind, ReplaceVersion: 3, Cancels: wireCancels, Modifies: wireMods, Orders: items})
	if err != nil {
		return nil, err
	}
	return &SignedAction{Path: "/v1/orders/replace", Body: body, Request: id, Market: m, Command: cmd, NowMs: nowMs, ExpiresAt: env.ExpiresAt,
		Cancels: len(cancels), Modifies: len(modifies), Places: len(orders)}, nil
}

// SendSequenced 发出(或原样重发)一次签好的写,定序即返回(`ack=sequenced`):回执只有日志位号与请求身份,结论凭
// [Client.Request] 按请求身份另查。定序之前就被拒(4xx、限流)照常报错,按 [ClassifyAction] 判;已经执行过的重发回
// 原来的位号。
func (c *Client) SendSequenced(ctx context.Context, a *SignedAction) (Receipt, error) {
	status, raw, err := c.postAck(ctx, a.Path, json.RawMessage(a.Body), "sequenced")
	if err != nil {
		return Receipt{}, err
	}
	r, err := receipt(status, raw, a.Request)
	if errors.Is(err, ErrPending) {
		return r, nil
	}
	return r, err
}
