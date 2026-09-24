// contracts:对着真节点走一遍 dex-os #1–#5 的对接契约(成交 / 事件补拉 / 原请求回执 / 批量逐项 / 流水与权益)。
//
//	GW=http://127.0.0.1:18480 ADMIN_TOKEN=… go run ./examples/contracts [-dump <目录>]
//
// 需要一个**没有历史缺口**的节点(全新纪元)、一个已建好的永续市场 0,以及 token 模式的测试入金
// (/internal/deposit)。两个全新账户:A 挂单、B 吃单。每步 PASS / FAIL,任何 FAIL 退出码 1。
// -dump 把各读接口的原始响应写进目录,供单测做金样。只打印地址,不打印任何私钥。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

type report struct{ fails int }

func (r *report) check(step string, cond bool, detail string) {
	if cond {
		fmt.Printf("PASS  %-26s %s\n", step, detail)
		return
	}
	r.fails++
	fmt.Printf("FAIL  %-26s %s\n", step, detail)
}

func (r *report) must(step string, err error) {
	if err != nil {
		r.check(step, false, err.Error())
		os.Exit(1)
	}
}

type trader struct {
	master, agent *dexos.Signer
	s             *dexos.Session
}

func main() {
	dump := flag.String("dump", "", "把原始响应写进这个目录")
	flag.Parse()
	gw, admin := os.Getenv("GW"), os.Getenv("ADMIN_TOKEN")
	if gw == "" || admin == "" {
		fmt.Println("需要 GW 与 ADMIN_TOKEN")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r := &report{}
	c, cfg, err := dexos.Connect(ctx, gw)
	r.must("connect", err)
	r.check("connect", true, fmt.Sprintf("chainId=%d codec=v%d", cfg.ChainID, cfg.CodecVer))

	onboard := func(name string) trader {
		master, _ := dexos.GenerateSigner()
		agent, _ := dexos.GenerateSigner()
		var tx [32]byte
		_, _ = rand.Read(tx[:])
		body, _ := json.Marshal(map[string]any{"address": master.Address().Hex(), "amount": "5000000000", "txHash": "0x" + hex.EncodeToString(tx[:]), "logIndex": 0, "token": 0})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gw+"/internal/deposit?wait=fold", bytes.NewReader(body))
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-admin-token", admin)
		resp, err := http.DefaultClient.Do(req)
		r.must(name+"-deposit", err)
		resp.Body.Close()
		var ref *dexos.AccountRef
		for range 20 {
			if ref, err = c.AccountByAddress(ctx, master.Address().Hex()); err == nil {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		r.must(name+"-deposit", err)
		n, _ := c.NextNonce(ctx, ref.AccountID)
		_, err = c.ApproveAgent(ctx, master, ref.AccountID, agent.Address(), time.Now().Add(time.Hour), n)
		r.must(name+"-approve", err)
		s, err := dexos.New(ctx, gw, agent.PrivateKeyHex())
		r.must(name+"-session", err)
		r.check(name+"-onboard", s.Account == ref.AccountID, fmt.Sprintf("account=%d master=%s agent=%s", s.Account, master.Address(), agent.Address()))
		return trader{master, agent, s}
	}
	a, b := onboard("A"), onboard("B")
	const m = uint16(0)

	// ── #4 全成功:两张 PostOnly 同一次 Replace,逐项结果 + 按原请求身份查回执 ──
	first, err := a.s.ReplaceWithReceipt(ctx, m, nil, []dexos.BatchOrder{
		{Market: m, Side: dexos.Buy, Price: 99900, Lots: 5, TIF: dexos.PostOnly},
		{Market: m, Side: dexos.Sell, Price: 100100, Lots: 5, TIF: dexos.PostOnly},
	})
	r.must("replace-ok", err)
	items := first.Receipt.Items
	r.check("replace-ok", len(items) == 2 && items[0].State == "accepted" && items[1].State == "accepted", fmt.Sprintf("items=%d", len(items)))
	askSeq := *items[1].OrderID
	rid, _ := first.Request.RequestID()
	rc, err := a.s.Client.ReceiptOf(ctx, *first.Request)
	r.must("receipt-executed", err)
	r.check("receipt-executed", rc.Status == dexos.ReceiptExecuted && *rc.NonceConsumed && len(rc.Items) == 2 && *rc.Items[1].OrderID == askSeq,
		fmt.Sprintf("requestId=%s seq=%d items=%d", rid, *rc.Seq, len(rc.Items)))

	// ── #4 部分成功:撤旧卖单 + 挂新卖单 + 一张会穿价的 PostOnly 买单 → 终局,不锁会话 ──
	var seq uint64
	fmt.Sscan(askSeq, &seq)
	partial, err := a.s.ReplaceWithReceipt(ctx, m, []uint64{seq}, []dexos.BatchOrder{
		{Market: m, Side: dexos.Sell, Price: 100050, Lots: 5, TIF: dexos.PostOnly},
		{Market: m, Side: dexos.Buy, Price: 100200, Lots: 1, TIF: dexos.PostOnly},
	})
	var bo *dexos.BatchOutcomeError
	r.check("replace-partial", errors.As(err, &bo) && !errors.Is(err, dexos.ErrSessionBlocked) && len(bo.Rejected) == 1 && bo.Rejected[0].Kind == "place" && bo.Rejected[0].InputIndex == 1,
		fmt.Sprintf("err=%v", err))
	rc2, err := a.s.Client.ReceiptOf(ctx, *partial.Request)
	r.check("receipt-partial", err == nil && rc2.Status == dexos.ReceiptExecuted && len(rc2.Items) == 3, fmt.Sprintf("err=%v", err))

	// ── #3 从未发出的请求:无缺口的账本上是 not_found(带证明到的水位)──
	ghost := *first.Request
	ghost.Nonce += 1000
	var h [32]byte
	_, _ = rand.Read(h[:])
	ghost.SigningHash = "0x" + hex.EncodeToString(h[:])
	nf, err := a.s.Client.ReceiptOf(ctx, ghost)
	r.check("receipt-not-found", err == nil && nf.Status == dexos.ReceiptNotFound && nf.AsOfSeq != nil, fmt.Sprintf("err=%v", err))

	// ── 成交:B 吃 A 三次买 + 一次卖 ──
	for _, o := range []dexos.BatchOrder{
		{Market: m, Side: dexos.Buy, Price: 100050, Lots: 1, TIF: dexos.IOC},
		{Market: m, Side: dexos.Buy, Price: 100050, Lots: 1, TIF: dexos.IOC},
		{Market: m, Side: dexos.Buy, Price: 100050, Lots: 1, TIF: dexos.IOC},
		{Market: m, Side: dexos.Sell, Price: 99900, Lots: 2, TIF: dexos.IOC},
	} {
		_, err := b.s.Place(ctx, o)
		r.must("taker", err)
	}

	// ── #1 成交补拉:每页 1 笔翻到底,续页带回 epoch/upper;再从中间游标续拉 ──
	fills, err := a.s.Fills(ctx, dexos.FillQuery{PageSize: 1})
	r.must("fills", err)
	okFields := len(fills.Fills) >= 4
	for _, f := range fills.Fills {
		okFields = okFields && f.Role == "maker" && f.Order != 0 && f.Fee != nil && f.TS != nil
	}
	r.check("fills-paged", okFields && fills.Epoch != "" && fills.Upper != "", fmt.Sprintf("fills=%d epoch=%s upper=%s", len(fills.Fills), fills.Epoch, fills.Upper))
	tail, err := a.s.Fills(ctx, dexos.FillQuery{After: fills.Fills[1].ID, Epoch: fills.Epoch, PageSize: 1})
	r.check("fills-resume", err == nil && len(tail.Fills) == len(fills.Fills)-2 && tail.Fills[0].ID == fills.Fills[2].ID, fmt.Sprintf("err=%v", err))
	_, err = a.s.Fills(ctx, dexos.FillQuery{After: fills.Fills[1].ID, Epoch: "k0-0"})
	r.check("fills-epoch-mismatch", errors.Is(err, dexos.ErrHistoryEpochMismatch), fmt.Sprintf("err=%v", err))

	// ── #5 流水与同水位权益:Σ流水 == 余额;净入金 == 入金 ──
	eq, err := a.s.Client.Equity(ctx, a.s.Account, fills.Epoch)
	r.must("equity", err)
	ledger, err := a.s.Client.Ledger(ctx, a.s.Account, dexos.LedgerQuery{PageSize: 2})
	r.must("ledger", err)
	sum := new(big.Int)
	for _, e := range ledger.Entries {
		v, _ := new(big.Int).SetString(e.Amount, 10)
		sum.Add(sum, v)
	}
	// 快照之后 A 再没有动账,流水全量合计必须等于快照余额(服务端自己也核这一条,不等即 500)
	bal, _ := new(big.Int).SetString(eq.Balances[0].Balance, 10)
	r.check("equity", len(eq.Groups) == 1 && eq.Groups[0].NetInflow == "5000000000" && sum.Cmp(bal) == 0, fmt.Sprintf("equity=%s netInflow=%s balance=%s ledgerΣ=%s entries=%d upper=%s/%s",
		eq.Groups[0].Equity, eq.Groups[0].NetInflow, bal, sum, len(ledger.Entries), eq.Upper, ledger.Upper))

	// ── #2 事件补拉:历史里的 Fill 与 /fills 的身份逐字符相同 ──
	evs, err := a.s.Client.Events(ctx, dexos.EventQuery{PageSize: 50})
	r.must("events", err)
	ids := map[string]bool{}
	for _, e := range evs.Events {
		if e.Kind == "Fill" {
			ids[e.ID()] = true
		}
	}
	all := true
	for _, f := range fills.Fills {
		all = all && ids[f.ID]
	}
	r.check("events-backfill", all && evs.CompleteSeq > 0, fmt.Sprintf("events=%d fills-in-events=%v completeSeq=%d", len(evs.Events), all, evs.CompleteSeq))

	if *dump != "" {
		_ = os.MkdirAll(*dump, 0o755)
		get := func(name, path string) {
			resp, err := http.Get(gw + path)
			if err != nil {
				r.check("dump", false, err.Error())
				return
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			_ = os.WriteFile(filepath.Join(*dump, name+".json"), raw, 0o644)
		}
		acct := fmt.Sprint(a.s.Account)
		get("receipt_executed", "/receipts/"+rid)
		rid2, _ := partial.Request.RequestID()
		get("receipt_partial", "/receipts/"+rid2)
		gid, _ := ghost.RequestID()
		get("receipt_not_found", "/receipts/"+gid)
		get("fills_page", "/fills?account="+acct+"&limit=2")
		get("ledger_page", "/ledger?account="+acct+"&limit=3")
		get("equity", "/equity?account="+acct)
		golden, _ := json.MarshalIndent(map[string]string{"agent": first.Request.Agent.Hex(), "signingHash": first.Request.SigningHash, "requestId": rid}, "", "  ")
		_ = os.WriteFile(filepath.Join(*dump, "request_id_golden.json"), golden, 0o644)
		fmt.Printf("dumped to %s\n", *dump)
	}
	if r.fails > 0 {
		os.Exit(1)
	}
}
