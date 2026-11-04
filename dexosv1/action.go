package dexosv1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// sigVersion 2 的预签写入(dex-os 子账户模型 v2 §3.10,开放 API v1 §4.3):签名与发送分开,不持会话级写锁,
// 结果不明不锁任何东西。同一账户上可以有多笔请求同时在途(每笔一个窗口 nonce),原样重发同一份字节是幂等回放
// (已执行的回 `duplicate` 与原结论)。只支持钱包寻址:v2 信封没有金库寻址。

// ErrVaultAction sigVersion 2 信封不支持金库寻址(开放 API v1 §4.3)。
var ErrVaultAction = errors.New("dexosv1: sigVersion 2 不支持金库寻址")

// ErrNonceAhead 窗口 nonce 领先墙钟太多:dex-os 只收块时刻前 2 天到后 1 天之内的 nonce,
// 同一签名者每毫秒签不止一笔时 nonce 会跑到墙钟前面,领先 12 小时即拒绝再签。
var ErrNonceAhead = errors.New("dexosv1: 窗口 nonce 领先墙钟超过 12 小时")

const maxNonceLead = 12 * time.Hour

// NonceSource 一个签名者的窗口 nonce:n = max(上一个 + 1, 当前毫秒),从 floor 之后起算。并发安全。
// 一个签名者在一个进程里只用一个实例;floor 取调用方持久化的高水位,重启后不复用已签过的值。
type NonceSource struct {
	mu   sync.Mutex
	last uint64
}

// NewNonceSource 下一个 nonce 严格大于 floor。
func NewNonceSource(floor uint64) *NonceSource { return &NonceSource{last: floor} }

// Next 取下一个 nonce;领先 now 超过 12 小时返回 [ErrNonceAhead],不推进。
func (n *NonceSource) Next(now time.Time) (uint64, error) {
	ms := uint64(now.UnixMilli())
	n.mu.Lock()
	defer n.mu.Unlock()
	next := max(ms, n.last+1)
	if next-ms > uint64(maxNonceLead.Milliseconds()) {
		return 0, fmt.Errorf("%w(nonce %d,墙钟 %d)", ErrNonceAhead, next, ms)
	}
	n.last = next
	return next, nil
}

// Last 已签出的最大 nonce(没签过时是 floor)。
func (n *NonceSource) Last() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.last
}

// ActionWriter API 钱包以 sigVersion 2 代一个钱包子账户签交易类请求。只签名,不发送;并发安全。
// 授权可以是按账户的旧授权,也可以是位图覆盖该编号的 owner 级授权(执行层两张表都认)。
type ActionWriter struct {
	Client  *Client
	Agent   *dexos.Signer
	Account AccountRef
	// ValidUntil 授权到期时刻;零值 = 不过期。到期后签名返回 [ErrAgentExpired]。
	ValidUntil time.Time

	domain dexos.Domain
	nonces *NonceSource
	now    func() time.Time
}

// NewActionWriter 读连接参数与授权列表。ref 是金库身份 → [ErrVaultAction];这把钥匙不在 ref 的授权列表里 →
// [ErrAgentNotAuthorized];已到期 → [ErrAgentExpired]。nonces 是这把 API 钱包的窗口 nonce 来源。
func NewActionWriter(ctx context.Context, c *Client, agent *dexos.Signer, ref AccountRef, nonces *NonceSource) (*ActionWriter, error) {
	if ref.Vault != nil {
		return nil, ErrVaultAction
	}
	if nonces == nil {
		return nil, errors.New("dexosv1: 缺窗口 nonce 来源")
	}
	if ref.Subaccount > math.MaxUint16 {
		return nil, fmt.Errorf("dexosv1: 子账户编号 %d 超出 uint16", ref.Subaccount)
	}
	ex, err := c.Exchange(ctx)
	if err != nil {
		return nil, err
	}
	d, err := ex.SigningDomain()
	if err != nil {
		return nil, err
	}
	agents, err := c.Agents(ctx, ref)
	if err != nil {
		return nil, err
	}
	until, err := grantOf(agents, agent.Address(), ref.Subaccount)
	if errors.Is(err, ErrAgentNotAuthorized) {
		return nil, fmt.Errorf("%w: %s 不在 %s 的授权列表里", ErrAgentNotAuthorized, agent.Address().Hex(), ref)
	} else if err != nil {
		return nil, err
	}
	return &ActionWriter{Client: c, Agent: agent, Account: ref, ValidUntil: until, domain: d, nonces: nonces, now: time.Now}, nil
}

