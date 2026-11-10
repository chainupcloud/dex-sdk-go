# dex-sdk-go

**English** | [简体中文](README.zh-CN.md)

Go SDK for the DEX-OS exchange (perpetuals + spot). It is built around the **API wallet (agent wallet)**: the master wallet authorizes a derived key to trade on its behalf, and that key **cannot move funds**.

```bash
go get github.com/chainupcloud/dex-sdk-go
```

| Package | What it is |
|---|---|
| [`dexosv1`](dexosv1/doc.go) | **Client for the open API v1 (`/v1/*`). Use it for all new integrations.** Markets are addressed by name + kind (`BTC-USD` + `perp`); accounts by master wallet address plus subaccount index (or vault ID) |
| [`dexos`](dexos/doc.go) | Signing ([`Signer`](dexos/signer.go), EIP-712 [`Domain`](dexos/eip712.go)), addresses, canonical command encoding and receipt types. `dexosv1` is built on top of them. Its HTTP client (`Client`, `Session`, `Connect`, `Discover`) talks to dex-os's legacy public endpoints, which dex-os is retiring (`public-api-v1.md` §9); those entry points are marked **Deprecated** |

## Examples and docs

| | When to use it |
|---|---|
| [`examples/session`](examples/session/main.go) | **Minimal integration**: a URL and one API wallet private key; place and cancel orders |
| [`examples/onboard`](examples/onboard/main.go) | Onboarding from scratch: connection parameters → address → account → (optionally) trade with an API wallet |
| [`examples/quickstart`](examples/quickstart/main.go) | Full API wallet lifecycle (authorize → batch place → atomic replace → disconnect protection → cancel all → revoke → negative proof); also an integration self-check against a real node |
| [`examples/vault`](examples/vault/main.go) | **Trading as a vault** (test stack: deposit → create vault → authorize an API wallet on the manager account → vault-identity session → place / cancel); `GW=… ADMIN_TOKEN=… go run ./examples/vault -state <file>` |
| [`examples/wslatency`](examples/wslatency/main.go) | Measures the latency distribution between a write receipt and the push notification arriving |
| [`examples/subaccounts`](examples/subaccounts/main.go) | **Multi-subaccount market making**: reads the key file exported by the web "API" page, one API wallet per subaccount placing / cancelling concurrently, and checks they never cross over; `-rounds` runs several rounds, and revoking one mid-run affects only that one |
| `dexosv1` contract tests | Check every response against the spec on a real node: reads (`contract_test.go`), writes (`TestContractWrites`), pushes (`contract_ws_test.go`), vaults (`contract_vault_test.go`); run with `go test -tags dexos_contract`, environment variables are listed in each file header |
| [`docs/`](docs/) | Written in the era of the legacy client (in Chinese). The concepts (account model, atomic replace, read consistency, rejection reasons, market-making integrity boundaries) still apply; **for code, this README, the `dexosv1` doc comments and the examples above are authoritative** |

---

## Account model

```
Master wallet (EVM address; accounts under it are told apart by subaccount index, 0 = main account)
  │  can do everything: deposit, withdraw, approve / revoke API wallets, create vaults
  └─ approves ─→ API wallet (agent address)
                  can only trade: place / cancel / replace / cancel all / disconnect protection
                  cannot move funds: withdrawals and transfers are not on the agent allowlist in the execution layer
```

**Why the split.** The machine running your strategy will eventually be compromised, through a poisoned dependency or an operations mistake. If the master wallet key lives there, a breach wipes out your assets. If only an API wallet key lives there, the worst case is someone trading on your behalf; the funds stay in the account and revoking the authorization stops the loss.

This boundary is **enforced inside the deterministic state machine**, not by a gateway check: any command outside the agent allowlist is rejected with `AgentScopeViolation`. A new command does not get agent permission automatically.

v1 does not address accounts by kernel account number and does not return it in responses. The `accountId` that goes into the signed struct is read by the SDK from `/v1/account/nonce`; callers need not care.

### Two write identities, two nonces

| Identity | What it does | Nonce |
|---|---|---|
| [`Session`](dexosv1/session.go) (signed by the API wallet) | Batch place / cancel / atomic replace, cancel all, disconnect protection. Market making runs here | The API wallet's **own** counter |
| [`Wallet`](dexosv1/session.go) (signed by the master wallet) | Approve / revoke API wallets, single place / cancel, set leverage, vault create / deposit / redeem / claim / configure | The master-wallet-scoped counter |

