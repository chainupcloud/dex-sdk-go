package dexos

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Session 绑定了身份的交易会话 —— **初始化之后不用再传账户号和签名者**。
//
// 对照 Hyperliquid:
//
//	# HL:私钥 + 主钱包地址
//	exchange = Exchange(Account.from_key(API_KEY), URL, account_address=MAIN_ADDR)
//
//	// 这里:只要私钥
//	s, _ := dexos.New(ctx, URL, API_KEY)
//
// 一把代理可被多个账户授权；有歧义时必须 ForAccount 指定，不能猜主账户。
//
// nonce 由会话自己维护。它是 **agent 地址自己的**计数器,与 master 的 nonce
// 无关 —— 这是接入时最常见的坑,所以干脆不暴露。
type Session struct {
	Client  *Client
	Config  *Config
	Agent   *Signer
	Account uint32
	// ValidUntilMs 授权到期时刻(0 = 永不过期)。到期后所有写操作会被拒。
	ValidUntilMs uint64

	mu      sync.Mutex
	nonce   uint64
	blocked error
}

// ErrSessionBlocked 表示上次写入尚未核对，禁止继续消耗 nonce。
// 创建新 Session 或重读 nonce 都不是恢复原结果的证据；调用方须持久化未决请求。
var ErrSessionBlocked = errors.New("dexos: 上次写入结果尚未核对，会话禁止继续写入")

// ErrAgentNotAuthorized 这把 API 钱包没有被任何账户授权(或授权已被撤销)。
//
// 不是「密钥不对」——密钥格式没问题,只是交易所这边不认它代表谁。
// 去前端「API 钱包」里重新生成并授权一把。
var ErrAgentNotAuthorized = errors.New("dexos: 这把 API 钱包未被授权(去前端重新授权)")

// ErrAmbiguousAccount 这把 API 钱包被**多个**账户授权,而调用方没指明代谁。
//
// 不替调用方猜:猜错的表现是订单落到另一个子账户上 —— 仓位、保证金、风险
// 全记在别处,而**没有任何报错**。宁可在初始化这一刻失败,也不要在第 300 笔
// 成交之后才被人发现。
//
// 用 ForAccount 指明:
//
//	s, err := dexos.New(ctx, url, apiKey, dexos.ForAccount(7))
var ErrAmbiguousAccount = errors.New("dexos: 这把 API 钱包被多个账户授权,请用 ForAccount 指明代哪个")

// Grant 一条授权:某个账户授权了这把 API 钱包。
type Grant struct {
	Master       uint32 `json:"master"`
	ValidUntilMs string `json:"validUntilMs"`
	Expired      bool   `json:"expired"`
}

// Option 调整 New 的行为。
type Option func(*newOpts)

type newOpts struct {
	account    uint32
	hasAccount bool
}

// ForAccount 指明这次会话代哪个账户。
//
// 只授权了一个账户时不必用它 —— 那种情况下账户是**能推出来的**,让人再传一遍
// 只是给抄错留位置。被多个账户授权时则必须用,否则 New 返回 ErrAmbiguousAccount。
func ForAccount(id uint32) Option {
	return func(o *newOpts) { o.account, o.hasAccount = id, true }
}

