package dexosv1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/chainupcloud/dex-sdk-go/dexos"
)

const expectManifestHeader = "x-dexos-expect-manifest"

// decU64 安全读口里 u64 一律是十进制字符串(防 JS 精度丢失);null 解成 0。
type decU64 uint64

func (d *decU64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		*d = 0
		return nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("dexosv1: %s 不是 u64: %w", b, err)
	}
	*d = decU64(v)
	return nil
}

// optDecU64 可空的 u64 十进制字符串。
type optDecU64 struct{ v *uint64 }

func (o *optDecU64) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		o.v = nil
		return nil
	}
	var d decU64
	if err := d.UnmarshalJSON(b); err != nil {
		return err
	}
	v := uint64(d)
	o.v = &v
	return nil
}

// SecurityNetwork 读 GET /v1/security/network:签 139 要钉的网络身份。网络身份与这个客户端上次读到的不同
// (账本重置)时,丢掉旧账本的读一致性位号,见 [Client.Seq]。
func (c *Client) SecurityNetwork(ctx context.Context) (Network, error) {
	var w struct {
		NetworkID    Hash   `json:"networkId"`
		GenesisHash  Hash   `json:"genesisHash"`
		LedgerEpoch  decU64 `json:"ledgerEpoch"`
		NodeChainID  Hash   `json:"nodeChainId"`
		NodeVerifier string `json:"nodeVerifier"`
		ManifestHash Hash   `json:"manifestHash"`
	}
	if err := c.get(ctx, "/v1/security/network", nil, marketRead, &w); err != nil {
		return Network{}, err
	}
	chain := new(big.Int).SetBytes(w.NodeChainID[:])
	if !chain.IsUint64() || chain.Sign() == 0 {
		return Network{}, fmt.Errorf("dexosv1: nodeChainId %s 不是可用的链号", w.NodeChainID.Hex())
	}
	verifier, err := dexos.ParseAddress(w.NodeVerifier)
	if err != nil {
		return Network{}, fmt.Errorf("dexosv1: nodeVerifier: %w", err)
	}
	if w.NetworkID.IsZero() || w.GenesisHash.IsZero() || w.ManifestHash.IsZero() {
		return Network{}, fmt.Errorf("dexosv1: /v1/security/network 缺网络身份")
	}
	n := Network{NetworkID: w.NetworkID, GenesisHash: w.GenesisHash, LedgerEpoch: uint64(w.LedgerEpoch), ChainID: chain.Uint64(), Verifier: verifier, ManifestHash: w.ManifestHash}
	c.pinLedger(n)
	return n, nil
}

// SecurityTime 账本认证时间与读到它的位号。139 的有效期按 TimestampMs 判;按 actionId 查回执时把 HostLogIndex
// 作 minSeq,读视图就一定覆盖了这个时刻之前定序的全部命令。
type SecurityTime struct {
	TimestampMs  uint64
	HostLogIndex uint64
	ManifestHash Hash
}

// SecurityTime 读 GET /v1/security/time。
func (c *Client) SecurityTime(ctx context.Context) (SecurityTime, error) {
	var w struct {
		HostLogIndex decU64 `json:"hostLogIndex"`
		ManifestHash Hash   `json:"manifestHash"`
		Time         struct {
			TimestampMs decU64 `json:"timestampMs"`
			Paused      bool   `json:"paused"`
		} `json:"time"`
	}
	if err := c.get(ctx, "/v1/security/time", nil, marketRead, &w); err != nil {
		return SecurityTime{}, err
	}
	if w.Time.TimestampMs == 0 || w.HostLogIndex == 0 {
		return SecurityTime{}, fmt.Errorf("dexosv1: 账本还没有认证时间")
	}
	return SecurityTime{TimestampMs: uint64(w.Time.TimestampMs), HostLogIndex: uint64(w.HostLogIndex), ManifestHash: w.ManifestHash}, nil
}

// NonceWindow 一个(主体, 车道)的 nonce 窗口:低于 Floor 拒,用过的拒,Next 及以上一定没用过。
type NonceWindow struct {
	Floor uint64
	High  *uint64
	Next  uint64
}