Both maintain the nonce and serialize writes for you. Mixing the two counters gives `NonceMismatch`, and the error message does not say which one was wrong. This is the most common integration pitfall, and exactly what these two types are meant to shield you from.

---

## Initialization

**One gateway address plus one API wallet private key.**

```go
c := dexosv1.NewClient("http://<host IP>:17807")
agent, _ := dexos.NewSigner("0x<API wallet private key>")

// Look up the accounts this API wallet is authorized on; if there is only one, that is the one
grants, _ := c.AgentGrants(ctx, agent.Address())
owner, _ := dexos.ParseAddress(grants[0].Address)
ref := dexosv1.AccountRef{Address: owner, Subaccount: *grants[0].Subaccount}

s, err := dexosv1.NewSession(ctx, c, agent, ref)   // reads the EIP-712 domain, checks the grant, reads the nonce
if err != nil { log.Fatal(err) }

cat, _ := c.Catalog(ctx)
btc, _ := cat.Market("BTC-USD")
px, _ := btc.Scale.Ticks("70000")      // decimal price → ticks; errors if not exact
sz, _ := btc.Scale.Lots("0.001")
res, err := s.Place(ctx, dexosv1.OrderRequest{
    Market: btc, Side: dexos.Buy, Price: px, Lots: sz, TIF: dexos.PostOnly,
})
```

One API wallet can be authorized **separately** on the main account and on each subaccount (the state machine keeps the books per "account × agent"; revoking one leaves the rest untouched). When it is authorized on more than one account you **must name** the `AccountRef`; the SDK will not pick for you. A wrong pick means orders land on another subaccount, with positions, margin and risk all booked elsewhere, **and no error at all**. [`examples/session`](examples/session/main.go) shows how to pick: drop expired grants first, and fail if more than one remains.

`NewSession` does three easy-to-get-wrong things for you:

| | What goes wrong if you get it wrong |
|---|---|
| Reads the EIP-712 domain (chain ID, contract address) from `/v1/exchange` | Wrong chain ID → every signature is rejected while the order book and candles look fine, so it seems that "only placing orders is broken" |
| Checks that this key is really authorized by `ref` and not expired | Not authorized → a wall of rejections; the SDK reports `ErrAgentNotAuthorized` / `ErrAgentExpired` locally first |
| Reads and maintains the API wallet's **own** nonce | Mixing it with the master wallet's nonce → `NonceMismatch` |

Get the API wallet private key from the frontend: `/app` → "API wallet" → generate → the master wallet signs one authorization; the private key is shown only once.

### Perp and spot sharing a name

Since dex-os#318 one name maps to at most one perp market and one spot market (for example a merchant's `demo:ETH-USDC` perp and `demo:ETH-USDC` spot), so the catalog does not use the name as a unique key. There are three ways to locate a market:

```go
m, err := cat.Market("BTC-USD")                          // when the name maps to a single active market
m, err  = cat.Lookup("demo:ETH-USDC", dexosv1.KindSpot) // name + kind (perp | spot)
m, err  = cat.MarketByID(7)                              // market ID (signingMarketId), unaffected by shared names or renames
```

When a name maps to both perp and spot, `cat.Market` returns `ErrAmbiguousSymbol` and does not pick for you (consistent with the server). Every market-scoped interface takes a `*Market` (place, cancel, replace, leverage, paired trade, order book, candles, the `Orders` / `Fills` filters, `AccountFeesIn`, and WS `SubscribeBook` / `SubscribeBBO` / `SubscribeTrades`), and the wire automatically carries `symbol` + `kind`. Market rows returned carry `Kind`; account rows (positions, open orders, fills, `orders` pushes) also carry the matched `SigningMarketID`, and ledger rows carry `SigningMarketID`. A rejected name lookup is a `*SymbolError`; test it with `errors.Is`:

| Error | Meaning |
|---|---|
| `ErrAmbiguousSymbol` | The name maps to both perp and spot and no kind was given (`Kinds` lists the available kinds) |
| `ErrBadKind` | The kind is neither `perp` nor `spot` |
| `ErrDuplicateSymbol` | Two active markets of the same kind share a name (an operator misconfiguration); lookup by market ID still works |

