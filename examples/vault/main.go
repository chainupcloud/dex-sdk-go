// vault:对着真节点走一遍「以金库身份交易」(ForVault)—— manager 名下的 API 钱包代金库主账户挂单、撤单。
//
//	GW=http://127.0.0.1:17807 ADMIN_TOKEN=… go run ./examples/vault -state <文件>
//
// 流程:生成 manager 钱包与 API 钱包 → 测试入金(/internal/deposit,只在 token 模式的测试栈可用)
// → manager 建库(自任 manager,出资人白名单只填自己)→ 在 manager 账户上授权 API 钱包
// → dexos.New(…, ForVault(金库号))→ 远离盘口挂一张 PostOnly → 挂单在金库主账户上、不在 manager 账户上
// → 撤单 → 主账户上没有这张单。每步 PASS / FAIL,任何 FAIL 退出码 1。
//
// 建库是主钥匙操作,不在 SDK 里;这里按 dex-os crates/auth 的 CreateManagedVault 结构手签,只为备好测试金库。
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
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

const (
	fundAmount = "2000000000" // 2000 USDC:建库费 10 + 种子 1000 + 挂单保证金
	seedAmount = 1_000_000_000
)

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
	gw, admin := os.Getenv("GW"), os.Getenv("ADMIN_TOKEN")
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

	c, cfg, err := dexos.Connect(ctx, gw)
	r.must("connect", err)
	r.check("connect", true, fmt.Sprintf("chainId=%d codec=v%d", cfg.ChainID, cfg.CodecVer))

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

	// ── 2. 测试入金 ──
	if !st.Funded {
		r.must("deposit", deposit(ctx, gw, admin, manager.Address()))
		st.Funded = true
		r.must("state", st.save(*statePath))
	}
	var ref *dexos.AccountRef
	for i := 0; i < 20; i++ {
		if ref, err = c.AccountByAddress(ctx, manager.Address().Hex()); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	r.must("deposit", err)
	managerAccount := ref.AccountID
	r.check("deposit", true, fmt.Sprintf("manager 账户=%d collateral=%s", managerAccount, ref.Collateral))

	// ── 3. 建库:manager 自建自管,出资人白名单只有自己 ──
	if st.Vault == nil {
		id, err := createVault(ctx, c, gw, manager, managerAccount)
		r.must("create-vault", err)
		st.Vault = &id
		r.must("state", st.save(*statePath))
	}
	vaultID := *st.Vault
	v, err := c.Vault(ctx, vaultID)
	r.must("vault", err)
	r.check("vault", v.Manager != nil && *v.Manager == manager.Address() && !v.TradingHalt &&
		slices.Equal(v.DepositorWhitelist, []dexos.Address{manager.Address()}),
		fmt.Sprintf("vault=%d mainAccount=%d manager=%v tradingHalt=%v depositorWhitelist=%v",
			vaultID, v.MainAccount, v.Manager, v.TradingHalt, v.DepositorWhitelist))
	vs, err := c.Vaults(ctx)
	r.must("vaults", err)
	listed := slices.ContainsFunc(vs, func(x dexos.Vault) bool { return x.Vault == vaultID && x.MainAccount == v.MainAccount })
	r.check("vaults", listed, fmt.Sprintf("列表 %d 个金库,含 vault=%d:%v", len(vs), vaultID, listed))

	// ── 4. 在 manager 账户上授权 API 钱包(manager 账户的 nonce)──
	if !st.Approved {
		n, err := c.NextNonce(ctx, managerAccount)
		r.must("approve-agent", err)
		_, err = c.ApproveAgent(ctx, manager, managerAccount, agent.Address(), time.Now().AddDate(0, 0, 30), n)
		r.must("approve-agent", err)
		st.Approved = true
		r.must("state", st.save(*statePath))
	}
	r.check("approve-agent", true, fmt.Sprintf("master=%d agent=%s", managerAccount, agent.Address()))

	// ── 5. 以金库身份开会话 ──
	s, err := dexos.New(ctx, gw, st.AgentKey, dexos.ForVault(vaultID))
	r.must("session", err)
	r.check("session", s.Account == v.MainAccount, fmt.Sprintf("session.Account=%d mainAccount=%d", s.Account, v.MainAccount))

	// ── 6. 远离盘口挂一张 PostOnly(参考价下方 5%,不会成交)──
	markets, err := s.Markets(ctx)
	r.must("markets", err)
	var perp *dexos.Market
	for i := range markets {
		if markets[i].Symbol == *symbol && markets[i].Kind == "perp" {
			perp = &markets[i]
		}
	}
	if perp == nil {
		r.check("markets", false, "没有永续市场 "+*symbol)
		os.Exit(1)
	}
	pm, refPx := perp.Market, perp.BestBid
	if refPx == 0 {
		refPx = perp.Oracle
	}
	price := refPx - refPx/20
	ev, err := s.Place(ctx, dexos.BatchOrder{Market: pm, Side: dexos.Buy, Price: price, Lots: 1, TIF: dexos.PostOnly})
	seq, ok := seqOf(ev, "OrderAccepted")
	r.check("place", err == nil && ok, fmt.Sprintf("market=%d price=%d seq=%d events=%s err=%v", pm, price, seq, kinds(ev), err))
	if !ok {
		os.Exit(1)
	}

	// ── 7. 挂单在金库主账户上,不在 manager 账户上 ──
	onMain, err := c.OrderSeqs(ctx, v.MainAccount, pm)
	r.check("orders-main", err == nil && slices.Contains(onMain, seq), fmt.Sprintf("/orders/%d seqs=%v err=%v", v.MainAccount, onMain, err))
	onManager, err := c.OrderSeqs(ctx, managerAccount, pm)
	r.check("orders-manager", err == nil && !slices.Contains(onManager, seq), fmt.Sprintf("/orders/%d seqs=%v err=%v", managerAccount, onManager, err))

	// ── 8. 撤单 ──
	ev, err = s.Cancel(ctx, pm, seq)
	_, canceled := seqOf(ev, "OrderCanceled")
	r.check("cancel", err == nil && canceled, fmt.Sprintf("seq=%d events=%s err=%v", seq, kinds(ev), err))
	left, err := c.OrderSeqs(ctx, v.MainAccount, pm)
	r.check("orders-after-cancel", err == nil && !slices.Contains(left, seq), fmt.Sprintf("/orders/%d seqs=%v err=%v", v.MainAccount, left, err))

	fmt.Printf("\n%d FAIL\n", r.fails)
	if r.fails > 0 {
		os.Exit(1)
	}
}

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