// SecurityNonce 读 GET /v1/security/nonces。
func (c *Client) SecurityNonce(ctx context.Context, p Principal, lane Lane) (NonceWindow, error) {
	if err := p.check(); err != nil {
		return NonceWindow{}, err
	}
	q := url.Values{"principal": {strings.ToLower(string(p.Kind))}, "policy": {strconv.FormatUint(p.Policy, 10)}, "lane": {lane.query()}}
	if p.Kind == PrincipalAgent {
		q.Set("grant", p.GrantID.Hex())
		q.Set("grantEpoch", strconv.FormatUint(p.GrantEpoch, 10))
	}
	var w struct {
		Floor decU64    `json:"floor"`
		High  optDecU64 `json:"high"`
		Next  decU64    `json:"next"`
	}
	if err := c.get(ctx, "/v1/security/nonces", q, accountRead, &w); err != nil {
		return NonceWindow{}, err
	}
	return NonceWindow{Floor: uint64(w.Floor), High: w.High.v, Next: uint64(w.Next)}, nil
}

// SecurityAccount 一个账户策略(成员、门限、纪元)。SoleSigner = 这个地址的单签主账户。
type SecurityAccount struct {
	AccountID   uint32
	PolicyID    uint64
	PolicyEpoch uint64
	Members     []dexos.Address
	Threshold   int
	SoleSigner  bool
}

// SecurityAccounts 读 GET /v1/security/accounts?member=:成员里有这个地址的全部账户策略。
func (c *Client) SecurityAccounts(ctx context.Context, member dexos.Address) ([]SecurityAccount, error) {
	var w struct {
		Accounts []struct {
			AccountID   uint32   `json:"accountId"`
			PolicyID    decU64   `json:"policyId"`
			PolicyEpoch decU64   `json:"policyEpoch"`
			Members     []string `json:"members"`
			Threshold   int      `json:"threshold"`
			SoleSigner  bool     `json:"soleSigner"`
		} `json:"accounts"`
	}
	if err := c.get(ctx, "/v1/security/accounts", url.Values{"member": {member.Hex()}}, accountRead, &w); err != nil {
		return nil, err
	}
	out := make([]SecurityAccount, 0, len(w.Accounts))
	for _, a := range w.Accounts {
		row := SecurityAccount{AccountID: a.AccountID, PolicyID: uint64(a.PolicyID), PolicyEpoch: uint64(a.PolicyEpoch), Threshold: a.Threshold, SoleSigner: a.SoleSigner}
		for _, m := range a.Members {
			addr, err := dexos.ParseAddress(m)
			if err != nil {
				return nil, fmt.Errorf("dexosv1: 账户 %d 的成员 %q: %w", a.AccountID, m, err)
			}
			row.Members = append(row.Members, addr)
		}
		out = append(out, row)
	}
	return out, nil
}

// GrantStatus grant 的状态。
type GrantStatus string

const (
	GrantActive  GrantStatus = "Active"
	GrantPaused  GrantStatus = "Paused"
	GrantRevoked GrantStatus = "Revoked"
)

// Grant 账本上的一份 grant(GET /v1/security/grants 的一行)。Reclaimable 非空 = 已失效、名额可回收
// ("revoked" | "expired" | "issuerEpochStale" | "resourceOwnerEpochStale")。
type Grant struct {
	GrantID     Hash
	Epoch       uint64
	IssuerEpoch uint64
	Status      GrantStatus
	Agent       dexos.Address
	Actions     []uint16
	Accounts    []uint32
	ExpiresAtMs uint64
	Markets     []GrantMarket
	Reclaimable string
}

// Grants 一个账户策略签发的 grant 与名额。
type Grants struct {
	PolicyID       uint64
	IssuerEpoch    uint64
	LiveGrants     int
	MaxLiveGrants  int
	MaxGrants      int
	OccupiedGrants int
	Grants         []Grant
}

