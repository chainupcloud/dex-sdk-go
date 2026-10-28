package dexosv1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// v1 WebSocket(`GET /v1/ws`,开放 API v1 §4.6 + v1.1 §4.5)的客户端。
//
// 一条连接承载全部订阅,断线自动重连并重订。每个订阅有一个状态([State]):
//
//   - [StateSyncing]:本地状态不可信 —— 刚订阅还没收到快照、连接断了、服务端丢帧(lagged)后在等快照或补拉;
//   - [StateLive]:快照已到 / 补拉已拼好,之后的帧按序应用即与服务端一致;
//   - [StateFailed]:终态 —— 订阅被服务端拒绝,或者账本纪元变了([ErrEpochChanged])。不会自行恢复。
//
// 做市的用法:收到 Syncing 就暂停对应币对,回到 Live 再恢复;Failed 要人或上层逻辑介入。
// 本包不在失效时悄悄改用 REST 轮询,也不吞错:每次状态变化都经 [StreamOptions.OnState] 报出原因。
//
// 成交 / 流水(fills / ledger)是流水类:从调用方给的游标起逐条交付,不缺不重。订阅确认帧给出起点位号,
// 起点之内、断线与丢帧(lagged)期间的用 REST 按游标补拉(`after` + `epoch` + `upper`),与 WS 帧按位置拼接。
// 游标可以持久化([Subscription.Cursor]),进程重启后从它接着订。

// State 订阅的状态。
type State int32

const (
	StateSyncing State = iota
	StateLive
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateSyncing:
		return "syncing"
	case StateLive:
		return "live"
	case StateFailed:
		return "failed"
	}
	return fmt.Sprintf("State(%d)", int32(s))
}

var (
	// ErrEpochChanged 账本纪元变了(账本重置、账户号重排):私有订阅的本地状态与游标整体作废,订阅进入
	// [StateFailed]。调用方按新纪元从 REST 重建,再新建订阅;旧游标不能再用。
	ErrEpochChanged = errors.New("dexosv1: 账本纪元变了,本地状态与游标作废")
	// ErrConnLost WS 连接断了,正在重连。
	ErrConnLost = errors.New("dexosv1: WS 连接断开")
)

// WSError 服务端的错误帧(`{channel:"error", data:{code, message}}`)。
// Code 与 REST 错误体同一套稳定码(`GatewayBadRequest`、`GatewayUnavailable`、`GatewayRateLimited` 等)。
type WSError struct {
	Code    string
	Message string
}

func (e *WSError) Error() string {
	return fmt.Sprintf("dexosv1: WS 错误帧 %s: %s", e.Code, e.Message)
}

// LaggedError 服务端有一段帧没推给这条连接。快照类订阅随后会收到新快照;流水类本包按 REST 补拉。
type LaggedError struct {
	// Reason "buffer" = 推送跟不上、服务端丢了 Dropped 个事件;"render" = 服务端渲染不出流水帧
	// (执行历史读不出来、市场缺元数据),事件没丢,Dropped 为 0(v1.1 §4.9-1)。
	Reason  string
	Dropped uint64
	// Upper 补拉上界:位置不超过它的流水由 REST 补拉,之后的 WS 帧都在它之后。
	Upper string
}

func (e *LaggedError) Error() string {
	return fmt.Sprintf("dexosv1: 服务端有一段帧没推(%s,丢了 %d 个事件,补拉上界 %s)", e.Reason, e.Dropped, e.Upper)
}

// TradesGapError 公共成交(trades)在断线 / 丢帧期间缺了帧。公共成交没有续传游标,本包不补;
// 订阅随后回到 Live,只是中间那段没有交付。
type TradesGapError struct{ Cause error }

func (e *TradesGapError) Error() string {
	return fmt.Sprintf("dexosv1: 公共成交有缺口(不补): %v", e.Cause)
}

func (e *TradesGapError) Unwrap() error { return e.Cause }

// StateEvent 一次订阅状态变化。Err 是进入 Syncing / Failed 的原因(Live 时为 nil)。
type StateEvent struct {
	Sub   *Subscription
	State State
	Err   error
}