The server returns these three as 400 / 409 before sequencing, so the session is not locked and the nonce is untouched; `errors.As` can still extract a `*dexosv1.Error`.

### Write receipts and errors

Writes always wait for a verdict (`ack=result`, wait capped by `Client.AckWait`, at most 5s). `Place` / `Cancel` / `Replace` return a [`*BatchResult`](dexosv1/write.go): per-item outcomes (`accepted` / `canceled` / `rejected`) with order ID, fills and rejection code.

| Error | Meaning | What to do |
|---|---|---|
| `*dexosv1.Error` | Business or gateway rejection (`Code` is the reason, e.g. `InsufficientMargin`) | Final verdict; the session is not locked; change parameters and resend |
| `*dexosv1.RateLimitedError` | 429: whether the IP bucket or the account bucket ran dry, and `RetryAfter` | The SDK does not retry; how to back off (for example, pausing one market only) is up to you |
| `dexosv1.ErrNotSent` | The request was not sent (local validation failed, or the connection was never established) | Definitely not sent; the session is not locked and the nonce is untouched; resend as is |
| `dexosv1.ErrPending` | Sequenced, verdict unknown (202 / transport failure after connecting / 5xx) | **Do not resend.** The session is locked; use the request identities from `s.Pending()` to query `c.Request`, reconcile, then create a new session |
| `dexosv1.ErrIncompleteBatch` | The per-item outcomes of a batch receipt do not match the request | The batch was executed but which items succeeded is unclear: reconcile by reading the account first |
| `dexosv1.ErrSessionBlocked` | The previous write's verdict is unknown, so the session may not consume further nonces | Same as `ErrPending` |

To write "replace sent → verdict" into your own durable log (so it can be reconciled after a crash), use `Session.ReplaceJournaled`.

### Trading as a vault

A vault's main account is operated by its manager: the API wallet is authorized on the account the manager used to claim the vault (`Vault.Manager` / `Vault.ManagerSubaccount`), and the session addresses the vault identity:

```go
s, err := dexosv1.NewSession(ctx, c, agent, dexosv1.VaultAccount(vaultID))
```

It reads and writes the vault's main account; the nonce is still the API wallet's own counter. A vault identity can only trade (place, cancel, replace, cancel all, disconnect protection); fund-moving actions are rejected by the gateway with 403. Read endpoints accept `VaultAccount(id)` too. See [`examples/vault`](examples/vault/main.go) for the full flow.

### Paired trades (test chain only) and owner-level grants

`POST /v1/orders/paired` makes two accounts trade atomically in one request (a maker leg M and a taker leg T; the price must lie strictly between the best bid and best ask, and neither leg rests on the book). It is used to paint trade candles in test environments. It is only available on the test chain, and the gateway opens it only to allowlisted accounts (404 when the list is empty). Both legs are `sigVersion: 2` signatures: the account is (owner address, subaccount index), and the nonce is a millisecond timestamp the signer picks itself (window nonce).

```go
// The master wallet signs one owner-level grant elsewhere (browser wallet): the bitmap covers only subaccounts M and T
ex, _ := c.Exchange(ctx)
domain, _ := ex.SigningDomain()
nonce := dexosv1.WindowNonce(owner)
payload := domain.ApproveAgentScopeTypedData(owner, agent.Address(), 1<<1|1<<2, 0, nonce)
// ... after getting the signature sig back
_, err := c.SubmitAgentScope(ctx, dexosv1.SignedAgentScope{Owner: owner, Domain: domain, Agent: agent.Address(),
    SubaccountMask: 1<<1 | 1<<2, Nonce: nonce, Signature: sig})

p, err := dexosv1.NewPairedSession(ctx, c, agent,
    dexosv1.AccountRef{Address: owner, Subaccount: 1}, dexosv1.AccountRef{Address: owner, Subaccount: 2})
res, err := p.Trade(ctx, dexosv1.PairedTrade{Market: btc, MakerSide: dexos.Sell, Price: px, Lots: sz})
```

