package dexosv1

import (
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func bookFrame(seq uint64) map[string]any {
	return map[string]any{"symbol": "ETH-USD", "depth": 5, "bids": [][]string{{"2492.6", fmt.Sprint(seq)}}, "asks": [][]string{},
		"seq": seq, "ts": seq}
}

func acceptedFrame(id uint64) map[string]any {
	return orderFrame(map[string]any{"orderId": fmt.Sprint(id), "event": "accepted", "status": "open",
		"side": "buy", "price": "2400", "size": "1", "filled": "0", "remaining": "1"})
}

func emptyOrders() map[string]any {
	return map[string]any{"address": testAddr, "subaccount": 0, "orders": []any{}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等不到: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestStreamSlowPrivateCallbackDoesNotHoldBackBook(t *testing.T) {
	h := newWSHarness(t, nil)
	eth := mustMarket(t, h.cat, "eth-usd")
	books := make(chan *Book, 4096)
	var handled atomic.Int64
	bs, _ := h.st.SubscribeBook(eth, 5, func(b *Book) { books <- b })
	ords, _ := h.st.SubscribeOrderChanges(h.ref, func(c OrderChange) {
		if !c.Snapshot {
			time.Sleep(20 * time.Millisecond)
			handled.Add(1)
		}
	})
	h.run()
	conn := h.fw.accept(t)
	reqs := conn.subscribes(t, 2)
	conn.ack(t, reqs["book"], testEpoch, 1)
	conn.ack(t, reqs["orders"], testEpoch, 1)
	conn.push(t, "book", bookFrame(1), 1, "", true)
	conn.push(t, "orders", emptyOrders(), 1, testEpoch, true)
	h.waitState(bs, StateLive)
	h.waitState(ords, StateLive)
	recv(t, books)

	for i := range uint64(200) {
		conn.push(t, "orders", acceptedFrame(i+1), i+2, testEpoch, false)
	}
	start := time.Now()
	for seq := uint64(2); seq <= 500; seq++ {
		conn.push(t, "book", bookFrame(seq), seq, "", false)
	}
	for b := recv(t, books); b.Seq != 500; b = recv(t, books) {
	}
	if took, done := time.Since(start), handled.Load(); took > time.Second || done >= 100 {
		t.Fatalf("盘口要越过还没处理完的挂单回调送到: 用了 %v,挂单回调已处理 %d/200", took, done)
	}
	waitFor(t, "挂单回调全部处理完", func() bool { return handled.Load() == 200 })
}

func TestStreamBookDeliversOnlyTheLatestFrameToASlowCallback(t *testing.T) {
	h := newWSHarness(t, nil)
	eth := mustMarket(t, h.cat, "eth-usd")
	gate, started := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var seen []uint64
	_, _ = h.st.SubscribeBook(eth, 5, func(b *Book) {
		if b.Seq == 1 {
			close(started)
			<-gate
		}
		mu.Lock()
		seen = append(seen, b.Seq)
		mu.Unlock()
	})
	h.run()
	conn := h.fw.accept(t)
	conn.ack(t, conn.subscribes(t, 1)["book"], testEpoch, 1)
	conn.push(t, "book", bookFrame(1), 1, "", true)
	recv(t, started)
	for seq := uint64(2); seq <= 50; seq++ {
		conn.push(t, "book", bookFrame(seq), seq, "", false)
	}
	time.Sleep(200 * time.Millisecond)
	close(gate)
	waitFor(t, "最后一帧盘口", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) > 0 && seen[len(seen)-1] == 50
	})
	mu.Lock()
	defer mu.Unlock()
	if len(seen) >= 10 {
		t.Fatalf("慢回调只拿最新一帧,中间帧合并: %v", seen)
	}
}

func TestStreamPrivateFramesArriveInOrderThroughAFullQueue(t *testing.T) {
	h := newWSHarness(t, func(o *StreamOptions) { o.PrivateQueue = 8 })
	h.history(1000)
	var mu sync.Mutex
	var orders []uint64
	var fills []string
	ords, _ := h.st.SubscribeOrderChanges(h.ref, func(c OrderChange) {
		if c.Snapshot {
			return
		}
		time.Sleep(time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		if c.Order == nil || c.Order.OrderID != c.Event.OrderID {
			t.Errorf("挂上的单要在集合里: %+v", c)
		}
		orders = append(orders, c.Event.OrderID)
	})
	fs, _ := h.st.SubscribeFills(h.ref, HistoryCursor{After: cursorAt(2).String(), Epoch: testEpoch}, func(f Fill) {
		time.Sleep(time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		fills = append(fills, f.ID)
	})
	h.run()
	conn := h.fw.accept(t)
	reqs := conn.subscribes(t, 2)
	conn.ack(t, reqs["orders"], testEpoch, 2)
	conn.ack(t, reqs["fills"], testEpoch, 2)
	conn.push(t, "orders", emptyOrders(), 2, testEpoch, true)
	h.waitState(ords, StateLive)
	h.waitState(fs, StateLive)

	var peak atomic.Int64
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n := int64(h.st.Backlog().Private); n > peak.Load() {
				peak.Store(n)
			}
			time.Sleep(100 * time.Microsecond)
		}
	}()
	const n = 300
	var wantOrders []uint64
	var wantFills []string
	for i := range uint64(n) {
		seq := i + 3
		conn.push(t, "orders", acceptedFrame(i+1), seq, testEpoch, false)
		conn.push(t, "fills", private(fillRow(fmt.Sprintf("%d-0-0", seq))), seq, testEpoch, false)
		wantOrders = append(wantOrders, i+1)
		wantFills = append(wantFills, fmt.Sprintf("%d-0-0", seq))
	}
	waitFor(t, "挂单与成交全部交付", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(orders) >= n && len(fills) >= n
	})
	time.Sleep(50 * time.Millisecond)
	close(stop)
	<-watched
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(orders, wantOrders) || !reflect.DeepEqual(fills, wantFills) {
		t.Fatalf("私有帧要按序、不缺不重:挂单 %d 条,成交 %d 条", len(orders), len(fills))
	}
	if p := peak.Load(); p == 0 || p > 8 {
		t.Fatalf("私有队列积压的峰值 %d 应在 1..=8 之间", p)
	}
	if c := fs.Cursor(); c.After != wantFills[n-1] {
		t.Fatalf("游标停在最后交付的成交: %+v", c)
	}
}

