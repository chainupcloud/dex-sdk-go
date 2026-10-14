package dexosv1

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 写接口的线上形状(规格 public-api-v1.md §3.2 / §3.3)与回执解析。签名与 nonce 由 [Session] / [Wallet] 管,
// 这里只管「签好的请求怎么发、回执怎么读」。

// ErrPending 请求已定序但还没有结论:`ack=sequenced`,或服务端在等待预算内没等到折叠(HTTP 202)。
// **不要重发**(同一 nonce 重发是另一个请求或被判重复):用 [RequestIdentity] 查 [Client.Request]。
var ErrPending = errors.New("dexosv1: 请求已定序,结论未知")

// ErrIncompleteBatch 批量回执的逐项结局与请求对不上(缺项、多项、重复、符号不符)。
// 批次已经执行,但哪些单成了说不清 —— 按账户读取核对之前不要继续写。
var ErrIncompleteBatch = errors.New("dexosv1: 批量回执与请求对不上")

// Domain 连接参数里的 EIP-712 域。签名一律用服务端给的域,不在本地写死链号。
func (e *Exchange) SigningDomain() (dexos.Domain, error) {
	vc, err := dexos.ParseAddress(e.Domain.VerifyingContract)
	if err != nil {
		return dexos.Domain{}, fmt.Errorf("dexosv1: domain.verifyingContract: %w", err)
	}
	if e.Domain.Name == "" || e.Domain.Version == "" || e.ChainID == 0 {
		return dexos.Domain{}, fmt.Errorf("dexosv1: /v1/exchange 的 EIP-712 域不完整: %+v chainId=%d", e.Domain, e.ChainID)
	}
	return dexos.Domain{Name: e.Domain.Name, Version: e.Domain.Version, ChainID: e.ChainID, VerifyingContract: vc}, nil
}

// RequestIdentity 一次签名请求的身份,发出前就确定(与 dex-os `SignedRequest::request_id` 同一定义):
// RequestID = keccak256(签名者 ‖ EIP-712 摘要)。结论未知时凭它查 [Client.Request]。
type RequestIdentity struct {
	Signer dexos.Address
	// Scope 回执查询用的 nonce 序列:"account:<signingAccountId>"(主钱包)或 "agent:0x…"(API 钱包)。
	Scope       string
	Nonce       uint64
	SigningHash string
	RequestID   string
	// CommandHash 只在 API 钱包路径有:keccak256(规范命令编码)。执行历史有缺口时,
	// 服务端凭它与 agent nonce 证明请求没执行(见 [RequestQuery])。
	CommandHash string
}

// Query 按本身份查回执时带上的 nonce 冲突检查。
func (id *RequestIdentity) Query() RequestQuery {
	n := id.Nonce
	return RequestQuery{Scope: id.Scope, Nonce: &n, CommandHash: id.CommandHash}
}

func newIdentity(signer dexos.Address, scope string, nonce uint64, digest, commandHash []byte) *RequestIdentity {
	id := &RequestIdentity{
		Signer:      signer,
		Scope:       scope,
		Nonce:       nonce,
		SigningHash: "0x" + hex.EncodeToString(digest),
		RequestID:   "0x" + hex.EncodeToString(dexos.Keccak256(signer[:], digest)),
	}
	if commandHash != nil {
		id.CommandHash = "0x" + hex.EncodeToString(commandHash)
	}
	return id
}

// Receipt 写请求的同步回执(两种 ack 共有的部分)。
type Receipt struct {
	// Seq / Sub 这条命令在日志里的位号;之后的账户读取会带上它(read-your-writes)。
	Seq uint64
	Sub uint16
	// Duplicate 同一请求早已执行过:这次没有再提交,位号与结局都是原来那一次的。
	Duplicate bool
	// Request 本地算出的请求身份;已与回执里的 `requestId` 核对过。
	Request *RequestIdentity
}

// envelope 统一签名信封。`signer` 给了就是 API 钱包代签,此时 `nowMs` 必填(它在内层命令里、进签名)。
// 被操作账户二选一:钱包寻址带 `subaccount`;金库身份带 `vault`、`account` 是 manager 的主钱包(同给是 400)。
type envelope struct {
	Account    string  `json:"account"`
	Subaccount *uint32 `json:"subaccount,omitempty"`
	Vault      *uint16 `json:"vault,omitempty"`
	Signer     string  `json:"signer,omitempty"`
	Nonce      uint64  `json:"nonce"`
	ExpiresAt  uint64  `json:"expiresAt,omitempty"`
	NowMs      uint64  `json:"nowMs,omitempty"`
	Signature  string  `json:"signature"`
}

