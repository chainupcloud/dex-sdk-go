package dexosv1

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 成对成交(`POST /v1/orders/paired`,dex-os docs/specs/paired-trade.md):一次请求带挂单腿 M 与吃单腿 T,
// 执行层在一次处理里让两腿以价格 P、数量 Q 互相成交,或者两腿都不生效。两腿都不进订单簿、不碰别人的挂单;
// 成交那一刻 P 必须严格落在买一与卖一之间。只在测试链上可用,网关只对白名单(`PAIRED_TRADE_ACCOUNTS`)里的账户开放,
// 名单为空时接口不存在(404)。
//
// 每条腿是一个 `sigVersion: 2` 信封:签 Action(账户, 编号, keccak(PairedTradeLeg), nonce, expiry),内层写明角色、
// 市场、价量、挂单方向、对手账户与 nowMs。nonce 是签名者的窗口 nonce(见 [WindowNonce]),两条腿各取一个。

// PairedSession 一把 API 钱包代两个账户(挂单方 Maker、吃单方 Taker)做成对成交的会话。并发安全:同一会话可以同时有多笔成交在途,
// 锁只包住检查、取 nonce、签名与写前日志,发送和等结论不持锁。
//
// 窗口 nonce 的窗口只记最近 100 个:在途的一笔到达时,若后签的请求已让窗口里堆满更大的 nonce,它会被拒成 NonceMismatch
// (验签阶段拒绝,没执行,nonce 没消耗,可以换新 nonce 重发)。[PairedSession.MaxNonceSpan] 限制这种乱序的幅度:最老的在途请求
// 与新请求相隔达到该个数的 nonce 时,新请求等最老的收到结论再签。
//
// 结局分两类:执行层拒绝(422,如 PairedTradeOutsideSpread、被拒那条腿的 InsufficientMargin,两腿都没生效)
// 与定序前的拒绝(400 / 401 / 403 / 404 / 429 …)是终局结论,原样返回 [*Error],会话照常可用 —— 窗口 nonce
// 没有计数器,一次被拒不会让后面错位。202([ErrPending])、连上之后的传输失败、5xx、回执对不上是结论未知:
// 会话锁定([ErrSessionBlocked]),此后不再签新单,已在途的各自收结论;[PairedSession.Pending] 给出所有结论未知的请求
// (挂单腿的请求身份),用它们查 [Client.Request] 核对之后再新建会话。**不要重发**:每次成交都签新的 nonce,重发就是另一笔成交。
type PairedSession struct {
	Client *Client
	Agent  *dexos.Signer
	Maker  AccountRef
	Taker  AccountRef
	// ValidUntil 两条腿授权里较早的到期时刻;零值 = 不过期(或 owner 本人签)。
	ValidUntil time.Time
	// MaxNonceSpan 最老的在途请求与新请求之间最多相隔的 nonce 个数(每笔成交两个);0 = [DefaultPairedNonceSpan]。
	MaxNonceSpan int

	domain  dexos.Domain
	now     func() time.Time
	nonces  *windowNonces
	mu      sync.Mutex
	changed chan struct{}
	seq     uint64
	flight  map[uint64]struct{}
	blocked error
	pending []RequestIdentity
}

// DefaultPairedNonceSpan 窗口 100 减去留给时钟取整与同一签名者其他写的余量。
const DefaultPairedNonceSpan = 60