func TestStreamStateEventsKeepTheirPlaceAmongBookFrames(t *testing.T) {
	var mu sync.Mutex
	var log []string
	note := func(s string) {
		mu.Lock()
		log = append(log, s)
		mu.Unlock()
	}
	h := newWSHarness(t, func(o *StreamOptions) {
		prev := o.OnState
		o.OnState = func(e StateEvent) {
			prev(e)
			note(e.State.String())
		}
	})
	eth := mustMarket(t, h.cat, "eth-usd")
	bs, _ := h.st.SubscribeBook(eth, 5, func(b *Book) {
		time.Sleep(3 * time.Millisecond)
		note(fmt.Sprintf("book:%d", b.Seq))
	})
	h.run()
	conn := h.fw.accept(t)
	conn.ack(t, conn.subscribes(t, 1)["book"], testEpoch, 1)
	conn.push(t, "book", bookFrame(1), 1, "", true)
	h.waitState(bs, StateLive)
	for seq := uint64(2); seq <= 20; seq++ {
		conn.push(t, "book", bookFrame(seq), seq, "", false)
	}
	conn.send(t, map[string]any{"channel": "lagged", "data": map[string]any{"dropped": 7, "epoch": testEpoch,
		"upper": "20-0-4294967295", "reason": "buffer"}})
	conn.push(t, "book", bookFrame(21), 21, "", true)
	conn.push(t, "book", bookFrame(22), 22, "", false)
	waitFor(t, "lagged 之后的帧", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(log, "book:22")
	})
	mu.Lock()
	defer mu.Unlock()
	syncing := slices.Index(log, "syncing")
	if syncing < 0 {
		t.Fatalf("lagged 之后要报 Syncing: %v", log)
	}
	after := log[syncing:]
	want := []string{"syncing", "book:21", "live", "book:22"}
	if !slices.Equal(after, want) {
		t.Fatalf("同一订阅的状态与数据要按发生顺序交付: %v", log)
	}
}

func TestStreamFillsCursorWaitsForTheCallback(t *testing.T) {
	h := newWSHarness(t, nil)
	h.history(200)
	start := cursorAt(100).String()
	gate, started := make(chan struct{}), make(chan struct{})
	got := make(chan string, 4)
	sub, _ := h.st.SubscribeFills(h.ref, HistoryCursor{After: start, Epoch: testEpoch}, func(f Fill) {
		if f.ID == "101-0-0" {
			close(started)
			<-gate
		}
		got <- f.ID
	})
	h.run()
	conn := h.fw.accept(t)
	conn.ack(t, conn.subscribes(t, 1)["fills"], testEpoch, 100)
	h.waitState(sub, StateLive)
	conn.push(t, "fills", private(fillRow("101-0-0")), 101, testEpoch, false)
	conn.push(t, "fills", private(fillRow("102-0-0")), 102, testEpoch, false)
	recv(t, started)
	time.Sleep(50 * time.Millisecond)
	if c := sub.Cursor(); c.After != start {
		t.Fatalf("回调还卡在 101-0-0,游标不能越过它: %+v", c)
	}
	close(gate)
	if a, b := recv(t, got), recv(t, got); a != "101-0-0" || b != "102-0-0" {
		t.Fatalf("交付顺序 %s %s", a, b)
	}
	waitFor(t, "游标走到 102-0-0", func() bool { return sub.Cursor().After == "102-0-0" })
}

func TestStreamNoCallbackAfterUnsubscribeReturns(t *testing.T) {
	h := newWSHarness(t, nil)
	gate, started := make(chan struct{}), make(chan struct{})
	got := make(chan uint64, 16)
	sub, _ := h.st.SubscribeOrderChanges(h.ref, func(c OrderChange) {
		if c.Snapshot {
			return
		}
		if c.Event.OrderID == 1 {
			close(started)
			<-gate
		}
		got <- c.Event.OrderID
	})
	h.run()
	conn := h.fw.accept(t)
	conn.ack(t, conn.subscribes(t, 1)["orders"], testEpoch, 1)
	conn.push(t, "orders", emptyOrders(), 1, testEpoch, true)
	h.waitState(sub, StateLive)
	for i := range uint64(5) {
		conn.push(t, "orders", acceptedFrame(i+1), i+2, testEpoch, false)
	}
	recv(t, started)
	time.Sleep(50 * time.Millisecond)
	sub.Unsubscribe()
	close(gate)
	if id := recv(t, got); id != 1 {
		t.Fatalf("在跑的那次回调照常结束: %d", id)
	}
	quiet(t, got, 100*time.Millisecond)
}