// StreamOptions WS 客户端的参数;零值字段取缺省。
type StreamOptions struct {
	// URL 缺省由 Client.BaseURL 推出:http → ws、https → wss,加 `/v1/ws`。
	URL string
	// Dialer 缺省 websocket.DefaultDialer。
	Dialer *websocket.Dialer
	// PingInterval 客户端心跳间隔,缺省 20s(服务端 60s 收不到客户端帧就关连接)。
	PingInterval time.Duration
	// ReadTimeout 这么久收不到任何帧(心跳的 pong 也算)就断开重连,缺省 50s。
	ReadTimeout time.Duration
	// ReconnectMin / ReconnectMax 重连的指数退避区间,缺省 500ms / 30s。
	// 快照失败后重订、补拉失败后重试也用同一个区间。
	ReconnectMin, ReconnectMax time.Duration
	// CatchUpWait 补拉的上界还没写进执行历史(400 GatewayHistoryIncomplete)时最多等多久,缺省 10s;
	// 等不到就把订阅置为 Syncing、稍后再试。
	CatchUpWait time.Duration
	// OnState 订阅状态变化。在 Run 的 goroutine 里调用,不要阻塞。
	OnState func(StateEvent)
	// OnConnError 连接层的错误(连不上、读写失败、心跳超时、协议错误)。在 Run 的 goroutine 里调用。
	OnConnError func(error)
}

// Stream 一条 v1 WS 连接。Subscribe* 可在 Run 之前或运行中从任意 goroutine 调用;
// 数据回调与 OnState 都在 Run 的 goroutine 里按序调用。
type Stream struct {
	c   *Client
	cat *Catalog
	opt StreamOptions
	url string

	mu      sync.Mutex
	subs    map[*Subscription]struct{}
	wake    chan struct{}
	due     chan func()
	quit    chan struct{}
	started atomic.Bool

	// 以下只在 Run 的 goroutine 里读写
	ctx     context.Context
	epoch   string
	gen     uint64
	conn    *websocket.Conn
	wmu     sync.Mutex
	nextID  int64
	pending map[int64]*Subscription
	routes  map[string]*Subscription
	// floor 本连接上 lagged 给过的最大补拉上界;hasFloor = 收到过。服务端此后不再推不超过它的流水帧,
	// 之后才确认的流水订阅也要补拉到它。
	floor    cpos
	hasFloor bool
}

// NewStream 建一个 WS 客户端。Client 用来补拉成交 / 流水;Catalog 用来把价量换成 tick / lot。
func NewStream(c *Client, cat *Catalog, opt StreamOptions) (*Stream, error) {
	if c == nil || cat == nil {
		return nil, errors.New("dexosv1: NewStream 需要 Client 与 Catalog")
	}
	if opt.URL == "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("dexosv1: BaseURL %q: %w", c.BaseURL, err)
		}
		switch u.Scheme {
		case "http":
			u.Scheme = "ws"
		case "https":
			u.Scheme = "wss"
		default:
			return nil, fmt.Errorf("dexosv1: BaseURL %q 推不出 WS 地址,请给 StreamOptions.URL", c.BaseURL)
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/v1/ws"
		opt.URL = u.String()
	}
	if opt.Dialer == nil {
		opt.Dialer = websocket.DefaultDialer
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&opt.PingInterval, 20*time.Second)
	def(&opt.ReadTimeout, 50*time.Second)
	def(&opt.ReconnectMin, 500*time.Millisecond)
	def(&opt.ReconnectMax, 30*time.Second)
	def(&opt.CatchUpWait, 10*time.Second)
	return &Stream{
		c: c, cat: cat, opt: opt, url: opt.URL,
		subs: map[*Subscription]struct{}{},
		wake: make(chan struct{}, 1),
		due:  make(chan func(), 256),
		quit: make(chan struct{}),
	}, nil
}