// SecurityGrants 读 GET /v1/security/grants?policy=。
func (c *Client) SecurityGrants(ctx context.Context, policy uint64) (Grants, error) {
	var w struct {
		PolicyID       decU64 `json:"policyId"`
		IssuerEpoch    decU64 `json:"issuerEpoch"`
		LiveGrants     int    `json:"liveGrants"`
		OccupiedGrants int    `json:"occupiedGrants"`
		MaxGrants      int    `json:"maxGrantsPerIssuer"`
		MaxLive        int    `json:"maxLiveGrantsPerPolicy"`
		Grants         []struct {
			GrantID     Hash        `json:"grantId"`
			Epoch       decU64      `json:"epoch"`
			IssuerEpoch decU64      `json:"issuerEpoch"`
			Status      GrantStatus `json:"status"`
			Grantee     struct {
				Agent string `json:"agent"`
			} `json:"grantee"`
			Actions []struct {
				Namespace uint8  `json:"namespace"`
				ID        uint16 `json:"id"`
			} `json:"actions"`
			Scope       json.RawMessage `json:"scope"`
			ExpiresAtMs decU64          `json:"expiresAtMs"`
			Limits      struct {
				Markets []struct {
					Market            uint16 `json:"market"`
					MarketSpecVersion decU64 `json:"marketSpecVersion"`
					MinTick           uint32 `json:"minTick"`
					MaxTick           uint32 `json:"maxTick"`
				} `json:"markets"`
			} `json:"limits"`
			Reclaimable *string `json:"reclaimable"`
		} `json:"grants"`
	}
	if err := c.get(ctx, "/v1/security/grants", url.Values{"policy": {strconv.FormatUint(policy, 10)}}, accountRead, &w); err != nil {
		return Grants{}, err
	}
	out := Grants{PolicyID: uint64(w.PolicyID), IssuerEpoch: uint64(w.IssuerEpoch), LiveGrants: w.LiveGrants, MaxLiveGrants: w.MaxLive, MaxGrants: w.MaxGrants, OccupiedGrants: w.OccupiedGrants}
	for _, g := range w.Grants {
		row := Grant{GrantID: g.GrantID, Epoch: uint64(g.Epoch), IssuerEpoch: uint64(g.IssuerEpoch), Status: g.Status, ExpiresAtMs: uint64(g.ExpiresAtMs)}
		if g.Reclaimable != nil {
			row.Reclaimable = *g.Reclaimable
		}
		if g.Grantee.Agent != "" {
			agent, err := dexos.ParseAddress(g.Grantee.Agent)
			if err != nil {
				return Grants{}, fmt.Errorf("dexosv1: grant %s 的代理地址: %w", g.GrantID.Hex(), err)
			}
			row.Agent = agent
		}
		for _, a := range g.Actions {
			if a.Namespace == 1 {
				row.Actions = append(row.Actions, a.ID)
			}
		}
		var scope struct {
			Accounts []uint32 `json:"accounts"`
		}
		if json.Unmarshal(g.Scope, &scope) == nil {
			row.Accounts = scope.Accounts
		}
		for _, m := range g.Limits.Markets {
			row.Markets = append(row.Markets, GrantMarket{Market: m.Market, SpecVersion: uint64(m.MarketSpecVersion), MinTick: m.MinTick, MaxTick: m.MaxTick})
		}
		out.Grants = append(out.Grants, row)
	}
	return out, nil
}

// SecurityMarket 账本上的市场与当前规格版本(签 grant 的市场限制要用)。
type SecurityMarket struct {
	Dex         uint16
	Market      uint16
	SpecVersion uint64
	Admission   int
}

// SecurityMarkets 读 GET /v1/security/markets。
func (c *Client) SecurityMarkets(ctx context.Context) ([]SecurityMarket, error) {
	var w struct {
		Markets []struct {
			Dex               uint16 `json:"dex"`
			Market            uint16 `json:"market"`
			MarketSpecVersion decU64 `json:"marketSpecVersion"`
			Admission         int    `json:"admission"`
		} `json:"markets"`
	}
	if err := c.get(ctx, "/v1/security/markets", nil, marketRead, &w); err != nil {
		return nil, err
	}
	out := make([]SecurityMarket, 0, len(w.Markets))
	for _, m := range w.Markets {
		out = append(out, SecurityMarket{Dex: m.Dex, Market: m.Market, SpecVersion: uint64(m.MarketSpecVersion), Admission: m.Admission})
	}
	return out, nil
}

// 提交 139 的三种结论之外的错误。
var (
	// ErrCommandOutcomeUnknown 提交结果未知(503、读应答超时、连接中途断):原样重发同一份命令是安全的
	// (已受理时回 200 与第一次的回执);过了 ValidUntilMs 之后按 actionId 查回执定结论。
	ErrCommandOutcomeUnknown = errors.New("dexosv1: 提交结果未知")
	// ErrNetworkChanged 网关钉的网络(manifest)和命令签名时的不同:账本已换代,这份命令永远不会被受理。
	ErrNetworkChanged = errors.New("dexosv1: dex-os 网络已换代")
)

// CommandRejectedError 网关在定序之前拒了这份命令:没执行、nonce 也没消耗。Code 是网关的稳定错误码,
// 如 invalid_security_identity、precheck_rejected、grant_revoked、GatewayBuilderMarketNotListed。
type CommandRejectedError struct {
	Status int
	Code   string
	Body   string
}

