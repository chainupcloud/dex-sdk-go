package dexosv1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// Client dex-os 开放 API v1 的 REST 客户端。并发安全。
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// AckWait 写请求等结论的预算(`?waitMs=`,上限 5s);0 = 服务端缺省(`V1_ACK_WAIT_MS`)。
	// 预算内没等到折叠回 202,会话按结论未知锁定([ErrPending])。
	AckWait time.Duration

	// seq 这个客户端见过的最大提交位号(`x-dexos-seq`)。语义与 dexos.Client 相同:账户类读取带上
	// `x-dexos-min-seq`,落后的只读副本以 412 明确失败,而不是悄悄返回写之前的状态。
	// 行情类读取不带(OpenAPI 明确要求:行情投影有自己的时间线,带了只会无谓地 412)。
	seq atomic.Uint64
}

// 读一致性的两个头,与旧接口同义。
const (
	seqHeader    = "x-dexos-seq"
	minSeqHeader = "x-dexos-min-seq"
)

// NewClient baseURL 形如 http://127.0.0.1:8080(不带 /v1)。
func NewClient(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// Seq 这个客户端见过的最大提交位号。
func (c *Client) Seq() uint64 { return c.seq.Load() }

// ObserveSeq 把外部得知的位号并入(取较大者),让本客户端之后的账户读取挂在同一条链上。
func (c *Client) ObserveSeq(v uint64) {
	for {
		cur := c.seq.Load()
		if v <= cur || c.seq.CompareAndSwap(cur, v) {
			return
		}
	}
}

// AccountRef v1 的账户寻址:主钱包地址 + 子账户序号(0 = 主账户),或者金库号(v1.1 §4.3)。
//
// Vault 非 nil = 以金库身份寻址:读写的是金库主账户,与 Address / Subaccount 互斥(二者须为零值)。
// 金库身份能读汇总 / 持仓 / 挂单 / 成交 / 流水 / nonce、能订私有频道、能交易;
// API 钱包列表([Client.Agents])与 portfolio 不接受金库身份。
//
// v1 不按数字账户号寻址;nonce 接口返回的 signingAccountId 只用来填签名结构体。
type AccountRef struct {
	Address    dexos.Address
	Subaccount uint32
	Vault      *uint16
}

// VaultAccount 以金库身份寻址金库 id。
func VaultAccount(id uint16) AccountRef { return AccountRef{Vault: &id} }

func (r AccountRef) values() (url.Values, error) {
	if r.Vault == nil {
		return url.Values{
			"address":    {r.Address.Hex()},
			"subaccount": {strconv.FormatUint(uint64(r.Subaccount), 10)},
		}, nil
	}
	if !r.Address.IsZero() || r.Subaccount != 0 {
		return nil, fmt.Errorf("dexosv1: 金库 %d 的寻址不能再带地址 / 子账户(%s/%d)", *r.Vault, r.Address.Hex(), r.Subaccount)
	}
	return url.Values{"vault": {strconv.FormatUint(uint64(*r.Vault), 10)}}, nil
}

// walletValues 只接受钱包寻址的端点(金库身份服务端回 400,这里先拦住)。
func (r AccountRef) walletValues(what string) (url.Values, error) {
	if r.Vault != nil {
		return nil, fmt.Errorf("dexosv1: %s 不接受金库身份(金库 %d)", what, *r.Vault)
	}
	return r.values()
}

func (r AccountRef) String() string {
	if r.Vault != nil {
		return fmt.Sprintf("vault %d", *r.Vault)
	}
	return fmt.Sprintf("%s/%d", r.Address.Hex(), r.Subaccount)
}

// readKind 决定一次读要不要带 `x-dexos-min-seq`。
type readKind int

const (
	marketRead  readKind = iota // 行情:不带
	accountRead                 // 账户:带(写之后的读要看到这次写)
)

// get 发一次 GET;2xx 解码进 out,非 2xx 解析成类型化错误。
func (c *Client) get(ctx context.Context, path string, q url.Values, kind readKind, out any) error {
	status, header, raw, err := c.roundTrip(ctx, path, q, kind)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return parseError(status, header, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("dexosv1: %s 响应解析失败: %w", path, err)
	}
	return nil
}

func (c *Client) roundTrip(ctx context.Context, path string, q url.Values, kind readKind) (int, http.Header, []byte, error) {
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	if s := c.seq.Load(); s > 0 && kind == accountRead {
		req.Header.Set(minSeqHeader, strconv.FormatUint(s, 10))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	// 只在 2xx 上采信位号:412 带的是副本**本地已应用**位号,并入它会把要求放松,方向正好错。
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if v, err := strconv.ParseUint(resp.Header.Get(seqHeader), 10, 64); err == nil {
			c.ObserveSeq(v)
		}
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("dexosv1: 响应读取失败: %w", err)
	}
	return resp.StatusCode, resp.Header, raw, nil
}