// New 初始化一个交易会话。**这是大多数接入方唯一需要调用的构造函数。**
//
//	s, err := dexos.New(ctx, "http://node:17807", "0x<API 钱包私钥>")
//	s.Place(ctx, dexos.BatchOrder{Market: 0, Side: dexos.Buy, Price: 70000, Lots: 1, TIF: dexos.GTC})
//
// 它做三件事,每一件都是接入时容易出错的地方:
//
//  1. 向 /config 发现链参数并核对 codec 版本(填错 → 每笔签名 401,而行情正常)
//  2. 由 API 钱包地址反查它被哪些账户授权(不必再被带外告知账户号)
//  3. 读取并维护 agent 自己的 nonce(与 master 的 nonce 混用是最常见的坑)
//
// # 一把 key 被多个账户授权时
//
// 主账户与每个子账户是**分别**授权同一把 API 钱包的(状态机按 (账户, agent)
// 对记账,撤销其中一个不影响其余)。这时账户号就不再是能推出来的,必须指明:
//
//	s, err := dexos.New(ctx, url, apiKey, dexos.ForAccount(subAccountID))
//
// 不指明会得到 ErrAmbiguousAccount —— 这里刻意不挑一个默认值。
func New(ctx context.Context, baseURL, apiKeyHex string, opts ...Option) (*Session, error) {
	var o newOpts
	for _, f := range opts {
		f(&o)
	}
	agent, err := NewSigner(apiKeyHex)
	if err != nil {
		return nil, fmt.Errorf("dexos: API 钱包私钥无法解析:%w", err)
	}
	c, cfg, err := Connect(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	resp, err := c.AgentGrants(ctx, agent.Address())
	if err != nil {
		return nil, err
	}
	g, err := pick(resp.Grants, o)
	if err != nil {
		return nil, err
	}
	until, err := strconv.ParseUint(g.ValidUntilMs, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("dexos: 授权到期值不合法: %w", err)
	}
	if until != 0 && uint64(time.Now().UnixMilli()) >= until {
		return nil, ErrAgentExpired
	}
	return &Session{
		Client:       c,
		Config:       cfg,
		Agent:        agent,
		Account:      g.Master,
		ValidUntilMs: until,
		nonce:        resp.NextNonce,
	}, nil
}

// pick 从授权列表里选出这次会话代哪个账户。
//
// 过期的授权**先剔除再判定**:一把 key 授权了主账户(已过期)与一个子账户
// (仍有效)时,正确行为是直接用那个子账户,而不是报「有歧义」让人去指明一个
// 本来就用不了的账户。
func pick(grants []Grant, o newOpts) (Grant, error) {
	live := grants[:0:0]
	for _, g := range grants {
		if !g.Expired {
			live = append(live, g)
		}
	}
	if o.hasAccount {
		for _, g := range live {
			if g.Master == o.account {
				return g, nil
			}
		}
		// 指定的账户没授权过这把 key。区分「压根没授权」与「授权已过期」——
		// 前者要去前端授权,后者要去续期,处置不同。
		for _, g := range grants {
			if g.Master == o.account {
				return Grant{}, fmt.Errorf(
					"dexos: 账户 %d 对这把 API 钱包的授权已过期 —— 去前端续期或重新生成", o.account)
			}
		}
		return Grant{}, fmt.Errorf(
			"dexos: 账户 %d 没有授权这把 API 钱包(已授权的:%s)", o.account, masters(live))
	}
	switch len(live) {
	case 0:
		if len(grants) > 0 {
			return Grant{}, fmt.Errorf("dexos: 这把 API 钱包的授权已全部过期 —— 去前端续期或重新生成")
		}
		return Grant{}, ErrAgentNotAuthorized
	case 1:
		return live[0], nil
	default:
		return Grant{}, fmt.Errorf("%w(已授权的:%s)", ErrAmbiguousAccount, masters(live))
	}
}

func masters(g []Grant) string {
	if len(g) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(g))
	for _, x := range g {
		parts = append(parts, strconv.FormatUint(uint64(x.Master), 10))
	}
	return strings.Join(parts, ", ")
}

// Resync 重新从服务端取 nonce。
//
// 仅在没有未决写入时使用；上次写入失败后本方法也被阻止。
// **同一把钥匙多进程并发是不支持的** —— nonce 是单调计数器,两个进程会互相
// 打架;要并发就给每个进程一把自己的 API 钱包(同一个账户可以授权多把)。
func (s *Session) Resync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blocked != nil {
		return s.blocked
	}
	n, err := s.Client.AgentNonce(ctx, s.Account, s.Agent.Address())
	if err != nil {
		return err
	}
	s.nonce = n
	return nil
}