// SignedAction 签好、还没发出的 sigVersion 2 请求。Body 是发出去的字节,原样重发就是再发一次 Body;
// Request 是请求身份(scope `signer:0x…`),结论未知时凭它查 [Client.Request]。
type SignedAction struct {
	Path    string
	Body    []byte
	Request *RequestIdentity
	Market  *Market
	// Command 内层命令的规范编码(行为人是 [dexos.ActorPlaceholder]),签名摘要 = Action(account, subaccount, keccak256(Command), nonce, expiry)。
	Command   []byte
	NowMs     uint64
	ExpiresAt uint64
	Cancels   int
	Modifies  int
	Places    int
}

// SignReplace 签一次原子换单(撤单 → 改单 → 下单,同 [Session.ReplaceModify]):没有改单项时内层是 BatchReplace,
// 否则是 BatchReplace2。expiresAt 零值 = 不过期;过期后 dex-os 以 SignatureExpired 拒(消费 nonce 之前)。
func (w *ActionWriter) SignReplace(m *Market, cancels []uint64, modifies []ModifyRequest, orders []OrderRequest, expiresAt time.Time) (*SignedAction, error) {
	p, err := prepareReplace(m, cancels, modifies, orders)
	if err != nil {
		return nil, err
	}
	now := w.now()
	nonce, err := w.nonce(now)
	if err != nil {
		return nil, err
	}
	nowMs := uint64(now.UnixMilli())
	cmd := p.command(dexos.ActorPlaceholder, nowMs)
	env, id, err := w.sign(cmd, nonce, expiresAt)
	if err != nil {
		return nil, err
	}
	env.NowMs = nowMs
	body, err := json.Marshal(p.body(env, m))
	if err != nil {
		return nil, err
	}
	return &SignedAction{Path: "/v1/orders/replace", Body: body, Request: id, Market: m, Command: cmd, NowMs: nowMs, ExpiresAt: env.ExpiresAt,
		Cancels: len(cancels), Modifies: len(modifies), Places: len(orders)}, nil
}

// SignCancel 签一次撤单:一张是 CancelOrder,多张是 BatchCancel(与网关相同)。
func (w *ActionWriter) SignCancel(m *Market, orderIDs []uint64, expiresAt time.Time) (*SignedAction, error) {
	nonce, err := w.nonce(w.now())
	if err != nil {
		return nil, err
	}
	return w.SignCancelAt(m, nonce, orderIDs, expiresAt)
}

// SignCancelAt 同 [ActionWriter.SignCancel],但用调用方给的 nonce(不经 nonce 来源)。用于在一笔结论未知的请求的
// nonce 上发占位撤单:两者只会有一个被执行,另一个以 NonceMismatch 拒在消费 nonce 之前。
func (w *ActionWriter) SignCancelAt(m *Market, nonce uint64, orderIDs []uint64, expiresAt time.Time) (*SignedAction, error) {
	if m == nil {
		return nil, errors.New("dexosv1: 缺市场")
	}
	if len(orderIDs) == 0 {
		return nil, errors.New("dexosv1: 没有要撤的单")
	}
	var cmd []byte
	if len(orderIDs) == 1 {
		cmd = dexos.EncodeCancelOrder(dexos.ActorPlaceholder, dexos.NewOrderID(m.SigningMarketID, orderIDs[0]))
	} else {
		ids := make([]dexos.OrderID, len(orderIDs))
		for i, seq := range orderIDs {
			ids[i] = dexos.NewOrderID(m.SigningMarketID, seq)
		}
		cmd = dexos.EncodeBatchCancel(dexos.ActorPlaceholder, ids)
	}
	env, id, err := w.sign(cmd, nonce, expiresAt)
	if err != nil {
		return nil, err
	}
	items := make([]wireCancelReq, len(orderIDs))
	for i, seq := range orderIDs {
		items[i] = cancelItem(env, m, seq)
	}
	body, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	return &SignedAction{Path: "/v1/orders/cancel", Body: body, Request: id, Market: m, Command: cmd, ExpiresAt: env.ExpiresAt, Cancels: len(orderIDs)}, nil
}

func (w *ActionWriter) nonce(now time.Time) (uint64, error) {
	if !w.ValidUntil.IsZero() && !now.Before(w.ValidUntil) {
		return 0, ErrAgentExpired
	}
	return w.nonces.Next(now)
}

