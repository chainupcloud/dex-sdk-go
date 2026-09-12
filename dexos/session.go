package dexos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
// 少一个参数不是为了短。主钱包地址是**能推出来的** —— agent 地址在状态机里
// 唯一绑定一个 master(绑第二个会被 AgentAlreadyBound 拒),所以让人再传一遍
// 只是给抄错留位置:抄错的表现是订单落到别人账户上(若那个号恰好存在)
// 或一片看不出原因的拒绝。
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

	nonce uint64
}

// ErrAgentNotAuthorized 这把 API 钱包没有被任何账户授权(或授权已被撤销)。
//
// 不是「密钥不对」——密钥格式没问题,只是交易所这边不认它代表谁。
// 去前端「API 钱包」里重新生成并授权一把。
var ErrAgentNotAuthorized = errors.New("dexos: 这把 API 钱包未被授权(去前端重新授权)")

type agentOwnerResp struct {
	Master       uint32 `json:"master"`
	ValidUntilMs string `json:"validUntilMs"`
	Expired      bool   `json:"expired"`
	NextNonce    uint64 `json:"nextNonce"`
}

// New 初始化一个交易会话。**这是大多数接入方唯一需要调用的构造函数。**
//
//	s, err := dexos.New(ctx, "http://node:17807", "0x<API 钱包私钥>")
//	s.Place(ctx, dexos.BatchOrder{Market: 0, Side: dexos.Buy, Price: 70000, Lots: 1, TIF: dexos.GTC})
//
// 它做三件事,每一件都是接入时容易出错的地方:
//
//  1. 向 /config 发现链参数并核对 codec 版本(填错 → 每笔签名 401,而行情正常)
//  2. 由 API 钱包地址反查它代表哪个账户(不必再被带外告知账户号)
//  3. 读取并维护 agent 自己的 nonce(与 master 的 nonce 混用是最常见的坑)
func New(ctx context.Context, baseURL, apiKeyHex string) (*Session, error) {
	agent, err := NewSigner(apiKeyHex)
	if err != nil {
		return nil, fmt.Errorf("dexos: API 钱包私钥无法解析:%w", err)
	}
	c, cfg, err := Connect(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	var o agentOwnerResp
	if err := c.do(ctx, http.MethodGet, "/agent/"+agent.Address().Hex(), nil, &o); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, ErrAgentNotAuthorized
		}
		return nil, err
	}
	if o.Expired {
		return nil, fmt.Errorf("dexos: 这把 API 钱包的授权已过期 —— 去前端续期或重新生成")
	}
	var until uint64
	fmt.Sscan(o.ValidUntilMs, &until)
	return &Session{
		Client:       c,
		Config:       cfg,
		Agent:        agent,
		Account:      o.Master,
		ValidUntilMs: until,
		nonce:        o.NextNonce,
	}, nil
}

// Resync 重新从服务端取 nonce。
//
// 丢帧、重启、或同一把 API 钱包被多个进程共用之后调用它。
// **同一把钥匙多进程并发是不支持的** —— nonce 是单调计数器,两个进程会互相
// 打架;要并发就给每个进程一把自己的 API 钱包(同一个账户可以授权多把)。
func (s *Session) Resync(ctx context.Context) error {
	n, err := s.Client.AgentNonce(ctx, s.Account, s.Agent.Address())
	if err != nil {
		return err
	}
	s.nonce = n
	return nil
}

// 写操作统一经这里:成功则 nonce++,失败则重新取。
//
// 失败后盲目 ++ 是错的:有的失败消耗了 nonce(内核已受理但业务拒绝),
// 有的没有(签名根本没到内核)。从服务端重取是唯一不会猜错的做法。
func (s *Session) write(
	ctx context.Context,
	f func(nonce uint64) ([]EventEnvelope, error),
) ([]EventEnvelope, error) {
	ev, err := f(s.nonce)
	if err != nil {
		if rerr := s.Resync(ctx); rerr != nil {
			return nil, fmt.Errorf("%w(且 nonce 重取失败:%v)", err, rerr)
		}
		return nil, err
	}
	s.nonce++
	return ev, nil
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
	return s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		return s.Client.BatchReplace(ctx, s.Agent, s.Account, market, cancelSeqs, orders, n)
	})
}

// SetLeverage 设置杠杆。
func (s *Session) SetLeverage(ctx context.Context, market uint16, leverage uint32) ([]EventEnvelope, error) {
	return s.write(ctx, func(n uint64) ([]EventEnvelope, error) {
		return s.Client.SetLeverage(ctx, s.Agent, s.Account, market, leverage, n)
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
