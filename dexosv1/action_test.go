package dexosv1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

type goldenActionOrder struct {
	Side       string `json:"side"`
	Price      uint32 `json:"price"`
	Lots       uint64 `json:"lots,string"`
	TIF        string `json:"tif"`
	ReduceOnly bool   `json:"reduceOnly"`
}

type goldenActionModify struct {
	Order      uint64 `json:"order"`
	Price      uint32 `json:"price"`
	Lots       uint64 `json:"lots,string"`
	TIF        string `json:"tif"`
	ReduceOnly bool   `json:"reduceOnly"`
}

type goldenAction struct {
	Name       string       `json:"name"`
	Domain     goldenDomain `json:"domain"`
	Owner      string       `json:"owner"`
	Subaccount uint16       `json:"subaccount"`
	SignerKey  string       `json:"signerKey"`
	Signer     string       `json:"signer"`
	Nonce      uint64       `json:"nonce,string"`
	Expiry     uint64       `json:"expiry,string"`
	NowMs      uint64       `json:"nowMs,string"`
	Inputs     struct {
		Market   uint16               `json:"market"`
		Cancels  []uint64             `json:"cancels"`
		Modifies []goldenActionModify `json:"modifies"`
		Orders   json.RawMessage      `json:"orders"`
	} `json:"inputs"`
	Inner     string `json:"inner"`
	Action    string `json:"action"`
	Digest    string `json:"digest"`
	Signature string `json:"signature"`
	RequestID string `json:"requestId"`
}