// 写操作在本会话内串行。**结论未知才锁定**;终局的业务拒绝不锁。
//
// 三类结果按「服务端计数器有没有前进 / 结论是否确定」分开处理:
//
//   - 成功:nonce 已消耗 → 本地 ++。
//   - *RejectedError(nonce 之后的业务拒绝,如 InsufficientMargin / MarketNotTrading):
//     内核按 验签 → 归属 → 注册 → scope → 有效期 → nonce → 业务 检查
//     (crates/kernel/src/engine/authn.rs),拒在业务这一步时计数器已写入且 apply 不回滚
//     → 本地 ++,**不锁**:结论是终局的,没有未决风险,锁住只会让一笔保证金不足停掉整个做市进程。
//   - *RejectedError(nonce 之前的拒绝,如 AgentScopeViolation / UnknownAgent):
//     计数器没动 → 本地不变,**不锁**:同样是终局结论。
//   - NonceMismatch / ErrExecutionPending / 传输失败 / ErrIncompleteBatch:本地计数器
//     不可信,或原请求结论未知 → **锁定**(含 Resync)。重读 nonce 不能证明原请求执行结果;
//     调用方须持久化未决请求,按 API 代理地址持有独占租约。
func (s *Session) write(
	ctx context.Context,
	f func(nonce uint64) ([]EventEnvelope, error),
) ([]EventEnvelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blocked != nil {
		return nil, s.blocked
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.ValidUntilMs != 0 && uint64(time.Now().UnixMilli()) >= s.ValidUntilMs {
		return nil, ErrAgentExpired
	}
	ev, err := f(s.nonce)
	if err == nil {
		s.nonce++
		return ev, nil
	}
	var re *RejectedError
	if errors.As(err, &re) && re.Reason != ReasonNonceMismatch {
		if !rejectedBeforeNonce[re.Reason] {
			s.nonce++ // 业务拒绝:认证层已过,nonce 已消耗且不会退回
		}
		return ev, err // 终局结论,不锁
	}
	var partial *BatchOutcomeError
	if errors.As(err, &partial) {
		s.nonce++ // 批次已执行、逐项结局齐全:nonce 已消耗,终局结论,不锁
		return ev, err
	}
	s.blocked = fmt.Errorf("%w (nonce=%d): %w", ErrSessionBlocked, s.nonce, err)
	return ev, s.blocked
}

// Place 下单(一张或多张,逐项独立判定)。
func (s *Session) Place(ctx context.Context, orders ...BatchOrder) ([]EventEnvelope, error) {
	return s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		return s.Client.BatchPlace(ctx, s.Agent, s.Account, orders, n)
	})
}

// Cancel 撤单。
func (s *Session) Cancel(ctx context.Context, market uint16, seqs ...uint64) ([]EventEnvelope, error) {
	return s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		return s.Client.BatchCancel(ctx, s.Agent, s.Account, market, seqs, n)
	})
}

// Replace **原子换单**:撤旧挂新落在同一个状态机转移里,不存在没有报价的中间窗口。
// 做市刷新报价用它,不要用 Cancel + Place。
func (s *Session) Replace(
	ctx context.Context, market uint16, cancelSeqs []uint64, orders []BatchOrder,
) ([]EventEnvelope, error) {
	result, err := s.ReplaceWithReceipt(ctx, market, cancelSeqs, orders)
	return result.Events(), err
}

// ReplaceWithReceipt 额外返回原请求身份与已解析回执；仍复用同一 nonce 锁与失败锁定。
// 因已有未决写而被阻断时不创建新身份，不会把新调用包装成旧请求恢复。
func (s *Session) ReplaceWithReceipt(ctx context.Context, market uint16, cancelSeqs []uint64, orders []BatchOrder) (BatchSubmission, error) {
	var result BatchSubmission
	_, err := s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		var sendErr error
		result, sendErr = s.Client.BatchReplaceWithReceipt(ctx, s.Agent, s.Account, market, cancelSeqs, orders, n)
		return result.Events(), sendErr
	})
	return result, err
}

// SetLeverage 设置该市场的初始保证金率(ppm):200000 = 20% = 5×。
//
// 参数是 **ppm 不是倍数** —— 传 5 得到的是 InvalidLeverage(5 ppm 低于任何市场的
// 基础保证金率),而拒因里看不出是单位错了。用 ImfPpmForLeverage(5) 换算。
// 只能调高保证金要求(降杠杆),低于市场基础 IMF 会被拒。
func (s *Session) SetLeverage(ctx context.Context, market uint16, customImfPpm uint32) ([]EventEnvelope, error) {
	return s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		return s.Client.SetLeverage(ctx, s.Agent, s.Account, market, customImfPpm, n)
	})
}

