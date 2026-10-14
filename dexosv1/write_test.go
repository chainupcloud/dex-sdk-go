package dexosv1

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// 写接口测试的身份:私钥是公开的测试常量(同 dex-os auth 测试与 eip712_golden.json),地址与 w_*.json 一致。
const (
	ownerKey  = "0x4242424242424242424242424242424242424242424242424242424242424242"
	ownerAddr = "0x17c5185167401ed00cf5f5b2fc97d9bbfdb7d025"
	agentKey  = "0x4343434343434343434343434343434343434343434343434343434343434343"
	agentAddr = "0x5975c152fe58cdcb7e25586a3c9b994a16dbb615"
	// signingAccountId 与 nonce 见 w_nonce_*.json。
	signingID = 7
	// vaultMainID 金库 3 的主账户号:以金库身份签的命令里 account 填它(见 w_nonce_vault.json)。
	vaultMainID = 31
)

// testDomain exchange.json 里的域。
var testDomain = dexos.Domain{Name: "dex-os", Version: "1", ChainID: 11155111}

func signer(t *testing.T, key string) *dexos.Signer {
	t.Helper()
	s, err := dexos.NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// recoverAddr 从 r‖s‖v 签名恢复签名者地址 —— 假网关用它验签,与 SDK 的签名路径无关。
func recoverAddr(t *testing.T, digest []byte, sigHex string) string {
	t.Helper()
	raw, err := hex.DecodeString(strings.TrimPrefix(sigHex, "0x"))
	if err != nil || len(raw) != 65 {
		t.Fatalf("签名不是 65 字节: %q", sigHex)
	}
	compact := append([]byte{raw[64]}, raw[:64]...)
	pub, _, err := ecdsa.RecoverCompact(compact, digest)
	if err != nil {
		t.Fatalf("恢复签名者失败: %v", err)
	}
	return "0x" + hex.EncodeToString(dexos.Keccak256(pub.SerializeUncompressed()[1:])[12:])
}

// captured 假网关收到的一个写请求。
type captured struct {
	path string
	body any
	raw  []byte
}

// writeFake 假 dex-os:读接口回 fixture,写接口记下请求体再回给定的状态与 body。
// 回执 body 里的占位 requestId(testReqID)换成假网关按请求体独立算出的请求身份,见 gatewayRequestID。
type writeFake struct {
	*fakeV1
	cat    *Catalog
	writes []captured
}

func newWriteFake(t *testing.T) (*writeFake, *Client) {
	t.Helper()
	var ms struct {
		Markets []wireMarket `json:"markets"`
	}
	if err := json.Unmarshal(fixture(t, "markets.json"), &ms); err != nil {
		t.Fatal(err)
	}
	cat, err := newCatalog(ms.Markets)
	if err != nil {
		t.Fatal(err)
	}
	f, c := newFake(t)
	f.routes["/v1/exchange"] = serve(t, 200, "exchange.json")
	f.routes["/v1/markets"] = serve(t, 200, "markets.json")
	f.routes["/v1/account/agents"] = serve(t, 200, "w_agents.json")
	f.routes["/v1/account/nonce"] = func(w http.ResponseWriter, r *http.Request) {
		name := "w_nonce_account.json"
		if r.URL.Query().Get("signer") != "" {
			name = "w_nonce_agent.json"
		}
		serve(t, 200, name)(w, r)
	}
	return &writeFake{fakeV1: f, cat: cat}, c
}

// answer 写端点 path 回 status + fixture,并记下请求。
func (f *writeFake) answer(t *testing.T, path string, status int, name string) {
	f.answerRaw(t, path, status, fixture(t, name))
}

func (f *writeFake) answerRaw(t *testing.T, path string, status int, body []byte) {
	f.routes[path] = func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Query().Get("ack") != "result" {
			t.Errorf("%s: 应为 POST ?ack=result,得到 %s %s", path, r.Method, r.URL)
		}
		req := decodeJSON(t, raw)
		f.mu.Lock()
		f.writes = append(f.writes, captured{path: path, body: req, raw: raw})
		f.mu.Unlock()
		out := body
		if strings.Contains(string(body), testReqID) {
			out = []byte(strings.ReplaceAll(string(body), testReqID, gatewayRequestID(t, f.cat, path, req)))
		}
		w.Header().Set("content-type", "application/json")
		w.Header().Set("x-dexos-seq", "500")
		if status == http.StatusTooManyRequests {
			// 与网关一致:账户桶 429 上 `ratelimit` 是 IP 桶的读数,扣光的账户桶在 `x-ratelimit-account`
			w.Header().Set("retry-after", "12")
			w.Header().Set("ratelimit", "limit=1200, remaining=1100, reset=12")
			w.Header().Set("x-ratelimit-account", "limit=1200, remaining=0, reset=12")
		}
		w.WriteHeader(status)
		_, _ = w.Write(out)
	}
}