// Subscription 一个订阅。
type Subscription struct {
	st       *Stream
	key      string // 规范化的订阅参数;服务端错误帧回显的也是它
	route    string // 数据帧的路由键(频道 + 符号 / 账户)
	channel  string
	params   map[string]any
	snapshot bool // 快照类:订阅即推一帧 isSnapshot:true,之后整帧替换
	private  bool // 私有频道:帧带账本纪元

	state   atomic.Int32
	removed bool // 受 st.mu 保护

	// 以下只在 Run 的 goroutine 里读写
	gen     uint64 // 在哪一代连接上发过 subscribe;与 st.gen 不等 = 当前连接上还没订
	acked   bool
	retries int
	apply   func(f *wsFrame) error
	hist    *histSync
}

// Channel 订阅的频道名。
func (s *Subscription) Channel() string { return s.channel }

// State 当前状态(可从任意 goroutine 读)。
func (s *Subscription) State() State { return State(s.state.Load()) }

// Params 发给服务端的订阅参数(副本)。
func (s *Subscription) Params() map[string]any {
	out := make(map[string]any, len(s.params))
	for k, v := range s.params {
		out[k] = v
	}
	return out
}

// Cursor 流水订阅(fills / ledger)已交付到的位置;持久化它,下次从这里接着订。非流水订阅返回零值。
func (s *Subscription) Cursor() HistoryCursor {
	if s.hist == nil {
		return HistoryCursor{}
	}
	s.hist.mu.Lock()
	defer s.hist.mu.Unlock()
	return HistoryCursor{After: s.hist.lastID, Epoch: s.hist.epoch}
}

// Unsubscribe 退订。之后不再回调这个订阅。
func (s *Subscription) Unsubscribe() {
	s.st.mu.Lock()
	s.removed = true
	s.st.mu.Unlock()
	s.st.poke()
}

