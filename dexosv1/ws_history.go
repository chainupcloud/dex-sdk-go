package dexosv1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 流水类私有频道(成交 `fills`、资金流水 `ledger`)的续传。
//
// 不缺不重靠三件事:
//   - 按位置去重:只交付位置严格大于已交付位置的条目;
//   - WS 帧的连续性:订阅确认帧给出起点位号,位号大于它的条目都会在这个订阅上推;服务端没推的必有 `lagged`,
//     而 lagged 之后推的帧都在它给的上界之后;
//   - 先补齐、再放行:(重)订阅确认之后补拉到起点、lagged 之后补拉到上界,补齐之前到的帧先缓存,补齐后再交付。
//
// REST 补拉在后台 goroutine 里跑,结果回到 Run 的 goroutine 里交付,不阻塞其他频道的帧。

// HistoryCursor 流水订阅的起点:只交付严格大于 After 的条目;Epoch 是 After 所属的账本纪元。
// 一般来自上一次的 [Subscription.Cursor],或 [Client.HistoryHead](从「现在」起)。
type HistoryCursor struct {
	After string
	Epoch string
}

// HistoryHead 账户当前视图处的游标:用它订阅成交 / 流水 = 只要这之后的,不要历史。
func (c *Client) HistoryHead(ctx context.Context, ref AccountRef) (HistoryCursor, error) {
	a, err := c.Account(ctx, ref)
	if err != nil {
		return HistoryCursor{}, err
	}
	return HistoryCursor{After: cursorAt(a.AsOfSeq).String(), Epoch: a.Epoch}, nil
}

// cpos 游标 `<seq>-<sub>-<idx>` 的数值形,按字典序比较。
type cpos struct{ seq, sub, idx uint64 }

// cursorAt 日志位号 seq 那一条命令的末尾(含折叠行):补拉到它 = 这条命令及之前的都要。
func cursorAt(seq uint64) cpos { return cpos{seq, 0xffff, 0xffffffff} }

func (p cpos) String() string { return fmt.Sprintf("%d-%d-%d", p.seq, p.sub, p.idx) }

func (p cpos) less(q cpos) bool {
	if p.seq != q.seq {
		return p.seq < q.seq
	}
	if p.sub != q.sub {
		return p.sub < q.sub
	}
	return p.idx < q.idx
}

func parseCursor(s string) (cpos, error) {
	parts := strings.Split(s, "-")
	if len(parts) != 3 {
		return cpos{}, fmt.Errorf("游标 %q 不是 <seq>-<sub>-<idx>", s)
	}
	var out [3]uint64
	for i, bits := range []int{64, 16, 32} {
		v, err := strconv.ParseUint(parts[i], 10, bits)
		if err != nil {
			return cpos{}, fmt.Errorf("游标 %q: %w", s, err)
		}
		out[i] = v
	}
	return cpos{out[0], out[1], out[2]}, nil
}

// histItem 一条待交付的流水(REST 补拉来的或 WS 帧)。
type histItem struct {
	id   string
	pos  cpos
	emit func()
}

// histSync 一个流水订阅的续传状态。lastID 受 mu 保护(给 Cursor 读);其余只在 Run 的 goroutine 里读写。
type histSync struct {
	mu     sync.Mutex
	lastID string
	epoch  string

	last       cpos       // 已交付到的位置;不超过它的都已交付
	covered    cpos       // REST 已补齐到(含)的位置
	hasCovered bool       //
	streaming  bool       // 当前订阅上的 WS 帧已与补拉接上,之后直接交付
	buf        []histItem // 还没接上时收到的 WS 帧
	lagging    bool       // 补拉到 lagTo 才接得上:订阅确认后是确认帧的起点,lagged 之后是它的上界
	lagTo      cpos       //
	running    bool       // 有一轮补拉在后台跑

	fetch  func(ctx context.Context, after, epoch, upper string) ([]histItem, error)
	decode func(f *wsFrame) ([]histItem, error)
}

func (h *histSync) emit(it histItem) {
	if !h.last.less(it.pos) {
		return
	}
	h.last = it.pos
	h.mu.Lock()
	h.lastID = it.id
	h.mu.Unlock()
	it.emit()
}

// reset 在当前连接上(重)订阅:WS 帧要重新接上。缓存的旧帧丢掉,由新确认帧之后的补拉覆盖。
func (h *histSync) reset() {
	h.streaming, h.buf, h.lagging = false, nil, false
}

