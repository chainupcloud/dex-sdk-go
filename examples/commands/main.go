// commands 演示 dex-os 安全账本上做市代理的完整写流程:主钱包(账户根)给代理发 141 授权 → 代理签 139
// 在子账户上挂一张远离市价的 post-only 单 → 按 actionId 查回执 → 原样重发拿到第一次的回执 → 撤单 →
// 一份没提交的命令过期后查到 Unknown(永远不会执行)→ 撤销授权。
//
//	go run ./examples/commands -url http://127.0.0.1:26080 -owner-key-file owner.key -subaccount 1 -market 0 -price 30000
//
// owner.key 是主钱包私钥(hex 一行);子账户要先有保证金。
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/chainupcloud/dex-sdk-go/dexos"
	"github.com/chainupcloud/dex-sdk-go/dexosv1"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:26080", "dex-os base URL")
	keyFile := flag.String("owner-key-file", "", "owner private key file")
	sub := flag.Uint("subaccount", 1, "subaccount index")
	market := flag.Uint("market", 0, "market id")
	price := flag.Uint("price", 0, "post-only buy price in ticks, far from the market")
	flag.Parse()
	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		log.Fatal(err)
	}
	owner, err := dexos.NewSigner(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(raw)), "0x")))
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	c := dexosv1.NewClient(*url)
	net := must(c.SecurityNetwork(ctx))
	log.Printf("network %s epoch %d chain %d manifest %s", net.NetworkID.Hex(), net.LedgerEpoch, net.ChainID, net.ManifestHash.Hex())

	accounts := must(c.SecurityAccounts(ctx, owner.Address()))
	i := slices.IndexFunc(accounts, func(a dexosv1.SecurityAccount) bool { return a.SoleSigner })
	if i < 0 {
		log.Fatalf("%s 没有单签主账户", owner.Address().Hex())
	}
	root := dexosv1.AccountRoot(accounts[i].PolicyID, accounts[i].PolicyEpoch)
	subs := must(c.Subaccounts(ctx, owner.Address()))
	j := slices.IndexFunc(subs, func(s dexosv1.Subaccount) bool { return uint(s.Subaccount) == *sub })
	if j < 0 {
		log.Fatalf("没有子账户 %d", *sub)
	}
	account := subs[j].SigningAccountID
	log.Printf("policy %d epoch %d, subaccount %d = account %d", root.Policy, root.Epoch, *sub, account)

	spec := uint64(0)
	for _, m := range must(c.SecurityMarkets(ctx)) {
		if uint(m.Market) == *market && m.Dex == 0 {
			spec = m.SpecVersion
		}
	}
	if spec == 0 {
		log.Fatalf("市场 %d 不在 /v1/security/markets", *market)
	}
	agent := must(dexos.GenerateSigner())
	var salt dexosv1.Hash
	_, _ = rand.Read(salt[:])
	now := must(c.SecurityTime(ctx))
	grant := dexosv1.GrantSpec{Salt: salt, Agent: agent.Address(), Accounts: []uint32{account}, ExpectedIssuerEpoch: root.Epoch,
		Actions:     []uint16{dexosv1.ActionCancelOrder, dexosv1.ActionBatchCancel, dexosv1.ActionBatchPlace, dexosv1.ActionBatchReplace2},
		ExpiresAtMs: now.TimestampMs + uint64(time.Hour.Milliseconds()),
		Limits:      dexosv1.GrantLimits{WindowMs: 1000, Markets: []dexosv1.GrantMarket{{Market: uint16(*market), SpecVersion: spec, MinTick: 1, MaxTick: ^uint32(0)}}}}
	payload, grantID := mustPair(net.EncodeGrantCapability(root.Policy, root.Epoch, grant))
	submitAndWait(ctx, c, net, root, dexosv1.LaneManagement, payload, owner, "grant")

	var grantEpoch uint64
	for range 50 {
		for _, g := range must(c.SecurityGrants(ctx, root.Policy)).Grants {
			if g.GrantID == grantID && g.Status == dexosv1.GrantActive {
				grantEpoch = g.Epoch
			}
		}
		if grantEpoch != 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if grantEpoch == 0 {
		log.Fatalf("grant %s 没生效", grantID.Hex())
	}
	log.Printf("grant %s epoch %d active", grantID.Hex(), grantEpoch)
	ag := dexosv1.AgentOf(root.Policy, root.Epoch, grantID, grantEpoch)

	place := dexos.EncodeBatchPlace(account, 0, []dexos.BatchOrder{{Market: uint16(*market), Side: dexos.Buy, Price: uint32(*price), Lots: 1, TIF: dexos.PostOnly}})
	placed := submitAndWait(ctx, c, net, ag, dexosv1.LaneTrade, place, agent, "place")
	ack, err := c.SubmitCommand(ctx, placed.cmd)
	if err != nil || !ack.Duplicate || ack.Seq != placed.ack.Seq {
		log.Fatalf("原样重发应回第一次的回执: %+v %v", ack, err)
	}
	log.Printf("resubmit: duplicate=%v seq=%d", ack.Duplicate, ack.Seq)

	orderID := orderFrom(placed.receipt)
	cancel := dexos.EncodeBatchCancel(account, []dexos.OrderID{dexos.OrderID(orderID)})
	submitAndWait(ctx, c, net, ag, dexosv1.LaneTrade, cancel, agent, "cancel")

	nonce := must(c.SecurityNonce(ctx, ag, dexosv1.LaneTrade)).Next
	t := must(c.SecurityTime(ctx))
	act := must(net.Action(dexosv1.Authorization{Principal: ag, Lane: dexosv1.LaneTrade, Nonce: nonce, ValidUntilMs: t.TimestampMs + 2000}, cancel))
	never := must(act.Sign(agent))
	for {
		t = must(c.SecurityTime(ctx))
		if t.TimestampMs > never.ValidUntilMs {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	r := must(c.CommandReceipt(ctx, never.ActionID, never.Digest, t.HostLogIndex))
	log.Printf("never submitted, after expiry at seq %d: %s", t.HostLogIndex, r.Status)
	if r.Status != dexosv1.CommandUnknown {
		log.Fatalf("没提交过的命令过期后应查到 Unknown")
	}
	if _, err := c.SubmitCommand(ctx, never); err != nil {
		log.Printf("submit after expiry: %v", err)
	} else {
		late := waitReceipt(ctx, c, never)
		log.Printf("submit after expiry: %s %s %d", late.Status, late.StableError, late.ErrorCode)
	}

	submitAndWait(ctx, c, net, root, dexosv1.LaneManagement, dexosv1.EncodeRevokeCapability(grantID, grantEpoch), owner, "revoke")
	log.Print("done")
}

type sent struct {
	cmd     dexosv1.Command
	ack     dexosv1.CommandAck
	receipt dexosv1.CommandReceipt
}


func submitAndWait(ctx context.Context, c *dexosv1.Client, net dexosv1.Network, p dexosv1.Principal, lane dexosv1.Lane, payload []byte, s *dexos.Signer, what string) sent {
	w := must(c.SecurityNonce(ctx, p, lane))
	t := must(c.SecurityTime(ctx))
	act := must(net.Action(dexosv1.Authorization{Principal: p, Lane: lane, Nonce: w.Next, ValidUntilMs: t.TimestampMs + 30000}, payload))
	cmd := must(act.Sign(s))
	start := time.Now()
	ack, err := c.SubmitCommand(ctx, cmd)
	if err != nil {
		log.Fatalf("%s: %v", what, err)
	}
	accepted := time.Since(start)
	r := waitReceipt(ctx, c, cmd)
	log.Printf("%s: nonce %d action %s accepted in %s, %s in %s, outcome %q businessRejected %q", what, w.Next, cmd.ActionID.Hex(), accepted, r.Status, time.Since(start), r.Outcome, r.BusinessRejected)
	dump(what, r)
	if r.Status != dexosv1.CommandExecuted || r.BusinessRejected != "" {
		log.Fatalf("%s 没执行成功: %+v", what, r)
	}
	return sent{cmd: cmd, ack: ack, receipt: r}
}

func waitReceipt(ctx context.Context, c *dexosv1.Client, cmd dexosv1.Command) dexosv1.CommandReceipt {
	for range 100 {
		r, err := c.CommandReceipt(ctx, cmd.ActionID, cmd.Digest, 0)
		var behind *dexosv1.ReadViewBehindError
		switch {
		case errors.As(err, &behind):
		case err != nil:
			log.Fatal(err)
		case r.Status == dexosv1.CommandExecuted && !r.ResultsPending, r.Status == dexosv1.CommandAuthorizationRejected:
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	log.Fatalf("回执一直没落定: %s", cmd.ActionID.Hex())
	return dexosv1.CommandReceipt{}
}

func dump(what string, r dexosv1.CommandReceipt) {
	b, _ := json.Marshal(map[string]any{"events": r.Events, "items": r.Items})
	fmt.Fprintf(os.Stderr, "--- %s receipt: %s\n", what, b)
}

func orderFrom(r dexosv1.CommandReceipt) uint64 {
	for _, it := range r.Items {
		var v struct {
			OrderID *json.Number `json:"orderId"`
		}
		if json.Unmarshal(it, &v) == nil && v.OrderID != nil {
			n, err := v.OrderID.Int64()
			if err == nil {
				return uint64(n)
			}
			var u uint64
			_, _ = fmt.Sscan(v.OrderID.String(), &u)
			return u
		}
	}
	log.Fatalf("回执里找不到订单号")
	return 0
}

func must[T any](v T, err error) T {
	if err != nil {
		log.Fatal(err)
	}
	return v
}

func mustPair[A, B any](a A, b B, err error) (A, B) {
	if err != nil {
		log.Fatal(err)
	}
	return a, b
}
