// smoke:用一个**全新账号**把 SDK 的全链路走一遍,对着真节点。
//
//	GW=http://127.0.0.1:17807 ADMIN_TOKEN=… go run ./examples/smoke
//
// 流程:生成主钱包与 API 钱包 → 测试入金(/internal/deposit,只在 token 模式的测试栈可用)
// → 主钱包授权 API 钱包 → Session → 行情 / 挂单 / 改单 / 换单 / 撤单 / GTT / 杠杆 / 条件单 /
// TWAP / TP-SL / 吃单成交 / 成交账本补拉 / 事件流身份 / 现货两腿 / 业务拒绝不锁会话 /
// 断线保护 / 撤销授权。每步 PASS / FAIL,最后汇总;任何 FAIL 退出码 1。
//
// 只打印地址,不打印任何私钥。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

type report struct{ fails int }

func (r *report) ok(step string, detail string) { fmt.Printf("PASS  %-28s %s\n", step, detail) }
func (r *report) fail(step string, err any)     { r.fails++; fmt.Printf("FAIL  %-28s %v\n", step, err) }
func (r *report) check(step string, cond bool, detail string) {
	if cond {
		r.ok(step, detail)
	} else {
		r.fail(step, detail)
	}
}

func seqOf(events []dexos.EventEnvelope, kind string) (uint64, bool) {
	for _, ev := range events {
		if ev.Kind != kind {
			continue
		}
		var d struct {
			OrderSeq *uint64 `json:"orderSeq"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.OrderSeq != nil {
			return *d.OrderSeq, true
		}
	}
	return 0, false
}

func kinds(events []dexos.EventEnvelope) string {
	m := map[string]int{}
	for _, e := range events {
		m[e.Kind]++
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func main() {
	gw := os.Getenv("GW")
	if gw == "" {
		gw = "http://127.0.0.1:17807"
	}
	adminToken := os.Getenv("ADMIN_TOKEN")
	if adminToken == "" {
		fmt.Println("需要 ADMIN_TOKEN(测试入金用,只在 token 模式的测试栈上有)")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	r := &report{}

	// ── 0. 发现 ──
	c, cfg, err := dexos.Connect(ctx, gw)
	if err != nil {
		r.fail("connect", err)
		os.Exit(1)
	}
	r.ok("connect", fmt.Sprintf("chainId=%d codec=v%d snapshot=v%d", cfg.ChainID, cfg.CodecVer, cfg.SnapshotVer))

	// ── 1. 全新主钱包 + API 钱包 ──
	master, err := dexos.GenerateSigner()
	if err != nil {
		r.fail("keygen", err)
		os.Exit(1)
	}
	api, _ := dexos.GenerateSigner()
	r.ok("keygen", fmt.Sprintf("master=%s api=%s", master.Address(), api.Address()))

	// ── 2. 测试入金:5000 USDC(token 0,6 位小数)──
	var tx [32]byte
	_, _ = rand.Read(tx[:])
	body, _ := json.Marshal(map[string]any{
		"address": master.Address().Hex(), "amount": "5000000000",
		"txHash": "0x" + hex.EncodeToString(tx[:]), "logIndex": 0, "token": 0,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gw+"/internal/deposit?wait=fold", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-admin-token", adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.fail("deposit", err)
		os.Exit(1)
	}
	var dep struct {
		Status    string  `json:"status"`
		AccountID *uint32 `json:"accountId"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&dep)
	resp.Body.Close()
	if resp.StatusCode != 200 || dep.Status != "ok" {
		r.fail("deposit", fmt.Sprintf("HTTP %d status=%s", resp.StatusCode, dep.Status))
		os.Exit(1)
	}
	var ref *dexos.AccountRef
	for i := 0; i < 20; i++ {
		ref, err = c.AccountByAddress(ctx, master.Address().Hex())
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		r.fail("deposit", fmt.Sprintf("入金后地址仍查不到账户:%v", err))
		os.Exit(1)
	}
	r.check("deposit", dep.AccountID == nil || *dep.AccountID == ref.AccountID,
		fmt.Sprintf("account=%d collateral=%s", ref.AccountID, ref.Collateral))
	account := ref.AccountID

	// ── 3. 主钱包授权 API 钱包(master 的 nonce)──
	n, _ := c.NextNonce(ctx, account)
	ev, err := c.ApproveAgent(ctx, master, account, api.Address(), time.Now().Add(24*time.Hour), n)
	r.check("approve-agent", err == nil, fmt.Sprintf("events=%s err=%v", kinds(ev), err))

	// ── 4. 只用 API 钱包私钥建会话 ──
	s, err := dexos.New(ctx, gw, api.PrivateKeyHex())
	if err != nil {
		r.fail("session", err)
		os.Exit(1)
	}
	r.check("session", s.Account == account, fmt.Sprintf("session.Account=%d", s.Account))

	// ── 5. 行情 ──
	markets, err := s.Markets(ctx)
	var perp, spot *dexos.Market
	for i := range markets {
		m := &markets[i]
		if m.Symbol == "BTC-USD" && m.Kind == "perp" {
			perp = m
		}
		if m.Symbol == "BTC-USDC" && m.Kind == "spot" {
			spot = m
		}
	}
	if err != nil || perp == nil || spot == nil {
		r.fail("markets", fmt.Sprintf("err=%v perp=%v spot=%v", err, perp != nil, spot != nil))
		os.Exit(1)
	}
	r.ok("markets", fmt.Sprintf("%d 个市场;perp#%d bid/ask %d/%d;spot#%d kind=%s base=%v quote=%v",
		len(markets), perp.Market, perp.BestBid, perp.BestAsk, spot.Market, spot.Kind, *spot.Base, *spot.Quote))
	book, err := s.Book(ctx, perp.Market, 3)
	r.check("book", err == nil && len(book.Bids) > 0 && len(book.Asks) > 0, fmt.Sprintf("bids=%d asks=%d", len(book.Bids), len(book.Asks)))
	pm := perp.Market
	farBid := perp.BestBid - 2000 // 远离盘口,不会成交

	// ── 6. 先订上事件流,后面的成交要在流上看见 ──
	st, err := s.Subscribe(ctx, pm, spot.Market)
	r.check("subscribe", err == nil, fmt.Sprintf("err=%v", err))
	seen := map[string]dexos.Event{}
	var streamMu sync.Mutex
	var streamErr error
	go func() {
		for ev := range st.Events {
			streamMu.Lock()
			if ev.Kind == "Fill" {
				seen[ev.ID()] = ev
			}
			streamMu.Unlock()
		}
		if e, ok := <-st.Err; ok {
			streamMu.Lock()
			streamErr = e
			streamMu.Unlock()
		}
	}()

	// ── 7. 挂单 / 改单 / 换单 / 撤单 ──
	ev, err = s.Place(ctx, dexos.BatchOrder{Market: pm, Side: dexos.Buy, Price: farBid, Lots: 1, TIF: dexos.PostOnly})
	seq, got := seqOf(ev, "OrderAccepted")
	r.check("place-postonly", err == nil && got, fmt.Sprintf("seq=%d events=%s err=%v", seq, kinds(ev), err))

	ev, err = s.Modify(ctx, pm, seq, farBid+1, 1, dexos.GTC, false, time.Time{})
	seq2, got2 := seqOf(ev, "OrderAccepted")
	r.check("modify", err == nil && got2 && seq2 != seq, fmt.Sprintf("new seq=%d events=%s err=%v", seq2, kinds(ev), err))

	seqs, _ := s.OrderSeqs(ctx, pm)
	ev, err = s.Replace(ctx, pm, seqs, []dexos.BatchOrder{
		{Market: pm, Side: dexos.Buy, Price: farBid - 10, Lots: 1, TIF: dexos.GTC},
		{Market: pm, Side: dexos.Buy, Price: farBid - 20, Lots: 1, TIF: dexos.GTC},
	})
	after, _ := s.OrderSeqs(ctx, pm)
	r.check("replace", err == nil && len(after) == 2, fmt.Sprintf("cancel %d → resting %d events=%s err=%v", len(seqs), len(after), kinds(ev), err))

	ev, err = s.Cancel(ctx, pm, after...)
	left, _ := s.OrderSeqs(ctx, pm)
	r.check("cancel", err == nil && len(left) == 0, fmt.Sprintf("resting=%d events=%s err=%v", len(left), kinds(ev), err))

	ev, err = s.PlaceGTT(ctx, dexos.BatchOrder{Market: pm, Side: dexos.Buy, Price: farBid, Lots: 1, TIF: dexos.GTC}, time.Now().Add(time.Hour))
	gseq, ggot := seqOf(ev, "OrderAccepted")
	r.check("place-gtt", err == nil && ggot, fmt.Sprintf("seq=%d err=%v", gseq, err))
	if ggot {
		_, _ = s.Cancel(ctx, pm, gseq)
	}

	// ── 8. 杠杆(ppm;10× = 100000,高于 BTC 档位的 5%)──
	ev, err = s.SetLeverage(ctx, pm, dexos.ImfPpmForLeverage(10))
	r.check("set-leverage", err == nil, fmt.Sprintf("events=%s err=%v", kinds(ev), err))

	// ── 9. 条件单 / TWAP / TP-SL ──
	ev, err = s.PlaceConditional(ctx, dexos.Conditional{
		Market: pm, Side: dexos.Sell, Price: perp.BestBid - 5100, Lots: 1, TIF: dexos.IOC,
		TriggerPrice: perp.BestBid - 5000, TriggerAbove: false,
	})
	cseq, cgot := seqOf(ev, "ConditionalPlaced")
	r.check("place-conditional", err == nil && cgot, fmt.Sprintf("seq=%d events=%s err=%v", cseq, kinds(ev), err))
	if cgot {
		ev, err = s.CancelConditional(ctx, pm, cseq)
		r.check("cancel-conditional", err == nil, fmt.Sprintf("events=%s err=%v", kinds(ev), err))
	}
	ev, err = s.PlaceTwap(ctx, dexos.Twap{Market: pm, Side: dexos.Buy, TotalLots: 2, Slices: 2, IntervalMs: 60_000, PriceTolerancePpm: 10_000})
	tseq, tgot := seqOf(ev, "TwapPlaced")
	r.check("place-twap", err == nil && tgot, fmt.Sprintf("seq=%d events=%s err=%v", tseq, kinds(ev), err))
	if tgot {
		ev, err = s.CancelTwap(ctx, pm, tseq)
		r.check("cancel-twap", err == nil, fmt.Sprintf("events=%s err=%v", kinds(ev), err))
	}

	// ── 10. 吃单成交(IOC 买 1 lot ≈ $81)→ 成交账本 / 事件流身份 ──
	ev, err = s.Place(ctx, dexos.BatchOrder{Market: pm, Side: dexos.Buy, Price: perp.BestAsk + 50, Lots: 1, TIF: dexos.IOC})
	r.check("taker-fill", err == nil && len(ev) > 0, fmt.Sprintf("events=%s err=%v", kinds(ev), err))
	risk, err := s.Risk(ctx)
	var lots int64
	for _, p := range risk.Positions {
		if p.Market == pm {
			lots = p.Lots
		}
	}
	r.check("risk-position", err == nil && lots == 1, fmt.Sprintf("lots=%d nc=%s imr=%s balances=%d", lots, risk.NC, risk.IMR, len(risk.Balances)))

	// TP/SL 挂在持仓上(positionTpsl):止盈 +3000 限价,止损 −3000 市价
	ev, err = s.PlaceTpslPair(ctx, dexos.TpslPair{
		Market: pm, CloseSide: dexos.Sell, Lots: 1,
		TpTrigger: perp.BestAsk + 3000, TpPrice: perp.BestAsk + 2990,
		SlTrigger: perp.BestBid - 3000, SlPrice: perp.BestBid - 3100, PositionTpsl: true,
	})
	r.check("place-tpsl", err == nil, fmt.Sprintf("events=%s err=%v", kinds(ev), err))

	time.Sleep(1500 * time.Millisecond)
	// 账本按成交身份升序;**最后一笔**才是刚才那笔吃单。前面若还有别的:那是账本重置前
	// 旧内核里同一个账户号的历史(Fee=nil、Order=0,升级前就丢了源数据),分析库跨重置保留。
	fillsH, err := s.Fills(ctx, dexos.FillQuery{})
	var fills []dexos.Fill
	if fillsH != nil {
		fills = fillsH.Fills
	}
	var mine *dexos.Fill
	if len(fills) > 0 {
		mine = &fills[len(fills)-1]
	}
	feeOK := mine != nil && mine.Fee != nil && mine.Role == "taker" && mine.Side == "buy" && mine.Order != 0 && mine.CounterOrder != 0
	legacy := 0
	for _, f := range fills {
		if f.Fee == nil {
			legacy++
		}
	}
	r.check("fills-backfill", err == nil && feeOK, fmt.Sprintf("n=%d(其中 %d 笔为重置前旧内核的同号账户历史)newest=%+v fee=%v err=%v",
		len(fills), legacy, mine, deref(mine), err))
	if mine != nil {
		streamMu.Lock()
		_, onStream := seen[mine.ID]
		nFills, serr := len(seen), streamErr
		streamMu.Unlock()
		r.check("ws-fill-identity", onStream, fmt.Sprintf("账本 id %s 在事件流上%s(流上共 %d 笔 Fill,流错误=%v)", mine.ID,
			map[bool]string{true: "看到了", false: "没看到"}[onStream], nFills, serr))
		tail, err := s.Fills(ctx, dexos.FillQuery{After: mine.ID, Epoch: fillsH.Epoch})
		r.check("fills-cursor", err == nil && len(tail.Fills) == 0, fmt.Sprintf("after=%s → %d 笔 err=%v", mine.ID, len(tail.Fills), err))
	}

	// ── 11. 现货:买 100 lot BTC-USDC(≈ $8)→ base 余额出现;合约专属命令在现货上被拒 ──
	ev, err = s.Place(ctx, dexos.BatchOrder{Market: spot.Market, Side: dexos.Buy, Price: spot.BestAsk + 50, Lots: 100, TIF: dexos.IOC})
	r.check("spot-taker-fill", err == nil && len(ev) > 0, fmt.Sprintf("events=%s err=%v", kinds(ev), err))
	risk, _ = s.Risk(ctx)
	var baseBal string
	for _, b := range risk.Balances {
		if b.Token == *spot.Base {
			baseBal = b.Amount
		}
	}
	bv, _ := strconv.ParseInt(baseBal, 10, 64)
	r.check("spot-balance", bv > 0, fmt.Sprintf("token %d amount=%s", *spot.Base, baseBal))
	_, err = s.SetLeverage(ctx, spot.Market, dexos.ImfPpmForLeverage(5))
	var re *dexos.RejectedError
	r.check("spot-unsupported", errors.As(err, &re) && re.Reason == "SpotUnsupported", fmt.Sprintf("err=%v", err))

	// ── 12. 业务拒绝是终局结论、不锁会话:PostOnly 越价 → PostOnlyWouldCross,下一笔照常 ──
	_, err = s.Place(ctx, dexos.BatchOrder{Market: pm, Side: dexos.Buy, Price: perp.BestAsk + 100, Lots: 1, TIF: dexos.PostOnly})
	r.check("rejected-typed", errors.As(err, &re) && re.Reason == "PostOnlyWouldCross", fmt.Sprintf("err=%v", err))
	ev, err = s.Place(ctx, dexos.BatchOrder{Market: pm, Side: dexos.Buy, Price: farBid, Lots: 1, TIF: dexos.PostOnly})
	nseq, ngot := seqOf(ev, "OrderAccepted")
	r.check("not-blocked-after-reject", err == nil && ngot && !errors.Is(err, dexos.ErrSessionBlocked), fmt.Sprintf("seq=%d err=%v", nseq, err))
	if ngot {
		_, _ = s.Cancel(ctx, pm, nseq)
	}

	// ── 13. 平仓(只减仓 IOC 卖 1 lot)→ 断线保护 → 撤销授权 ──
	ev, err = s.Place(ctx, dexos.BatchOrder{Market: pm, Side: dexos.Sell, Price: perp.BestBid - 50, Lots: 1, TIF: dexos.IOC, ReduceOnly: true})
	r.check("close-position", err == nil, fmt.Sprintf("events=%s err=%v", kinds(ev), err))
	_, err = s.ScheduleCancel(ctx, time.Now().Add(30*time.Second))
	r.check("schedule-cancel", err == nil, fmt.Sprintf("err=%v", err))
	_, err = s.ScheduleCancel(ctx, time.Time{})
	r.check("schedule-cancel-clear", err == nil, fmt.Sprintf("err=%v", err))

	n, _ = c.NextNonce(ctx, account)
	ev, err = c.RevokeAgent(ctx, master, account, api.Address(), n)
	r.check("revoke-agent", err == nil, fmt.Sprintf("events=%s err=%v", kinds(ev), err))
	_, err = s.ScheduleCancel(ctx, time.Time{})
	r.check("revoked-agent-rejected", errors.As(err, &re) && re.Reason == "UnknownAgent", fmt.Sprintf("err=%v", err))

	fmt.Printf("\n%d 步失败\n", r.fails)
	if r.fails > 0 {
		os.Exit(1)
	}
}

func deref(f *dexos.Fill) string {
	if f == nil || f.Fee == nil {
		return "<nil>"
	}
	return *f.Fee
}