// SubscribeFills 账户成交(`fills`):从 from 起逐条交付,不缺不重。from 用 [Client.HistoryHead] 或上次的
// [Subscription.Cursor];纪元与当前不符时订阅失败([ErrEpochChanged])。
func (st *Stream) SubscribeFills(ref AccountRef, from HistoryCursor, fn func(Fill)) (*Subscription, error) {
	sub, h, err := st.histSub("fills", ref, from)
	if err != nil {
		return nil, err
	}
	item := func(f Fill) (histItem, error) {
		p, err := parseCursor(f.ID)
		if err != nil {
			return histItem{}, fmt.Errorf("dexosv1: 成交 id: %w", err)
		}
		return histItem{id: f.ID, pos: p, emit: func() { fn(f) }}, nil
	}
	h.fetch = func(ctx context.Context, after, epoch, upper string) ([]histItem, error) {
		res, err := st.c.Fills(ctx, st.cat, ref, "", HistoryQuery{After: after, Epoch: epoch, Upper: upper})
		if err != nil {
			return nil, err
		}
		out := make([]histItem, 0, len(res.Fills))
		for _, f := range res.Fills {
			it, err := item(f)
			if err != nil {
				return nil, err
			}
			out = append(out, it)
		}
		return out, nil
	}
	h.decode = func(f *wsFrame) ([]histItem, error) {
		var w wireFill
		if err := json.Unmarshal(f.Data, &w); err != nil {
			return nil, fmt.Errorf("dexosv1: fills 帧解析失败: %w", err)
		}
		fill, err := w.fill(st.cat)
		if err != nil {
			return nil, err
		}
		it, err := item(fill)
		if err != nil {
			return nil, err
		}
		return []histItem{it}, nil
	}
	return st.add(sub)
}

// SubscribeLedger 账户资金流水(`ledger`):从 from 起逐条交付,不缺不重,规则同 [Stream.SubscribeFills]。
func (st *Stream) SubscribeLedger(ref AccountRef, from HistoryCursor, fn func(LedgerEntry)) (*Subscription, error) {
	sub, h, err := st.histSub("ledger", ref, from)
	if err != nil {
		return nil, err
	}
	item := func(e LedgerEntry) (histItem, error) {
		p, err := parseCursor(e.ID)
		if err != nil {
			return histItem{}, fmt.Errorf("dexosv1: 流水 id: %w", err)
		}
		return histItem{id: e.ID, pos: p, emit: func() { fn(e) }}, nil
	}
	h.fetch = func(ctx context.Context, after, epoch, upper string) ([]histItem, error) {
		res, err := st.c.Ledger(ctx, ref, LedgerQuery{HistoryQuery: HistoryQuery{After: after, Epoch: epoch, Upper: upper}})
		if err != nil {
			return nil, err
		}
		out := make([]histItem, 0, len(res.Entries))
		for _, e := range res.Entries {
			it, err := item(e)
			if err != nil {
				return nil, err
			}
			out = append(out, it)
		}
		return out, nil
	}
	h.decode = func(f *wsFrame) ([]histItem, error) {
		// 一条命令一帧:身份 + entries[],每条与 REST 的条目逐字段相同(v1.1 §4.9-2)
		var w struct {
			Entries *[]LedgerEntry `json:"entries"`
		}
		if err := json.Unmarshal(f.Data, &w); err != nil {
			return nil, fmt.Errorf("dexosv1: ledger 帧解析失败: %w", err)
		}
		if w.Entries == nil {
			return nil, fmt.Errorf("dexosv1: ledger 帧没有 entries: %s", f.Data)
		}
		out := make([]histItem, 0, len(*w.Entries))
		for _, e := range *w.Entries {
			it, err := item(e)
			if err != nil {
				return nil, err
			}
			out = append(out, it)
		}
		return out, nil
	}
	return st.add(sub)
}

func (st *Stream) histSub(channel string, ref AccountRef, from HistoryCursor) (*Subscription, *histSync, error) {
	if from.After == "" || from.Epoch == "" {
		return nil, nil, fmt.Errorf("dexosv1: %s 订阅要给起点游标与纪元(从现在起用 Client.HistoryHead)", channel)
	}
	p, err := parseCursor(from.After)
	if err != nil {
		return nil, nil, fmt.Errorf("dexosv1: %s 起点: %w", channel, err)
	}
	sub, err := accountSub(channel, ref, false)
	if err != nil {
		return nil, nil, err
	}
	h := &histSync{lastID: from.After, epoch: from.Epoch, last: p}
	sub.hist = h
	return sub, h, nil
}

// histStart 订阅确认之后(纪元已核对):位号大于确认帧 seq 的条目都会在这个订阅上推,补拉到 seq 那条命令的
// 末尾就接得上。本连接上已有过 lagged 时还要补到它的上界:服务端不再推不超过它的流水帧。
func (st *Stream) histStart(sub *Subscription, seq uint64) {
	h := sub.hist
	h.lagging, h.lagTo = true, cursorAt(seq)
	if st.hasFloor && h.lagTo.less(st.floor) {
		h.lagTo = st.floor
	}
	st.histKick(sub)
}