func (st *Stream) poke() {
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

// add 登记一个新订阅。同一组参数、同一个路由键只能有一个(服务端的帧不带订阅号,靠频道 + 符号 / 账户分发)。
func (st *Stream) add(sub *Subscription) (*Subscription, error) {
	key, err := canonical(sub.params)
	if err != nil {
		return nil, err
	}
	sub.key, sub.st = key, st
	st.mu.Lock()
	defer st.mu.Unlock()
	for o := range st.subs {
		if o.removed {
			continue
		}
		if o.key == key || o.route == sub.route {
			return nil, fmt.Errorf("dexosv1: 已经有同一个 %s 订阅(%s)", sub.channel, sub.route)
		}
	}
	st.subs[sub] = struct{}{}
	st.poke()
	return sub, nil
}

// canonical 订阅参数的规范形:JSON 往返后键有序、数字同形,与服务端错误帧里回显的参数可比。
func canonical(params map[string]any) (string, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	out, err := json.Marshal(m)
	return string(out), err
}

// accountRoute 私有频道的路由键:钱包 = 地址 + 子账户,金库身份 = 金库号(帧里只回显其一)。
func accountRoute(channel string, ref AccountRef) string {
	if ref.Vault != nil {
		return fmt.Sprintf("%s:vault:%d", channel, *ref.Vault)
	}
	return fmt.Sprintf("%s:%s:%d", channel, strings.ToLower(ref.Address.Hex()), ref.Subaccount)
}

func accountParams(channel string, ref AccountRef) map[string]any {
	if ref.Vault != nil {
		return map[string]any{"channel": channel, "vault": *ref.Vault}
	}
	return map[string]any{"channel": channel, "address": strings.ToLower(ref.Address.Hex()), "subaccount": ref.Subaccount}
}

// accountSub 私有频道订阅的公共部分;金库寻址同时带了地址 / 子账户的,在这里拒掉。
func accountSub(channel string, ref AccountRef, snapshot bool) (*Subscription, error) {
	if _, err := ref.values(); err != nil {
		return nil, err
	}
	return &Subscription{channel: channel, route: accountRoute(channel, ref), snapshot: snapshot, private: true,
		params: accountParams(channel, ref)}, nil
}

// Run 连上并保持连接,直到 ctx 结束(返回 ctx.Err())。断线按指数退避重连,重连后重订全部未失效的订阅。
// 只能调用一次。
func (st *Stream) Run(ctx context.Context) error {
	if !st.started.CompareAndSwap(false, true) {
		return errors.New("dexosv1: Stream.Run 只能调用一次")
	}
	defer close(st.quit)
	st.ctx = ctx
	backoff := st.opt.ReconnectMin
	for {
		var header http.Header
		if st.c.Host != "" {
			header = http.Header{"Host": {st.c.Host}}
		}
		conn, _, err := st.opt.Dialer.DialContext(ctx, st.url, header)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			st.connError(fmt.Errorf("dexosv1: 连接 %s: %w", st.url, err))
		} else {
			healthy, err := st.serve(ctx, conn)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			st.connError(err)
			st.lostAll(err)
			if healthy {
				backoff = st.opt.ReconnectMin
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, st.opt.ReconnectMax)
	}
}

func (st *Stream) connError(err error) {
	if err != nil && st.opt.OnConnError != nil {
		st.opt.OnConnError(err)
	}
}

// setState 改订阅状态并报出;状态没变且没有新原因就不报。
func (st *Stream) setState(sub *Subscription, s State, err error) {
	old := State(sub.state.Swap(int32(s)))
	if old == s && err == nil {
		return
	}
	if st.opt.OnState != nil {
		st.opt.OnState(StateEvent{Sub: sub, State: s, Err: err})
	}
}

// fail 订阅进入终态;服务端那边已订上的顺手退订。
func (st *Stream) fail(sub *Subscription, err error) {
	if sub.State() == StateFailed {
		return
	}
	st.setState(sub, StateFailed, err)
	if st.routes[sub.route] == sub {
		delete(st.routes, sub.route)
	}
	if sub.gen == st.gen && sub.acked {
		sub.acked = false
		if _, err := st.send("unsubscribe", sub.params); err != nil {
			st.connError(err)
		}
	}
}

// lostAll 连接断了:未失效的订阅都回到 Syncing。公共成交在断线期间的帧补不回来,报成缺口。
func (st *Stream) lostAll(cause error) {
	err := fmt.Errorf("%w: %w", ErrConnLost, cause)
	st.mu.Lock()
	subs := make([]*Subscription, 0, len(st.subs))
	for s := range st.subs {
		if !s.removed {
			subs = append(subs, s)
		}
	}
	st.mu.Unlock()
	for _, s := range subs {
		if s.State() == StateFailed {
			continue
		}
		if s.channel == "trades" && s.acked {
			st.setState(s, StateSyncing, &TradesGapError{Cause: err})
			continue
		}
		st.setState(s, StateSyncing, err)
	}
}

// after 在 d 之后于 Run 的 goroutine 里执行 fn;连接换代之后不再执行。
func (st *Stream) after(d time.Duration, fn func()) {
	g := st.gen
	time.AfterFunc(d, func() {
		select {
		case st.due <- func() {
			if st.gen == g {
				fn()
			}
		}:
		case <-st.quit:
		}
	})
}

// post 在 Run 的 goroutine 里执行 fn(不论连接换没换代)。
func (st *Stream) post(fn func()) {
	select {
	case st.due <- fn:
	case <-st.quit:
	}
}

// serve 跑一条连接直到出错。healthy = 这条连接上收到过帧(用来重置重连退避)。
func (st *Stream) serve(ctx context.Context, conn *websocket.Conn) (healthy bool, err error) {
	st.gen++
	st.conn = conn
	st.pending = map[int64]*Subscription{}
	st.routes = map[string]*Subscription{}
	st.hasFloor = false
	done := make(chan struct{})
	defer func() {
		close(done)
		_ = conn.Close()
	}()
	frames := make(chan []byte, 1024)
	rerr := make(chan error, 1)
	go st.reader(conn, frames, rerr, done)
	go st.pinger(conn, done)
	if err := st.syncSubs(); err != nil {
		return false, err
	}
	for {
		select {
		case <-ctx.Done():
			_ = st.write(conn, websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return healthy, ctx.Err()
		case raw := <-frames:
			healthy = true
			if err := st.handle(raw); err != nil {
				return healthy, err
			}
		case err := <-rerr:
			// 读到错误之前已经读进来的帧照常处理
			for {
				select {
				case raw := <-frames:
					if herr := st.handle(raw); herr != nil {
						return true, herr
					}
					continue
				default:
				}
				break
			}
			return healthy, fmt.Errorf("dexosv1: WS 读失败: %w", err)
		case <-st.wake:
			if err := st.syncSubs(); err != nil {
				return healthy, err
			}
		case fn := <-st.due:
			fn()
		}
	}
}

func (st *Stream) reader(conn *websocket.Conn, frames chan<- []byte, rerr chan<- error, done <-chan struct{}) {
	for {
		_ = conn.SetReadDeadline(time.Now().Add(st.opt.ReadTimeout))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			select {
			case rerr <- err:
			case <-done:
			}
			return
		}
		select {
		case frames <- msg:
		case <-done:
			return
		}
	}
}

func (st *Stream) pinger(conn *websocket.Conn, done <-chan struct{}) {
	t := time.NewTicker(st.opt.PingInterval)
	defer t.Stop()
	ping, _ := json.Marshal(map[string]string{"method": "ping"})
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if err := st.write(conn, websocket.TextMessage, ping); err != nil {
				// 写失败时读那一侧也会很快失败,由它触发重连
				return
			}
		}
	}
}

