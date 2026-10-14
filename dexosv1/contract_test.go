//go:build dexos_contract

package dexosv1

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// 契约测试:对一台真实 dex-os 跑只读接口 —— 每个接口既用本包解码,也把原始响应对钉住的规格做
// schema 校验。默认不编译,要跑:
//
//	DEXOS_V1_URL=http://127.0.0.1:17807 DEXOS_V1_ADDRESS=0x… go test -tags dexos_contract -run Contract ./dexosv1/
//
// DEXOS_V1_ADDRESS 可选,给了才测账户类接口。只发二十来个读请求(翻页 Limit 很小),不调 /v1/events。
func TestContractReadOnly(t *testing.T) {
	base := os.Getenv("DEXOS_V1_URL")
	if base == "" {
		t.Skip("没给 DEXOS_V1_URL")
	}
	spec := loadSpec(t)
	c := NewClient(strings.TrimRight(base, "/"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// conforms 原样取一次响应,对规格里 GET specPath 的 200 schema 校验。
	conforms := func(path string, q url.Values, specPath string) {
		t.Helper()
		status, _, raw, err := c.roundTrip(ctx, path, q, marketRead)
		if err != nil || status != 200 {
			t.Fatalf("GET %s: HTTP %d, %v: %s", path, status, err, raw)
		}
		if errs := validate(spec, responseSchema(t, spec, specPath, "200"), decodeJSON(t, raw), path); len(errs) > 0 {
			t.Errorf("GET %s 的响应不符合钉住的规格:\n  %s", path, strings.Join(errs, "\n  "))
		}
	}

	ex, err := c.Exchange(ctx)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if ex.FundingTickSec == nil || *ex.FundingTickSec == 0 {
		t.Errorf("fundingTickSec 应是正整数秒: %v", ex.FundingTickSec)
	}
	conforms("/v1/exchange", nil, "/v1/exchange")

	cat, err := c.Catalog(ctx)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(cat.Markets()) == 0 {
		t.Fatal("市场目录为空")
	}
	conforms("/v1/markets", nil, "/v1/markets")
	m := cat.Markets()[0]
	if m.MarginMode != "cross" && m.MarginMode != "isolated" {
		t.Errorf("%s 的 marginMode = %q", m.Symbol, m.MarginMode)
	}
	if b, err := c.Book(ctx, &m, 5); err != nil {
		t.Fatalf("Book %s: %v", m.Symbol, err)
	} else if b.Depth != 5 {
		t.Errorf("Book %s 的 depth = %d,问的是 5", m.Symbol, b.Depth)
	}
	conforms("/v1/markets/"+url.PathEscape(m.Symbol)+"/book", url.Values{"depth": {"5"}}, "/v1/markets/{symbol}/book")
	if _, err := c.Prices(ctx, cat); err != nil {
		t.Fatalf("Prices: %v", err)
	}
	conforms("/v1/prices", nil, "/v1/prices")
	if _, err := c.Tickers(ctx, cat); err != nil {
		t.Fatalf("Tickers: %v", err)
	}
	conforms("/v1/tickers", nil, "/v1/tickers")
	for _, iv := range []string{Interval2h, Interval4h, Interval12h, Interval1M} {
		if _, err := c.Candles(ctx, &m, CandleQuery{Interval: iv, Limit: 5}); err != nil {
			t.Fatalf("Candles %s %s: %v", m.Symbol, iv, err)
		}
		conforms("/v1/markets/"+url.PathEscape(m.Symbol)+"/candles", url.Values{"interval": {iv}, "limit": {"5"}}, "/v1/markets/{symbol}/candles")
	}

	addr := os.Getenv("DEXOS_V1_ADDRESS")
	if addr == "" {
		t.Log("没给 DEXOS_V1_ADDRESS,跳过账户类接口")
		return
	}
	a, err := dexos.ParseAddress(addr)
	if err != nil {
		t.Fatal(err)
	}
	ref := AccountRef{Address: a}
	q, err := ref.values()
	if err != nil {
		t.Fatal(err)
	}
	acc, err := c.Account(ctx, ref)
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if acc.Epoch == "" || acc.AsOfSeq == 0 {
		t.Errorf("账户汇总的纪元 / 视图序号: %q / %d", acc.Epoch, acc.AsOfSeq)
	}
	conforms("/v1/account", q, "/v1/account")
	if _, err := c.Positions(ctx, cat, ref); err != nil {
		t.Fatalf("Positions: %v", err)
	}
	conforms("/v1/account/positions", q, "/v1/account/positions")
	if _, err := c.Orders(ctx, cat, ref, ""); err != nil {
		t.Fatalf("Orders: %v", err)
	}
	conforms("/v1/account/orders", q, "/v1/account/orders")
	if _, err := c.Fills(ctx, cat, ref, "", HistoryQuery{PageSize: 3, Limit: 3}); err != nil {
		t.Fatalf("Fills: %v", err)
	}
	small := cloneValues(q)
	small.Set("limit", "3")
	conforms("/v1/account/fills", small, "/v1/account/fills")
	if _, err := c.Ledger(ctx, ref, LedgerQuery{HistoryQuery: HistoryQuery{PageSize: 3, Limit: 3}}); err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	conforms("/v1/account/ledger", small, "/v1/account/ledger")
	// 上界远超执行历史的完整前缀:稳定错误码 GatewayHistoryIncomplete
	far := cursorAt(acc.AsOfSeq + 1_000_000_000).String()
	if _, err := c.Ledger(ctx, ref, LedgerQuery{HistoryQuery: HistoryQuery{Epoch: acc.Epoch, Upper: far, Limit: 1}}); !errors.Is(err, dexos.ErrHistoryUpperIncomplete) {
		t.Errorf("上界 %s 应当是 ErrHistoryUpperIncomplete: %v", far, err)
	}
	if _, err := c.Nonce(ctx, ref, nil); err != nil {
		t.Fatalf("Nonce: %v", err)
	}
	conforms("/v1/account/nonce", q, "/v1/account/nonce")
	agents, err := c.Agents(ctx, ref)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	conforms("/v1/account/agents", q, "/v1/account/agents")
	if len(agents) > 0 {
		ag, err := dexos.ParseAddress(agents[0].Address)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.AgentGrants(ctx, ag); err != nil {
			t.Fatalf("AgentGrants: %v", err)
		}
		conforms("/v1/agents/"+ag.Hex(), nil, "/v1/agents/{address}")
	}
	if _, err := c.Portfolio(ctx, ref, Period7d); err != nil {
		t.Fatalf("Portfolio: %v", err)
	}
	pq := cloneValues(q)
	pq.Set("period", Period7d)
	conforms("/v1/account/portfolio", pq, "/v1/account/portfolio")
	if _, err := c.MyVaults(ctx, a); err != nil {
		t.Fatalf("MyVaults: %v", err)
	}
	conforms("/v1/account/vaults", url.Values{"address": {a.Hex()}}, "/v1/account/vaults")
}

// capture 记下每个写请求的响应,事后对钉住规格的 POST 响应 schema 校验(本包的类型只读它需要的字段,
// 规格外多出的字段与形状差异要靠原始响应才看得见)。
type capture struct {
	next http.RoundTripper
	mu   sync.Mutex
	got  []capturedResp
}

type capturedResp struct {
	path   string
	status int
	body   []byte
}

func (c *capture) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(r)
	if err != nil || r.Method != http.MethodPost {
		return resp, err
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	c.mu.Lock()
	c.got = append(c.got, capturedResp{path: r.URL.Path, status: resp.StatusCode, body: raw})
	c.mu.Unlock()
	return resp, nil
}

// 契约测试(写):对一台真实 dex-os 用**测试账户**走一遍写路径 —— 授权一把新生成的 API 钱包,
// 挂远离市价的只挂单(alo)再换单、撤单、断线保护、撤全部,主钱包单笔下单 / 撤单 / 设杠杆;
// 每个响应对钉住的规格做 schema 校验,最后核对本地 nonce 与服务端一致。默认不编译,要跑:
//
//	DEXOS_V1_URL=http://127.0.0.1:8080 DEXOS_V1_WRITE_KEY=0x… go test -tags dexos_contract -run ContractWrites ./dexosv1/
//
// DEXOS_V1_WRITE_KEY 必须是测试账户的主钱包私钥(例如本地栈的 anvil 测试账户),账户里要有 USDC。
// DEXOS_V1_WRITE_SYMBOL 可选,缺省用目录里第一个永续。挂的单价格是标记价的一半,不会成交。
func TestContractWrites(t *testing.T) {
	base, key := os.Getenv("DEXOS_V1_URL"), os.Getenv("DEXOS_V1_WRITE_KEY")
	if base == "" || key == "" {
		t.Skip("没给 DEXOS_V1_URL / DEXOS_V1_WRITE_KEY")
	}
	spec := loadSpec(t)
	rec := &capture{next: http.DefaultTransport}
	c := NewClient(strings.TrimRight(base, "/"))
	c.HTTP = &http.Client{Timeout: 30 * time.Second, Transport: rec}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	owner, err := dexos.NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m *Market
	if sym := os.Getenv("DEXOS_V1_WRITE_SYMBOL"); sym != "" {
		if m, err = cat.Market(sym); err != nil {
			t.Fatal(err)
		}
	} else {
		for i := range cat.Markets() {
			if mk := &cat.Markets()[i]; mk.Kind == "perp" && mk.MarkPrice != nil && *mk.MarkPrice > 1 {
				m = mk
				break
			}
		}
	}
	if m == nil || m.MarkPrice == nil {
		t.Fatal("找不到有标记价的市场")
	}
	far := *m.MarkPrice / 2
	// 数量取满足最小名义额的最小值再加 1 lot:名义额 = ticks × lots / 10^(pd+sd)
	minN, err := ParseFixed(m.MinNotional, m.Scale.PriceDecimals+m.Scale.SizeDecimals)
	if err != nil {
		t.Fatal(err)
	}
	lots := minN.Uint64()/uint64(far) + 1
	t.Logf("市场 %s:标记价 %s,挂 %s × %s(alo)", m.Symbol, m.Scale.Price(*m.MarkPrice), m.Scale.Price(far), m.Scale.Size(lots))

	w, err := NewWallet(ctx, c, owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := dexos.GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.ApproveAgent(ctx, agent.Address(), time.Now().Add(time.Hour), "sdk-contract"); err != nil {
		t.Fatalf("ApproveAgent: %v", err)
	}
	s, err := NewSession(ctx, c, agent, w.Account)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	o := OrderRequest{Market: m, Side: dexos.Buy, Price: far, Lots: lots, TIF: dexos.PostOnly}
	// 主钱包单笔下单 / 撤单放在撤全部之前:撤全部在下一个块边界才生效,其间新挂的单同样会被撤。
	single, err := w.PlaceOrder(ctx, o, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Wallet.PlaceOrder: %v", err)
	}
	if single.Status != "resting" {
		t.Fatalf("主钱包挂单: %+v", single)
	}
	if _, err := w.Cancel(ctx, m, single.OrderID); err != nil {
		t.Fatalf("Wallet.Cancel: %v", err)
	}
	placed, err := s.Place(ctx, o)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if it := placed.Items[0]; it.Status != "accepted" || it.OrderID == nil || it.Resting == nil || !*it.Resting {
		t.Fatalf("挂单没有挂上: %+v", it)
	}
	o2 := o
	o2.Price = far - 1
	// 换单的新单带 clientOrderId(v1.1 起支持);账户内唯一,按时间取
	o2.ClientOrderID = fmt.Sprintf("sdk-ct-%d", time.Now().UnixMilli())
	rep, err := s.Replace(ctx, m, []uint64{*placed.Items[0].OrderID}, []OrderRequest{o2})
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	var newID *uint64
	for _, it := range rep.Items {
		if it.Kind == "cancel" && it.Status != "canceled" || it.Kind == "place" && it.Status != "accepted" {
			t.Fatalf("换单项: %+v", it)
		}
		if it.Kind == "place" {
			newID = it.OrderID
			if it.ClientOrderID == nil || *it.ClientOrderID != o2.ClientOrderID {
				t.Errorf("换单新单没回显 clientOrderId: %+v", it)
			}
		}
	}
	if cr, err := s.Cancel(ctx, m, *newID); err != nil || cr.Items[0].Status != "canceled" {
		t.Fatalf("Cancel: %+v %v", cr, err)
	}
	if _, err := s.ScheduleCancel(ctx, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("ScheduleCancel: %v", err)
	}
	if _, err := s.ScheduleCancel(ctx, time.Time{}); err != nil {
		t.Fatalf("ScheduleCancel 清除: %v", err)
	}
	if _, err := s.CancelAll(ctx); err != nil {
		t.Fatalf("CancelAll: %v", err)
	}
	if _, err := w.SetLeverage(ctx, m, 0); err != nil {
		t.Fatalf("SetLeverage(0 = 市场缺省): %v", err)
	}

	// 本地 nonce 与服务端一致
	addr := agent.Address()
	if n, err := c.Nonce(ctx, w.Account, &addr); err != nil || n.Nonce != s.Nonce() {
		t.Errorf("agent nonce:服务端 %+v(%v),本地 %d", n, err, s.Nonce())
	}
	if n, err := c.Nonce(ctx, w.Account, nil); err != nil || n.Nonce != w.Nonce() {
		t.Errorf("账户 nonce:服务端 %+v(%v),本地 %d", n, err, w.Nonce())
	}

	// 每个写响应对钉住的规格校验
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, r := range rec.got {
		if r.status != 200 {
			t.Errorf("POST %s 回 %d: %s", r.path, r.status, r.body)
			continue
		}
		if errs := validate(spec, postResponseSchema(t, spec, r.path, "200"), decodeJSON(t, r.body), r.path); len(errs) > 0 {
			t.Errorf("POST %s 的响应不符合钉住的规格:\n  %s\n  原文 %s", r.path, strings.Join(errs, "\n  "), r.body)
		}
	}
	t.Logf("写了 %d 次,全部对过规格", len(rec.got))
}
