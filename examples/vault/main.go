// vault:对着真节点走一遍「以金库身份交易」—— manager 名下的 API 钱包代金库主账户挂单、撤单。
//
//	GW=http://127.0.0.1:17807 ADMIN_TOKEN=… go run ./examples/vault -state <文件>
//
// 流程:生成 manager 钱包与 API 钱包 → 测试入金(/internal/deposit,只在 token 模式的测试栈可用)
// → manager 建库(自任 manager,出资人白名单只填自己)→ 在 manager 账户上授权 API 钱包
// → NewSession(…, VaultAccount(金库号))→ 远离盘口挂一张只挂单 → 挂单在金库主账户上、不在 manager 账户上
// → 撤单 → 金库主账户上没有这张单。每步 PASS / FAIL,任何 FAIL 退出码 1。
//
// -state 文件(0600)存两把私钥与金库号,重跑复用同一个金库,不重复入金、建库、授权。只打印地址,不打印私钥。
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
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
	"github.com/chainupcloud/dex-sdk-go/dexosv1"
)

// fundAmount 测试入金 2000 USDC(原子单位):建库费 + 种子 1000 + 挂单保证金。
const fundAmount = "2000000000"

type state struct {
	ManagerKey string  `json:"managerKey"`
	AgentKey   string  `json:"agentKey"`
	Manager    string  `json:"manager"`
	Agent      string  `json:"agent"`
	Funded     bool    `json:"funded"`
	Vault      *uint16 `json:"vault"`
	Approved   bool    `json:"approved"`
}

func (s *state) save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600) // 文件已存在时 WriteFile 不改权限
}

type report struct{ fails int }

func (r *report) check(step string, cond bool, detail string) {
	if cond {
		fmt.Printf("PASS  %-22s %s\n", step, detail)
		return
	}
	r.fails++
	fmt.Printf("FAIL  %-22s %s\n", step, detail)
}

func (r *report) must(step string, err error) {
	if err != nil {
		r.check(step, false, err.Error())
		os.Exit(1)
	}
}