func (st *Stream) write(conn *websocket.Conn, kind int, data []byte) error {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(kind, data)
}

func (st *Stream) send(method string, params map[string]any) (int64, error) {
	st.nextID++
	raw, err := json.Marshal(map[string]any{"method": method, "params": params, "id": st.nextID})
	if err != nil {
		return 0, err
	}
	if err := st.write(st.conn, websocket.TextMessage, raw); err != nil {
		return 0, fmt.Errorf("dexosv1: WS 写失败: %w", err)
	}
	return st.nextID, nil
}

// syncSubs 让当前连接上的订阅与登记表一致:新订阅发 subscribe,已退订的发 unsubscribe。
func (st *Stream) syncSubs() error {
	st.mu.Lock()
	var todo []*Subscription
	for s := range st.subs {
		if s.removed {
			delete(st.subs, s)
			if s.gen == st.gen {
				if st.routes[s.route] == s {
					delete(st.routes, s.route)
				}
				if s.acked {
					if _, err := st.send("unsubscribe", s.params); err != nil {
						st.mu.Unlock()
						return err
					}
				}
			}
			continue
		}
		if s.gen != st.gen && s.State() != StateFailed {
			todo = append(todo, s)
		}
	}
	st.mu.Unlock()
	for _, s := range todo {
		if err := st.subscribe(s); err != nil {
			return err
		}
	}
	return nil
}

func (st *Stream) subscribe(s *Subscription) error {
	s.gen, s.acked = st.gen, false
	if s.hist != nil {
		s.hist.reset()
	}
	st.routes[s.route] = s
	id, err := st.send("subscribe", s.params)
	if err != nil {
		return err
	}
	st.pending[id] = s
	return nil
}

// resubscribe 在当前连接上退订再重订,换一帧新快照(快照失败、本地状态对不上时)。
func (st *Stream) resubscribe(s *Subscription) error {
	if s.gen == st.gen && s.acked {
		if _, err := st.send("unsubscribe", s.params); err != nil {
			return err
		}
	}
	return st.subscribe(s)
}

// retryLater 按订阅自己的退避在稍后重订。
func (st *Stream) retryLater(s *Subscription, fn func() error) {
	d := st.opt.ReconnectMin << min(s.retries, 16)
	d = min(d, st.opt.ReconnectMax)
	s.retries++
	st.after(d, func() {
		if s.State() == StateFailed || st.isRemoved(s) {
			return
		}
		if err := fn(); err != nil {
			st.connError(err)
		}
	})
}

// keyInUse 还有别的未退订订阅用着同一组参数。
func (st *Stream) keyInUse(s *Subscription) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	for o := range st.subs {
		if o != s && !o.removed && o.key == s.key {
			return true
		}
	}
	return false
}

func (st *Stream) isRemoved(s *Subscription) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return s.removed
}

// wsFrame 服务端帧的外层。
type wsFrame struct {
	Channel    string          `json:"channel"`
	Data       json.RawMessage `json:"data"`
	Seq        *uint64         `json:"seq"`
	Epoch      *string         `json:"epoch"`
	IsSnapshot bool            `json:"isSnapshot"`
	ID         *int64          `json:"id"`
}