// NewPairedSession 读连接参数与两条腿账户的授权列表,建会话。maker / taker 是钱包寻址(sigVersion 2 不支持金库身份),
// 不能是同一个账户。agent 就是某条腿的 owner 时那条腿由 owner 本人签,不查授权;否则要有按账户授权或位图覆盖该编号的
// owner 级授权(与执行层同口径):没有 → ErrAgentNotAuthorized,都过期了 → ErrAgentExpired。
func NewPairedSession(ctx context.Context, c *Client, agent *dexos.Signer, maker, taker AccountRef) (*PairedSession, error) {
	for _, r := range []AccountRef{maker, taker} {
		if r.Vault != nil {
			return nil, fmt.Errorf("dexosv1: 成对成交是 sigVersion 2,不支持金库身份(%s)", r)
		}
		if r.Subaccount > math.MaxUint16 {
			return nil, fmt.Errorf("dexosv1: 子账户编号 %d 超出 0..65535", r.Subaccount)
		}
	}
	if maker == taker {
		return nil, fmt.Errorf("dexosv1: 成对成交的两条腿是同一个账户 %s", maker)
	}
	ex, err := c.Exchange(ctx)
	if err != nil {
		return nil, err
	}
	d, err := ex.SigningDomain()
	if err != nil {
		return nil, err
	}
	p := &PairedSession{Client: c, Agent: agent, Maker: maker, Taker: taker, domain: d, now: time.Now, nonces: &processNonces, changed: make(chan struct{}), flight: map[uint64]struct{}{}}
	for _, r := range []AccountRef{maker, taker} {
		if agent.Address() == r.Address {
			continue
		}
		agents, err := c.Agents(ctx, r)
		if err != nil {
			return nil, err
		}
		until, err := grantOf(agents, agent.Address(), r.Subaccount)
		if errors.Is(err, ErrAgentNotAuthorized) {
			return nil, fmt.Errorf("%w: %s 不能代 %s 签", ErrAgentNotAuthorized, agent.Address().Hex(), r)
		} else if err != nil {
			return nil, err
		}
		if !until.IsZero() && (p.ValidUntil.IsZero() || until.Before(p.ValidUntil)) {
			p.ValidUntil = until
		}
	}
	return p, nil
}

// PairedTrade 一笔成对成交:挂单腿以 MakerSide 方向、吃单腿以反方向,按 Price(tick)成交 Lots(lot)。
// 一期只支持永续市场(现货是 422 SpotUnsupported)。
type PairedTrade struct {
	Market    *Market
	MakerSide dexos.Side
	Price     uint32
	Lots      uint64
	// ExpiresAt 两条腿签名的过期时刻(进签名);零值 = 不过期。
	ExpiresAt time.Time
}

func (t PairedTrade) check() error {
	switch {
	case t.Market == nil:
		return errors.New("dexosv1: 成对成交缺市场")
	case t.MakerSide != dexos.Buy && t.MakerSide != dexos.Sell:
		return fmt.Errorf("dexosv1: 未知的挂单方向 %d", t.MakerSide)
	case t.Price == 0 || t.Lots == 0:
		return fmt.Errorf("dexosv1: %s 成对成交的价格与数量都必须大于 0", t.Market.Symbol)
	}
	return nil
}

// PairedTradeResult 成对成交的结论(只有一种成功形态:吃单腿恰好吃掉挂单腿)。
type PairedTradeResult struct {
	// Receipt.Request 是挂单腿的签名身份(dex-os 对成对成交请求身份的定义)。
	Receipt
	// Status 恒为 "filled"。
	Status string
	Symbol string
	// Kind 成交市场的类型(回执带了 kind 就已核对过与请求的市场一致)。
	Kind  string
	Price uint32
	Lots  uint64
	// MakerOrderID / TakerOrderID 两条腿的订单号(订单序号)。
	MakerOrderID uint64
	TakerOrderID uint64
	// AvgPrice / Fills 吃单腿的成交均价与逐笔成交;AvgPrice nil = 回执没给。
	AvgPrice *uint32
	Fills    []ReceiptFill
	// MakerFills 挂单腿的逐笔成交,与 Fills 同一 FillID;Fee 是挂单腿的协议费(负 = 返佣),
	// 与挂单方 `/v1/account/fills` 里同一笔相同。
	MakerFills []ReceiptFill
	// LegFees 两条腿在这次成交上实付的手续费;nil = 网关没给(dex-os 加 legFees 之前的版本)。
	LegFees *PairedLegFees
}

// PairedLegFees 成对成交两条腿各自实付的手续费(dex-os docs/specs/paired-trade.md §4.5)。
type PairedLegFees struct {
	Maker PairedLegFee
	Taker PairedLegFee
}

// PairedLegFee 一条腿实付的手续费,都是按 FeeToken 小数位的十进制串,正 = 付出、负 = 返佣。
// Fee = 该腿账户在同一 seq 的流水里 taker_fee / maker_fee / maker_rebate / deployer_fee / builder_fee 之和取负
// = Protocol + Deployer;Protocol = 这条腿逐笔成交的 Fee 之和;Deployer 只有吃单腿会有。
// FeeToken:合约 = 结算 token;现货 = 这条腿收到的币(买腿 base、卖腿 quote)。
type PairedLegFee struct {
	Fee      string
	FeeToken string
	Protocol string
	Deployer string
}