// wireOrderReq 一张限价单的请求形状。批量项每项都带信封(规格的 PlaceOrder 要求每项带);
// 换单里的新单不带(*envelope 为 nil 时整组字段不出现)。
type wireOrderReq struct {
	*envelope
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`
	Type          string `json:"type"`
	Price         string `json:"price"`
	Size          string `json:"size"`
	TIF           string `json:"tif"`
	ReduceOnly    bool   `json:"reduceOnly"`
	ClientOrderID string `json:"clientOrderId,omitempty"`
}

var tifWire = map[dexos.TimeInForce]string{dexos.GTC: "gtc", dexos.IOC: "ioc", dexos.PostOnly: "alo", dexos.FOK: "fok"}

// OrderRequest 一张限价单。价量是 tick / lot(按 Market.Scale 换成十进制上线,签名签的是整数本身)。
//
// 只有限价单:v1 的 `market` 类型由网关按标记价现算保护价,客户端签不出同一个价格;
// 市价单请用限价 + IOC,保护价自己定。
type OrderRequest struct {
	Market     *Market
	Side       dexos.Side
	Price      uint32
	Lots       uint64
	TIF        dexos.TimeInForce
	ReduceOnly bool
	// ClientOrderID 可选,`^[A-Za-z0-9._:/-]{1,36}$`,账户内唯一;**不进签名**。
	ClientOrderID string
}

func (o OrderRequest) batchOrder() dexos.BatchOrder {
	return dexos.BatchOrder{Market: o.Market.SigningMarketID, Side: o.Side, Price: o.Price, Lots: o.Lots, TIF: o.TIF, ReduceOnly: o.ReduceOnly}
}

// wire 校验并换成线上形状。所有本地能判的错都在这里报,签名之前、不碰 nonce。
func (o OrderRequest) wire(env *envelope) (wireOrderReq, error) {
	if o.Market == nil {
		return wireOrderReq{}, errors.New("dexosv1: 订单缺市场")
	}
	tif, ok := tifWire[o.TIF]
	if !ok {
		return wireOrderReq{}, fmt.Errorf("dexosv1: 未知的 TIF %d", o.TIF)
	}
	if o.Side != dexos.Buy && o.Side != dexos.Sell {
		return wireOrderReq{}, fmt.Errorf("dexosv1: 未知的方向 %d", o.Side)
	}
	if o.Price == 0 || o.Lots == 0 {
		return wireOrderReq{}, fmt.Errorf("dexosv1: %s 价格与数量都必须大于 0", o.Market.Symbol)
	}
	if o.ClientOrderID != "" && !validCID(o.ClientOrderID) {
		return wireOrderReq{}, fmt.Errorf("dexosv1: clientOrderId %q 不符合 ^[A-Za-z0-9._:/-]{1,36}$", o.ClientOrderID)
	}
	return wireOrderReq{
		envelope:      env,
		Symbol:        o.Market.Symbol,
		Side:          o.Side.String(),
		Type:          "limit",
		Price:         o.Market.Scale.Price(o.Price),
		Size:          o.Market.Scale.Size(o.Lots),
		TIF:           tif,
		ReduceOnly:    o.ReduceOnly,
		ClientOrderID: o.ClientOrderID,
	}, nil
}

func validCID(s string) bool {
	if len(s) == 0 || len(s) > 36 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:/-", c)) {
			return false
		}
	}
	return true
}

// post 发一次写请求(恒 `ack=result`:会话要靠结论决定 nonce 前进与否)。
// 2xx 返回原始响应;非 2xx 解析成类型化错误。
func (c *Client) post(ctx context.Context, path string, body any) (int, []byte, error) {
	if c.AckWait < 0 || c.AckWait > maxAckWait {
		return 0, nil, fmt.Errorf("%w: AckWait %s 超出 0..%s", errNotSent, c.AckWait, maxAckWait)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %w", errNotSent, err)
	}
	u := c.BaseURL + path + "?ack=result"
	if c.AckWait > 0 {
		u += "&waitMs=" + strconv.FormatInt(c.AckWait.Milliseconds(), 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %w", errNotSent, err)
	}
	req.Header.Set("content-type", "application/json")
	// 带上见过的最大位号:刚授权就写时,网关的预检在这个位号之后的视图上查(v1.1 §4.1)。
	if s := c.seq.Load(); s > 0 {
		req.Header.Set(minSeqHeader, strconv.FormatUint(s, 10))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if v, err := strconv.ParseUint(resp.Header.Get(seqHeader), 10, 64); err == nil {
			c.ObserveSeq(v)
		}
	}
	out := new(bytes.Buffer)
	if _, err := out.ReadFrom(resp.Body); err != nil {
		return 0, nil, fmt.Errorf("dexosv1: 响应读取失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, out.Bytes(), parseError(resp.StatusCode, resp.Header, out.Bytes())
	}
	return resp.StatusCode, out.Bytes(), nil
}

// errNotSent 请求在发出之前就失败了(配置错误、编码失败):服务端什么都没收到,nonce 没动,结论是确定的。
var errNotSent = errors.New("dexosv1: 请求没有发出")

// maxAckWait 服务端 `waitMs` 的上限。
const maxAckWait = 5 * time.Second

func decodeInto(raw []byte, out any, what string) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("dexosv1: %s解析失败: %w", what, err)
	}
	return nil
}

// wireAck 每个写回执都有的字段。
type wireAck struct {
	Status    string  `json:"status"`
	Seq       *uint64 `json:"seq"`
	Sub       *uint16 `json:"sub"`
	Duplicate bool    `json:"duplicate"`
	RequestID string  `json:"requestId"`
}

// receipt 解析公共部分;已定序但没有结论(202 / status=sequenced)返回 ErrPending。
// 回执必须带 requestId 且与本地按签名算出的相同(v1.1 §4.2;只有 builder 转发的单没有,本包不发那种单)。
func receipt(status int, raw []byte, id *RequestIdentity) (Receipt, error) {
	var a wireAck
	if err := json.Unmarshal(raw, &a); err != nil {
		return Receipt{}, fmt.Errorf("dexosv1: 写回执解析失败: %w", err)
	}
	if a.Seq == nil || a.Sub == nil {
		return Receipt{}, fmt.Errorf("dexosv1: 写回执缺 seq/sub: %s", raw)
	}
	if a.RequestID == "" {
		return Receipt{}, fmt.Errorf("dexosv1: 写回执缺 requestId(本地是 %s): %s", id.RequestID, raw)
	}
	if !strings.EqualFold(a.RequestID, id.RequestID) {
		return Receipt{}, fmt.Errorf("dexosv1: 服务端的 requestId %s 与本地按签名算出的 %s 不同", a.RequestID, id.RequestID)
	}
	r := Receipt{Seq: *a.Seq, Sub: *a.Sub, Duplicate: a.Duplicate, Request: id}
	if status == http.StatusAccepted || a.Status == "sequenced" {
		return r, fmt.Errorf("%w(seq %d,requestId %s)", ErrPending, r.Seq, id.RequestID)
	}
	return r, nil
}

// ReceiptFill 回执里本命令作为吃单方产生的一笔成交(v1.1 §4.2)。被动成交(别人吃到本账户的挂单)
// 不在回执里,走 fills / WS。FillID 与 `/v1/account/fills` 里同一笔的 id 相同。
type ReceiptFill struct {
	FillID   string
	Price    uint32
	Lots     uint64
	Fee      string
	FeeToken string
}

type wireReceiptFill struct {
	FillID   string `json:"fillId"`
	Price    string `json:"price"`
	Size     string `json:"size"`
	Fee      string `json:"fee"`
	FeeToken string `json:"feeToken"`
}

func receiptFills(m *Market, in []wireReceiptFill) ([]ReceiptFill, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]ReceiptFill, 0, len(in))
	for _, w := range in {
		if w.FillID == "" || w.FeeToken == "" {
			return nil, fmt.Errorf("dexosv1: 回执成交缺 fillId / feeToken: %+v", w)
		}
		f := ReceiptFill{FillID: w.FillID, Fee: w.Fee, FeeToken: w.FeeToken}
		var err error
		if f.Price, err = m.Scale.Ticks(w.Price); err != nil {
			return nil, fmt.Errorf("dexosv1: 回执成交 %s price: %w", w.FillID, err)
		}
		if f.Lots, err = m.Scale.Lots(w.Size); err != nil {
			return nil, fmt.Errorf("dexosv1: 回执成交 %s size: %w", w.FillID, err)
		}
		out = append(out, f)
	}
	return out, nil
}

// OrderResult 主钱包单笔下单的结论。
type OrderResult struct {
	Receipt
	OrderID       uint64
	ClientOrderID *string
	Symbol        string
	// Status resting | partiallyFilled | filled | canceled(IOC / FOK 未成部分被撤)。
	Status     string
	Price      uint32
	Lots       uint64
	FilledLots uint64
	// AvgPrice 本命令吃单成交的均价;nil = 没有即时成交。
	AvgPrice *uint32
	Fills    []ReceiptFill
}

type wireOrderResult struct {
	Status        string            `json:"status"`
	Symbol        string            `json:"symbol"`
	OrderID       string            `json:"orderId"`
	ClientOrderID *string           `json:"clientOrderId"`
	Price         string            `json:"price"`
	Size          string            `json:"size"`
	Filled        string            `json:"filled"`
	AvgPrice      *string           `json:"avgPrice"`
	Fills         []wireReceiptFill `json:"fills"`
}

func orderResult(m *Market, r Receipt, raw []byte) (*OrderResult, error) {
	var w wireOrderResult
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("dexosv1: 下单回执解析失败: %w", err)
	}
	if !strings.EqualFold(w.Symbol, m.Symbol) {
		return nil, fmt.Errorf("dexosv1: 在 %s 下单,回执是 %q", m.Symbol, w.Symbol)
	}
	o := &OrderResult{Receipt: r, ClientOrderID: w.ClientOrderID, Symbol: w.Symbol, Status: w.Status}
	var err error
	if o.OrderID, err = strconv.ParseUint(w.OrderID, 10, 64); err != nil {
		return nil, fmt.Errorf("dexosv1: 下单回执 orderId %q: %w", w.OrderID, err)
	}
	if o.Price, err = m.Scale.Ticks(w.Price); err != nil {
		return nil, fmt.Errorf("dexosv1: 下单回执 price: %w", err)
	}
	if o.Lots, err = m.Scale.Lots(w.Size); err != nil {
		return nil, fmt.Errorf("dexosv1: 下单回执 size: %w", err)
	}
	if o.FilledLots, err = m.Scale.Lots(w.Filled); err != nil {
		return nil, fmt.Errorf("dexosv1: 下单回执 filled: %w", err)
	}
	if w.AvgPrice != nil {
		p, err := m.Scale.Ticks(*w.AvgPrice)
		if err != nil {
			return nil, fmt.Errorf("dexosv1: 下单回执 avgPrice: %w", err)
		}
		o.AvgPrice = &p
	}
	if o.Fills, err = receiptFills(m, w.Fills); err != nil {
		return nil, err
	}
	return o, nil
}

// BatchItem 批量(下单 / 撤单 / 换单)的一项结局。逐项独立判定:一项被拒不影响其余。
type BatchItem struct {
	// Kind "place" | "cancel";Index 是该项在**同类**输入里的下标(换单的撤单与新单各自从 0 数)。
	Kind  string
	Index int
	// Status place → accepted | rejected;cancel → canceled | rejected。
	Status string
	Symbol string
	// OrderID 下单成功时是新单号;撤单项是被撤(或撤不到)的单号。
	OrderID *uint64
	// Code 被拒时的拒因(CoreError 变体名,如 InsufficientMargin / UnknownOrder)。
	Code string
	// FilledLots / Resting 下单项被接受时有:即时成交量、余量是否挂上了簿。
	FilledLots *uint64
	Resting    *bool
	// CanceledLots 撤单项成功时有:撤掉的剩余量。
	CanceledLots *uint64
	// ClientOrderID 下单项带了 clientOrderId 时回显。
	ClientOrderID *string
	// AvgPrice / Fills 下单项本命令里作为吃单方的成交;没有即时成交时为空。
	AvgPrice *uint32
	Fills    []ReceiptFill
}

// BatchResult 批量写的结论。Items 与请求逐项对齐(本包已核对过每一项都在)。
type BatchResult struct {
	Receipt
	Items []BatchItem
}

type wireBatchItem struct {
	Index         *int              `json:"index"`
	Kind          string            `json:"kind"`
	Status        string            `json:"status"`
	Symbol        string            `json:"symbol"`
	OrderID       *string           `json:"orderId"`
	Code          string            `json:"code"`
	Filled        *string           `json:"filled"`
	Resting       *bool             `json:"resting"`
	Canceled      *string           `json:"canceled"`
	ClientOrderID *string           `json:"clientOrderId"`
	AvgPrice      *string           `json:"avgPrice"`
	Fills         []wireReceiptFill `json:"fills"`
}

// batchResult 解析并核对逐项结局:每个 (kind, index) 恰好一项,项数与请求相同,符号是本市场。
func batchResult(m *Market, r Receipt, raw []byte, places, cancels int) (*BatchResult, error) {
	var w struct {
		Items []wireBatchItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("dexosv1: 批量回执解析失败: %w", err)
	}
	want := map[string]int{"place": places, "cancel": cancels}
	seen := map[string]bool{}
	out := &BatchResult{Receipt: r, Items: make([]BatchItem, 0, len(w.Items))}
	for _, wi := range w.Items {
		if wi.Index == nil || *wi.Index < 0 || *wi.Index >= want[wi.Kind] {
			return nil, fmt.Errorf("%w: 项 %+v 不在请求里(下单 %d 项,撤单 %d 项)", ErrIncompleteBatch, wi, places, cancels)
		}
		key := wi.Kind + "/" + strconv.Itoa(*wi.Index)
		if seen[key] {
			return nil, fmt.Errorf("%w: %s 出现两次", ErrIncompleteBatch, key)
		}
		seen[key] = true
		if !strings.EqualFold(wi.Symbol, m.Symbol) {
			return nil, fmt.Errorf("%w: %s 的符号是 %q,请求的是 %s", ErrIncompleteBatch, key, wi.Symbol, m.Symbol)
		}
		it, err := wi.item(m)
		if err != nil {
			return nil, fmt.Errorf("dexosv1: 批量回执 %s: %w", key, err)
		}
		out.Items = append(out.Items, it)
	}
	if len(seen) != places+cancels {
		return nil, fmt.Errorf("%w: 回了 %d 项,请求 %d 项", ErrIncompleteBatch, len(seen), places+cancels)
	}
	return out, nil
}

func (w wireBatchItem) item(m *Market) (BatchItem, error) {
	it := BatchItem{Kind: w.Kind, Index: *w.Index, Status: w.Status, Symbol: w.Symbol, Code: w.Code,
		Resting: w.Resting, ClientOrderID: w.ClientOrderID}
	switch {
	case w.Status == "rejected" && w.Code == "":
		return it, errors.New("被拒但没有拒因")
	case w.Kind == "place" && w.Status != "accepted" && w.Status != "rejected",
		w.Kind == "cancel" && w.Status != "canceled" && w.Status != "rejected":
		return it, fmt.Errorf("%s 项的状态 %q 不在契约内", w.Kind, w.Status)
	}
	if w.OrderID != nil {
		id, err := strconv.ParseUint(*w.OrderID, 10, 64)
		if err != nil {
			return it, fmt.Errorf("orderId %q: %w", *w.OrderID, err)
		}
		it.OrderID = &id
	}
	lots := func(s *string) (*uint64, error) {
		if s == nil {
			return nil, nil
		}
		n, err := m.Scale.Lots(*s)
		return &n, err
	}
	var err error
	if it.FilledLots, err = lots(w.Filled); err != nil {
		return it, fmt.Errorf("filled: %w", err)
	}
	if it.CanceledLots, err = lots(w.Canceled); err != nil {
		return it, fmt.Errorf("canceled: %w", err)
	}
	if w.AvgPrice != nil {
		p, err := m.Scale.Ticks(*w.AvgPrice)
		if err != nil {
			return it, fmt.Errorf("avgPrice: %w", err)
		}
		it.AvgPrice = &p
	}
	if it.Fills, err = receiptFills(m, w.Fills); err != nil {
		return it, err
	}
	return it, nil
}
