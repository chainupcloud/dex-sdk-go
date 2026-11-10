package dexosv1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

// pairedClock 画线金样(paired_golden.json 第二组)的 nowMs。
const pairedClock = 1762070000000

// newPairedSession API 钱包 0x43…43 经 owner 级授权(位图 {1, 2})代同一 owner 的子账户 1(挂单)与 2(吃单)签。
// 时钟钉在金样的 nowMs,窗口 nonce 用独立的计数器(不受本进程别的测试影响)。
func newPairedSession(t *testing.T) (*writeFake, *PairedSession, *Market) {
	t.Helper()
	f, c := newWriteFake(t)
	f.routes["/v1/account/agents"] = serve(t, 200, "w_agents_owner.json")
	owner := mustAddr(t, ownerAddr)
	p, err := NewPairedSession(context.Background(), c, signer(t, agentKey), AccountRef{Address: owner, Subaccount: 1}, AccountRef{Address: owner, Subaccount: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !p.ValidUntil.Equal(time.UnixMilli(1893456000000)) {
		t.Fatalf("会话有效期应取授权的 validUntil: %s", p.ValidUntil)
	}
	p.now = func() time.Time { return time.UnixMilli(pairedClock) }
	p.nonces = &windowNonces{}
	cat, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	eth, err := cat.Market("ETH-USD")
	if err != nil {
		t.Fatal(err)
	}
	return f, p, eth
}

func goldenTrade(eth *Market) PairedTrade {
	return PairedTrade{Market: eth, MakerSide: dexos.Buy, Price: 827155, Lots: 12, ExpiresAt: time.UnixMilli(pairedClock + 60_000)}
}

// 会话发出的两条腿与 dex-os Rust 实现导出的金样逐字节相同;假网关按请求体独立重建两条腿的摘要并恢复签名者。
func TestPairedSessionSignsTheGoldenLegs(t *testing.T) {
	f, p, eth := newPairedSession(t)
	f.answer(t, "/v1/orders/paired", 200, "w_paired.json")
	res, err := p.Trade(context.Background(), goldenTrade(eth))
	if err != nil {
		t.Fatal(err)
	}
	g := loadPairedGolden(t).PairedTrades[1]
	body := m(f.last(t).body)
	conformsRequest(t, "/v1/orders/paired", body)
	if body["symbol"] != "ETH-USD" || body["price"] != "82715.5" || body["size"] != "0.12" || body["makerSide"] != "buy" || num(t, body["nowMs"]) != g.NowMs {
		t.Fatalf("外层: %v", body)
	}
	for role, want := range map[string]goldenLeg{"maker": g.Maker, "taker": g.Taker} {
		leg := m(body[role])
		if num(t, leg["sigVersion"]) != 2 || leg["account"] != ownerAddr || num(t, leg["subaccount"]) != uint64(want.Subaccount) || leg["signer"] != agentAddr ||
			num(t, leg["nonce"]) != want.Nonce || num(t, leg["expiresAt"]) != want.Expiry || leg["signature"] != want.Signature {
			t.Errorf("%s 腿与金样不同: %v", role, leg)
		}
		digest, who := gatewayPairedLeg(t, f.cat, body, role)
		if who != agentAddr || recoverAddr(t, digest, leg["signature"].(string)) != agentAddr {
			t.Errorf("%s 腿:网关恢复不出 API 钱包", role)
		}
	}
	want := PairedTradeResult{Status: "filled", Symbol: "ETH-USD", Price: 827155, Lots: 12, MakerOrderID: 301, TakerOrderID: 302}
	if res.Status != want.Status || res.Symbol != want.Symbol || res.Price != want.Price || res.Lots != want.Lots ||
		res.MakerOrderID != want.MakerOrderID || res.TakerOrderID != want.TakerOrderID || res.AvgPrice == nil || *res.AvgPrice != 827155 {
		t.Fatalf("回执: %+v", res)
	}
	if len(res.Fills) != 1 || res.Fills[0] != (ReceiptFill{FillID: "510-0-3", Price: 827155, Lots: 12, Fee: "4.96293", FeeToken: "USDC"}) {
		t.Fatalf("吃单腿成交: %+v", res.Fills)
	}
	if len(res.MakerFills) != 1 || res.MakerFills[0] != (ReceiptFill{FillID: "510-0-3", Price: 827155, Lots: 12, Fee: "-0.992586", FeeToken: "USDC"}) {
		t.Fatalf("挂单腿成交: %+v", res.MakerFills)
	}
	wantFees := PairedLegFees{
		Maker: PairedLegFee{Fee: "-0.992586", FeeToken: "USDC", Protocol: "-0.992586", Deployer: "0"},
		Taker: PairedLegFee{Fee: "6.451809", FeeToken: "USDC", Protocol: "4.96293", Deployer: "1.488879"},
	}
	if res.LegFees == nil || *res.LegFees != wantFees {
		t.Fatalf("两腿手续费: %+v", res.LegFees)
	}
	id := res.Request
	if res.Seq != 510 || res.Sub != 0 || id.RequestID != g.RequestID || id.Scope != "signer:"+agentAddr || id.Nonce != g.Maker.Nonce ||
		id.Signer != mustAddr(t, agentAddr) || id.SigningHash != g.Maker.Digest || id.CommandHash != "" {
		t.Fatalf("请求身份应是挂单腿的签名身份: %+v", id)
	}
	if q := id.Query(); q.Scope != "signer:"+agentAddr || *q.Nonce != g.Maker.Nonce || q.CommandHash != "" {
		t.Fatalf("回执查询: %+v", q)
	}
	// 同一毫秒里的下一笔:两条腿的 nonce 接着严格递增
	if _, err := p.Trade(context.Background(), goldenTrade(eth)); err != nil {
		t.Fatal(err)
	}
	next := m(f.last(t).body)
	if num(t, m(next["maker"])["nonce"]) != pairedClock+2 || num(t, m(next["taker"])["nonce"]) != pairedClock+3 {
		t.Fatalf("下一笔的 nonce: %v / %v", m(next["maker"])["nonce"], m(next["taker"])["nonce"])
	}
}

// 结局分两类(同 nonceGuard 的口径):执行层拒绝(422)与定序前的拒绝是终局结论,不锁会话;202、5xx、回执对不上
// 是结论未知,锁定会话并给出挂单腿的请求身份,之后不再发请求。窗口 nonce 没有计数器,所以 422 也不需要锁。
func TestPairedSessionOutcomes(t *testing.T) {
	for _, c := range []struct {
		name    string
		status  int
		fixture string
		raw     string
		blocked bool
		check   func(error) bool
	}{
		{name: "价差外", status: 422, fixture: "err422_paired_spread.json", check: func(err error) bool {
			var e *Error
			return errors.As(err, &e) && e.Code == "PairedTradeOutsideSpread" && e.Num == 20127
		}},
		{name: "被拒那条腿的真因", status: 422, fixture: "err422_margin.json", check: func(err error) bool {
			var e *Error
			return errors.As(err, &e) && e.Code == "InsufficientMargin"
		}},
		{name: "名单外", status: 403, fixture: "err403_paired.json", check: func(err error) bool {
			var e *Error
			return errors.As(err, &e) && e.Status == 403 && detailString(e.Details, "reason") == "paired_trade_not_allowed" && detailString(e.Details, "leg") == "taker"
		}},
		{name: "接口未开", status: 404, fixture: "err404_route.json", check: func(err error) bool {
			var e *Error
			return errors.As(err, &e) && e.Status == 404 && e.Code == "GatewayNotFound"
		}},
		{name: "腿签名不对", status: 401, fixture: "err401_sig.json"},
		{name: "账户桶限流", status: 429, fixture: "err429_account.json", check: func(err error) bool {
			var rl *RateLimitedError
			return errors.As(err, &rl) && rl.Bucket == "account"
		}},
		{name: "202 结论未知", status: 202, fixture: "w_sequenced_202.json", blocked: true, check: func(err error) bool { return errors.Is(err, ErrPending) }},
		{name: "5xx", status: 500, fixture: "err404.json", blocked: true},
		{name: "执行结果落库背压", status: 503, fixture: "err503_backpressure.json"},
		{name: "回执不是 filled", status: 200, raw: `{"status":"ok","symbol":"ETH-USD","seq":9,"sub":0,"requestId":"` + testReqID + `"}`, blocked: true},
		{name: "requestId 对不上", status: 200, raw: `{"status":"filled","symbol":"ETH-USD","seq":9,"sub":0,"requestId":"0x` + strings.Repeat("cd", 32) + `"}`, blocked: true},
		{name: "legFees 缺一条腿", status: 200, raw: pairedReceiptWith(t, `{"taker":{"fee":"1","feeToken":"USDC","protocol":"1","deployer":"0"}}`), blocked: true},
		{name: "legFees 缺 feeToken", status: 200, raw: pairedReceiptWith(t, `{"maker":{"fee":"0","protocol":"0","deployer":"0"},"taker":{"fee":"1","feeToken":"USDC","protocol":"1","deployer":"0"}}`), blocked: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, p, eth := newPairedSession(t)
			if c.raw != "" {
				f.answerRaw(t, "/v1/orders/paired", c.status, []byte(c.raw))
			} else {
				f.answer(t, "/v1/orders/paired", c.status, c.fixture)
			}
			res, err := p.Trade(context.Background(), goldenTrade(eth))
			if err == nil || res != nil {
				t.Fatalf("应报错: %+v %v", res, err)
			}
			if c.check != nil && !c.check(err) {
				t.Fatalf("错误类型不对: %v", err)
			}
			if blocked := errors.Is(err, ErrSessionBlocked); blocked != c.blocked {
				t.Fatalf("锁定应为 %v: %v", c.blocked, err)
			}
			before := f.count()
			_, again := p.Trade(context.Background(), goldenTrade(eth))
			if !c.blocked {
				if p.Pending() != nil || f.count() != before+1 {
					t.Fatalf("终局结论之后应能接着发: pending %+v,发了 %d 次", p.Pending(), f.count()-before)
				}
				return
			}
			id := p.Pending()
			sent := m(f.writes[0].body)
			if id == nil || id.RequestID != gatewayRequestID(t, f.cat, "/v1/orders/paired", sent) || id.Nonce != num(t, m(sent["maker"])["nonce"]) {
				t.Fatalf("锁定后应给出挂单腿的请求身份: %+v", id)
			}
			if !errors.Is(again, ErrSessionBlocked) || f.count() != before {
				t.Fatalf("锁定后应直接拒绝且不发请求: %v", again)
			}
		})
	}
}

// pairedReceiptWith w_paired.json 换掉 legFees;legFees 为空串 = 加这两个字段之前的网关(没有 legFees 与 makerFills)。
func pairedReceiptWith(t *testing.T, legFees string) string {
	t.Helper()
	var r map[string]json.RawMessage
	if err := json.Unmarshal(fixture(t, "w_paired.json"), &r); err != nil {
		t.Fatal(err)
	}
	if legFees == "" {
		delete(r, "legFees")
		delete(r, "makerFills")
	} else {
		r["legFees"] = json.RawMessage(legFees)
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// dex-os 加 makerFills / legFees 之前的网关:两项为空,不算回执对不上。
func TestPairedResultFromGatewayWithoutLegFees(t *testing.T) {
	f, p, eth := newPairedSession(t)
	f.answerRaw(t, "/v1/orders/paired", 200, []byte(pairedReceiptWith(t, "")))
	res, err := p.Trade(context.Background(), goldenTrade(eth))
	if err != nil {
		t.Fatal(err)
	}
	if res.LegFees != nil || res.MakerFills != nil || len(res.Fills) != 1 {
		t.Fatalf("老网关的回执: %+v", res)
	}
}

// 会话并发安全:同时成交的多笔串行发出,每条腿的窗口 nonce 各不相同,吃单腿紧跟挂单腿。
func TestPairedSessionConcurrentTrades(t *testing.T) {
	f, p, eth := newPairedSession(t)
	f.answer(t, "/v1/orders/paired", 200, "w_paired.json")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				if _, err := p.Trade(context.Background(), goldenTrade(eth)); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	seen := map[uint64]bool{}
	for _, w := range f.writes {
		maker, taker := num(t, m(m(w.body)["maker"])["nonce"]), num(t, m(m(w.body)["taker"])["nonce"])
		if taker != maker+1 || seen[maker] || seen[taker] {
			t.Fatalf("nonce 重复或不连续: %d / %d", maker, taker)
		}
		seen[maker], seen[taker] = true, true
	}
	if len(seen) != 80 {
		t.Fatalf("40 笔应用 80 个不同的 nonce,得到 %d", len(seen))
	}
}

// 本地能判的错在签名之前报,不发请求、不锁会话。
func TestPairedSessionLocalChecks(t *testing.T) {
	f, p, eth := newPairedSession(t)
	for name, tr := range map[string]PairedTrade{
		"没有市场":  {MakerSide: dexos.Buy, Price: 1, Lots: 1},
		"方向非法":  {Market: eth, MakerSide: 7, Price: 1, Lots: 1},
		"价格为 0": {Market: eth, MakerSide: dexos.Sell, Lots: 1},
		"数量为 0": {Market: eth, MakerSide: dexos.Sell, Price: 1},
	} {
		if _, err := p.Trade(context.Background(), tr); err == nil {
			t.Errorf("%s: 应报错", name)
		}
	}
	p.ValidUntil = time.UnixMilli(pairedClock)
	if _, err := p.Trade(context.Background(), goldenTrade(eth)); !errors.Is(err, ErrAgentExpired) {
		t.Fatalf("授权到期应本地拦住: %v", err)
	}
	if f.count() != 0 || p.Pending() != nil {
		t.Fatalf("本地校验失败不该发请求或锁定: 发了 %d 次", f.count())
	}
}

// 两条腿都要能签:API 钱包对每条腿的账户有按账户授权或覆盖该编号的 owner 级授权;owner 本人签不需要授权。
func TestNewPairedSessionChecksBothLegs(t *testing.T) {
	owner := mustAddr(t, ownerAddr)
	vault := uint16(3)
	for _, c := range []struct {
		name         string
		key          string
		maker, taker AccountRef
		rows         string
		want         error
	}{
		{name: "吃单腿不在位图里", key: agentKey, maker: AccountRef{Address: owner, Subaccount: 1}, taker: AccountRef{Address: owner, Subaccount: 3}, want: ErrAgentNotAuthorized},
		{name: "授权过期", key: agentKey, maker: AccountRef{Address: owner, Subaccount: 1}, taker: AccountRef{Address: owner, Subaccount: 2},
			rows: `{"address":"` + agentAddr + `","scope":"owner","subaccountMask":"6","subaccounts":[1,2],"label":"","validUntil":1,"expired":true}`, want: ErrAgentExpired},
		{name: "owner 本人签", key: ownerKey, maker: AccountRef{Address: owner, Subaccount: 1}, taker: AccountRef{Address: owner, Subaccount: 3}, rows: "-"},
		{name: "两条腿同一账户", key: agentKey, maker: AccountRef{Address: owner, Subaccount: 1}, taker: AccountRef{Address: owner, Subaccount: 1}, want: errLocal},
		{name: "金库身份", key: agentKey, maker: AccountRef{Vault: &vault}, taker: AccountRef{Address: owner, Subaccount: 2}, want: errLocal},
		{name: "编号超出 u16", key: agentKey, maker: AccountRef{Address: owner, Subaccount: 1}, taker: AccountRef{Address: owner, Subaccount: 1 << 16}, want: errLocal},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, cl := newWriteFake(t)
			switch c.rows {
			case "":
				f.routes["/v1/account/agents"] = serve(t, 200, "w_agents_owner.json")
			case "-":
				f.routes["/v1/account/agents"] = func(w http.ResponseWriter, _ *http.Request) {
					t.Error("owner 本人签不用查授权")
				}
			default:
				f.routes["/v1/account/agents"] = func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(`{"address":"` + ownerAddr + `","subaccount":1,"agents":[` + c.rows + `]}`))
				}
			}
			p, err := NewPairedSession(context.Background(), cl, signer(t, c.key), c.maker, c.taker)
			switch {
			case c.want == nil:
				if err != nil || p == nil || !p.ValidUntil.IsZero() {
					t.Fatalf("应建成且不过期: %+v %v", p, err)
				}
			case c.want == errLocal:
				if err == nil || len(f.requests()) != 0 {
					t.Fatalf("应在本地报错、不发请求: %v", err)
				}
			case !errors.Is(err, c.want):
				t.Fatalf("应为 %v,得到 %v", c.want, err)
			}
		})
	}
}

