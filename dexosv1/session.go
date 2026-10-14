package dexosv1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 两种写入身份,各自一条 nonce 序列(混用得到 NonceMismatch):
//
//   - [Session]:API 钱包(agent)代签。做市跑在这里:批量下单、撤单、原子换单、撤全部、断线保护。
//     nonce 是 **agent 地址自己的**计数器,同一把钥匙被多个账户授权时几个账户共用它。
//   - [Wallet]:主钱包自签。单笔下单 / 撤单、设杠杆、授权 API 钱包 —— 授权之外一般用不到,
//     主钱包密钥不该放在跑策略的机器上。
//
// 两者都把写串行化,nonce 规则与 dexos.Session 相同(见 nonceGuard.run)。

// ErrSessionBlocked 上一次写入结论未知,本会话禁止继续消耗 nonce。
// 重读 nonce 不是原请求结局的证据:用 Pending() 给出的请求身份查 [Client.Request],核对完再新建会话。
var ErrSessionBlocked = errors.New("dexosv1: 上次写入结论未知,会话禁止继续写入")

// ErrAgentNotAuthorized 这把 API 钱包不在该账户的授权列表里(没授权、已撤销,或授权给的是别的子账户)。
var ErrAgentNotAuthorized = errors.New("dexosv1: 这把 API 钱包没有被该账户授权")

// ErrAgentExpired 授权已到期,写入会被执行层拒(AgentExpired),本地先拦住。
var ErrAgentExpired = errors.New("dexosv1: API 钱包授权已到期")

// preConsume 认证层在**消费 nonce 之前**给出的拒因(dex-os `AuthState::PRE_CONSUME_REJECTIONS`):
// 带着这些拒因的命令没有消费 nonce。其余 422 拒因都在 nonce 消费之后。
// NonceMismatch 也在其中,但它说明本地计数器不可信,单独处理(锁定)。
var preConsume = map[string]bool{
	"SignatureExpired": true, "BadSignature": true, "AuthUnavailable": true, "UnboundSigner": true,
	"UnknownAgent": true, "AgentScopeViolation": true, "AgentExpired": true, "NonceMismatch": true,
	"VaultNoManager": true,
}

// beforeSequencing 网关在定序之前就回的状态码:命令没进日志,nonce 没动,结论是确定的。
// (400 参数 / 401 网关验签 / 403 归属 / 404 未知符号 / 409 cid 重复 / 412 副本落后 / 421 非 leader / 429 限流)
var beforeSequencing = map[int]bool{
	http.StatusBadRequest: true, http.StatusUnauthorized: true, http.StatusForbidden: true,
	http.StatusNotFound: true, http.StatusConflict: true, http.StatusPreconditionFailed: true,
	http.StatusMisdirectedRequest: true, http.StatusTooManyRequests: true,
}

// nonceGuard 一条 nonce 序列的本地计数器 + 「结论未知就锁」。
type nonceGuard struct {
	mu      sync.Mutex
	nonce   uint64
	blocked error
	pending *RequestIdentity
}

// run 串行执行一次签名写入。f 用给定的 nonce 签名并发送,返回请求身份(签名之后恒非 nil)。
//
//   - 成功:nonce 已消耗 → 本地 ++。
//   - 没发出(errNotSent)或定序前的拒绝(beforeSequencing):命令没进日志 → 不动,不锁。
//   - 422 执行层拒绝:NonceMismatch → 锁(本地计数器不可信);preConsume 里的其余拒因 → 不动,不锁;
//     其余(InsufficientMargin 等业务拒绝)→ ++,不锁。都是终局结论,锁住只会让一笔保证金不足停掉整个做市进程。
//   - ErrPending(202)、ErrIncompleteBatch、传输失败、5xx、回执解析失败:结论未知或本地看不全 → 锁定。
//   - 写前日志自身失败([ReplaceJournal]):不论场所的结论,本地没记下来 → 锁定。
func (g *nonceGuard) run(ctx context.Context, f func(nonce uint64) (*RequestIdentity, error)) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blocked != nil {
		return g.blocked
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := f(g.nonce)
	if err == nil {
		g.nonce++
		return nil
	}
	var je *journalError
	if errors.Is(err, errNotSent) && !errors.As(err, &je) {
		return err
	}
	var e *Error
	if !errors.As(err, &je) && errors.As(err, &e) && e.Code != "NonceMismatch" {
		switch {
		case beforeSequencing[e.Status]:
			return err
		case e.Status == http.StatusUnprocessableEntity && e.Code != "":
			if !preConsume[e.Code] {
				g.nonce++
			}
			return err
		}
	}
	g.blocked = fmt.Errorf("%w (nonce=%d): %w", ErrSessionBlocked, g.nonce, err)
	g.pending = id
	return g.blocked
}