A 422 (`PairedTradeOutsideSpread`, `InsufficientMargin` on the rejected leg, ...) or a 4xx before sequencing is a final verdict and the session stays usable; a 202 / transport failure / 5xx means the verdict is unknown and the session locks; `p.Pending()` gives the maker leg's request identity of every unknown trade (which is the paired trade's requestId); the session accepts concurrent `Trade` calls (only signing is serialized), and no new trade is signed once it is locked. To journal before sending, use `PairedSession.TradeJournaled`. To revoke an owner-level grant: the master wallet signs `domain.RevokeAgentScopeTypedData` (an `ApproveAgentScope` with both bitmap and expiry set to 0) and hands it to `c.SubmitAgentScopeRevoke` (via `/v1/account/agents/revoke`; the approve endpoint does not accept bitmap 0). In the `Agents` result an owner-level grant has `Scope == "owner"`: it appears in the list for every index under the owner, and whether it can trade a given index is determined by `SubaccountMask`.

### Multi-subaccount market making: one API wallet per subaccount

Market makers often put different strategies in different subaccounts, each with its own API wallet (as on Hyperliquid).

- **Create the keys**: on the web "API" page the master wallet clicks "Create API" and ticks a few subaccounts. The browser generates one key per subaccount and exports a **key file** (exported only this once; it contains plaintext private keys). The master wallet then signs one owner-level grant per key; the bitmap has a single bit, so each key can trade only its own subaccount.
- **Use the keys**: [`dexosv1.ParseKeyFile`](dexosv1/keyfile.go) reads this file and checks that each private key matches its address and that subaccounts are not duplicated. Build one `ActionWriter` per key, each with its own `NonceSource`: signer window nonces do not queue behind one another, so several processes signing at once do not knock each other out.
- **Revoke**: revoke one on the web page and the execution layer immediately rejects it (`UnknownAgent`); the rest carry on.

```go
kf, _ := dexosv1.ParseKeyFile(raw)
for _, w := range kf.Wallets {
    aw, err := dexosv1.NewActionWriter(ctx, c, w.Signer, w.Account(kf.Owner), dexosv1.NewNonceSource(0))
    // ... one goroutine per subaccount: aw.SignReplace / c.SendReplace to place, aw.SignCancel / c.SendCancel to cancel
}
```

For the full example see [`examples/subaccounts`](examples/subaccounts/main.go): `go run ./examples/subaccounts -keys api-wallets-xxxx.json -rounds 5`. A subaccount needs margin transferred from the main account first, or orders are rejected with `InsufficientMargin`.

---

## Connection parameters

Chain ID, EIP-712 domain, deposit system address, token table and command encoding version all come from `/v1/exchange`:

```go
ex, err := c.Exchange(ctx)
fmt.Println("chain", ex.ChainID, "deposit address", ex.Deposit.SystemGateway)
if ex.Version.CodecVer != dexos.CodecVersion {
    log.Fatal("the gateway's command encoding differs from this SDK; commands it signs will not be accepted. Upgrade the SDK first")
}
```

Why you should not hard-code these in your own config: **the consequences of getting them wrong are all subtle**.

| What is wrong | What you see |
|---|---|
| Chain ID | Every signature is rejected, while the order book, candles and positions (read-only) are **all fine** |
| Deposit address (the operator redeployed the contract) | Funds go to a contract nobody is listening to: debited on chain, absent from your account |
| Command encoding version | The canonical encoding changed, the hash signed by the API wallet no longer matches, and again everything is rejected |

Deployments use different chains (local anvil is 31337, the test tier on Sepolia is 11155111); the operator redeploys contracts every time the environment is rebuilt, so the address changes. **Do not copy the address into code**: funds sent to a stale system address cannot be recovered.

---

## Onboarding from scratch

```bash
go run ./examples/onboard -url http://<host IP>:17807
```

**1. Address**: use a wallet you already have, or let the SDK generate one:

```go
me, _ := dexos.GenerateSigner()          // generated locally; the private key never leaves this process
```

**2. Deposit**: send a bare USDC `transfer` to `ex.Deposit.SystemGateway`. There is no bridge contract and no `deposit()` method. Once the relayer observes it, the execution layer **opens the account automatically** and binds your EVM address. There is no registration endpoint and no approval: paying opens the account.