var errLocal = errors.New("本地报错")

type pairedJournal struct {
	before, after []PairedTradeIntent
	results       []*PairedTradeResult
	errs          []error
	beforeErr     error
	afterErr      error
}

func (j *pairedJournal) BeforeSend(_ context.Context, in PairedTradeIntent) error {
	j.before = append(j.before, in)
	return j.beforeErr
}

func (j *pairedJournal) AfterReceive(_ context.Context, in PairedTradeIntent, r *PairedTradeResult, err error) error {
	j.after = append(j.after, in)
	j.results, j.errs = append(j.results, r), append(j.errs, err)
	return j.afterErr
}

// 写前日志:签好之后、发送之前拿到请求身份(做市重启后凭它收口未决请求),拿到结论之后再记一次。
func TestPairedTradeJournaled(t *testing.T) {
	f, p, eth := newPairedSession(t)
	f.answer(t, "/v1/orders/paired", 200, "w_paired.json")
	j := &pairedJournal{}
	res, err := p.TradeJournaled(context.Background(), goldenTrade(eth), j)
	if err != nil {
		t.Fatal(err)
	}
	g := loadPairedGolden(t).PairedTrades[1]
	in := j.before[0]
	if len(j.before) != 1 || len(j.after) != 1 || in.Request.RequestID != g.RequestID || res.Request.RequestID != g.RequestID ||
		in.NowMs != pairedClock || in.MakerNonce != g.Maker.Nonce || in.TakerNonce != g.Taker.Nonce ||
		in.Maker != p.Maker || in.Taker != p.Taker || in.Domain != testDomain || in.Trade.Price != 827155 || j.results[0] != res || j.errs[0] != nil {
		t.Fatalf("写前日志: %+v / %+v", j, in)
	}

	// 落账前拒发:不发请求、不锁
	sent := f.count()
	j = &pairedJournal{beforeErr: ErrRequestDeclined}
	if _, err := p.TradeJournaled(context.Background(), goldenTrade(eth), j); !errors.Is(err, ErrNotSent) || errors.Is(err, ErrSessionBlocked) || f.count() != sent || len(j.after) != 0 {
		t.Fatalf("拒发: %v", err)
	}
	// AfterReceive 失败:结论只在内存里 → 锁定
	j = &pairedJournal{afterErr: errors.New("disk full")}
	if _, err := p.TradeJournaled(context.Background(), goldenTrade(eth), j); !errors.Is(err, ErrSessionBlocked) || p.Pending() == nil ||
		p.Pending().RequestID != j.before[0].Request.RequestID {
		t.Fatalf("AfterReceive 失败应锁定并给出请求身份: %v", err)
	}

	// BeforeSend 失败(本地可能已留下记录):锁定,不发请求
	f2, p2, _ := newPairedSession(t)
	f2.answer(t, "/v1/orders/paired", 200, "w_paired.json")
	j = &pairedJournal{beforeErr: errors.New("db down")}
	if _, err := p2.TradeJournaled(context.Background(), goldenTrade(eth), j); !errors.Is(err, ErrSessionBlocked) || f2.count() != 0 {
		t.Fatalf("BeforeSend 失败应锁定且不发: %v", err)
	}
	if _, err := p2.TradeJournaled(context.Background(), goldenTrade(eth), nil); err == nil {
		t.Fatal("缺写前日志应报错")
	}
}