func main() {
	statePath := flag.String("state", "", "存私钥与金库号的文件(0600);已存在就复用")
	symbol := flag.String("market", "BTC-USD", "挂单用的永续市场")
	flag.Parse()
	gw, admin := strings.TrimRight(os.Getenv("GW"), "/"), os.Getenv("ADMIN_TOKEN")
	if gw == "" {
		gw = "http://127.0.0.1:17807"
	}
	if admin == "" || *statePath == "" {
		fmt.Println("需要 ADMIN_TOKEN(测试入金用)与 -state <文件>")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r := &report{}
	c := dexosv1.NewClient(gw)

	ex, err := c.Exchange(ctx)
	r.must("exchange", err)
	r.check("exchange", ex.Version.CodecVer == dexos.CodecVersion, fmt.Sprintf("chainId=%d codec=v%d", ex.ChainID, ex.Version.CodecVer))

	// ── 1. 两把密钥:先落盘再做任何写操作,中途失败重跑也找得回钱包 ──
	var st state
	if b, err := os.ReadFile(*statePath); err == nil {
		r.must("state", json.Unmarshal(b, &st))
	} else if !errors.Is(err, os.ErrNotExist) {
		r.must("state", err)
	}
	if st.ManagerKey == "" {
		m, err := dexos.GenerateSigner()
		r.must("keygen", err)
		a, err := dexos.GenerateSigner()
		r.must("keygen", err)
		st = state{ManagerKey: m.PrivateKeyHex(), AgentKey: a.PrivateKeyHex(), Manager: m.Address().Hex(), Agent: a.Address().Hex()}
		r.must("state", st.save(*statePath))
	}
	manager, err := dexos.NewSigner(st.ManagerKey)
	r.must("keygen", err)
	agent, err := dexos.NewSigner(st.AgentKey)
	r.must("keygen", err)
	r.check("keys", true, fmt.Sprintf("manager=%s api=%s state=%s", manager.Address(), agent.Address(), *statePath))

	// ── 2. 测试入金(首次入金即开户)──
	if !st.Funded {
		r.must("deposit", deposit(ctx, gw, admin, manager.Address()))
		st.Funded = true
		r.must("state", st.save(*statePath))
	}
	managerRef := dexosv1.AccountRef{Address: manager.Address()}
	var acct *dexosv1.Account
	for i := 0; i < 20; i++ {
		if acct, err = c.Account(ctx, managerRef); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	r.must("deposit", err)
	r.check("deposit", true, fmt.Sprintf("manager 账户 %s collateral=%s", managerRef, acct.Collateral))

	// ── 3. 建库:manager 自建自管,出资人白名单只有自己(主钱包签名,Wallet 管 nonce)──
	w, err := dexosv1.NewWallet(ctx, c, manager, 0)
	r.must("wallet", err)
	if st.Vault == nil {
		me := manager.Address()
		created, err := w.CreateVault(ctx, dexosv1.VaultCreate{
			Name:               "sdk-vault-" + me.Hex()[2:10],
			Description:        "dex-sdk-go examples/vault 的测试金库:manager 自建自管,出资人白名单只有 manager",
			Manager:            &me,
			SeedAmount:         "1000",
			DepositorWhitelist: []dexos.Address{me},
		})
		r.must("create-vault", err)
		st.Vault = &created.Vault
		r.must("state", st.save(*statePath))
	}
	vaultID := *st.Vault
	v, err := c.Vault(ctx, vaultID)
	r.must("vault", err)
	isManager := v.Manager != nil && strings.EqualFold(*v.Manager, manager.Address().Hex()) && v.ManagerSubaccount != nil && *v.ManagerSubaccount == 0
	r.check("vault", isManager && !v.TradingHalt,
		fmt.Sprintf("vault=%d manager=%v/%v tradingHalt=%v depositorWhitelist=%v", vaultID, v.Manager, v.ManagerSubaccount, v.TradingHalt, v.DepositorWhitelist))
	vs, err := c.Vaults(ctx)
	r.must("vaults", err)
	listed := slices.ContainsFunc(vs, func(x dexosv1.Vault) bool { return x.Vault == vaultID })
	r.check("vaults", listed, fmt.Sprintf("列表 %d 个金库,含 vault=%d:%v", len(vs), vaultID, listed))

	// ── 4. 在 manager 认领金库所用的账户上授权 API 钱包 ──
	if !st.Approved {
		_, err = w.ApproveAgent(ctx, agent.Address(), time.Now().AddDate(0, 0, 30), "sdk-vault")
		r.must("approve-agent", err)
		st.Approved = true
		r.must("state", st.save(*statePath))
	}
	r.check("approve-agent", true, fmt.Sprintf("account=%s agent=%s", managerRef, agent.Address()))

	// ── 5. 以金库身份开会话:读写的是金库主账户,nonce 仍是这把 API 钱包自己的 ──
	vaultRef := dexosv1.VaultAccount(vaultID)
	s, err := dexosv1.NewSession(ctx, c, agent, vaultRef)
	r.must("session", err)
	r.check("session", true, "ref="+vaultRef.String())

	// ── 6. 远离盘口挂一张只挂单(标记价下方 5%,不会成交)──
	cat, err := c.Catalog(ctx)
	r.must("markets", err)
	m, err := cat.Market(*symbol)
	r.must("markets", err)
	if m.Kind != "perp" || m.MarkPrice == nil {
		r.check("markets", false, *symbol+" 不是有标记价的永续")
		os.Exit(1)
	}
	price := *m.MarkPrice - *m.MarkPrice/20
	minN, err := dexosv1.ParseFixed(m.MinNotional, m.Scale.PriceDecimals+m.Scale.SizeDecimals)
	r.must("markets", err)
	lots := minN.Uint64()/uint64(price) + 1
	res, err := s.Place(ctx, dexosv1.OrderRequest{Market: m, Side: dexos.Buy, Price: price, Lots: lots, TIF: dexos.PostOnly})
	r.must("place", err)
	it := res.Items[0]
	r.check("place", it.Status == "accepted" && it.OrderID != nil, fmt.Sprintf("%s %s × %s → %+v", m.Symbol, m.Scale.Price(price), m.Scale.Size(lots), it))
	if it.OrderID == nil {
		os.Exit(1)
	}
	id := *it.OrderID

	// ── 7. 挂单在金库主账户上,不在 manager 账户上 ──
	has := func(ref dexosv1.AccountRef) (bool, error) {
		open, err := c.Orders(ctx, cat, ref, m.Symbol)
		return slices.ContainsFunc(open, func(o dexosv1.Order) bool { return o.OrderID == id }), err
	}
	onVault, err := has(vaultRef)
	r.check("orders-vault", err == nil && onVault, fmt.Sprintf("金库主账户上有订单 %d:%v(%v)", id, onVault, err))
	onManager, err := has(managerRef)
	r.check("orders-manager", err == nil && !onManager, fmt.Sprintf("manager 账户上有订单 %d:%v(%v)", id, onManager, err))

	// ── 8. 撤单 ──
	cr, err := s.Cancel(ctx, m, id)
	r.check("cancel", err == nil && cr.Items[0].Status == "canceled", fmt.Sprintf("订单 %d → %v", id, err))
	left, err := has(vaultRef)
	r.check("orders-after-cancel", err == nil && !left, fmt.Sprintf("撤单后金库主账户上还有订单 %d:%v(%v)", id, left, err))

	fmt.Printf("\n%d FAIL\n", r.fails)
	if r.fails > 0 {
		os.Exit(1)
	}
}

// deposit 测试入金。`/internal/*` 是运营方接口(不在开放 API 里),只有 token 模式的测试栈开着。
func deposit(ctx context.Context, gw, admin string, to dexos.Address) error {
	var tx [32]byte
	if _, err := rand.Read(tx[:]); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"address": to.Hex(), "amount": fundAmount, "token": 0,
		"txHash": "0x" + hex.EncodeToString(tx[:]), "logIndex": 0,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw+"/internal/deposit?wait=fold", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-admin-token", admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Status string `json:"status"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &out) != nil || out.Status != "ok" {
		return fmt.Errorf("入金失败: HTTP %d %s", resp.StatusCode, raw)
	}
	return nil
}