// gatewayRequestID 假网关按线上请求体独立算请求身份,与网关同一定义:keccak256(签名者 ‖ 签名摘要)。
// API 钱包签 AgentExec(规范命令编码, nonce),主钱包签 EIP-712 具名结构体;都从请求体重建,不经 SDK 的签名路径。
func gatewayRequestID(t *testing.T, cat *Catalog, path string, body any) string {
	t.Helper()
	env := m(body)
	items, isBatch := body.([]any)
	if isBatch {
		env = m(items[0])
	}
	market := func(sym any) *Market {
		mk, err := cat.Market(sym.(string))
		if err != nil {
			t.Fatal(err)
		}
		return mk
	}
	orderID := func(sym, id any) dexos.OrderID {
		seq, err := strconv.ParseUint(id.(string), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return dexos.NewOrderID(market(sym).SigningMarketID, seq)
	}
	nonce := num(t, env["nonce"])
	var digest []byte
	signerHex, isAgent := env["signer"].(string)
	if isAgent {
		acct := uint32(signingID)
		if env["vault"] != nil {
			acct = vaultMainID
		}
		var cmd []byte
		switch path {
		case "/v1/orders":
			orders := make([]dexos.BatchOrder, len(items))
			for i, it := range items {
				orders[i] = gatewayOrder(t, cat, m(it))
			}
			cmd = dexos.EncodeBatchPlace(acct, num(t, env["nowMs"]), orders)
		case "/v1/orders/cancel":
			ids := make([]dexos.OrderID, len(items))
			for i, it := range items {
				ids[i] = orderID(m(it)["symbol"], m(it)["orderId"])
			}
			cmd = dexos.EncodeBatchCancel(acct, ids)
		case "/v1/orders/replace":
			var ids []dexos.OrderID
			for _, c := range env["cancels"].([]any) {
				ids = append(ids, orderID(env["symbol"], c))
			}
			var orders []dexos.BatchOrder
			for _, o := range env["orders"].([]any) {
				orders = append(orders, gatewayOrder(t, cat, m(o)))
			}
			cmd = dexos.EncodeBatchReplace(acct, num(t, env["nowMs"]), ids, orders)
		case "/v1/orders/cancel-all":
			cmd = dexos.EncodeScheduleCancel(acct, num(t, env["nowMs"]), num(t, env["nowMs"]))
		case "/v1/orders/schedule-cancel":
			cmd = dexos.EncodeScheduleCancel(acct, num(t, env["at"]), num(t, env["nowMs"]))
		default:
			t.Fatalf("假网关不会算 %s 的 API 钱包请求身份", path)
		}
		digest = testDomain.AgentExecHash(cmd, nonce)
	} else {
		signerHex = env["account"].(string)
		owner := mustAddr(t, signerHex)
		optional := func(k string) uint64 {
			if env[k] == nil {
				return 0
			}
			return num(t, env[k])
		}
		switch path {
		case "/v1/orders":
			g := gatewayOrder(t, cat, env)
			digest = orderHash(testDomain, owner, signingID, g.Market, g.Side, g.Price, g.Lots, g.TIF, g.ReduceOnly, nonce, optional("expiresAt"))
		case "/v1/orders/cancel":
			id := orderID(env["symbol"], env["orderId"])
			digest = cancelHash(testDomain, owner, signingID, id.Market(), id.Seq(), nonce)
		case "/v1/account/leverage":
			var imf uint32
			if lev := num(t, env["leverage"]); lev > 0 {
				imf = uint32(1_000_000 / lev)
			}
			digest = leverageHash(testDomain, owner, signingID, market(env["symbol"]).SigningMarketID, imf, nonce)
		case "/v1/account/agents/approve":
			digest = testDomain.ApproveAgentHash(owner, signingID, mustAddr(t, env["agent"].(string)), optional("validUntil"), nonce)
		default:
			t.Fatalf("假网关不会算 %s 的主钱包请求身份", path)
		}
	}
	signer := mustAddr(t, signerHex)
	return "0x" + hex.EncodeToString(dexos.Keccak256(signer[:], digest))
}

func (f *writeFake) last(t *testing.T) captured {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) == 0 {
		t.Fatal("没有收到写请求")
	}
	return f.writes[len(f.writes)-1]
}