func loadActionGolden(t *testing.T) []goldenAction {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "v2_quote_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		ActorPlaceholder uint32         `json:"actorPlaceholder"`
		Vectors          []goldenAction `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if g.ActorPlaceholder != dexos.ActorPlaceholder || len(g.Vectors) == 0 {
		t.Fatalf("金样占位值 %d、向量 %d 个", g.ActorPlaceholder, len(g.Vectors))
	}
	return g.Vectors
}

var goldenTIF = map[string]dexos.TimeInForce{"gtc": dexos.GTC, "ioc": dexos.IOC, "alo": dexos.PostOnly, "fok": dexos.FOK}
var goldenSide = map[string]dexos.Side{"buy": dexos.Buy, "sell": dexos.Sell}

func (v goldenAction) inner(t *testing.T) []byte {
	t.Helper()
	ids := func(seqs []uint64) []dexos.OrderID {
		out := make([]dexos.OrderID, len(seqs))
		for i, s := range seqs {
			out[i] = dexos.NewOrderID(v.Inputs.Market, s)
		}
		return out
	}
	switch v.Name {
	case "replace", "replace_modify":
		var orders []goldenActionOrder
		if err := json.Unmarshal(v.Inputs.Orders, &orders); err != nil {
			t.Fatal(err)
		}
		batch := make([]dexos.BatchOrder, len(orders))
		for i, o := range orders {
			batch[i] = dexos.BatchOrder{Market: v.Inputs.Market, Side: goldenSide[o.Side], Price: o.Price, Lots: o.Lots, TIF: goldenTIF[o.TIF], ReduceOnly: o.ReduceOnly}
		}
		if v.Name == "replace" {
			return dexos.EncodeBatchReplace(dexos.ActorPlaceholder, v.NowMs, ids(v.Inputs.Cancels), batch)
		}
		mods := make([]dexos.BatchModify, len(v.Inputs.Modifies))
		for i, x := range v.Inputs.Modifies {
			mods[i] = dexos.BatchModify{Order: dexos.NewOrderID(v.Inputs.Market, x.Order), Price: x.Price, Lots: x.Lots, TIF: goldenTIF[x.TIF], ReduceOnly: x.ReduceOnly}
		}
		return dexos.EncodeBatchReplace2(dexos.ActorPlaceholder, v.NowMs, ids(v.Inputs.Cancels), mods, batch)
	case "cancel_one", "cancel_batch":
		var seqs []uint64
		if err := json.Unmarshal(v.Inputs.Orders, &seqs); err != nil {
			t.Fatal(err)
		}
		if v.Name == "cancel_one" {
			return dexos.EncodeCancelOrder(dexos.ActorPlaceholder, dexos.NewOrderID(v.Inputs.Market, seqs[0]))
		}
		return dexos.EncodeBatchCancel(dexos.ActorPlaceholder, ids(seqs))
	}
	t.Fatalf("金样 %s 不认识", v.Name)
	return nil
}

func TestActionGoldenVectors(t *testing.T) {
	for _, v := range loadActionGolden(t) {
		t.Run(v.Name+"/"+strconv.FormatUint(v.Domain.ChainID, 10), func(t *testing.T) {
			inner := v.inner(t)
			if got := hex0x(inner); got != v.Inner {
				t.Fatalf("内层编码 %s,金样 %s", got, v.Inner)
			}
			if got := hex0x(dexos.Keccak256(inner)); got != v.Action {
				t.Fatalf("action %s,金样 %s", got, v.Action)
			}
			digest := v.Domain.domain(t).ActionHash(mustAddr(t, v.Owner), v.Subaccount, inner, v.Nonce, v.Expiry)
			if got := hex0x(digest); got != v.Digest {
				t.Fatalf("摘要 %s,金样 %s", got, v.Digest)
			}
			key := signer(t, v.SignerKey)
			if key.Address().Hex() != v.Signer {
				t.Fatalf("签名者 %s,私钥推出 %s", v.Signer, key.Address().Hex())
			}
			if sig, err := key.SignHashHex(digest); err != nil || sig != v.Signature {
				t.Fatalf("签名 %s(%v),金样 %s", sig, err, v.Signature)
			}
			if id := newIdentity(key.Address(), "signer:"+key.Address().Hex(), v.Nonce, digest, nil); id.RequestID != v.RequestID {
				t.Fatalf("请求身份 %s,金样 %s", id.RequestID, v.RequestID)
			}
		})
	}
}

func goldenFor(t *testing.T, name string, chain uint64) goldenAction {
	t.Helper()
	for _, v := range loadActionGolden(t) {
		if v.Name == name && v.Domain.ChainID == chain {
			return v
		}
	}
	t.Fatalf("没有金样 %s/%d", name, chain)
	return goldenAction{}
}

func newActionWriter(t *testing.T, floor uint64, now int64) (*writeFake, *ActionWriter, *Market) {
	t.Helper()
	f, c := newWriteFake(t)
	f.routes["/v1/account/agents"] = serve(t, 200, "w_agents_owner.json")
	w, err := NewActionWriter(context.Background(), c, signer(t, agentKey), AccountRef{Address: mustAddr(t, ownerAddr), Subaccount: 1}, NewNonceSource(floor))
	if err != nil {
		t.Fatal(err)
	}
	w.now = func() time.Time { return time.UnixMilli(now) }
	cat, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	btc, err := cat.Market("BTC-USD")
	if err != nil {
		t.Fatal(err)
	}
	return f, w, btc
}

func TestActionWriterSignsTheGoldenReplace(t *testing.T) {
	for _, name := range []string{"replace", "replace_modify"} {
		g := goldenFor(t, name, testDomain.ChainID)
		f, w, btc := newActionWriter(t, g.Nonce-1, int64(g.NowMs))
		var orders []goldenActionOrder
		if err := json.Unmarshal(g.Inputs.Orders, &orders); err != nil {
			t.Fatal(err)
		}
		reqs := make([]OrderRequest, len(orders))
		for i, o := range orders {
			reqs[i] = OrderRequest{Market: btc, Side: goldenSide[o.Side], Price: o.Price, Lots: o.Lots, TIF: goldenTIF[o.TIF], ReduceOnly: o.ReduceOnly}
		}
		var mods []ModifyRequest
		for _, x := range g.Inputs.Modifies {
			mods = append(mods, ModifyRequest{OrderID: x.Order, Price: x.Price, Lots: x.Lots, TIF: goldenTIF[x.TIF], ReduceOnly: x.ReduceOnly})
		}
		a, err := w.SignReplace(btc, g.Inputs.Cancels, mods, reqs, time.UnixMilli(int64(g.Expiry)))
		if err != nil {
			t.Fatal(err)
		}
		if hex0x(a.Command) != g.Inner || a.Request.RequestID != g.RequestID || a.Request.SigningHash != g.Digest || a.Request.Nonce != g.Nonce ||
			a.Request.Scope != "signer:"+agentAddr || a.Request.CommandHash != "" {
			t.Fatalf("%s 与金样不同: %+v", name, a.Request)
		}
		body := m(decodeJSON(t, a.Body))
		conformsRequest(t, "/v1/orders/replace", body)
		if num(t, body["sigVersion"]) != 2 || body["account"] != ownerAddr || num(t, body["subaccount"]) != 1 || body["signer"] != agentAddr ||
			num(t, body["nonce"]) != g.Nonce || num(t, body["expiresAt"]) != g.Expiry || num(t, body["nowMs"]) != g.NowMs || body["signature"] != g.Signature {
			t.Fatalf("%s 信封与金样不同: %v", name, body)
		}
		if got := gatewayRequestID(t, f.cat, "/v1/orders/replace", decodeJSON(t, a.Body)); got != g.RequestID {
			t.Fatalf("%s:网关按请求体算出的身份 %s,金样 %s", name, got, g.RequestID)
		}
	}
}

func TestActionWriterSignsTheGoldenCancels(t *testing.T) {
	for _, name := range []string{"cancel_one", "cancel_batch"} {
		g := goldenFor(t, name, testDomain.ChainID)
		f, w, btc := newActionWriter(t, 0, int64(g.NowMs))
		var seqs []uint64
		if err := json.Unmarshal(g.Inputs.Orders, &seqs); err != nil {
			t.Fatal(err)
		}
		var expiry time.Time
		if g.Expiry != 0 {
			expiry = time.UnixMilli(int64(g.Expiry))
		}
		a, err := w.SignCancelAt(btc, g.Nonce, seqs, expiry)
		if err != nil {
			t.Fatal(err)
		}
		if hex0x(a.Command) != g.Inner || a.Request.RequestID != g.RequestID || a.Request.Nonce != g.Nonce {
			t.Fatalf("%s 与金样不同: %+v", name, a.Request)
		}
		if got := gatewayRequestID(t, f.cat, "/v1/orders/cancel", decodeJSON(t, a.Body)); got != g.RequestID {
			t.Fatalf("%s:网关按请求体算出的身份 %s,金样 %s", name, got, g.RequestID)
		}
		if w.nonces.Last() != 0 {
			t.Fatalf("%s:给定 nonce 的撤单不该动 nonce 来源: %d", name, w.nonces.Last())
		}
	}
}

func TestActionSendAndResendIsTheSameRequest(t *testing.T) {
	f, w, btc := newActionWriter(t, 0, 1793680000123)
	f.answer(t, "/v1/orders/replace", 200, "w_replace.json")
	a, err := w.SignReplace(btc, []uint64{101}, nil, []OrderRequest{{Market: btc, Side: dexos.Buy, Price: 64900, Lots: 200, TIF: dexos.IOC}}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := w.Client.SendReplace(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if first.Request.RequestID != a.Request.RequestID || len(first.Items) != 2 || first.Seq != 502 {
		t.Fatalf("回执: %+v", first)
	}
	f.answerRaw(t, "/v1/orders/replace", 200, duplicateOf(t, fixture(t, "w_replace.json")))
	again, err := w.Client.SendReplace(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Duplicate || again.Seq != first.Seq || again.Request.RequestID != a.Request.RequestID {
		t.Fatalf("原样重发应拿到原来那一次的结论: %+v", again)
	}
	if len(f.writes) != 2 || string(f.writes[0].raw) != string(f.writes[1].raw) || string(f.writes[0].raw) != string(a.Body) {
		t.Fatal("原样重发必须是同一份字节")
	}
}

func duplicateOf(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	v["duplicate"] = true
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestActionSendCancel(t *testing.T) {
	f, w, btc := newActionWriter(t, 0, 1793680000123)
	f.answer(t, "/v1/orders/cancel", 200, "w_cancel_one.json")
	one, err := w.SignCancel(btc, []uint64{202}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := w.Client.SendCancel(context.Background(), one)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Items) != 1 || r.Items[0].Kind != "cancel" || r.Items[0].Status != "canceled" || *r.Items[0].OrderID != 202 || *r.Items[0].CanceledLots != 10 {
		t.Fatalf("撤一张: %+v", r.Items)
	}
	f.answer(t, "/v1/orders/cancel", 200, "w_batch_cancel.json")
	batch, err := w.SignCancel(btc, []uint64{101, 999999}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	r, err = w.Client.SendCancel(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Items) != 2 || r.Items[1].Status != "rejected" || r.Items[1].Code != "UnknownOrder" {
		t.Fatalf("批量撤: %+v", r.Items)
	}
	if one.Request.Nonce >= batch.Request.Nonce {
		t.Fatalf("同一毫秒的下一笔 nonce 应递增: %d → %d", one.Request.Nonce, batch.Request.Nonce)
	}
}

func TestClassifyAction(t *testing.T) {
	f, w, btc := newActionWriter(t, 0, 1793680000123)
	cases := []struct {
		status  int
		fixture string
		want    ActionOutcome
	}{
		{422, "err422_nonce.json", ActionNotExecuted},
		{422, "err422_margin.json", ActionRejected},
		{422, "err422_unknown_agent.json", ActionNotExecuted},
		{401, "err401_sig.json", ActionNotExecuted},
		{409, "err409_epoch.json", ActionNotExecuted},
		{429, "err429.json", ActionNotExecuted},
		{202, "w_sequenced_202.json", ActionUnknown},
		{503, "err503_gaps.json", ActionUnknown},
		{503, "err503_backpressure.json", ActionNotExecuted},
	}
	for _, c := range cases {
		f.answer(t, "/v1/orders/replace", c.status, c.fixture)
		a, err := w.SignReplace(btc, []uint64{101}, nil, nil, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = w.Client.SendReplace(context.Background(), a)
		if err == nil {
			t.Fatalf("%s 应报错", c.fixture)
		}
		if got := ClassifyAction(err); got != c.want {
			t.Errorf("%d %s:结论 %d,应为 %d(%v)", c.status, c.fixture, got, c.want, err)
		}
	}
	for code, want := range map[string]ActionOutcome{"NonceOutOfWindow": ActionNotExecuted, "SignatureExpired": ActionNotExecuted, "InsufficientMargin": ActionRejected} {
		if got := ClassifyAction(&Error{Status: http.StatusUnprocessableEntity, Code: code}); got != want {
			t.Errorf("%s:结论 %d,应为 %d", code, got, want)
		}
	}
	if ClassifyAction(fmt.Errorf("%w: dial", ErrNotSent)) != ActionNotExecuted || ClassifyAction(errors.New("connection reset")) != ActionUnknown {
		t.Fatal("没发出 / 连上之后断开")
	}
}

func TestNonceSource(t *testing.T) {
	n := NewNonceSource(1_000)
	at := func(ms int64) uint64 {
		v, err := n.Next(time.UnixMilli(ms))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if at(500) != 1_001 || at(500) != 1_002 || at(5_000) != 5_000 || at(5_000) != 5_001 || n.Last() != 5_001 {
		t.Fatal("nonce 应从下限之后起、同一毫秒内严格递增、随墙钟前进")
	}
	ahead := NewNonceSource(uint64(time.Hour.Milliseconds()) * 12)
	if _, err := ahead.Next(time.UnixMilli(0)); !errors.Is(err, ErrNonceAhead) || ahead.Last() != uint64(time.Hour.Milliseconds())*12 {
		t.Fatalf("领先墙钟超过 12 小时应拒绝且不推进: %v", err)
	}
	var mu sync.Mutex
	seen := map[uint64]bool{}
	var wg sync.WaitGroup
	shared := NewNonceSource(0)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := shared.Next(time.UnixMilli(10_000))
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[v] {
				t.Errorf("nonce %d 重复", v)
			}
			seen[v] = true
		}()
	}
	wg.Wait()
}

func TestNewActionWriterChecks(t *testing.T) {
	f, c := newWriteFake(t)
	vault := uint16(3)
	if _, err := NewActionWriter(context.Background(), c, signer(t, agentKey), AccountRef{Vault: &vault}, NewNonceSource(0)); !errors.Is(err, ErrVaultAction) {
		t.Fatalf("金库寻址: %v", err)
	}
	f.routes["/v1/account/agents"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"address":"` + ownerAddr + `","agents":[],"subaccount":5}`))
	}
	if _, err := NewActionWriter(context.Background(), c, signer(t, agentKey), AccountRef{Address: mustAddr(t, ownerAddr), Subaccount: 5}, NewNonceSource(0)); !errors.Is(err, ErrAgentNotAuthorized) {
		t.Fatalf("没授权的编号: %v", err)
	}
	f.routes["/v1/account/agents"] = serve(t, 200, "w_agents_owner.json")
	w, err := NewActionWriter(context.Background(), c, signer(t, agentKey), AccountRef{Address: mustAddr(t, ownerAddr), Subaccount: 1}, NewNonceSource(0))
	if err != nil {
		t.Fatal(err)
	}
	w.now = func() time.Time { return w.ValidUntil }
	cat, _ := c.Catalog(context.Background())
	btc, _ := cat.Market("BTC-USD")
	if _, err := w.SignCancel(btc, []uint64{1}, time.Time{}); !errors.Is(err, ErrAgentExpired) {
		t.Fatalf("授权到期后不该再签: %v", err)
	}
}

