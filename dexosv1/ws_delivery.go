package dexosv1

import "sync"

type Backlog struct {
	Private int
	Market  int
}

func (st *Stream) Backlog() Backlog {
	return Backlog{Private: len(st.priv), Market: st.latest.pending()}
}

type delivery struct {
	sub *Subscription
	fn  func()
}

func latestWins(channel string) bool {
	switch channel {
	case "book", "bbo", "prices":
		return true
	}
	return false
}

func (st *Stream) deliverData(sub *Subscription, fn func()) {
	st.enqueue(sub, fn, true)
}

func (st *Stream) enqueue(sub *Subscription, fn func(), data bool) {
	if latestWins(sub.channel) {
		st.latest.put(sub, fn, data)
		return
	}
	select {
	case st.priv <- delivery{sub: sub, fn: fn}:
	case <-st.ctx.Done():
	}
}

func (st *Stream) deliverOne(d delivery) {
	if !st.isRemoved(d.sub) {
		d.fn()
	}
}

func (st *Stream) deliverPrivate() {
	for {
		select {
		case d := <-st.priv:
			st.deliverOne(d)
		case <-st.quit:
			return
		}
	}
}

func (st *Stream) deliverLatest() {
	for {
		select {
		case <-st.latest.wake:
		case <-st.quit:
			return
		}
		for _, batch := range st.latest.take() {
			for _, it := range batch.items {
				st.deliverOne(delivery{sub: batch.sub, fn: it.fn})
			}
		}
	}
}

type latestItem struct {
	fn   func()
	data bool
}

type latestBatch struct {
	sub   *Subscription
	items []latestItem
}

type latestQueue struct {
	mu    sync.Mutex
	order []*Subscription
	items map[*Subscription][]latestItem
	wake  chan struct{}
}

func newLatestQueue() *latestQueue {
	return &latestQueue{items: map[*Subscription][]latestItem{}, wake: make(chan struct{}, 1)}
}

func (q *latestQueue) put(sub *Subscription, fn func(), data bool) {
	q.mu.Lock()
	items, queued := q.items[sub]
	if !queued {
		q.order = append(q.order, sub)
	}
	if n := len(items); data && n > 0 && items[n-1].data {
		items[n-1].fn = fn
	} else {
		items = append(items, latestItem{fn: fn, data: data})
	}
	q.items[sub] = items
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *latestQueue) take() []latestBatch {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]latestBatch, 0, len(q.order))
	for _, sub := range q.order {
		out = append(out, latestBatch{sub: sub, items: q.items[sub]})
	}
	q.order = nil
	clear(q.items)
	return out
}

func (q *latestQueue) pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.order)
}