// ── 其余 agent 白名单命令,同样由会话管 nonce ──
//
// 这些在 Client 上都有,但 Client 版要调用方自己传 agent nonce —— 正是 Session
// 存在要挡掉的坑。少包一条,用到那条的人就得回去手工管计数器。

// PlaceGTT 带过期时刻的单张下单(BatchPlace 的单项没有 good_til)。
func (s *Session) PlaceGTT(ctx context.Context, o BatchOrder, goodTil time.Time) ([]EventEnvelope, error) {
	return s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		return s.Client.PlaceOrderGTT(ctx, s.Agent, s.Account, o.Market, o.Side, o.Price, o.Lots,
			o.TIF, o.ReduceOnly, goodTil, n)
	})
}

// Modify 改单 = 撤旧建新,**丢失时间优先**;要保队列位置用 Replace。
func (s *Session) Modify(ctx context.Context, market uint16, seq uint64, price uint32, lots uint64,
	tif TimeInForce, reduceOnly bool, goodTil time.Time) ([]EventEnvelope, error) {
	return s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		return s.Client.ModifyOrder(ctx, s.Agent, s.Account, NewOrderID(market, seq),
			price, lots, tif, reduceOnly, goodTil, n)
	})
}

// PlaceConditional 下条件单(止损 / 止盈触发)。
func (s *Session) PlaceConditional(ctx context.Context, c Conditional) ([]EventEnvelope, error) {
	return s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		return s.Client.PlaceConditional(ctx, s.Agent, s.Account, c, n)
	})
}

// CancelConditional 撤销一张未触发的条件单。
func (s *Session) CancelConditional(ctx context.Context, market uint16, seq uint64) ([]EventEnvelope, error) {
	return s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		return s.Client.CancelConditional(ctx, s.Agent, s.Account, NewOrderID(market, seq), n)
	})
}

// PlaceTwap 下 TWAP 母单。
func (s *Session) PlaceTwap(ctx context.Context, t Twap) ([]EventEnvelope, error) {
	return s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		return s.Client.PlaceTwap(ctx, s.Agent, s.Account, t, n)
	})
}

// CancelTwap 撤销 TWAP 母单:已成交的留下,未执行的切片停掉。
func (s *Session) CancelTwap(ctx context.Context, market uint16, seq uint64) ([]EventEnvelope, error) {
	return s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		return s.Client.CancelTwap(ctx, s.Agent, s.Account, NewOrderID(market, seq), n)
	})
}

// PlaceTpslPair 下止盈 / 止损配对(OCO)。
func (s *Session) PlaceTpslPair(ctx context.Context, p TpslPair) ([]EventEnvelope, error) {
	return s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		return s.Client.PlaceTpslPair(ctx, s.Agent, s.Account, p, n)
	})
}

// ScheduleCancel 断线保护(dead man's switch):到点自动撤光挂单。
func (s *Session) ScheduleCancel(ctx context.Context, at time.Time) ([]EventEnvelope, error) {
	return s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		return s.Client.ScheduleCancel(ctx, s.Agent, s.Account, at, n)
	})
}

// ── 只读直通(不消耗 nonce)──

func (s *Session) Risk(ctx context.Context) (*Risk, error) {
	return s.Client.Risk(ctx, s.Account)
}

func (s *Session) Orders(ctx context.Context, market uint16) ([]Order, error) {
	return s.Client.Orders(ctx, s.Account, market)
}

func (s *Session) OrderSeqs(ctx context.Context, market uint16) ([]uint64, error) {
	return s.Client.OrderSeqs(ctx, s.Account, market)
}

func (s *Session) Markets(ctx context.Context) ([]Market, error) { return s.Client.Markets(ctx) }

func (s *Session) Book(ctx context.Context, market uint16, depth int) (*Book, error) {
	return s.Client.Book(ctx, market, depth)
}

// Subscribe 事件流,默认只收本账户与指定市场的事件。
func (s *Session) Subscribe(ctx context.Context, markets ...uint16) (*Stream, error) {
	opts := []SubscribeOption{WithAccounts(s.Account)}
	if len(markets) > 0 {
		opts = append(opts, WithMarkets(markets...))
	}
	return s.Client.Subscribe(ctx, opts...)
}