> **The SDK cannot do this step.** It only covers the exchange API and has no EVM capability (it does not sign chain transactions or send ERC20 transfers). Use the frontend, a wallet, or `go-ethereum` yourself.

**3. Confirm the account is open**: before depositing, the account table is empty (account reads answer 404). That is the normal starting point, not an error:

```go
subs, _ := c.Subaccounts(ctx, me.Address())
if len(subs) == 0 {
    // not deposited yet
}
acct, _ := c.Account(ctx, dexosv1.AccountRef{Address: me.Address()})
fmt.Println("collateral", acct.Collateral, "USDC")
```

**4. Authorize an API wallet, then trade.** Any of three routes works:

- **Browser** (recommended for humans): `http://<host IP>:17807/app` → "API wallet" → generate → name it → pick a validity period → the master wallet signs once. The private key is shown only once.
- **Code** (recommended for automation):

  ```go
  owner, _ := dexos.NewSigner(masterPrivHex)
  w, _ := dexosv1.NewWallet(ctx, c, owner, 0)          // 0 = main account
  api, _ := dexos.GenerateSigner()                     // the private key never leaves this process; the exchange only gets the address
  w.ApproveAgent(ctx, api.Address(), time.Now().AddDate(0, 6, 0), "my-bot")
  s, _ := dexosv1.NewSession(ctx, c, api, w.Account)   // from here on the master wallet key can be put away
  ```

- **The master wallet key is not on the server** (for example the backend generates the API wallet and the user signs in a browser wallet): `Domain.ApproveAgentTypedData(...)` gives a structure signable with `eth_signTypedData_v4` (the `signingAccountId` and nonce needed for signing are read from `c.Nonce(ctx, ref, nil)`); after getting the signature back, submit it with `c.SubmitApproval(...)`, with parameters identical to those used for signing.

For the full lifecycle (authorize → place → replace → cancel → revoke → negative proof) see [`examples/quickstart`](examples/quickstart/main.go):

```bash
go run ./examples/quickstart -url http://127.0.0.1:17807 -master-key 0x…
```

### Three common pitfalls

- **Trading before depositing**: with no account there is nothing to authorize or trade on; `Subaccounts` answers an empty table and account reads answer 404.
- **Deposit arrival time varies with the finality setting**: `head` takes seconds, `finalized` waits two epochs (about 13 minutes on Sepolia). Do not mistake "not arrived yet" for "deposit failed".
- **An occasional 412 is not a fault.** Production separates reads from writes (writes hit the Raft voters, reads hit read-only replicas); a 412 means the replica has not yet caught up with the log entry you just wrote. It would rather say it is behind than return a stale value. For account reads the SDK automatically attaches the sequence header (`x-dexos-min-seq`) to pin this; if you do get a 412, all replicas are behind, so just retry.

---

## Atomic replace

The basic primitive for a market maker refreshing quotes. **Cancel first, then place, in one state-machine transition.**

```go
res, err := s.Replace(ctx, btc, oldOrderIDs, newQuotes)
```

The difference from "`Cancel`, then `Place`" is not saving one round trip:

- Two separate submissions are **two consensus entries**, and there is a window in between with no quotes
- If the second is rejected (insufficient margin, market halted), the first **has already taken effect**: your orders are all cancelled and no new ones placed, leaving one side exposed

Combined into one entry, the cancel and the place land in the same transition, so no intermediate state can be observed.

"Atomic" precisely means **no intermediate window**, not "all succeed or all roll back": each item is still best effort (a cancel that fails is skipped, a place that fails is skipped), because otherwise one order's margin shortfall would ruin the whole quote refresh, which is worse for a market maker than partial success.