func (st *Stream) handle(raw []byte) error {
	var f wsFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("dexosv1: WS 帧不是 JSON: %w", err)
	}
	switch f.Channel {
	case "pong":
		return nil
	case "subscribed":
		return st.onAck(&f)
	case "unsubscribed":
		return nil
	case "error":
		return st.onError(&f)
	case "lagged":
		return st.onLagged(&f)
	}
	if f.Epoch != nil && st.epoch != "" && *f.Epoch != st.epoch {
		st.epochChanged(*f.Epoch)
	}
	route, err := routeOf(f.Channel, f.Data)
	if err != nil {
		return err
	}
	sub := st.routes[route]
	if sub == nil || sub.State() == StateFailed || st.isRemoved(sub) {
		// 退订之后在途的帧
		return nil
	}
	if sub.hist != nil {
		err = st.histFrame(sub, &f)
	} else {
		err = sub.apply(&f)
	}
	if err != nil {
		var rs *resyncError
		if errors.As(err, &rs) {
			st.setState(sub, StateSyncing, rs.err)
			return st.resubscribe(sub)
		}
		// 帧解不开(价量不在刻度上、字段缺失):只这个订阅置 Syncing 报出原因,稍后重订
		st.setState(sub, StateSyncing, err)
		st.retryLater(sub, func() error { return st.resubscribe(sub) })
		return nil
	}
	if sub.hist != nil {
		return nil
	}
	if sub.channel == "orders" && !f.IsSnapshot && sub.State() != StateLive {
		return nil
	}
	sub.retries = 0
	st.setState(sub, StateLive, nil)
	return nil
}

// resyncError 本地状态与帧对不上:不猜,退订再重订换一帧新快照。
type resyncError struct{ err error }

func (e *resyncError) Error() string { return e.err.Error() }

// routeOf 数据帧 → 路由键,与 Subscribe* 登记时的一致。
func routeOf(channel string, data json.RawMessage) (string, error) {
	var h struct {
		Symbol     string  `json:"symbol"`
		Address    string  `json:"address"`
		Subaccount *uint32 `json:"subaccount"`
		Vault      *uint16 `json:"vault"`
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return "", fmt.Errorf("dexosv1: %s 帧的 data 不是对象: %w", channel, err)
	}
	switch channel {
	case "prices":
		return "prices", nil
	case "book", "bbo", "trades":
		return channel + ":" + strings.ToUpper(h.Symbol), nil
	case "orders", "fills", "account", "positions", "ledger":
		switch {
		case h.Vault != nil && h.Address == "" && h.Subaccount == nil:
			return accountRoute(channel, VaultAccount(*h.Vault)), nil
		case h.Vault == nil && h.Address != "" && h.Subaccount != nil:
			return fmt.Sprintf("%s:%s:%d", channel, strings.ToLower(h.Address), *h.Subaccount), nil
		}
		return "", fmt.Errorf("dexosv1: %s 帧的身份应为 address + subaccount 或 vault 之一", channel)
	}
	// 本包不订的频道(candles / funding / events):不该收到
	return "unsubscribed:" + channel, nil
}

func (st *Stream) onAck(f *wsFrame) error {
	if f.ID == nil {
		return errors.New("dexosv1: subscribed 帧没有 id")
	}
	sub := st.pending[*f.ID]
	delete(st.pending, *f.ID)
	if sub == nil {
		return nil
	}
	if f.Epoch == nil || *f.Epoch == "" {
		return errors.New("dexosv1: subscribed 帧没有 epoch")
	}
	if f.Seq == nil {
		return errors.New("dexosv1: subscribed 帧没有起点 seq")
	}
	switch {
	case st.epoch == "":
		st.epoch = *f.Epoch
	case *f.Epoch != st.epoch:
		st.epochChanged(*f.Epoch)
	}
	if sub.hist != nil && sub.hist.epoch != st.epoch {
		st.fail(sub, fmt.Errorf("%w(游标属于 %s,当前 %s)", ErrEpochChanged, sub.hist.epoch, st.epoch))
	}
	if sub.State() == StateFailed || st.isRemoved(sub) {
		// 确认到达之前已失效或已退订:服务端那边已经订上了,退订 —— 除非同一组参数已被新订阅接着用
		if st.keyInUse(sub) {
			return nil
		}
		_, err := st.send("unsubscribe", sub.params)
		return err
	}
	sub.acked = true
	switch {
	case sub.hist != nil:
		st.histStart(sub, *f.Seq)
	case sub.channel == "trades":
		sub.retries = 0
		st.setState(sub, StateLive, nil)
	}
	// 快照类:等紧跟着的快照帧
	return nil
}

