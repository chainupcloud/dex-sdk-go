package dexosv1

import (
	"context"
	"encoding/json"
	"testing"
)

func benchStream(b *testing.B) (*Stream, AccountRef) {
	f, c := newFake(b)
	cat := catalog(b, f, c)
	st, err := NewStream(c, cat, StreamOptions{})
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.Cleanup(cancel)
	st.ctx, st.gen, st.epoch = ctx, 1, testEpoch
	st.routes = map[string]*Subscription{}
	go st.deliverPrivate()
	go st.deliverLatest()
	b.Cleanup(func() { close(st.quit) })
	return st, AccountRef{Address: mustAddr(b, testAddr)}
}

func benchLive(st *Stream, sub *Subscription) {
	sub.gen, sub.acked = st.gen, true
	sub.state.Store(int32(StateLive))
	st.routes[sub.route] = sub
}

func benchFrame(b *testing.B, channel string, data any, seq uint64, epoch string) []byte {
	f := map[string]any{"channel": channel, "data": data, "seq": seq, "isSnapshot": false}
	if epoch != "" {
		f["epoch"] = epoch
	}
	raw, err := json.Marshal(f)
	if err != nil {
		b.Fatal(err)
	}
	return raw
}

func BenchmarkStreamHandleOrderEvent(b *testing.B) {
	st, ref := benchStream(b)
	sub, err := st.SubscribeOrderChanges(ref, func(OrderChange) {})
	if err != nil {
		b.Fatal(err)
	}
	benchLive(st, sub)
	raw := benchFrame(b, "orders", orderFrame(map[string]any{"orderId": "2240412712970", "clientOrderId": "mm-2", "event": "canceled",
		"status": "canceled", "side": "sell", "price": "2495", "size": "1", "filled": "0.25", "remaining": "0.75"}), 12, testEpoch)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := st.handle(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamHandleFill(b *testing.B) {
	st, ref := benchStream(b)
	sub, err := st.SubscribeFills(ref, HistoryCursor{After: "1-0-0", Epoch: testEpoch}, func(Fill) {})
	if err != nil {
		b.Fatal(err)
	}
	benchLive(st, sub)
	sub.hist.streaming = true
	raw := benchFrame(b, "fills", private(fillRow("5-0-0")), 5, testEpoch)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		sub.hist.last = cpos{}
		if err := st.handle(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamHandleBook(b *testing.B) {
	st, _ := benchStream(b)
	sub, err := st.SubscribeBook(mustMarket(b, st.catalog(), "eth-usd"), 20, func(*Book) {})
	if err != nil {
		b.Fatal(err)
	}
	benchLive(st, sub)
	frame := bookFrame(7)
	var bids, asks [][]string
	for range 20 {
		bids = append(bids, []string{"2492.6", "1"})
		asks = append(asks, []string{"2493.6", "1"})
	}
	frame["bids"], frame["asks"], frame["kind"], frame["depth"] = bids, asks, "perp", 20
	raw := benchFrame(b, "book", frame, 7, "")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := st.handle(raw); err != nil {
			b.Fatal(err)
		}
	}
}