func gatewayAction(t *testing.T, cat *Catalog, path string, body any) ([]byte, dexos.Address) {
	t.Helper()
	items, isBatch := body.([]any)
	env := m(body)
	if isBatch {
		env = m(items[0])
	}
	mk := gatewayMarket(t, cat, env)
	seq := func(v any) uint64 {
		n, err := strconv.ParseUint(v.(string), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	var inner []byte
	switch path {
	case "/v1/orders/replace":
		var ids []dexos.OrderID
		for _, c := range env["cancels"].([]any) {
			ids = append(ids, dexos.NewOrderID(mk.SigningMarketID, seq(c)))
		}
		var orders []dexos.BatchOrder
		for _, o := range env["orders"].([]any) {
			orders = append(orders, gatewayOrder(t, cat, m(o)))
		}
		if mods, _ := env["modifies"].([]any); len(mods) > 0 {
			var modifies []dexos.BatchModify
			for _, x := range mods {
				modifies = append(modifies, gatewayModify(t, mk, m(x)))
			}
			inner = dexos.EncodeBatchReplace2(dexos.ActorPlaceholder, num(t, env["nowMs"]), ids, modifies, orders)
		} else {
			inner = dexos.EncodeBatchReplace(dexos.ActorPlaceholder, num(t, env["nowMs"]), ids, orders)
		}
	case "/v1/orders/cancel":
		if len(items) == 1 {
			inner = dexos.EncodeCancelOrder(dexos.ActorPlaceholder, dexos.NewOrderID(mk.SigningMarketID, seq(env["orderId"])))
		} else {
			ids := make([]dexos.OrderID, len(items))
			for i, it := range items {
				ids[i] = dexos.NewOrderID(mk.SigningMarketID, seq(m(it)["orderId"]))
			}
			inner = dexos.EncodeBatchCancel(dexos.ActorPlaceholder, ids)
		}
	default:
		t.Fatalf("假网关不会算 %s 的 sigVersion 2 请求身份", path)
	}
	var expiry uint64
	if env["expiresAt"] != nil {
		expiry = num(t, env["expiresAt"])
	}
	who := env["account"].(string)
	if s, ok := env["signer"].(string); ok {
		who = s
	}
	digest := testDomain.ActionHash(mustAddr(t, env["account"].(string)), uint16(num(t, env["subaccount"])), inner, num(t, env["nonce"]), expiry)
	return digest, mustAddr(t, who)
}