func (st *Stream) onError(f *wsFrame) error {
	var d struct {
		Code         string          `json:"code"`
		Message      string          `json:"message"`
		Subscription json.RawMessage `json:"subscription"`
	}
	if err := json.Unmarshal(f.Data, &d); err != nil {
		return fmt.Errorf("dexosv1: 错误帧解析失败: %w", err)
	}
	werr := &WSError{Code: d.Code, Message: d.Message}
	if f.ID != nil {
		// 订阅请求被拒(参数不对、地址没开户、超过 200 个订阅):重订也一样,终态
		if sub := st.pending[*f.ID]; sub != nil {
			delete(st.pending, *f.ID)
			st.fail(sub, werr)
			return nil
		}
		return nil
	}
	if len(d.Subscription) > 0 && string(d.Subscription) != "null" {
		// 某个订阅取快照失败:稍后重订换快照
		var m map[string]any
		if err := json.Unmarshal(d.Subscription, &m); err != nil {
			return fmt.Errorf("dexosv1: 错误帧的 subscription 解析失败: %w", err)
		}
		key, err := canonical(m)
		if err != nil {
			return err
		}
		var sub *Subscription
		st.mu.Lock()
		for s := range st.subs {
			if s.key == key && !s.removed {
				sub = s
			}
		}
		st.mu.Unlock()
		if sub == nil || sub.State() == StateFailed {
			return nil
		}
		st.setState(sub, StateSyncing, werr)
		st.retryLater(sub, func() error { return st.resubscribe(sub) })
		return nil
	}
	return fmt.Errorf("dexosv1: 连接级错误帧: %w", werr)
}

func (st *Stream) onLagged(f *wsFrame) error {
	var d struct {
		Dropped uint64 `json:"dropped"`
		Epoch   string `json:"epoch"`
		Upper   string `json:"upper"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(f.Data, &d); err != nil {
		return fmt.Errorf("dexosv1: lagged 帧解析失败: %w", err)
	}
	if d.Epoch != "" && st.epoch != "" && d.Epoch != st.epoch {
		st.epochChanged(d.Epoch)
	}
	upper, err := parseCursor(d.Upper)
	if err != nil {
		return fmt.Errorf("dexosv1: lagged 帧的 upper %q: %w", d.Upper, err)
	}
	if !st.hasFloor || st.floor.less(upper) {
		st.floor, st.hasFloor = upper, true
	}
	lag := &LaggedError{Reason: d.Reason, Dropped: d.Dropped, Upper: d.Upper}
	for _, sub := range st.routes {
		if !sub.acked || sub.State() == StateFailed {
			continue
		}
		switch {
		case sub.hist != nil:
			st.histLagged(sub, lag, upper)
		case sub.channel == "trades":
			// render 只是流水帧渲染不出来,事件没丢,公共成交照常推完了
			if d.Reason != "render" {
				st.setState(sub, StateSyncing, &TradesGapError{Cause: lag})
				st.setState(sub, StateLive, nil)
			}
		default:
			// 快照类:服务端紧接着重发一帧 isSnapshot:true
			st.setState(sub, StateSyncing, lag)
		}
	}
	return nil
}

// epochChanged 账本纪元变了:私有订阅全部失效(终态),公共订阅不受影响。
func (st *Stream) epochChanged(epoch string) {
	st.epoch = epoch
	st.mu.Lock()
	subs := make([]*Subscription, 0, len(st.subs))
	for s := range st.subs {
		if s.private && !s.removed {
			subs = append(subs, s)
		}
	}
	st.mu.Unlock()
	for _, s := range subs {
		st.fail(s, ErrEpochChanged)
	}
}