// createVault 建库并从 VaultCreated 事件取金库号。creator = manager = 出资人白名单唯一成员;
// 业绩分成、锁定期、提款窗口、存入上限都不设,只放种子资金。
func createVault(ctx context.Context, c *dexos.Client, gw string, owner *dexos.Signer, account uint32) (uint16, error) {
	n, err := c.NextNonce(ctx, account)
	if err != nil {
		return 0, err
	}
	me := owner.Address()
	sig, err := owner.SignHashHex(createManagedVaultHash(c.Domain, me, account, me, seedAmount, me, n))
	if err != nil {
		return 0, err
	}
	body, _ := json.Marshal(map[string]any{
		"owner": me.Hex(), "accountId": account, "manager": me.Hex(),
		"nickname":    "mm-825-" + me.Hex()[2:10],
		"description": "dex-liquidity #825 以金库身份做市的测试金库:manager 专用钱包,出资人白名单只有 manager",
		"seedAmount":  fmt.Sprint(seedAmount), "depositorWhitelist": []string{me.Hex()},
		"nonce": n, "signature": sig,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw+"/vault?wait=fold", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Status string                `json:"status"`
		Reason string                `json:"reason"`
		Events []dexos.EventEnvelope `json:"events"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &out) != nil || out.Status != "ok" {
		return 0, fmt.Errorf("建库失败: HTTP %d %s", resp.StatusCode, raw)
	}
	for _, ev := range out.Events {
		var d struct {
			Vault *uint16 `json:"vault"`
		}
		if ev.Kind == "VaultCreated" && json.Unmarshal(ev.Data, &d) == nil && d.Vault != nil {
			return *d.Vault, nil
		}
	}
	return 0, fmt.Errorf("建库回执里没有 VaultCreated: events=%s", kinds(out.Events))
}

// createManagedVaultHash 按 dex-os crates/auth 的 CreateManagedVault 算 EIP-712 待签哈希。
// 未设的可选项按哨兵 0 编码;出资人白名单只有一个地址,摘要 = keccak(这 20 字节)。
func createManagedVaultHash(d dexos.Domain, owner dexos.Address, account uint32, manager dexos.Address,
	seed uint64, depositor dexos.Address, nonce uint64) []byte {
	const typ = "CreateManagedVault(address owner,uint32 accountId,address manager,uint32 profitSharePpm," +
		"uint64 depositMinDurationMs,uint32 managerMinPortionPpm,uint32 managerLiqPortionPpm,uint32 withdrawWindowS," +
		"uint32 withdrawDurationS,uint128 depositCap,uint128 seedAmount,bytes32 depositorWhitelistHash,uint64 nonce)"
	structHash := dexos.Keccak256(
		dexos.Keccak256([]byte(typ)),
		wordAddr(owner), word(uint64(account)), wordAddr(manager),
		word(0), word(0), word(0), word(0), word(0), word(0), word(0), // profitShare … depositCap
		word(seed), dexos.Keccak256(depositor[:]), word(nonce),
	)
	separator := dexos.Keccak256(
		dexos.Keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)")),
		dexos.Keccak256([]byte(d.Name)), dexos.Keccak256([]byte(d.Version)),
		word(d.ChainID), wordAddr(d.VerifyingContract),
	)
	return dexos.Keccak256([]byte{0x19, 0x01}, separator, structHash)
}

func word(v uint64) []byte {
	var w [32]byte
	for i := 0; i < 8; i++ {
		w[31-i] = byte(v >> (8 * i))
	}
	return w[:]
}

func wordAddr(a dexos.Address) []byte {
	var w [32]byte
	copy(w[12:], a[:])
	return w[:]
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