func (e *CommandRejectedError) Error() string {
	return fmt.Sprintf("dexosv1: /v1/commands 拒绝(HTTP %d %s): %s", e.Status, e.Code, e.Body)
}

// CommandAck POST /v1/commands 的应答。202 只说明已定序,不说明执行了;Duplicate = 同一份原件早已受理,
// Receipt 是第一次的回执。
type CommandAck struct {
	RequestID Hash
	Seq       uint64
	Sub       uint16
	Duplicate bool
	Receipt   *CommandReceipt
}

// SubmitCommand 提交一条签好名的 139 / 179。命令签名时的 manifest 随请求钉住,签到旧网络上的命令拿到 [ErrNetworkChanged]。
func (c *Client) SubmitCommand(ctx context.Context, cmd Command) (CommandAck, error) {
	body, err := json.Marshal(map[string]string{"command": cmd.Hex()})
	if err != nil {
		return CommandAck{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/commands", bytes.NewReader(body))
	if err != nil {
		return CommandAck{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set(expectManifestHeader, cmd.ManifestHash.Hex())
	if c.Host != "" {
		req.Host = c.Host
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return CommandAck{}, &CommandRejectedError{Code: "not_sent", Body: err.Error()}
		}
		return CommandAck{}, fmt.Errorf("%w: %w", ErrCommandOutcomeUnknown, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return CommandAck{}, fmt.Errorf("%w: 应答读取失败: %w", ErrCommandOutcomeUnknown, err)
	}
	switch resp.StatusCode {
	case http.StatusAccepted:
		var w struct {
			RequestID Hash   `json:"requestId"`
			Seq       decU64 `json:"seq"`
			Sub       uint16 `json:"sub"`
		}
		if err := json.Unmarshal(raw, &w); err != nil {
			return CommandAck{}, fmt.Errorf("%w: 202 应答解析失败: %w", ErrCommandOutcomeUnknown, err)
		}
		if w.RequestID != cmd.ActionID {
			return CommandAck{}, fmt.Errorf("dexosv1: 网关回的 requestId %s 不是本命令的 actionId %s", w.RequestID.Hex(), cmd.ActionID.Hex())
		}
		c.ObserveSeq(uint64(w.Seq))
		return CommandAck{RequestID: w.RequestID, Seq: uint64(w.Seq), Sub: w.Sub}, nil
	case http.StatusOK:
		r, err := decodeReceipt(raw)
		if err != nil {
			return CommandAck{}, fmt.Errorf("%w: 重发应答解析失败: %w", ErrCommandOutcomeUnknown, err)
		}
		return CommandAck{RequestID: cmd.ActionID, Seq: r.Seq, Sub: r.Sub, Duplicate: true, Receipt: &r}, nil
	case http.StatusTooManyRequests:
		return CommandAck{}, parseError(resp.StatusCode, resp.Header, raw)
	}
	code := securityErrorCode(raw)
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable && code == "upstream_manifest_mismatch":
		return CommandAck{}, fmt.Errorf("%w: %s", ErrNetworkChanged, raw)
	case resp.StatusCode == http.StatusServiceUnavailable && code == "GatewayHistoryBackpressure":
		return CommandAck{}, &CommandRejectedError{Status: resp.StatusCode, Code: code, Body: string(raw)}
	case resp.StatusCode >= 500:
		return CommandAck{}, fmt.Errorf("%w: HTTP %d %s", ErrCommandOutcomeUnknown, resp.StatusCode, raw)
	}
	return CommandAck{}, &CommandRejectedError{Status: resp.StatusCode, Code: code, Body: string(raw)}
}

func securityErrorCode(raw []byte) string {
	var w struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &w) != nil || len(w.Error) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(w.Error, &s) == nil {
		return s
	}
	var obj struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Error, &obj)
	return obj.Code
}

// CommandStatus 回执状态。
type CommandStatus string

const (
	// CommandUnknown 读视图里没有这份原件:单独看不证明没执行;配合 minSeq 与有效期才能下结论,见 [Client.CommandReceipt]。
	CommandUnknown CommandStatus = "Unknown"
	// CommandAccepted 已受理,结果可能还没落定(ResultsPending)。
	CommandAccepted CommandStatus = "Accepted"
	// CommandExecuted 已执行,业务结果在 Events / Items / BusinessRejected。
	CommandExecuted CommandStatus = "Executed"
	// CommandAuthorizationRejected 账本授权拒绝:没执行、nonce 没消耗。
	CommandAuthorizationRejected CommandStatus = "AuthorizationRejected"
	// CommandConflict 同一 actionId 账本受理的是另一份原件(digest 不同,如改了有效期重签)。
	CommandConflict CommandStatus = "Conflict"
)

// CommandEvent 回执里的一条业务事件。
type CommandEvent struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// CommandReceipt GET /v1/requests/{actionId} 的结论。
type CommandReceipt struct {
	Status           CommandStatus
	Digest           Hash
	Seq              uint64
	Sub              uint16
	ReadSeq          uint64
	Outcome          string
	ResultsPending   bool
	BusinessRejected string
	StableError      string
	ErrorCode        int
	Events           []CommandEvent
	Items            []json.RawMessage
}

func decodeReceipt(raw []byte) (CommandReceipt, error) {
	var w struct {
		Status         CommandStatus `json:"status"`
		Digest         *Hash         `json:"digest"`
		HostLogIndex   decU64        `json:"hostLogIndex"`
		Seq            decU64        `json:"seq"`
		Sub            *uint16       `json:"sub"`
		Outcome        *string       `json:"outcome"`
		ResultsPending bool          `json:"resultsPending"`
		BusinessRej    *string       `json:"businessRejected"`
		StableError    string        `json:"stableError"`
		ErrorCode      int           `json:"errorCode"`
		Position       *struct {
			HostLogIndex   decU64 `json:"hostLogIndex"`
			CommandOrdinal uint16 `json:"commandOrdinal"`
		} `json:"position"`
		Events []CommandEvent    `json:"events"`
		Items  []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return CommandReceipt{}, err
	}
	r := CommandReceipt{Status: w.Status, ReadSeq: uint64(w.HostLogIndex), ResultsPending: w.ResultsPending, StableError: w.StableError, ErrorCode: w.ErrorCode, Events: w.Events, Items: w.Items}
	if w.Digest != nil {
		r.Digest = *w.Digest
	}
	if w.Position != nil {
		r.Seq, r.Sub = uint64(w.Position.HostLogIndex), w.Position.CommandOrdinal
	}
	if w.Seq != 0 {
		r.Seq = uint64(w.Seq)
	}
	if w.Sub != nil {
		r.Sub = *w.Sub
	}
	if w.Outcome != nil {
		r.Outcome = *w.Outcome
	}
	if w.BusinessRej != nil {
		r.BusinessRejected = *w.BusinessRej
	}
	switch r.Status {
	case CommandUnknown, CommandAccepted, CommandExecuted, CommandAuthorizationRejected, CommandConflict:
	default:
		return CommandReceipt{}, fmt.Errorf("dexosv1: 未知回执状态 %q", r.Status)
	}
	return r, nil
}

// ReadViewBehindError 读视图还没到要求的位号(412):稍后再查,不能据此下结论。
type ReadViewBehindError struct {
	ReadSeq     uint64
	RequiredSeq uint64
}

func (e *ReadViewBehindError) Error() string {
	return fmt.Sprintf("dexosv1: 读视图在 %d,还没到要求的 %d", e.ReadSeq, e.RequiredSeq)
}

// CommandReceipt 按 actionId 查一份原件的回执。digest 是这份原件的签名摘要;minSeq 非 0 时读视图必须
// 覆盖到这个位号,否则返回 [*ReadViewBehindError]。
//
// 404 回 Status = [CommandUnknown] 而不是错误:原件过期(账本认证时间超过 ValidUntilMs)之后,以那一刻
// [SecurityTime].HostLogIndex 作 minSeq 查到 Unknown,说明这份原件没有、也永远不会被执行。
func (c *Client) CommandReceipt(ctx context.Context, actionID, digest Hash, minSeq uint64) (CommandReceipt, error) {
	u := c.BaseURL + "/v1/requests/" + actionID.Hex() + "?" + url.Values{"digest": {digest.Hex()}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return CommandReceipt{}, err
	}
	if minSeq > 0 {
		req.Header.Set(minSeqHeader, strconv.FormatUint(minSeq, 10))
	}
	if c.Host != "" {
		req.Host = c.Host
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return CommandReceipt{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return CommandReceipt{}, fmt.Errorf("dexosv1: 回执读取失败: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNotFound, http.StatusConflict:
		r, err := decodeReceipt(raw)
		if err != nil {
			return CommandReceipt{}, fmt.Errorf("dexosv1: 回执解析失败(HTTP %d): %w: %s", resp.StatusCode, err, raw)
		}
		if resp.StatusCode == http.StatusNotFound && r.Status != CommandUnknown {
			return CommandReceipt{}, fmt.Errorf("dexosv1: 404 回执状态是 %q: %s", r.Status, raw)
		}
		if resp.StatusCode == http.StatusConflict {
			r.Status = CommandConflict
		}
		if r.Status != CommandUnknown && r.Status != CommandConflict && r.Digest != digest {
			return CommandReceipt{}, fmt.Errorf("dexosv1: 回执 digest %s 不是查询的 %s", r.Digest.Hex(), digest.Hex())
		}
		if v, err := strconv.ParseUint(resp.Header.Get(seqHeader), 10, 64); err == nil {
			r.ReadSeq = max(r.ReadSeq, v)
		}
		if minSeq > 0 && r.ReadSeq < minSeq {
			return CommandReceipt{}, &ReadViewBehindError{ReadSeq: r.ReadSeq, RequiredSeq: minSeq}
		}
		return r, nil
	case http.StatusPreconditionFailed:
		var w struct {
			HostLogIndex decU64 `json:"hostLogIndex"`
			RequiredSeq  decU64 `json:"requiredSeq"`
		}
		_ = json.Unmarshal(raw, &w)
		return CommandReceipt{}, &ReadViewBehindError{ReadSeq: uint64(w.HostLogIndex), RequiredSeq: uint64(w.RequiredSeq)}
	}
	return CommandReceipt{}, &Error{Status: resp.StatusCode, Code: securityErrorCode(raw), Body: string(raw)}
}

// ErrNotFinal 还下不了结论:命令还没过期,或已受理但结果未落定、读视图落后。稍后再查。
var ErrNotFinal = errors.New("dexosv1: 命令结论还没落定")

// FinalReceipt 一份命令的终局结论。Executed = false 时这份原件没有、也永远不会被执行(nonce 没消耗);
// Executed = true 时业务结果在 Receipt(含 BusinessRejected)。
type FinalReceipt struct {
	Executed bool
	Receipt  CommandReceipt
}

// Finalize 给出命令的终局结论,结果未知(超时、503)之后用它收口。账本认证时间超过 ValidUntilMs 之后,以那一刻的
// 位号作 minSeq 查回执:查到 Executed 按回执收口;授权被拒或查不到,说明这份原件没执行、以后也不会再被受理。
// 还没过期、结果未落定或读视图落后时返回 [ErrNotFinal]。
func (c *Client) Finalize(ctx context.Context, cmd Command) (FinalReceipt, error) {
	t, err := c.SecurityTime(ctx)
	if err != nil {
		return FinalReceipt{}, err
	}
	if t.ManifestHash != cmd.ManifestHash {
		return FinalReceipt{}, fmt.Errorf("%w: 命令签在 %s,账本已是 %s", ErrNetworkChanged, cmd.ManifestHash.Hex(), t.ManifestHash.Hex())
	}
	if t.TimestampMs <= cmd.ValidUntilMs {
		return FinalReceipt{}, fmt.Errorf("%w: 认证时间 %d 还没过有效期 %d", ErrNotFinal, t.TimestampMs, cmd.ValidUntilMs)
	}
	r, err := c.CommandReceipt(ctx, cmd.ActionID, cmd.Digest, t.HostLogIndex)
	var behind *ReadViewBehindError
	if errors.As(err, &behind) {
		return FinalReceipt{}, fmt.Errorf("%w: %w", ErrNotFinal, err)
	}
	if err != nil {
		return FinalReceipt{}, err
	}
	switch r.Status {
	case CommandUnknown, CommandAuthorizationRejected:
		return FinalReceipt{Executed: false, Receipt: r}, nil
	case CommandExecuted:
		if r.ResultsPending {
			return FinalReceipt{}, fmt.Errorf("%w: 已执行,业务结果还没落定", ErrNotFinal)
		}
		return FinalReceipt{Executed: true, Receipt: r}, nil
	case CommandAccepted:
		return FinalReceipt{}, fmt.Errorf("%w: 已受理,还没执行完", ErrNotFinal)
	}
	return FinalReceipt{}, fmt.Errorf("dexosv1: 账本受理的是同一 actionId 的另一份原件(digest %s)", r.Digest.Hex())
}