**Modify items** (dex-os#256): the same command can also carry modify items, each being one single-order modify. If the old order has already filled, no new order is placed; if the new order is rejected, the old one is cancelled with it. So for each modify item at most one of the old and new orders can fill, and an account sitting at its position limit need not cancel before placing:

```go
res, err := s.ReplaceModify(ctx, btc, cancelIDs,
	[]dexosv1.ModifyRequest{{OrderID: bidID, Price: newBid, Lots: lots, TIF: dexos.PostOnly}},
	newQuotes)
```

In the receipt a modify item has `Kind == "modify"`, `ReplacedOrderID` is the old order and `OrderID` the new one; `CanceledLots` is set when the old order was actually cancelled. Without modify items it behaves exactly like `Replace`; the write-ahead-journal variant is `ReplaceModifyJournaled`.

Disconnect protection: `s.ScheduleCancel(ctx, time.Now().Add(30*time.Second))` cancels all of the account's open orders at the deadline; extend it with every quote round; a zero time clears it. `s.CancelAll` takes effect at the next block boundary, and orders placed in the meantime are cancelled too.

---

## Push (WebSocket)

One connection carries all subscriptions; on disconnect it reconnects with exponential backoff and resubscribes:

```go
st, _ := dexosv1.NewStream(c, cat, dexosv1.StreamOptions{OnState: func(e dexosv1.StateEvent) { /* … */ }})
st.SubscribeOrders(ref, func(u dexosv1.OrdersUpdate) { /* u.Open = all open orders after applying */ })
st.SubscribeOrderChanges(ref, func(c dexosv1.OrderChange) { /* snapshots arrive in c.Open; deltas give only c.Order (nil for a removal), so per-frame cost is independent of the total open-order count */ })
st.SubscribeFills(ref, cursor, func(f dexosv1.Fill) { /* from the cursor on, no gaps and no duplicates */ })
go st.Run(ctx)
```

Each subscription has a readiness signal: while `Syncing`, local state cannot be trusted and market making should pause the affected market; after `Live`, applying updates in order keeps you consistent with the server; `Failed` is terminal (subscription rejected, ledger epoch changed). On failure it never silently falls back to polling; every cause is reported through `OnState`. Gaps in fills and fund flows caused by a disconnect are backfilled by the SDK through REST using the cursor.

---

## Units

| Concept | In the SDK | On the wire |
|---|---|---|
| Price | tick (`uint32`) | decimal string, `price = tick / 10^priceDecimals` |
| Quantity | lot (`uint64`) | decimal string, `size = lot / 10^sizeDecimals` |
| Amounts, fees, PnL | decimal string | same; for atomic units use `dexosv1.ParseFixed(amount, decimals)` |

Order price and quantity stay integers in the SDK, in the same quantity space as the EIP-712 struct: they are converted exactly using the market's `Scale` on the way in and out, and an inexact conversion is an error, **never rounded**. A silently rounded price, once signed, is a different order.

---

## Canonical encoding and golden-sample cross-checks

The signature of an API-wallet-signed command commits to

```
commandHash = keccak256(kernel canonical encoding bytes)
```

The server re-encodes the inner command with **its own** encoder and compares. One byte off → the signature does not match → rejected. **It never fails silently, but it gives no clue either.** So `dexos/codec.go` must be byte-for-byte identical to dex-os's canonical encoding.

Review cannot guarantee this; cross-checking does:

```bash
# in the dex-os repository
cargo run -q --example sdk_golden > ../dex-sdk-go/dexos/testdata/golden.json
# in this repository
go test ./dexos/
```

The golden samples cover edge cases: empty sets, extreme values, cancel-only, place-only. Paired trades and v2 signatures (the `PairedTradeLeg` encoding, the `Action` / `ApproveAgentScope` digests, signatures, request identity) have their own `dexosv1/testdata/paired_golden.json`, exported from dex-os's Rust implementation; see [`dexosv1/testdata/README.md`](dexosv1/testdata/README.md) for how. Two byte orders coexist in this SDK, which is the easiest thing to mix up:

- **Canonical command encoding** → little-endian (the kernel's compact encoding)
- **EIP-712 words** → big-endian, 32 bytes right-aligned (the Ethereum ABI convention)

---

## Verification

```bash
go test ./...      # golden cross-check, v1 wire shapes and spec, session / push state machines, examples use only v1
go vet ./...
go run ./examples/quickstart -url … -master-key 0x…   # integration self-check against a real node
```

Golden samples only prove the encoding is right; the signing path (secp256k1 v-bit ordering, address derivation, EIP-712 domain separator) can only be validated against a real node. If any of those three is wrong, the symptom is the server rejecting signatures, which static checks cannot see. That is why quickstart ends with a **negative proof**: after the authorization is revoked, the same key must be rejected when placing an order. Otherwise an all-green run cannot show that the authorization actually took effect.

---

## License

Apache-2.0