func (f *writeFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

// conformsRequest 请求体对钉住规格的请求 schema 校验;allow 是已知的规格缺陷(见 specDeviations 的说明)。
func conformsRequest(t *testing.T, path string, body any, allow ...string) {
	t.Helper()
	spec := loadSpec(t)
	var left []string
	for _, e := range validate(spec, requestSchema(t, spec, path), body, "body") {
		known := false
		for _, a := range allow {
			known = known || strings.Contains(e, a)
		}
		if !known {
			left = append(left, e)
		}
	}
	if len(left) > 0 {
		t.Errorf("POST %s 请求体不符合规格:\n  %s", path, strings.Join(left, "\n  "))
	}
}

func m(v any) map[string]any { return obj(v) }

func num(t *testing.T, v any) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(v.(json.Number).String(), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// gatewayOrder 按网关的做法把线上的一张单换回执行层参数(符号 → 市场号,十进制 → tick / lot)。
func gatewayOrder(t *testing.T, cat *Catalog, item map[string]any) dexos.BatchOrder {
	t.Helper()
	mk, err := cat.Market(item["symbol"].(string))
	if err != nil {
		t.Fatal(err)
	}
	price, err := mk.Scale.Ticks(item["price"].(string))
	if err != nil {
		t.Fatal(err)
	}
	lots, err := mk.Scale.Lots(item["size"].(string))
	if err != nil {
		t.Fatal(err)
	}
	side := dexos.Buy
	if item["side"] == "sell" {
		side = dexos.Sell
	}
	tif := map[string]dexos.TimeInForce{"gtc": dexos.GTC, "ioc": dexos.IOC, "alo": dexos.PostOnly, "fok": dexos.FOK}[item["tif"].(string)]
	if item["type"] != "limit" {
		t.Fatalf("type 应为 limit,得到 %v", item["type"])
	}
	return dexos.BatchOrder{Market: mk.SigningMarketID, Side: side, Price: price, Lots: lots, TIF: tif, ReduceOnly: item["reduceOnly"] == true}
}

func newTestSession(t *testing.T) (*writeFake, *Session, *Catalog) {
	t.Helper()
	f, c := newWriteFake(t)
	cat, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(context.Background(), c, signer(t, agentKey), AccountRef{Address: mustAddr(t, ownerAddr)})
	if err != nil {
		t.Fatal(err)
	}
	if s.Nonce() != 41 {
		t.Fatalf("agent nonce 应取 41,得到 %d", s.Nonce())
	}
	return f, s, cat
}

// 批量下单:假网关按线上的请求体重建 BatchPlace、重算 AgentExec 摘要并恢复签名者 —— 与网关 / 执行层同一条验签路径。
func TestSessionPlaceSignsWhatTheGatewayVerifies(t *testing.T) {
	f, s, cat := newTestSession(t)
	f.answer(t, "/v1/orders", 200, "w_batch_place.json")
	btc, _ := cat.Market("BTC-USD")
	res, err := s.Place(context.Background(),
		OrderRequest{Market: btc, Side: dexos.Buy, Price: 65000, Lots: 500, TIF: dexos.GTC},
		OrderRequest{Market: btc, Side: dexos.Sell, Price: 66000, Lots: 250, TIF: dexos.PostOnly, ReduceOnly: true, ClientOrderID: "q-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	req := f.last(t)
	conformsRequest(t, "/v1/orders", req.body)
	items := req.body.([]any)
	if len(items) != 2 {
		t.Fatalf("应发 2 项,得到 %d", len(items))
	}
	first := m(items[0])
	if first["signer"] != agentAddr || first["account"] != ownerAddr || num(t, first["nonce"]) != 41 {
		t.Fatalf("信封不对: %v", first)
	}
	if m(items[1])["price"] != "66000" || m(items[1])["size"] != "0.25" || m(items[1])["tif"] != "alo" || m(items[1])["clientOrderId"] != "q-1" {
		t.Fatalf("第二项线上形状不对: %v", items[1])
	}
	orders := []dexos.BatchOrder{gatewayOrder(t, cat, first), gatewayOrder(t, cat, m(items[1]))}
	cmd := dexos.EncodeBatchPlace(signingID, num(t, first["nowMs"]), orders)
	digest := testDomain.AgentExecHash(cmd, 41)
	if got := recoverAddr(t, digest, first["signature"].(string)); got != agentAddr {
		t.Fatalf("网关恢复出的签名者 %s,应为 %s", got, agentAddr)
	}
	// 请求身份与 dexos 包的定义互相印证
	want, err := dexos.AgentRequestIdentity{Agent: mustAddr(t, agentAddr), SigningHash: "0x" + hex.EncodeToString(digest)}.RequestID()
	if err != nil {
		t.Fatal(err)
	}
	if res.Request.RequestID != want || res.Request.Scope != "agent:"+agentAddr || res.Request.CommandHash != "0x"+hex.EncodeToString(dexos.Keccak256(cmd)) {
		t.Fatalf("请求身份不对: %+v(应为 %s)", res.Request, want)
	}
	if s.Nonce() != 42 {
		t.Fatalf("成功后 nonce 应前进到 42,得到 %d", s.Nonce())
	}
	if res.Seq != 500 || len(res.Items) != 2 {
		t.Fatalf("回执: %+v", res)
	}
	a, r := res.Items[0], res.Items[1]
	if a.Status != "accepted" || *a.OrderID != 101 || *a.FilledLots != 0 || !*a.Resting {
		t.Fatalf("第 0 项: %+v", a)
	}
	if r.Status != "rejected" || r.Code != "InsufficientMargin" || r.OrderID != nil || r.ClientOrderID == nil || *r.ClientOrderID != "q-1" {
		t.Fatalf("第 1 项: %+v", r)
	}
}

func TestSessionCancelAndReplace(t *testing.T) {
	f, s, cat := newTestSession(t)
	btc, _ := cat.Market("BTC-USD")

	f.answer(t, "/v1/orders/cancel", 200, "w_batch_cancel.json")
	res, err := s.Cancel(context.Background(), btc, 101, 999999)
	if err != nil {
		t.Fatal(err)
	}
	req := f.last(t)
	conformsRequest(t, "/v1/orders/cancel", req.body)
	items := req.body.([]any)
	ids := []dexos.OrderID{dexos.NewOrderID(0, 101), dexos.NewOrderID(0, 999999)}
	if m(items[1])["orderId"] != "999999" {
		t.Fatalf("撤单项: %v", items[1])
	}
	digest := testDomain.AgentExecHash(dexos.EncodeBatchCancel(signingID, ids), 41)
	if got := recoverAddr(t, digest, m(items[0])["signature"].(string)); got != agentAddr {
		t.Fatalf("撤单签名者 %s", got)
	}
	if *res.Items[0].CanceledLots != 500 || res.Items[1].Code != "UnknownOrder" || *res.Items[1].OrderID != 999999 {
		t.Fatalf("撤单回执: %+v", res.Items)
	}

	f.answer(t, "/v1/orders/replace", 200, "w_replace.json")
	rep, err := s.Replace(context.Background(), btc, []uint64{101}, []OrderRequest{{Market: btc, Side: dexos.Buy, Price: 64900, Lots: 200, TIF: dexos.GTC, ClientOrderID: "r-1"}})
	if err != nil {
		t.Fatal(err)
	}
	req = f.last(t)
	// 规格缺陷:replace 的 orders[] 引用了 PlaceOrder,把信封字段也列成必填;实现只用外层信封(v1_write.rs replace)。
	conformsRequest(t, "/v1/orders/replace", req.body)
	body := m(req.body)
	if body["cancels"].([]any)[0] != "101" {
		t.Fatalf("cancels 应是十进制字符串: %v", body["cancels"])
	}
	newOrder := gatewayOrder(t, cat, m(body["orders"].([]any)[0]))
	cmd := dexos.EncodeBatchReplace(signingID, num(t, body["nowMs"]), []dexos.OrderID{dexos.NewOrderID(0, 101)}, []dexos.BatchOrder{newOrder})
	if got := recoverAddr(t, testDomain.AgentExecHash(cmd, 42), body["signature"].(string)); got != agentAddr {
		t.Fatalf("换单签名者 %s", got)
	}
	if len(rep.Items) != 2 || rep.Items[1].Kind != "place" || *rep.Items[1].FilledLots != 200 || rep.Sub != 1 {
		t.Fatalf("换单回执: %+v", rep)
	}
	// 换单的新单也带 clientOrderId(v1.1 §4.2);吃单成交逐笔回在回执里
	if m(body["orders"].([]any)[0])["clientOrderId"] != "r-1" {
		t.Fatalf("换单新单的 clientOrderId 没带上: %v", body["orders"])
	}
	if it := rep.Items[1]; *it.AvgPrice != 64900 || len(it.Fills) != 1 || it.Fills[0] != (ReceiptFill{FillID: "502-1-0", Price: 64900, Lots: 200, Fee: "6.49", FeeToken: "USDC"}) {
		t.Fatalf("换单新单的成交: %+v", it)
	}
	if s.Nonce() != 43 {
		t.Fatalf("nonce 应为 43,得到 %d", s.Nonce())
	}
}

func TestSessionCancelAllAndScheduleCancel(t *testing.T) {
	f, s, _ := newTestSession(t)
	f.answer(t, "/v1/orders/cancel-all", 200, "w_cancel_all.json")
	if _, err := s.CancelAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := m(f.last(t).body)
	conformsRequest(t, "/v1/orders/cancel-all", body)
	now := num(t, body["nowMs"])
	cmd := dexos.EncodeScheduleCancel(signingID, now, now) // 撤全部 = 触发时刻为 nowMs 的 ScheduleCancel
	if got := recoverAddr(t, testDomain.AgentExecHash(cmd, 41), body["signature"].(string)); got != agentAddr {
		t.Fatalf("撤全部签名者 %s", got)
	}

	f.answer(t, "/v1/orders/schedule-cancel", 200, "w_schedule.json")
	at := time.UnixMilli(1791900000000)
	if _, err := s.ScheduleCancel(context.Background(), at); err != nil {
		t.Fatal(err)
	}
	body = m(f.last(t).body)
	conformsRequest(t, "/v1/orders/schedule-cancel", body)
	cmd = dexos.EncodeScheduleCancel(signingID, 1791900000000, num(t, body["nowMs"]))
	if num(t, body["at"]) != 1791900000000 || recoverAddr(t, testDomain.AgentExecHash(cmd, 42), body["signature"].(string)) != agentAddr {
		t.Fatalf("断线保护请求不对: %v", body)
	}
	// 零值 = 清除,线上仍带 at=0(必填)
	if _, err := s.ScheduleCancel(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if body = m(f.last(t).body); num(t, body["at"]) != 0 {
		t.Fatalf("清除应发 at=0: %v", body)
	}
}

// nonce 的前进与锁定按「命令进没进日志、结论是否确定」区分(nonceGuard.run 的注释)。
func TestSessionNonceOutcomes(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		fixture   string
		nonce     uint64
		blocked   bool
		checkErr  func(error) bool
		wantError bool
	}{
		{"成功", 200, "w_cancel_all.json", 42, false, nil, false},
		{"定序前:未知符号", 404, "err404.json", 41, false, nil, true},
		{"定序前:网关验签失败", 401, "err401_sig.json", 41, false, nil, true},
		{"定序前:账户桶限流", 429, "err429_account.json", 41, false, func(err error) bool {
			var rl *RateLimitedError
			return errors.As(err, &rl) && rl.Bucket == "account" && rl.Remaining == 0 && rl.Limit == 1200 && rl.RetryAfter == 12*time.Second
		}, true},
		{"业务拒绝:nonce 已消耗", 422, "err422_margin.json", 42, false, nil, true},
		{"nonce 之前的拒绝", 422, "err422_unknown_agent.json", 41, false, nil, true},
		{"NonceMismatch:本地计数器不可信", 422, "err422_nonce.json", 41, true, nil, true},
		{"202:结论未知", 202, "w_sequenced_202.json", 41, true, func(err error) bool { return errors.Is(err, ErrPending) }, true},
		{"5xx", 500, "err404.json", 41, true, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, s, _ := newTestSession(t)
			f.answer(t, "/v1/orders/cancel-all", c.status, c.fixture)
			_, err := s.CancelAll(context.Background())
			if (err != nil) != c.wantError {
				t.Fatalf("err = %v", err)
			}
			if c.checkErr != nil && !c.checkErr(err) {
				t.Fatalf("错误类型不对: %v", err)
			}
			if s.Nonce() != c.nonce {
				t.Fatalf("nonce 应为 %d,得到 %d", c.nonce, s.Nonce())
			}
			if blocked := errors.Is(err, ErrSessionBlocked); blocked != c.blocked {
				t.Fatalf("锁定应为 %v: %v", c.blocked, err)
			}
			if !c.blocked {
				if s.Pending() != nil {
					t.Fatal("没锁定却有未决请求")
				}
				return
			}
			p := s.Pending()
			if p == nil || p.Nonce != 41 || !isHash32(p.RequestID) || p.Scope != "agent:"+agentAddr {
				t.Fatalf("锁定后应给出未决请求的身份: %+v", p)
			}
			// 锁定之后不再发请求
			before := f.count()
			if _, err := s.CancelAll(context.Background()); !errors.Is(err, ErrSessionBlocked) || f.count() != before {
				t.Fatalf("锁定后应直接拒绝且不发请求: %v", err)
			}
		})
	}
}

func TestBatchReceiptMustCoverEveryItem(t *testing.T) {
	f, s, cat := newTestSession(t)
	btc, _ := cat.Market("BTC-USD")
	// 下两张单,回执只有一项:批次执行了,但另一张的结局说不清 → 锁定
	f.answerRaw(t, "/v1/orders", 200, []byte(`{"status":"ok","items":[{"index":0,"kind":"place","status":"accepted","symbol":"BTC-USD","orderId":"1","filled":"0","resting":true}],"seq":9,"sub":0,"requestId":"`+testReqID+`"}`))
	o := OrderRequest{Market: btc, Side: dexos.Buy, Price: 1, Lots: 1}
	_, err := s.Place(context.Background(), o, o)
	if !errors.Is(err, ErrIncompleteBatch) || !errors.Is(err, ErrSessionBlocked) {
		t.Fatalf("应报 ErrIncompleteBatch 并锁定: %v", err)
	}
}

// 回执的 requestId 必须在、且与本地按签名算出的相同;对不上说明两边签的不是同一个请求,结论不可信 → 锁定。
func TestServerRequestIDMustMatchLocal(t *testing.T) {
	other := "0x" + strings.Repeat("cd", 32)
	for name, body := range map[string]string{
		"不同": `{"status":"ok","seq":9,"sub":0,"requestId":"` + other + `"}`,
		"缺失": `{"status":"ok","seq":9,"sub":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			f, s, _ := newTestSession(t)
			f.answerRaw(t, "/v1/orders/cancel-all", 200, []byte(body))
			if _, err := s.CancelAll(context.Background()); err == nil || !strings.Contains(err.Error(), "requestId") || !errors.Is(err, ErrSessionBlocked) {
				t.Fatalf("requestId %s 应报错并锁定: %v", name, err)
			}
		})
	}
}

// AckWait 换成 `waitMs`;超过 5s 本地就报错,不发请求。
func TestAckWaitParam(t *testing.T) {
	f, s, _ := newTestSession(t)
	var got string
	f.answer(t, "/v1/orders/cancel-all", 200, "w_cancel_all.json")
	h := f.routes["/v1/orders/cancel-all"]
	f.routes["/v1/orders/cancel-all"] = func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("waitMs")
		h(w, r)
	}
	s.Client.AckWait = 2500 * time.Millisecond
	if _, err := s.CancelAll(context.Background()); err != nil || got != "2500" {
		t.Fatalf("waitMs 应为 2500,得到 %q: %v", got, err)
	}
	s.Client.AckWait = 6 * time.Second
	before := f.count()
	if _, err := s.CancelAll(context.Background()); err == nil || f.count() != before || s.Nonce() != 42 {
		t.Fatalf("AckWait 超上限应本地报错、不发请求、不动 nonce: %v", err)
	}
}

func TestLocalValidationDoesNotTouchNonce(t *testing.T) {
	f, s, cat := newTestSession(t)
	btc, _ := cat.Market("BTC-USD")
	eth, _ := cat.Market("ETH-USD")
	for name, orders := range map[string][]OrderRequest{
		"价格为 0":    {{Market: btc, Side: dexos.Buy, Lots: 1}},
		"未知 TIF":   {{Market: btc, Side: dexos.Buy, Price: 1, Lots: 1, TIF: 9}},
		"cid 非法":   {{Market: btc, Side: dexos.Buy, Price: 1, Lots: 1, ClientOrderID: "有空格 x"}},
		"跨市场":      {{Market: btc, Side: dexos.Buy, Price: 1, Lots: 1}, {Market: eth, Side: dexos.Buy, Price: 1, Lots: 1}},
		"没有市场":     {{Side: dexos.Buy, Price: 1, Lots: 1}, {Side: dexos.Buy, Price: 1, Lots: 1}},
		"没有单":      {},
		"方向非法":     {{Market: btc, Side: 7, Price: 1, Lots: 1}},
		"cid 超 36": {{Market: btc, Side: dexos.Buy, Price: 1, Lots: 1, ClientOrderID: strings.Repeat("a", 37)}},
	} {
		if _, err := s.Place(context.Background(), orders...); err == nil {
			t.Errorf("%s: 应报错", name)
		}
	}
	if f.count() != 0 || s.Nonce() != 41 {
		t.Fatalf("本地校验失败不该发请求或动 nonce: 发了 %d 次,nonce %d", f.count(), s.Nonce())
	}
}

func TestNewSessionChecksAuthorization(t *testing.T) {
	f, c := newWriteFake(t)
	ref := AccountRef{Address: mustAddr(t, ownerAddr)}
	if _, err := NewSession(context.Background(), c, signer(t, ownerKey), ref); !errors.Is(err, ErrAgentNotAuthorized) {
		t.Fatalf("不在授权列表里应报 ErrAgentNotAuthorized: %v", err)
	}
	f.routes["/v1/account/agents"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"address":"` + ownerAddr + `","subaccount":0,"agents":[{"address":"` + agentAddr + `","label":"","validUntil":1,"expired":true}]}`))
	}
	if _, err := NewSession(context.Background(), c, signer(t, agentKey), ref); !errors.Is(err, ErrAgentExpired) {
		t.Fatalf("已到期应报 ErrAgentExpired: %v", err)
	}
}

// 主钱包:单笔下单 / 撤单 / 设杠杆 / 授权,签的是 EIP-712 具名结构体;假网关按请求体重建结构体验签。
func TestWalletWrites(t *testing.T) {
	f, c := newWriteFake(t)
	cat, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	btc, _ := cat.Market("BTC-USD")
	w, err := NewWallet(context.Background(), c, signer(t, ownerKey), 0)
	if err != nil {
		t.Fatal(err)
	}
	owner := mustAddr(t, ownerAddr)

	f.answer(t, "/v1/orders", 200, "w_order.json")
	exp := time.UnixMilli(1791999999000)
	o, err := w.PlaceOrder(context.Background(), OrderRequest{Market: btc, Side: dexos.Buy, Price: 65000, Lots: 15, TIF: dexos.IOC, ClientOrderID: "c-1"}, exp)
	if err != nil {
		t.Fatal(err)
	}
	body := m(f.last(t).body)
	conformsRequest(t, "/v1/orders", body)
	if body["signer"] != nil || num(t, body["expiresAt"]) != 1791999999000 {
		t.Fatalf("主钱包下单不带 signer、带 expiresAt: %v", body)
	}
	g := gatewayOrder(t, cat, body)
	digest := orderHash(testDomain, owner, signingID, g.Market, g.Side, g.Price, g.Lots, g.TIF, g.ReduceOnly, 2, num(t, body["expiresAt"]))
	if recoverAddr(t, digest, body["signature"].(string)) != ownerAddr {
		t.Fatal("下单签名恢复不出主钱包")
	}
	if o.OrderID != 202 || o.Status != "partiallyFilled" || o.FilledLots != 5 || *o.AvgPrice != 64990 || *o.ClientOrderID != "c-1" || o.Request.Scope != "account:7" {
		t.Fatalf("下单结论: %+v", o)
	}
	if len(o.Fills) != 1 || o.Fills[0] != (ReceiptFill{FillID: "503-0-0", Price: 64990, Lots: 5, Fee: "0.16", FeeToken: "USDC"}) {
		t.Fatalf("下单回执的成交: %+v", o.Fills)
	}

	f.answer(t, "/v1/orders/cancel", 200, "w_cancel_one.json")
	cr, err := w.Cancel(context.Background(), btc, 202)
	if err != nil {
		t.Fatal(err)
	}
	body = m(f.last(t).body)
	conformsRequest(t, "/v1/orders/cancel", body)
	if recoverAddr(t, cancelHash(testDomain, owner, signingID, 0, 202, 3), body["signature"].(string)) != ownerAddr || *cr.RemainingLots != 10 {
		t.Fatalf("撤单: %v / %+v", body, cr)
	}

	f.answer(t, "/v1/account/leverage", 200, "w_leverage.json")
	if _, err := w.SetLeverage(context.Background(), btc, 3); err != nil {
		t.Fatal(err)
	}
	body = m(f.last(t).body)
	conformsRequest(t, "/v1/account/leverage", body)
	// 网关:customImfPpm = floor(1e6 / leverage)
	if num(t, body["leverage"]) != 3 || recoverAddr(t, leverageHash(testDomain, owner, signingID, 0, 1_000_000/3, 4), body["signature"].(string)) != ownerAddr {
		t.Fatalf("杠杆: %v", body)
	}

	f.answer(t, "/v1/account/agents/approve", 200, "w_approve.json")
	if _, err := w.ApproveAgent(context.Background(), mustAddr(t, agentAddr), time.Time{}, "mm"); err != nil {
		t.Fatal(err)
	}
	body = m(f.last(t).body)
	conformsRequest(t, "/v1/account/agents/approve", body)
	if body["validUntil"] != nil || recoverAddr(t, testDomain.ApproveAgentHash(owner, signingID, mustAddr(t, agentAddr), 0, 5), body["signature"].(string)) != ownerAddr {
		t.Fatalf("授权: %v", body)
	}
	if w.Nonce() != 6 {
		t.Fatalf("四次成功写之后 nonce 应为 6,得到 %d", w.Nonce())
	}
}

// specDeviations 本包按实现读、而钉住的规格里没有的字段:发现了就报给 dex-os,规格改好、同步副本后
// 删掉对应条目 —— 条目多余时测试会红。dex-os 3c1ce22(v1.1 #79 金库身份)之后没有已知偏差。
var specDeviations = map[string][]string{}

// 本包解码 / 发送用到的每个字段都必须在规格里,specDeviations 除外。
func TestWriteFieldsExistInPinnedSpec(t *testing.T) {
	spec := loadSpec(t)
	props := func(s map[string]any) map[string]any {
		out := map[string]any{}
		s = deref(spec, s)
		for k, v := range obj(s["properties"]) {
			out[k] = v
		}
		for _, sub := range asSlice(s["allOf"]) {
			for k, v := range obj(deref(spec, obj(sub))["properties"]) {
				out[k] = v
			}
		}
		return out
	}
	batchItems := props(obj(obj(props(componentSchema(t, spec, "BatchResult"))["items"])["items"]))
	cases := []struct {
		name  string
		typ   reflect.Type
		props map[string]any
	}{
		{"OrderResult", reflect.TypeFor[wireOrderResult](), props(componentSchema(t, spec, "OrderResult"))},
		{"OrderResult(ack)", reflect.TypeFor[wireAck](), props(componentSchema(t, spec, "OrderResult"))},
		{"BatchResult.items[]", reflect.TypeFor[wireBatchItem](), batchItems},
		{"ReceiptFill", reflect.TypeFor[wireReceiptFill](), props(componentSchema(t, spec, "ReceiptFill"))},
		{"envelope", reflect.TypeFor[envelope](), props(componentSchema(t, spec, "SignedEnvelope"))},
		{"PlaceOrder", reflect.TypeFor[wireOrderReq](), props(componentSchema(t, spec, "PlaceOrder"))},
		{"cancel", reflect.TypeFor[wireCancelReq](), props(obj(asSlice(requestSchema(t, spec, "/v1/orders/cancel")["oneOf"])[0]))},
		{"replace", reflect.TypeFor[wireReplaceReq](), props(requestSchema(t, spec, "/v1/orders/replace"))},
		{"schedule-cancel", reflect.TypeFor[wireScheduleReq](), props(requestSchema(t, spec, "/v1/orders/schedule-cancel"))},
		{"leverage", reflect.TypeFor[wireLeverageReq](), props(requestSchema(t, spec, "/v1/account/leverage"))},
		{"approve", reflect.TypeFor[wireApproveReq](), props(requestSchema(t, spec, "/v1/account/agents/approve"))},
	}
	for _, c := range cases {
		allowed := map[string]bool{}
		for _, f := range specDeviations[c.name] {
			allowed[f] = true
		}
		var missing, stale []string
		for i := range c.typ.NumField() {
			f := c.typ.Field(i)
			if f.Anonymous {
				continue // 内嵌的信封单独核对
			}
			tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if tag == "" || tag == "-" {
				continue
			}
			_, inSpec := c.props[tag]
			switch {
			case !inSpec && !allowed[tag]:
				missing = append(missing, tag)
			case inSpec && allowed[tag]:
				stale = append(stale, tag)
			}
		}
		sort.Strings(missing)
		sort.Strings(stale)
		if len(missing) > 0 {
			t.Errorf("%s 用了规格里没有的字段: %v", c.name, missing)
		}
		if len(stale) > 0 {
			t.Errorf("%s: 规格已经有了 %v,从 specDeviations 里删掉", c.name, stale)
		}
	}
}

// allOf / oneOf 的校验不能是空转:缺签名、枚举外的 tif 都要被拦住。
func TestRequestValidatorIsNotVacuous(t *testing.T) {
	spec := loadSpec(t)
	schema := requestSchema(t, spec, "/v1/orders")
	ok := `{"account":"` + ownerAddr + `","nonce":1,"signature":"0x` + strings.Repeat("ab", 65) + `","symbol":"BTC-USD","side":"buy","type":"limit","size":"1","tif":"gtc"}`
	if errs := validate(spec, schema, decodeJSON(t, []byte(ok)), "body"); len(errs) > 0 {
		t.Fatalf("合法请求被拦: %v", errs)
	}
	for name, body := range map[string]string{
		"缺签名":     strings.Replace(ok, `"signature":"0x`+strings.Repeat("ab", 65)+`",`, "", 1),
		"tif 枚举外": strings.Replace(ok, `"tif":"gtc"`, `"tif":"post_only"`, 1),
		"批量项缺签名":  "[" + strings.Replace(ok, `"signature":"0x`+strings.Repeat("ab", 65)+`",`, "", 1) + "]",
		"地址不是小写":  strings.Replace(ok, ownerAddr, strings.ToUpper(ownerAddr[2:3])+ownerAddr, 1),
	} {
		if errs := validate(spec, schema, decodeJSON(t, []byte(body)), "body"); len(errs) == 0 {
			t.Errorf("%s 没被拦住", name)
		}
	}
}