// PairedTradeIntent 一次成对成交在发出之前就确定的全部内容。两条腿的签名由 Domain、Maker / Taker、Trade、NowMs
// 与各自的 nonce 唯一确定(内层 [dexos.EncodePairedTradeLeg],摘要 [dexos.Domain.ActionHash]),调用方可以独立重算核对。
type PairedTradeIntent struct {
	// Request 挂单腿的签名身份;结论未知时凭它查 [Client.Request]。
	Request    *RequestIdentity
	Domain     dexos.Domain
	Maker      AccountRef
	Taker      AccountRef
	Trade      PairedTrade
	NowMs      uint64
	MakerNonce uint64
	TakerNonce uint64
}

// PairedTradeJournal 成对成交的写前日志,规则同 [ReplaceJournal],但回调可能并发调用,实现须并发安全:BeforeSend 在会话锁里调用,AfterReceive 在锁外调用。BeforeSend 先把 intent
// 提交到本地存储再返回 nil;返回包着 [ErrRequestDeclined] 的错误 = 不发、不锁,其余错误即锁定会话。AfterReceive 记下结论
// (result 非 nil 时 err 为 nil),返回错误即锁定会话。
type PairedTradeJournal interface {
	BeforeSend(ctx context.Context, intent PairedTradeIntent) error
	AfterReceive(ctx context.Context, intent PairedTradeIntent, result *PairedTradeResult, err error) error
}

// Trade 成交一笔。两条腿各取一个窗口 nonce、各签一次,发 `POST /v1/orders/paired` 并等结论。
func (p *PairedSession) Trade(ctx context.Context, t PairedTrade) (*PairedTradeResult, error) {
	return p.trade(ctx, t, nil)
}

// TradeJournaled 同 [PairedSession.Trade],另在发送前、收到结论后各调一次 journal(见 [PairedTradeJournal])。
func (p *PairedSession) TradeJournaled(ctx context.Context, t PairedTrade, journal PairedTradeJournal) (*PairedTradeResult, error) {
	if journal == nil {
		return nil, errors.New("dexosv1: 缺写前日志")
	}
	return p.trade(ctx, t, journal)
}

// Pending 锁定时结论未知的所有成交(挂单腿的请求身份,按发生先后);没有为 nil。
func (p *PairedSession) Pending() []RequestIdentity {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pending) == 0 {
		return nil
	}
	return append([]RequestIdentity(nil), p.pending...)
}

func (p *PairedSession) nonceSpan() uint64 {
	if p.MaxNonceSpan > 0 {
		return uint64(p.MaxNonceSpan)
	}
	return DefaultPairedNonceSpan
}

func (p *PairedSession) spanFull() bool {
	if len(p.flight) == 0 {
		return false
	}
	oldest := p.seq
	for s := range p.flight {
		oldest = min(oldest, s)
	}
	return p.seq+2-oldest > p.nonceSpan()
}