// histFrame 一帧 WS 流水:接上了就直接交付(去重);没接上先缓存,补拉覆盖到它再交付。
func (st *Stream) histFrame(sub *Subscription, f *wsFrame) error {
	h := sub.hist
	if !sub.acked {
		// 确认之前的帧来自同一连接上的上一次订阅,位置都不超过新确认帧的起点,由确认后的补拉覆盖
		return nil
	}
	items, err := h.decode(f)
	if err != nil {
		// 这一帧交付不了:之后的帧不能再直接交付,要等补拉越过它
		h.streaming = false
		return err
	}
	if h.streaming {
		for _, it := range items {
			h.emit(it)
		}
		return nil
	}
	h.buf = append(h.buf, items...)
	st.histKick(sub)
	return nil
}

// histLagged 服务端丢帧:补拉到它给的上界,之后的 WS 帧都在上界之后,接得上。
func (st *Stream) histLagged(sub *Subscription, lag *LaggedError, upper cpos) {
	h := sub.hist
	h.streaming = false
	if !h.lagging || h.lagTo.less(upper) {
		h.lagging, h.lagTo = true, upper
	}
	st.setState(sub, StateSyncing, lag)
	st.histKick(sub)
}

// histKick 需要补拉且没有一轮在跑时,在后台起一轮。目标依次是 lagTo(确认帧起点 / lagged 上界)、缓存的第一帧。
func (st *Stream) histKick(sub *Subscription) {
	h := sub.hist
	if h.running || sub.State() == StateFailed || st.isRemoved(sub) {
		return
	}
	var target cpos
	switch {
	case h.lagging:
		target = h.lagTo
	case len(h.buf) > 0:
		target = h.buf[0].pos
	default:
		return
	}
	h.running = true
	from, epoch, ctx := h.lastID, h.epoch, st.ctx
	go func() {
		items, err := st.histFetch(ctx, h, from, epoch, target)
		st.post(func() { st.histDone(sub, target, items, err) })
	}()
}

// histFetch 补拉 (from, upper](后台 goroutine)。上界还没写进执行历史(400 GatewayHistoryIncomplete)就等,
// 最多 CatchUpWait。
func (st *Stream) histFetch(ctx context.Context, h *histSync, from, epoch string, upper cpos) ([]histItem, error) {
	start, err := parseCursor(from)
	if err != nil {
		return nil, err
	}
	if !start.less(upper) {
		return nil, nil
	}
	deadline := time.Now().Add(st.opt.CatchUpWait)
	wait := 50 * time.Millisecond
	for {
		items, err := h.fetch(ctx, from, epoch, upper.String())
		switch {
		case err == nil:
			return items, nil
		case errors.Is(err, dexos.ErrHistoryEpochMismatch):
			return nil, fmt.Errorf("%w: %w", ErrEpochChanged, err)
		case !errors.Is(err, dexos.ErrHistoryUpperIncomplete) || !time.Now().Before(deadline):
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Second)
	}
}

// histDone 一轮补拉的结果(Run 的 goroutine):交付、记下覆盖位置、能接上就放行缓存的帧,还有目标就再起一轮。
func (st *Stream) histDone(sub *Subscription, upper cpos, items []histItem, err error) {
	h := sub.hist
	h.running = false
	if sub.State() == StateFailed || st.isRemoved(sub) {
		return
	}
	if err != nil {
		if errors.Is(err, ErrEpochChanged) {
			st.fail(sub, err)
			return
		}
		st.setState(sub, StateSyncing, err)
		st.retryLater(sub, func() error {
			st.histKick(sub)
			return nil
		})
		return
	}
	for _, it := range items {
		h.emit(it)
	}
	if !h.hasCovered || h.covered.less(upper) {
		h.covered, h.hasCovered = upper, true
	}
	if h.lagging && !h.covered.less(h.lagTo) {
		h.lagging, h.streaming = false, true
	}
	// 还在补向 lagTo 时不能凭缓存的第一帧放行:lagTo 之内服务端不再推,缓存里第一帧之后可能就是缺口
	if !h.lagging && len(h.buf) > 0 && !h.covered.less(h.buf[0].pos) {
		h.streaming = true
	}
	if h.streaming {
		for _, it := range h.buf {
			h.emit(it)
		}
		h.buf = nil
		sub.retries = 0
		st.setState(sub, StateLive, nil)
	}
	st.histKick(sub)
}