func (g *nonceGuard) pendingRequest() *RequestIdentity {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pending
}

// Session API 钱包代签的交易会话。并发安全(写入串行);同一把钥匙不要在两个进程里同时用。
type Session struct {
	Client  *Client
	Agent   *dexos.Signer
	Account AccountRef
	// ValidUntil 授权到期时刻;零值 = 不过期。
	ValidUntil time.Time

	domain    dexos.Domain
	signingID uint32
	// owner 信封的 account:钱包寻址 = Account.Address;金库身份 = manager 的主钱包(网关按它核对 manager)。
	owner dexos.Address
	guard nonceGuard
}

// NewSession 读连接参数、授权列表与 agent nonce,建会话。
// 这把钥匙不在 ref 账户的授权列表里 → ErrAgentNotAuthorized;已到期 → ErrAgentExpired。
//
// ref 是金库身份([VaultAccount])时以金库主账户交易(v1.1 §4.3):授权要挂在 manager 认领金库所用的账户上,
// 本函数从 `/v1/vaults/{id}` 读出那个账户来核对;金库待认领时报错。nonce 仍是这把 API 钱包自己的计数器。
// 金库身份只能交易:下单、撤单、换单、撤全部、断线保护(资金类动作由网关以 403 拒)。
func NewSession(ctx context.Context, c *Client, agent *dexos.Signer, ref AccountRef) (*Session, error) {
	if _, err := ref.values(); err != nil {
		return nil, err
	}
	ex, err := c.Exchange(ctx)
	if err != nil {
		return nil, err
	}
	d, err := ex.SigningDomain()
	if err != nil {
		return nil, err
	}
	grant := ref
	if ref.Vault != nil {
		v, err := c.Vault(ctx, *ref.Vault)
		if err != nil {
			return nil, err
		}
		if v.Manager == nil {
			return nil, fmt.Errorf("dexosv1: 金库 %d 待认领,没有 manager,不能交易", *ref.Vault)
		}
		manager, err := dexos.ParseAddress(*v.Manager)
		if err != nil {
			return nil, fmt.Errorf("dexosv1: 金库 %d 的 manager: %w", *ref.Vault, err)
		}
		grant = AccountRef{Address: manager, Subaccount: *v.ManagerSubaccount}
	}
	agents, err := c.Agents(ctx, grant)
	if err != nil {
		return nil, err
	}
	s := &Session{Client: c, Agent: agent, Account: ref, domain: d, owner: grant.Address}
	found := false
	for _, a := range agents {
		if a.Address == agent.Address().Hex() {
			found = true
			if a.Expired {
				return nil, ErrAgentExpired
			}
			if a.ValidUntil != nil {
				s.ValidUntil = time.UnixMilli(*a.ValidUntil)
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: %s 不在 %s 的授权列表里(会话账户 %s)", ErrAgentNotAuthorized, agent.Address().Hex(), grant, ref)
	}
	addr := agent.Address()
	n, err := c.Nonce(ctx, ref, &addr)
	if err != nil {
		return nil, err
	}
	s.signingID, s.guard.nonce = n.SigningAccountID, n.Nonce
	return s, nil
}

// Nonce 下一个要用的 agent nonce。
func (s *Session) Nonce() uint64 {
	s.guard.mu.Lock()
	defer s.guard.mu.Unlock()
	return s.guard.nonce
}

// Pending 锁定时结论未知的那次请求;没锁定为 nil。
func (s *Session) Pending() *RequestIdentity { return s.guard.pendingRequest() }

func (s *Session) write(ctx context.Context, f func(nonce uint64) (*RequestIdentity, error)) error {
	if !s.ValidUntil.IsZero() && !time.Now().Before(s.ValidUntil) {
		return ErrAgentExpired
	}
	return s.guard.run(ctx, f)
}

// exec 签 AgentExec(commandHash, nonce) 并发送 body(body 由信封构造)。
func (s *Session) exec(ctx context.Context, nonce uint64, cmd []byte, path string, body func(env *envelope) any) (*RequestIdentity, int, []byte, error) {
	digest := s.domain.AgentExecHash(cmd, nonce)
	agent := s.Agent.Address()
	id := newIdentity(agent, "agent:"+agent.Hex(), nonce, digest, dexos.Keccak256(cmd))
	sig, err := s.Agent.SignHashHex(digest)
	if err != nil {
		return id, 0, nil, err
	}
	env := &envelope{Account: s.owner.Hex(), Signer: agent.Hex(), Nonce: nonce, Signature: sig}
	if s.Account.Vault != nil {
		env.Vault = s.Account.Vault
	} else {
		env.Subaccount = &s.Account.Subaccount
	}
	status, raw, err := s.Client.post(ctx, path, body(env))
	return id, status, raw, err
}

func nowMs() uint64 { return uint64(time.Now().UnixMilli()) }

// sameMarket 批量里每张单必须在 m 上(v1 的批量回执按一个符号核对)。
func sameMarket(m *Market, orders []OrderRequest) error {
	if m == nil {
		return errors.New("dexosv1: 缺市场")
	}
	for i, o := range orders {
		if o.Market == nil || o.Market.Symbol != m.Symbol {
			return fmt.Errorf("dexosv1: 第 %d 张单不在 %s 上", i, m.Symbol)
		}
	}
	return nil
}

// Place 在一个市场上批量下单(一张也走批量:API 钱包没有单笔签名形状)。逐项独立判定。
func (s *Session) Place(ctx context.Context, orders ...OrderRequest) (*BatchResult, error) {
	if len(orders) == 0 {
		return nil, errors.New("dexosv1: 没有要下的单")
	}
	m := orders[0].Market
	if err := sameMarket(m, orders); err != nil {
		return nil, err
	}
	items := make([]wireOrderReq, len(orders))
	batch := make([]dexos.BatchOrder, len(orders))
	for i, o := range orders {
		w, err := o.wire(nil)
		if err != nil {
			return nil, err
		}
		items[i], batch[i] = w, o.batchOrder()
	}
	var out *BatchResult
	err := s.write(ctx, func(nonce uint64) (*RequestIdentity, error) {
		now := nowMs()
		id, status, raw, err := s.exec(ctx, nonce, dexos.EncodeBatchPlace(s.signingID, now, batch), "/v1/orders", func(env *envelope) any {
			env.NowMs = now
			body := make([]wireOrderReq, len(items))
			for i, it := range items {
				it.envelope = env // 规格的 PlaceOrder 要求每项带信封;服务端取第一项
				body[i] = it
			}
			return body
		})
		if err != nil {
			return id, err
		}
		r, err := receipt(status, raw, id)
		if err != nil {
			return id, err
		}
		out, err = batchResult(m, r, raw, len(orders), 0)
		return id, err
	})
	return out, err
}

type wireCancelReq struct {
	*envelope
	Symbol  string `json:"symbol"`
	OrderID string `json:"orderId"`
}

// Cancel 在一个市场上批量撤单(orderIDs 是 v1 的 orderId,即订单序号)。逐项尽力而为:撤不到的项 rejected。
func (s *Session) Cancel(ctx context.Context, m *Market, orderIDs ...uint64) (*BatchResult, error) {
	if len(orderIDs) == 0 {
		return nil, errors.New("dexosv1: 没有要撤的单")
	}
	ids := make([]dexos.OrderID, len(orderIDs))
	for i, seq := range orderIDs {
		ids[i] = dexos.NewOrderID(m.SigningMarketID, seq)
	}
	var out *BatchResult
	err := s.write(ctx, func(nonce uint64) (*RequestIdentity, error) {
		id, status, raw, err := s.exec(ctx, nonce, dexos.EncodeBatchCancel(s.signingID, ids), "/v1/orders/cancel", func(env *envelope) any {
			body := make([]wireCancelReq, len(orderIDs))
			for i, seq := range orderIDs {
				body[i] = wireCancelReq{envelope: env, Symbol: m.Symbol, OrderID: strconv.FormatUint(seq, 10)}
			}
			return body
		})
		if err != nil {
			return id, err
		}
		r, err := receipt(status, raw, id)
		if err != nil {
			return id, err
		}
		out, err = batchResult(m, r, raw, 0, len(orderIDs))
		return id, err
	})
	return out, err
}

type wireReplaceReq struct {
	*envelope
	Symbol  string         `json:"symbol"`
	Cancels []string       `json:"cancels"`
	Orders  []wireOrderReq `json:"orders"`
}

// Replace **原子换单**:撤旧挂新落在同一个状态机转移里,没有无报价的中间窗口。做市刷新报价用它。
// 逐项仍尽力而为(一张保证金不足不废掉整轮)。新单可以带 clientOrderId,规则同批量下单。
func (s *Session) Replace(ctx context.Context, m *Market, cancels []uint64, orders []OrderRequest) (*BatchResult, error) {
	if err := sameMarket(m, orders); err != nil {
		return nil, err
	}
	ids := make([]dexos.OrderID, len(cancels))
	wireCancels := make([]string, len(cancels))
	for i, seq := range cancels {
		ids[i] = dexos.NewOrderID(m.SigningMarketID, seq)
		wireCancels[i] = strconv.FormatUint(seq, 10)
	}
	items := make([]wireOrderReq, len(orders))
	batch := make([]dexos.BatchOrder, len(orders))
	for i, o := range orders {
		w, err := o.wire(nil)
		if err != nil {
			return nil, err
		}
		items[i], batch[i] = w, o.batchOrder()
	}
	var out *BatchResult
	err := s.write(ctx, func(nonce uint64) (*RequestIdentity, error) {
		now := nowMs()
		id, status, raw, err := s.exec(ctx, nonce, dexos.EncodeBatchReplace(s.signingID, now, ids, batch), "/v1/orders/replace", func(env *envelope) any {
			env.NowMs = now
			return wireReplaceReq{envelope: env, Symbol: m.Symbol, Cancels: wireCancels, Orders: items}
		})
		if err != nil {
			return id, err
		}
		r, err := receipt(status, raw, id)
		if err != nil {
			return id, err
		}
		out, err = batchResult(m, r, raw, len(orders), len(cancels))
		return id, err
	})
	return out, err
}

// CancelAll 撤掉账户全部市场的挂单(= 触发时刻为现在的 ScheduleCancel,下一个块边界生效)。
//
// 生效在**下一个块边界**:回执之后、生效之前新挂的单同样会被撤。撤完要立刻重挂的,
// 改用 Cancel 按单号撤,或等下一块之后再挂。它也会覆盖已设的断线保护(同一个开关)。
func (s *Session) CancelAll(ctx context.Context) (*Receipt, error) {
	var out Receipt
	err := s.write(ctx, func(nonce uint64) (*RequestIdentity, error) {
		now := nowMs()
		id, status, raw, err := s.exec(ctx, nonce, dexos.EncodeScheduleCancel(s.signingID, now, now), "/v1/orders/cancel-all", func(env *envelope) any {
			env.NowMs = now
			return env
		})
		if err != nil {
			return id, err
		}
		out, err = receipt(status, raw, id)
		return id, err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

type wireScheduleReq struct {
	*envelope
	At uint64 `json:"at"`
}

// ScheduleCancel 断线保护:到 at 撤掉账户全部挂单;再发一次覆盖,at 为零值 = 清除。
// 做市的用法是每轮报价顺手往后续一次,断线了就自动生效。
func (s *Session) ScheduleCancel(ctx context.Context, at time.Time) (*Receipt, error) {
	var atMs uint64
	if !at.IsZero() {
		atMs = uint64(at.UnixMilli())
	}
	var out Receipt
	err := s.write(ctx, func(nonce uint64) (*RequestIdentity, error) {
		now := nowMs()
		id, status, raw, err := s.exec(ctx, nonce, dexos.EncodeScheduleCancel(s.signingID, atMs, now), "/v1/orders/schedule-cancel", func(env *envelope) any {
			env.NowMs = now
			return wireScheduleReq{envelope: env, At: atMs}
		})
		if err != nil {
			return id, err
		}
		out, err = receipt(status, raw, id)
		return id, err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Wallet 主钱包自签的写入。nonce 是被操作账户自己的计数器(scope account)。
type Wallet struct {
	Client  *Client
	Owner   *dexos.Signer
	Account AccountRef

	domain    dexos.Domain
	signingID uint32
	guard     nonceGuard
}

// NewWallet 读连接参数与账户 nonce,建主钱包写入。subaccount 0 = 主账户。
func NewWallet(ctx context.Context, c *Client, owner *dexos.Signer, subaccount uint32) (*Wallet, error) {
	ex, err := c.Exchange(ctx)
	if err != nil {
		return nil, err
	}
	d, err := ex.SigningDomain()
	if err != nil {
		return nil, err
	}
	ref := AccountRef{Address: owner.Address(), Subaccount: subaccount}
	n, err := c.Nonce(ctx, ref, nil)
	if err != nil {
		return nil, err
	}
	w := &Wallet{Client: c, Owner: owner, Account: ref, domain: d, signingID: n.SigningAccountID}
	w.guard.nonce = n.Nonce
	return w, nil
}

// Nonce 下一个要用的账户 nonce。
func (w *Wallet) Nonce() uint64 {
	w.guard.mu.Lock()
	defer w.guard.mu.Unlock()
	return w.guard.nonce
}

// Pending 锁定时结论未知的那次请求;没锁定为 nil。
func (w *Wallet) Pending() *RequestIdentity { return w.guard.pendingRequest() }

// sign 主钱包对 EIP-712 摘要签名,返回信封与请求身份。
func (w *Wallet) sign(nonce uint64, digest []byte) (*envelope, *RequestIdentity, error) {
	owner := w.Owner.Address()
	id := newIdentity(owner, "account:"+strconv.FormatUint(uint64(w.signingID), 10), nonce, digest, nil)
	sig, err := w.Owner.SignHashHex(digest)
	if err != nil {
		return nil, id, err
	}
	return &envelope{Account: owner.Hex(), Subaccount: &w.Account.Subaccount, Nonce: nonce, Signature: sig}, id, nil
}

// PlaceOrder 单笔限价单。expiresAt 零值 = 签名不过期(它进签名)。
func (w *Wallet) PlaceOrder(ctx context.Context, o OrderRequest, expiresAt time.Time) (*OrderResult, error) {
	item, err := o.wire(nil)
	if err != nil {
		return nil, err
	}
	var expiry uint64
	if !expiresAt.IsZero() {
		expiry = uint64(expiresAt.UnixMilli())
	}
	var out *OrderResult
	err = w.guard.run(ctx, func(nonce uint64) (*RequestIdentity, error) {
		digest := orderHash(w.domain, w.Owner.Address(), w.signingID, o.Market.SigningMarketID, o.Side, o.Price, o.Lots, o.TIF, o.ReduceOnly, nonce, expiry)
		env, id, err := w.sign(nonce, digest)
		if err != nil {
			return id, err
		}
		env.ExpiresAt = expiry
		item.envelope = env
		status, raw, err := w.Client.post(ctx, "/v1/orders", item)
		if err != nil {
			return id, err
		}
		r, err := receipt(status, raw, id)
		if err != nil {
			return id, err
		}
		out, err = orderResult(o.Market, r, raw)
		return id, err
	})
	return out, err
}

// CancelResult 主钱包单笔撤单的结论。撤不到(已成交 / 已撤 / 不存在)是 422 UnknownOrder,不在这里。
type CancelResult struct {
	Receipt
	OrderID uint64
	// RemainingLots 撤掉的剩余量;nil = 回执没给。
	RemainingLots *uint64
}

// Cancel 单笔撤单。
func (w *Wallet) Cancel(ctx context.Context, m *Market, orderID uint64) (*CancelResult, error) {
	var out *CancelResult
	err := w.guard.run(ctx, func(nonce uint64) (*RequestIdentity, error) {
		env, id, err := w.sign(nonce, cancelHash(w.domain, w.Owner.Address(), w.signingID, m.SigningMarketID, orderID, nonce))
		if err != nil {
			return id, err
		}
		status, raw, err := w.Client.post(ctx, "/v1/orders/cancel", wireCancelReq{envelope: env, Symbol: m.Symbol, OrderID: strconv.FormatUint(orderID, 10)})
		if err != nil {
			return id, err
		}
		r, err := receipt(status, raw, id)
		if err != nil {
			return id, err
		}
		out, err = cancelResult(m, r, raw, orderID)
		return id, err
	})
	return out, err
}

func cancelResult(m *Market, r Receipt, raw []byte, orderID uint64) (*CancelResult, error) {
	var wr struct {
		Symbol    string  `json:"symbol"`
		OrderID   string  `json:"orderId"`
		Remaining *string `json:"remaining"`
	}
	if err := decodeInto(raw, &wr, "撤单回执"); err != nil {
		return nil, err
	}
	if !strings.EqualFold(wr.Symbol, m.Symbol) || wr.OrderID != strconv.FormatUint(orderID, 10) {
		return nil, fmt.Errorf("dexosv1: 撤 %s/%d,回执是 %s/%s", m.Symbol, orderID, wr.Symbol, wr.OrderID)
	}
	out := &CancelResult{Receipt: r, OrderID: orderID}
	if wr.Remaining != nil {
		n, err := m.Scale.Lots(*wr.Remaining)
		if err != nil {
			return nil, fmt.Errorf("dexosv1: 撤单回执 remaining: %w", err)
		}
		out.RemainingLots = &n
	}
	return out, nil
}

type wireLeverageReq struct {
	*envelope
	Symbol   string `json:"symbol"`
	Leverage uint32 `json:"leverage"`
}

// SetLeverage 设某市场的杠杆上限(只能调低到市场最大值以下);0 = 回到市场缺省。
// 签的是 customImfPpm = floor(1e6 / leverage),与网关同一公式。
func (w *Wallet) SetLeverage(ctx context.Context, m *Market, leverage uint32) (*Receipt, error) {
	var imf uint32
	if leverage > 0 {
		imf = 1_000_000 / leverage
	}
	var out Receipt
	err := w.guard.run(ctx, func(nonce uint64) (*RequestIdentity, error) {
		env, id, err := w.sign(nonce, leverageHash(w.domain, w.Owner.Address(), w.signingID, m.SigningMarketID, imf, nonce))
		if err != nil {
			return id, err
		}
		status, raw, err := w.Client.post(ctx, "/v1/account/leverage", wireLeverageReq{envelope: env, Symbol: m.Symbol, Leverage: leverage})
		if err != nil {
			return id, err
		}
		out, err = receipt(status, raw, id)
		return id, err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

type wireApproveReq struct {
	*envelope
	Agent      string `json:"agent"`
	ValidUntil uint64 `json:"validUntil,omitempty"`
	Label      string `json:"label,omitempty"`
}

// ApproveAgent 授权一把 API 钱包代本账户交易。validUntil 零值 = 不过期;label 只是网关上的标签(≤ 32 字符,不进签名)。
// 同一账户重复授权同一地址 = 更新有效期。授权后用回执的 Seq 建 Session,读到的就是授权之后的视图。
func (w *Wallet) ApproveAgent(ctx context.Context, agent dexos.Address, validUntil time.Time, label string) (*Receipt, error) {
	var until uint64
	if !validUntil.IsZero() {
		until = uint64(validUntil.UnixMilli())
	}
	var out Receipt
	err := w.guard.run(ctx, func(nonce uint64) (*RequestIdentity, error) {
		env, id, err := w.sign(nonce, w.domain.ApproveAgentHash(w.Owner.Address(), w.signingID, agent, until, nonce))
		if err != nil {
			return id, err
		}
		status, raw, err := w.Client.post(ctx, "/v1/account/agents/approve", wireApproveReq{envelope: env, Agent: agent.Hex(), ValidUntil: until, Label: label})
		if err != nil {
			return id, err
		}
		if out, err = receipt(status, raw, id); err != nil {
			return id, err
		}
		var wr struct {
			Agent string `json:"agent"`
		}
		if err := decodeInto(raw, &wr, "授权回执"); err != nil {
			return id, err
		}
		if !strings.EqualFold(wr.Agent, agent.Hex()) {
			return id, fmt.Errorf("dexosv1: 授权 %s,回执是 %q", agent.Hex(), wr.Agent)
		}
		return id, nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