func (p *PairedSession) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *PairedSession) admit(ctx context.Context) (uint64, uint64, error) {
	for {
		if p.blocked != nil {
			return 0, 0, p.blocked
		}
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		now := p.now()
		if !p.ValidUntil.IsZero() && !now.Before(p.ValidUntil) {
			return 0, 0, ErrAgentExpired
		}
		if !p.spanFull() {
			start := p.seq
			p.seq += 2
			p.flight[start] = struct{}{}
			return start, uint64(now.UnixMilli()), nil
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		p.mu.Lock()
	}
}

func (p *PairedSession) trade(ctx context.Context, t PairedTrade, journal PairedTradeJournal) (*PairedTradeResult, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	start, now, err := p.admit(ctx)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	in, body, err := p.prepare(ctx, t, now, journal)
	p.mu.Unlock()
	var out *PairedTradeResult
	if err == nil {
		out, err = p.post(ctx, t.Market, body, in.Request)
		if journal != nil {
			if jerr := journal.AfterReceive(ctx, in, out, err); jerr != nil {
				if err != nil {
					jerr = fmt.Errorf("%w(发送结论: %v)", jerr, err)
				}
				out, err = nil, &journalError{err: jerr}
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.flight, start)
	p.notifyLocked()
	if err == nil || pairedDefinite(err) {
		return out, err
	}
	if in.Request == nil {
		return nil, err
	}
	if p.blocked == nil {
		p.blocked = fmt.Errorf("%w(requestId %s): %w", ErrSessionBlocked, in.Request.RequestID, err)
	}
	p.pending = append(p.pending, *in.Request)
	return nil, p.blocked
}

// pairedDefinite 一次成对成交的错误是不是终局结论:没发出、定序前的拒绝、带拒因的执行层拒绝。写前日志自身的失败不是。
func pairedDefinite(err error) bool {
	var je *journalError
	if errors.As(err, &je) {
		return false
	}
	if errors.Is(err, ErrNotSent) {
		return true
	}
	var e *Error
	return errors.As(err, &e) && (notSequenced(e) || e.Status == http.StatusUnprocessableEntity && e.Code != "")
}

type wirePairedLeg struct {
	SigVersion int    `json:"sigVersion"`
	Account    string `json:"account"`
	Subaccount uint32 `json:"subaccount"`
	Signer     string `json:"signer"`
	Nonce      uint64 `json:"nonce"`
	ExpiresAt  uint64 `json:"expiresAt,omitempty"`
	Signature  string `json:"signature"`
}

type wirePairedReq struct {
	Symbol    string        `json:"symbol"`
	Kind      string        `json:"kind,omitempty"`
	Price     string        `json:"price"`
	Size      string        `json:"size"`
	MakerSide string        `json:"makerSide"`
	NowMs     uint64        `json:"nowMs"`
	Maker     wirePairedLeg `json:"maker"`
	Taker     wirePairedLeg `json:"taker"`
}

// prepare 取两个 nonce、签两条腿、记写前日志;持会话锁调用。返回的意图在签名之后带请求身份。
func (p *PairedSession) prepare(ctx context.Context, t PairedTrade, now uint64, journal PairedTradeJournal) (PairedTradeIntent, wirePairedReq, error) {
	agent := p.Agent.Address()
	in := PairedTradeIntent{Domain: p.domain, Maker: p.Maker, Taker: p.Taker, Trade: t, NowMs: now,
		MakerNonce: p.nonces.next(agent, now), TakerNonce: p.nonces.next(agent, now)}
	var expiry uint64
	if !t.ExpiresAt.IsZero() {
		expiry = uint64(t.ExpiresAt.UnixMilli())
	}
	body := wirePairedReq{Symbol: t.Market.Symbol, Kind: t.Market.Kind, Price: t.Market.Scale.Price(t.Price), Size: t.Market.Scale.Size(t.Lots),
		MakerSide: t.MakerSide.String(), NowMs: now}
	var makerDigest []byte
	for _, leg := range []struct {
		taker     bool
		me, other AccountRef
		nonce     uint64
		out       *wirePairedLeg
	}{{false, p.Maker, p.Taker, in.MakerNonce, &body.Maker}, {true, p.Taker, p.Maker, in.TakerNonce, &body.Taker}} {
		inner := dexos.EncodePairedTradeLeg(dexos.PairedTradeLeg{Taker: leg.taker, Market: t.Market.SigningMarketID, MakerSide: t.MakerSide,
			Price: t.Price, Lots: t.Lots, Counterparty: leg.other.Address, CounterpartySubaccount: uint16(leg.other.Subaccount)}, now)
		digest := p.domain.ActionHash(leg.me.Address, uint16(leg.me.Subaccount), inner, leg.nonce, expiry)
		sig, err := p.Agent.SignHashHex(digest)
		if err != nil {
			return in, body, fmt.Errorf("%w: %w", ErrNotSent, err)
		}
		*leg.out = wirePairedLeg{SigVersion: 2, Account: leg.me.Address.Hex(), Subaccount: leg.me.Subaccount, Signer: agent.Hex(),
			Nonce: leg.nonce, ExpiresAt: expiry, Signature: sig}
		if !leg.taker {
			makerDigest = digest
		}
	}
	id := newIdentity(agent, "signer:"+agent.Hex(), in.MakerNonce, makerDigest, nil)
	in.Request = id
	if journal != nil {
		if err := journal.BeforeSend(ctx, in); err != nil {
			if errors.Is(err, ErrRequestDeclined) {
				return in, body, fmt.Errorf("%w: %w", ErrNotSent, err)
			}
			return in, body, &journalError{err: err}
		}
	}
	return in, body, nil
}

func (p *PairedSession) post(ctx context.Context, m *Market, body wirePairedReq, id *RequestIdentity) (*PairedTradeResult, error) {
	status, raw, err := p.Client.post(ctx, "/v1/orders/paired", body)
	if err != nil {
		return nil, err
	}
	r, err := receipt(status, raw, id)
	if err != nil {
		return nil, err
	}
	return pairedResult(m, r, raw)
}

type wirePairedResult struct {
	Status       string            `json:"status"`
	Symbol       string            `json:"symbol"`
	Kind         string            `json:"kind"`
	Price        string            `json:"price"`
	Size         string            `json:"size"`
	MakerOrderID string            `json:"makerOrderId"`
	TakerOrderID string            `json:"takerOrderId"`
	AvgPrice     *string           `json:"avgPrice"`
	Fills        []wireReceiptFill `json:"fills"`
	MakerFills   []wireReceiptFill `json:"makerFills"`
	LegFees      *struct {
		Maker *wirePairedLegFee `json:"maker"`
		Taker *wirePairedLegFee `json:"taker"`
	} `json:"legFees"`
}

type wirePairedLegFee struct {
	Fee      string `json:"fee"`
	FeeToken string `json:"feeToken"`
	Protocol string `json:"protocol"`
	Deployer string `json:"deployer"`
}

func pairedLegFee(role string, w *wirePairedLegFee) (PairedLegFee, error) {
	if w == nil || w.Fee == "" || w.FeeToken == "" || w.Protocol == "" || w.Deployer == "" {
		return PairedLegFee{}, fmt.Errorf("dexosv1: 成对成交回执 legFees.%s 不完整: %+v", role, w)
	}
	return PairedLegFee(*w), nil
}

// pairedResult 解析 200 回执(dex-os v1_write.rs paired_result):状态必须是 filled、符号是本市场。
func pairedResult(m *Market, r Receipt, raw []byte) (*PairedTradeResult, error) {
	var w wirePairedResult
	if err := decodeInto(raw, &w, "成对成交回执"); err != nil {
		return nil, err
	}
	if w.Status != "filled" || !sameMarketRow(m, w.Symbol, w.Kind) {
		return nil, fmt.Errorf("dexosv1: 在 %s(%s)成对成交,回执是 %q/%q(%s)", m.Symbol, m.Kind, w.Status, w.Symbol, w.Kind)
	}
	out := &PairedTradeResult{Receipt: r, Status: w.Status, Symbol: w.Symbol, Kind: m.Kind}
	var err error
	if out.MakerOrderID, err = strconv.ParseUint(w.MakerOrderID, 10, 64); err != nil {
		return nil, fmt.Errorf("dexosv1: 成对成交回执 makerOrderId %q: %w", w.MakerOrderID, err)
	}
	if out.TakerOrderID, err = strconv.ParseUint(w.TakerOrderID, 10, 64); err != nil {
		return nil, fmt.Errorf("dexosv1: 成对成交回执 takerOrderId %q: %w", w.TakerOrderID, err)
	}
	if out.Price, err = m.Scale.Ticks(w.Price); err != nil {
		return nil, fmt.Errorf("dexosv1: 成对成交回执 price: %w", err)
	}
	if out.Lots, err = m.Scale.Lots(w.Size); err != nil {
		return nil, fmt.Errorf("dexosv1: 成对成交回执 size: %w", err)
	}
	if w.AvgPrice != nil {
		a, err := m.Scale.Ticks(*w.AvgPrice)
		if err != nil {
			return nil, fmt.Errorf("dexosv1: 成对成交回执 avgPrice: %w", err)
		}
		out.AvgPrice = &a
	}
	if out.Fills, err = receiptFills(m, w.Fills); err != nil {
		return nil, err
	}
	if out.MakerFills, err = receiptFills(m, w.MakerFills); err != nil {
		return nil, err
	}
	if w.LegFees != nil {
		var fees PairedLegFees
		if fees.Maker, err = pairedLegFee("maker", w.LegFees.Maker); err != nil {
			return nil, err
		}
		if fees.Taker, err = pairedLegFee("taker", w.LegFees.Taker); err != nil {
			return nil, err
		}
		out.LegFees = &fees
	}
	return out, nil
}