func (w *ActionWriter) sign(cmd []byte, nonce uint64, expiresAt time.Time) (*envelope, *RequestIdentity, error) {
	var expiry uint64
	if !expiresAt.IsZero() {
		expiry = uint64(expiresAt.UnixMilli())
	}
	digest := w.domain.ActionHash(w.Account.Address, uint16(w.Account.Subaccount), cmd, nonce, expiry)
	agent := w.Agent.Address()
	sig, err := w.Agent.SignHashHex(digest)
	if err != nil {
		return nil, nil, err
	}
	sub := w.Account.Subaccount
	env := &envelope{SigVersion: 2, Account: w.Account.Address.Hex(), Subaccount: &sub, Signer: agent.Hex(), Nonce: nonce, ExpiresAt: expiry, Signature: sig}
	return env, newIdentity(agent, "signer:"+agent.Hex(), nonce, digest, nil), nil
}

// SendReplace 发出(或原样重发)一次签好的换单。返回的错误按 [ClassifyAction] 判结论。
func (c *Client) SendReplace(ctx context.Context, a *SignedAction) (*BatchResult, error) {
	if a.Path != "/v1/orders/replace" {
		return nil, fmt.Errorf("dexosv1: %s 不是换单", a.Path)
	}
	r, raw, err := c.sendAction(ctx, a)
	if err != nil {
		return nil, err
	}
	return batchResult(a.Market, r, raw, a.Places, a.Cancels, a.Modifies)
}

// SendCancel 发出(或原样重发)一次签好的撤单。撤一张的回执也按一项撤单项给出(Kind "cancel"、Index 0)。
func (c *Client) SendCancel(ctx context.Context, a *SignedAction) (*BatchResult, error) {
	if a.Path != "/v1/orders/cancel" {
		return nil, fmt.Errorf("dexosv1: %s 不是撤单", a.Path)
	}
	r, raw, err := c.sendAction(ctx, a)
	if err != nil {
		return nil, err
	}
	if a.Cancels > 1 {
		return batchResult(a.Market, r, raw, 0, a.Cancels, 0)
	}
	var items []struct {
		OrderID string `json:"orderId"`
	}
	if err := json.Unmarshal(a.Body, &items); err != nil || len(items) != 1 {
		return nil, fmt.Errorf("dexosv1: 撤单请求体不是一项: %v", err)
	}
	seq, err := strconv.ParseUint(items[0].OrderID, 10, 64)
	if err != nil {
		return nil, err
	}
	one, err := cancelResult(a.Market, r, raw, seq)
	if err != nil {
		return nil, err
	}
	return &BatchResult{Receipt: r, Items: []BatchItem{{Kind: "cancel", Index: 0, Status: "canceled", Symbol: a.Market.Symbol,
		SigningMarketID: a.Market.SigningMarketID, OrderID: &seq, CanceledLots: one.RemainingLots}}}, nil
}

func (c *Client) sendAction(ctx context.Context, a *SignedAction) (Receipt, []byte, error) {
	status, raw, err := c.post(ctx, a.Path, json.RawMessage(a.Body))
	if err != nil {
		return Receipt{}, nil, err
	}
	r, err := receipt(status, raw, a.Request)
	return r, raw, err
}

// ActionOutcome 一次 sigVersion 2 写入出错时的结论。
type ActionOutcome int

const (
	// ActionUnknown 结论未知:已定序没有结论(202)、连上之后的传输失败、5xx、回执解析或核对失败。
	// 原样重发同一份 Body 拿结论;过期之后凭请求身份查 [Client.Request]。
	ActionUnknown ActionOutcome = iota
	// ActionNotExecuted 确定没执行、nonce 没消费:没发出、定序前被网关拒、认证层在消费 nonce 之前拒
	// (NonceMismatch、NonceOutOfWindow、SignatureExpired 等)。可以用新 nonce 重签。
	ActionNotExecuted
	// ActionRejected 已执行、整条被执行层拒(如保证金不足),nonce 已消费。终局结论。
	ActionRejected
)

// ClassifyAction 按 [Client.SendReplace] / [Client.SendCancel] 的错误判结论;err 为 nil 不该调用。
func ClassifyAction(err error) ActionOutcome {
	if errors.Is(err, ErrNotSent) {
		return ActionNotExecuted
	}
	var e *Error
	if !errors.As(err, &e) {
		return ActionUnknown
	}
	switch {
	case beforeSequencing[e.Status]:
		return ActionNotExecuted
	case e.Status == http.StatusUnprocessableEntity && e.Code != "" && preConsume[e.Code]:
		return ActionNotExecuted
	case e.Status == http.StatusUnprocessableEntity && e.Code != "":
		return ActionRejected
	}
	return ActionUnknown
}
